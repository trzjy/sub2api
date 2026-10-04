package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

func logOpenAIInstructionsRequiredDebug(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	upstreamStatusCode int,
	upstreamMsg string,
	requestBody []byte,
	upstreamBody []byte,
) {
	msg := strings.TrimSpace(upstreamMsg)
	if !isOpenAIInstructionsRequiredError(upstreamStatusCode, msg, upstreamBody) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	accountID := int64(0)
	accountName := ""
	if account != nil {
		accountID = account.ID
		accountName = strings.TrimSpace(account.Name)
	}

	userAgent := ""
	originator := ""
	if c != nil {
		userAgent = strings.TrimSpace(c.GetHeader("User-Agent"))
		originator = strings.TrimSpace(c.GetHeader("originator"))
	}

	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.String("account_name", accountName),
		zap.Int("upstream_status_code", upstreamStatusCode),
		zap.String("upstream_error_message", msg),
		zap.String("request_user_agent", userAgent),
		zap.Bool("codex_official_client_match", openai.IsCodexOfficialClientByHeaders(userAgent, originator)),
	}
	fields = appendCodexCLIOnlyRejectedRequestFields(fields, c, requestBody)

	logger.FromContext(ctx).With(fields...).Warn("OpenAI 上游返回 Instructions are required，已记录请求详情用于排查")
}

func isOpenAIInstructionsRequiredError(upstreamStatusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if upstreamStatusCode != http.StatusBadRequest {
		return false
	}

	hasInstructionRequired := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "instructions are required") {
			return true
		}
		if strings.Contains(lower, "required parameter: 'instructions'") {
			return true
		}
		if strings.Contains(lower, "required parameter: instructions") {
			return true
		}
		if strings.Contains(lower, "missing required parameter") && strings.Contains(lower, "instructions") {
			return true
		}
		return strings.Contains(lower, "instruction") && strings.Contains(lower, "required")
	}

	if hasInstructionRequired(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}

	errMsg := gjson.GetBytes(upstreamBody, "error.message").String()
	errMsgLower := strings.ToLower(strings.TrimSpace(errMsg))
	errCode := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.code").String()))
	errParam := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.param").String()))
	errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.type").String()))

	if errParam == "instructions" {
		return true
	}
	if hasInstructionRequired(errMsg) {
		return true
	}
	if strings.Contains(errCode, "missing_required_parameter") && strings.Contains(errMsgLower, "instructions") {
		return true
	}
	if strings.Contains(errType, "invalid_request") && strings.Contains(errMsgLower, "instructions") && strings.Contains(errMsgLower, "required") {
		return true
	}

	return false
}

func isOpenAITransientProcessingError(upstreamStatusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if upstreamStatusCode < http.StatusBadRequest {
		return false
	}

	hasOpenAIServerOverloadedCode := func(payload []byte) bool {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.code").String()))
		if code == "" {
			code = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String()))
		}
		return code == "server_is_overloaded" || code == "slow_down"
	}

	if len(upstreamBody) > 0 && hasOpenAIServerOverloadedCode(upstreamBody) {
		return true
	}
	if isOpenAICapacityShedMessage(upstreamMsg) ||
		isOpenAICapacityShedMessage(gjson.GetBytes(upstreamBody, "error.message").String()) ||
		isOpenAICapacityShedMessage(gjson.GetBytes(upstreamBody, "response.error.message").String()) ||
		(!gjson.ValidBytes(upstreamBody) && isOpenAICapacityShedMessage(string(upstreamBody))) {
		return true
	}
	if upstreamStatusCode != http.StatusBadRequest && upstreamStatusCode != http.StatusServiceUnavailable {
		return false
	}
	if upstreamStatusCode != http.StatusBadRequest {
		return false
	}

	match := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "an error occurred while processing your request") {
			return true
		}
		if strings.Contains(lower, "selected model is at capacity") {
			return true
		}
		return strings.Contains(lower, "you can retry your request") &&
			strings.Contains(lower, "help.openai.com") &&
			strings.Contains(lower, "request id")
	}

	if match(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}
	if match(gjson.GetBytes(upstreamBody, "error.message").String()) {
		return true
	}
	if match(gjson.GetBytes(upstreamBody, "response.error.message").String()) ||
		match(gjson.GetBytes(upstreamBody, "message").String()) {
		return true
	}
	// A valid JSON error may echo arbitrary request content. Only its explicit
	// error fields are authoritative; scan the whole body only for non-JSON
	// providers that return a plain-text error response.
	return !gjson.ValidBytes(upstreamBody) && match(string(upstreamBody))
}

