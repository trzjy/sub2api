package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// —— 测试基础设施 ——

const cockpitTestMasterSecret = "test-master-secret-required-minimum-length-32b+"

// fakeCockpitCommitRepo 是 CockpitImportCommitRepository 的内存假实现（handler 测试用）。
type fakeCockpitCommitRepo struct {
	lastPayloads []service.CockpitImportAccountPayload
	lastResult   service.CockpitImportCommitResult
	lastErr      error
	calls        int
}

func (f *fakeCockpitCommitRepo) CommitCockpitImport(_ context.Context, payloads []service.CockpitImportAccountPayload) (service.CockpitImportCommitResult, error) {
	f.calls++
	f.lastPayloads = payloads
	return f.lastResult, f.lastErr
}

// captureAuditRepository 内存审计仓储，收集审计事件以便断言（零哨兵 / 动作名）。
type captureAuditRepository struct {
	mu   sync.Mutex
	logs []*service.AuditLog
}

func (r *captureAuditRepository) BatchInsert(_ context.Context, logs []*service.AuditLog) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, logs...)
	return int64(len(logs)), nil
}
func (r *captureAuditRepository) Insert(_ context.Context, log *service.AuditLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, log)
	return nil
}
func (r *captureAuditRepository) List(context.Context, *service.AuditLogFilter) (*service.AuditLogList, error) {
	return &service.AuditLogList{}, nil
}
func (r *captureAuditRepository) GetByID(context.Context, int64) (*service.AuditLog, error) {
	return nil, service.ErrAuditLogNotFound
}
func (r *captureAuditRepository) Count(context.Context) (int64, error) { return 0, nil }
func (r *captureAuditRepository) TruncateAll(context.Context) error    { return nil }
func (r *captureAuditRepository) DeleteBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

// captureLogHandler 收集 slog 记录消息，用于断言服务日志中零哨兵。
type captureLogHandler struct {
	mu       sync.Mutex
	messages []string
}

func (h *captureLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, r.Message)
	r.Attrs(func(a slog.Attr) bool {
		h.messages = append(h.messages, a.Value.String())
		return true
	})
	return nil
}
func (h *captureLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureLogHandler) WithGroup(string) slog.Handler      { return h }
func (h *captureLogHandler) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.messages...)
}

// —— 构造辅助 ——

func cockpitTestSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// cockpitRandomSentinel 运行时生成随机哨兵（§1.7 防止快照式假通过），形如 WBSENTINEL-<hex>。
func cockpitRandomSentinel(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return fmt.Sprintf("WBSENTINEL-%s", hex.EncodeToString(b))
}

// cockpitJSONBody 构造 JSON 形态的请求体（content 为原始上传文本，receipt 可选）。
// 经 json.Marshal 保证嵌套 JSON 文本被安全转义。
func cockpitJSONBody(content, receipt string) string {
	b, _ := json.Marshal(map[string]string{"content": content, "receipt": receipt})
	return string(b)
}

// buildCockpitBackupJSON 构造 cockpit 备份导入 JSON（JSON 形态 content 字段的原始文本）。
// sentinel 放置于 access_token，用于 §1.7 泄漏负向。
func buildCockpitBackupJSON(sentinel, slug, variant string) []byte {
	doc := map[string]any{"schema": "cockpit-tools.data-transfer", "version": 1}
	if variant == "schema_error" {
		doc["schema"] = "wrong-schema"
		b, _ := json.Marshal(doc)
		return b
	}
	var entry map[string]any
	switch variant {
	case "missing_uid":
		entry = map[string]any{"access_token": sentinel, "refresh_token": "r"}
	case "bad_expires":
		entry = map[string]any{"uid": "u1", "access_token": sentinel, "refresh_token": "r", "expires_at": "not-a-number"}
	default: // valid
		entry = map[string]any{"uid": "u1", "access_token": sentinel, "refresh_token": "r", "expires_at": 123}
	}
	doc["accounts"] = map[string]any{
		"platforms": map[string]any{slug: map[string]any{"exported_data": []any{entry}}},
	}
	b, _ := json.Marshal(doc)
	return b
}

