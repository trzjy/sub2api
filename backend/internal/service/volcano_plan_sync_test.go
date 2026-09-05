package service

// 火山订阅号同步编排测试：预览不动库、部分确认只新增（保留旧托管）、完全确认替换、
// 绝不删除人工 alias 映射、K3 发现、失败关闭/凭证终止不落库。复用
// newVolcanoSyncService（官方文档夹具 + 探活桩）与火山专用探活桩。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// volcanoPlanSyncRepoStub 记录 UpdateExtra / UpdateCredentials 调用（嵌入 nil 接口，
// 任何其它方法调用都会 panic——被测路径只触碰这两者）。
type volcanoPlanSyncRepoStub struct {
	AccountRepository
	mu              sync.Mutex
	extraUpdates    []map[string]any
	credUpdates     []map[string]any // 每次 UpdateCredentials 的凭据
	updateExtraCall int
	updateCredCall  int
	credErr         error // 非 nil 时 UpdateCredentials 返回此错误（模拟映射持久化失败）
	extraErr        error // 非 nil 时 UpdateExtra 返回此错误（模拟托管快照持久化失败）
}

func (r *volcanoPlanSyncRepoStub) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateExtraCall++
	if r.extraErr != nil {
		return r.extraErr
	}
	r.extraUpdates = append(r.extraUpdates, updates)
	return nil
}

func (r *volcanoPlanSyncRepoStub) UpdateCredentials(_ context.Context, _ int64, creds map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateCredCall++
	if r.credErr != nil {
		return r.credErr
	}
	cp := map[string]any{}
	for k, v := range creds {
		cp[k] = v
	}
	r.credUpdates = append(r.credUpdates, cp)
	return nil
}

func (r *volcanoPlanSyncRepoStub) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updateExtraCall, r.updateCredCall
}

// volcanoSyncAccount 构造火山订阅号账号；mapping 为 model_mapping（可空串联富集）。
func volcanoSyncAccount(baseURL, protocol string, mapping map[string]any) *Account {
	if mapping == nil {
		mapping = map[string]any{}
	}
	return &Account{
		ID:       101,
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       "ark-key",
			"base_url":      baseURL,
			"api_protocol":  protocol,
			"model_mapping": mapping,
		},
	}
}

// allOKRespond 恒返回 HTTP 200 有效响应（OpenAI 协议 shape）。coding_chat_protocol()。
func allOKRespond(_ *http.Request, model string) (int, string, error) {
	_ = model
	return 200, `{"choices":[{"message":{"content":"hi"}}]}`, nil
}

func newVolcanoSyncSvc(t *testing.T, stub *volcanoSyncHTTPStub, repo *volcanoPlanSyncRepoStub) *AccountTestService {
	svc := newVolcanoSyncService(t, stub)
	if repo != nil {
		svc.accountRepo = repo
	}
	return svc
}

// codingCandidates 直接来自官方文档夹具：个人版∪企业版并集（9 个可直调文本模型）。
var codingCandidates = []string{
	"deepseek-v4-flash",
	"deepseek-v4-pro",
	"doubao-seed-2.0-lite",
	"doubao-seed-2.1-turbo",
	"doubao-seed-evolving",
	"glm-5.3",
	"glm-5.3-flash",
	"kimi-k2.7-code",
	"minimax-m3",
}

// TestVolcanoSyncPlanPreviewDoesNotApply 验证 apply=false 只返回分类与差异，绝不动库。
func TestVolcanoSyncPlanPreviewDoesNotApply(t *testing.T) {
	t.Parallel()

	repo := &volcanoPlanSyncRepoStub{}
	stub := &volcanoSyncHTTPStub{respond: allOKRespond}
	svc := newVolcanoSyncSvc(t, stub, repo)
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions", nil)

	result, err := svc.SyncVolcanoPlanModels(context.Background(), account, false, nil)
	require.NoError(t, err)
	require.False(t, result.Applied)
	require.True(t, result.FullConfirm)
	require.Equal(t, codingCandidates, result.Confirmed)
	require.Empty(t, result.Unavailable)
	require.Empty(t, result.Unverified)
	require.Empty(t, result.WillRemove)
	// 新账号：9 个候选全部将新增为 identity 键。
	require.Equal(t, codingCandidates, result.WillAdd)

	extraCalls, credCalls := repo.counts()
	require.Zero(t, extraCalls, "preview 不得写 extra 快照")
	require.Zero(t, credCalls, "preview 不得写 model_mapping")
}

