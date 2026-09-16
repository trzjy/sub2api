// Package provider contains concrete payment provider implementations.
package provider

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

// XunhuPay constants.
const (
	// xunhupayCurrency 是 CN 渠道协议币种（虎皮椒以人民币计价）。
	// 协议透传行为不变；下单快照据此写 snapshot["currency"]。
	xunhupayCurrency         = "CNY"
	xunhupayVersion          = "1.1"
	xunhupayStatusDone       = "OD" // Order Done — paid
	xunhupayHTTPTimeout      = 10 * time.Second
	maxXunhupayResponseSize  = 1 << 20 // 1MB
	maxXunhupayErrorSummary  = 512
	xunhupayWAPType          = "WAP" // WeChat H5 payment
	xunhupayRefundStatusDone = "CD"  // Refunded
)

// Xunhupay implements payment.Provider for the XunhuPay (虎皮椒) aggregation platform.
type Xunhupay struct {
	instanceID string
	config     map[string]string
	httpClient *http.Client
}

// NewXunhupay creates a new XunhuPay provider.
// config keys: appId, appSecret, apiBase, notifyUrl, returnUrl
func NewXunhupay(instanceID string, config map[string]string) (*Xunhupay, error) {
	for _, k := range []string{"appId", "appSecret", "apiBase", "notifyUrl", "returnUrl"} {
		if strings.TrimSpace(config[k]) == "" {
			return nil, fmt.Errorf("xunhupay config missing required key: %s", k)
		}
	}
	cfg := make(map[string]string, len(config))
	for k, v := range config {
		cfg[k] = v
	}
	cfg["apiBase"] = normalizeXunhupayAPIBase(cfg["apiBase"])
	return &Xunhupay{
		instanceID: instanceID,
		config:     cfg,
		httpClient: &http.Client{Timeout: xunhupayHTTPTimeout},
	}, nil
}

func normalizeXunhupayAPIBase(apiBase string) string {
	base := strings.TrimSpace(apiBase)
	if base == "" {
		return ""
	}
	if parsed, err := url.Parse(base); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		parsed.RawPath = ""
		parsed.Path = trimXunhupayEndpointPath(parsed.Path)
		return strings.TrimRight(parsed.String(), "/")
	}
	return strings.TrimRight(trimXunhupayEndpointPath(base), "/")
}

func trimXunhupayEndpointPath(path string) string {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	lower := strings.ToLower(path)
	for _, endpoint := range []string{"/payment", "/do.html", "/query.html", "/refund.html"} {
		if strings.HasSuffix(lower, endpoint) {
			return strings.TrimRight(path[:len(path)-len(endpoint)], "/")
		}
	}
	return path
}

func (x *Xunhupay) apiBase() string {
	if x == nil {
		return ""
	}
	return normalizeXunhupayAPIBase(x.config["apiBase"])
}

func (x *Xunhupay) Name() string        { return "XunhuPay" }
func (x *Xunhupay) ProviderKey() string { return payment.TypeXunhupay }
func (x *Xunhupay) SupportedTypes() []payment.PaymentType {
	return []payment.PaymentType{payment.TypeWxpay}
}

func (x *Xunhupay) MerchantIdentityMetadata() map[string]string {
	if x == nil {
		return nil
	}
	appid := strings.TrimSpace(x.config["appId"])
	if appid == "" {
		return nil
	}
	return map[string]string{"appid": appid}
}

