//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	accountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	entgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// reconOpsRepo 在 opsRepoMock 之上捕获心跳写入次数、job 名与 LastResult，
// 用于断言 reconciler 每轮写且仅写一次心跳、job 名正确、以及"无候选 key 停拉"原因被记录。
type reconOpsRepo struct {
	*opsRepoMock
	mu         sync.Mutex
	calls      int
	jobName    string
	lastResult string
	lastError  string
	lastSucc   bool
}

// captureLog 把标准日志输出重定向到缓冲区并返回读取函数（F4：断言告警级日志内容）。
// 返回的 restore 必须在用例结束前 defer 调用，避免污染其他用例的日志输出。
func captureLog(t *testing.T) (read func() string, restore func()) {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	return func() string { return buf.String() }, func() { log.SetOutput(os.Stderr) }
}

func newReconOpsRepo() *reconOpsRepo {
	return &reconOpsRepo{opsRepoMock: &opsRepoMock{}}
}

func (o *reconOpsRepo) UpsertJobHeartbeat(_ context.Context, input *OpsUpsertJobHeartbeatInput) error {
	o.mu.Lock()
	o.calls++
	o.jobName = input.JobName
	if input.LastSuccessAt != nil {
		o.lastSucc = true
	} else {
		o.lastSucc = false
	}
	if input.LastResult != nil {
		o.lastResult = *input.LastResult
	}
	if input.LastError != nil {
		o.lastError = *input.LastError
	}
	o.mu.Unlock()
	return nil
}

func (o *reconOpsRepo) hbCalls() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

func (o *reconOpsRepo) lastJobName() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.jobName
}

func (o *reconOpsRepo) lastResultVal() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastResult
}

func (o *reconOpsRepo) lastErrorVal() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastError
}

// lastSuccessCalled 报告最近一次心跳是否带 LastSuccessAt（R15-1 断言用）。
func (o *reconOpsRepo) lastSuccessCalled() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastSucc
}

// fakeModelWater 是 QuotaKeyModelWaterProvider 的测试替身（P1-D2 总量口径）：控制枚举的
// 模型、池级水位（cursor 可服务账号【去重总量】）与池级候选 key，并可注入错误
// （合同 §5：替身仅存于测试）。
//
// 总量口径下水位与候选都不再按模型键区分 —— 同一账号跨多个覆盖模型只计一次，故替身用单个
// level / 单个 candidates 列表表达池级状态；models 仍保留（决定新号挂组的模型上下文）。
type fakeModelWater struct {
	mu         sync.Mutex
	models     []string
	level      int
	candidates []int64

	// cooldownKeys / fullKeys 模拟提供方侧【有效码预检】（P1-D2 §6.3）：冷却窗口内的码与
	// 无剩余容量（working>=15）的码不得进入候选集。默认空 → 不过滤，保持其它用例原语义。
	cooldownKeys map[int64]bool
	fullKeys     map[int64]bool

	listErr  error
	waterErr error
	// waterErrAfter>0 让 WaterLevel 在累计调用次数超过该值时返回 waterErrAfterMsg，
	// 用于"拉后重算失败"（初始调用成功、后续重算失败）这类交错的断言，零值无影响。
	waterErrAfter    int
	waterErrAfterMsg string

	candErr error

	// 调用计数（用于断言扫描行为与"缺口驱动的重算次数"）。
	listCalls  int
	waterCalls int
	candCalls  int
	// limits 记录每次 ListCandidateKeys 收到的 limit（reconciler 应传【总量缺口】）。
	limits []int
}

func newFakeModelWater() *fakeModelWater {
	return &fakeModelWater{}
}

// markCooldown / markFull 把指定 key 标为"冷却中" / "容量已满（working>=15）"，
// 使其在 ListCandidateKeys 预检阶段即被排除（不进入候选集、本轮不得被拉）。
func (f *fakeModelWater) markCooldown(ids ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		if f.cooldownKeys == nil {
			f.cooldownKeys = make(map[int64]bool)
		}
		f.cooldownKeys[id] = true
	}
}

func (f *fakeModelWater) markFull(ids ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		if f.fullKeys == nil {
			f.fullKeys = make(map[int64]bool)
		}
		f.fullKeys[id] = true
	}
}

func (f *fakeModelWater) ListAllowedModels(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return f.models, f.listErr
}

func (f *fakeModelWater) WaterLevel(_ context.Context, _ string) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waterCalls++
	if f.waterErr != nil {
		return 0, 0, f.waterErr
	}
	if f.waterErrAfter > 0 && f.waterCalls > f.waterErrAfter {
		msg := f.waterErrAfterMsg
		if msg == "" {
			msg = "water level recompute boom"
		}
		return 0, 0, errors.New(msg)
	}
	return f.level, quotaKeyWaterLevelThreshold, nil
}

func (f *fakeModelWater) ListCandidateKeys(_ context.Context, _ string, limit int) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candCalls++
	f.limits = append(f.limits, limit)
	if f.candErr != nil {
		return nil, f.candErr
	}
	// 有效码预检（提供方职责）：冷却中 / 无剩余容量的码不进入候选集。
	cands := make([]int64, 0, len(f.candidates))
	for _, id := range f.candidates {
		if f.cooldownKeys[id] || f.fullKeys[id] {
			continue
		}
		cands = append(cands, id)
	}
	if limit > 0 && len(cands) > limit {
		cands = cands[:limit]
	}
	out := make([]int64, len(cands))
	copy(out, cands)
	return out, nil
}

// addAccounts 模拟"拉号成功后实际入库并可服务"的账号增量：只有真实入库才会抬高池级水位，
// 用于断言 reconciler 是按实际入库账号重算缺口，而不是按"选中/拉取过的 key 数"计数。
func (f *fakeModelWater) addAccounts(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.level += n
}

// waterCallCount 返回 WaterLevel 被调用的次数（断言"每次拉号后按实际入库账号重算"）。
func (f *fakeModelWater) waterCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waterCalls
}

// candCallCount 返回 ListCandidateKeys 被调用的次数。
func (f *fakeModelWater) candCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.candCalls
}

// currentLevel 返回当前池级总量（断言收尾状态）。
func (f *fakeModelWater) currentLevel() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.level
}

// requestedLimits 返回每次 ListCandidateKeys 收到的 limit 快照（断言传入的是总量缺口）。
func (f *fakeModelWater) requestedLimits() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, len(f.limits))
	copy(out, f.limits)
	return out
}

func (f *fakeModelWater) listCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

// fakePuller 是 quotaKeyPuller 的测试替身，记录每次调用的 quotaKeyID 与携带的模型 M
// （2 参入口记 ""，3 参 PullAccountsForModel 记实际 M，用于 F2 断言走的是带模型上下文的入口），
// 可按 keyID 注入错误；blockOn 让指定 key 的拉号阻塞直到通道关闭（用于并发单 flight 测试）。
type fakePuller struct {
	mu         sync.Mutex
	calls      []int64
	models     []string
	errOnIDs   map[int64]error
	defaultErr error

	blockOn map[int64]chan struct{}
	onStart func(int64)

	// afterPull 在每次拉号动作完成后回调（携带拉号结果），供用例模拟"成功入库后水位上涨"。
	// 总量制断言依赖它区分【拉过了】与【真的补到了号】。
	afterPull func(keyID int64, err error)
}