// TestVolcanoSyncPlanFullReplacePreservesManualAlias 验证完全确认→托管替换为新候选集，
// 人工 alias 键（非 identity）绝不删除；被 alias 覆盖的上游模型仍全量探活并进入
// Confirmed，但不重复落 identity 键。
func TestVolcanoSyncPlanFullReplacePreservesManualAlias(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	probed := map[string]bool{}
	repo := &volcanoPlanSyncRepoStub{}
	stub := &volcanoSyncHTTPStub{
		respond: func(req *http.Request, model string) (int, string, error) {
			mu.Lock()
			probed[model] = true
			mu.Unlock()
			return allOKRespond(req, model)
		},
	}
	svc := newVolcanoSyncSvc(t, stub, repo)
	// 人工 alias：my-alias -> glm-5.3（glm-5.3 只是 mapping 目标值）。
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions",
		map[string]any{"my-alias": "glm-5.3"})

	result, err := svc.SyncVolcanoPlanModels(context.Background(), account, true, nil)
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.True(t, result.FullConfirm)

	// 全量探活：被 alias 覆盖的 glm-5.3 仍重新探活并进入 Confirmed；9 个候选全部确认。
	mu.Lock()
	require.True(t, probed["glm-5.3"], "被 alias 覆盖的候选仍应重新探活")
	mu.Unlock()
	require.Equal(t, codingCandidates, result.Confirmed)
	// glm-5.3 已被 alias 目标值覆盖，不重复加 identity 键；其余 8 个新增。
	require.NotContains(t, result.WillAdd, "glm-5.3")
	require.Len(t, result.WillAdd, 8)

	extraCalls, credCalls := repo.counts()
	require.Equal(t, 1, extraCalls, "完全确认应用应写一次托管快照")
	require.Equal(t, 1, credCalls, "完全确认应用应写一次 model_mapping")

	// 落库后的 model_mapping：人工 alias 保留，identity 键 8 个，无重复 glm-5.3。
	newMapping, ok := account.Credentials["model_mapping"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "glm-5.3", newMapping["my-alias"], "人工 alias 永不删除")
	_, hasIdentityGLM := newMapping["glm-5.3"]
	require.False(t, hasIdentityGLM, "被 alias 覆盖的上游模型不应重复身份键")
	sortedKeys := make([]string, 0, len(newMapping))
	for k := range newMapping {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	require.Equal(t, 9, len(sortedKeys), "8 identity + 1 alias")

	// 托管快照 = 完整候选集（含 glm-5.3）。
	snap := account.GetVolcanoPlanManagedModels()
	require.NotNil(t, snap)
	require.Equal(t, codingCandidates, snap.Models)
	require.NotZero(t, snap.SyncedAt)
}

// TestVolcanoSyncPlanPartialKeepsOldManaged 验证部分确认（某候选未探通）→ 托管集合 =
// 旧托管 ∪ 新 confirmed：官方已不存在的旧托管模型保留，未探通模型不入列，绝不删。
func TestVolcanoSyncPlanPartialKeepsOldManaged(t *testing.T) {
	t.Parallel()

	// doubao-seed-2.0-mini 只在 Agent 套餐，Coding 官方文档已无它——但若旧托管里有，
	// 部分确认时必须保留（add-only，即便官方下线也不删）。
	oldManaged := []string{"doubao-seed-2.0-mini"}
	repo := &volcanoPlanSyncRepoStub{}
	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, model string) (int, string, error) {
			if model == "glm-5.3-flash" {
				return 429, `{"error":"rate limited"}`, nil // 未探通 → unverified
			}
			return 200, `{"choices":[{"message":{"content":"hi"}}]}`, nil
		},
	}
	svc := newVolcanoSyncSvc(t, stub, repo)
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions",
		map[string]any{"doubao-seed-2.0-mini": "doubao-seed-2.0-mini"})
	account.Extra = map[string]any{
		VolcanoPlanManagedModelExtraKey: &VolcanoPlanManagedModels{Models: oldManaged, SyncedAt: time.Now().UTC()},
	}

	result, err := svc.SyncVolcanoPlanModels(context.Background(), account, true, nil)
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.False(t, result.FullConfirm, "存在 unverified 阻断 → 部分确认")
	require.Equal(t, []string{"glm-5.3-flash"}, result.Unverified)
	require.Empty(t, result.WillRemove, "部分确认绝不收敛下架")

	// 部分确认绝不收敛下架（will_remove 为空，上面已断言）。旧托管已在 mapping 中
	// （curSet 命中）故不入 will_add，但必须保留；未探通模型不得新增。
	require.NotContains(t, result.WillAdd, "glm-5.3-flash")

	newMapping, ok := account.Credentials["model_mapping"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "doubao-seed-2.0-mini", newMapping["doubao-seed-2.0-mini"], "官方下线的旧托管 identity 键在部分确认下保留")
	_, hasUnverified := newMapping["glm-5.3-flash"]
	require.False(t, hasUnverified, "未探通模型不得并入 mapping")

	// 托管快照 = 旧托管 ∪ confirmed（共 9 个：8 探通 + 旧托管），不含未探通者。
	snap := account.GetVolcanoPlanManagedModels()
	require.NotNil(t, snap)
	require.NotContains(t, snap.Models, "glm-5.3-flash")
	var expected = append(append([]string(nil), oldManaged...), excludeStrings(codingCandidates, "glm-5.3-flash")...)
	require.Equal(t, dedupeAndSortModelIDs(expected), snap.Models)
}

