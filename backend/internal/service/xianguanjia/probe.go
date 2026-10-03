package xianguanjia

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// pathAuthorizeList 是探活用官方路径（reference/open-platform/api-93586388.md:12）。
const pathAuthorizeList = "/api/open/user/authorize/list"

// probeTimeout 是单次探活的超时上限；探活是 admin 手动动作，避免长时间挂起。
const probeTimeout = 10 * time.Second

// ProbeAuthorizeList 用已配置凭证对 /api/open/user/authorize/list 发起一次探活请求。
//
// 签名与出站 client.do 同一套官方四段逗号 md5（appKey,bodyMd5,timestamp,appSecret），
// query 为 appid/timestamp/sign。本函数只做一次性请求，不维护任何连接状态；
// 网络行为完全由注入的 httpClient 决定（生产注入默认客户端，单测注入 httptest）。
func ProbeAuthorizeList(ctx context.Context, httpClient *http.Client, baseURL, appKey, appSecret string) error {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = defaultBaseURL
	}
	if appKey == "" || appSecret == "" {
		return fmt.Errorf("xianguanjia probe: app key/secret not configured")
	}

	body := []byte("{}")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sign := Sign(appKey, BodyMd5(body), ts, appSecret)
	query := url.Values{}
	query.Set("appid", appKey)
	query.Set("timestamp", ts)
	query.Set("sign", sign)

	fullURL := strings.TrimRight(base, "/") + pathAuthorizeList + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("xianguanjia probe build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json;charset=utf-8")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("xianguanjia probe request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("xianguanjia probe read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("xianguanjia probe returned status %d: %s", resp.StatusCode, string(respBody))
	}
	if _, err := decodeEnvelope(respBody); err != nil {
		return fmt.Errorf("xianguanjia probe envelope: %w", err)
	}
	return nil
}