func newFakePuller() *fakePuller {
	return &fakePuller{errOnIDs: make(map[int64]error)}
}

func (p *fakePuller) PullAccounts(_ context.Context, quotaKeyID int64) error {
	if ch, ok := p.blockOn[quotaKeyID]; ok {
		if p.onStart != nil {
			p.onStart(quotaKeyID)
		}
		<-ch
	}
	p.mu.Lock()
	p.calls = append(p.calls, quotaKeyID)
	p.models = append(p.models, "") // 2 参入口：不携带模型上下文
	p.mu.Unlock()
	if err, ok := p.errOnIDs[quotaKeyID]; ok {
		p.invokeAfterPull(quotaKeyID, err)
		return err
	}
	p.invokeAfterPull(quotaKeyID, p.defaultErr)
	return p.defaultErr
}

// invokeAfterPull 触发 afterPull 钩子（在返回错误之前），用例可据此抬高池级水位以模拟真实入库。
func (p *fakePuller) invokeAfterPull(quotaKeyID int64, err error) {
	if p.afterPull != nil {
		p.afterPull(quotaKeyID, err)
	}
}

// autoRegisterOnSuccess 让替身拉号在【成功】时把 n 个账号计入池级水位，模拟"拉号成功且新号
// 确实入库、可服务"。总量制编排依赖这个反馈闭环：只有真实入库才会抬高 cursor 可服务账号总量，
// 因此用例可以用它区分【拉过了】与【真的补到了号】（禁止场景：一次都没入库却认为补满）。
func (p *fakePuller) autoRegisterOnSuccess(mw *fakeModelWater, accountsPerPull int) {
	p.afterPull = func(_ int64, err error) {
		if err == nil {
			mw.addAccounts(accountsPerPull)
		}
	}
}

// PullAccountsForModel 是 quotaKeyPuller 接口扩展的测试替身实现（F2：水位触发补池携带模型
// M 上下文）。与 PullAccounts 同记录调用（calls 含 keyID），并同享 errOnIDs/defaultErr 注入；
// 额外把模型 M 记入 models，供断言"补池确实走带 M 的 3 参入口"。
func (p *fakePuller) PullAccountsForModel(_ context.Context, quotaKeyID int64, modelM string) error {
	if ch, ok := p.blockOn[quotaKeyID]; ok {
		if p.onStart != nil {
			p.onStart(quotaKeyID)
		}
		<-ch
	}
	p.mu.Lock()
	p.calls = append(p.calls, quotaKeyID)
	p.models = append(p.models, modelM)
	p.mu.Unlock()
	if err, ok := p.errOnIDs[quotaKeyID]; ok {
		p.invokeAfterPull(quotaKeyID, err)
		return err
	}
	p.invokeAfterPull(quotaKeyID, p.defaultErr)
	return p.defaultErr
}

func (p *fakePuller) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *fakePuller) calledIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int64, len(p.calls))
	copy(out, p.calls)
	return out
}

// calledModels 返回每次拉号携带的模型 M（2 参入口为 ""，3 参入口为实际 M）。
// 用于 F2 断言：水位触发的补池走的是 PullAccountsForModel（带 M），而非裸 2 参入口。
func (p *fakePuller) calledModels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.models))
	copy(out, p.models)
	return out
}

// newTestReconciler 构造一个带 fake puller / ops 的 reconciler（未启动后台循环）。
// provider 为 nil 时表示"未接入模型水位提供方"。
func newTestReconciler(puller *fakePuller, ops *reconOpsRepo, provider QuotaKeyModelWaterProvider) *QuotaKeyReconciler {
	r := NewQuotaKeyReconciler(newFakeQuotaKeyRepo(), puller, ops)
	if provider != nil {
		r.SetModelWaterProvider(provider)
	}
	return r
}

// TestReconciler_WaterLevelBelow_TriggersPullByDeficit 验证模型水位低于阈值时，
// 按差额（threshold - current）触发拉号，且仅取前 deficit 个候选 key。
func TestReconciler_WaterLevelBelow_TriggersPullByDeficit(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 7 // 低于 10 → 差额 3
	mw.candidates = []int64{100, 101, 102, 103, 104}

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	ids := puller.calledIDs()
	if len(ids) != 3 {
		t.Fatalf("expected 3 pulls (deficit=10-7), got %d (%v)", len(ids), ids)
	}
	want := []int64{100, 101, 102}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("expected pulls for %v, got %v", want, ids)
		}
	}
	if c := mw.candCallCount(); c != 1 {
		t.Fatalf("expected ListCandidateKeys called once, got %d", c)
	}
	// F2：补池必须携带模型 M 走 PullAccountsForModel（3 参入口），而非无模型的 2 参入口
	//（2 参入口不执行 §13.3 挂载，落成裸号）。
	models := puller.calledModels()
	if len(models) != 3 {
		t.Fatalf("expected 3 pulls with model context, got %d (%v)", len(models), models)
	}
	for i, m := range models {
		if m != "m1" {
			t.Fatalf("pull %d must carry model %q via PullAccountsForModel, got %q", i, "m1", m)
		}
	}
}

// TestReconciler_WaterLevelTrigger_MountsPulledAccountToCoveringGroup 是 F2 的贯通用例：
// 拉号侧不再用替身，而是真实 *QuotaKeyService（命中 quota_test 库），只有水位提供方是替身
// （仅控制编排输入：模型枚举 / 水位 / 候选 key）。断言水位触发的补池最终把新号挂入
// "白名单含 M"的分组——即新号确实挂组、不再是裸号（F2 Done when）。
//
// 覆盖的生产链路：wire 装配 SetModelWaterProvider → runOnce 枚举模型 → WaterLevel 判定
// 低于阈值 → 按差额 ListCandidateKeys → pullKey → PullAccountsForModel → pull 状态机 →
// cursorGroupResolver(findCursorPlatformGroups，U7) → mountAccountToGroups（§20.2-A）。
func TestReconciler_WaterLevelTrigger_MountsPulledAccountToCoveringGroup(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// U7（§20.2-A）：挂载按账号模型能力（静态实供清单）判定。grp = composite 聚合组、
	// 白名单覆盖实供模型 gpt-6-sol → 新号挂入；modelM 仅作触发上下文（不收窄挂载）。
	modelM := "gpt-6-sol"
	grp := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{modelM}})
	// 不覆盖任何实供模型的分组：不得被挂入。
	other := mustCreateGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{fix1UniqueModel("cursor-fix1-f2-other")},
	})
	// R10-1（CUR-F13）：本用例自建分组按 ID 清理，避免覆盖实供模型的启用组污染他用例。
	t.Cleanup(func() {
		cctx := context.Background()
		for _, gid := range []int64{grp.ID, other.ID} {
			_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(gid)).Exec(cctx)
			_, _ = client.Group.Delete().Where(entgroup.IDEQ(gid)).Exec(cctx)
		}
	})

	// 拉号走真实服务（上游替身发 1 个号），等价于生产装配下的 *QuotaKeyService。
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{
				Name:        fix1UniqueModel("cursor-fix1-f2-acc"),
				Platform:    domain.PlatformCursor,
				Type:        "oauth",
				Credentials: map[string]any{"k": "v"},
			},
		}},
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	mw := newFakeModelWater()
	mw.models = []string{modelM}
	mw.level = 0 // 低于阈值 10 → 按差额补池
	mw.candidates = []int64{qk.ID}

	r := NewQuotaKeyReconciler(newFakeQuotaKeyRepo(), svc, newReconOpsRepo())
	r.SetModelWaterProvider(mw)

	r.runOnce()

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1, "reconciler must pull and persist the account")
	// 挂组断言：新号挂入覆盖 M 的分组（不再是裸号）。
	require.True(t, accountGroupLinked(t, ctx, client, accs[0].ID, grp.ID),
		"pulled account must be mounted to the group covering M (not a bare account)")
	require.False(t, accountGroupLinked(t, ctx, client, accs[0].ID, other.ID),
		"pulled account must NOT be mounted to a non-covering group")
}