func excludeStrings(in []string, drop string) []string {
	var out []string
	for _, s := range in {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// TestVolcanoSyncPlanFullReplaceRemovesGoneManaged 验证完全确认替换会把官方已下线的
// 旧托管 identity 键从 mapping 收敛删除（人工键不受影响）。
func TestVolcanoSyncPlanFullReplaceRemovesGoneManaged(t *testing.T) {
	t.Parallel()

	repo := &volcanoPlanSyncRepoStub{}
	stub := &volcanoSyncHTTPStub{respond: allOKRespond}
	svc := newVolcanoSyncSvc(t, stub, repo)
	// 旧托管含官方已下线的 doubao-seed-2.0-mini（不在 Coding 候选）+ 人工 alias。
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions",
		map[string]any{
			"doubao-seed-2.0-mini": "doubao-seed-2.0-mini",
			"manual-alias":         "glm-5.3",
		})
	// doubao-seed-2.0-mini 是此前同步器落地身份的键 → 存于 identity_keys，可被完全确认收敛删除。
	account.Extra = map[string]any{
		VolcanoPlanManagedModelExtraKey: &VolcanoPlanManagedModels{
			Models:       []string{"doubao-seed-2.0-mini"},
			IdentityKeys: []string{"doubao-seed-2.0-mini"},
			SyncedAt:     time.Now().UTC(),
		},
	}

	result, err := svc.SyncVolcanoPlanModels(context.Background(), account, true, []string{"doubao-seed-2.0-mini"})
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.True(t, result.FullConfirm)
	// 旧托管 doubao-seed-2.0-mini 完全确认下被收敛下架。
	require.Equal(t, []string{"doubao-seed-2.0-mini"}, result.WillRemove)

	newMapping, ok := account.Credentials["model_mapping"].(map[string]any)
	require.True(t, ok)
	_, hasGone := newMapping["doubao-seed-2.0-mini"]
	require.False(t, hasGone, "完全确认：官方下线的旧托管 identity 键应被移除")
	require.Equal(t, "glm-5.3", newMapping["manual-alias"], "人工 alias 键永远保留")
}

// TestVolcanoSyncPlanAgentDiscoversKimiK3 验证 Agent 套餐经真实探活发现 kimi-k3
// （agent 专属，Coding 独立互不串）。
func TestVolcanoSyncPlanAgentDiscoversKimiK3(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	probed := map[string]bool{}
	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, model string) (int, string, error) {
			mu.Lock()
			probed[model] = true
			mu.Unlock()
			return 200, `{"content":[{"type":"text","text":"hi"}]}`, nil
		},
	}
	svc := newVolcanoSyncSvc(t, stub, nil)
	// Agent 账号走 anthropic 协议 → /v1/messages。
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/plan", "anthropic", nil)

	result, err := svc.SyncVolcanoPlanModels(context.Background(), account, false, nil)
	require.NoError(t, err)
	require.Empty(t, result.Unverified)
	require.Empty(t, result.Unavailable)
	mu.Lock()
	require.True(t, probed["kimi-k3"], "agent 官方候选应包含并探活 kimi-k3")
	mu.Unlock()
	require.Contains(t, result.Confirmed, "kimi-k3")
}

