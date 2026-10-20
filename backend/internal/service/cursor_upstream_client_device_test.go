//go:build unit

package service

// P1-B4 设备同源统一：拉号侧 device-id 三分支解析的配对测试。
//
// 固定语义（方案 §15.4-设备边界、§16.1）：
//   1. quota_keys.device_id 已持久化 → 用它（取号与出站全链同值）；
//   2. 未持久化且该码已有账号（存量）→ 保持既有派生值 deviceID(code)，不写库、不迁移；
//   3. 未持久化且无账号（新码）→ DeviceIDFor 生成并持久化，全链复用。
//
// 本文件用**真实 *QuotaKeyService** 作解析器（只把 repo 换成不连库的内存替身），
// 因此 DeviceIDFor 走的是 B-10 的真实实现（含条件落库），"取号与出站同值"是端到端
// 校验而非替身自证。替身上游恒返回 code=0 且 metadata 为空（= errNoMetadata），
// 使一次 PullBatch 必然产生 refresh → activate → refresh 三帧，便于校验
// 同一取号链内 device-id 逐帧同值；同时不强依赖 zk01 取证向量文件。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
)

// countingQuotaKeyRepo 包裹既有内存替身，统计"账号探测"次数，用于证明分支顺序
// （① 已持久化时不得再查账号；②/③ 未持久化时才查）。
// 内嵌 QuotaKeyRepository 继承其余方法，不重复实现。
type countingQuotaKeyRepo struct {
	QuotaKeyRepository
	listCalls int
}

func (r *countingQuotaKeyRepo) ListAccountsByQuotaKey(ctx context.Context, quotaKeyID int64) ([]*ent.Account, error) {
	r.listCalls++
	return r.QuotaKeyRepository.ListAccountsByQuotaKey(ctx, quotaKeyID)
}

