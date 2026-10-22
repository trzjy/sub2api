//go:build unit

package service

// P1-A2 出站鉴权重构的单测（配对文件：cursor_gateway_forward.go / cursor_bridge.go）。
//
// 钉死四条硬性质：
//  1. 出站头 = 选中账号已存 access_token（Authorization: Bearer）+ 同源 device/code
//     的 x-jg-auth；不同账号各持各的凭证，绝不共享任何会话材料（方案 §15.2-1）。
//  2. deviceId 三分支与取号链同值：① 已持久化 → 用它；② 存量未持久化 → 取号链同款
//     派生值 deviceID(code)（不生成、不落库）；③ 新码 → DeviceIDFor 生成并持久化。
//  3. 流内拒绝（HTTP200 正常帧里的固定模板）一律终态失败：不得当成功、不得产出 usage。
//  4. 流内拒绝 → lookup 核对：仅当确认「密钥已过期」才走既有写口标 expired；
//     无法确认时按上游失败返回，不删号、不标账号耗尽（I2 红线）。
//
// 旧链（按 key 换 session token）已随 cursor_auth_token.go 删除：本文件同时反证
// 出站不再产生任何 AuthService 调用。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	wasmcrypto "github.com/Wei-Shaw/sub2api/internal/service/wasm"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// cursorOutboundRepo 是 QuotaKeyRepository 替身：在 identityCursorQuotaKeys 之上只放开
// 两个观测点——"归属账号列表"（device 三分支②的判据）与"device 落库调用次数"
// （用于反证存量分支不生成、不持久化新 device）。
type cursorOutboundRepo struct {
	*identityCursorQuotaKeys
	mu             sync.Mutex
	accounts       []*ent.Account
	setDeviceCalls int
}

func (r *cursorOutboundRepo) ListAccountsByQuotaKey(context.Context, int64) ([]*ent.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accounts, nil
}

func (r *cursorOutboundRepo) SetDeviceIDIfAbsent(ctx context.Context, id int64, deviceID string) (int, error) {
	r.mu.Lock()
	r.setDeviceCalls++
	r.mu.Unlock()
	return r.identityCursorQuotaKeys.SetDeviceIDIfAbsent(ctx, id, deviceID)
}

func (r *cursorOutboundRepo) deviceWrites() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.setDeviceCalls
}

// newCursorOutboundRepo 构造替身仓储（CodeEncrypted 即明文激活码）。
func newCursorOutboundRepo(rows map[int64]*ent.QuotaKey) *cursorOutboundRepo {
	return &cursorOutboundRepo{identityCursorQuotaKeys: &identityCursorQuotaKeys{byID: rows}}
}

// cursorOutboundUpstream 是只扮演 ChatService 的假中转（旧链的 AuthService 端点已删除，
// 故本替身不提供任何换 token 路径；出站路径集合本身就是"旧链已死"的反证）。
type cursorOutboundUpstream struct {
	mu       sync.Mutex
	requests []cursorE2ERequest
	body     []byte
	status   int
}

func (u *cursorOutboundUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		u.mu.Lock()
		u.requests = append(u.requests, cursorE2ERequest{
			path: r.URL.Path, body: buf[:n], header: r.Header.Clone(),
		})
		status, body := u.status, u.body
		u.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write(body)
	})
}

func (u *cursorOutboundUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func (u *cursorOutboundUpstream) at(i int) cursorE2ERequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[i]
}

func (u *cursorOutboundUpstream) paths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, 0, len(u.requests))
	for _, r := range u.requests {
		out = append(out, r.path)
	}
	return out
}