// TestVolcanoSyncPlanAllUnavailableFailsClosed 验证全部候选明确不可用 → 失败关闭，
// 不当作“无模型”，绝不落库。
func TestVolcanoSyncPlanAllUnavailableFailsClosed(t *testing.T) {
	t.Parallel()

	repo := &volcanoPlanSyncRepoStub{}
	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, _ string) (int, string, error) {
			return 404, `{"error":"model not found"}`, nil
		},
	}
	svc := newVolcanoSyncSvc(t, stub, repo)
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions", nil)

	_, err := svc.SyncVolcanoPlanModels(context.Background(), account, true, nil)
	require.Error(t, err)
	var syncErr *UpstreamModelSyncError
	require.ErrorAs(t, err, &syncErr)
	extraCalls, credCalls := repo.counts()
	require.Zero(t, extraCalls, "失败关闭：不得写快照")
	require.Zero(t, credCalls, "失败关闭：不得写 mapping")
}

// TestVolcanoSyncPlanCredentialErrorTerminates 验证 401/403 凭证级终止（不回落不删除）。
func TestVolcanoSyncPlanCredentialErrorTerminates(t *testing.T) {
	t.Parallel()

	repo := &volcanoPlanSyncRepoStub{}
	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, _ string) (int, string, error) {
			return 401, `{"error":"invalid api key"}`, nil
		},
	}
	svc := newVolcanoSyncSvc(t, stub, repo)
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions", nil)

	_, err := svc.SyncVolcanoPlanModels(context.Background(), account, true, nil)
	require.Error(t, err)
	var syncErr *UpstreamModelSyncError
	require.ErrorAs(t, err, &syncErr)
	require.Equal(t, UpstreamModelSyncErrorCredential, syncErr.Kind)
	extraCalls, credCalls := repo.counts()
	require.Zero(t, extraCalls)
	require.Zero(t, credCalls)
}

// TestVolcanoSyncPlanFullyPopulatedReprobesAllCandidates 验证全套官方候选已收录于
// mapping 时仍全量重新探活（不按已收录跳过）：探活次数等于官方候选数量，Confirmed
// 包含全部成功候选，WillAdd 仍为空；托管快照与 mapping 写入语义保持不变。
func TestVolcanoSyncPlanFullyPopulatedReprobesAllCandidates(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	probed := map[string]int{}
	repo := &volcanoPlanSyncRepoStub{}
	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, model string) (int, string, error) {
			mu.Lock()
			probed[model]++
			mu.Unlock()
			return 200, `{"choices":[{"message":{"content":"hi"}}]}`, nil
		},
	}
	svc := newVolcanoSyncSvc(t, stub, repo)
	m := make(map[string]any, len(codingCandidates))
	for _, c := range codingCandidates {
		m[c] = c
	}
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions", m)

	result, err := svc.SyncVolcanoPlanModels(context.Background(), account, true, nil)
	require.NoError(t, err)
	require.True(t, result.FullConfirm, "全部探通且无阻断 → 完全确认")
	require.Equal(t, codingCandidates, result.Confirmed, "全已收录仍重新探活，全部候选进入 Confirmed")
	require.Empty(t, result.WillAdd, "全已收录：无新增")
	require.Empty(t, result.WillRemove, "无官方下线：无下架")
	mu.Lock()
	require.Len(t, probed, len(codingCandidates), "每个官方候选都必须被重新探活")
	for _, c := range codingCandidates {
		require.Equal(t, 1, probed[c], "候选 %s 应恰好探活一次", c)
	}
	mu.Unlock()

	snap := account.GetVolcanoPlanManagedModels()
	require.NotNil(t, snap, "完全确认仍初始化/更新托管快照")
	require.Equal(t, codingCandidates, snap.Models)
	extraCalls, credCalls := repo.counts()
	require.Equal(t, 1, extraCalls, "仍落一次托管快照")
	require.Equal(t, 1, credCalls, "仍落一次 mapping（幂等覆盖）")
}

