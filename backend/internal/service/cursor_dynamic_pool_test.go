package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursorupstream"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// CARD-01：Cursor Auto 动态池定向测试。
//
// 覆盖 Codex 外审提出的自愈与异常分支：
//   1. 上游 GetCredentialsSecure 正常返回 —— 锁外拉号 + Token 预热加入活跃池；
//   2. 上游返回 forceReused —— 卡密冷却 60s + 自动切至备用卡；
//   3. 上游 429 —— 取号失败不污染活跃池；MarkTokenError 触发异步补池确实追加了新 Token；
//   4. 并发获取（In-Flight 计数）、释放与会话粘性迁移。
//
// 上游协议为 AES 加密（见 internal/pkg/cursorupstream），mock 复用同一 SharedSecret
// 派生的密钥做真实加解密，请求/响应均走真实协议，确保能命中 GetCredentialsSecure
// 的解析分支（force_reused、code != 200 等）。
// ---------------------------------------------------------------------------

// mockCursorUpstreamCall 记录一次上游 getCredentialsSecure 请求。
type mockCursorUpstreamCall struct {
	DeviceCode string
	DeviceMD5  string
}

// mockCursorUpstreamResponse 描述一次上游响应的构造参数。
type mockCursorUpstreamResponse struct {
	HTTPStatus       int
	Code             int
	Msg              string
	Email            string
	Token            string
	ForceReused      bool
	ForceReuseReason string
}

// mockCursorUpstream 模拟 Cursor 上游取号接口。
type mockCursorUpstream struct {
	server *httptest.Server

	mu        sync.Mutex
	calls     []mockCursorUpstreamCall
	gateFirst bool
	responder func(callIdx int, call mockCursorUpstreamCall) mockCursorUpstreamResponse

	gate     chan struct{}
	gateOnce sync.Once
	gated    chan struct{}
}

func newMockCursorUpstream(t *testing.T, responder func(callIdx int, call mockCursorUpstreamCall) mockCursorUpstreamResponse) *mockCursorUpstream {
	t.Helper()

	m := &mockCursorUpstream{
		responder: responder,
		gate:      make(chan struct{}),
		gated:     make(chan struct{}),
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	// Cleanup 按 LIFO 执行：先放闸（避免 Close 等待未完成的请求），再关服务。
	t.Cleanup(m.server.Close)
	t.Cleanup(m.ReleaseGate)
	return m
}

// GateFirstCall 让第 1 次上游请求阻塞在闸门处，用于验证拉号发生在主锁之外。
func (m *mockCursorUpstream) GateFirstCall() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gateFirst = true
}

// Gated 返回在第 1 次上游请求到达闸门时被关闭的 channel。
func (m *mockCursorUpstream) Gated() <-chan struct{} {
	return m.gated
}

// ReleaseGate 放闸，允许被阻塞的上游请求继续。
func (m *mockCursorUpstream) ReleaseGate() {
	m.gateOnce.Do(func() { close(m.gate) })
}

// URL 返回上游服务地址。
func (m *mockCursorUpstream) URL() string {
	return m.server.URL
}

