package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DevinUserStatusPath 是 seat management 的 GetUserStatus RPC 路径。
const DevinUserStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"

// maxDevinBalanceMicro 是 balanceMicro 的可信上限（微美元，1e12 = $1,000,000）。
// 超过该值或为负（int64 溢出）都视为未知，不暴露给调用方。
const maxDevinBalanceMicro uint64 = 1_000_000_000_000

// DevinAccountStatus 是 Devin 账号额度快照。
type DevinAccountStatus struct {
	Plan                   string          `json:"plan,omitempty"`
	BalanceUSD             *float64        `json:"balance_usd,omitempty"`
	DailyRemainingPercent  *int            `json:"daily_remaining_percent,omitempty"`
	WeeklyRemainingPercent *int            `json:"weekly_remaining_percent,omitempty"`
	DailyResetAt           *time.Time      `json:"daily_reset_at,omitempty"`
	WeeklyResetAt          *time.Time      `json:"weekly_reset_at,omitempty"`
	FetchedAt              time.Time       `json:"fetched_at"`
	LocalCost              *DevinLocalCost `json:"local_cost,omitempty"`
}

// DevinStatusClient 封装 GetUserStatus 查询。
type DevinStatusClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewDevinStatusClient() *DevinStatusClient {
	return &DevinStatusClient{
		BaseURL:    DevinAPIURL,
		HTTPClient: &http.Client{Timeout: DevinDefaultTimeout},
	}
}

// Fetch 用 session token 查询 Devin 账号额度。
// 请求体与 GetUserJwt 一致（field 1 metadata）；鉴权头为 Basic n-n。
func (c *DevinStatusClient) Fetch(ctx context.Context, token string) (*DevinAccountStatus, error) {
	n := NormalizeDevinToken(token)
	meta := &Metadata{
		IDEName:          DevinClientName,
		IDEVersion:       DevinIDEVersion,
		ExtensionName:    DevinClientName,
		ExtensionVersion: DevinExtensionVersion,
		APIKey:           n,
		Locale:           "en",
	}
	reqBody := encodeGetUserJwtRequest(meta)

	url := strings.TrimRight(c.BaseURL, "/") + DevinUserStatusPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("devin status create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Authorization", "Basic "+n+"-"+n)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("devin status http: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("devin status read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := respBody
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("devin status %d: %s", resp.StatusCode, string(snippet))
	}

	status, err := decodeGetUserStatusResponse(respBody)
	if err != nil {
		return nil, fmt.Errorf("devin status decode: %w", err)
	}
	status.FetchedAt = time.Now().UTC()
	return status, nil
}

// statusWireField 是扫描 protobuf message 时记录的单个字段原始值。
type statusWireField struct {
	num    int
	wire   int
	varint uint64
	bytes  []byte
}

// scanStatusFields 用生产解码器扫描一段 protobuf message 的全部字段。
// 未知字段按 proto3 语义跳过；任何长度/线类型异常通过 error 上抛。
func scanStatusFields(data []byte) ([]statusWireField, error) {
	d := NewProtoDecoder(data)
	var out []statusWireField
	for !d.Done() {
		num, wire := d.ReadTag()
		f := statusWireField{num: num, wire: wire}
		switch wire {
		case 0:
			f.varint = d.ReadUint64()
		case 2:
			f.bytes = append([]byte(nil), d.ReadBytes()...)
		default:
			d.Skip(wire)
		}
		out = append(out, f)
	}
	if err := d.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func firstBytesField(fields []statusWireField, num int) ([]byte, bool) {
	for _, f := range fields {
		if f.num == num && f.wire == 2 {
			return f.bytes, true
		}
	}
	return nil, false
}

func firstVarintField(fields []statusWireField, num int) (uint64, bool) {
	for _, f := range fields {
		if f.num == num && f.wire == 0 {
			return f.varint, true
		}
	}
	return 0, false
}

func normalizeDevinPlan(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// decodeGetUserStatusResponse 按字段路径解码：
//   - #1.13.14 dailyQuotaRemainingPercent / #1.13.15 weeklyQuotaRemainingPercent (varint, 0..100)
//   - #1.13.16 balanceMicro (varint, 微美元)
//   - #1.13.17 dailyReset / #1.13.18 weeklyReset (epoch 秒)
//   - #1.13.1.2 plan (string)，fallback 到 #2.2
func decodeGetUserStatusResponse(data []byte) (*DevinAccountStatus, error) {
	root, err := scanStatusFields(data)
	if err != nil {
		return nil, fmt.Errorf("decode root: %w", err)
	}
	status := &DevinAccountStatus{}

	if lvl1, ok := firstBytesField(root, 1); ok {
		lvl1Fields, err := scanStatusFields(lvl1)
		if err != nil {
			return nil, fmt.Errorf("decode field 1: %w", err)
		}
		if billing, ok := firstBytesField(lvl1Fields, 13); ok {
			if err := decodeDevinBillingBlock(billing, status); err != nil {
				return nil, fmt.Errorf("decode billing: %w", err)
			}
		}
	}

	if status.Plan == "" {
		if lvl2, ok := firstBytesField(root, 2); ok {
			lvl2Fields, err := scanStatusFields(lvl2)
			if err != nil {
				return nil, fmt.Errorf("decode field 2: %w", err)
			}
			if plan, ok := firstBytesField(lvl2Fields, 2); ok {
				status.Plan = normalizeDevinPlan(string(plan))
			}
		}
	}

	return status, nil
}

func decodeDevinBillingBlock(data []byte, status *DevinAccountStatus) error {
	fields, err := scanStatusFields(data)
	if err != nil {
		return err
	}

	if pct, ok := firstVarintField(fields, 14); ok && pct <= 100 {
		v := int(pct)
		status.DailyRemainingPercent = &v
	}
	if pct, ok := firstVarintField(fields, 15); ok && pct <= 100 {
		v := int(pct)
		status.WeeklyRemainingPercent = &v
	}
	if micro, ok := firstVarintField(fields, 16); ok && int64(micro) >= 0 && micro <= maxDevinBalanceMicro {
		usd := float64(micro) / 1e6
		status.BalanceUSD = &usd
	}
	if sec, ok := firstVarintField(fields, 17); ok {
		t := time.Unix(int64(sec), 0)
		status.DailyResetAt = &t
	}
	if sec, ok := firstVarintField(fields, 18); ok {
		t := time.Unix(int64(sec), 0)
		status.WeeklyResetAt = &t
	}
	if planMsg, ok := firstBytesField(fields, 1); ok {
		planFields, err := scanStatusFields(planMsg)
		if err != nil {
			return err
		}
		if plan, ok := firstBytesField(planFields, 2); ok {
			status.Plan = normalizeDevinPlan(string(plan))
		}
	}
	return nil
}