// buildCockpitZip 构造 cockpit 导出 ZIP（ZIP 形态），内含 backup.json（含 sentinel）。
func buildCockpitZip(t *testing.T, backupJSON []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("backup.json")
	require.NoError(t, err)
	_, err = w.Write(backupJSON)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// issueReceiptOverRaw 为给定原始字节（JSON 或 ZIP）签发 preview 凭证，供 commit 链路使用。
func issueReceiptOverRaw(t *testing.T, signer *service.CockpitPreviewReceiptSigner, raw []byte) string {
	t.Helper()
	receipt, err := signer.Issue("77", cockpitTestSHA(string(raw)))
	require.NoError(t, err)
	return receipt
}

// newCockpitTestRouter 搭建仅挂载 cockpit preview/commit 的 gin 引擎，含认证注入与审计捕获。
// 真实路由前缀 /api/v1/admin/accounts/cockpit-import 与生产一致，以便审计 FullPath 命中省略清单。
func newCockpitTestRouter(t *testing.T, repo service.CockpitImportCommitRepository, signer *service.CockpitPreviewReceiptSigner) (*gin.Engine, *captureAuditRepository, *service.AuditLogService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	auditRepo := &captureAuditRepository{}
	auditSvc := service.NewAuditLogService(auditRepo, nil)
	auditSvc.Start()
	t.Cleanup(auditSvc.Stop)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Unauth") == "1" {
			c.Next()
			return
		}
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 77})
		c.Set(string(middleware.ContextKeyUserRole), "admin")
		c.Next()
	})
	r.Use(gin.HandlerFunc(middleware.NewAuditLogMiddleware(auditSvc)))

	handler := NewCockpitImportHandler(service.NewCockpitImportCommitServiceWithReceipt(repo, signer), signer)
	admin := r.Group("/api/v1/admin")
	ci := admin.Group("/accounts/cockpit-import")
	ci.POST("/preview", handler.PreviewCockpitImport)
	ci.POST("/commit", handler.CommitCockpitImport)
	return r, auditRepo, auditSvc
}

// —— 测试 ——