func isOpenAICapacityShedMessage(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "server is overloaded") ||
		strings.Contains(lower, "servers are overloaded") ||
		strings.Contains(lower, "servers are currently overloaded")
}

func isOpenAIRequestScopedCapacityShed(upstreamMsg string, upstreamBody []byte) bool {
	return isOpenAIUpstreamCapacityShedEvent(upstreamBody) ||
		isOpenAICapacityShedMessage(upstreamMsg) ||
		(!gjson.ValidBytes(upstreamBody) && isOpenAICapacityShedMessage(string(upstreamBody)))
}

func isOpenAIContextWindowError(upstreamMsg string, upstreamBody []byte) bool {
	match := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "context_too_large") || strings.Contains(lower, "context_length_exceeded") {
			return true
		}
		if strings.Contains(lower, "maximum context length") || strings.Contains(lower, "max context length") {
			return true
		}
		hasExceeded := strings.Contains(lower, "exceed") || strings.Contains(lower, "too large") || strings.Contains(lower, "too long")
		if strings.Contains(lower, "context window") && hasExceeded {
			return true
		}
		if strings.Contains(lower, "context length") && hasExceeded {
			return true
		}
		return strings.Contains(lower, "token limit") &&
			strings.Contains(lower, "context") &&
			hasExceeded
	}

	if match(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}
	for _, path := range []string{
		"error.message",
		"response.error.message",
		"message",
		"error.code",
		"response.error.code",
		"code",
	} {
		if match(gjson.GetBytes(upstreamBody, path).String()) {
			return true
		}
	}
	// Do not let echoed request content in a structured JSON error change the
	// retry/client-status classification. Plain-text upstream errors remain
	// supported by scanning the whole body only when it is not valid JSON.
	return !gjson.ValidBytes(upstreamBody) && match(string(upstreamBody))
}

func (s *OpenAIGatewayService) shouldFailoverUpstreamError(statusCode int) bool {
	switch statusCode {
	case 401, 402, 403, 405, 429, 529:
		return true
	default:
		return statusCode >= 500
	}
}

func (s *OpenAIGatewayService) shouldFailoverOpenAIUpstreamResponse(account *Account, statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	// cyber_policy is request-scoped even when an intermediary wraps the
	// provider response in a retryable 5xx status. Never punish or rotate the
	// selected credential for it.
	if hit, _, _ := detectOpenAICyberPolicy(upstreamBody); hit {
		return false
	}
	if isOpenAIContextWindowError(upstreamMsg, upstreamBody) {
		return false
	}
	if isOpenAIHTTPUpstreamAccessStateError(statusCode, upstreamMsg, upstreamBody) {
		return true
	}
	if isOpenAIRequestBodyTooLargeError(statusCode, upstreamMsg, upstreamBody) {
		return true
	}
	// A missing model is account/provider availability, not a malformed client
	// request. Keep this unconditional exception inside the OpenAI-compatible
	// gateway and require an eligible account so Anthropic/Gemini paths retain
	// their existing opt-in 400 behavior.
	// A bare forwarding service has no account-selection owner to consume a
	// failover sentinel. In that mode (used by direct/single-account callers),
	// preserve the deterministic upstream 400 instead of returning an unwritten
	// retry signal. Managed gateway instances always have an account repository;
	// their handler can exclude this account and actually select another one.
	if s != nil && s.accountRepo != nil && account != nil && account.IsOpenAICompatible() && statusCode == http.StatusBadRequest &&
		isOpenAICompatibleModelNotFound400(upstreamBody) {
		return true
	}
	if s.shouldFailoverUpstreamError(statusCode) {
		return true
	}
	return isOpenAITransientProcessingError(statusCode, upstreamMsg, upstreamBody)
}

func isOpenAICompatibleModelNotFound400(respBody []byte) bool {
	code := strings.TrimSpace(extractUpstreamErrorCode(respBody))
	if code != "" {
		return strings.EqualFold(code, "model_not_found")
	}

	msg := strings.ToLower(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))
	if msg == "" && !gjson.ValidBytes(respBody) {
		msg = strings.ToLower(strings.TrimSpace(string(respBody)))
	}
	return strings.Contains(msg, "unknown provider for model") ||
		strings.Contains(msg, "model not found") ||
		strings.Contains(msg, "model is not supported")
}