// newCursorOutboundTestService 装配指向假中转的 OpenAIGatewayService。
func newCursorOutboundTestService(
	t *testing.T,
	upstream http.Handler,
	repo QuotaKeyRepository,
	deviceIDs CursorDeviceIDResolver,
) (*OpenAIGatewayService, *gin.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ts := httptest.NewUnstartedServer(upstream)
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)

	nodes := NewCursorUpstreamNodesResolver(WithCursorNodesCacheTTL(time.Hour))
	nodes.cached = CursorUpstreamNodes{HTTP1Base: ts.URL, HTTP2Base: ts.URL}
	nodes.cachedAt = time.Now()

	svc := NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil /* cursorExecution */)
	svc.cursorExecution = attachCursorTestExecution(t, svc, repo, NewCursorTestEncryptor(), deviceIDs,
		NewCursorUpstreamForwarder(
			WithCursorUpstreamNodes(nodes),
			WithCursorUpstreamHTTPClient(ts.Client()),
		))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return svc, c
}

// cursorOKStreamBody 构造一条成功的 Cursor 响应流（stream_start + 文本 + EndStream）。
func cursorOKStreamBody(t *testing.T) []byte {
	t.Helper()
	var out []byte
	out = append(out, cursorTextDeltaFrame(t, "")...)
	out = append(out, cursorTextDeltaFrame(t, "Hello")...)
	out = append(out, cursorTestEndStream(t, `{}`)...)
	return out
}

// cursorOutboundAccount 构造一个带模型映射与用户级 Bearer 的 cursor 账号。
func cursorOutboundAccount(id int64, quotaKeyID int64, bearer string) *Account {
	return &Account{
		ID: id, Platform: PlatformCursor, QuotaKeyID: qkID(quotaKeyID),
		Credentials: map[string]any{
			"model_mapping": map[string]any{"cursor-sonnet": "upstream-sonnet"},
			"access_token":  bearer,
		},
	}
}

// cursorDecodeJgAuth 解出站材料 zk03 产物，读出 {d,a}（激活码/deviceId 均为非凭据标识）。
func cursorDecodeJgAuth(t *testing.T, material string) cursorEncryptedCredentialsInput {
	t.Helper()
	plain, err := wasmcrypto.DecryptLocalData(material)
	require.NoError(t, err)
	var decoded cursorEncryptedCredentialsInput
	require.NoError(t, json.Unmarshal([]byte(plain), &decoded))
	return decoded
}

// ---------------------------------------------------------------------------
// Done when 1：出站头 = 账号 Bearer + x-jg-auth，且凭证归属本账号
// ---------------------------------------------------------------------------

// TestCursorOutboundHeadersCarryPerAccountBearerAndDeviceAuth 钉死出站头的两维身份：
// 用户级 = 选中账号已存的 access_token，设备级 = 该码同源 device/code 的 x-jg-auth。
//
// 关键性质是**凭证归属**：两个不同账号（不同 quota key、不同 access_token）出站后，
// 各自请求里的 Bearer 与 x-jg-auth 必须逐字属于自己，绝不出现跨账号串号。
func TestCursorOutboundHeadersCarryPerAccountBearerAndDeviceAuth(t *testing.T) {
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
		22: {ID: 22, CodeEncrypted: "ACTIVATION-CODE-TWENTYTWO"},
	})
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	svc, c := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	ctx := context.Background()

	resA, err := executeCursorForTest(t, svc, ctx, c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), body, "cursor-sonnet", false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, resA)
	resB, err := executeCursorForTest(t, svc, ctx, c, cursorOutboundAccount(2, 22, "BEARER-ACCOUNT-B"), body, "cursor-sonnet", false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, resB)

	// 旧链已删：出站只应有对话调用，不得再打任何 AuthService 端点。
	require.Equal(t, []string{
		cursorPathAgentRun,
		cursorPathAgentRun,
	}, up.paths())

	reqA, reqB := up.at(0), up.at(1)
	// 用户级：各自账号已存的 access_token（不是换来的 session token，也不是激活码）。
	require.Equal(t, "Bearer BEARER-ACCOUNT-A", reqA.header.Get("Authorization"))
	require.Equal(t, "Bearer BEARER-ACCOUNT-B", reqB.header.Get("Authorization"))

	jgA, jgB := reqA.header.Get("x-jg-auth"), reqB.header.Get("x-jg-auth")
	require.NotEmpty(t, jgA, "device-level auth must be present")
	require.NotEmpty(t, jgB, "device-level auth must be present")
	require.NotEqual(t, jgA, jgB, "两个账号绝不能共享同一份鉴权材料（跨账号复用 = 串号）")

	// 凭证归属：解出明文后，每个账号的 d/a 必须属于自己的码与设备。
	decA, decB := cursorDecodeJgAuth(t, jgA), cursorDecodeJgAuth(t, jgB)
	require.Equal(t, "ACTIVATION-CODE-ELEVEN", decA.A)
	require.Equal(t, "ACTIVATION-CODE-TWENTYTWO", decB.A)
	require.NotEqual(t, decA.D, decB.D, "deviceId 必须与账号绑定，不得共用")
	require.NotContains(t, jgB, decA.A, "B 号出站绝不能携带 A 号的激活码")
	require.NotContains(t, jgA, decB.A, "A 号出站绝不能携带 B 号的激活码")

	// 同一账号重复出站：Bearer 恒定（账号已存凭证），不产生第二次换 token。
	_, err = executeCursorForTest(t, svc, ctx, c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), body, "cursor-sonnet", false, time.Now())
	require.NoError(t, err)
	require.Equal(t, "Bearer BEARER-ACCOUNT-A", up.at(2).header.Get("Authorization"))
	require.Equal(t, 3, up.count())
}