// TestReconciler_WaterLevelAbove_NoPull 验证模型水位达到阈值时，不触发任何拉号。
func TestReconciler_WaterLevelAbove_NoPull(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 10 // 等于阈值 → 已达水位
	mw.candidates = []int64{100, 101}

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	if got := puller.callCount(); got != 0 {
		t.Fatalf("expected 0 pulls when at water level, got %d", got)
	}
}

// TestReconciler_MultipleModels_PullsByPoolDeficitOnly 验证 P1-D2 总量口径：多模型不再各自
// 派生缺口——补池只看"cursor 可服务账号去重总量"。两个允许模型 + 池级缺口 2 → 只补 2 次，
// 不因"有 2 个模型"而翻倍（旧每模型 10 口径会拉 2+5=7 次）。
func TestReconciler_MultipleModels_PullsByPoolDeficitOnly(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1", "m2"}
	mw.level = 8 // 池级总量 8 → 缺口 2（与模型个数无关）
	mw.candidates = []int64{10, 11, 12, 20, 21, 22, 23, 24, 25}

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	if got := puller.callCount(); got != 2 {
		t.Fatalf("expected 2 pulls (pool deficit=10-8), not per-model sum, got %d (%v)", got, puller.calledIDs())
	}
	// 挂载上下文仍须携带具体模型 M（§13.3 挂组），不得退化成无模型的裸拉号。
	for i, m := range puller.calledModels() {
		if m == "" {
			t.Fatalf("pull %d must carry a model context for group mounting, got empty", i)
		}
	}
}

// TestReconciler_PullErrorDoesNotAbort 验证单 key 拉号失败时，其余 key 仍被拉取。
func TestReconciler_PullErrorDoesNotAbort(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0 // 差额 10，但候选仅 2 个
	mw.candidates = []int64{30, 40}

	puller := newFakePuller()
	puller.errOnIDs[30] = context.DeadlineExceeded // 任意错误，模拟拉号失败
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	ids := puller.calledIDs()
	if len(ids) != 2 {
		t.Fatalf("expected both keys pulled despite first error, got %v", ids)
	}
	if !(ids[0] == 30 && ids[1] == 40) {
		t.Fatalf("expected PullAccounts called for IDs [30 40], got %v", ids)
	}
}

// TestReconciler_NoCandidateKeys_StopsAndRecordsReason 验证所有模型均达水位（无候选 key）
// 时，reconciler 停拉并记录原因到心跳 LastResult。
func TestReconciler_NoCandidateKeys_StopsAndRecordsReason(t *testing.T) {
	mw := newFakeModelWater()
	// 总量口径：models 仅作挂载上下文，水位是池级单一总量（不再按模型取 levels）。
	mw.models = []string{"m1", "m2"}
	mw.level = 12 // 池级总量已达水位 → 不补池

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	if got := puller.callCount(); got != 0 {
		t.Fatalf("expected 0 pulls when no candidate keys, got %d", got)
	}
	if got := ops.hbCalls(); got != 1 {
		t.Fatalf("expected exactly 1 heartbeat written, got %d", got)
	}
	if res := ops.lastResultVal(); res == "" {
		t.Fatalf("expected no-candidate reason recorded in heartbeat LastResult, got empty")
	}
}

// TestReconciler_NoAllowedModels_StopsAndRecordsReason 验证无允许模型时停拉并记录原因。
func TestReconciler_NoAllowedModels_StopsAndRecordsReason(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = nil // 无允许模型

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	if got := puller.callCount(); got != 0 {
		t.Fatalf("expected 0 pulls, got %d", got)
	}
	if got := ops.hbCalls(); got != 1 {
		t.Fatalf("expected 1 heartbeat, got %d", got)
	}
	if res := ops.lastResultVal(); res == "" {
		t.Fatalf("expected no-allowed-models reason recorded, got empty")
	}
}

// TestReconciler_NoProvider_StopsAndRecordsReason 验证模型水位提供方未接入（nil）时，
// reconciler 安全停拉并记录原因（C11 接线前的默认状态）。
func TestReconciler_NoProvider_StopsAndRecordsReason(t *testing.T) {
	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, nil) // 未 SetModelWaterProvider

	r.runOnce()

	if got := puller.callCount(); got != 0 {
		t.Fatalf("expected 0 pulls without provider, got %d", got)
	}
	if got := ops.hbCalls(); got != 1 {
		t.Fatalf("expected 1 heartbeat, got %d", got)
	}
	if res := ops.lastResultVal(); res == "" {
		t.Fatalf("expected not-configured reason recorded, got empty")
	}
}

// TestReconciler_ListModelsError_RecordsError 验证枚举模型失败时记录错误心跳、不 panic。
func TestReconciler_ListModelsError_RecordsError(t *testing.T) {
	mw := newFakeModelWater()
	mw.listErr = errors.New("db boom")

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	if got := puller.callCount(); got != 0 {
		t.Fatalf("expected 0 pulls on list error, got %d", got)
	}
	if got := ops.hbCalls(); got != 1 {
		t.Fatalf("expected 1 heartbeat, got %d", got)
	}
}

// TestReconciler_HeartbeatWritten 验证 runOnce 每轮写且仅写一次心跳，且 job 名正确。
func TestReconciler_HeartbeatWritten(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 10 // 达水位 → 无候选，仍写一次心跳

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	if got := ops.hbCalls(); got != 1 {
		t.Fatalf("expected exactly 1 heartbeat written, got %d", got)
	}
	if name := ops.lastJobName(); name != quotaKeyReconcileJobName {
		t.Fatalf("expected heartbeat job name %q, got %q", quotaKeyReconcileJobName, name)
	}
}