// IsOpenAICompatibleModelNotFound400 reports whether an OpenAI-compatible 400
// is an account-specific missing-model response eligible for failover.
func IsOpenAICompatibleModelNotFound400(respBody []byte) bool {
	return isOpenAICompatibleModelNotFound400(respBody)
}

// OpenAIRequestBodyTooLargeClientMessage is the fixed downstream message used
// after all account-specific request body limit failovers are exhausted.
const OpenAIRequestBodyTooLargeClientMessage = "Request payload is too large"

const openAIRequestBodyTooLargeReason = GatewayFailureReason("openai_request_body_too_large")

func isOpenAIRequestBodyTooLargeError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	return statusCode == http.StatusRequestEntityTooLarge && !isOpenAIContextWindowError(upstreamMsg, upstreamBody)
}

func newOpenAIUpstreamFailoverError(
	statusCode int,
	responseHeaders http.Header,
	responseBody []byte,
	upstreamMsg string,
	retryableOnSameAccount bool,
) *UpstreamFailoverError {
	requestScopedCapacity := isOpenAIRequestScopedCapacityShed(upstreamMsg, responseBody)
	failoverErr := &UpstreamFailoverError{
		StatusCode:             statusCode,
		ResponseBody:           responseBody,
		ResponseHeaders:        responseHeaders.Clone(),
		RetryableOnSameAccount: retryableOnSameAccount || requestScopedCapacity,
		RequestScopedTransient: requestScopedCapacity,
	}
	if isOpenAIRequestBodyTooLargeError(statusCode, upstreamMsg, responseBody) {
		failoverErr.RetryableOnSameAccount = false
		failoverErr.RequestScopedTransient = false
		failoverErr.Scope = GatewayFailureScopeAccount
		failoverErr.Reason = openAIRequestBodyTooLargeReason
		failoverErr.NextAccountAction = NextAccountRetry
		failoverErr.ClientStatusCode = http.StatusRequestEntityTooLarge
		failoverErr.ClientMessage = OpenAIRequestBodyTooLargeClientMessage
	}
	if isOpenAIHTTPUpstreamAccessStateError(statusCode, upstreamMsg, responseBody) {
		failoverErr.RetryableOnSameAccount = false
		failoverErr.RequestScopedTransient = false
		failoverErr.Stage = GatewayFailureStageAccountAuth
		failoverErr.Scope = GatewayFailureScopeAccount
		failoverErr.Reason = OpenAIUpstreamAccessStateReason
		failoverErr.NextAccountAction = NextAccountRetry
		failoverErr.ClientStatusCode = http.StatusBadGateway
		failoverErr.ClientMessage = openAIUpstreamAccessUnavailableClientMessage
	} else if requestScopedCapacity {
		// Preserve the provider's actionable overload message after gateway
		// retries are exhausted, but expose it as a retryable server_error.
		failoverErr.ClientStatusCode = http.StatusServiceUnavailable
		failoverErr.ClientMessage = openAICapacityShedClientMessage(upstreamMsg, responseBody)
	}
	return failoverErr
}

func (s *OpenAIGatewayService) newOpenAIAccountFailoverError(
	account *Account,
	statusCode int,
	responseHeaders http.Header,
	responseBody []byte,
	upstreamMsg string,
	shouldDisable bool,
	retryableOnSameAccount bool,
) *UpstreamFailoverError {
	return s.newOpenAIAccountFailoverErrorWithClassificationHeaders(account, statusCode, responseHeaders, responseHeaders, responseBody, upstreamMsg, shouldDisable, retryableOnSameAccount)
}

func (s *OpenAIGatewayService) newOpenAIAccountFailoverErrorWithClassificationHeaders(
	account *Account,
	statusCode int,
	responseHeaders http.Header,
	classificationHeaders http.Header,
	responseBody []byte,
	upstreamMsg string,
	shouldDisable bool,
	retryableOnSameAccount bool,
) *UpstreamFailoverError {
	oauth429Retry := s.shouldRetryOpenAIOAuth429OnSameAccountWithResponse(account, statusCode, shouldDisable, classificationHeaders, responseBody)
	failoverErr := newOpenAIUpstreamFailoverError(
		statusCode,
		responseHeaders,
		responseBody,
		upstreamMsg,
		retryableOnSameAccount || oauth429Retry,
	)
	if oauth429Retry {
		failoverErr.SameAccountRetryDeadline = s.openAIOAuth429RetryDeadline(account)
		failoverErr.SameAccountRetryDelay = openAIOAuth429SameAccountRetryDelay(responseHeaders, failoverErr.SameAccountRetryDeadline)
	}
	return failoverErr
}