// TestVolcanoSyncPlanResultJSONEmptyArraysNotNull 验证响应契约：完整成功（无
// unavailable/unverified/will_remove）与全已收录再同步（will_add 为空）时，所有分类
// 数组必须序列化为 [] 而非 null——前端直接读 .length，null 会触发 TypeError。
func TestVolcanoSyncPlanResultJSONEmptyArraysNotNull(t *testing.T) {
	t.Parallel()

	stub := &volcanoSyncHTTPStub{respond: allOKRespond}

	t.Run("full success with empty categories", func(t *testing.T) {
		t.Parallel()
		svc := newVolcanoSyncSvc(t, stub, nil)
		account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions", nil)

		result, err := svc.SyncVolcanoPlanModels(context.Background(), account, false, nil)
		require.NoError(t, err)
		require.True(t, result.FullConfirm)

		body, err := json.Marshal(result)
		require.NoError(t, err)
		require.Contains(t, string(body), `"unavailable":[]`)
		require.Contains(t, string(body), `"unverified":[]`)
		require.Contains(t, string(body), `"will_remove":[]`)
		require.NotContains(t, string(body), `"unavailable":null`)
		require.NotContains(t, string(body), `"unverified":null`)
		require.NotContains(t, string(body), `"will_add":null`)
		require.NotContains(t, string(body), `"will_remove":null`)
	})

	t.Run("re-sync of fully populated account keeps empty will_add as array", func(t *testing.T) {
		t.Parallel()
		svc := newVolcanoSyncSvc(t, stub, nil)
		m := make(map[string]any, len(codingCandidates))
		for _, c := range codingCandidates {
			m[c] = c
		}
		account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions", m)

		result, err := svc.SyncVolcanoPlanModels(context.Background(), account, false, nil)
		require.NoError(t, err)
		require.Empty(t, result.WillAdd)

		body, err := json.Marshal(result)
		require.NoError(t, err)
		require.Contains(t, string(body), `"will_add":[]`)
		require.Contains(t, string(body), `"will_remove":[]`)
	})
}

// TestVolcanoSyncPlanMappingPersistFailureLeavesSnapshotUntouched 验证 mapping 持久化失败
// 时失败上抛且托管快照绝不先落：快照一致性可恢复，杜绝"新映射既未提交、旧托管却被覆盖"。
func TestVolcanoSyncPlanMappingPersistFailureLeavesSnapshotUntouched(t *testing.T) {
	t.Parallel()

	repo := &volcanoPlanSyncRepoStub{credErr: errors.New("db unavailable")}
	stub := &volcanoSyncHTTPStub{respond: allOKRespond}
	svc := newVolcanoSyncSvc(t, stub, repo)
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions", nil)

	_, err := svc.SyncVolcanoPlanModels(context.Background(), account, true, nil)
	require.Error(t, err, "mapping 持久化失败必须上抛")
	require.Nil(t, account.GetVolcanoPlanManagedModels(), "mapping 未提交前不得写托管快照")
	extraCalls, credCalls := repo.counts()
	require.Equal(t, 1, credCalls, "mapping 写调用已发生但失败")
	require.Zero(t, extraCalls, "快照不得先于 mapping 落盘")
}

// TestVolcanoSyncPlanSnapshotPersistFailureStillApplies 验证托管快照（派生缓存）写失败
// 不推翻已提交的 model_mapping（事实源）：仍 Applied=true，前端据此一致更新，消除
// “后端已生效、前端报错不更新”的不一致窗口（P2-4）；快照留待重试幂等补写。
func TestVolcanoSyncPlanSnapshotPersistFailureStillApplies(t *testing.T) {
	t.Parallel()

	repo := &volcanoPlanSyncRepoStub{extraErr: errors.New("snapshot db down")}
	stub := &volcanoSyncHTTPStub{respond: allOKRespond}
	svc := newVolcanoSyncSvc(t, stub, repo)
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions", nil)

	result, err := svc.SyncVolcanoPlanModels(context.Background(), account, true, nil)
	require.NoError(t, err, "快照失败不得让本次已提交的同步报错")
	require.True(t, result.Applied, "mapping 已提交 → 必须报告 Applied 供前端一致更新")
	require.Equal(t, codingCandidates, result.Confirmed)

	extraCalls, credCalls := repo.counts()
	require.Equal(t, 1, extraCalls, "快照写调用已发生（但失败）")
	require.Equal(t, 1, credCalls, "mapping 已提交")
}