// TestPullCoalescer_SingleFlight 直接验证单 flight：同 key 在拉号中再次请求被合并，
// 且无 pending 时 end 不触发 wakeup；有 pending 时 end 触发一次 wakeup。
func TestPullCoalescer_SingleFlight(t *testing.T) {
	c := newPullCoalescer(nil)

	// 单 flight：首个 begin 成功，后续同 key 在拉号中合并为 pending（不重复发起）。
	if !c.tryBegin(7) {
		t.Fatal("first begin should succeed")
	}
	if c.tryBegin(7) {
		t.Fatal("second begin for in-flight key must coalesce (return false)")
	}
	if c.tryBegin(7) {
		t.Fatal("third begin for in-flight key must coalesce (return false)")
	}
	// 期间有 2 次合并请求 → pending=2 → end 触发一次 wakeup（合并为单次补拉）。
	if !c.end(7) {
		t.Fatal("end with pending must wakeup")
	}

	// 无 pending 的场景：begin 成功且期间无其他请求 → end 不触发 wakeup。
	if !c.tryBegin(5) {
		t.Fatal("begin for key 5 should succeed")
	}
	if c.end(5) {
		t.Fatal("end with no pending must not wakeup")
	}

	// 带恰好一次 pending 的场景：end 触发一次 wakeup。
	if !c.tryBegin(6) {
		t.Fatal("begin for key 6 should succeed")
	}
	if c.tryBegin(6) {
		t.Fatal("second begin for key 6 must coalesce")
	}
	if !c.end(6) {
		t.Fatal("end with pending must wakeup")
	}
}

// TestReconciler_SingleFlightConcurrentPull 验证并发对同一 key 的拉号请求被合并为单 flight：
// 仅一个真实拉号在途，另一个合并为 pending wakeup，结束后补拉一次（总调用数 = 2，而非 3+）。
func TestReconciler_SingleFlightConcurrentPull(t *testing.T) {
	puller := newFakePuller()
	release := make(chan struct{})
	puller.blockOn = map[int64]chan struct{}{1: release}
	started := make(chan int64, 8)
	puller.onStart = func(id int64) { started <- id }

	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, nil) // 不依赖 provider，直接调 pullKey

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.pullKey(context.Background(), 1, "") }()
	go func() { defer wg.Done(); r.pullKey(context.Background(), 1, "") }()

	// 等待首个拉号真正在途（k1 进入 inflight）。
	select {
	case id := <-started:
		if id != 1 {
			t.Fatalf("unexpected first pull key %d", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first pull did not start (single-flight broken)")
	}
	// 给第二个 goroutine 时间完成 tryBegin 并合并为 pending。
	time.Sleep(50 * time.Millisecond)
	// 释放首个拉号 → 结束后 pending wakeup 触发补拉一次。
	close(release)
	wg.Wait()

	// 单 flight：仅一个在途 + 一个 pending 补拉 = 2 次真实 PullAccounts。
	if got := puller.callCount(); got != 2 {
		t.Fatalf("expected exactly 2 PullAccounts (1 in-flight + 1 pending wakeup), got %d", got)
	}
}

// TestReconciler_PendingWakeupRepulls 验证拉号期间再有同 key 请求时，结束后补拉一次。
func TestReconciler_PendingWakeupRepulls(t *testing.T) {
	c := newPullCoalescer(nil)
	if !c.tryBegin(1) {
		t.Fatal("begin should succeed")
	}
	// 模拟拉号期间另一个请求到达 → 合并为 pending。
	if c.tryBegin(1) {
		t.Fatal("concurrent begin must coalesce")
	}
	// 第一个拉号结束 → 有 pending → 应触发一次 wakeup 补拉。
	if !c.end(1) {
		t.Fatal("end with pending must wakeup")
	}
	// 补拉期间无新请求 → 再次 end 不触发 wakeup（避免无限循环）。
	// 注意：end(1) 返回 true（有 pending）后，调用方会再次发起补拉 → tryBegin(1) 应成功
	//（开始补拉这一 flight），而非被拒绝。
	if !c.tryBegin(1) {
		t.Fatal("wakeup repull begin must succeed")
	}
	if c.end(1) {
		t.Fatal("end with no pending must not wakeup again")
	}
}

// TestReconciler_DoubleCheckedExit 验证停止信号置位后，pullKey 不再新发起任何真实拉号
// （含 pending wakeup）。
func TestReconciler_DoubleCheckedExit(t *testing.T) {
	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, nil)

	r.Stop() // 置位 stopping

	r.pullKey(context.Background(), 1, "")

	if got := puller.callCount(); got != 0 {
		t.Fatalf("expected 0 pulls after stop (double-checked exit), got %d", got)
	}
}

// TestReconciler_StopCancels 验证 Start 后 Stop 能无阻塞返回（goroutine 已退出）。
func TestReconciler_StopCancels(t *testing.T) {
	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, nil)

	r.Start()

	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Stop 返回即说明 goroutine 已退出（否则 wg.Wait 会一直阻塞）。
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return; goroutine likely blocked and did not exit")
	}
}

// TestReconciler_NotifyAccountRemoved_OnlySetsPending 验证 NotifyAccountRemoved 只置 pending
// 唤醒标志、绝不直接拉号（Done when #2：存在且只置 pending）。
func TestReconciler_NotifyAccountRemoved_OnlySetsPending(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0 // 严重缺水位
	mw.candidates = []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	// 未启动后台循环：NotifyAccountRemoved 只置 pending，绝不直接拉号。
	r.NotifyAccountRemoved()

	if got := puller.callCount(); got != 0 {
		t.Fatalf("NotifyAccountRemoved must NOT pull directly (only sets pending), got %d pulls", got)
	}
}