const (
	openAIUpstreamAccessUnavailableClientMessage = "Upstream access is temporarily unavailable, please retry later"
	// OpenAIUpstreamAccessStateReason marks a provider credential whose
	// account, workspace, or organization is unavailable.
	OpenAIUpstreamAccessStateReason = GatewayFailureReason("openai_upstream_access_state")
	// OpenAIHTTPContinuationUnsupportedReason identifies accounts that cannot
	// preserve an official Responses HTTP continuation without dropping state.
	OpenAIHTTPContinuationUnsupportedReason = GatewayFailureReason("openai_http_continuation_unsupported")
)

// isOpenAIUpstreamAccessStateError recognizes provider-side credential state
// failures only from explicit structured codes. Free-form messages may contain
// echoed user input, including inside stream terminal error.message fields.
func isOpenAIUpstreamAccessStateError(_ string, body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	for _, path := range []string{"error.code", "response.error.code", "detail.code", "code"} {
		if isOpenAIUpstreamAccessStateCode(gjson.GetBytes(body, path).String()) {
			return true
		}
	}
	return false
}

func isOpenAIUpstreamAccessStateCode(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "deactivated_workspace" {
		return true
	}
	for _, subject := range []string{"workspace", "account", "organization", "org"} {
		for _, state := range []string{"deactivated", "disabled", "suspended"} {
			if value == subject+"_"+state || value == state+"_"+subject {
				return true
			}
		}
	}
	return false
}

// isOpenAIHTTPUpstreamAccessStateError is deliberately status-independent:
// known provider codes are durable evidence, while 401/403 messages without
// such a code must flow through the existing authentication/403 policies.
func isOpenAIHTTPUpstreamAccessStateError(_ int, _ string, body []byte) bool {
	return isOpenAIUpstreamAccessStateError("", body)
}

func openAICapacityShedClientMessage(upstreamMsg string, body []byte) string {
	for _, candidate := range []string{
		upstreamMsg,
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "response.error.message").String(),
		gjson.GetBytes(body, "message").String(),
	} {
		candidate = sanitizeUpstreamErrorMessage(strings.TrimSpace(candidate))
		if candidate != "" && isOpenAICapacityShedMessage(candidate) {
			return candidate
		}
	}
	return "Upstream service is temporarily overloaded, please retry later"
}

// IsOpenAIRequestBodyTooLarge reports whether another account may accept the
// same request even though the selected account rejected its serialized size.
func (e *UpstreamFailoverError) IsOpenAIRequestBodyTooLarge() bool {
	return e != nil && e.Reason == openAIRequestBodyTooLargeReason
}

// IsOpenAICapacityShed reports whether typed client fields were derived from a
// recognized provider overload rather than supplied by an unrelated failure.
func (e *UpstreamFailoverError) IsOpenAICapacityShed() bool {
	return e != nil && e.RequestScopedTransient && isOpenAIRequestScopedCapacityShed("", e.ResponseBody)
}

func marshalOpenAIUpstreamJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

func openAIUpstreamErrorBodyReadLimitForConfig(cfg *config.Config) int64 {
	limit := openAIUpstreamErrorBodyReadLimit
	if cfg != nil && cfg.Gateway.LogUpstreamErrorBody && cfg.Gateway.LogUpstreamErrorBodyMaxBytes > int(limit) {
		limit = int64(cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
	}
	return limit
}

func (s *OpenAIGatewayService) readUpstreamErrorBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	cfg := (*config.Config)(nil)
	if s != nil {
		cfg = s.cfg
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, openAIUpstreamErrorBodyReadLimitForConfig(cfg)))
	return body
}

func (s *OpenAIGatewayService) handleFailoverSideEffects(ctx context.Context, resp *http.Response, account *Account, responseBody []byte, canonicalModel ...string) bool {
	if len(canonicalModel) > 0 {
		return s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, responseBody, canonicalModel[0])
	}
	return s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, responseBody)
}

// --- QK-S2: masked-account upstream identity scrubbing for client errors ---
//
// TH free / masked-identity accounts (extra.mask_upstream_identity=true, decided
// by IsIdentityMaskedAccount) must never leak the upstream model name or the
// tokenharbor platform trace to the client. The helpers below scrub client-
// visible error text for such accounts. Non-masked accounts (including genuine
// kimi accounts) are a hard zero-impact boundary: every helper is a no-op for
// them and returns the input unchanged.
//
// This is the client-visible scrubbing layer that complements the model-name
// rewrite (QK-S1, account_identity_mask.go) which already rewrites echoed model
// names in successful responses. Error bodies are scrubbed here, at every error
// write point, because failure responses are not covered by the success-path
// rewrite.