// newDeviceCaptureClient 起一个记录 device-id 请求头的替身上游，返回拉号客户端与取帧函数。
//
// 上游恒答 {"code":0,"data":{"metadata":""}}：refresh 因此返回 errNoMetadata，
// PullBatch 补一次 activate 后再 refresh，最终以 errNoMetadata 收尾。三帧携带的
// device-id 即为本次取号链的设备取值事实（不读响应的 bidding）。
func newDeviceCaptureClient(t *testing.T, opts ...ShareAIClientOption) (QuotaKeyUpstreamClient, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var frames []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		frames = append(frames, r.Header.Get("device-id"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"metadata":"","mail":"probe@example.com","timestamp":"1"}}`))
	}))
	t.Cleanup(ts.Close)

	base := []ShareAIClientOption{
		WithEndpoints(ts.URL, ts.URL),
		WithHTTPClient(ts.Client()),
	}
	c, err := NewShareAIUpstreamClient(fakeEncryptor{}, append(base, opts...)...)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	capture := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), frames...)
	}
	return c, capture
}

// pullAndCaptureFrames 执行一次 PullBatch 并断言其按替身上游的既定语义走到 errNoMetadata，
// 返回全部取号帧的 device-id（至少三帧：refresh → activate → refresh）。
func pullAndCaptureFrames(t *testing.T, c QuotaKeyUpstreamClient, qk *ent.QuotaKey, capture func() []string) []string {
	t.Helper()
	_, err := c.PullBatch(context.Background(), qk)
	if !errors.Is(err, errNoMetadata) {
		t.Fatalf("PullBatch = %v, want errNoMetadata（替身上游恒无 metadata）", err)
	}
	frames := capture()
	if len(frames) < 3 {
		t.Fatalf("取号链帧数 = %d, want >=3（refresh → activate → refresh），got %v", len(frames), frames)
	}
	for i, v := range frames {
		if strings.TrimSpace(v) == "" {
			t.Fatalf("第 %d 帧 device-id 为空（设备值缺失会导致上游拒绝）", i+1)
		}
	}
	return frames
}

// assertAllFramesSame 断言 refresh/activate/refresh 全程同值（取号链内设备同源）。
func assertAllFramesSame(t *testing.T, frames []string, want string) {
	t.Helper()
	for i, got := range frames {
		if got != want {
			t.Errorf("第 %d 帧 device-id = %q, want %q（取号链必须全程同值）", i+1, got, want)
		}
	}
}

// newB4ResolverService 构造真实的设备解析服务：QuotaKeyService 同时提供
// DeviceIDFor（B-10 持久化入口）与 HasAccount（本卡新增的存量判定）。
func newB4ResolverService(repo QuotaKeyRepository) *QuotaKeyService {
	return NewQuotaKeyService(repo, fakeEncryptor{}, nil, nil)
}

// ① 已持久化 device_id → 全链复用，且不得再回落到派生值、不得重查账号。
func TestCursorUpstreamClientDeviceSameOriginPersisted(t *testing.T) {
	id := int64(11)
	repo := newFakeQuotaKeyRepo()
	persisted := "AA11BB22"
	qk := &ent.QuotaKey{ID: id, CodeEncrypted: "CODE-PERSISTED", DeviceID: &persisted}
	repo.byID[id] = qk
	wrapped := &countingQuotaKeyRepo{QuotaKeyRepository: repo}
	resolver := newB4ResolverService(wrapped)

	c, capture := newDeviceCaptureClient(t, WithDeviceResolver(resolver))
	frames := pullAndCaptureFrames(t, c, qk, capture)
	assertAllFramesSame(t, frames, persisted)

	if wrapped.listCalls != 0 {
		t.Errorf("已持久化时仍查了 %d 次账号（应在分支①短路，不进入存量判定）", wrapped.listCalls)
	}
	if qk.DeviceID == nil || *qk.DeviceID != persisted {
		t.Errorf("持久化 device_id 被改写为 %v, want %q", qk.DeviceID, persisted)
	}

	// 全链同值：出站侧（同一 DeviceIDFor 入口）必须解析到同一个值。
	outbound, err := resolver.DeviceIDFor(context.Background(), id)
	if err != nil {
		t.Fatalf("出站 DeviceIDFor: %v", err)
	}
	if outbound != persisted {
		t.Errorf("出站 device-id = %q, 取号 = %q，必须同值", outbound, persisted)
	}
}

// ② 存量（未持久化 + 已有账号）→ 保持既有派生值 deviceID(code)，不写库、不迁移。
func TestCursorUpstreamClientDeviceSameOriginLegacyKey(t *testing.T) {
	const code = "CODE-LEGACY"
	id := int64(12)
	repo := newFakeQuotaKeyRepo()
	qk := &ent.QuotaKey{ID: id, CodeEncrypted: code}
	repo.byID[id] = qk
	// 该码已持有账号 = 存量 key。
	repo.accounts = []*ent.Account{{ID: 900, QuotaKeyID: &id}}
	wrapped := &countingQuotaKeyRepo{QuotaKeyRepository: repo}
	resolver := newB4ResolverService(wrapped)

	c, capture := newDeviceCaptureClient(t, WithDeviceResolver(resolver))
	frames := pullAndCaptureFrames(t, c, qk, capture)
	assertAllFramesSame(t, frames, deviceID(code))

	if wrapped.listCalls != 1 {
		t.Errorf("账号探测次数 = %d, want 1（未持久化时才进入存量判定）", wrapped.listCalls)
	}
	// §16 红线：存量先核对语义期间不得自动迁移 —— device_id 必须仍为空。
	if qk.DeviceID != nil {
		t.Errorf("存量码被写入 device_id = %q，违反「不写库、不迁移」红线", *qk.DeviceID)
	}
}

// ③ 新码（未持久化 + 无账号）→ DeviceIDFor 生成并持久化，取号与出站同值。
func TestCursorUpstreamClientDeviceSameOriginNewKey(t *testing.T) {
	const code = "CODE-FRESH"
	id := int64(13)
	repo := newFakeQuotaKeyRepo()
	qk := &ent.QuotaKey{ID: id, CodeEncrypted: code}
	repo.byID[id] = qk
	wrapped := &countingQuotaKeyRepo{QuotaKeyRepository: repo}
	resolver := newB4ResolverService(wrapped)

	c, capture := newDeviceCaptureClient(t, WithDeviceResolver(resolver))
	frames := pullAndCaptureFrames(t, c, qk, capture)

	generated := frames[0]
	assertAllFramesSame(t, frames, generated)
	if generated == deviceID(code) {
		t.Errorf("新码 device-id 退化为按激活码派生（=%q），必须走 DeviceIDFor 站点生成", generated)
	}
	if wrapped.listCalls != 1 {
		t.Errorf("账号探测次数 = %d, want 1（未持久化时才进入存量判定）", wrapped.listCalls)
	}
	if qk.DeviceID == nil || *qk.DeviceID != generated {
		t.Errorf("新码 device_id 未落库或值不一致：db=%v, 取号=%q", qk.DeviceID, generated)
	}

	// 全链同值：出站侧解析必须与本次取号完全一致。
	outbound, err := resolver.DeviceIDFor(context.Background(), id)
	if err != nil {
		t.Fatalf("出站 DeviceIDFor: %v", err)
	}
	if outbound != generated {
		t.Errorf("出站 device-id = %q, 取号 = %q，必须同值", outbound, generated)
	}

	// 落库后再次取号走分支①，值恒定不漂移。
	c2, capture2 := newDeviceCaptureClient(t, WithDeviceResolver(resolver))
	frames2 := pullAndCaptureFrames(t, c2, qk, capture2)
	assertAllFramesSame(t, frames2, generated)
}

// stubDeviceResolver 是窄依赖替身：用于注入"必错"路径与顺序校验。
type stubDeviceResolver struct {
	forCalls int
	hasCalls int
	hasOK    bool
	hasErr   error
}

func (s *stubDeviceResolver) DeviceIDFor(context.Context, int64) (string, error) {
	s.forCalls++
	return "FFFFFFFF", nil
}

func (s *stubDeviceResolver) HasAccount(context.Context, int64) (bool, error) {
	s.hasCalls++
	return s.hasOK, s.hasErr
}

// 失败关闭：账号探测出错时必须上抛，绝不带派生值继续打上游（静默回落会把
// 设备身份搞错，且错的设备会白耗上游额度）。
func TestCursorUpstreamClientDeviceSameOriginProbeErrorFailsClosed(t *testing.T) {
	probeErr := errors.New("repo down")
	resolver := &stubDeviceResolver{hasErr: probeErr}
	c, capture := newDeviceCaptureClient(t, WithDeviceResolver(resolver))

	_, err := c.PullBatch(context.Background(), &ent.QuotaKey{ID: 14, CodeEncrypted: "CODE-ERR"})
	if err == nil {
		t.Fatal("账号探测失败时必须返回错误，got nil")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("错误链须保留探测原因：%v", err)
	}
	if frames := capture(); len(frames) != 0 {
		t.Errorf("不得以派生值静默回落触网：上游收到 %d 帧 %v", len(frames), frames)
	}
}

// 未注入解析器（测试占位 / 装配缺失）→ 保持既有派生语义 deviceID(code)，行为不变。
func TestCursorUpstreamClientDeviceSameOriginNoResolverKeepsDerived(t *testing.T) {
	const code = "CODE-NO-RESOLVER"
	c, capture := newDeviceCaptureClient(t)
	frames := pullAndCaptureFrames(t, c, &ent.QuotaKey{ID: 15, CodeEncrypted: code}, capture)
	assertAllFramesSame(t, frames, deviceID(code))
}

// 生产装配闭环：NewQuotaKeyService 构造时自动把自身回填为解析器，
// 因此 wire 装配点无需改代码即可让真实拉号客户端走上三分支（新码 → DeviceIDFor）。
func TestCursorUpstreamClientDeviceResolverAutoWiredByService(t *testing.T) {
	const code = "CODE-AUTOWIRE"
	id := int64(16)
	repo := newFakeQuotaKeyRepo()
	qk := &ent.QuotaKey{ID: id, CodeEncrypted: code}
	repo.byID[id] = qk

	c, capture := newDeviceCaptureClient(t)
	svc := NewQuotaKeyService(repo, fakeEncryptor{}, c, nil)
	if svc == nil {
		t.Fatal("NewQuotaKeyService returned nil")
	}

	frames := pullAndCaptureFrames(t, c, qk, capture)
	generated := frames[0]
	if generated == deviceID(code) {
		t.Errorf("自动装配未生效：仍在用派生值 %q，应经 DeviceIDFor 生成", generated)
	}
	if qk.DeviceID == nil || *qk.DeviceID != generated {
		t.Errorf("新码 device_id 未落库：db=%v, 取号=%q", qk.DeviceID, generated)
	}
}

// HasAccount（存量判定）单元行为：有账号=true / 无账号=false / 仓储缺失=错误。
func TestCursorUpstreamClientHasAccountProbe(t *testing.T) {
	id := int64(17)
	repo := newFakeQuotaKeyRepo()
	repo.byID[id] = &ent.QuotaKey{ID: id, CodeEncrypted: "CODE-HAS-ACCOUNT"}
	svc := newB4ResolverService(repo)

	if has, err := svc.HasAccount(context.Background(), id); err != nil || has {
		t.Errorf("无账号时 HasAccount = (%v, %v), want (false, nil)", has, err)
	}

	repo.accounts = []*ent.Account{{ID: 910, QuotaKeyID: &id}}
	if has, err := svc.HasAccount(context.Background(), id); err != nil || !has {
		t.Errorf("有账号时 HasAccount = (%v, %v), want (true, nil)", has, err)
	}

	// 仓储未配置 → 明确错误（禁止静默当作"无账号"而误判成新码去生成设备）。
	if _, err := (&QuotaKeyService{}).HasAccount(context.Background(), id); err == nil {
		t.Error("repo 缺失时 HasAccount 必须返回错误")
	}
}