// TestReconciler_NotifyAccountRemoved_PullsOnDeficit 验证软删唤醒后，reconciler 轮出前追加的
// 一次水位判定「有缺口才补」：存在缺口时确实触发补池（Start 立即一轮 + 唤醒一轮，各补差额）。
func TestReconciler_NotifyAccountRemoved_PullsOnDeficit(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 4 // 差额 6
	mw.candidates = []int64{100, 101, 102, 103, 104, 105, 106}

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)
	r.Start()
	defer r.Stop()

	// Start 立即跑一轮（pull 6），随后 notify 唤醒再跑一轮（pull 6）→ 共 12。
	r.NotifyAccountRemoved()

	deadline := time.After(3 * time.Second)
	for puller.callCount() < 12 {
		select {
		case <-deadline:
			t.Fatalf("expected 12 pulls (6 initial + 6 after notify on deficit), got %d", puller.callCount())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestReconciler_NotifyAccountRemoved_NoDeficitNoPull 验证软删唤醒后，若已无缺口（已达水位），
// 追加的水位判定不触发任何拉号（不无条件拉号）。
func TestReconciler_NotifyAccountRemoved_NoDeficitNoPull(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 10 // 达水位，无缺口
	mw.candidates = []int64{100}

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)
	r.Start()
	defer r.Stop()

	// 即使软删唤醒，runOnce 判定无缺口 → 不拉号。
	r.NotifyAccountRemoved()
	time.Sleep(200 * time.Millisecond)

	if got := puller.callCount(); got != 0 {
		t.Fatalf("expected 0 pulls when no deficit even after notify, got %d", got)
	}
}

// TestReconciler_NotifyAccountRemoved_NoLossWhileBusy 验证 loop 正在 runOnce（忙碌）期间到达
// 的软删唤醒不丢事件：首个忙碌 runOnce 完成后，loop 轮出应再跑一轮补齐补池。
func TestReconciler_NotifyAccountRemoved_NoLossWhileBusy(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 5 // 差额 5
	mw.candidates = []int64{100}

	puller := newFakePuller()
	release := make(chan struct{})
	puller.blockOn = map[int64]chan struct{}{100: release}
	started := make(chan int64, 1)
	puller.onStart = func(id int64) { started <- id }

	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)
	r.Start()
	defer r.Stop()

	// 等待首个（busy）runOnce 真正进入拉号——此时 loop 正忙。
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("busy runOnce did not start")
	}

	// loop 忙碌期间到达唤醒：应被保留（落入 pending / wakeCh 缓冲），不得丢失。
	r.NotifyAccountRemoved()
	// 释放首个拉号 → 其完成后 loop 轮出，应再跑一轮（唤醒不丢）。
	close(release)

	deadline := time.After(3 * time.Second)
	for puller.callCount() < 2 {
		select {
		case <-deadline:
			t.Fatalf("expected 2 pulls (busy run + wake repull); event lost, got %d", puller.callCount())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestQuotaKeyReconciler_StopRejectsNewFlight 验证 F3 停机竞态收口：Stop() 置位 stopping 后，
// coalescer.tryBegin 必须拒绝任何新 flight（同一互斥域内判定），消除"检查后发起前"间隙；
// 且即便仅 stopping 置位、未显式 coalescer.stop()，tryBegin 仍因 stopping 标志被拒。
// （与 TestReconciler_DoubleCheckedExit 互补：后者验证 pullKey 零新发起，本例验证单 flight
// 入口 tryBegin 直接拒绝。）
func TestQuotaKeyReconciler_StopRejectsNewFlight(t *testing.T) {
	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, nil)

	r.Stop() // 置位 stopping + coalescer.stop()

	// Stop 后：coalescer 拒绝任何新 flight。
	if r.coalescer.tryBegin(42) {
		t.Fatal("tryBegin must reject new flight after Stop (F3)")
	}
	// 即便新建一个仅共享 stopping 标志的 coalescer（未显式 stop），仍因 stopping 被拒。
	c := newPullCoalescer(&r.stopping)
	if c.tryBegin(7) {
		t.Fatal("coalescer sharing stopped flag must reject new flight (F3)")
	}
}

// TestQuotaKeyReconciler_StopLinearization_ConfirmRejectsAfterTryBegin 是 F3 的受控交错用例：
// 手工构造"tryBegin 已通过、但尚未发起真实拉号"的中间态，随后 Stop() 置位，
// 断言"发起前的最终门禁"（与 Stop 同处 coalescer.mu）必须拒绝该次发起 —— 即消除
// "检查后发起前"间隙，使"发起拉号"与"停机置位"共享同一可线性化点。
func TestQuotaKeyReconciler_StopLinearization_ConfirmRejectsAfterTryBegin(t *testing.T) {
	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, nil)

	// 受控交错 step 1：tryBegin 通过（此时未停机）→ 发起门禁应放行。
	if !r.coalescer.tryBegin(9) {
		t.Fatal("tryBegin must succeed before stop")
	}
	if !r.coalescer.confirmBegin(9) {
		t.Fatal("confirmBegin must allow the pull before stop")
	}
	// 受控交错 step 2：Stop 置位（先关 coalescer 门禁、再置 stopping）。
	r.Stop()
	// 受控交错 step 3：发起前的最终门禁必须拒绝（同一 c.mu 内与停机置位互斥）。
	if r.coalescer.confirmBegin(9) {
		t.Fatal("F3: confirmBegin must reject the pull after Stop (in-lock final check)")
	}
	// 端到端：pullKey 也不得产生任何真实拉号。
	r.pullKey(context.Background(), 9, "")
	if got := puller.callCount(); got != 0 {
		t.Fatalf("expected 0 real pulls after stop, got %d", got)
	}
}

// TestQuotaKeyReconciler_StopDuringInflight_NoNewPull 是 F3 的端到端受控交错用例：
// 首个候选 key 的拉号阻塞在途（loop 正忙）时停机，随后①在途期间同 key 的新请求
// （应合并为 pending / 被门禁拒绝）、②在途拉号结束后的 pending wakeup 补拉、
// ③本轮后续候选拉号 —— 三者均不得转化为任何新真实拉号；Stop() 返回后计数恒定。
func TestQuotaKeyReconciler_StopDuringInflight_NoNewPull(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0 // 差额 10
	mw.candidates = []int64{100, 101, 102}

	puller := newFakePuller()
	release := make(chan struct{})
	puller.blockOn = map[int64]chan struct{}{100: release}
	started := make(chan int64, 1)
	puller.onStart = func(id int64) { started <- id }

	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)
	r.Start()

	// 交错点 A：首个候选拉号已在途（loop 正忙）。
	select {
	case id := <-started:
		if id != 100 {
			t.Fatalf("unexpected in-flight key %d", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first pull did not start")
	}

	// 交错点 B：在途期间停机（Stop 阻塞在 wg.Wait，等待本轮 runOnce 自然结束）。
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		r.Stop()
	}()
	time.Sleep(50 * time.Millisecond) // 让 Stop 完成置位（coalescer.stop + stopping）

	// 交错点 C：在途期间对同 key 的新请求（合并为 pending，或直接被门禁拒绝）。
	r.pullKey(context.Background(), 100, "m1")

	// 释放首个（在途）拉号：其结束后 pending wakeup 与后续候选均不得新发起。
	close(release)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return (goroutine likely blocked)")
	}

	if got := puller.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 real pull (the in-flight one), got %d (%v)", got, puller.calledIDs())
	}
	// Stop 返回后：无论再等多久、再直接请求拉号，都不得有新发起。
	r.pullKey(context.Background(), 101, "m1")
	time.Sleep(100 * time.Millisecond)
	if got := puller.callCount(); got != 1 {
		t.Fatalf("no new pull may start after Stop returns, got %d (%v)", got, puller.calledIDs())
	}
}

// TestQuotaKeyReconciler_NoNewPullAfterStopReturns 是 F3 的竞态受控用例：反复让 Stop()
// 与本轮 runOnce 在任意候选处交错（无人工 sleep，尽量撞开时序），断言 Stop() 返回后
// 真实拉号计数恒定（含再直接调用 pullKey 也不发起）。
func TestQuotaKeyReconciler_NoNewPullAfterStopReturns(t *testing.T) {
	for i := 0; i < 20; i++ {
		mw := newFakeModelWater()
		mw.models = []string{"m1"}
		mw.level = 0
		cands := make([]int64, 0, 20)
		for k := int64(1); k <= 20; k++ {
			cands = append(cands, k)
		}
		mw.candidates = cands

		puller := newFakePuller()
		ops := newReconOpsRepo()
		r := newTestReconciler(puller, ops, mw)
		r.Start()
		r.Stop() // 与 runOnce 在任意候选处交错

		snapshot := puller.callCount()
		r.pullKey(context.Background(), 999, "m1")
		time.Sleep(10 * time.Millisecond)
		if got := puller.callCount(); got != snapshot {
			t.Fatalf("iteration %d: new pull started after Stop returned (%d -> %d)", i, snapshot, got)
		}
	}
}

