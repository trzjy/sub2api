//go:build unit

package service

// §25.17.3「A05首次来源指纹」的定向测试（配对文件：cursor_gateway_forward.go /
// cursor_execution_service.go）。
//
// 钉死三件事：
//
//  1. **固定**：首次 x-jg-auth 生成时，原QuotaKeyID 与该次来源行的 CodeMAC 被固定进
//     会话 spec（两个内部字段），MAC 就是取号链同一 HMAC 对同一明文的值。
//  2. **不事后反推**：换码夹在"生成 auth"与"建 session"之间时，会话仍固定**旧头**的
//     MAC，绝不被标成新 MAC；同时反证新旧 MAC 确实不同（指纹不是恒定值蒙对）。
//  3. **失败关闭**：拿不到可靠指纹时明确失败，绝不以空值代表"与首次鉴权同源"。
//
// 指纹只在进程内传递：会话 spec 是内部结构，不进日志/DTO/工具 ID，由本卡生产代码保证。

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

// cursorRotateAfterReadRepo 在出站来源读取的**行快照返回之后立刻换码**，精确模拟
// §25.17.3 要求的「换码夹在生成 auth 与建 session 之间」。
//
// 关键：返回换码前的行快照副本，故本次鉴权材料与随之传出的指纹都必须属于**旧码**。
type cursorRotateAfterReadRepo struct {
	*cursorOutboundRepo
	mu       sync.Mutex
	rotateTo string
	rotated  bool
}

func (r *cursorRotateAfterReadRepo) GetByID(ctx context.Context, id int64) (*ent.QuotaKey, error) {
	qk, err := r.cursorOutboundRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	snapshot := *qk
	r.mu.Lock()
	if r.rotateTo != "" && !r.rotated {
		r.rotated = true
		qk.CodeEncrypted = r.rotateTo
	}
	r.mu.Unlock()
	return &snapshot, nil
}

// currentCode 读回来源行此刻的激活码（证明换码确实已发生）。
func (r *cursorRotateAfterReadRepo) currentCode(id int64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id].CodeEncrypted
}

// cursorSpecCapture 在**出站请求发生的瞬间**抓取唯一会话的 spec。
//
// 单 HTTP 完成后会话即被终结移除，故只能在存活窗口内读；这里用确定性的出站回调作
// 屏障，不用 sleep 猜时序。
type cursorSpecCapture struct {
	mu    sync.Mutex
	store *cursorSessionStore
	spec  *cursorSessionSpec
}

// bind 绑定被测执行部件的会话表（在 Execute 之前调用，出站请求才可能读到它）。
func (c *cursorSpecCapture) bind(exec *CursorExecutionService) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store = exec.sessions
}

// handler 返回在抓取后写出成功流的 handler。
func (c *cursorSpecCapture) handler(body []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		store := c.store
		c.mu.Unlock()
		if store != nil {
			store.mu.Lock()
			for _, rec := range store.sessions {
				spec := rec.spec
				c.mu.Lock()
				c.spec = &spec
				c.mu.Unlock()
			}
			store.mu.Unlock()
		}
		_, _ = w.Write(body)
	})
}

func (c *cursorSpecCapture) got(t *testing.T) cursorSessionSpec {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotNil(t, c.spec, "出站请求发生时必须能抓到已登记的会话 spec")
	return *c.spec
}

// ---------------------------------------------------------------------------
// 1. 首次生成即固定：spec 含原QuotaKeyID + 该次 CodeMAC
// ---------------------------------------------------------------------------

