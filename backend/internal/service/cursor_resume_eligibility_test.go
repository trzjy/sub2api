//go:build unit

package service

// §25.17.3「A05续轮顺序」资格核验（verifyResumeEligibility）的定向测试。
//
// 钉死六件事：
//
//  1. 账号未变 + 来源未变 + 映射未变 → 资格通过（返回 nil），且**不**因"旧会话仍占着
//     并发 1 的槽"而被否决：本谓词不持有选号器/并发/预留依赖，也不调用它们。
//  2. 停用 / 临停 / 模型级临停 / 退出原分组 → 403，各自原因串可分辨。
//  3. 改绑定（QuotaKeyID 变了）/ 换码（同一行的激活码变了）→ 400 失效关联。
//  4. 来源读取失败 → 503（"读不到"≠"明确不合格"），绝不折叠成 400/403。
//  5. A04 共同来源门被明确否决（耗尽）→ 403，原因串沿用来源门自己的取值。
//  6. 改模型映射 → 400；拒绝不写会话状态（spec 指纹原样保留）。
//
// 全部断言走包内既有夹具（stub repo / 包级来源读取器安装），不连 DB、不引 mock 框架。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

const (
	cursorEligibilityAccountID = int64(6601)
	cursorEligibilityGroupID   = int64(7801)
	cursorEligibilityQuotaKey  = int64(31)
	cursorEligibilityOtherKey  = int64(32)
	cursorEligibilityModel     = "cursor-sonnet"
	cursorEligibilityUpstream  = "upstream-cursor-sonnet"
	cursorEligibilityCode      = "ACTIVATION-CODE-THIRTYONE"
	cursorEligibilityNewCode   = "ACTIVATION-CODE-ROTATED"
)

// cursorEligibilityFailRepo 让来源读取失败（模拟 DB/依赖故障），其余方法沿用既有替身。
type cursorEligibilityFailRepo struct {
	*cursorOutboundRepo
}

func (r cursorEligibilityFailRepo) GetByID(context.Context, int64) (*ent.QuotaKey, error) {
	return nil, errors.New("quota key store unavailable")
}

// newCursorEligibilityService 构造唯一执行部件（依赖与生产同构，仅 transport 为替身）。
func newCursorEligibilityService(t *testing.T, repo QuotaKeyRepository) *CursorExecutionService {
	t.Helper()
	return newCursorTestExecutionService(t, repo, NewCursorTestEncryptor(),
		cursorTestDeviceIDs(repo), NewCursorUpstreamForwarder())
}

// cursorEligibilityRepo 构造一条来源行（CodeEncrypted 即明文激活码）。
func cursorEligibilityRepo(code string) *cursorOutboundRepo {
	return newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		cursorEligibilityQuotaKey: {ID: cursorEligibilityQuotaKey, CodeEncrypted: code},
		cursorEligibilityOtherKey: {ID: cursorEligibilityOtherKey, CodeEncrypted: "ACTIVATION-CODE-THIRTYTWO"},
	})
}

// cursorEligibilitySession 登记一条已固定首次来源指纹的会话（并发 1 的原会话）。
func cursorEligibilitySession(svc *CursorExecutionService, code string) *cursorSessionRecord {
	return svc.sessions.start(cursorSessionSpec{
		protocol:         cursorProtocolChat,
		requestedModel:   cursorEligibilityModel,
		upstreamModel:    cursorEligibilityUpstream,
		accountID:        cursorEligibilityAccountID,
		quotaKeyID:       cursorEligibilityQuotaKey,
		sourceQuotaKeyID: cursorEligibilityQuotaKey,
		sourceCodeMAC:    quotaKeyMACOfPlaintext(code),
		identity: CursorSessionIdentity{
			UserID:   9101,
			APIKeyID: 9102,
			GroupID:  cursorEligibilityGroupID,
		},
	})
}

// cursorEligibilityAccount 构造原账号：显式挂组 + 声明模型映射 + 绑定来源。
func cursorEligibilityAccount() *Account {
	return &Account{
		ID:            cursorEligibilityAccountID,
		Platform:      PlatformCursor,
		Type:          AccountTypeAPIKey,
		Status:        StatusActive,
		Schedulable:   true,
		Concurrency:   1,
		QuotaKeyID:    qkID(cursorEligibilityQuotaKey),
		AccountGroups: []AccountGroup{{AccountID: cursorEligibilityAccountID, GroupID: cursorEligibilityGroupID}},
		GroupIDs:      []int64{cursorEligibilityGroupID},
		Credentials: map[string]any{
			"model_mapping": map[string]any{cursorEligibilityModel: cursorEligibilityUpstream},
			"access_token":  "BEARER-ACCOUNT-ORIGINAL",
		},
	}
}

