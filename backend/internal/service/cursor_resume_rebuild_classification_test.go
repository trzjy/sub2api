//go:build unit

package service

// 续轮资格拒绝的**「两类」定性**（2026-11-04 用户裁定）。
//
// 背景：九步核验原本把八种完全不同的原因压成同一句"账号已不可用，请重新发起请求"，
// 全都回显给下游用户。用户裁定："本来可以站点侧重试或者换号请求的问题，不能推给下游"。
//
// 于是把拒绝分成两类，这个文件把分类钉死：
//
//	A 类 真用户错误（②请求平台不允许 / ③账号已不在原分组）：请求本身就送不到这个号上，
//	   换任何号结果都一样 → 必须如实报错，不得装作能换号。
//	B 类 站点侧问题（①④⑤⑥⑦⑧⑨）：号被删/被停/卡过期/卡额度用完/卡被改/卡读不到/
//	   映射被改/指纹缺失——换一个还有额度的号就能接着干 → 一律标记 Rebuild，
//	   由 guardResumeEligibility 终结旧会话后在同一 HTTP 内换号重建。
//
// 断言三个不变量：
//  1. 分类正确：A 类 IsRebuild()==false，B 类 IsRebuild()==true。
//  2. 换号原因可分辨：额度类走 CursorQuotaExhaustedReason（全池都这样时才由 CUR-RES-11
//     说明容量），其余走 CursorUpstreamAccountFailureReason。
//  3. 接线正确：guardResumeEligibility 对 B 类返回 CursorResumeRebuildError（不再回显），
//     对 A 类与「历史不完整」仍返回原拒绝。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// cursorEligibilitySessionWith 登记会话，并允许按用例改写 spec（例如抹掉首次指纹）。
func cursorEligibilitySessionWith(svc *CursorExecutionService, code string, mutate func(*cursorSessionSpec)) *cursorSessionRecord {
	spec := cursorEligibilitySpec(code)
	if mutate != nil {
		mutate(&spec)
	}
	return svc.sessions.start(spec)
}

// cursorResumeClassCase 是一个"九步里的某一步 → 该不该换号"的定性用例。
type cursorResumeClassCase struct {
	name string
	// prepare 构造本用例的执行部件、旧会话与账号（依赖与生产同构，仅 transport 是替身）。
	prepare func(t *testing.T) (*CursorExecutionService, *cursorSessionRecord, *Account)
	// 拒绝的对外形状（状态码与稳定原因串，日志与既有断言沿用）。
	wantStatus   int
	wantReason   string
	wantSentinel error
	// wantRebuild 是从用户视角唯一重要的结论：这次拒绝该不该换成换号重建。
	wantRebuild bool
	// wantRebuildReason 只在 wantRebuild 时有意义：换号时上抛的失败分类。
	wantRebuildReason GatewayFailureReason
}

