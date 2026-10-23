//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

// A04-N：gateway_scheduling.go 的两个跨平台准入消费点走**同一个来源共同门**
// （cursorSupplySourceAvailabilityVetoReason）：
//
//  1. isAccountAdmittedForPlatform 的同平台分支（cursor 账号走 cursor 平台）；
//  2. cursorSupplyAdmittedForNativePlatform（cursor 账号承接原生平台请求）。
//
// 本文件只钉死**调用点行为**，不复制门的判定规则：
//   - gate 明确否决（reason 非空）→ 账号不入选；
//   - gate 读取失败（err != nil）→ 按本文件既有错误语义失败关闭（明确拒绝，
//     既不当 false 放行，也不过早 503——本文件既有 bool 门遇错一律返回 false）；
//   - gate 通过 → 账号入选（证明消费点未新增任何放行条件）。
//
// 门的判定规则本身由来源门维护方（openai_gateway_scheduling.go）负责，
// 此处一律经由其唯一入口消费。

const (
	a04nSchedGroupID  = int64(8801)
	a04nSchedKeyID    = int64(8802)
	a04nSchedModel    = "claude-sonnet-4-5"
	a04nSchedPlatform = PlatformAnthropic
)

// a04nSchedAccount 构造一个可参与 cursor 跨平台供给的账号：显式挂组 +
// model_mapping 显式声明请求模型 + 绑定激活码（绑定后才会进入来源门）。
func a04nSchedAccount() Account {
	keyID := a04nSchedKeyID
	return Account{
		ID:          8803,
		Platform:    PlatformCursor,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		QuotaKeyID:  &keyID,
		GroupIDs:    []int64{a04nSchedGroupID},
		Credentials: map[string]any{"model_mapping": map[string]any{a04nSchedModel: "upstream-" + a04nSchedModel}},
	}
}

// a04nSchedGroupCtx 构造带分组白名单的上下文（分组模型白名单是准入的独立维度，
// 本文件只固定它"允许"，让来源门成为唯一变量）。
func a04nSchedGroupCtx() context.Context {
	group := &Group{
		ID:       a04nSchedGroupID,
		Platform: a04nSchedPlatform,
		Status:   StatusActive,
		Hydrated: true,
		ModelAllowlist: GroupModelAllowlist{
			Enabled: true,
			Models:  []string{a04nSchedModel},
		},
	}
	return context.WithValue(context.Background(), ctxkey.Group, group)
}

// a04nSchedFreezeNow 冻结来源门的新鲜度时钟（快照 checked_at 判定用）。
func a04nSchedFreezeNow(t *testing.T) time.Time {
	t.Helper()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	prev := cursorSupplySourceUsageNow
	cursorSupplySourceUsageNow = func() time.Time { return now }
	t.Cleanup(func() { cursorSupplySourceUsageNow = prev })
	return now
}