// Calls 返回已记录的上游请求序列（副本）。
func (m *mockCursorUpstream) Calls() []mockCursorUpstreamCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mockCursorUpstreamCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func (m *mockCursorUpstream) handle(w http.ResponseWriter, r *http.Request) {
	var envelope struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	plain, err := cursorupstream.NewCrypto().Decrypt(envelope.Data)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var req struct {
		DeviceCode string `json:"device_code"`
		DeviceMD5  string `json:"device_code_md5"`
	}
	if err := json.Unmarshal(plain, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	call := mockCursorUpstreamCall{DeviceCode: req.DeviceCode, DeviceMD5: req.DeviceMD5}

	m.mu.Lock()
	callIdx := len(m.calls)
	m.calls = append(m.calls, call)
	gateFirst := m.gateFirst
	m.mu.Unlock()

	if gateFirst && callIdx == 0 {
		close(m.gated)
		<-m.gate
	}

	resp := m.responder(callIdx, call)

	status := resp.HTTPStatus
	if status == 0 {
		status = http.StatusOK
	}
	code := resp.Code
	if code == 0 {
		code = http.StatusOK
	}
	msg := resp.Msg
	if msg == "" {
		msg = "ok"
	}

	payload, err := json.Marshal(map[string]any{
		"email":            resp.Email,
		"token":            resp.Token,
		"forceReused":      resp.ForceReused,
		"forceReuseReason": resp.ForceReuseReason,
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	encrypted, err := cursorupstream.NewCrypto().Encrypt(payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": code,
		"msg":  msg,
		"data": encrypted,
	})
}

// cursorPoolSnapshotTokens 在主锁保护下拷贝活跃池快照，避免与异步补池竞争。
func cursorPoolSnapshotTokens(pool *CursorDynamicPool) []*DynamicCursorToken {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	out := make([]*DynamicCursorToken, len(pool.activeTokens))
	copy(out, pool.activeTokens)
	return out
}

// cursorPoolSnapshotCard 在主锁保护下拷贝单张卡密快照。
func cursorPoolSnapshotCard(pool *CursorDynamicPool, id int64) ManagedCard {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	if c, ok := pool.cards[id]; ok {
		return *c
	}
	return ManagedCard{}
}

// cursorPoolFindToken 按 AccessToken 在快照中查找。
func cursorPoolFindToken(tokens []*DynamicCursorToken, accessToken string) *DynamicCursorToken {
	for _, tok := range tokens {
		if tok.AccessToken == accessToken {
			return tok
		}
	}
	return nil
}

// TestCursorDynamicPool_AcquireAndRelease 覆盖基础的获取、粘性、释放与换号。
func TestCursorDynamicPool_AcquireAndRelease(t *testing.T) {
	pool := NewCursorDynamicPool("http://127.0.0.1:9098", zap.NewNop())

	// 手动注入两个预热 Token
	t1 := &DynamicCursorToken{
		AccessToken: "tok_1",
		Email:       "a1@test.com",
		MachineIDs:  cursorupstream.GenerateMachineIDs(),
		CreatedAt:   time.Now(),
	}
	t2 := &DynamicCursorToken{
		AccessToken: "tok_2",
		Email:       "a2@test.com",
		MachineIDs:  cursorupstream.GenerateMachineIDs(),
		CreatedAt:   time.Now(),
	}
	pool.activeTokens = append(pool.activeTokens, t1, t2)

	// 1. 首次获取 Token
	tok, err := pool.AcquireToken(context.Background(), "session_A")
	require.NoError(t, err)
	require.Equal(t, "tok_1", tok.AccessToken)
	require.Equal(t, 1, tok.InFlight)

	// 2. 同会话再次获取：优先命中 SessionAffinity 粘性
	tokSticky, err := pool.AcquireToken(context.Background(), "session_A")
	require.NoError(t, err)
	require.Equal(t, "tok_1", tokSticky.AccessToken)
	require.Equal(t, 2, tokSticky.InFlight)

	// 3. 释放一个并发
	pool.ReleaseToken(tokSticky)
	require.Equal(t, 1, tokSticky.InFlight)

	// 4. 模拟收到 429 故障：标记冷却换号
	pool.MarkTokenError(tokSticky, "session_A", 60*time.Second)
	require.Equal(t, 1, tokSticky.FailCount)
	require.True(t, tokSticky.CooldownTo.After(time.Now()))

	// 5. 换号后下一次请求会话 A：自动漂移至 t2
	nextTok, err := pool.AcquireToken(context.Background(), "session_A")
	require.NoError(t, err)
	require.Equal(t, "tok_2", nextTok.AccessToken)
}

// TestCursorDynamicPool_AcquirePullsUpstreamOutsideLockAndJoinsActivePool 验证
// 池内无 Token 时走锁外拉号，且拉到的 Token 被预热进活跃池并绑定会话。
func TestCursorDynamicPool_AcquirePullsUpstreamOutsideLockAndJoinsActivePool(t *testing.T) {
	up := newMockCursorUpstream(t, func(callIdx int, _ mockCursorUpstreamCall) mockCursorUpstreamResponse {
		return mockCursorUpstreamResponse{
			Email: fmt.Sprintf("u%d@test.com", callIdx),
			Token: fmt.Sprintf("tok_%d", callIdx),
		}
	})
	up.GateFirstCall()

	pool := NewCursorDynamicPool(up.URL(), zap.NewNop())
	pool.RegisterCard(11, "card-key-11", "dev-11", "md5-11", 5, 100)

	type acquireResult struct {
		tok *DynamicCursorToken
		err error
	}

	firstCh := make(chan acquireResult, 1)
	go func() {
		tok, err := pool.AcquireToken(context.Background(), "sess-A")
		firstCh <- acquireResult{tok: tok, err: err}
	}()

	// 等上游收到第 1 次请求并阻塞在闸门上
	select {
	case <-up.Gated():
	case <-time.After(5 * time.Second):
		t.Fatal("上游 getCredentialsSecure 未被调用")
	}

	// 上游 I/O 期间主锁必须已经释放，否则网络延迟会拖住所有并发请求。
	if !pool.mu.TryLock() {
		t.Fatal("上游 I/O 期间主锁仍被占用，拉号未发生在锁外")
	}
	pool.mu.Unlock()

	// 主锁空闲时另一个会话的获取应能独立完成
	secondCh := make(chan acquireResult, 1)
	go func() {
		tok, err := pool.AcquireToken(context.Background(), "sess-B")
		secondCh <- acquireResult{tok: tok, err: err}
	}()

	var second acquireResult
	select {
	case second = <-secondCh:
	case <-time.After(5 * time.Second):
		t.Fatal("上游 I/O 期间拉号阻塞了其它请求")
	}
	require.NoError(t, second.err)
	require.Equal(t, "tok_1", second.tok.AccessToken)
	require.Equal(t, 1, second.tok.InFlight)

	// 拉到的 Token 已被预热进活跃池
	active := cursorPoolSnapshotTokens(pool)
	require.Len(t, active, 1)
	require.Equal(t, "tok_1", active[0].AccessToken)

	up.ReleaseGate()

	var first acquireResult
	select {
	case first = <-firstCh:
	case <-time.After(5 * time.Second):
		t.Fatal("首个 AcquireToken 未返回")
	}
	require.NoError(t, first.err)
	require.Equal(t, "tok_0", first.tok.AccessToken)
	require.Equal(t, 1, first.tok.InFlight)

	// 两个 Token 均在活跃池中，并各自绑定到对应会话
	active = cursorPoolSnapshotTokens(pool)
	require.Len(t, active, 2)
	tokA := cursorPoolFindToken(active, "tok_0")
	tokB := cursorPoolFindToken(active, "tok_1")
	require.NotNil(t, tokA)
	require.NotNil(t, tokB)

	pool.mu.RLock()
	require.Same(t, tokA, pool.sessionMap["sess-A"])
	require.Same(t, tokB, pool.sessionMap["sess-B"])
	pool.mu.RUnlock()

	stats := pool.GetStats()
	require.Equal(t, 1, stats["total_cards"])
	require.Equal(t, 2, stats["active_tokens"])
}

// TestCursorDynamicPool_ForceReusedCoolsCardAndFallsBackToBackupCard 验证
// 上游返回 forceReused 时该卡密被冷却 60 秒，且本次拉号自动切至备用卡。
func TestCursorDynamicPool_ForceReusedCoolsCardAndFallsBackToBackupCard(t *testing.T) {
	up := newMockCursorUpstream(t, func(callIdx int, _ mockCursorUpstreamCall) mockCursorUpstreamResponse {
		if callIdx == 0 {
			return mockCursorUpstreamResponse{
				Email:            "forced@test.com",
				ForceReused:      true,
				ForceReuseReason: "pull_too_frequent",
			}
		}
		return mockCursorUpstreamResponse{
			Email: fmt.Sprintf("ok%d@test.com", callIdx),
			Token: fmt.Sprintf("tok_ok_%d", callIdx),
		}
	})

	pool := NewCursorDynamicPool(up.URL(), zap.NewNop())
	pool.RegisterCard(21, "card-key-21", "dev-21", "md5-21", 5, 100)
	pool.RegisterCard(22, "card-key-22", "dev-22", "md5-22", 5, 100)

	tok, err := pool.AcquireToken(context.Background(), "sess-F")
	require.NoError(t, err)
	require.Equal(t, "tok_ok_1", tok.AccessToken)

	cards := []ManagedCard{
		cursorPoolSnapshotCard(pool, 21),
		cursorPoolSnapshotCard(pool, 22),
	}

	var cooled *ManagedCard
	var fallback *ManagedCard
	for i := range cards {
		if cards[i].CooldownUntil.After(time.Now()) {
			cooled = &cards[i]
		} else {
			fallback = &cards[i]
		}
	}

	// 恰好一张卡被冷却，且不是出号成功的那张
	require.NotNil(t, cooled, "forceReused 卡密未进入冷却")
	require.NotNil(t, fallback, "未找到可用的备用卡")
	require.NotEqual(t, cooled.ID, fallback.ID)
	require.Equal(t, fallback.ID, tok.CardID)

	// 冷却窗口为 60 秒
	require.True(t, cooled.CooldownUntil.After(time.Now().Add(55*time.Second)))
	require.False(t, cooled.CooldownUntil.After(time.Now().Add(65*time.Second)))

	// 被冷却的卡不扣额度，备用卡扣额度
	require.Equal(t, 5, cooled.OverCount)
	require.Equal(t, 4, fallback.OverCount)

	// 后续拉号必须跳过已冷却的卡，只用备用卡
	next, err := pool.pullNewTokenFromAnyCard(context.Background())
	require.NoError(t, err)
	require.Equal(t, fallback.ID, next.CardID)
	require.Equal(t, "tok_ok_2", next.AccessToken)

	calls := up.Calls()
	require.Len(t, calls, 3)
	cooledCalls, fallbackCalls := 0, 0
	for _, c := range calls {
		switch c.DeviceCode {
		case cooled.DeviceCode:
			cooledCalls++
		case fallback.DeviceCode:
			fallbackCalls++
		}
	}
	require.Equal(t, 1, cooledCalls, "已冷却卡不应再次参与拉号")
	require.Equal(t, 2, fallbackCalls, "备用卡应承担全部出号")
}

// TestCursorDynamicPool_Upstream429FailsAcquireWithoutWarmingPool 验证
// 上游 429 时取号失败，且不污染活跃池、不消耗卡密额度。
func TestCursorDynamicPool_Upstream429FailsAcquireWithoutWarmingPool(t *testing.T) {
	up := newMockCursorUpstream(t, func(_ int, _ mockCursorUpstreamCall) mockCursorUpstreamResponse {
		return mockCursorUpstreamResponse{
			HTTPStatus: http.StatusTooManyRequests,
			Code:       http.StatusTooManyRequests,
			Msg:        "too many requests",
		}
	})

	pool := NewCursorDynamicPool(up.URL(), zap.NewNop())
	pool.RegisterCard(31, "card-key-31", "dev-31", "md5-31", 5, 100)

	_, err := pool.AcquireToken(context.Background(), "sess-429")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoAvailableCursorAutoToken)
	require.Contains(t, err.Error(), "429")
	require.Contains(t, err.Error(), "too many requests")

	// 失败不写入活跃池，也不消耗额度、不误判冷却
	require.Empty(t, cursorPoolSnapshotTokens(pool))
	card := cursorPoolSnapshotCard(pool, 31)
	require.Equal(t, 5, card.OverCount)
	require.True(t, card.CooldownUntil.IsZero())
}

// TestCursorDynamicPool_MarkTokenErrorAsyncPrewarmAppendsNewToken 验证
// MarkTokenError 触发的异步补池确实向上游拉到了新 Token 并追加进活跃池。
func TestCursorDynamicPool_MarkTokenErrorAsyncPrewarmAppendsNewToken(t *testing.T) {
	up := newMockCursorUpstream(t, func(callIdx int, _ mockCursorUpstreamCall) mockCursorUpstreamResponse {
		return mockCursorUpstreamResponse{
			Email: fmt.Sprintf("warm%d@test.com", callIdx),
			Token: fmt.Sprintf("tok_warm_%d", callIdx),
		}
	})

	pool := NewCursorDynamicPool(up.URL(), zap.NewNop())
	pool.RegisterCard(41, "card-key-41", "dev-41", "md5-41", 5, 100)

	tok, err := pool.AcquireToken(context.Background(), "sess-1")
	require.NoError(t, err)
	require.Equal(t, "tok_warm_0", tok.AccessToken)
	require.Len(t, cursorPoolSnapshotTokens(pool), 1)

	pool.MarkTokenError(tok, "sess-1", 60*time.Second)

	// 故障 Token 被标记并进入冷却
	require.Equal(t, 1, tok.FailCount)
	require.True(t, tok.CooldownTo.After(time.Now()))

	// 故障会话被解绑
	pool.mu.RLock()
	_, stillBound := pool.sessionMap["sess-1"]
	pool.mu.RUnlock()
	require.False(t, stillBound, "故障会话未从粘性表解绑")

	// 异步补池确实拉到了新 Token 并追加进活跃池
	require.Eventually(t, func() bool {
		return cursorPoolFindToken(cursorPoolSnapshotTokens(pool), "tok_warm_1") != nil
	}, 3*time.Second, 20*time.Millisecond, "异步补池未把新 Token 追加进活跃池")

	active := cursorPoolSnapshotTokens(pool)
	require.Len(t, active, 2)
	warmed := cursorPoolFindToken(active, "tok_warm_1")
	require.NotNil(t, warmed)
	require.Equal(t, tok.CardID, warmed.CardID)
	require.Equal(t, 0, warmed.InFlight)
	require.Equal(t, 0, warmed.FailCount)

	// 补池消耗了一张额度
	card := cursorPoolSnapshotCard(pool, 41)
	require.Equal(t, 3, card.OverCount)
}

// TestCursorDynamicPool_ConcurrentAcquireRespectsInFlightCapAndRelease 验证
// 并发获取遵守单 Token 的 In-Flight 上限，释放后计数归零。
func TestCursorDynamicPool_ConcurrentAcquireRespectsInFlightCapAndRelease(t *testing.T) {
	pool := NewCursorDynamicPool("http://127.0.0.1:1", zap.NewNop())

	// 预置 3 个 Token，每个 Token 最多 2 个并发 → 总容量 6
	poolTokens := make([]*DynamicCursorToken, 3)
	for i := range poolTokens {
		poolTokens[i] = &DynamicCursorToken{
			AccessToken: fmt.Sprintf("tok_%d", i),
			Email:       fmt.Sprintf("u%d@test.com", i),
			MachineIDs:  cursorupstream.GenerateMachineIDs(),
			CreatedAt:   time.Now(),
		}
		pool.activeTokens = append(pool.activeTokens, poolTokens[i])
	}
	require.Equal(t, 2, pool.maxInFlight)

	const concurrency = 6
	acquired := make([]*DynamicCursorToken, concurrency)
	acquireErrs := make([]error, concurrency)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			acquired[idx], acquireErrs[idx] = pool.AcquireToken(context.Background(), fmt.Sprintf("sess-%d", idx))
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < concurrency; i++ {
		require.NoError(t, acquireErrs[i], "并发获取失败")
		require.NotNil(t, acquired[i])
	}

	// In-Flight 计数：任何 Token 都不超过上限，且总和等于并发数
	total := 0
	for _, tok := range poolTokens {
		require.LessOrEqual(t, tok.InFlight, pool.maxInFlight)
		require.Equal(t, pool.maxInFlight, tok.InFlight)
		total += tok.InFlight
	}
	require.Equal(t, concurrency, total)

	// 释放后全部归零
	for i := 0; i < concurrency; i++ {
		pool.ReleaseToken(acquired[i])
	}
	for _, tok := range poolTokens {
		require.Equal(t, 0, tok.InFlight)
	}

	// 重复释放不会把计数压成负数
	pool.ReleaseToken(poolTokens[0])
	pool.ReleaseToken(nil)
	require.Equal(t, 0, poolTokens[0].InFlight)
}

// TestCursorDynamicPool_SessionAffinityMigratesWhenBoundTokenCoolsDown 验证
// 绑定的 Token 进入冷却后会话解绑并漂移到其它 Token。
func TestCursorDynamicPool_SessionAffinityMigratesWhenBoundTokenCoolsDown(t *testing.T) {
	pool := NewCursorDynamicPool("http://127.0.0.1:1", zap.NewNop())

	tokA := &DynamicCursorToken{
		AccessToken: "tok_A",
		Email:       "a@test.com",
		MachineIDs:  cursorupstream.GenerateMachineIDs(),
		CreatedAt:   time.Now(),
	}
	tokB := &DynamicCursorToken{
		AccessToken: "tok_B",
		Email:       "b@test.com",
		MachineIDs:  cursorupstream.GenerateMachineIDs(),
		CreatedAt:   time.Now(),
	}
	pool.activeTokens = append(pool.activeTokens, tokA, tokB)
	pool.sessionMap["sess-X"] = tokA

	// 绑定 Token 健康 → 命中粘性
	got, err := pool.AcquireToken(context.Background(), "sess-X")
	require.NoError(t, err)
	require.Same(t, tokA, got)
	require.Equal(t, 1, tokA.InFlight)

	// 绑定 Token 进入冷却 → 解绑并漂移
	tokA.CooldownTo = time.Now().Add(time.Minute)
	got2, err := pool.AcquireToken(context.Background(), "sess-X")
	require.NoError(t, err)
	require.Same(t, tokB, got2)

	pool.mu.RLock()
	require.Same(t, tokB, pool.sessionMap["sess-X"])
	pool.mu.RUnlock()
}

// TestCursorDynamicPool_AcquireWithoutCardsReturnsExhausted 验证无任何可用卡密时
// 返回可识别的耗尽错误，而不是 panic 或空指针。
func TestCursorDynamicPool_AcquireWithoutCardsReturnsExhausted(t *testing.T) {
	pool := NewCursorDynamicPool("http://127.0.0.1:1", zap.NewNop())

	_, err := pool.AcquireToken(context.Background(), "sess-empty")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoAvailableCursorAutoToken)
}