const (
	maskedUpstreamModelPlaceholder = "the requested model"
	maskedTokenHarborPlaceholder   = "the upstream service"

	// MaskedAccountFailoverKey is the gin.Context key under which the service
	// stashes whether the account that produced the current failover error is a
	// masked-identity account. handleFailoverExhausted reads it to decide whether
	// to mask the final client error. This closes the QK-S2 source-binding on the
	// handler side without touching the UpstreamFailoverError struct or any of
	// its construction points.
	MaskedAccountFailoverKey = "openai_gateway_masked_account_failover"
)

// maskedAccountUpstreamModelIdentifiers returns the upstream model name strings
// that must be scrubbed from client-visible error text for a masked-identity
// account. These are the values of the account's model mappings (and their
// suffix-stripped aliases), since those values are the upstream model names the
// client must not learn about.
func maskedAccountUpstreamModelIdentifiers(account *Account) []string {
	if account == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var ids []string
	add := func(m string) {
		for _, variant := range IdentityRewriteAliases(m) {
			variant = strings.TrimSpace(variant)
			if variant == "" {
				continue
			}
			if _, ok := seen[variant]; ok {
				continue
			}
			seen[variant] = struct{}{}
			ids = append(ids, variant)
		}
	}
	if mp := account.GetModelMapping(); mp != nil {
		for _, v := range mp {
			add(v)
		}
	}
	if cmp := account.GetCompactModelMapping(); cmp != nil {
		for _, v := range cmp {
			add(v)
		}
	}
	// QK-S2R2 P2: sort identifiers by length descending so that longer (more
	// specific) identifiers are replaced before shorter overlapping ones (e.g.
	// "qwen3.8-flash" before "qwen"). Equal-length identifiers keep their
	// insertion order (stable) so the replacement sequence is deterministic.
	// Without this, a short identifier replaced first fragments a longer one
	// into an unmatched residue that survives scrubbing and leaks.
	sort.SliceStable(ids, func(i, j int) bool {
		return len(ids[i]) > len(ids[j])
	})
	return ids
}

func replaceAllIgnoreCase(s, old, repl string) string {
	if old == "" {
		return s
	}
	var b strings.Builder
	lower := strings.ToLower(s)
	oldLower := strings.ToLower(old)
	for {
		idx := strings.Index(lower, oldLower)
		if idx < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:idx])
		b.WriteString(repl)
		s = s[idx+len(old):]
		lower = lower[idx+len(oldLower):]
	}
	return b.String()
}

// upstreamIdentityLeaks reports whether client-visible text still contains an
// upstream model identifier or the tokenharbor trace for a masked account.
func upstreamIdentityLeaks(account *Account, msg string) bool {
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "tokenharbor") {
		return true
	}
	for _, id := range maskedAccountUpstreamModelIdentifiers(account) {
		if strings.Contains(lower, strings.ToLower(id)) {
			return true
		}
	}
	return false
}

// MaskUpstreamErrorMessageForClient returns the client-safe version of an
// upstream error message for a masked-identity account. Non-masked accounts get
// the message unchanged (zero-impact no-op). Masked accounts get upstream model
// identifiers and the tokenharbor trace scrubbed to generic placeholders. If the
// text cannot be safely scrubbed (an identifier or trace survives replacement),
// it fails closed to an empty string so the caller can substitute a fixed
// platform-standard message.
func MaskUpstreamErrorMessageForClient(account *Account, upstreamMsg string) string {
	if account == nil || !IsIdentityMaskedAccount(account) {
		return upstreamMsg
	}
	msg := strings.TrimSpace(upstreamMsg)
	if msg == "" {
		return ""
	}
	scrubbed := msg
	for _, id := range maskedAccountUpstreamModelIdentifiers(account) {
		scrubbed = replaceAllIgnoreCase(scrubbed, id, maskedUpstreamModelPlaceholder)
	}
	scrubbed = replaceAllIgnoreCase(scrubbed, "tokenharbor", maskedTokenHarborPlaceholder)
	if upstreamIdentityLeaks(account, scrubbed) {
		return ""
	}
	return scrubbed
}

