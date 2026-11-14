//go:build unit

// W2-D1：两层额度读通路 DTO 投影（方案 §25.6 消费闭包⑤⑥⑦ + §25.14-C「唯一状态表示」、
// §25.7-B 展示口径）的定向单测。
//
// 钉死的三条口径：
//  1. 快照三态：缺失 / 成功 / 失败。缺失**不是** 0（数值字段 nil，不落 0）；
//  2. 失败保留上次成功值与其时间，只加"查询失败"标记与 attempted_at/error_code；
//  3. 批量读取只做常量次仓储查询（不逐 key 查询、不发网络）。

package service

import (
	"context"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

const (
	snapshotSuccessJSON = `{"total":400,"used":86,"status_code":200,` +
		`"checked_at":"2026-10-22T02:00:00Z","attempted_at":"2026-10-22T02:00:00Z","error_code":""}`
	snapshotFailedJSON = `{"total":400,"used":86,"status_code":200,` +
		`"checked_at":"2026-10-22T02:00:00Z","attempted_at":"2026-10-22T02:05:00Z","error_code":"lookup_timeout"}`
)

// TestUsageSnapshotDtoMissingIsNotZero 钉死"缺失不是 0"：
// nil / 空 / "null" / {} 一律视为"无快照"，投影为 nil（响应里省略字段），绝不产出 0 值。
func TestUsageSnapshotDtoMissingIsNotZero(t *testing.T) {
	for name, raw := range map[string][]byte{
		"nil":           nil,
		"empty":         []byte(""),
		"blank":         []byte("   "),
		"json-null":     []byte("null"),
		"empty-object":  []byte("{}"),
		"null-fields":   []byte(`{"total":null,"used":null,"checked_at":null}`),
		"no-success-no": []byte(`{"status_code":200}`),
	} {
		t.Run(name, func(t *testing.T) {
			snapshot, ok := ParseQuotaKeyUsageSnapshot(raw)
			require.False(t, ok, "缺失快照不得被判为有效")
			require.False(t, snapshot.ParseFailed, "缺失不是解析失败")
			require.Nil(t, snapshot.Total, "缺失时 total 必须为 nil（不是 0）")
			require.Nil(t, snapshot.Used, "缺失时 used 必须为 nil（不是 0）")

			projection := BuildQuotaKeyUsageProjection(raw)
			require.Nil(t, projection, "无快照时不得输出投影（前端不得按 0 渲染）")
		})
	}
}

// TestUsageSnapshotDtoBadJSONRegistersParseFailure 钉死"坏 JSON"的容错：
// 返回"无快照"（不返回错误、不返回 0 值），但把"读不出来"登记成 error_code 语义，
// 以便展示层标记查询失败而不是伪装成"没有额度"。
func TestUsageSnapshotDtoBadJSONRegistersParseFailure(t *testing.T) {
	raw := []byte(`{"total":400,"used":`)
	snapshot, ok := ParseQuotaKeyUsageSnapshot(raw)
	require.False(t, ok)
	require.True(t, snapshot.ParseFailed)
	require.Equal(t, QuotaKeyUsageErrorCodeParseFailed, snapshot.ErrorCode)
	require.Nil(t, snapshot.Total)
	require.Nil(t, snapshot.Used)

	projection := BuildQuotaKeyUsageProjection(raw)
	require.NotNil(t, projection, "解析失败必须留下可展示的失败标记")
	require.False(t, projection.HasSnapshot)
	require.True(t, projection.QueryFailed)
	require.Equal(t, QuotaKeyUsageErrorCodeParseFailed, projection.ErrorCode)
	require.Nil(t, projection.Total, "解析失败不得补 0")
	require.Nil(t, projection.Used, "解析失败不得补 0")
	require.Nil(t, projection.Remaining)
}

// TestUsageSnapshotDtoSuccessProjectsValues 钉死成功态投影：total/used/算术剩余/时间齐全。
func TestUsageSnapshotDtoSuccessProjectsValues(t *testing.T) {
	projection := BuildQuotaKeyUsageProjection([]byte(snapshotSuccessJSON))
	require.NotNil(t, projection)
	require.True(t, projection.HasSnapshot)
	require.False(t, projection.QueryFailed)
	require.Empty(t, projection.ErrorCode)

	require.NotNil(t, projection.Total)
	require.Equal(t, 400, *projection.Total)
	require.NotNil(t, projection.Used)
	require.Equal(t, 86, *projection.Used)
	require.NotNil(t, projection.Remaining, "remaining 是 total-used 的算术剩余")
	require.Equal(t, 314, *projection.Remaining)
	require.NotNil(t, projection.StatusCode)
	require.Equal(t, 200, *projection.StatusCode)

	require.NotNil(t, projection.CheckedAt)
	require.Equal(t, time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC), projection.CheckedAt.UTC())
	require.NotNil(t, projection.AttemptedAt)

	// 投影自带的归属标签：这是"所属激活码额度"，不是本站账单余额/账号余额。
	require.Equal(t, QuotaKeyUsageScope, projection.Scope)
	require.Equal(t, QuotaKeyUsageLabel, projection.Label)
}