// TestQuotaKeyReconciler_MountFailedSentinel_StopsRoundAndRecordsHeartbeat 验证 F4 哨兵消费：
// 拉号返回 ErrQuotaKeyMountFailed（上游已发号成功、本地挂载失败 —— shareai 非幂等，
// 上游该次配额可能已被消耗）时：①打出含 quota_key_id + "upstream may have been consumed"
// 语义的告警日志（不落凭据）；②停止本轮后续候选拉号（含其余模型）；③写失败心跳，
// LastResult 记录停轮原因。
func TestQuotaKeyReconciler_MountFailedSentinel_StopsRoundAndRecordsHeartbeat(t *testing.T) {
	readLog, restoreLog := captureLog(t)
	defer restoreLog()

	mw := newFakeModelWater()
	mw.models = []string{"m1", "m2"}
	mw.level = 0 // 池级总量 0 → 缺口 10
	// 候选是池级集合（不按模型分桶）：哨兵命中首个 key 后，其余候选全部不得再拉。
	mw.candidates = []int64{100, 101, 102, 200}

	puller := newFakePuller()
	// 哨兵以包装错误形式返回（生产为 fmt.Errorf("...: %w: %w", ...)），errors.Is 可识别。
	puller.errOnIDs[100] = fmt.Errorf("mount account to groups: %w: %w", ErrQuotaKeyMountFailed, errors.New("db down"))
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	// ②停轮：命中哨兵的 key 之后不再拉任何候选（含后续模型 m2 的候选）。
	ids := puller.calledIDs()
	if len(ids) != 1 || ids[0] != 100 {
		t.Fatalf("sentinel must stop the whole round after the failing key, got pulls %v", ids)
	}
	// ①告警日志：含 quota_key_id 与"上游可能已被消耗"语义，且不落凭据。
	logs := readLog()
	if !strings.Contains(logs, "quota key 100") {
		t.Fatalf("warn log must include quota_key_id, got %q", logs)
	}
	if !strings.Contains(logs, "upstream may have been consumed") {
		t.Fatalf("warn log must include 'upstream may have been consumed' semantics, got %q", logs)
	}
	if strings.Contains(logs, "credential") || strings.Contains(logs, "secret") {
		t.Fatalf("warn log must not leak credentials, got %q", logs)
	}
	// ③失败心跳：写一次，LastResult 记录原因、LastError 记录哨兵。
	if got := ops.hbCalls(); got != 1 {
		t.Fatalf("expected 1 heartbeat on sentinel stop, got %d", got)
	}
	if res := ops.lastResultVal(); !strings.Contains(res, "upstream may have been consumed") || !strings.Contains(res, "100") {
		t.Fatalf("failure heartbeat LastResult must record the stop reason, got %q", res)
	}
	if le := ops.lastErrorVal(); !strings.Contains(le, "mount failed after upstream success") {
		t.Fatalf("failure heartbeat LastError must record the sentinel, got %q", le)
	}
}

// TestQuotaKeyReconciler_InsertFailure_StopsRoundAndRecordsHeartbeat 是 R3-1 的停轮链用例：
// 拉号侧用真实 *QuotaKeyService（上游发号成功、本地 create.Save 落库失败），错误经哨兵
// ErrQuotaKeyMountFailed 上抛 → reconciler 按非幂等边界处理：停止本轮后续候选拉号 +
// 写失败心跳（LastError 记哨兵 / LastResult 记停轮原因）。与
// TestQuotaKeyService_Pull_InsertFailureCarriesMountFailedSentinel 构成同一条链的上下游两段。
func TestQuotaKeyReconciler_InsertFailure_StopsRoundAndRecordsHeartbeat(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 真实拉号服务 + 上游发号成功但载荷不合规（空 name）→ 本地插入被拒（落库失败）。
	// R4-2：替身 hook 记录【上游被调用的 key ID】——停轮断言直接落在"第二个 key 零上游
	// 调用"上，不再以"该 key 名下账号数为 0"作代理（账号数为 0 也可能是本次无号可拉、
	// 或落库失败，无法区分"根本没被拉"与"拉了但没落地"）。
	var mu sync.Mutex
	var upstreamKeys []int64
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "", Platform: domain.PlatformCursor, Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
		onPull: func(qk *ent.QuotaKey) {
			mu.Lock()
			upstreamKeys = append(upstreamKeys, qk.ID)
			mu.Unlock()
		},
	})
	key1 := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	key2 := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0 // 低于阈值 → 两个候选都会被尝试（除非停轮）
	mw.candidates = []int64{key1.ID, key2.ID}

	ops := newReconOpsRepo()
	r := NewQuotaKeyReconciler(newFakeQuotaKeyRepo(), svc, ops)
	r.SetModelWaterProvider(mw)

	r.runOnce()

	// 停轮（直接断言上游调用）：哨兵命中后，本轮后续候选 key2 零上游调用 —— 即停轮发生在
	// 发号之前，而非"拉了但没落库"。同时 calledKeys 恰好一个元素 ⇒ 失败的那个 key 本轮
	// 也没有被重复拉 / 盲重试（§15.6 非幂等边界：沿用既有单 flight + 停轮保护）。
	mu.Lock()
	calledKeys := append([]int64(nil), upstreamKeys...)
	mu.Unlock()
	require.Equal(t, []int64{key1.ID}, calledKeys,
		"sentinel must stop the round before any later candidate reaches upstream")
	// 落库失败的那个号未入库（事务回滚）。
	n1, err := client.Account.Query().Where(account.QuotaKeyID(key1.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n1, "failed insert must roll back (no account persisted)")

	// 失败心跳：LastError 记哨兵、LastResult 记停轮原因。
	require.Equal(t, 1, ops.hbCalls(), "expected exactly 1 heartbeat on sentinel stop")
	require.Contains(t, ops.lastErrorVal(), "mount failed after upstream success")
	require.Contains(t, ops.lastResultVal(), "upstream may have been consumed")
}