// TestCursorDynamicPool_SessionAffinityRespectsMaxInFlight 锁定会话粘性分支的并发上限
// （增量外审 P1）。
//
// 回归背景：会话粘性分支原先只检查「未冷却 + 无失败计数」，**没有**检查
// tok.InFlight < p.maxInFlight，而普通选取分支有该检查。于是同一 sessionID
// 绑定到健康 Token 后，第三次并发调用会把同一个 Token 的 InFlight 顶到 3，
// 突破 maxInFlight=2 的单 Token 并发上限 —— 同一上游试用号被同时使用 3 路。
//
// 修复后：粘性 Token 达到并发上限时走既有解绑路径（delete sessionMap），
// 漂移到别的有余量 Token 或拉新 Token，InFlight 永远不超过 maxInFlight。
func TestCursorDynamicPool_SessionAffinityRespectsMaxInFlight(t *testing.T) {
	up := newMockCursorUpstream(t, func(callIdx int, _ mockCursorUpstreamCall) mockCursorUpstreamResponse {
		return mockCursorUpstreamResponse{
			Email: fmt.Sprintf("cap%d@test.com", callIdx),
			Token: fmt.Sprintf("tok_cap_%d", callIdx),
		}
	})

	pool := NewCursorDynamicPool(up.URL(), zap.NewNop())
	pool.RegisterCard(51, "card-key-51", "dev-51", "md5-51", 5, 100)
	require.Equal(t, 2, pool.maxInFlight)

	// 第 1 次：池空 → 走上游拉号，绑定 sess-cap。
	tok1, err := pool.AcquireToken(context.Background(), "sess-cap")
	require.NoError(t, err)
	require.Equal(t, "tok_cap_0", tok1.AccessToken)
	require.Equal(t, 1, tok1.InFlight)

	// 第 2 次：同会话命中粘性 → 同一个 Token，占满 maxInFlight=2。
	tok2, err := pool.AcquireToken(context.Background(), "sess-cap")
	require.NoError(t, err)
	require.Same(t, tok1, tok2, "同会话未达上限时必须保持粘性")
	require.Equal(t, 2, tok1.InFlight)

	// 第 3 次：粘性 Token 已达并发上限 → 绝不能把 InFlight 顶到 3。
	tok3, err := pool.AcquireToken(context.Background(), "sess-cap")
	require.NoError(t, err)

	// 核心断言：原 Token 的 InFlight 不得突破上限。
	require.LessOrEqual(t, tok1.InFlight, pool.maxInFlight,
		"会话粘性分支突破了单 Token 并发上限（InFlight=%d > maxInFlight=%d）", tok1.InFlight, pool.maxInFlight)
	require.Equal(t, 2, tok1.InFlight, "原 Token 的并发占用不应再增加")

	// 且必须漂移到另一个 Token（本次是上游拉的新号），而不是继续压原 Token。
	require.NotSame(t, tok1, tok3, "达到并发上限后必须漂移到其它 Token")
	require.Equal(t, "tok_cap_1", tok3.AccessToken)
	require.Equal(t, 1, tok3.InFlight)

	// 会话已重新绑定到新 Token，池内每个 Token 的 InFlight 都在上限内。
	pool.mu.RLock()
	require.Same(t, tok3, pool.sessionMap["sess-cap"])
	pool.mu.RUnlock()

	for _, tok := range cursorPoolSnapshotTokens(pool) {
		require.LessOrEqual(t, tok.InFlight, pool.maxInFlight)
	}
}