// TestUsageSnapshotDtoFailedObservationKeepsLastSuccessAndFlagsQueryFailed 钉死失败态：
// 保留上次成功数值与其时间（仅作带时间的历史展示），attempted_at/error_code 是最近观测，
// query_failed=true；绝不写 0、不抹掉最近成功数值。
func TestUsageSnapshotDtoFailedObservationKeepsLastSuccessAndFlagsQueryFailed(t *testing.T) {
	projection := BuildQuotaKeyUsageProjection([]byte(snapshotFailedJSON))
	require.NotNil(t, projection)
	require.True(t, projection.HasSnapshot)
	require.True(t, projection.QueryFailed)

	require.NotNil(t, projection.Total)
	require.Equal(t, 400, *projection.Total, "失败不得抹掉最近成功总额")
	require.NotNil(t, projection.Used)
	require.Equal(t, 86, *projection.Used, "失败不得把 used 写成 0")
	require.NotNil(t, projection.Remaining)
	require.Equal(t, 314, *projection.Remaining)

	require.NotNil(t, projection.CheckedAt, "成功值必须带自己的时间")
	require.Equal(t, time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC), projection.CheckedAt.UTC())
	require.NotNil(t, projection.AttemptedAt, "最近观测时间必须保留")
	require.Equal(t, time.Date(2026, 10, 22, 2, 5, 0, 0, time.UTC), projection.AttemptedAt.UTC())
	require.Equal(t, "lookup_timeout", projection.ErrorCode)
}

// TestUsageSnapshotDtoFailedWithoutHistoryHasNoNumbers 钉死"仅失败、无成功历史"：
// 只标记失败与尝试时间，任何数值都保持缺失（不是 0）。
func TestUsageSnapshotDtoFailedWithoutHistoryHasNoNumbers(t *testing.T) {
	raw := []byte(`{"attempted_at":"2026-10-22T02:05:00Z","error_code":"upstream_500"}`)
	projection := BuildQuotaKeyUsageProjection(raw)
	require.NotNil(t, projection)
	require.True(t, projection.QueryFailed)
	require.Nil(t, projection.Total)
	require.Nil(t, projection.Used)
	require.Nil(t, projection.Remaining)
	require.Nil(t, projection.CheckedAt)
	require.NotNil(t, projection.AttemptedAt)
}

// TestUsageSnapshotDtoRealZeroIsDistinctFromMissing 钉死"真实 0 与缺失不同"：
// used=0 是成功观测的合法值，必须投影成 0 而不是被当成缺失。
func TestUsageSnapshotDtoRealZeroIsDistinctFromMissing(t *testing.T) {
	raw := []byte(`{"total":400,"used":0,"status_code":200,"checked_at":"2026-10-22T02:00:00Z","error_code":""}`)
	projection := BuildQuotaKeyUsageProjection(raw)
	require.NotNil(t, projection)
	require.NotNil(t, projection.Used, "真实 0 必须保留为 0，不能当成缺失")
	require.Equal(t, 0, *projection.Used)
	require.NotNil(t, projection.Remaining)
	require.Equal(t, 400, *projection.Remaining)
}