// ClientSafeUpstreamErrorMessage returns the client-safe error message for the
// account, falling back to fallback when the masked-account message cannot be
// safely normalized. Non-masked accounts always get upstreamMsg unchanged.
func ClientSafeUpstreamErrorMessage(account *Account, upstreamMsg, fallback string) string {
	if account == nil || !IsIdentityMaskedAccount(account) {
		return upstreamMsg
	}
	if m := MaskUpstreamErrorMessageForClient(account, upstreamMsg); m != "" {
		return m
	}
	return fallback
}

func (s *OpenAIGatewayService) handleErrorResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestBody []byte,
	requestedModel ...string,
) (*OpenAIForwardResult, error) {
	body := s.readUpstreamErrorBody(resp)
	body = s.redactAgentIdentitySensitiveBody(ctx, account, body)

	// cyber_policy 硬阻断：透传上游原始错误体给客户端（不重包成通用 502），不冷却账号。
	// 当前请求恒透传（需求1）；标记供 handler 事后写风控/邮件。400 cyber 不可 failover
	// （shouldFailoverUpstreamError(400)=false），故走到此处即可安全早返回。
	if hit, code, cyberMsg := detectOpenAICyberPolicy(body); hit {
		MarkOpsCyberPolicy(c, CyberPolicyMark{
			Code:           code,
			Message:        cyberMsg,
			Body:           truncateString(string(body), 4096),
			UpstreamStatus: resp.StatusCode,
		})
		setOpsUpstreamError(c, resp.StatusCode, cyberMsg, truncateString(string(body), 2048))
		writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
		}
		c.Data(resp.StatusCode, contentType, body)
		if cyberMsg == "" {
			return nil, fmt.Errorf("openai cyber_policy: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("openai cyber_policy: %s", cyberMsg)
	}
	if account != nil && account.Platform == PlatformGrok && isGrokContentPolicyRejection(resp.StatusCode, body) {
		clientMsg := grokContentPolicyClientMessage(body)
		setOpsUpstreamError(c, resp.StatusCode, clientMsg, truncateString(string(body), 2048))
		writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		MarkResponseCommitted(c)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": clientMsg,
			},
		})
		return nil, fmt.Errorf("grok content policy rejection: %s", clientMsg)
	}

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
	// QK-S2: client-safe variant of the upstream message. No-op for non-masked
	// accounts (including genuine kimi accounts) — byte-for-byte unchanged.
	// Also stash the masked-identity flag for the failover final-write closure;
	// set per-attempt so it always reflects the account that produced this error.
	masked := IsIdentityMaskedAccount(account)
	c.Set(MaskedAccountFailoverKey, masked)
	clientMsg := ClientSafeUpstreamErrorMessage(account, upstreamMsg, infraerrors.UpstreamRequestFailed)
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	logOpenAIInstructionsRequiredDebug(ctx, c, account, resp.StatusCode, upstreamMsg, requestBody, body)

	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		logger.LegacyPrintf("service.openai_gateway",
			"OpenAI upstream error %d (account=%d platform=%s type=%s): %s",
			resp.StatusCode,
			account.ID,
			account.Platform,
			account.Type,
			truncateForLog(body, s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes),
		)
	}

	if isOpenAIRequestBodyTooLargeError(resp.StatusCode, upstreamMsg, body) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            opsUpstreamProxyID(account),
			ProxyName:          opsUpstreamProxyName(account),
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "failover",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, requestedModel...)
		return nil, newOpenAIUpstreamFailoverError(
			resp.StatusCode,
			resp.Header,
			body,
			upstreamMsg,
			false,
		)
	}

	// Apply error passthrough rules
	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c,
		PlatformOpenAI,
		resp.StatusCode,
		body,
		http.StatusBadGateway,
		"upstream_error",
		infraerrors.UpstreamRequestFailed,
	); matched {
		// QK-S2: a masked account must not receive an upstream-derived message.
		// Scrub it; if it cannot be safely normalized, skip the passthrough and
		// fall through to the fixed platform-standard handling below.
		if account != nil && IsIdentityMaskedAccount(account) {
			if safe := MaskUpstreamErrorMessageForClient(account, errMsg); safe != "" {
				errMsg = safe
			} else {
				matched = false
			}
		}
		if matched {
			MarkResponseCommitted(c)
			c.JSON(status, gin.H{
				"error": gin.H{
					"type":    errType,
					"message": errMsg,
				},
			})
			if upstreamMsg == "" {
				upstreamMsg = errMsg
			}
			if upstreamMsg == "" {
				return nil, fmt.Errorf("upstream error: %d (passthrough rule matched)", resp.StatusCode)
			}
			return nil, fmt.Errorf("upstream error: %d (passthrough rule matched) message=%s", resp.StatusCode, upstreamMsg)
		}
	}

	// Check custom error codes
	if !account.ShouldHandleErrorCode(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            opsUpstreamProxyID(account),
			ProxyName:          opsUpstreamProxyName(account),
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		MarkResponseCommitted(c)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"type":    "upstream_error",
				"message": "Upstream gateway error",
			},
		})
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (not in custom error codes)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (not in custom error codes) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Handle upstream error (mark account status)
	var reqModel string
	if len(requestedModel) > 0 {
		reqModel = strings.TrimSpace(requestedModel[0])
	}
	if reqModel == "" {
		reqModel, _, _ = extractOpenAIRequestMetaFromBody(requestBody)
		reqModel = canonicalOpenAIAccountSchedulingModel(account, reqModel)
	}
	shouldDisable := s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, reqModel)
	kind := "http_error"
	if shouldDisable {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               kind,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	if shouldDisable {
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           body,
			RetryableOnSameAccount: false,
		}
	}

	MarkResponseCommitted(c)

	// 上游 400 是确定性的请求错误：同一份请求体换账号、重试多少次都会失败。归一成
	// 502 upstream_error 会让下游网关把它当成可重试的上游故障反复重放（#5479 实测
	// 30 个失败请求被放大成 60 次上游调用），同时抹掉客户端定位问题所需的 code/param。
	//
	// 走到这里说明 shouldFailoverOpenAIUpstreamResponse 已判定该 400 不可 failover，
	// 即 server_is_overloaded / at capacity 这类可重试的 400 不会到达此处。
	//
	// 兄弟路径早已这么做：handleCompatErrorResponse（ChatCompletions / Anthropic）
	// 回真实状态码 + invalid_request_error + 真实 message；/v1/images 还额外透传
	// code/param。原生 Responses 是唯一漏掉的一条。
	if isOpenAIDeterministicClientError(resp.StatusCode) {
		if account != nil && IsIdentityMaskedAccount(account) {
			// QK-S2: never echo upstream body fields (error.type/code/param may
			// carry the upstream model identity); write a generic client error.
			msg := clientMsg
			if msg == "" {
				msg = infraerrors.UpstreamRequestFailed
			}
			c.JSON(resp.StatusCode, gin.H{
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": msg,
				},
			})
		} else {
			writeOpenAIUpstreamClientError(c, resp.StatusCode, body, upstreamMsg)
		}
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d message=%s", resp.StatusCode, upstreamMsg)
	}

	// Return appropriate error response
	var errType, errMsg string
	var statusCode int

	switch resp.StatusCode {
	case 401:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = infraerrors.UpstreamAuthFailed
	case 402:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = infraerrors.UpstreamPaymentRequired
	case 403:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = infraerrors.UpstreamForbidden
	case 429:
		statusCode = http.StatusTooManyRequests
		errType = "rate_limit_error"
		errMsg = infraerrors.UpstreamRateLimited
	default:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = infraerrors.UpstreamRequestFailed
	}
	if isOpenAIContextWindowError(upstreamMsg, body) && clientMsg != "" {
		errMsg = clientMsg
	}

	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": errMsg,
		},
	})

	if upstreamMsg == "" {
		return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
	}
	return nil, fmt.Errorf("upstream error: %d message=%s", resp.StatusCode, upstreamMsg)
}