func TestCursorSessionSourceFingerprintFixedFromFirstAuthGeneration(t *testing.T) {
	const code = "ACTIVATION-CODE-ELEVEN"
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: code},
	})

	capture := &cursorSpecCapture{}
	svc, c := newCursorOutboundTestService(t, capture.handler(cursorOKStreamBody(t)), repo, cursorTestDeviceIDs(repo))
	capture.bind(svc.cursorExecution)

	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	_, err := executeCursorForTest(t, svc, context.Background(), c,
		cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), body, "cursor-sonnet", false, time.Now())
	require.NoError(t, err)

	spec := capture.got(t)
	require.Equal(t, int64(11), spec.sourceQuotaKeyID, "原QuotaKeyID 必须固定进 spec")
	require.Equal(t, quotaKeyMACOfPlaintext(code), spec.sourceCodeMAC,
		"CodeMAC 必须取自生成该头那一次来源行的同一明文、同一 HMAC")
	require.NotEqual(t, spec.quotaKeyID, int64(0), "既有账号侧字段不受影响")

	// 该指纹可作为可靠同源依据被取出（后续资料读取的唯一入口）。
	fp, err := cursorSessionSourceFingerprint(spec)
	require.NoError(t, err)
	require.Equal(t, int64(11), fp.QuotaKeyID)
	require.Equal(t, quotaKeyMACOfPlaintext(code), fp.CodeMAC)
}

// ---------------------------------------------------------------------------
// 2. 换码夹在生成 auth 与建 session 之间 → 旧头不得标成新 MAC
// ---------------------------------------------------------------------------

func TestCursorSessionSourceFingerprintNotRetroLabelledAfterCodeRotation(t *testing.T) {
	const oldCode, newCode = "ACTIVATION-CODE-OLD", "ACTIVATION-CODE-NEW"
	base := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: oldCode},
	})
	repo := &cursorRotateAfterReadRepo{cursorOutboundRepo: base, rotateTo: newCode}

	capture := &cursorSpecCapture{}
	svc, c := newCursorOutboundTestService(t, capture.handler(cursorOKStreamBody(t)), repo, cursorTestDeviceIDs(repo))
	capture.bind(svc.cursorExecution)

	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	_, err := executeCursorForTest(t, svc, context.Background(), c,
		cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), body, "cursor-sonnet", false, time.Now())
	require.NoError(t, err)

	// 前置：换码确实发生在本次来源读取之后（否则本用例什么也没证明）。
	require.Equal(t, newCode, repo.currentCode(11), "来源行必须已换码")

	spec := capture.got(t)
	require.Equal(t, quotaKeyMACOfPlaintext(oldCode), spec.sourceCodeMAC,
		"首次来源指纹必须固定旧头的 MAC，绝不能被事后另读一次来源标成新 MAC")
	require.NotEqual(t, quotaKeyMACOfPlaintext(newCode), spec.sourceCodeMAC,
		"新旧 MAC 必须可区分，否则本用例是蒙对的")
	require.Equal(t, int64(11), spec.sourceQuotaKeyID)
}

// ---------------------------------------------------------------------------
// 3. 拿不到可靠指纹 → 明确失败，不以空值代表同源
// ---------------------------------------------------------------------------

func TestCursorSessionSourceFingerprintFailsClosedWhenUnavailable(t *testing.T) {
	// 材料尚未生成（未调用过 CursorUpstreamAuth）时，provider 不得声称有可靠指纹。
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	provider, err := newCursorAccountAuthProvider(
		cursorOutboundAccount(1, 11, "BEARER"), repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo))
	require.NoError(t, err)
	_, ok := provider.SourceFingerprint()
	require.False(t, ok, "未生成鉴权材料时不得给出任何指纹（含空值）")

	// 生成后指纹可靠。
	_, err = provider.CursorUpstreamAuth(context.Background())
	require.NoError(t, err)
	fp, ok := provider.SourceFingerprint()
	require.True(t, ok)
	require.True(t, fp.valid())

	// 会话侧：spec 缺任一字段都必须明确失败，绝不以空值代表"与首次鉴权同源"。
	for name, spec := range map[string]cursorSessionSpec{
		"no fingerprint at all": {quotaKeyID: 11},
		"missing code mac":      {quotaKeyID: 11, sourceQuotaKeyID: 11},
		"missing quota key id":  {quotaKeyID: 11, sourceCodeMAC: quotaKeyMACOfPlaintext("X")},
		"zero quota key id":     {sourceQuotaKeyID: 0, sourceCodeMAC: quotaKeyMACOfPlaintext("X")},
	} {
		_, err := cursorSessionSourceFingerprint(spec)
		require.ErrorIs(t, err, ErrCursorSourceFingerprintUnavailable, name)
	}
}