// TestQuotaKeyReconciler_NotifyDuringBusy_TriggersExactlyOneExtraRound 是 R3-3 的受控交错用例。
//
// 缺陷形态：一次 NotifyAccountRemoved 在 reconciler 中留下【两个表征】——
// accountRemovedPending 标志与 wakeCh 缓冲令牌。loop 轮出后先 select、再 CAS 补跑，
// 因此当通知发生在"由 select 触发的那一轮"期间时，两个表征会被先后各消费一次：
// CAS 补跑一轮（正确）+ 下一轮 select 又消费残留令牌再跑一轮（多跑一轮）。
//
// 本用例逐轮受控放行（gate 无缓冲，每次 send 放行一轮拉号），精确构造该交错：
//
//	第 1 轮（Start 立即跑的常规轮，忙碌中）→ 通知 #1 → 放行
//	  → select 消费通知 #1 的令牌 → 第 2 轮（忙碌中）→ 通知 #2 → 放行
//	  → 第 2 轮轮出后 CAS 补跑第 3 轮（通知 #2 的唯一一次追加轮次）→ 放行
//	  → 不得再出现第 4 轮（通知 #2 的残留令牌被重复消费）。
func TestQuotaKeyReconciler_NotifyDuringBusy_TriggersExactlyOneExtraRound(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 5 // 低于阈值 → 每轮按差额补 1 个候选
	mw.candidates = []int64{100}

	puller := newFakePuller()
	gate := make(chan struct{}) // 无缓冲：每次 send 恰好放行一轮拉号
	puller.blockOn = map[int64]chan struct{}{100: gate}
	started := make(chan int64, 8)
	puller.onStart = func(id int64) { started <- id }

	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)
	r.Start()
	defer r.Stop()
	defer close(gate) // 先于 Stop 放行任何在途拉号，避免退出时卡住

	waitRoundStart := func(round int) {
		t.Helper()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d pull did not start", round)
		}
	}

	// 第 1 轮（Start 的常规轮）阻塞在拉号 → 忙碌期通知 #1（pending + 缓冲令牌）。
	waitRoundStart(1)
	r.NotifyAccountRemoved()
	gate <- struct{}{} // 放行第 1 轮

	// 第 2 轮由 select 消费通知 #1 的令牌触发（消费即该次通知已消费，pending 已清）。
	// 其执行期间到达通知 #2：pending 置位 + wakeCh 缓冲令牌。
	waitRoundStart(2)
	r.NotifyAccountRemoved()
	gate <- struct{}{} // 放行第 2 轮

	// 第 3 轮 = 通知 #2 的唯一一次追加轮次（CAS 补跑；残留令牌须随 CAS 一并消费）。
	waitRoundStart(3)
	gate <- struct{}{} // 放行第 3 轮

	// 锁定"唤醒单次"：同一次通知不得再触发第 4 轮。
	select {
	case <-started:
		t.Fatalf("one NotifyAccountRemoved must trigger at most one extra round; observed a 4th round (pulls=%v)",
			puller.calledIDs())
	case <-time.After(500 * time.Millisecond):
	}
	if got := puller.callCount(); got != 3 {
		t.Fatalf("expected 3 pulls (initial round + notify#1 round + exactly one extra round for notify#2), got %d (%v)",
			got, puller.calledIDs())
	}
}

// TestQuotaKeyReconciler_HeartbeatPulledCountIsRoundLocal 验证 R3-4：心跳摘要中的拉号计数是
// 轮内局部计数 —— 只反映本轮成功发起的拉号数，不把历史轮次的计数累计进本轮摘要。
func TestQuotaKeyReconciler_HeartbeatPulledCountIsRoundLocal(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	// 总量口径：初始池级总量 5 / 阈值 10 → 缺口 5（正好由 5 个候选补满，每个成功拉号入库 1 个号）。
	mw.level = 5
	mw.candidates = []int64{100, 101, 102, 103, 104}

	puller := newFakePuller()
	// 每个成功拉号真实入库 1 个可服务账号 → 补满后本轮正常收尾（写成功摘要）。
	puller.autoRegisterOnSuccess(mw, 1)
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	// 第一轮：5 个候选全部成功并入库 → 达标停拉，摘要 "reconciled 5 quota keys"。
	r.runOnce()
	require.Equal(t, "reconciled 5 quota keys", ops.lastResultVal())
	require.Equal(t, quotaKeyWaterLevelThreshold, mw.currentLevel(), "round 1 must fill the pool to the target")

	// 第二轮：候选收缩到 2 个（池因账号移除回落到 8）→ 摘要只含本轮的 2，不得累计成 5+2=7。
	mw.level = 8
	mw.candidates = []int64{200, 201}
	r.runOnce()
	require.Equal(t, "reconciled 2 quota keys", ops.lastResultVal(),
		"heartbeat summary must reflect only the current round's pulls (no cross-round accumulation)")

	// 每轮写且仅写一次心跳（两轮 → 2 次）。
	require.Equal(t, 2, ops.hbCalls(), "each round writes exactly one heartbeat")
}

// TestQuotaKeyReconciler_NonMountFailedError_DoesNotStopRound 是 F4 的对照用例：
// 普通（非 MountFailed）拉号错误维持既有语义 —— 记日志、继续下一个候选，不停轮、
// 不写失败心跳。
func TestQuotaKeyReconciler_NonMountFailedError_DoesNotStopRound(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0
	mw.candidates = []int64{100, 101, 102}

	puller := newFakePuller()
	puller.errOnIDs[100] = errors.New("upstream boom") // 普通错误：与哨兵无 errors.Is 关系
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	ids := puller.calledIDs()
	if len(ids) != 3 {
		t.Fatalf("non-sentinel error must not stop the round, got pulls %v", ids)
	}
	if got := ops.hbCalls(); got != 1 {
		t.Fatalf("expected 1 heartbeat, got %d", got)
	}
	if res := ops.lastResultVal(); strings.Contains(res, "upstream may have been consumed") {
		t.Fatalf("non-sentinel error must not record sentinel stop reason, got %q", res)
	}
	if le := ops.lastErrorVal(); le != "" {
		t.Fatalf("non-sentinel error must not write a failure heartbeat, got LastError %q", le)
	}
}

// ---------------------------------------------------------------------------
// P1-D2 总量制编排（方案 §6.3 / §15.6 / §16.1）：替身驱动，不制造真实上游耗尽。
// 断言对象是 runOnce 的编排行为本身（缺口 → 有效码预检 → 拉号 → 按实际入库重算 → 达标停拉 /
// 如实报告缺口），覆盖范围不含 quota_key_service.go 的真实事务链（那部分由失败 Cases 覆盖）。
// ---------------------------------------------------------------------------

// TestQuotaKeyReconciler_PoolTotalDeficit_PrecheckPullRecomputeStopsAtTarget 验证 §6.3 编排主体：
//
//  1. 缺口驱动：total=0 < threshold=10 → 按缺口 10 取候选（limit 必须等于总量缺口，不是固定值、
//     也不是"每模型 10"）；
//  2. 每次拉号后按【实际入库账号】重算总量（不是把"已选中/已拉取的 key 数"当账号增量）；
//  3. 达标（total>=10）立即停拉：候选池里剩下的 key 一个都不再拉。
func TestQuotaKeyReconciler_PoolTotalDeficit_PrecheckPullRecomputeStopsAtTarget(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0 // 池级总量 0 → 缺口 10
	// 候选池远多于所需：只有达标前真正被消费的那几个 key 会被拉。
	mw.candidates = []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

	puller := newFakePuller()
	// 每个成功拉号真实入库 2 个可服务账号 → 5 次拉号补齐 10。
	// 若编排"把拉取过的 key 数当账号增量"，则会拉满 10 次才停——正是本用例要拦的回归。
	puller.autoRegisterOnSuccess(mw, 2)
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	// ②/③：按实际入库重算并在达标时停拉 —— 只拉前 5 个 key，其余候选（6..12）零拉取。
	require.Equal(t, []int64{1, 2, 3, 4, 5}, puller.calledIDs(),
		"must stop pulling as soon as the pool total reaches the target (pulls counted as accounts would over-pull)")
	require.Equal(t, quotaKeyWaterLevelThreshold, mw.currentLevel(), "pool must end at the target total")

	// ①：候选请求次数参数 = 总量缺口（首次 10，非每模型固定值、非单 key 15 上限）。
	require.Equal(t, []int{quotaKeyWaterLevelThreshold}, mw.requestedLimits(),
		"candidate listing must be limited by the pool deficit")

	// ②：重算发生在每次拉号之后（初始 1 次 + 每次拉号后 1 次 ≥ 6）。
	require.GreaterOrEqual(t, mw.waterCallCount(), 5+1,
		"water level must be recomputed after each pull (by newly registered accounts, not by pull attempts)")

	// 收尾：一轮一次心跳，成功摘要只含本轮真实完成拉号的 key 数。
	require.Equal(t, 1, ops.hbCalls(), "exactly one heartbeat per round")
	require.True(t, ops.lastSuccessCalled(), "round reaching the target must record success")
	require.Empty(t, ops.lastErrorVal(), "no failure must be recorded when the target is reached")
	require.Equal(t, "reconciled 5 quota keys", ops.lastResultVal())
}