// compatErrorWriter is the signature for format-specific error writers used by
// the compat paths (Chat Completions and Anthropic Messages).
type compatErrorWriter func(c *gin.Context, statusCode int, errType, message string)

// handleCompatErrorResponse is the shared non-failover error handler for the
// Chat Completions and Anthropic Messages compat paths. It mirrors the logic of
// handleErrorResponse (passthrough rules, ShouldHandleErrorCode, rate-limit
// tracking, secondary failover) but delegates the final error write to the
// format-specific writer function.
func (s *OpenAIGatewayService) handleCompatErrorResponse(
	resp *http.Response,
	c *gin.Context,
	account *Account,
	writeError compatErrorWriter,
	requestedModel ...string,
) (*OpenAIForwardResult, error) {
	body := s.readUpstreamErrorBody(resp)
	body = s.redactAgentIdentitySensitiveBody(context.Background(), account, body)

	// cyber_policy：兼容路径（Chat Completions / Anthropic）以各自格式回写错误，
	// 不原样透传 responses 格式的 cyber body（否则对下游格式不合法）。cyber 是上游网络
	// 安全策略拦截，不冷却账号，故标记后直接以兼容格式回写错误并返回，跳过下方
	// handleOpenAIAccountUpstreamError（避免自定义 temp-unschedulable 规则误冷却）。
	if hit, code, cyberMsg := detectOpenAICyberPolicy(body); hit {
		MarkOpsCyberPolicy(c, CyberPolicyMark{
			Code:           code,
			Message:        cyberMsg,
			Body:           truncateString(string(body), 4096),
			UpstreamStatus: resp.StatusCode,
		})
		setOpsUpstreamError(c, resp.StatusCode, cyberMsg, truncateString(string(body), 2048))
		clientMsg := cyberMsg
		if clientMsg == "" {
			clientMsg = "Request blocked by upstream cyber-security policy"
		}
		writeError(c, resp.StatusCode, "invalid_request_error", clientMsg)
		if cyberMsg == "" {
			return nil, fmt.Errorf("openai cyber_policy: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("openai cyber_policy: %s", cyberMsg)
	}
	if account != nil && account.Platform == PlatformGrok && isGrokContentPolicyRejection(resp.StatusCode, body) {
		clientMsg := grokContentPolicyClientMessage(body)
		setOpsUpstreamError(c, resp.StatusCode, clientMsg, truncateString(string(body), 2048))
		MarkResponseCommitted(c)
		writeError(c, http.StatusForbidden, "invalid_request_error", clientMsg)
		return nil, fmt.Errorf("grok content policy rejection: %s", clientMsg)
	}

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("Upstream error: %d", resp.StatusCode)
	}
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
	// QK-S2: client-safe variant. No-op for non-masked accounts. Stash the
	// masked-identity flag for the failover final-write closure (per-attempt).
	c.Set(MaskedAccountFailoverKey, IsIdentityMaskedAccount(account))
	clientMsg := ClientSafeUpstreamErrorMessage(account, upstreamMsg, infraerrors.UpstreamRequestFailed)

	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)

	// Apply error passthrough rules
	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c, account.Platform, resp.StatusCode, body,
		http.StatusBadGateway, "api_error", infraerrors.UpstreamRequestFailed,
	); matched {
		// QK-S2: scrub the passthrough message for masked accounts; if it cannot
		// be safely normalized, skip the passthrough and fall through.
		if account != nil && IsIdentityMaskedAccount(account) {
			if safe := MaskUpstreamErrorMessageForClient(account, errMsg); safe != "" {
				errMsg = safe
			} else {
				matched = false
			}
		}
		if matched {
			MarkResponseCommitted(c)
			writeError(c, status, errType, errMsg)
			if upstreamMsg == "" {
				upstreamMsg = errMsg
			}
			if upstreamMsg == "" {
				return nil, fmt.Errorf("upstream error: %d (passthrough rule matched)", resp.StatusCode)
			}
			return nil, fmt.Errorf("upstream error: %d (passthrough rule matched) message=%s", resp.StatusCode, upstreamMsg)
		}
	}

	// Check custom error codes — if the account does not handle this status,
	// return a generic error without exposing upstream details.
	if !account.ShouldHandleErrorCode(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            opsUpstreamProxyID(account),
			ProxyName:          opsUpstreamProxyName(account),
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		MarkResponseCommitted(c)
		writeError(c, http.StatusInternalServerError, "api_error", "Upstream gateway error")
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (not in custom error codes)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (not in custom error codes) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Track rate limits and decide whether to trigger secondary failover.
	var modelForCooldown string
	if len(requestedModel) > 0 {
		modelForCooldown = requestedModel[0]
	}
	shouldDisable := s.handleOpenAIAccountUpstreamError(
		c.Request.Context(), account, resp.StatusCode, resp.Header, body, modelForCooldown,
	)
	kind := "http_error"
	if shouldDisable {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               kind,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	if shouldDisable {
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           body,
			RetryableOnSameAccount: false,
		}
	}

	MarkResponseCommitted(c)

	// Map status code to error type and write response
	errType := "api_error"
	switch {
	case resp.StatusCode == 400:
		errType = "invalid_request_error"
	case resp.StatusCode == 404:
		errType = "not_found_error"
	case resp.StatusCode == 429:
		errType = "rate_limit_error"
	case resp.StatusCode >= 500:
		errType = "api_error"
	}

	writeError(c, resp.StatusCode, errType, clientMsg)
	return nil, fmt.Errorf("upstream error: %d %s", resp.StatusCode, upstreamMsg)
}