// TestUsageSnapshotDtoRemainingRequiresBothValues 钉死"剩余只在两个数值都存在时给出"，
// 且非法（负数）计数按缺失处理，不补 0。
func TestUsageSnapshotDtoRemainingRequiresBothValues(t *testing.T) {
	onlyTotal := BuildQuotaKeyUsageProjection([]byte(`{"total":400,"checked_at":"2026-10-22T02:00:00Z"}`))
	require.NotNil(t, onlyTotal)
	require.NotNil(t, onlyTotal.Total)
	require.Nil(t, onlyTotal.Used)
	require.Nil(t, onlyTotal.Remaining, "缺 used 不得算出剩余")

	illegal := BuildQuotaKeyUsageProjection([]byte(`{"total":400,"used":-1,"checked_at":"2026-10-22T02:00:00Z"}`))
	require.NotNil(t, illegal)
	require.Nil(t, illegal.Used, "非法负值按缺失处理，不落成 -1 或 0")
	require.Nil(t, illegal.Remaining)
}

// usageProjectionCountingRepo 在既有内存替身上只加"查询次数"计数：
// 用于证明批量投影是常量次查询，不会随行数增长（禁 N+1）。
type usageProjectionCountingRepo struct {
	*fakeQuotaKeyRepo
	listCalls   int
	getByIDCall int
}

func (r *usageProjectionCountingRepo) List(ctx context.Context) ([]*ent.QuotaKey, error) {
	r.listCalls++
	return r.fakeQuotaKeyRepo.List(ctx)
}

func (r *usageProjectionCountingRepo) GetByID(ctx context.Context, id int64) (*ent.QuotaKey, error) {
	r.getByIDCall++
	return r.fakeQuotaKeyRepo.GetByID(ctx, id)
}

func newUsageProjectionCountingRepo() (*usageProjectionCountingRepo, map[int64]*ent.QuotaKey) {
	base := newFakeQuotaKeyRepo()
	keys := map[int64]*ent.QuotaKey{
		1: {ID: 1, CodeLast4: "1234", Status: "active", UsageSnapshot: jsontext.Value(snapshotSuccessJSON)},
		2: {ID: 2, CodeLast4: "5678", Status: "active", UsageSnapshot: jsontext.Value(snapshotFailedJSON)},
		3: {ID: 3, CodeLast4: "9012", Status: "active"}, // 无快照：必须省略而不是 0
	}
	for id, qk := range keys {
		base.byID[id] = qk
	}
	return &usageProjectionCountingRepo{fakeQuotaKeyRepo: base}, keys
}

// TestQuotaKeyDtoListWithUsageProjectionsSingleQuery 钉死列表端点：
// 一次仓储查询同时得出脱敏状态与快照投影，且无快照的激活码保持 nil（不是 0）。
func TestQuotaKeyDtoListWithUsageProjectionsSingleQuery(t *testing.T) {
	repo, _ := newUsageProjectionCountingRepo()
	svc := &QuotaKeyService{repo: repo}

	statuses, err := svc.ListWithUsageProjections(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, repo.listCalls, "列表投影只允许一次仓储查询")
	require.Len(t, statuses, 3)

	byID := map[int64]*QuotaKeyStatus{}
	for _, st := range statuses {
		byID[st.ID] = st
	}
	require.Equal(t, 400, *byID[1].Usage.Total)
	require.Equal(t, 86, *byID[1].Usage.Used)
	require.False(t, byID[1].Usage.QueryFailed)

	require.NotNil(t, byID[2].Usage)
	require.True(t, byID[2].Usage.QueryFailed)
	require.Equal(t, "lookup_timeout", byID[2].Usage.ErrorCode)
	require.Equal(t, 400, *byID[2].Usage.Total, "失败态保留最近成功值")

	require.Nil(t, byID[3].Usage, "无快照的激活码不得输出投影（缺失不是 0）")
}

// TestQuotaKeyDtoLoadUsageProjectionsBatchNoPerRowQuery 钉死批量投影：
// 多 id 只查一次 List（与行数无关）；单 id 走一次主键查询。
func TestQuotaKeyDtoLoadUsageProjectionsBatchNoPerRowQuery(t *testing.T) {
	repo, _ := newUsageProjectionCountingRepo()
	svc := &QuotaKeyService{repo: repo}

	projections, err := svc.LoadQuotaKeyUsageProjections(context.Background(), []int64{1, 2, 3})
	require.NoError(t, err)
	require.Equal(t, 1, repo.listCalls, "多 id 批量投影只允许一次仓储查询（禁逐 key 查询）")
	require.Zero(t, repo.getByIDCall, "批量路径不得退化成逐行主键查询")
	require.Len(t, projections, 2, "无快照的 id 不出现在结果里")
	require.NotContains(t, projections, int64(3))

	single, err := svc.QuotaKeyUsageProjectionFor(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, single)
	require.Equal(t, 1, repo.getByIDCall, "单 id 详情只允许一次主键查询")
	require.Equal(t, 1, repo.listCalls, "单 id 详情不得再走 List")

	missing, err := svc.QuotaKeyUsageProjectionFor(context.Background(), 3)
	require.NoError(t, err)
	require.Nil(t, missing, "无快照时返回 nil 投影")
}

