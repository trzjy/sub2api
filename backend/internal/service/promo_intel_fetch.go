package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
)

// 优惠情报抓取层：GET 无凭证抓取 + 正文提取 + 内容指纹。
//
// 安全语义（对齐 volcano_plan_docs.go 的既有先例）：
//   - 专用客户端：无任何账号凭证、固定超时、响应体上限；
//   - 仅允许 http(s) 外网地址，拒绝私有地址解析（SSRF 防护复用 httpclient 池）；
//   - 任一失败即该源本次抓取失败，绝不静默降级为旧内容。

const promoIntelUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// promoIntelFetchResult 单次页面抓取结果。
type promoIntelFetchResult struct {
	Body     []byte // 原始响应体（已限长）
	FinalURL string // 重定向后的最终 URL
}

// fetchPromoIntelPage 抓取资讯源页面：GET、UA、限长、超时。
// 客户端由 s.promoIntelHTTPClient() 提供（测试可注入）。
func (s *PromoIntelService) fetchPromoIntelPage(ctx context.Context, rawURL string) (*promoIntelFetchResult, error) {
	if s == nil {
		return nil, fmt.Errorf("promo intel service is nil")
	}
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, fmt.Errorf("empty source url")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("source url must be absolute http(s): %q", trimmed)
	}

	client, err := s.promoIntelHTTPClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("build promo intel http client: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trimmed, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", promoIntelUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("unexpected HTTP %d for %s", resp.StatusCode, trimmed)
	}
	maxBytes := s.fetchMaxBytes()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		// 超限截断而不是失败：情报页大多远小于上限，截断只影响尾部内容。
		body = body[:maxBytes]
	}
	if !utf8.Valid(body) {
		// 非UTF-8（GBK 等）不强行转码：LLM 整理层对乱码会判无关，正文提取仍可用 ASCII 部分。
		return nil, fmt.Errorf("response body is not valid utf-8 for %s", trimmed)
	}
	return &promoIntelFetchResult{Body: body, FinalURL: resp.Request.URL.String()}, nil
}

// fetchMaxBytes 返回响应体上限（配置化，默认 2MB）。
func (s *PromoIntelService) fetchMaxBytes() int64 {
	if s != nil && s.cfg != nil && s.cfg.FetchMaxBytes > 0 {
		return int64(s.cfg.FetchMaxBytes)
	}
	return PromoIntelDefaultFetchMaxBytes
}