// TestVolcanoSyncPlanApplyDriftRejected 验证 apply 重扫出现 preview 未确认的下架时拒绝落库
// （R3-1）：preview 是部分确认（无下架），apply 时临时探活恢复升级为完全确认、产生
// preview 未提示的下架 → apply 必须 fail（config 错误），绝不静默删除未经确认的模型。
func TestVolcanoSyncPlanApplyDriftRejected(t *testing.T) {
	t.Parallel()

	repo := &volcanoPlanSyncRepoStub{}
	stub := &volcanoSyncHTTPStub{respond: allOKRespond}
	svc := newVolcanoSyncSvc(t, stub, repo)
	// doubao-seed-2.0-mini 是上轮同步器落地的 identity 键（官方 Coding 已无此模型）。
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions",
		map[string]any{"doubao-seed-2.0-mini": "doubao-seed-2.0-mini"})
	account.Extra = map[string]any{
		VolcanoPlanManagedModelExtraKey: &VolcanoPlanManagedModels{
			Models:       []string{"doubao-seed-2.0-mini"},
			IdentityKeys: []string{"doubao-seed-2.0-mini"},
			SyncedAt:     time.Now().UTC(),
		},
	}

	// 用户 preview 时未看到任何下架（allowed_removals 为空）。
	_, err := svc.SyncVolcanoPlanModels(context.Background(), account, true, nil)
	require.Error(t, err, "apply 下架未经 preview 确认必须拒绝")
	var syncErr *UpstreamModelSyncError
	require.ErrorAs(t, err, &syncErr)
	require.Equal(t, UpstreamModelSyncErrorConfiguration, syncErr.Kind)
	extraCalls, credCalls := repo.counts()
	require.Zero(t, extraCalls, "拒绝的下架不得写快照")
	require.Zero(t, credCalls, "拒绝的下架不得写 mapping")
	// 既有 identity 键未被删除。
	_, hasGone := account.Credentials["model_mapping"].(map[string]any)["doubao-seed-2.0-mini"]
	require.True(t, hasGone)
}

// TestVolcanoSyncPlanApplyDriftCoveredAllowed 验证 apply 下架集合被 preview 的
// allowed_removals 完整覆盖（preview 已展示并确认该下架）→ 允许落库。
func TestVolcanoSyncPlanApplyDriftCoveredAllowed(t *testing.T) {
	t.Parallel()

	repo := &volcanoPlanSyncRepoStub{}
	stub := &volcanoSyncHTTPStub{respond: allOKRespond}
	svc := newVolcanoSyncSvc(t, stub, repo)
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions",
		map[string]any{"doubao-seed-2.0-mini": "doubao-seed-2.0-mini"})
	account.Extra = map[string]any{
		VolcanoPlanManagedModelExtraKey: &VolcanoPlanManagedModels{
			Models:       []string{"doubao-seed-2.0-mini"},
			IdentityKeys: []string{"doubao-seed-2.0-mini"},
			SyncedAt:     time.Now().UTC(),
		},
	}

	result, err := svc.SyncVolcanoPlanModels(context.Background(), account, true, []string{"doubao-seed-2.0-mini"})
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.Equal(t, []string{"doubao-seed-2.0-mini"}, result.WillRemove)
	_, hasGone := account.Credentials["model_mapping"].(map[string]any)["doubao-seed-2.0-mini"]
	require.False(t, hasGone, "preview 确认的下架应被移除")
	// 快照 identity_keys 不再含被下架键。
	extraCalls, _ := repo.counts()
	require.Equal(t, 1, extraCalls)
}

// TestVolcanoSyncPlanManualIdentityNotDeleted 验证 R3-2：用户手动 identity 键
// （key==value==候选模型，但不在同步器落地的 IdentityKeys）在完全确认时不被下架——执行器
// 遇到官方下架的手动 identity 是非托管键，必须保留，绝不因"当前==候选"被吸收进托管可删集。
func TestVolcanoSyncPlanManualIdentityNotDeleted(t *testing.T) {
	t.Parallel()

	repo := &volcanoPlanSyncRepoStub{}
	stub := &volcanoSyncHTTPStub{respond: allOKRespond}
	svc := newVolcanoSyncSvc(t, stub, repo)
	// manual-key: 用户手工 identity，已在 coding 官方下架（不在候选），但从未被同步器
	// 写入（不在快照 IdentityKeys）→ 完全确认也不得删。
	account := volcanoSyncAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions",
		map[string]any{"manual-gone": "manual-gone"})
	account.Extra = map[string]any{
		VolcanoPlanManagedModelExtraKey: &VolcanoPlanManagedModels{
			Models:       []string{"manual-gone"},
			IdentityKeys: nil, // 同步器从未写入 → 空
			SyncedAt:     time.Now().UTC(),
		},
	}

	result, err := svc.SyncVolcanoPlanModels(context.Background(), account, true, []string{"manual-gone"})
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.Empty(t, result.WillRemove, "手动 identity 键绝不下架")
	_, hasManual := account.Credentials["model_mapping"].(map[string]any)["manual-gone"]
	require.True(t, hasManual, "手动 identity 键必须保留")
	// 快照 Managed 集仍被候选替换，但 Manual 键保留。
	snap := account.GetVolcanoPlanManagedModels()
	require.NotNil(t, snap)
	require.Contains(t, snap.Models, "deepseek-v4-flash")
}