// TestQuotaKeyDtoProjectionFailsClosedWithoutRepository 钉死未装配仓储时失败关闭：
// 返回明确错误，不用空投影冒充"无额度"。
func TestQuotaKeyDtoProjectionFailsClosedWithoutRepository(t *testing.T) {
	svc := &QuotaKeyService{}
	_, err := svc.LoadQuotaKeyUsageProjections(context.Background(), []int64{1})
	require.Error(t, err)

	_, err = svc.ListWithUsageProjections(context.Background())
	require.Error(t, err)
}

// parseUsageSnapshot 是新增用例的便捷封装：把原始 JSON 解析为类型化快照。
func parseUsageSnapshot(t *testing.T, raw string) QuotaKeyUsageSnapshot {
	t.Helper()
	snapshot, ok := ParseQuotaKeyUsageSnapshot([]byte(raw))
	require.True(t, ok, "新增用例的快照应当可被解析为有效快照")
	return snapshot
}

// TestQuotaKeyUsageExhaustedUSD 覆盖 usd_cent 口径的耗尽判定：
// 今日剩余低于 0.5 USD（50 分）即耗尽，等于或高于下限则不耗尽。
func TestQuotaKeyUsageExhaustedUSD(t *testing.T) {
	below := parseUsageSnapshot(t, `{"total":29048,"used":0,"today_remaining_cent":48,"unit":"usd_cent"}`)
	require.True(t, QuotaKeyUsageExhausted(below), "today_remaining_cent=48 < 50 → 耗尽")

	floor := parseUsageSnapshot(t, `{"total":29048,"used":0,"today_remaining_cent":50,"unit":"usd_cent"}`)
	require.False(t, QuotaKeyUsageExhausted(floor), "today_remaining_cent=50 == 下限 → 未耗尽")

	above := parseUsageSnapshot(t, `{"total":29048,"used":0,"today_remaining_cent":51,"unit":"usd_cent"}`)
	require.False(t, QuotaKeyUsageExhausted(above), "today_remaining_cent=51 > 下限 → 未耗尽")
}

// TestQuotaKeyUsageExhaustedCount 覆盖份额制（默认/count）口径的耗尽判定：
// 仍按既有 (*Total - *Used) < cursorQuotaKeyRemainingFloor（绝对值 10）。
func TestQuotaKeyUsageExhaustedCount(t *testing.T) {
	notExhausted := parseUsageSnapshot(t, `{"total":100,"used":90}`)
	require.False(t, QuotaKeyUsageExhausted(notExhausted), "份额制剩余 10 不小于 10 → 未耗尽")

	exhausted := parseUsageSnapshot(t, `{"total":100,"used":91}`)
	require.True(t, QuotaKeyUsageExhausted(exhausted), "份额制剩余 9 < 10 → 耗尽")
}