func (x *Xunhupay) CreatePayment(ctx context.Context, req payment.CreatePaymentRequest) (*payment.CreatePaymentResponse, error) {
	notifyURL, returnURL := x.resolveURLs(req)
	params := map[string]string{
		"version":         xunhupayVersion,
		"appid":           x.config["appId"],
		"trade_order_id":  req.OrderID,
		"total_fee":       req.Amount,
		"title":           truncateXunhupayTitle(req.Subject),
		"time":            strconv.FormatInt(time.Now().Unix(), 10),
		"notify_url":      notifyURL,
		"return_url":      returnURL,
		"nonce_str":       xunhupayNonce(),
		"plugins":         "sub2api", // 对接程序标识；回调会原样带回，但本实现不依赖它选择 appSecret
	}
	if req.IsMobile {
		params["type"] = xunhupayWAPType
	}
	params["hash"] = xunhupaySign(params, x.config["appSecret"])

	body, err := x.post(ctx, x.apiBase()+"/payment/do.html", params)
	if err != nil {
		return nil, fmt.Errorf("xunhupay create: %w", err)
	}
	var resp xunhupayResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("xunhupay parse create: %w", err)
	}
	if err := resp.checkError(); err != nil {
		return nil, err
	}
	if resp.Data == nil {
		return nil, fmt.Errorf("xunhupay create: empty data")
	}
	return &payment.CreatePaymentResponse{
		TradeNo: resp.Data.OpenOrderID,
		PayURL:  resp.Data.URL,
		QRCode:  resp.Data.URLQRCode,
	}, nil
}

func (x *Xunhupay) QueryOrder(ctx context.Context, tradeNo string) (*payment.QueryOrderResponse, error) {
	params := map[string]string{
		"appid":           x.config["appId"],
		"out_trade_order": tradeNo,
		"time":            strconv.FormatInt(time.Now().Unix(), 10),
		"nonce_str":       xunhupayNonce(),
	}
	params["hash"] = xunhupaySign(params, x.config["appSecret"])

	body, err := x.post(ctx, x.apiBase()+"/payment/query.html", params)
	if err != nil {
		return nil, fmt.Errorf("xunhupay query: %w", err)
	}
	var resp xunhupayResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("xunhupay parse query: %w", err)
	}
	if err := resp.checkError(); err != nil {
		return nil, err
	}
	if resp.Data == nil {
		return nil, fmt.Errorf("xunhupay query: empty data")
	}

	status := payment.ProviderStatusPending
	if strings.EqualFold(resp.Data.Status, xunhupayStatusDone) {
		status = payment.ProviderStatusPaid
	}
	amount, _ := strconv.ParseFloat(resp.Data.TotalAmount, 64)
	return &payment.QueryOrderResponse{
		TradeNo:  resp.Data.TransactionID,
		Status:   status,
		Amount:   amount,
		Metadata: x.MerchantIdentityMetadata(),
	}, nil
}

func (x *Xunhupay) VerifyNotification(_ context.Context, rawBody string, _ map[string]string) (*payment.PaymentNotification, error) {
	values, err := url.ParseQuery(rawBody)
	if err != nil {
		return nil, fmt.Errorf("parse notify: %w", err)
	}
	params := make(map[string]string)
	for k := range values {
		params[k] = values.Get(k)
	}
	hash := params["hash"]
	if hash == "" {
		return nil, fmt.Errorf("missing hash")
	}
	if !xunhupayVerifySign(params, x.config["appSecret"], hash) {
		return nil, fmt.Errorf("invalid signature")
	}

	status := payment.ProviderStatusFailed
	if strings.EqualFold(params["status"], xunhupayStatusDone) {
		status = payment.ProviderStatusSuccess
	}
	amount, _ := strconv.ParseFloat(params["total_fee"], 64)

	metadata := x.MerchantIdentityMetadata()
	if appid := strings.TrimSpace(params["appid"]); appid != "" {
		if metadata == nil {
			metadata = map[string]string{}
		}
		metadata["appid"] = appid
	}
	return &payment.PaymentNotification{
		TradeNo: params["transaction_id"], OrderID: params["trade_order_id"],
		Amount: amount, Status: status, RawData: rawBody, Metadata: metadata,
	}, nil
}