// TestCursorDynamicPool_MarkTokenErrorKeepsBindingAfterSessionMigrated 锁定 P1：
// 会话迁移到新 Token 后，旧 Token 迟到的 429 不得把**新绑定**误删。
//
// 回归背景：MarkTokenError 原先无条件执行 delete(p.sessionMap, sessionID)。
// 但会话因并发上限/冷却漂移后，p.sessionMap[sessionID] 已指向**新** Token；
// 此时旧 Token 的迟到 429 会把新绑定误删 —— 粘性被无谓打断，
// 且被绑的新 Token 根本没有任何故障。
//
// 修复后：只有 p.sessionMap[sessionID] 仍指向出故障的 tok 时才解绑。
func TestCursorDynamicPool_MarkTokenErrorKeepsBindingAfterSessionMigrated(t *testing.T) {
	up := newMockCursorUpstream(t, func(callIdx int, _ mockCursorUpstreamCall) mockCursorUpstreamResponse {
		return mockCursorUpstreamResponse{
			Email: fmt.Sprintf("mig%d@test.com", callIdx),
			Token: fmt.Sprintf("tok_mig_%d", callIdx),
		}
	})

	pool := NewCursorDynamicPool(up.URL(), zap.NewNop())
	pool.RegisterCard(61, "card-key-61", "dev-61", "md5-61", 5, 100)

	// 第 1 次：池空 → 拉号 tok_mig_0（记为 tokA），会话粘在 tokA 上。
	tokA, err := pool.AcquireToken(context.Background(), "sess-mig")
	require.NoError(t, err)
	require.Equal(t, "tok_mig_0", tokA.AccessToken)
	require.Equal(t, 1, tokA.InFlight)

	// 第 2 次：同会话命中粘性 → 仍是 tokA，占满 maxInFlight=2。
	tokAgain, err := pool.AcquireToken(context.Background(), "sess-mig")
	require.NoError(t, err)
	require.Same(t, tokA, tokAgain)
	require.Equal(t, 2, tokA.InFlight)

	// 第 3 次：tokA 已达并发上限 → 解绑并漂移到新 Token（tok_mig_1）。
	tokB, err := pool.AcquireToken(context.Background(), "sess-mig")
	require.NoError(t, err)
	require.NotSame(t, tokA, tokB, "达到并发上限后必须漂移到新 Token")
	require.Equal(t, "tok_mig_1", tokB.AccessToken)

	pool.mu.RLock()
	require.Same(t, tokB, pool.sessionMap["sess-mig"], "前置条件：会话应已迁移到新 Token")
	pool.mu.RUnlock()

	// 旧 Token 迟到 429：标记 tokA 故障，但**不得**删掉指向 tokB 的新绑定。
	pool.MarkTokenError(tokA, "sess-mig", 60*time.Second)

	// tokA 确实被标记故障（冷却 + 失败计数）。
	require.Equal(t, 1, tokA.FailCount)
	require.True(t, tokA.CooldownTo.After(time.Now()))

	// 核心断言：会话的新绑定仍在，未被旧 Token 的迟到故障误删。
	pool.mu.RLock()
	bound, stillBound := pool.sessionMap["sess-mig"]
	pool.mu.RUnlock()
	require.True(t, stillBound, "旧 Token 的迟到故障把会话的新绑定误删了")
	require.Same(t, tokB, bound, "会话绑定必须仍指向迁移后的新 Token")

	// 反向对照：故障会话仍绑在该 Token 上时，MarkTokenError 必须照常解绑。
	pool.MarkTokenError(tokB, "sess-mig", 60*time.Second)
	pool.mu.RLock()
	_, stillBound = pool.sessionMap["sess-mig"]
	pool.mu.RUnlock()
	require.False(t, stillBound, "会话确实绑在该故障 Token 上时必须解绑")
}

