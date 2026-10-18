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
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	wasmcrypto "github.com/Wei-Shaw/sub2api/internal/service/wasm"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
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

	ts := httptest.NewServer(upstream)
	t.Cleanup(ts.Close)

	nodes := NewCursorUpstreamNodesResolver(WithCursorNodesCacheTTL(time.Hour))
	nodes.cached = CursorUpstreamNodes{HTTP1Base: ts.URL, HTTP2Base: ts.URL}
	nodes.cachedAt = time.Now()

	svc := NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	svc.SetCursorAuthDeps(repo, NewCursorTestEncryptor(), deviceIDs)
	svc.SetCursorForwarder(NewCursorUpstreamForwarder(
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
	out = append(out, cursorStreamStartFrame(t)...)
	out = append(out, cursorTextFrame(t, "Hello")...)
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

	resA, err := svc.forwardCursor(ctx, c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), body, "cursor-sonnet", false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, resA)
	resB, err := svc.forwardCursor(ctx, c, cursorOutboundAccount(2, 22, "BEARER-ACCOUNT-B"), body, "cursor-sonnet", false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, resB)

	// 旧链已删：出站只应有对话调用，不得再打任何 AuthService 端点。
	require.Equal(t, []string{
		cursorPathStreamUnifiedChatWithTools,
		cursorPathStreamUnifiedChatWithTools,
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
	_, err = svc.forwardCursor(ctx, c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), body, "cursor-sonnet", false, time.Now())
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

	_, err := svc.forwardCursor(context.Background(), c, acc, body, "cursor-sonnet", false, time.Now())
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
// Done when 4：流内拒绝 → lookup 核对链
// ---------------------------------------------------------------------------

// cursorStubKeyStatusLookup 是激活码终态核对的测试替身。
type cursorStubKeyStatusLookup struct {
	status cursorKeyStatus
	err    error
	calls  int
}

func (l *cursorStubKeyStatusLookup) LookupKeyStatus(context.Context, int64) (cursorKeyStatus, error) {
	l.calls++
	return l.status, l.err
}

// cursorRejectedStreamBody 构造一条 HTTP200 但流内含终态拒绝模板的响应。
//
// 模板后跟动态后缀标识（证据 125 / 134 样本：` (1792327810746-28d0be39)`），
// 判据扫描按前缀匹配即可命中，后缀原样进结构化字段。
func cursorRejectedStreamBody(t *testing.T) []byte {
	t.Helper()
	var out []byte
	out = append(out, cursorStreamStartFrame(t)...)
	out = append(out, cursorTextFrame(t, "密钥已过期或额度耗尽，请续费后使用 (1792327810746-28d0be39)")...)
	out = append(out, cursorTestEndStream(t, `{}`)...)
	return out
}

// newCursorKeyRejectedTestService 装配"流内拒绝"场景所需的服务与真实 DB（sqlite）。
//
// 用真实 DB 的理由：确认过期分支的写链（cursorMarkKeyExpiredByUpstream →
// QuotaKeyService.markKeyExpiredByUpstream）走 ent 条件更新，桩掉就测不到
// "仅当 status=active 才写"这条口径。
func newCursorKeyRejectedTestService(
	t *testing.T,
	upstream http.Handler,
	lookup cursorKeyStatusLookup,
) (*OpenAIGatewayService, *gin.Context, *ent.Client, int64) {
	t.Helper()
	ctx := context.Background()

	db, err := sql.Open("sqlite", "file:cursor_key_rejected?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(ent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })

	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:test").
		SetStatus(domain.QuotaKeyStatusActive).
		Save(ctx)
	require.NoError(t, err)
	_, err = client.Account.Create().
		SetName("cursor-acc-rejected").
		SetPlatform(PlatformCursor).
		SetType("oauth").
		SetCredentials(map[string]any{"access_token": "BEARER-ACCOUNT-A"}).
		SetQuotaKeyID(qk.ID).
		Save(ctx)
	require.NoError(t, err)

	svc, c := newCursorOutboundTestService(t, upstream, nil, nil)
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		qk.ID: {ID: qk.ID, CodeEncrypted: "ACTIVATION-CODE-REJECTED"},
	})
	svc.SetCursorAuthDeps(repo, NewCursorTestEncryptor(),
		NewQuotaKeyService(repo, NewCursorTestEncryptor(), nil, client))
	svc.SetCursorKeyStatusLookup(lookup)
	return svc, c, client, qk.ID
}

// TestCursorStreamRejectionIsTerminalNeverSuccess 钉死流内拒绝不得当成功、不得产出 usage。
//
// 判据：HTTP200 正常流帧中出现固定模板「密钥已过期或额度耗尽，请续费后使用」。
// 命中即终态错误——本用例同时反证"不得伪造 usage"（返回 res 必为 nil）。
func TestCursorStreamRejectionIsTerminalNeverSuccess(t *testing.T) {
	up := &cursorOutboundUpstream{body: cursorRejectedStreamBody(t)}
	svc, c, client, qkID := newCursorKeyRejectedTestService(t, up.handler(), &cursorStubKeyStatusLookup{})

	acc := cursorOutboundAccount(1, qkID, "BEARER-ACCOUNT-A")
	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

	res, err := svc.forwardCursor(context.Background(), c, acc, body, "cursor-sonnet", false, time.Now())
	require.Error(t, err)
	require.Nil(t, res, "流内拒绝绝不能返回可计费结果（不得当成功、不得伪造 usage）")
	require.ErrorIs(t, err, ErrCursorKeyRejectedTerminal)
	require.Contains(t, err.Error(), cursorKeyRejectedTemplate)

	// 未注入确认入口（替身返回零值 status=unknown）时不得写 key 状态。
	row, gerr := client.QuotaKey.Get(context.Background(), qkID)
	require.NoError(t, gerr)
	require.NotEqual(t, domain.QuotaKeyStatusExpired, row.Status,
		"未经 lookup 确认绝不得标过期")

	// 模板识别本身：结构化片段含动态后缀标识，用于与上游侧记录对齐。
	rej, ok := cursorDetectKeyRejectedUpstream(cursorRejectedStreamBody(t))
	require.True(t, ok)
	require.Equal(t, "1792327810746-28d0be39", rej.TurnID)
	require.Contains(t, rej.Snippet, cursorKeyRejectedTemplate)
}

// TestCursorStreamRejectionConfirmedExpiredMarksKeyExpired 覆盖"lookup 确认过期"分支：
// 走既有 key 状态写入口把激活码标 expired，供 S 卡消费调度资格。
//
// 同时钉死两条红线：本次请求仍是终态失败（不因标过期就变成成功）；绝不软删账号。
func TestCursorStreamRejectionConfirmedExpiredMarksKeyExpired(t *testing.T) {
	up := &cursorOutboundUpstream{body: cursorRejectedStreamBody(t)}
	lookup := &cursorStubKeyStatusLookup{status: cursorKeyStatusExpired}
	svc, c, client, qkID := newCursorKeyRejectedTestService(t, up.handler(), lookup)
	ctx := context.Background()

	acc := cursorOutboundAccount(1, qkID, "BEARER-ACCOUNT-A")
	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

	res, err := svc.forwardCursor(ctx, c, acc, body, "cursor-sonnet", false, time.Now())
	require.Error(t, err)
	require.Nil(t, res, "确认过期后本次请求仍是终态失败，不得转成成功")
	require.ErrorIs(t, err, ErrCursorKeyRejectedTerminal)
	require.Equal(t, 1, lookup.calls, "流内拒绝必须走 lookup 核对")

	row, gerr := client.QuotaKey.Get(ctx, qkID)
	require.NoError(t, gerr)
	require.Equal(t, domain.QuotaKeyStatusExpired, row.Status,
		"lookup 确认「密钥已过期」→ 走既有写口标 expired")
	require.False(t, row.Exhausted, "标过期不得顺带改 exhausted 维度（与 sweeper 同口径）")

	// I2 红线：绝不软删账号、绝不标账号耗尽。
	accounts, aerr := client.Account.Query().Where().All(ctx)
	require.NoError(t, aerr)
	require.Len(t, accounts, 1)
	require.Nil(t, accounts[0].DeletedAt, "绝不软删账号")
}

// TestCursorStreamRejectionUnconfirmedLeavesKeyAndAccountUntouched 覆盖"无法确认"分支：
// lookup 报错 / 回包语义不在已取证范围 / 确认码仍在使用 → 一律按上游失败返回，
// 不删号、不标账号耗尽、不标 key 过期。
func TestCursorStreamRejectionUnconfirmedLeavesKeyAndAccountUntouched(t *testing.T) {
	for name, lookup := range map[string]*cursorStubKeyStatusLookup{
		"lookup_error": {status: cursorKeyStatusUnknown, err: context.DeadlineExceeded},
		"key_active":   {status: cursorKeyStatusActive},
	} {
		t.Run(name, func(t *testing.T) {
			up := &cursorOutboundUpstream{body: cursorRejectedStreamBody(t)}
			svc, c, client, qkID := newCursorKeyRejectedTestService(t, up.handler(), lookup)
			ctx := context.Background()

			acc := cursorOutboundAccount(1, qkID, "BEARER-ACCOUNT-A")
			body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

			res, ferr := svc.forwardCursor(ctx, c, acc, body, "cursor-sonnet", false, time.Now())
			require.Error(t, ferr)
			require.Nil(t, res, "无法确认时按上游失败返回，绝不当成功")
			require.ErrorIs(t, ferr, ErrCursorKeyRejectedTerminal)

			row, gerr := client.QuotaKey.Get(ctx, qkID)
			require.NoError(t, gerr)
			require.Equal(t, domain.QuotaKeyStatusActive, row.Status,
				"lookup 无法确认过期 → 绝不标 key 过期")

			accounts, aerr := client.Account.Query().Where().All(ctx)
			require.NoError(t, aerr)
			require.Len(t, accounts, 1)
			require.Nil(t, accounts[0].DeletedAt, "绝不软删账号")
		})
	}
}

// TestCursorBridgeInboundOpenAIChatDispatchesToForwarder 钉死契约 0.1 的包级入口：
// 未安装实例时失败关闭；入站形状不受理时失败关闭；受理后头与出站一致。
func TestCursorBridgeInboundOpenAIChatDispatchesToForwarder(t *testing.T) {
	prev := cursorBridgeInstalled()
	t.Cleanup(func() { InstallCursorBridge(prev) })

	// 未安装 → 失败关闭（绝不静默降级到通用 OpenAI 路径）。
	InstallCursorBridge(nil)
	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	_, err := CursorForwardInbound(context.Background(), nil, acc, cursorInboundOpenAIChat,
		[]byte(`{"model":"cursor-sonnet","messages":[{"role":"user","content":"hi"}]}`))
	require.ErrorIs(t, err, ErrCursorBridgeNotInstalled)

	// 形状不受理 → 失败关闭（不让未转换的形状直达 Cursor 原生端点）。
	_, err = CursorForwardInbound(context.Background(), nil, acc, "openai_responses", []byte(`{}`))
	require.ErrorIs(t, err, ErrCursorBridgeInboundUnsupported)

	// 受理：走真实出站，头与 forwardCursor 完全一致（不二次计费，usage 由调用方记）。
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	svc, c := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))
	InstallCursorBridge(svc)

	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	res, err := CursorForwardInbound(context.Background(), c, acc, cursorInboundOpenAIChat, body)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, 1, up.count())
	require.Equal(t, "Bearer BEARER-ACCOUNT-A", up.at(0).header.Get("Authorization"))
	require.NotEmpty(t, up.at(0).header.Get("x-jg-auth"))
	require.Equal(t, cursorPathStreamUnifiedChatWithTools, up.at(0).path)

	// 桥接只做字段搬运：usage 逐字段直搬，绝不折算、绝不补默认值。
	out := cursorForwardResultToOpenAI(res)
	require.Equal(t, res.Usage.InputTokens, out.Usage.InputTokens)
	require.Equal(t, res.Usage.OutputTokens, out.Usage.OutputTokens)
	require.Equal(t, strings.TrimSpace(res.RequestID), out.ResponseID)
}