// TestCursorOutboundFailsClosedWithoutAccountBearer 钉死账号凭证缺失时失败关闭。
//
// 证据 134-no-bearer-active-key：无 Bearer 也能拿到 HTTP200 成功样本——那是"上游未拒绝"，
// **不等于**本站可以拿空凭证出站；更不因此删账号对象或回收 Bearer（I2 红线）。
func TestCursorOutboundFailsClosedWithoutAccountBearer(t *testing.T) {
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	svc, c := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	acc := cursorOutboundAccount(1, 11, "")
	delete(acc.Credentials, "access_token")

	_, err := executeCursorForTest(t, svc, context.Background(), c, acc, body, "cursor-sonnet", false, time.Now())
	require.ErrorIs(t, err, ErrCursorAccountBearerMissing)
	require.Zero(t, up.count(), "凭证缺失必须在发出任何出站请求之前失败关闭")
}

// ---------------------------------------------------------------------------
// Done when 2：deviceId 三分支与取号链同值
// ---------------------------------------------------------------------------

// TestCursorOutboundDeviceIDMatchesPullChainThreeBranches 钉死出站 deviceId 的三分支
// 与取号链（refresh/activate/lookup）**同判据、同结果**：同一激活码在两条链上恒为
// 同一设备身份，绝不出现"取号链用派生值、出站链凭空生成一份"的分裂（证据 124）。
func TestCursorOutboundDeviceIDMatchesPullChainThreeBranches(t *testing.T) {
	ctx := context.Background()

	// ① 已持久化 device_id → 用它（取号链读同一列，两链同为持久化值）。
	persisted := "PERSISTED-DEVICE-0001"
	repoA := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		31: {ID: 31, CodeEncrypted: "ACTIVATION-CODE-31", DeviceID: &persisted},
	})
	gotA, err := cursorResolveOutboundDeviceID(ctx, repoA, cursorTestDeviceIDs(repoA), repoA.byID[31], "ACTIVATION-CODE-31")
	require.NoError(t, err)
	require.Equal(t, persisted, gotA, "分支①：出站必须复用已持久化的 device_id（与取号链同值）")
	require.Zero(t, repoA.deviceWrites(), "分支①：已有值不得再生成/落库")

	// ② 存量（未持久化但已持有账号）→ 取号链同款派生值 deviceID(code)。
	//
	// 取号链的事实源是 cursor_upstream_client.go 的 deviceID(code)（refresh/activate
	// 的 device-id 请求头），故此处直接与该函数的输出比对——两链必须逐字同值。
	const code32 = "ACTIVATION-CODE-32"
	repoB := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		32: {ID: 32, CodeEncrypted: code32},
	})
	repoB.accounts = []*ent.Account{{ID: 9, QuotaKeyID: qkID(32)}}
	gotB, err := cursorResolveOutboundDeviceID(ctx, repoB, cursorTestDeviceIDs(repoB), repoB.byID[32], code32)
	require.NoError(t, err)
	require.Equal(t, deviceID(code32), gotB,
		"分支②：存量未持久化分支两链必须同为取号链派生值 deviceID(code)")
	require.Zero(t, repoB.deviceWrites(),
		"分支②：存量码绝不生成、绝不落库新 device（§16 红线：不自动改设备身份）")

	// ③ 新码（未持久化且无账号）→ DeviceIDFor 生成并持久化，之后全链复用该值。
	const code33 = "ACTIVATION-CODE-33"
	repoC := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		33: {ID: 33, CodeEncrypted: code33},
	})
	gotC, err := cursorResolveOutboundDeviceID(ctx, repoC, cursorTestDeviceIDs(repoC), repoC.byID[33], code33)
	require.NoError(t, err)
	require.NotEmpty(t, gotC)
	require.Equal(t, 1, repoC.deviceWrites(), "分支③：新码必须生成并持久化一份 deviceId")
	require.Equal(t, gotC, *repoC.byID[33].DeviceID, "分支③：生成值必须落库，供两链复用")
	require.NotEqual(t, deviceID(code33), gotC, "分支③：新码走随机生成，绝不回落按码派生")

	// 落库后再次解析（取号链视角第二次取号 / 出站第二次请求）必须恒同值：
	// 即分支③ 会收敛到分支①，两条链永不分裂。
	gotCAgain, err := cursorResolveOutboundDeviceID(ctx, repoC, cursorTestDeviceIDs(repoC), repoC.byID[33], code33)
	require.NoError(t, err)
	require.Equal(t, gotC, gotCAgain, "deviceId 落库后两链必须永久复用同一份")
	require.Equal(t, 1, repoC.deviceWrites(), "已有值不得重复生成")

	// 出站材料里的 d 必须就是上述解析值（把三分支结果钉到实际出站头上）。
	provC, err := newCursorAccountAuthProvider(
		cursorOutboundAccount(3, 33, "BEARER-ACCOUNT-C"), repoC, NewCursorTestEncryptor(), cursorTestDeviceIDs(repoC))
	require.NoError(t, err)
	materialC, err := provC.CursorUpstreamAuth(ctx)
	require.NoError(t, err)
	require.Equal(t, gotC, cursorDecodeJgAuth(t, materialC).D,
		"出站 x-jg-auth 的 d 必须等于三分支解析出的 deviceId")
}