func TestVerifyResumeEligibilityClassifiesDenialsForRebuild(t *testing.T) {
	cases := []cursorResumeClassCase{
		{
			// ① 号被删 / 不再是 cursor 号：站点侧资源变化。
			name: "账号已不是 cursor 号_换号重建",
			prepare: func(t *testing.T) (*CursorExecutionService, *cursorSessionRecord, *Account) {
				installCursorEligibilitySource(t, nil)
				svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
				sess := cursorEligibilitySession(svc, cursorEligibilityCode)
				account := cursorEligibilityAccount()
				account.Platform = PlatformOpenAI
				return svc, sess, account
			},
			wantStatus: 403, wantReason: "account_unavailable",
			wantSentinel: ErrCursorResumeAccountIneligible,
			wantRebuild:  true, wantRebuildReason: CursorUpstreamAccountFailureReason,
		},
		{
			// ② 请求平台准入不通过：请求本身就送不到这个号上，换号也一样。
			name: "请求平台不允许_如实报错",
			prepare: func(t *testing.T) (*CursorExecutionService, *cursorSessionRecord, *Account) {
				installCursorEligibilitySource(t, nil)
				svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
				sess := cursorEligibilitySessionWith(svc, cursorEligibilityCode, func(spec *cursorSessionSpec) {
					spec.indexProtocol = "grpc-not-admitted"
				})
				return svc, sess, cursorEligibilityAccount()
			},
			wantStatus: 403, wantReason: "platform_not_admitted",
			wantSentinel: ErrCursorResumeAccountIneligible,
			wantRebuild:  false,
		},
		{
			// ③ 账号已不在原分组：本次请求的分组关系不成立，换号也解决不了。
			name: "账号已退出原分组_如实报错",
			prepare: func(t *testing.T) (*CursorExecutionService, *cursorSessionRecord, *Account) {
				installCursorEligibilitySource(t, nil)
				svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
				sess := cursorEligibilitySession(svc, cursorEligibilityCode)
				account := cursorEligibilityAccount()
				const otherGroup = int64(7802)
				account.AccountGroups = []AccountGroup{{AccountID: cursorEligibilityAccountID, GroupID: otherGroup}}
				account.GroupIDs = []int64{otherGroup}
				return svc, sess, account
			},
			wantStatus: 403, wantReason: "group_not_allowed",
			wantSentinel: ErrCursorResumeAccountIneligible,
			wantRebuild:  false,
		},
		{
			// ④ 号被停用：站点侧，换号。
			name: "账号停用_换号重建",
			prepare: func(t *testing.T) (*CursorExecutionService, *cursorSessionRecord, *Account) {
				installCursorEligibilitySource(t, nil)
				svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
				sess := cursorEligibilitySession(svc, cursorEligibilityCode)
				account := cursorEligibilityAccount()
				account.Status = StatusDisabled
				return svc, sess, account
			},
			wantStatus: 403, wantReason: "not_schedulable",
			wantSentinel: ErrCursorResumeAccountIneligible,
			wantRebuild:  true, wantRebuildReason: CursorUpstreamAccountFailureReason,
		},
		{
			// ④ 号临时不可调度（上游 401 冷却中）：站点侧，换号。
			name: "账号临停_换号重建",
			prepare: func(t *testing.T) (*CursorExecutionService, *cursorSessionRecord, *Account) {
				installCursorEligibilitySource(t, nil)
				svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
				sess := cursorEligibilitySession(svc, cursorEligibilityCode)
				account := cursorEligibilityAccount()
				until := time.Now().Add(10 * time.Minute)
				account.TempUnschedulableUntil = &until
				account.TempUnschedulableReason = "upstream_401"
				return svc, sess, account
			},
			wantStatus: 403, wantReason: "not_schedulable",
			wantSentinel: ErrCursorResumeAccountIneligible,
			wantRebuild:  true, wantRebuildReason: CursorUpstreamAccountFailureReason,
		},
		{
			// ⑤ 会话没固定首次来源指纹：站内读数不全，换号重建。
			name: "首次指纹缺失_换号重建",
			prepare: func(t *testing.T) (*CursorExecutionService, *cursorSessionRecord, *Account) {
				installCursorEligibilitySource(t, nil)
				svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
				sess := cursorEligibilitySessionWith(svc, cursorEligibilityCode, func(spec *cursorSessionSpec) {
					spec.sourceQuotaKeyID = 0
				})
				return svc, sess, cursorEligibilityAccount()
			},
			wantStatus: 400, wantReason: "source_fingerprint_unavailable",
			wantSentinel: ErrCursorSourceFingerprintUnavailable,
			wantRebuild:  true, wantRebuildReason: CursorUpstreamAccountFailureReason,
		},
		{
			// ⑥ 号被运维换了绑卡：站点侧变化，换号（新号按自己的卡解析）。
			name: "账号被换绑卡_换号重建",
			prepare: func(t *testing.T) (*CursorExecutionService, *cursorSessionRecord, *Account) {
				installCursorEligibilitySource(t, nil)
				svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
				sess := cursorEligibilitySession(svc, cursorEligibilityCode)
				account := cursorEligibilityAccount()
				account.QuotaKeyID = qkID(cursorEligibilityOtherKey)
				return svc, sess, account
			},
			wantStatus: 400, wantReason: "source_changed",
			wantSentinel: ErrCursorResumeSourceChanged,
			wantRebuild:  true, wantRebuildReason: CursorUpstreamAccountFailureReason,
		},
		{
			// ⑦ 卡内容与首次记录不符（卡被改过）：站点侧变化，换号。
			name: "激活码被改_换号重建",
			prepare: func(t *testing.T) (*CursorExecutionService, *cursorSessionRecord, *Account) {
				installCursorEligibilitySource(t, nil)
				// 库里已是新码，会话固定的是旧码的 MAC → 不等。
				svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityNewCode))
				sess := cursorEligibilitySession(svc, cursorEligibilityCode)
				return svc, sess, cursorEligibilityAccount()
			},
			wantStatus: 400, wantReason: "source_changed",
			wantSentinel: ErrCursorResumeSourceChanged,
			wantRebuild:  true, wantRebuildReason: CursorUpstreamAccountFailureReason,
		},
		{
			// ⑦ 卡行读不到（DB/解密故障）：站内读不到，与⑤⑧同处置，绝不说成用户账号问题。
			name: "卡行读不到_换号重建",
			prepare: func(t *testing.T) (*CursorExecutionService, *cursorSessionRecord, *Account) {
				installCursorEligibilitySource(t, nil)
				repo := cursorEligibilityFailRepo{cursorOutboundRepo: cursorEligibilityRepo(cursorEligibilityCode)}
				svc := newCursorEligibilityService(t, repo)
				sess := cursorEligibilitySession(svc, cursorEligibilityCode)
				return svc, sess, cursorEligibilityAccount()
			},
			wantStatus: 503, wantReason: "source_unavailable",
			wantSentinel: ErrCursorResumeSourceUnavailable,
			wantRebuild:  true, wantRebuildReason: CursorUpstreamAccountFailureReason,
		},
		{
			// ⑧ 卡额度用完：最典型的"本该换号却报错"，换号原因必须是额度耗尽。
			name: "卡额度用完_换号重建",
			prepare: func(t *testing.T) (*CursorExecutionService, *cursorSessionRecord, *Account) {
				installCursorEligibilitySource(t, []byte(fmt.Sprintf(
					`{"total":100,"used":100,"status_code":200,"checked_at":%q,"error_code":""}`,
					time.Now().Add(-time.Minute).Format(time.RFC3339),
				)))
				svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
				sess := cursorEligibilitySession(svc, cursorEligibilityCode)
				return svc, sess, cursorEligibilityAccount()
			},
			wantStatus: 403, wantReason: quotaKeyUsageExhausted,
			wantSentinel: ErrCursorResumeSourceBlocked,
			wantRebuild:  true, wantRebuildReason: CursorQuotaExhaustedReason,
		},
		{
			// ⑨ 号上的模型映射被改：站点侧配置变化，换号（新号按自己的映射解析）。
			name: "模型映射被改_换号重建",
			prepare: func(t *testing.T) (*CursorExecutionService, *cursorSessionRecord, *Account) {
				installCursorEligibilitySource(t, nil)
				svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
				sess := cursorEligibilitySession(svc, cursorEligibilityCode)
				account := cursorEligibilityAccount()
				account.Credentials = map[string]any{
					"model_mapping": map[string]any{cursorEligibilityModel: "another-upstream-model"},
					"access_token":  "BEARER-ACCOUNT-ORIGINAL",
				}
				return svc, sess, account
			},
			wantStatus: 400, wantReason: "model_mapping_changed",
			wantSentinel: ErrCursorResumeConfigChanged,
			wantRebuild:  true, wantRebuildReason: CursorUpstreamAccountFailureReason,
		},
	}

	require.NotEmpty(t, cases)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, sess, account := tc.prepare(t)
			denial := svc.verifyResumeEligibility(context.Background(), sess, account)
			requireCursorEligibilityDenial(t, denial, tc.wantStatus, tc.wantReason, tc.wantSentinel)
			require.Equal(t, tc.wantRebuild, denial.IsRebuild(),
				"分类错误：A 类（平台/分组）必须如实报错，B 类（站点侧容量/配置）必须换号重建")
			if tc.wantRebuild {
				require.Equal(t, tc.wantRebuildReason, denial.Rebuild,
					"换号原因必须可分辨：额度类走 quota_exhausted，其余按账号不可用")
			} else {
				require.Empty(t, denial.Rebuild, "A 类不得携带换号标记，否则会被误改成重建")
			}
		})
	}
}