// extractPromoIntelText 从 HTML/JSON/纯文本响应中提取可供指纹与 LLM 阅读的正文：
// 剥离 script/style/noscript 与注释 → 剥标签 → 常见 HTML 实体解码 → 空白折叠。
// 不做任何站点特定解析，保持通用；提取失败（空文本）返回错误。
func extractPromoIntelText(body []byte) (string, error) {
	text := string(body)
	if text == "" {
		return "", fmt.Errorf("empty body")
	}

	// 剥离不应参与指纹的动态/不可见块（script 内的构建哈希变化会造成假阳性）。
	for _, tag := range []string{"script", "style", "noscript", "svg", "iframe"} {
		text = removePromoIntelTagBlocks(text, tag)
	}
	// HTML 注释与 head 元信息。
	text = removePromoIntelTagBlocks(text, "head")

	var b strings.Builder
	b.Grow(len(text))
	inTag := false
	for _, r := range text {
		switch {
		case r == '<':
			inTag = true
			b.WriteRune(' ')
		case r == '>':
			inTag = false
			b.WriteRune(' ')
		case inTag:
			// 标签内内容丢弃
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()

	out = decodePromoIntelEntities(out)
	// NUL 字节对指纹无意义且 PG 不收，直接剔除。
	out = strings.ReplaceAll(out, "\x00", " ")
	// 空白折叠：换行/制表压成单空格，保证指纹对排版抖动稳定。
	out = strings.Join(strings.Fields(out), " ")
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("no extractable text")
	}
	return out, nil
}

// removePromoIntelTagBlocks 删除 <tag ...>...</tag> 整块（大小写不敏感、贪婪到闭合标签；
// 无闭合标签时删除自开标签到串尾，避免残留脚本内容进入指纹）。
func removePromoIntelTagBlocks(text, tag string) string {
	open := "<" + tag
	var out strings.Builder
	lower := strings.ToLower(text)
	for {
		idx := strings.Index(lower, open)
		if idx < 0 {
			out.WriteString(text)
			return out.String()
		}
		out.WriteString(text[:idx])
		closeTag := "</" + tag
		closeIdx := strings.Index(lower[idx:], closeTag)
		if closeIdx < 0 {
			// 无闭合：丢弃剩余全部（防御残缺 HTML）。
			return out.String()
		}
		end := idx + closeIdx + len(closeTag)
		gt := strings.Index(text[end:], ">")
		if gt >= 0 {
			end += gt + 1
		}
		// 占位空格：被剥块处保留一个分隔空格。
		out.WriteString(" ")
		text = text[end:]
		lower = lower[end:]
	}
}

// decodePromoIntelEntities 解码常见命名实体与数字实体（不引入依赖）。
func decodePromoIntelEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	replacer := strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", "\"", "&#39;", "'", "&apos;", "'", "&ldquo;", "“", "&rdquo;", "”",
		"&mdash;", "—", "&hellip;", "…", "&copy;", "©", "&middot;", "·",
	)
	out := replacer.Replace(s)
	// 数字实体 &#123; / &#x1F600;。任何无法解析的 "&#" 一律消费掉 '&' 本身再继续，
	// 保证循环必然前进（否则形如 "&#abc&#65;" 的输入会死循环）。
	for {
		start := strings.Index(out, "&#")
		if start < 0 {
			return out
		}
		end := strings.Index(out[start:], ";")
		if end < 0 || end > 8 {
			out = out[:start] + out[start+1:]
			continue
		}
		token := out[start+2 : start+end]
		var r rune
		ok := false
		if strings.HasPrefix(token, "x") || strings.HasPrefix(token, "X") {
			var v int64
			for _, c := range token[1:] {
				v *= 16
				switch {
				case c >= '0' && c <= '9':
					v += int64(c - '0')
				case c >= 'a' && c <= 'f':
					v += int64(c-'a') + 10
				case c >= 'A' && c <= 'F':
					v += int64(c-'A') + 10
				default:
					v = -1
				}
				if v < 0 {
					break
				}
			}
			if v >= 0 && v <= 0x10FFFF {
				r, ok = rune(v), true
			}
		} else {
			var v int64
			for _, c := range token {
				if c < '0' || c > '9' {
					v = -1
					break
				}
				v = v*10 + int64(c-'0')
			}
			if v >= 0 && v <= 0x10FFFF {
				r, ok = rune(v), true
			}
		}
		switch {
		case ok && r != 0 && utf8.ValidRune(r):
			out = out[:start] + string(r) + out[start+end+1:]
		case ok:
			// NUL（&#0;）即使合法 UTF-8 也会被 PG 拒绝：整体替换为空格，
			// 不留 "#0;" 残渣。
			out = out[:start] + " " + out[start+end+1:]
		default:
			out = out[:start] + out[start+1:]
		}
	}
}

// promoIntelContentHash 计算正文内容指纹。
func promoIntelContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// promoIntelRawFingerprint 降级原文条目的去重指纹：同一源同一内容版本只占一行。
func promoIntelRawFingerprint(sourceName, contentHash string) string {
	sum := sha256.Sum256([]byte("raw|" + sourceName + "|" + contentHash))
	return hex.EncodeToString(sum[:])
}

// promoIntelOfferFingerprint LLM 情报条目的去重指纹：厂商 + 归一化标题。
// 归一化：小写、去空白与全半角标点差异，保证跨源重复发现可合并。
func promoIntelOfferFingerprint(vendor, title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch r {
		case ' ', '\t', '，', '。', '、', '：', '；', '！', '？', '"', '\'', '(', ')', '（', '）', ',', '.', ':', ';', '!', '?', '-', '—', '/':
			continue
		}
		b.WriteRune(r)
	}
	sum := sha256.Sum256([]byte("offer|" + vendor + "|" + b.String()))
	return hex.EncodeToString(sum[:])
}

// promoIntelHTTPClient 懒构造共享抓取客户端（无凭证、SSRF 校验、固定超时）。
func (s *PromoIntelService) promoIntelHTTPClient(ctx context.Context) (*http.Client, error) {
	if s.testFetchClient != nil {
		return s.testFetchClient, nil
	}
	timeout := PromoIntelDefaultFetchTimeoutSec * time.Second
	if s.cfg != nil && s.cfg.FetchTimeoutSeconds > 0 {
		timeout = time.Duration(s.cfg.FetchTimeoutSeconds) * time.Second
	}
	return httpclient.GetClient(httpclient.Options{
		Timeout:            timeout,
		ValidateResolvedIP: true,
		// 管理员配置的是厂商公网页面；默认拒绝私网地址。
		AllowPrivateHosts: false,
	})
}

// capPromoIntelRunes 按 rune 截断文本，送入 LLM 的正文上限。
func capPromoIntelRunes(text string, max int) string {
	if max <= 0 || len(text) <= max {
		return text
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}
