package service

// 本文件实现 cursor 平台出站的**号池中转节点发现**（E1 卡，方案 §2 E1）。
//
// 取证结论（~/.sub2api-acceptance/cursor-20261013/02-webcrack-out/deobfuscated.js）：
//   - :7883-7884 静态中转节点 http1=https://cur-api2.shareai.pro / http2=https://cur-api5.shareai.pro
//   - :7887-7958 fetchRoute2Addresses() 动态发现备用节点：
//       GET https://cur-api-1301225960.cos.ap-shanghai.myqcloud.com/add.csv
//       → 读响应头 x-cos-meta-id
//       → base36 变长解码（见下方 decodeCursorBase36VarLen）
//       → JSON.parse → 取 http1（兼容 http1 / "http1.1" / http11 三种键名）与 http2
//
// 降级语义（本文件唯一的降级点）：动态发现失败时回落静态节点。这属于**运维可用性**
// 降级（换一组等价的中转节点继续出站），与"协议解析失败不降级"的正确性红线不冲突。
// 站点侧永不因节点发现问题降级到 Cursor 官方端点。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// cursorStaticHTTP1Base / cursorStaticHTTP2Base 是插件硬编码的静态兜底节点。
	cursorStaticHTTP1Base = "https://cur-api2.shareai.pro"
	cursorStaticHTTP2Base = "https://cur-api5.shareai.pro"

	// cursorRoute2Endpoint 是备用节点下发端点（腾讯云 COS 上的 add.csv）。
	cursorRoute2Endpoint = "https://cur-api-1301225960.cos.ap-shanghai.myqcloud.com/add.csv"

	// cursorRoute2MetaHeader 是承载 base36 变长编码节点表的响应头。
	cursorRoute2MetaHeader = "x-cos-meta-id"

	// cursorNodesCacheTTL 是动态节点缓存时长（对齐插件 20s 轮询量级，取更宽松的 10 分钟，
	// 避免每次对话都去打 COS；节点是"备用线路"，短时沿用旧值不影响正确性）。
	cursorNodesCacheTTL = 10 * time.Minute

	// cursorNodesFetchTimeout 是节点发现单次请求超时（插件用 10s）。
	cursorNodesFetchTimeout = 10 * time.Second

	// cursorNodeMetaMaxBytes 限制 base36 元数据长度上限，防止异常响应打爆内存。
	cursorNodeMetaMaxBytes = 64 << 10
)

// errCursorNodeMetaMissing 表示响应缺少 x-cos-meta-id 头。
var errCursorNodeMetaMissing = errors.New("cursor: route2 response missing x-cos-meta-id header")

// errCursorNodeFieldsIncomplete 表示解码后的 JSON 缺少 http1/http2 字段。
var errCursorNodeFieldsIncomplete = errors.New("cursor: route2 payload missing http1/http2 fields")

// CursorUpstreamNodes 是一组号池中转节点。
//
// HTTP1Base 承载 HTTP/1.1 出站，HTTP2Base 承载 HTTP/2 出站（插件语义：
// route2 生效时 http1 走 https://cur-api2、http2 走 https://cur-api5）。
type CursorUpstreamNodes struct {
	HTTP1Base string
	HTTP2Base string
	// Dynamic 为 true 表示来自动态发现（备用线路），false 表示静态兜底节点。
	Dynamic bool
}

// CursorUpstreamNodesOption 用于测试或自定义装配节点发现器。
type CursorUpstreamNodesOption func(*CursorUpstreamNodesResolver)

// WithCursorNodesEndpoint 覆盖备用节点下发端点（测试用）。
func WithCursorNodesEndpoint(endpoint string) CursorUpstreamNodesOption {
	return func(r *CursorUpstreamNodesResolver) {
		if endpoint != "" {
			r.endpoint = endpoint
		}
	}
}

// WithCursorNodesHTTPClient 覆盖 HTTP 客户端（测试用，可注入短超时或 fake transport）。
func WithCursorNodesHTTPClient(hc *http.Client) CursorUpstreamNodesOption {
	return func(r *CursorUpstreamNodesResolver) {
		if hc != nil {
			r.httpClient = hc
		}
	}
}