// 来源门能给出的否决串必须逐个归类，且只有"真没额度"的两种算容量类——这条直接决定
// 池子耗尽时对外说的是"没额度"（CUR-RES-11）还是"账号不可用"，混判会让 V3 验收判错。
func TestIsCursorQuotaVetoReasonCoversEverySourceGateReason(t *testing.T) {
	for _, veto := range []string{quotaKeyUsageExhausted, quotaKeyExpired} {
		require.True(t, isCursorQuotaVetoReason(veto), "%q 是真实容量耗尽/失效，必须算容量类", veto)
	}
	for _, veto := range []string{
		quotaKeyUsageUnobserved,        // 从未观测：缺失 ≠ 0
		quotaKeyUsageObservationFailed, // 最近一次观测失败
		quotaKeyUsageStale,             // 读数过期
		quotaKeyUsageIncomplete,        // 字段不全
		quotaKeySourceUnbound,          // 没绑卡
		"quota_key_state_unavailable",  // 卡状态读不到（既有折叠串，无反查常量）
		"",                             // 来源门放行
	} {
		require.False(t, isCursorQuotaVetoReason(veto),
			"%q 是站内配置/观测问题，换号照换，但不得对外声称容量耗尽", veto)
	}
}

// buildCursorRebuildTicket 造一张最小续轮票：只为驱动 guardResumeEligibility 的分流。
func buildCursorRebuildTicket(sess *cursorSessionRecord, account *Account, historyComplete bool) *CursorResumeTicket {
	var probe *CursorResumeProbe
	if historyComplete {
		probe = &CursorResumeProbe{
			Protocol: CursorResumeProtocolChat,
			Model:    cursorEligibilityModel,
			Plan:     &cursorContextPlan{historyComplete: true},
		}
	}
	return &CursorResumeTicket{
		SessionID: sess.id,
		AccountID: account.ID,
		BatchID:   "batch-rebuild-1",
		Model:     cursorEligibilityModel,
		Protocol:  CursorResumeProtocolChat,
		session:   sess,
		probe:     probe,
	}
}