// TestCockpitImportPreviewUnauthenticated 未认证 → 401，且（即便带 sentinel 的 body）审计不泄露。
func TestCockpitImportPreviewUnauthenticated(t *testing.T) {
	signer := service.NewCockpitPreviewReceiptSigner(cockpitTestMasterSecret)
	r, _, _ := newCockpitTestRouter(t, &fakeCockpitCommitRepo{}, signer)

	sentinel := cockpitRandomSentinel(t)
	body := cockpitJSONBody(string(buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid")), "")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/cockpit-import/preview", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Unauth", "1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.NotContains(t, rec.Body.String(), sentinel)
}

// TestCockpitImportCommitUnauthenticated 未认证 → 401。
func TestCockpitImportCommitUnauthenticated(t *testing.T) {
	signer := service.NewCockpitPreviewReceiptSigner(cockpitTestMasterSecret)
	r, _, _ := newCockpitTestRouter(t, &fakeCockpitCommitRepo{}, signer)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/cockpit-import/commit", strings.NewReader(cockpitJSONBody("x", "")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Unauth", "1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestCockpitImportPreviewSuccess 认证预览成功：200，响应零凭证材料（无 sentinel），审计体省略。
func TestCockpitImportPreviewSuccess(t *testing.T) {
	signer := service.NewCockpitPreviewReceiptSigner(cockpitTestMasterSecret)
	r, auditRepo, auditSvc := newCockpitTestRouter(t, &fakeCockpitCommitRepo{}, signer)

	sentinel := cockpitRandomSentinel(t)
	raw := buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/cockpit-import/preview", strings.NewReader(cockpitJSONBody(string(raw), "")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), sentinel, "响应体不得含哨兵")

	// 审计事件：动作名含 cockpit 锚点，请求体整体省略，且无哨兵（先停机落盘再读取）。
	auditSvc.Stop()
	var previewLog *service.AuditLog
	for _, l := range auditRepo.logs {
		if l.Path == "/api/v1/admin/accounts/cockpit-import/preview" && l.Method == "POST" {
			previewLog = l
		}
	}
	require.NotNil(t, previewLog)
	require.Contains(t, previewLog.Action, "cockpit")
	require.Contains(t, previewLog.RequestBody, "<credential-bearing body omitted>")
	require.NotContains(t, previewLog.RequestBody, sentinel)
}

// TestCockpitImportCommitNoReceipt 无凭证 → 400 PREVIEW_RECEIPT_INVALID，响应无哨兵。
func TestCockpitImportCommitNoReceipt(t *testing.T) {
	signer := service.NewCockpitPreviewReceiptSigner(cockpitTestMasterSecret)
	r, _, _ := newCockpitTestRouter(t, &fakeCockpitCommitRepo{}, signer)

	sentinel := cockpitRandomSentinel(t)
	raw := buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/cockpit-import/commit", strings.NewReader(cockpitJSONBody(string(raw), "")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), service.ErrCockpitPreviewReceiptInvalid)
	require.NotContains(t, rec.Body.String(), sentinel)
}

// TestCockpitImportCommitManifestMismatch 摘要错配 → 400 MANIFEST_MISMATCH。
func TestCockpitImportCommitManifestMismatch(t *testing.T) {
	signer := service.NewCockpitPreviewReceiptSigner(cockpitTestMasterSecret)
	r, _, _ := newCockpitTestRouter(t, &fakeCockpitCommitRepo{}, signer)

	sentinel := cockpitRandomSentinel(t)
	raw := buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid")
	// 为"其它内容"签发凭证，提交内容不同 → 摘要不一致。
	receipt, err := signer.Issue("77", cockpitTestSHA("other-content"))
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/cockpit-import/commit", strings.NewReader(cockpitJSONBody(string(raw), receipt)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), service.ErrCockpitPreviewManifestMismatch)
	require.NotContains(t, rec.Body.String(), sentinel)
}

// TestCockpitImportCommitVersionChanged 凭证版本不一致 → 400 PREVIEW_RECEIPT_INVALID。
func TestCockpitImportCommitVersionChanged(t *testing.T) {
	signer := service.NewCockpitPreviewReceiptSigner(cockpitTestMasterSecret)
	r, _, _ := newCockpitTestRouter(t, &fakeCockpitCommitRepo{}, signer)

	sentinel := cockpitRandomSentinel(t)
	raw := buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid")
	rawSha := cockpitTestSHA(string(raw))
	bad := &service.CockpitPreviewReceipt{
		Operator: "77", RawSHA256: rawSha,
		ParserVersion: service.CockpitImportParserVersion() + 7,
		IssuedAt:      time.Now().Unix(), ExpiresAt: time.Now().Unix() + 100,
	}
	receipt, err := signer.Sign(bad)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/cockpit-import/commit", strings.NewReader(cockpitJSONBody(string(raw), receipt)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), service.ErrCockpitPreviewReceiptInvalid)
	require.NotContains(t, rec.Body.String(), sentinel)
}

// TestCockpitImportPreviewConcurrencyRejected 并发护栏：槽位占满 → 429 + audit preview_rejected_concurrency。
func TestCockpitImportPreviewConcurrencyRejected(t *testing.T) {
	signer := service.NewCockpitPreviewReceiptSigner(cockpitTestMasterSecret)
	auditRepo := &captureAuditRepository{}
	auditSvc := service.NewAuditLogService(auditRepo, nil)
	auditSvc.Start()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 77})
		c.Set(string(middleware.ContextKeyUserRole), "admin")
		c.Next()
	})
	r.Use(gin.HandlerFunc(middleware.NewAuditLogMiddleware(auditSvc)))

	handler := NewCockpitImportHandler(service.NewCockpitImportCommitServiceWithReceipt(&fakeCockpitCommitRepo{}, signer), signer)
	// 占满所有并发槽位（同包可访问内部 previewSlots）。
	for i := 0; i < cockpitPreviewConcurrencyLimit; i++ {
		handler.previewSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cockpitPreviewConcurrencyLimit; i++ {
			<-handler.previewSlots
		}
	}()
	admin := r.Group("/api/v1/admin")
	ci := admin.Group("/accounts/cockpit-import")
	ci.POST("/preview", handler.PreviewCockpitImport)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/cockpit-import/preview", strings.NewReader(cockpitJSONBody("{}", "")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Contains(t, rec.Body.String(), "RATE_LIMITED")

	auditSvc.Stop()
	var found bool
	for _, l := range auditRepo.logs {
		if l.Action == "preview_rejected_concurrency" {
			found = true
		}
	}
	require.True(t, found, "应产生 preview_rejected_concurrency 审计事件")
}