// WithCursorNodesCacheTTL 覆盖缓存时长（测试用，设 0 关闭缓存）。
func WithCursorNodesCacheTTL(ttl time.Duration) CursorUpstreamNodesOption {
	return func(r *CursorUpstreamNodesResolver) {
		r.cacheTTL = ttl
	}
}

// CursorUpstreamNodesResolver 发现并缓存 cursor 号池中转节点。
//
// 并发：Resolve 内的动态发现用 singleflight 语义的互斥 + 二次检查，避免并发请求
// 同时打 COS；缓存读用 RWMutex 保护。
type CursorUpstreamNodesResolver struct {
	endpoint   string
	httpClient *http.Client
	cacheTTL   time.Duration

	mu       sync.RWMutex
	cached   CursorUpstreamNodes
	cachedAt time.Time

	// fetchMu 串行化动态发现（缓存未命中时生效）。
	fetchMu sync.Mutex
}

// NewCursorUpstreamNodesResolver 构造节点发现器。
func NewCursorUpstreamNodesResolver(opts ...CursorUpstreamNodesOption) *CursorUpstreamNodesResolver {
	r := &CursorUpstreamNodesResolver{
		endpoint:   cursorRoute2Endpoint,
		httpClient: &http.Client{Timeout: cursorNodesFetchTimeout},
		cacheTTL:   cursorNodesCacheTTL,
		cached:     staticCursorUpstreamNodes(),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// staticCursorUpstreamNodes 返回静态兜底节点。
func staticCursorUpstreamNodes() CursorUpstreamNodes {
	return CursorUpstreamNodes{
		HTTP1Base: cursorStaticHTTP1Base,
		HTTP2Base: cursorStaticHTTP2Base,
		Dynamic:   false,
	}
}

// StaticCursorUpstreamNodes 返回插件硬编码的静态兜底节点（不触网络）。
// 供 E2 卡在拿不到 resolver 实例时兜底取 base。
func StaticCursorUpstreamNodes() CursorUpstreamNodes {
	return staticCursorUpstreamNodes()
}

// Resolve 返回当前应使用的号池节点：缓存命中直接返回；未命中则动态发现，
// 失败降级到静态节点（不返回错误——节点发现的失败是可用性问题，不是正确性问题）。
func (r *CursorUpstreamNodesResolver) Resolve(ctx context.Context) CursorUpstreamNodes {
	if n, ok := r.fromCache(); ok {
		return n
	}

	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()

	// 二次检查：等锁期间可能已被其它 goroutine 刷新。
	if n, ok := r.fromCache(); ok {
		return n
	}

	n, err := r.fetchDynamic(ctx)
	if err != nil || n.HTTP1Base == "" || n.HTTP2Base == "" {
		// 降级：动态失败 → 静态兜底节点。这是本文件唯一的降级点。
		n = staticCursorUpstreamNodes()
	}
	r.storeCache(n)
	return n
}

// fromCache 读取未过期的缓存节点。
func (r *CursorUpstreamNodesResolver) fromCache() (CursorUpstreamNodes, bool) {
	if r.cacheTTL <= 0 {
		return CursorUpstreamNodes{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cached.HTTP1Base == "" || r.cached.HTTP2Base == "" {
		return CursorUpstreamNodes{}, false
	}
	if time.Since(r.cachedAt) > r.cacheTTL {
		return CursorUpstreamNodes{}, false
	}
	return r.cached, true
}

// storeCache 写入缓存。
func (r *CursorUpstreamNodesResolver) storeCache(n CursorUpstreamNodes) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cached = n
	r.cachedAt = time.Now()
}

// fetchDynamic 按插件同款流程拉取备用节点：add.csv → x-cos-meta-id → base36 解码 → JSON。
func (r *CursorUpstreamNodesResolver) fetchDynamic(ctx context.Context) (CursorUpstreamNodes, error) {
	ctx, cancel := context.WithTimeout(ctx, cursorNodesFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.endpoint, nil)
	if err != nil {
		return CursorUpstreamNodes{}, fmt.Errorf("cursor: build route2 request: %w", err)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return CursorUpstreamNodes{}, fmt.Errorf("cursor: route2 fetch: %w", err)
	}
	defer resp.Body.Close()

	// 插件对 route2 端点接受 2xx 与 3xx（validateStatus: 200<=s<400）。
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return CursorUpstreamNodes{}, fmt.Errorf("cursor: route2 status %d", resp.StatusCode)
	}

	// 节点信息在响应头里；body 仍需读取少量以复用连接，但内容不参与解析。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	meta := resp.Header.Get(cursorRoute2MetaHeader)
	if strings.TrimSpace(meta) == "" {
		return CursorUpstreamNodes{}, errCursorNodeMetaMissing
	}
	if len(meta) > cursorNodeMetaMaxBytes {
		return CursorUpstreamNodes{}, fmt.Errorf("cursor: route2 meta too large: %d bytes", len(meta))
	}

	decoded, err := decodeCursorBase36VarLen(meta)
	if err != nil {
		return CursorUpstreamNodes{}, fmt.Errorf("cursor: route2 decode: %w", err)
	}

	var payload struct {
		HTTP1    string `json:"http1"`
		HTTP11   string `json:"http1.1"`
		HTTP1Alt string `json:"http11"`
		HTTP2    string `json:"http2"`
	}
	if err := json.Unmarshal([]byte(decoded), &payload); err != nil {
		return CursorUpstreamNodes{}, fmt.Errorf("cursor: route2 json: %w", err)
	}

	// 键名兼容：http1 / "http1.1" / http11（插件 :7946 的三段 ?? 链）。
	http1 := payload.HTTP1
	if http1 == "" {
		http1 = payload.HTTP11
	}
	if http1 == "" {
		http1 = payload.HTTP1Alt
	}
	http2 := payload.HTTP2

	if http1 == "" || http2 == "" {
		return CursorUpstreamNodes{}, errCursorNodeFieldsIncomplete
	}

	return CursorUpstreamNodes{
		HTTP1Base: strings.TrimRight(http1, "/"),
		HTTP2Base: strings.TrimRight(http2, "/"),
		Dynamic:   true,
	}, nil
}

// cursorBase36Alphabet 是 base36 字母表（插件 :7911 的 indexOf 目标串）。
const cursorBase36Alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// decodeCursorBase36VarLen 逐字复刻插件的 base36 变长解码（:7930-7949）。
//
// 格式： "<字节长度十进制>-<base36 载荷>"。载荷按大端 base36 累乘展开成
// 长度个字节：每个字符 v 从最高位字节开始做 *36+v，进位用 >>>8 传递；
// 展开结束后若仍有进位则说明声明长度装不下载荷（报错）。
func decodeCursorBase36VarLen(s string) (string, error) {
	sep := strings.Index(s, "-")
	if sep <= 0 {
		return "", errors.New("cursor: invalid base36 var-len format")
	}

	length, err := strconv.Atoi(s[:sep])
	if err != nil {
		return "", fmt.Errorf("cursor: invalid declared length: %w", err)
	}
	if length < 0 {
		return "", errors.New("cursor: negative declared length")
	}

	payload := s[sep+1:]
	buf := make([]byte, length)
	// 载荷先转小写再逐字符展开（插件 toLowerCase 后按小写字母表 indexOf）。
	for _, ch := range strings.ToLower(payload) {
		digit := strings.IndexRune(cursorBase36Alphabet, ch)
		if digit < 0 {
			return "", fmt.Errorf("cursor: invalid base36 character: %q", ch)
		}
		carry := int64(digit)
		for i := length - 1; i >= 0; i-- {
			cur := int64(buf[i])*36 + carry
			buf[i] = byte(cur & 255)
			carry = cur >> 8
		}
		if carry != 0 {
			return "", errors.New("cursor: encoded value exceeds declared length")
		}
	}

	return string(buf), nil
}