func (x *Xunhupay) Refund(ctx context.Context, req payment.RefundRequest) (*payment.RefundResponse, error) {
	attempts := x.refundAttempts(req)
	if len(attempts) == 0 {
		return nil, fmt.Errorf("xunhupay refund missing order identifier")
	}
	var firstErr error
	for i, attempt := range attempts {
		params := map[string]string{
			"appid":     x.config["appId"],
			"time":      strconv.FormatInt(time.Now().Unix(), 10),
			"nonce_str": xunhupayNonce(),
		}
		if reason := strings.TrimSpace(req.Reason); reason != "" {
			params["reason"] = truncateXunhupayReason(reason)
		}
		for k, v := range attempt.params {
			params[k] = v
		}
		params["hash"] = xunhupaySign(params, x.config["appSecret"])

		body, status, err := x.postRaw(ctx, x.apiBase()+"/payment/refund.html", params)
		if err != nil {
			return nil, fmt.Errorf("xunhupay refund request: %w", err)
		}
		refundStatus, err := parseXunhupayRefundResponse(status, body)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if i+1 < len(attempts) && isXunhupayRefundOrderNotFound(err) {
				continue
			}
			return nil, err
		}
		return &payment.RefundResponse{RefundID: attempt.refundID, Status: refundStatus}, nil
	}
	return nil, firstErr
}

type xunhupayRefundAttempt struct {
	params   map[string]string
	refundID string
}

func (x *Xunhupay) refundAttempts(req payment.RefundRequest) []xunhupayRefundAttempt {
	var attempts []xunhupayRefundAttempt
	if orderID := strings.TrimSpace(req.OrderID); orderID != "" {
		attempts = append(attempts, xunhupayRefundAttempt{
			params:   map[string]string{"trade_order_id": orderID},
			refundID: orderID,
		})
	}
	if tradeNo := strings.TrimSpace(req.TradeNo); tradeNo != "" {
		attempts = append(attempts, xunhupayRefundAttempt{
			params:   map[string]string{"open_order_id": tradeNo},
			refundID: tradeNo,
		})
	}
	return attempts
}

func (x *Xunhupay) resolveURLs(req payment.CreatePaymentRequest) (string, string) {
	notifyURL := req.NotifyURL
	if notifyURL == "" {
		notifyURL = x.config["notifyUrl"]
	}
	returnURL := req.ReturnURL
	if returnURL == "" {
		returnURL = x.config["returnUrl"]
	}
	return notifyURL, returnURL
}

// xunhupayResponse is the shared XunhuPay API response envelope.
type xunhupayResponse struct {
	ErrCode int               `json:"errcode"`
	ErrMsg  string            `json:"errmsg"`
	Hash    string            `json:"hash"`
	Data    *xunhupayRespData `json:"data"`
}

type xunhupayRespData struct {
	URL           string `json:"url"`
	URLQRCode     string `json:"url_qrcode"`
	OpenOrderID   string `json:"open_order_id"`
	OutTradeOrder string `json:"out_trade_order"`
	Status        string `json:"status"`
	TotalAmount   string `json:"total_amount"`
	TransactionID string `json:"transaction_id"`
}

// checkError validates the response envelope. Only errcode is authoritative
// across the whole API surface: the official PHP demo and the webx-top Go SDK
// both gate on errcode==0 alone. Response hash verification is NOT performed —
// the official docs never specify how the nested "data" object is serialized
// into the signature, and the SDKs disagree on it, so attempting to verify
// would reject valid responses on a signature we cannot reproduce. Request
// signing and callback verification (both fully specified) are where the money
// safety lies.
//
// Residual risk (documented for ops): because responses are trusted on
// errcode + HTTPS alone, a leaked appSecret would let an attacker craft
// errcode=0 query/refund responses the server cannot detect. Keep appSecret
// private and reconcile refunds against gateway statements.
func (r *xunhupayResponse) checkError() error {
	if r == nil {
		return fmt.Errorf("xunhupay: nil response")
	}
	if r.ErrCode != 0 {
		return fmt.Errorf("xunhupay error %d: %s", r.ErrCode, strings.TrimSpace(r.ErrMsg))
	}
	return nil
}