// TestCockpitImportSentinelLeakJsonAndZip §1.7 哨兵泄漏负向（handler 层）：JSON & ZIP 形式 ×
// preview/commit 全链路，断言 HTTP 响应体 + 审计事件输出 + 服务日志中哨兵零出现。
func TestCockpitImportSentinelLeakJsonAndZip(t *testing.T) {
	signer := service.NewCockpitPreviewReceiptSigner(cockpitTestMasterSecret)
	r, auditRepo, auditSvc := newCockpitTestRouter(t, &fakeCockpitCommitRepo{}, signer)

	// 捕获 slog，验证服务日志零哨兵。
	logCap := &captureLogHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(logCap))
	defer slog.SetDefault(oldLogger)

	sentinel := cockpitRandomSentinel(t)

	// —— JSON 形式：合法 preview/commit 全链路 ——
	rawJSON := buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid")
	rec := serveJSON(t, r, "/api/v1/admin/accounts/cockpit-import/preview", cockpitJSONBody(string(rawJSON), ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), sentinel)

	var previewResp struct {
		Data struct {
			Receipt string `json:"receipt"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &previewResp))
	require.NotEmpty(t, previewResp.Data.Receipt)

	rec = serveJSON(t, r, "/api/v1/admin/accounts/cockpit-import/commit", cockpitJSONBody(string(rawJSON), previewResp.Data.Receipt))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), sentinel)

	// —— JSON 形式：schema_error 失败关闭，错误响应 / 审计零哨兵 ——
	rawErr := buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "schema_error")
	rec = serveJSON(t, r, "/api/v1/admin/accounts/cockpit-import/preview", cockpitJSONBody(string(rawErr), ""))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.NotContains(t, rec.Body.String(), sentinel)

	// —— ZIP 形式：合法 preview/commit 全链路（multipart 文件上传）——
	rawZip := buildCockpitZip(t, buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid"))
	rec = serveMultipartZip(t, r, "/api/v1/admin/accounts/cockpit-import/preview", "", rawZip)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), sentinel)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &previewResp))
	require.NotEmpty(t, previewResp.Data.Receipt)

	rec = serveMultipartZip(t, r, "/api/v1/admin/accounts/cockpit-import/commit", previewResp.Data.Receipt, rawZip)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), sentinel)

	// 停机落盘所有异步审计条目后再读取（避免竞态）。
	auditSvc.Stop()

	// —— 审计事件输出：遍历所有 cockpit 审计条目，零哨兵且 body 省略 ——
	for _, l := range auditRepo.logs {
		if strings.Contains(l.Path, "cockpit-import") {
			require.NotContains(t, l.RequestBody, sentinel, "审计请求体不得含哨兵: %s", l.Path)
			require.NotContains(t, l.Action, sentinel)
			if l.Extra != nil {
				for k, v := range l.Extra {
					require.NotContains(t, v, sentinel, "审计 Extra[%s] 不得含哨兵", k)
				}
			}
		}
	}

	// —— 服务日志：零哨兵 ——
	for _, msg := range logCap.all() {
		require.NotContains(t, msg, sentinel, "服务日志不得含哨兵")
	}
}

// TestCockpitImportUploadSizeLimit 上传体积硬上限（方案 §1.4 ≤ 10 MB，失败关闭）：
// 两分支超限 → 400 PAYLOAD_STRUCTURE_LIMIT（响应不含上传内容）；边界内小请求不误伤。
func TestCockpitImportUploadSizeLimit(t *testing.T) {
	signer := service.NewCockpitPreviewReceiptSigner(cockpitTestMasterSecret)
	r, _, _ := newCockpitTestRouter(t, &fakeCockpitCommitRepo{}, signer)

	// —— JSON 分支：超限（receipt 字段垫大体积，请求总长 > 上限）→ 400，响应零内容回显 ——
	overBody := cockpitJSONBody("{}", strings.Repeat("p", 10<<20+128))
	require.Greater(t, len(overBody), cockpitImportMaxUploadBytes+1)
	rec := serveJSON(t, r, "/api/v1/admin/accounts/cockpit-import/preview", overBody)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), service.ErrCockpitPayloadStructureLimit)
	require.NotContains(t, rec.Body.String(), strings.Repeat("p", 64))

	// —— JSON 分支：上限内（receipt 垫至 ~9MB，content 为合法小备份）不误伤 → 200 ——
	sentinel := cockpitRandomSentinel(t)
	raw := buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid")
	underBody := cockpitJSONBody(string(raw), strings.Repeat("p", 9<<20))
	require.Less(t, len(underBody), cockpitImportMaxUploadBytes)
	rec = serveJSON(t, r, "/api/v1/admin/accounts/cockpit-import/preview", underBody)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), sentinel)

	// —— multipart 分支：>10MB ZIP（随机字节不可压，Deflate 后仍超限）→ 400 ——
	big := make([]byte, 11<<20)
	_, err := rand.Read(big)
	require.NoError(t, err)
	rec = serveMultipartZip(t, r, "/api/v1/admin/accounts/cockpit-import/preview", "", big)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), service.ErrCockpitPayloadStructureLimit)

	// —— multipart 分支：小 ZIP 不误伤 → 200 ——
	smallZip := buildCockpitZip(t, buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid"))
	rec = serveMultipartZip(t, r, "/api/v1/admin/accounts/cockpit-import/preview", "", smallZip)
	require.Equal(t, http.StatusOK, rec.Code)
}

// serveJSON 以 JSON 形态发送请求并返回 recorder。
func serveJSON(t *testing.T, r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// serveMultipartZip 以 multipart 文件形态发送 ZIP 请求（含可选 receipt 字段）并返回 recorder。
func serveMultipartZip(t *testing.T, r *gin.Engine, path, receipt string, rawZip []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "backup.zip")
	require.NoError(t, err)
	_, err = part.Write(rawZip)
	require.NoError(t, err)
	if receipt != "" {
		require.NoError(t, mw.WriteField("receipt", receipt))
	}
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}