// TestGatewayAdmissionConsumesSourceGate 钉死两个准入消费点的来源门消费行为。
func TestGatewayAdmissionConsumesSourceGate(t *testing.T) {
	now := a04nSchedFreezeNow(t)
	groupID := a04nSchedGroupID
	freshCheckedAt := now.Add(-time.Minute).Format(time.RFC3339)

	// 消费点①：同平台分支（cursor 账号 + cursor 平台）。
	// 消费点②：cursorSupplyAdmittedForNativePlatform（cursor 账号承接原生平台）。
	admittedSamePlatform := func(ctx context.Context, acc *Account) bool {
		svc := &GatewayService{}
		return svc.isAccountAdmittedForPlatform(ctx, acc, PlatformCursor, false, a04nSchedModel, &groupID)
	}
	admittedNativePlatform := func(ctx context.Context, acc *Account) bool {
		svc := &GatewayService{}
		return svc.isAccountAdmittedForPlatform(ctx, acc, a04nSchedPlatform, false, a04nSchedModel, &groupID)
	}

	t.Run("gate passes: both consumption points admit", func(t *testing.T) {
		// 来源可用（码 active + 快照新鲜未耗尽）→ 两个消费点都入选，
		// 证明消费点没有新增任何放行条件。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses: map[int64]string{a04nSchedKeyID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{a04nSchedKeyID: []byte(
				`{"total":400,"used":86,"status_code":200,"checked_at":"` + freshCheckedAt + `","error_code":""}`,
			)},
		})
		ctx := a04nSchedGroupCtx()
		acc := a04nSchedAccount()
		require.True(t, admittedSamePlatform(ctx, &acc), "来源门通过时同平台消费点应入选")
		require.True(t, admittedNativePlatform(ctx, &acc), "来源门通过时原生平台消费点应入选")
	})

	t.Run("gate vetoes by expired key: both consumption points reject", func(t *testing.T) {
		// 激活码终态 → 门明确否决（reason 非空）→ 两个消费点都不入选。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses: map[int64]string{a04nSchedKeyID: domain.QuotaKeyStatusExpired},
		})
		ctx := a04nSchedGroupCtx()
		acc := a04nSchedAccount()
		require.False(t, admittedSamePlatform(ctx, &acc), "码终态时同平台消费点不得入选")
		require.False(t, admittedNativePlatform(ctx, &acc), "码终态时原生平台消费点不得入选")
	})

	t.Run("gate vetoes by exhausted source: both consumption points reject", func(t *testing.T) {
		// 真实来源剩余 ≤ 0 → 门明确否决 → 两个消费点都不入选。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses: map[int64]string{a04nSchedKeyID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{a04nSchedKeyID: []byte(
				`{"total":100,"used":100,"status_code":200,"checked_at":"` + freshCheckedAt + `","error_code":""}`,
			)},
		})
		ctx := a04nSchedGroupCtx()
		acc := a04nSchedAccount()
		require.False(t, admittedSamePlatform(ctx, &acc), "来源耗尽时同平台消费点不得入选")
		require.False(t, admittedNativePlatform(ctx, &acc), "来源耗尽时原生平台消费点不得入选")
	})

	t.Run("gate read failure: both consumption points reject (fail closed)", func(t *testing.T) {
		// 来源读不到（快照解析失败 → 门返回 error）→ 沿用本文件既有错误语义：
		// 明确拒绝（false），既不当 false 放行，也不过早 503。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses: map[int64]string{a04nSchedKeyID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{a04nSchedKeyID: []byte(
				`{not-valid-json`,
			)},
		})
		ctx := a04nSchedGroupCtx()
		acc := a04nSchedAccount()
		// 钉死本例走的是「读取失败」分支而非「来源明确被拒」分支：reason 为空 + err 非空。
		reason, err := cursorSupplySourceAvailabilityVetoReason(ctx, &acc)
		require.Error(t, err, "快照解析失败必须让门返回 error")
		require.Empty(t, reason, "读取失败时门不产出 reason（非空即否决的分支不适用）")
		require.False(t, admittedSamePlatform(ctx, &acc), "来源读不到时同平台消费点必须失败关闭")
		require.False(t, admittedNativePlatform(ctx, &acc), "来源读不到时原生平台消费点必须失败关闭")
	})

	t.Run("gate state read failure: both consumption points reject (fail closed)", func(t *testing.T) {
		// 激活码状态读不到（provider 报错）→ 门返回 error → 两个消费点都失败关闭。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statusErr: errors.New("quota key store unreachable"),
		})
		ctx := a04nSchedGroupCtx()
		acc := a04nSchedAccount()
		reason, err := cursorSupplySourceAvailabilityVetoReason(ctx, &acc)
		require.Error(t, err, "激活码状态读不到必须让门返回 error")
		require.Empty(t, reason, "读取失败时门不产出 reason")
		require.False(t, admittedSamePlatform(ctx, &acc), "激活码状态读不到时同平台消费点必须失败关闭")
		require.False(t, admittedNativePlatform(ctx, &acc), "激活码状态读不到时原生平台消费点必须失败关闭")
	})

	t.Run("non cursor account is unaffected by the source gate", func(t *testing.T) {
		// 非 cursor 账号不在来源门的判定范围内（门只对绑定激活码的 cursor 账号生效），
		// 消费点不得因此收紧原生账号的既有准入——即使它绑定了同一个失效激活码。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{a04nSchedKeyID: domain.QuotaKeyStatusExpired},
			statusErr: errors.New("quota key store unreachable"),
		})
		ctx := a04nSchedGroupCtx()
		keyID := a04nSchedKeyID
		native := Account{
			ID:          8804,
			Platform:    a04nSchedPlatform,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			QuotaKeyID:  &keyID,
		}
		svc := &GatewayService{}
		require.True(t, svc.isAccountAdmittedForPlatform(ctx, &native, a04nSchedPlatform, false, a04nSchedModel, &groupID),
			"来源门不得收紧非 cursor 账号的既有准入")
	})
}