// parseXunhupayRefundResponse parses the refund.html response and maps the
// refund_status to a provider status. CD (refunded) is a terminal success;
// RD (refund in progress) is reported as pending so the service layer can
// follow up; OD (paid, not yet refunded) means the request has not taken
// effect and is treated as an error.
func parseXunhupayRefundResponse(status int, body []byte) (string, error) {
	summary := summarizeXunhupayResponse(body)
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return "", fmt.Errorf("xunhupay refund HTTP %d: %s", status, summary)
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", fmt.Errorf("xunhupay refund empty response (HTTP %d): %s", status, summary)
	}
	var resp struct {
		ErrCode      int    `json:"errcode"`
		ErrMsg       string `json:"errmsg"`
		RefundStatus string `json:"refund_status"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("xunhupay refund non-JSON response (HTTP %d): %s", status, summary)
	}
	if resp.ErrCode != 0 {
		msg := strings.TrimSpace(resp.ErrMsg)
		if msg == "" {
			msg = summary
		}
		return "", fmt.Errorf("xunhupay refund failed (HTTP %d): %s", status, msg)
	}
	switch resp.RefundStatus {
	case "", xunhupayRefundStatusDone: // "CD" is the terminal success
		return payment.ProviderStatusSuccess, nil
	case "RD": // refund in progress
		return payment.ProviderStatusPending, nil
	default: // "OD" = paid but not yet refunded
		return "", fmt.Errorf("xunhupay refund status %s", resp.RefundStatus)
	}
}

func isXunhupayRefundOrderNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	return strings.Contains(msg, "订单编号不存在") ||
		strings.Contains(msg, "订单不存在") ||
		strings.Contains(lower, "order not found") ||
		strings.Contains(lower, "not exist")
}

func summarizeXunhupayResponse(body []byte) string {
	summary := strings.Join(strings.Fields(string(body)), " ")
	if summary == "" {
		return "<empty>"
	}
	if len(summary) > maxXunhupayErrorSummary {
		truncated := summary[:maxXunhupayErrorSummary]
		for len(truncated) > 0 && !utf8.ValidString(truncated) {
			truncated = truncated[:len(truncated)-1]
		}
		return truncated + "..."
	}
	return summary
}

func truncateXunhupayTitle(title string) string {
	// Title max 128 chars; strip % (reserved by the gateway) and emoji to be safe.
	title = strings.ReplaceAll(strings.TrimSpace(title), "%", "")
	runes := []rune(title)
	if len(runes) > 128 {
		return string(runes[:128])
	}
	return title
}

func truncateXunhupayReason(reason string) string {
	// Reason max 80 chars per the refund API.
	runes := []rune(strings.TrimSpace(reason))
	if len(runes) > 80 {
		return string(runes[:80])
	}
	return reason
}

func xunhupayNonce() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// xunhupaySign builds the XunhuPay request signature: all params (excluding
// empty values and the hash field itself) sorted by key in ascending order,
// joined as key=value&..., with the app secret appended directly with no
// separator, then MD5 hex (lowercase).
func xunhupaySign(params map[string]string, appSecret string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == "hash" || v == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf strings.Builder
	for i, k := range keys {
		if i > 0 {
			_ = buf.WriteByte('&')
		}
		_, _ = buf.WriteString(k + "=" + params[k])
	}
	_, _ = buf.WriteString(appSecret)
	sum := md5.Sum([]byte(buf.String()))
	return hex.EncodeToString(sum[:])
}

func xunhupayVerifySign(params map[string]string, appSecret string, hash string) bool {
	return hmac.Equal([]byte(xunhupaySign(params, appSecret)), []byte(hash))
}

func (x *Xunhupay) post(ctx context.Context, endpoint string, params map[string]string) ([]byte, error) {
	body, _, err := x.postRaw(ctx, endpoint, params)
	return body, err
}

func (x *Xunhupay) postRaw(ctx context.Context, endpoint string, params map[string]string) ([]byte, int, error) {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := x.httpClient
	if client == nil {
		client = &http.Client{Timeout: xunhupayHTTPTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxXunhupayResponseSize))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

var (
	_ payment.Provider               = (*Xunhupay)(nil)
	_ payment.MerchantIdentityProvider = (*Xunhupay)(nil)
)
