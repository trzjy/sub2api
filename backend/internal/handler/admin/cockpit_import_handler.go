// cockpit_import_handler.go —— cockpit 备份导入 preview/commit HTTP 端点（方案 §1.4-1.8，B1c）。
//
// 设计要点（contract §5：生产代码零兜底、失败关闭）：
//   - 仅新增端点；权限/限流/审计复用既有 admin 边界（admin_auth）、PanelRateLimiter（preview 挂 Heavy）、
//     AuditLogMiddleware（admin 路由组已 Use）。不新造权限/限流/指标机制。
//   - preview/commit 双端点统一挂在既有 admin 认证边界之后：未认证 → 401 拒。
//   - 响应与审计零凭证材料：仅计数、slug、uid 摘要（raw_sha256 是 §1.5 明文允许的摘要）与签名凭证 token
//     （token 仅含 operator/raw_sha256/parser_version/iat/exp，不含任何上传内容）。

package admin

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// cockpitPreviewConcurrencyLimit 是 preview 端点进程内并发上限（资源护栏，非按用户限流框架）。
// 取值依据：单次 preview 最坏解析 60MB ZIP / 50MB JSON，CPU 密集；上限 8 并发把最坏总内存占用
// 限制在 ~480MB+解析临时缓冲内，避免单实例被大体积导入打满；超出 → 429 + 审计事件
// preview_rejected_concurrency（对应 §1.5 钉死事件名）。
const cockpitPreviewConcurrencyLimit = 8

// cockpitImportMaxUploadBytes 是 cockpit 备份导入上传体积硬上限（方案 §1.4 "上传 ≤ 10 MB"）。
// 请求体解析前统一经 http.MaxBytesReader 强制生效（失败关闭）：multipart 与 JSON 两分支
// 超限一律 400 PAYLOAD_STRUCTURE_LIMIT，且响应不含任何上传内容。与解析器内
// maxBackupJSONBytes(50MB) 并行：前者封传输字节，后者封解压后结构上限。
const cockpitImportMaxUploadBytes = 10 << 20

// CockpitImportHandler 暴露 cockpit 备份导入的 preview/commit 端点（B1c）。
type CockpitImportHandler struct {
	commitSvc    *service.CockpitImportCommitService
	receiptSigner *service.CockpitPreviewReceiptSigner
	previewSlots chan struct{}
}

// NewCockpitImportHandler 构造 cockpit 导入 handler。
func NewCockpitImportHandler(commitSvc *service.CockpitImportCommitService, signer *service.CockpitPreviewReceiptSigner) *CockpitImportHandler {
	return &CockpitImportHandler{
		commitSvc:     commitSvc,
		receiptSigner: signer,
		previewSlots:  make(chan struct{}, cockpitPreviewConcurrencyLimit),
	}
}

// cockpitImportOperator 从 gin 上下文提取操作者主体（admin user id 字符串）。
// 认证边界由既有 AdminAuthMiddleware 保证；此处仅做防御性校验，缺失 → false（调用方 401 拒）。
func cockpitImportOperator(c *gin.Context) (string, bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		return "", false
	}
	return strconv.FormatInt(subject.UserID, 10), true
}

// readCockpitImportUpload 从请求读取导入原始字节（multipart "file" 或 JSON 体 "content"）。
// 返回 raw 字节与 receipt（commit 用）。两者均优先 multipart 表单字段，回退 raw JSON 体。
// 上传体积硬上限（方案 §1.4 ≤ 10 MB）在请求体解析前统一生效：入口处包 MaxBytesReader，
// multipart 解析（FormFile 触发 ParseMultipartForm）与 JSON bind 均从被限 body 读取，
// 超限以 *http.MaxBytesError 浮出 → 400 PAYLOAD_STRUCTURE_LIMIT（失败关闭，响应不含上传内容）。
// JSON bind 成功后显式消费尾部：解码器只读到首个 JSON 值为止，不消费尾部则上限可被
// "短 JSON + 大尾随"绕过；残留非空（含尾随读出越限）同样按结构违规拒绝。
// multipart 形态下审计中间件按非 JSON 整段省略请求体，天然规避凭证泄漏（§1.7）。
func readCockpitImportUpload(c *gin.Context) (raw []byte, receipt string, err error) {
	limitExceeded := func(e error) bool {
		var maxErr *http.MaxBytesError
		return errors.As(e, &maxErr)
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, cockpitImportMaxUploadBytes)
	if strings.Contains(strings.ToLower(c.ContentType()), "multipart/form-data") {
		file, ferr := c.FormFile("file")
		if ferr != nil {
			if limitExceeded(ferr) {
				return nil, "", &service.CockpitImportError{Code: service.ErrCockpitPayloadStructureLimit}
			}
			return nil, "", ferr
		}
		f, oerr := file.Open()
		if oerr != nil {
			return nil, "", oerr
		}
		defer f.Close()
		data, rerr := io.ReadAll(f)
		if rerr != nil {
			return nil, "", rerr
		}
		return data, c.PostForm("receipt"), nil
	}
	// 回退：JSON 体 {"receipt": "...", "content": "<raw json/zip 文本>"}（content 为原始文本，非 base64）。
	var body struct {
		Receipt string `json:"receipt"`
		Content string `json:"content"`
	}
	if berr := c.ShouldBindJSON(&body); berr != nil {
		if limitExceeded(berr) {
			return nil, "", &service.CockpitImportError{Code: service.ErrCockpitPayloadStructureLimit}
		}
		return nil, "", berr
	}
	n, derr := io.Copy(io.Discard, c.Request.Body)
	if limitExceeded(derr) || n > 0 {
		return nil, "", &service.CockpitImportError{Code: service.ErrCockpitPayloadStructureLimit}
	}
	if derr != nil {
		return nil, "", derr
	}
	return []byte(body.Content), body.Receipt, nil
}