// ---------------------------------------------------------------------------
// 流内拒绝：终态错误，不核对、不写来源状态
// ---------------------------------------------------------------------------

// cursorRejectedStreamBody 构造一条 HTTP200 但流内含终态拒绝模板的响应。
//
// 模板后跟动态后缀标识（证据 125 / 134 样本：` (1792327810746-28d0be39)`），
// 判据扫描按前缀匹配即可命中，后缀原样进结构化字段。
func cursorRejectedStreamBody(t *testing.T) []byte {
	t.Helper()
	var out []byte
	out = append(out, cursorTextDeltaFrame(t, "")...)
	out = append(out, cursorTextDeltaFrame(t, "密钥已过期或额度耗尽，请续费后使用 (1792327810746-28d0be39)")...)
	out = append(out, cursorTestEndStream(t, `{}`)...)
	return out
}

// TestCursorStreamRejectionIsTerminalNeverSuccess 钉死流内拒绝不得当成功、不得产出 usage。
//
// 按 §25.14-C：流内固定拒绝**只**返回明确上游错误并记录安全阶段，不再发起流内
// lookup 核对，也不再修改来源期限/状态（那些只归唯一 lookup 观测入口）。
func TestCursorStreamRejectionIsTerminalNeverSuccess(t *testing.T) {
	up := &cursorOutboundUpstream{body: cursorRejectedStreamBody(t)}
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	svc, c := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

	res, err := executeCursorForTest(t, svc, context.Background(), c, acc, body, "cursor-sonnet", false, time.Now())
	require.Error(t, err)
	require.Nil(t, res, "流内拒绝绝不能返回可计费结果（不得当成功、不得伪造 usage）")
	require.ErrorIs(t, err, ErrCursorKeyRejectedTerminal)
	require.Contains(t, err.Error(), cursorKeyRejectedTemplate)

	// 模板识别本身：结构化片段含动态后缀标识，用于与上游侧记录对齐。
	rej, ok := cursorDetectKeyRejectedUpstream(cursorRejectedStreamBody(t))
	require.True(t, ok)
	require.Equal(t, "1792327810746-28d0be39", rej.TurnID)
	require.Contains(t, rej.Snippet, cursorKeyRejectedTemplate)
}