// TestQuotaKeyReconciler_PoolShortfallReportedHonestly 验证 §15.6 如实报告：
// 无有效码 / 冷却 / 容量满三种分支下——不虚报补满（摘要不得出现成功拉号口径）、不生成或购买
// 激活码（零拉取或零入库），并在心跳 LastResult 里记下总量 / 阈值 / 缺口与成因。
func TestQuotaKeyReconciler_PoolShortfallReportedHonestly(t *testing.T) {
	// 三个子用例共用同一套断言工具：验证缺口的如实上报口径。
	assertShortfall := func(t *testing.T, ops *reconOpsRepo, total, wantDeficit int, cause string) {
		t.Helper()
		require.Equal(t, 1, ops.hbCalls(), "exactly one heartbeat per round even on shortfall")
		require.Empty(t, ops.lastErrorVal(), "shortfall (no effective key / exhausted upstream) must not be reported as failure")
		res := ops.lastResultVal()
		require.Contains(t, res, "pool below target", "shortfall must be honestly recorded, got %q", res)
		require.Contains(t, res, fmt.Sprintf("total=%d threshold=%d deficit=%d", total, quotaKeyWaterLevelThreshold, wantDeficit),
			"shortfall must carry total/threshold/deficit, got %q", res)
		require.Contains(t, res, cause, "shortfall must name its cause, got %q", res)
		require.NotContains(t, res, "reconciled", "shortfall must not be reported as a successful reconcile, got %q", res)
	}

	// ① 无有效码：候选集为空（全部 exhausted/过期/软删或被本轮用尽）——零拉取、如实记缺口。
	t.Run("no effective quota key", func(t *testing.T) {
		mw := newFakeModelWater()
		mw.models = []string{"m1"}
		mw.level = 3 // 缺口 7
		mw.candidates = nil

		puller := newFakePuller()
		ops := newReconOpsRepo()
		r := newTestReconciler(puller, ops, mw)

		r.runOnce()

		require.Equal(t, 0, puller.callCount(), "no candidate key → no pull at all (must never fabricate a refill)")
		require.Equal(t, 3, mw.currentLevel(), "pool total must stay honest (unchanged)")
		assertShortfall(t, ops, 3, 7, "no effective quota key")
	})

	// ② 冷却：候选码仍在冷却窗口 → 提供方预检排除，同轮不得重复拉（一次也没拉）。
	t.Run("candidate in cooldown", func(t *testing.T) {
		mw := newFakeModelWater()
		mw.models = []string{"m1"}
		mw.level = 4 // 缺口 6
		mw.candidates = []int64{501, 502}
		// 501 在冷却窗口内 → 预检排除；502 可用但上游无号（拉号返回冷却类错误，无新号入库）。
		mw.markCooldown(501)

		puller := newFakePuller()
		puller.errOnIDs[502] = fmt.Errorf("%w: 每5分钟内仅可刷新1次", ErrUpstreamCooldown)
		ops := newReconOpsRepo()
		r := newTestReconciler(puller, ops, mw)

		r.runOnce()

		// 冷却中的 501 零拉取；502 拉一次即如实收尾，绝不在本轮重复拉同一码。
		require.Equal(t, []int64{502}, puller.calledIDs(),
			"cooldown key must not be pulled this round; the remaining key must not be pulled twice")
		require.Equal(t, 4, mw.currentLevel(), "no account registered → pool total must stay 4")
		assertShortfall(t, ops, 4, 6, "no new servable cursor account registered")
	})

	// ③ 容量满：所有候选码 working 已达 MaxAccountsPerQuotaKey(15) → 预检排除，零拉取。
	t.Run("all candidates at capacity", func(t *testing.T) {
		mw := newFakeModelWater()
		mw.models = []string{"m1"}
		mw.level = 5 // 缺口 5
		mw.candidates = []int64{601, 602}
		mw.markFull(601, 602) // working >= 15：无剩余容量

		puller := newFakePuller()
		ops := newReconOpsRepo()
		r := newTestReconciler(puller, ops, mw)

		r.runOnce()

		require.Equal(t, 0, puller.callCount(), "keys at capacity must not be pulled (per-key 15 cap stays)")
		require.Equal(t, 5, mw.currentLevel(), "pool total must stay honest (unchanged)")
		assertShortfall(t, ops, 5, 5, "no effective quota key")
	})
}

// TestQuotaKeyReconciler_MountFailedSentinel_NoRepeatPullSameKeyThisRound 是 §15.6 非幂等停轮
// 断言的纯替身版本：拉号链命中 ErrQuotaKeyMountFailed（上游已发号、本地入库/挂载失败）时——
//
//  1. 立即停轮：本轮后续候选零拉取；
//  2. 同一 key 本轮不重复拉、不盲重试（既有单 flight / 非幂等保护，本卡不新增机制）；
//  3. 写失败心跳：LastError 记哨兵、LastResult 记停轮原因。
func TestQuotaKeyReconciler_MountFailedSentinel_NoRepeatPullSameKeyThisRound(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0
	mw.candidates = []int64{800, 801, 802}

	puller := newFakePuller()
	puller.errOnIDs[800] = errors.New("upstream boom")                                                         // 普通错误：不停轮
	puller.errOnIDs[801] = fmt.Errorf("create account: %w: %w", ErrQuotaKeyMountFailed, errors.New("db down")) // 哨兵：停轮
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	// ①/②：802 零拉取，且每个 key 至多被拉一次（无重复、无盲重试）。
	require.Equal(t, []int64{800, 801}, puller.calledIDs(),
		"sentinel must stop the round; no candidate may be pulled twice in one round")
	// ③：失败心跳写一次，LastError 记哨兵、LastResult 记停轮原因。
	require.Equal(t, 1, ops.hbCalls(), "exactly one heartbeat on sentinel stop")
	require.Contains(t, ops.lastErrorVal(), "mount failed after upstream success")
	require.Contains(t, ops.lastResultVal(), "upstream may have been consumed")
	require.False(t, ops.lastSuccessCalled(), "non-idempotent failure must be a failed heartbeat")
}
