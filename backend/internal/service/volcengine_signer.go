package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// 火山引擎（Volcengine）OpenAPI 网关（open.volcengineapi.com）的 SignatureV4 签名。
//
// 火山签名与 AWS SigV4 有三处关键差异，必须严格对齐官方 SDK
// （volcengine/auth/SignerV4.py 的 sign 流程）：
//  1. signing key 推导不加 "AWS4" 前缀：kdate = HMAC(sk, date)，而非 HMAC("AWS4"+sk, date)。
//  2. 请求体哈希写入 X-Content-Sha256 头，且 signed_headers 包含该头。
//  3. 时间头为 X-Date（%Y%m%dT%H%M%SZ），signed_headers 涵盖 host 与所有 X-* 头。
//
// 用于方舟 Agent/Coding Plan 管理 API（GetAFPUsage / GetCodingPlanUsage）的探测签名，
// 与账号数据面推理 key（ark-*）完全无关。当前仅支持 GET（查询串携带 Action/Version）。

// volcEngineSignQuery 对 GET 查询参数做火山 SignatureV4 签名，返回：
//   - queryString：规范化并按规则编码后的查询串（用于拼接 URL）
//   - headers：待随请求发送的签名头（Host / X-Date / X-Content-Sha256 / Authorization）
func volcEngineSignQuery(ak, sk, region, service, host string, query url.Values, now time.Time) (string, http.Header, error) {
	// X-Date 官方格式 yyyyMMdd'T'HHmmss'Z'（UTC）。
	formatDate := now.UTC().Format("20060102T150405Z")
	date := formatDate[:8]

	payloadHash := sha256Hex(nil)
	canonQuery := volcCanonicalQuery(query)

	// signed_headers：{Content-Type, Content-Md5, Host} ∪ 所有 X-* 头，小写排序。
	// GET 仅为 host + x-content-sha256 + x-date。
	signed := []string{"host", "x-content-sha256", "x-date"}
	sort.Strings(signed)
	signedHeaders := strings.Join(signed, ";")

	// canonical request：method \n path \n query \n 头块 \n signed_headers \n payload_hash。
	var headerLines strings.Builder
	for _, name := range signed {
		value := ""
		switch name {
		case "host":
			value = host
		case "x-content-sha256":
			value = payloadHash
		case "x-date":
			value = formatDate
		}
		headerLines.WriteString(name + ":" + value + "\n")
	}
	// 官方 '\n'.join([method, path, query, signed_str, signed_headers, payload_hash])：
	// signed_str 尾部带 '\n'，join 再补一个 '\n'，故头块与 signed_headers 之间会多出一个空行。
	canonicalRequest := "GET\n/\n" + canonQuery + "\n" + headerLines.String() +
		"\n" + signedHeaders + "\n" + payloadHash

	scope := date + "/" + region + "/" + service + "/request"
	stringToSign := "HMAC-SHA256\n" + formatDate + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))

	kDate := hmacSHA256([]byte(sk), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("request")) // 无 "AWS4" 前缀
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	authorization := "HMAC-SHA256 Credential=" + ak + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature

	headers := make(http.Header)
	headers.Set("Host", host)
	headers.Set("X-Date", formatDate)
	headers.Set("X-Content-Sha256", payloadHash)
	headers.Set("Authorization", authorization)
	return canonQuery, headers, nil
}

// volcCanonicalQuery 规范化查询串：按键排序，键值按 url.QueryEscape
// （保留字母数字与 -_.~，等价官方 quote(..., safe='-_.~')）编码，以 & 相连。
func volcCanonicalQuery(query url.Values) string {
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		for _, v := range query[k] {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(data)
	return h.Sum(nil)
}