// TestQuotaKeyUsageExhaustedUSDFailsClosedWhenTodayMissingOrZero 钉死 KNCY-M 修复：
// USD 口径（unit=usd_cent）一律不回落份额制判定——「今日剩余」缺失或归零时按"今日额度
// 不可用"失败关闭，绝不允许用整卡余额（total-used）证明今天可用。
//
// 对应派发单断言：
//   - usd_cent 且 today_remaining_cent 缺失 → 耗尽 = true；
//   - usd_cent 且 today_remaining_cent:0 → 耗尽 = true；
//   - usd_cent 且 today_remaining_cent:50 → 耗尽 = false（下限）。
func TestQuotaKeyUsageExhaustedUSDFailsClosedWhenTodayMissingOrZero(t *testing.T) {
	// 缺失今日剩余：即便整卡余额充足（total=29048），也不得回落 total-used 放行，
	// 必须失败关闭（返回 true）。这是 KNCY-M 修复前会误判"未耗尽"的漏洞。
	missing := parseUsageSnapshot(t, `{"total":29048,"used":0,"unit":"usd_cent"}`)
	require.True(t, QuotaKeyUsageExhausted(missing),
		"usd_cent 缺失今日剩余必须失败关闭，不得回落整卡余额")

	// 今日剩余 = 0：今日额度已归零，必须耗尽。
	zero := parseUsageSnapshot(t, `{"total":29048,"used":0,"today_remaining_cent":0,"unit":"usd_cent"}`)
	require.True(t, QuotaKeyUsageExhausted(zero), "usd_cent 今日剩余=0 必须耗尽")

	// 今日剩余 = 50（下限）：等于下限，未耗尽（回落分支不触发，本就走今日剩余判定）。
	floor := parseUsageSnapshot(t, `{"total":29048,"used":0,"today_remaining_cent":50,"unit":"usd_cent"}`)
	require.False(t, QuotaKeyUsageExhausted(floor), "usd_cent 今日剩余=50 == 下限 → 未耗尽")
}

// TestQuotaKeyUsageExhaustedUSDNegativeNormalizedFailsClosed 钉死"上游负今日余额"端到端处理：
// 负 today_remaining_cent 经 normalizeQuotaCounter 归一化为 nil，usd_cent 口径缺失 → 失败关闭。
// 即便整卡余额为正（total=29048），负今日余额绝不参与判定、绝不证明今日可用。
func TestQuotaKeyUsageExhaustedUSDNegativeNormalizedFailsClosed(t *testing.T) {
	// 直接构造一份"负值被归一化为 nil"的快照：today_remaining_cent 字段为 -1 在解析时被丢弃。
	negativeInDoc := BuildQuotaKeyUsageProjection([]byte(`{"total":29048,"used":0,"today_remaining_cent":-1,"unit":"usd_cent"}`))
	require.NotNil(t, negativeInDoc, "负数计数字段按缺失处理，不抹掉整卡展示值")
	require.Nil(t, negativeInDoc.TodayRemaining, "负今日剩余必须按缺失处理（不出现今日 USD 展示）")

	snapshot, ok := ParseQuotaKeyUsageSnapshot([]byte(`{"total":29048,"used":0,"today_remaining_cent":-1,"unit":"usd_cent"}`))
	require.True(t, ok)
	require.Nil(t, snapshot.TodayRemainingCent, "负 today_remaining_cent 必须归一化为 nil（缺失）")
	require.Equal(t, "usd_cent", snapshot.Unit)
	require.True(t, QuotaKeyUsageExhausted(snapshot),
		"usd_cent 负今日余额归一化后缺失 → 失败关闭，绝不用整卡余额放行")
}

// TestQuotaKeyUsageProjectionUSD 覆盖 usd_cent 快照的 USD 展示投影。
func TestQuotaKeyUsageProjectionUSD(t *testing.T) {
	projection := BuildQuotaKeyUsageProjection([]byte(`{"total":29048,"used":0,"today_remaining_cent":48,"unit":"usd_cent"}`))
	require.NotNil(t, projection)
	require.NotNil(t, projection.TodayRemaining, "usd_cent 应给出今日剩余 USD")
	require.Equal(t, 0.48, *projection.TodayRemaining)
	require.NotNil(t, projection.TotalRemaining, "usd_cent 应给出总额剩余 USD")
	require.Equal(t, 290.48, *projection.TotalRemaining)
	require.Equal(t, "usd", projection.RemainingUnit)
}

// TestQuotaKeyUsageProjectionCountOmitsUSD 覆盖份额制快照：不出现 USD 展示字段。
func TestQuotaKeyUsageProjectionCountOmitsUSD(t *testing.T) {
	projection := BuildQuotaKeyUsageProjection([]byte(`{"total":100,"used":90}`))
	require.NotNil(t, projection)
	require.Nil(t, projection.TodayRemaining, "份额制不出现 today_remaining")
	require.Nil(t, projection.TotalRemaining, "份额制不出现 total_remaining")
	require.Empty(t, projection.RemainingUnit, "份额制不出现 remaining_unit")
}