func cursorSessionStateOf(sess *cursorSessionRecord) cursorSessionState {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.state
}

// B 类拒绝必须转成"换号重建"，且旧会话先被终结（否则会在同一账号上留下残留会话）。
func TestGuardResumeEligibilityTurnsAccountDenialIntoRebuild(t *testing.T) {
	installCursorEligibilitySource(t, nil)
	svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()
	account.Status = StatusDisabled

	err := svc.guardResumeEligibility(context.Background(), buildCursorRebuildTicket(sess, account, true), account)

	var rebuild *CursorResumeRebuildError
	require.ErrorAs(t, err, &rebuild, "B 类拒绝必须换成换号重建信号，不能回显给下游")
	require.Equal(t, account.ID, rebuild.DeadAccountID)
	require.Equal(t, CursorUpstreamAccountFailureReason, rebuild.Reason)
	require.Equal(t, cursorSessionClosed, cursorSessionStateOf(sess), "重建前必须把旧会话终结干净")
}

// 历史不完整（典型：Responses 只给 previous_response_id）时不许重建：重开一局会丢上下文，
// 比报错更糟。这类仍按原拒绝返回，且旧会话同样要终结。
func TestGuardResumeEligibilityRefusesRebuildWhenHistoryIncomplete(t *testing.T) {
	installCursorEligibilitySource(t, nil)
	svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()
	account.Status = StatusDisabled

	err := svc.guardResumeEligibility(context.Background(), buildCursorRebuildTicket(sess, account, false), account)

	var rebuild *CursorResumeRebuildError
	require.NotErrorAs(t, err, &rebuild, "历史不完整时不得重建（会丢上下文）")
	status, reason := CursorResumeHTTPStatus(err)
	require.Equal(t, 403, status)
	require.Equal(t, "not_schedulable", reason)
	require.Equal(t, cursorSessionClosed, cursorSessionStateOf(sess))
}

// A 类拒绝（分组不符）走原路：不重建、不换号，如实报错。
func TestGuardResumeEligibilityKeepsUserErrorDenial(t *testing.T) {
	installCursorEligibilitySource(t, nil)
	svc := newCursorEligibilityService(t, cursorEligibilityRepo(cursorEligibilityCode))
	sess := cursorEligibilitySession(svc, cursorEligibilityCode)
	account := cursorEligibilityAccount()
	const otherGroup = int64(7802)
	account.AccountGroups = []AccountGroup{{AccountID: cursorEligibilityAccountID, GroupID: otherGroup}}
	account.GroupIDs = []int64{otherGroup}

	err := svc.guardResumeEligibility(context.Background(), buildCursorRebuildTicket(sess, account, true), account)

	var rebuild *CursorResumeRebuildError
	require.NotErrorAs(t, err, &rebuild)
	status, reason := CursorResumeHTTPStatus(err)
	require.Equal(t, 403, status)
	require.Equal(t, "group_not_allowed", reason)
}

// M2 的排除集合合并已由 cursor_resume_quota_failover_test.go 的
// TestCursorRebuildExcludedIDs 覆盖（含"不得改写调用方 map"与幂等），此处不重复。