// installCursorEligibilitySource 安装来源读取器：状态 active + 新鲜快照（A04 门放行）。
func installCursorEligibilitySource(t *testing.T, snapshot []byte) {
	t.Helper()
	if snapshot == nil {
		snapshot = []byte(fmt.Sprintf(
			`{"total":400,"used":86,"status_code":200,"checked_at":%q,"error_code":""}`,
			time.Now().Add(-time.Minute).Format(time.RFC3339),
		))
	}
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses:  map[int64]string{cursorEligibilityQuotaKey: domain.QuotaKeyStatusActive},
		snapshots: map[int64][]byte{cursorEligibilityQuotaKey: snapshot},
	})
}

// requireCursorEligibilityDenial 断言拒绝的状态/reason/sentinel，并证明它沿既有续轮
// 错误链（CursorResumeHTTPStatus 能取回同一对取值）。
func requireCursorEligibilityDenial(t *testing.T, denial *CursorResumeDenial, status int, reason string, sentinel error) {
	t.Helper()
	require.NotNil(t, denial, "必须返回明确拒绝（nil 代表资格通过）")
	require.Equal(t, status, denial.Status)
	require.Equal(t, reason, denial.Reason)
	require.ErrorIs(t, error(denial), sentinel)
	gotStatus, gotReason := CursorResumeHTTPStatus(denial)
	require.Equal(t, status, gotStatus, "拒绝必须沿既有续轮错误链给出状态")
	require.Equal(t, reason, gotReason)
}

// ---------------------------------------------------------------------------
// 1. 账号未变：资格通过（并发 1、旧会话占槽也不否决）
// ---------------------------------------------------------------------------

func TestVerifyResumeEligibilityAllowsUnchangedAccountUnderSingleConcurrency(t *testing.T) {
	installCursorEligibilitySource(t, nil)
	svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()

	// 并发 1 + 旧会话仍占槽：资格核验不持有选号器/并发/预留依赖，故不可能用
	// "并发已满"否决自己仍占槽的合法续轮（若将来在此加 Acquire，无法编译）。
	account.Concurrency = 1

	require.Nil(t, svc.verifyResumeEligibility(context.Background(), sess, account),
		"账号/来源/映射均未改变时必须通过资格核验")
}

// ---------------------------------------------------------------------------
// 2. 账号本体不再可用：停用 / 临停 / 模型级临停 / 移组
// ---------------------------------------------------------------------------

func TestVerifyResumeEligibilityDeniesDisabledAccount(t *testing.T) {
	installCursorEligibilitySource(t, nil)
	svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()
	account.Status = StatusDisabled

	requireCursorEligibilityDenial(t, svc.verifyResumeEligibility(context.Background(), sess, account),
		403, "not_schedulable", ErrCursorResumeAccountIneligible)
}

func TestVerifyResumeEligibilityDeniesTemporarilyUnschedulableAccount(t *testing.T) {
	installCursorEligibilitySource(t, nil)
	svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()
	until := time.Now().Add(10 * time.Minute)
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = "upstream_401"

	requireCursorEligibilityDenial(t, svc.verifyResumeEligibility(context.Background(), sess, account),
		403, "not_schedulable", ErrCursorResumeAccountIneligible)
}

func TestVerifyResumeEligibilityDeniesModelLevelSuspension(t *testing.T) {
	installCursorEligibilitySource(t, nil)
	svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()
	// 账号本体可调度，仅该模型被熔断：原因串必须是 model_rate_limited（与调度链同口径）。
	account.Extra = map[string]any{
		modelRateLimitsKey: map[string]any{
			cursorEligibilityUpstream: map[string]any{
				"rate_limit_reset_at": time.Now().Add(5 * time.Minute).Format(time.RFC3339),
			},
		},
	}

	requireCursorEligibilityDenial(t, svc.verifyResumeEligibility(context.Background(), sess, account),
		403, "model_rate_limited", ErrCursorResumeAccountIneligible)
}