// TestResolveCursorAutoUpstreamBaseURL 表驱动验证上游基址解析口径：
// 非空、绝对 http(s) URL、带主机名、无内嵌凭据；未配置即失败关闭；
// 不限制明文 http；不回落默认地址；返回去尾部斜杠。
func TestResolveCursorAutoUpstreamBaseURL(t *testing.T) {
	cases := []struct {
		name      string
		value     string
		wantErr   bool
		wantValue string
	}{
		{name: "empty", value: "", wantErr: true},
		{name: "whitespace-only", value: "   ", wantErr: true},
		{name: "http-ip-port", value: "http://82.157.20.83:9098", wantErr: false, wantValue: "http://82.157.20.83:9098"},
		{name: "ftp-scheme", value: "ftp://cursor.example.com", wantErr: true},
		{name: "https-no-host", value: "https:///api", wantErr: true},
		{name: "https-embedded-creds", value: "https://user:pass@cursor.example.com", wantErr: true},
		{name: "https-trailing-slash", value: "https://cursor.example.com/", wantErr: false, wantValue: "https://cursor.example.com"},
		{name: "http-private-ip", value: "http://10.0.0.5:9098", wantErr: false, wantValue: "http://10.0.0.5:9098"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.value == "" {
				// 确保未配置：清空该环境变量
				t.Setenv(EnvCursorAutoUpstreamBaseURL, "")
			} else {
				t.Setenv(EnvCursorAutoUpstreamBaseURL, c.value)
			}

			got, err := ResolveCursorAutoUpstreamBaseURL()
			if c.wantErr {
				require.Error(t, err, "期望解析失败但成功返回: %q", c.value)
				return
			}
			require.NoError(t, err, "期望解析成功但报错: %q -> %v", c.value, err)
			require.Equal(t, c.wantValue, got, "返回值与期望不符")
		})
	}
}

// TestCursorDynamicPool_MarkUpstreamUnavailable_FailsClosed 验证号池失败关闭：
// 上游不可用被记录后，AcquireToken 直接返回该错误、不访问网络、不 panic。
func TestCursorDynamicPool_MarkUpstreamUnavailable_FailsClosed(t *testing.T) {
	pool := NewCursorDynamicPool("http://127.0.0.1:1", zap.NewNop())

	pool.MarkUpstreamUnavailable(errors.New("boom"))

	tok, err := pool.AcquireToken(context.Background(), "")
	require.Error(t, err, "上游不可用时 AcquireToken 必须返回错误")
	require.Nil(t, tok, "上游不可用时不应返回 Token")
	require.Contains(t, err.Error(), "boom", "返回错误应包含上游不可用原因")
}