// TestCursorOutboundHeadersAlignToolProbe 钉死 O11-C1 修复：forwardCursor 出站头必须
// 含活体全通探针（工具请求形态）的 11 项客户端上下文头，否则带 mcp_tools 的上游请求
// 会挂起（生产 request_id 74d9673a：first_frame 后无后续事件，180s 超时）。
//
// 这 11 项头由探针 headers() 逐字对齐（除 x-request-id 与 x-jg-auth 已由鉴权注入），
// 值恒定不依赖请求级上下文，既不改动既有 6 项的值/鉴权，也不引入动态值。
func TestCursorOutboundHeadersAlignToolProbe(t *testing.T) {
	// 活体全通探针的客户端上下文头（值逐字对齐，O11-C1 修法唯一清单）。
	wantClientContextHeaders := map[string]string{
		"x-cursor-client-type":        "ide",
		"x-cursor-client-layout":      "unifiedAgent",
		"x-cursor-client-os":          "linux",
		"x-cursor-client-arch":        "x64",
		"x-cursor-client-device-type": "desktop",
		"x-ghost-mode":                "true",
		"x-new-onboarding-completed":  "false",
		"x-cursor-timezone":           "Asia/Shanghai",
		"x-cursor-streaming":          "true",
		"x-jg-net-optimize":           "http2",
		"user-agent":                  "connect-es/1.6.1",
	}

	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	svc, c := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	_, err := executeCursorForTest(t, svc, context.Background(), c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), body, "cursor-sonnet", false, time.Now())
	require.NoError(t, err)
	require.Equal(t, 1, up.count())

	captured := up.at(0).header
	for k, want := range wantClientContextHeaders {
		got := captured.Get(k)
		require.Equal(t, want, got,
			"出站头必须逐字含探针客户端上下文头 %q（工具请求形态对齐，缺则带 tools 请求挂起）", k)
	}

	// 既有 6 项的值不得被改动（鉴权注入 / 协议头恒定）。
	require.Equal(t, "application/connect+proto", captured.Get("Content-Type"))
	require.Equal(t, "1", captured.Get("Connect-Protocol-Version"))
	require.Equal(t, "Bearer BEARER-ACCOUNT-A", captured.Get("Authorization"))
	require.Equal(t, cursorClientVersion, captured.Get("x-cursor-client-version"))
	require.NotEmpty(t, captured.Get("x-cursor-checksum"), "x-cursor-checksum 必须由设备级材料派生")
	require.NotEmpty(t, captured.Get("x-jg-auth"), "x-jg-auth 必须仍由鉴权注入")
}