func TestVerifyResumeEligibilityDeniesAccountMovedOutOfGroup(t *testing.T) {
	installCursorEligibilitySource(t, nil)
	svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()
	// 本次读取的账号已只在别的组（移组）：组关系必须以当前查询为准。
	const otherGroup = int64(7802)
	account.AccountGroups = []AccountGroup{{AccountID: cursorEligibilityAccountID, GroupID: otherGroup}}
	account.GroupIDs = []int64{otherGroup}

	requireCursorEligibilityDenial(t, svc.verifyResumeEligibility(context.Background(), sess, account),
		403, "group_not_allowed", ErrCursorResumeAccountIneligible)
}

// ---------------------------------------------------------------------------
// 3. 来源改变：改绑定 / 换码（400，失效关联）
// ---------------------------------------------------------------------------

func TestVerifyResumeEligibilityDeniesReboundActivationSource(t *testing.T) {
	installCursorEligibilitySource(t, nil)
	svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()
	account.QuotaKeyID = qkID(cursorEligibilityOtherKey)

	requireCursorEligibilityDenial(t, svc.verifyResumeEligibility(context.Background(), sess, account),
		400, "source_changed", ErrCursorResumeSourceChanged)
}

func TestVerifyResumeEligibilityDeniesRotatedActivationCode(t *testing.T) {
	installCursorEligibilitySource(t, nil)
	// 来源行已被换码：行 ID 不变，激活码明文不同 → MAC 不等。
	svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityNewCode))
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()

	denial := svc.verifyResumeEligibility(context.Background(), sess, account)
	requireCursorEligibilityDenial(t, denial, 400, "source_changed", ErrCursorResumeSourceChanged)

	// 拒绝是只读的：会话固定的首次指纹与会话身份都不被改写。
	sess.mu.Lock()
	fixedMAC := sess.spec.sourceCodeMAC
	fixedKey := sess.spec.sourceQuotaKeyID
	state := sess.state
	sess.mu.Unlock()
	require.Equal(t, quotaKeyMACOfPlaintext(cursorEligibilityCode), fixedMAC,
		"换码不得把旧头改标成新 MAC")
	require.Equal(t, cursorEligibilityQuotaKey, fixedKey)
	require.Equal(t, cursorSessionRunning, state)
}

// ---------------------------------------------------------------------------
// 4. 依赖读取失败：503（读不到 ≠ 明确不合格）
// ---------------------------------------------------------------------------

func TestVerifyResumeEligibilityDeniesWhenSourceReadFails(t *testing.T) {
	installCursorEligibilitySource(t, nil)
	repo := cursorEligibilityFailRepo{cursorOutboundRepo: cursorEligibilityRepo(cursorEligibilityCode)}
	svc := newCursorEligibilityService(t, repo)
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()

	requireCursorEligibilityDenial(t, svc.verifyResumeEligibility(context.Background(), sess, account),
		503, "source_unavailable", ErrCursorResumeSourceUnavailable)
}

// ---------------------------------------------------------------------------
// 5. A04 共同来源门被明确否决：403（原因串沿用来源门取值）
// ---------------------------------------------------------------------------

func TestVerifyResumeEligibilityDeniesWhenSourceExhausted(t *testing.T) {
	installCursorEligibilitySource(t, []byte(fmt.Sprintf(
		`{"total":100,"used":100,"status_code":200,"checked_at":%q,"error_code":""}`,
		time.Now().Add(-time.Minute).Format(time.RFC3339),
	)))
	svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()

	requireCursorEligibilityDenial(t, svc.verifyResumeEligibility(context.Background(), sess, account),
		403, quotaKeyUsageExhausted, ErrCursorResumeSourceBlocked)
}

// ---------------------------------------------------------------------------
// 6. 模型映射改变：400（不换号、不重建 AgentRun）
// ---------------------------------------------------------------------------

func TestVerifyResumeEligibilityDeniesWhenModelMappingChanged(t *testing.T) {
	installCursorEligibilitySource(t, nil)
	svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()
	account.Credentials = map[string]any{
		"model_mapping": map[string]any{cursorEligibilityModel: "another-upstream-model"},
		"access_token":  "BEARER-ACCOUNT-ORIGINAL",
	}

	requireCursorEligibilityDenial(t, svc.verifyResumeEligibility(context.Background(), sess, account),
		400, "model_mapping_changed", ErrCursorResumeConfigChanged)
}