// writeCockpitErr 将 cockpit 失败关闭错误映射为 HTTP 响应。
// *CockpitImportError → 400，reason=错误码；其余 → 走既有 ApplicationError 映射。
func (h *CockpitImportHandler) writeCockpitErr(c *gin.Context, err error) {
	var ce *service.CockpitImportError
	if errors.As(err, &ce) {
		response.ErrorWithDetails(c, http.StatusBadRequest, ce.Code, ce.Code, nil)
		return
	}
	response.ErrorFrom(c, err)
}

// PreviewCockpitImport 解析上传字节流并返回计数摘要 + 签名凭证；零副作用（不写库不落盘）。
//
//	POST /api/v1/admin/accounts/cockpit-import/preview
//
// 未认证 → 401；并发超限 → 审计事件 preview_rejected_concurrency + 429；解析超时 → 审计事件
// preview_parse_timeout + 400；其它解析失败 → 400（错误码透出）。
func (h *CockpitImportHandler) PreviewCockpitImport(c *gin.Context) {
	operator, ok := cockpitImportOperator(c)
	if !ok {
		response.Unauthorized(c, "UNAUTHORIZED")
		return
	}
	// 并发护栏：拿不到槽位 → 审计事件 preview_rejected_concurrency + 429 失败关闭。
	select {
	case h.previewSlots <- struct{}{}:
		defer func() { <-h.previewSlots }()
	default:
		middleware.SetAuditAction(c, "preview_rejected_concurrency")
		response.Error(c, http.StatusTooManyRequests, "RATE_LIMITED")
		return
	}

	raw, _, err := readCockpitImportUpload(c)
	if err != nil {
		response.BadRequest(c, "failed to read upload: "+err.Error())
		return
	}

	result, err := h.commitSvc.PreviewCockpitImport(c.Request.Context(), operator, raw)
	if err != nil {
		if service.IsCockpitErrorCode(err, service.ErrCockpitPreviewParseTimeout) {
			middleware.SetAuditAction(c, "preview_parse_timeout")
		}
		h.writeCockpitErr(c, err)
		return
	}
	response.Success(c, result)
}

// CommitCockpitImport 校验凭证 → 重算 SHA 比对 → 同版本解析 → 调既有 B1b 写入器。
//
//	POST /api/v1/admin/accounts/cockpit-import/commit
//
// 未认证 → 401；无凭证/无效凭证 → 400 PREVIEW_RECEIPT_INVALID（失败关闭）；
// 字节流与凭证内 raw_sha256 不一致 → 400 MANIFEST_MISMATCH（失败关闭）。
func (h *CockpitImportHandler) CommitCockpitImport(c *gin.Context) {
	operator, ok := cockpitImportOperator(c)
	if !ok {
		response.Unauthorized(c, "UNAUTHORIZED")
		return
	}
	raw, receipt, err := readCockpitImportUpload(c)
	if err != nil {
		response.BadRequest(c, "failed to read upload: "+err.Error())
		return
	}
	result, err := h.commitSvc.CommitCockpitImport(c.Request.Context(), operator, receipt, raw)
	if err != nil {
		h.writeCockpitErr(c, err)
		return
	}
	response.Success(c, result)
}
