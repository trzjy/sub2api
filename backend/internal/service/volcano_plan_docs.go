package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// 火山方舟官方文档读取器。
//
// 火山订阅号（Coding/Agent Plan）的“支持模型”唯一事实源是火山引擎官方文档中心
// （docs.volcengine.com）。本文件用官方结构化 API 读取“订阅 [Agent/Coding Plan]”
// 目录树与“套餐概览”的 Markdown 正文（MDContent），解析“支持模型及 Harness”/
// “支持的模型”表/散文，得到可直接调用的模型候选名单，再交由探活分类。
//
// 安全语义（只进固定官方域名）：
//   - 专用无凭证客户端，仅允许 https://docs.volcengine.com，可用官方 lib-axios
//     同源 API：getDocList / getDocDetail。
//   - 不携带账号 API Key、不使用账号代理、不跨主机跳转、固定超时与响应体上限。
//   - 任一获取/解析失败失败关闭，绝不回落静态清单、绝不删除既有已确认模型。
//
// 不对官方文档运行 HTML 抓取，也不用 LLM 解析：只解析官方结构化目录与 Markdown。

const (
	volcanoDocBaseURL       = "https://docs.volcengine.com"
	volcanoDocHost          = "docs.volcengine.com"
	volcanoDocLibraryID     = "82379" // 火山方舟（Volcengine Ark）官方文档库
	volcanoDocGetListPath   = "/api/doc/getDocList"
	volcanoDocGetDetailPath = "/api/doc/getDocDetail"
	volcanoDocTimeout       = 15 * time.Second
	volcanoDocMaxBytes      = 8 << 20
	volcanoDocDialTimeout   = 10 * time.Second

	// volcanoDocSubscriptionRoot 是“订阅 [Agent/Coding Plan]”目录节点标题。
	volcanoDocSubscriptionRoot = "订阅 [Agent/Coding Plan]"
	// volcanoDocOverviewTitle 是“套餐概览”节点标题，定位目标。
	volcanoDocOverviewTitle = "套餐概览"
)

// volcanoDocNode 承载 getDocList 返回的单条目录节点（平铺数组）。
type volcanoDocNode struct {
	DocumentID int64  `json:"DocumentID"`
	ParentID   int64  `json:"ParentID"`
	Title      string `json:"Title"`
	Status     int    `json:"Status"`
}

// volcanoDocDetail 承载 getDocDetail 返回的单篇文档详情。
type volcanoDocDetail struct {
	DocumentID  int64  `json:"DocumentID"`
	Title       string `json:"Title"`
	MDContent   string `json:"MDContent"`
	UpdatedTime string `json:"UpdatedTime"`
}

// volcanoPlanDocRef 是某套餐（个人版/企业版）“套餐概览”文档引用。
type volcanoPlanDocRef struct {
	DocumentID  int64  `json:"document_id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	MDContent   string `json:"-"`
	UpdatedTime string `json:"updated_time"`
}

// VolcanoPlanDocEvidence 是一次同步的来源证据：官方文档 URL、DocumentID、UpdatedTime
// 与候选数，供前端与审计展示。
type VolcanoPlanDocEvidence struct {
	Kind           string   `json:"kind"`
	URLs           []string `json:"urls,omitempty"`
	DocumentIDs    []int64  `json:"document_ids,omitempty"`
	Titles         []string `json:"titles,omitempty"`
	UpdatedTimes   []string `json:"updated_times,omitempty"`
	CandidateCount int      `json:"candidate_count"`
}

// volcanoReportSet 按套餐整理个人版/企业版“套餐概览”文档。
type volcanoReportSet struct {
	Personal   *volcanoPlanDocRef
	Enterprise *volcanoPlanDocRef
}

// volcanoDocHostGuardTransport 防御性主机锁定：只允许发往 docs.volcengine.com。
// 即使上层拼错 URL，走到这里也会拒绝其它主机。
type volcanoDocHostGuardTransport struct {
	next http.RoundTripper
}

func (t *volcanoDocHostGuardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("volcano doc request has nil url")
	}
	if !strings.EqualFold(req.URL.Hostname(), volcanoDocHost) || req.URL.Scheme != "https" {
		return nil, fmt.Errorf("volcano doc request to non-official host rejected: %s", req.URL.Hostname())
	}
	return t.next.RoundTrip(req)
}

// guardVolcanoDocRedirect 拒绝任何跨主机/跨协议跳转，且对同源跳转也失败关闭
// （官方结构化 API 成功即返 200，出现跳转说明路径已变更，应失败而非跟随）。
func guardVolcanoDocRedirect(req *http.Request, via []*http.Request) error {
	// 官方结构化 API 成功即返 200；任何跳转（含同源 http→https / 路径变更）都说明
	// 权威路径已变化，一律失败关闭，绝不静默跟随改变解析目标。
	return fmt.Errorf("volcano doc redirect rejected (official API must return 200 directly): %s", req.URL.String())
}

// newVolcanoDocClient 构造专用固定文档客户端：无凭证、无代理、GET-only、主机锁定、
// 拒绝跳转、固定超时与响应体上限。复用标准库 http.Transport，仅外包主机守卫，
// 不重复实现 HTTP/TLS 栈。
func newVolcanoDocClient() *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   volcanoDocDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2: true,
		TLSHandshakeTimeout: volcanoDocDialTimeout,
	}
	return &http.Client{
		Timeout:       volcanoDocTimeout,
		Transport:     &volcanoDocHostGuardTransport{next: transport},
		CheckRedirect: guardVolcanoDocRedirect,
	}
}

// getVolcanoDocClient 返回文档客户端：测试可注入 svc.volcanoDocClient，否则懒构造一次。
func (s *AccountTestService) getVolcanoDocClient() *http.Client {
	s.volcanoDocClientOnce.Do(func() {
		if s.volcanoDocClient == nil {
			s.volcanoDocClient = newVolcanoDocClient()
		}
	})
	return s.volcanoDocClient
}

// volcanoDocBaseFor 返回文档基址：测试注入覆盖，否则固定官方域名。
func (s *AccountTestService) volcanoDocBaseFor() string {
	if s != nil && s.volcanoDocBaseURL != "" {
		return strings.TrimRight(s.volcanoDocBaseURL, "/")
	}
	return volcanoDocBaseURL
}

// volcanoDocGet 对固定官方域名执行 GET，校验 2xx 并限长读取响应体。
func volcanoDocGet(ctx context.Context, client *http.Client, baseURL, path string, query url.Values) ([]byte, error) {
	if client == nil {
		return nil, errors.New("volcano doc client is nil")
	}
	full := baseURL + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, fmt.Errorf("build volcano doc request: %w", err)
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", volcanoDocBaseURL+"/docs")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("volcano doc request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("volcano doc request returned HTTP %d for %s", resp.StatusCode, path)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, volcanoDocMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read volcano doc response: %w", err)
	}
	if int64(len(body)) > volcanoDocMaxBytes {
		return nil, fmt.Errorf("volcano doc response exceeds %d bytes", volcanoDocMaxBytes)
	}
	return body, nil
}

// fetchVolcanoDocTree 拉取 82379 库目录树（平铺节点数组，含 ParentID）。
func (s *AccountTestService) fetchVolcanoDocTree(ctx context.Context) ([]volcanoDocNode, error) {
	query := url.Values{}
	query.Set("LibraryID", volcanoDocLibraryID)
	body, err := volcanoDocGet(ctx, s.getVolcanoDocClient(), s.volcanoDocBaseFor(), volcanoDocGetListPath, query)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Result []volcanoDocNode `json:"Result"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse volcano doc list: %w", err)
	}
	if len(payload.Result) == 0 {
		return nil, errors.New("volcano doc list is empty")
	}
	return payload.Result, nil
}

// fetchVolcanoDocDetail 拉取单篇文档详情（MDContent + UpdatedTime）。
func (s *AccountTestService) fetchVolcanoDocDetail(ctx context.Context, documentID int64) (volcanoDocDetail, error) {
	query := url.Values{}
	query.Set("DocumentID", fmt.Sprintf("%d", documentID))
	body, err := volcanoDocGet(ctx, s.getVolcanoDocClient(), s.volcanoDocBaseFor(), volcanoDocGetDetailPath, query)
	if err != nil {
		return volcanoDocDetail{}, err
	}
	var payload struct {
		Result volcanoDocDetail `json:"Result"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return volcanoDocDetail{}, fmt.Errorf("parse volcano doc detail %d: %w", documentID, err)
	}
	if payload.Result.DocumentID == 0 || strings.TrimSpace(payload.Result.MDContent) == "" {
		return volcanoDocDetail{}, fmt.Errorf("volcano doc detail %d returned empty content", documentID)
	}
	return payload.Result, nil
}

// volcanoDocEdition 标识“套餐概览”属于哪个订阅及其版本。
type volcanoDocEdition struct {
	Kind     string // volcanoPlanKindAgent | volcanoPlanKindCoding
	Personal bool
	DocumentID int64
	Title     string
}

// locateVolcanoOverviewDocs 在目录树中定位“订阅 [Agent/Coding Plan]”下的
// “套餐概览”节点，并按祖先标题归类到 agent/coding × personal/business。
func locateVolcanoOverviewDocs(nodes []volcanoDocNode) ([]volcanoDocEdition, error) {
	byID := make(map[int64]volcanoDocNode, len(nodes))
	for _, n := range nodes {
		byID[n.DocumentID] = n
	}
	// 祖先标题链（不含自身，根在前）。
	ancestors := func(n volcanoDocNode) []string {
		var chain []string
		cur := byID[n.ParentID]
		for i := 0; i < 64 && cur.DocumentID != 0; i++ {
			chain = append(chain, cur.Title)
			next, ok := byID[cur.ParentID]
			if !ok || next.DocumentID == 0 {
				break
			}
			cur = next
		}
		return chain
	}
	var out []volcanoDocEdition
	for _, n := range nodes {
		if strings.TrimSpace(n.Title) != volcanoDocOverviewTitle {
			continue
		}
		path := ancestors(n)
		if !containsVolcanoTitle(path, volcanoDocSubscriptionRoot) {
			continue
		}
		kind, ok := classifyVolcanoAncestors(path)
		if !ok {
			continue
		}
		out = append(out, volcanoDocEdition{
			Kind:       kind.Kind,
			Personal:   kind.Personal,
			DocumentID: n.DocumentID,
			Title:      n.Title,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("volcano official docs: no 套餐概览 located under 订阅 [Agent/Coding Plan]")
	}
	return out, nil
}

// volcanoKindEdition 是由套餐概览祖先链判得的订阅与版本。
type volcanoKindEdition struct {
	Kind     string
	Personal bool
}

// classifyVolcanoAncestors 由套餐概览的祖先标题链判得订阅（agent/coding）与版本（个人/企业）。
func classifyVolcanoAncestors(path []string) (volcanoKindEdition, bool) {
	kind := ""
	personal := false
	hasKind, hasEdition := false, false
	for _, t := range path {
		switch {
		case strings.Contains(t, "Agent Plan"):
			kind, hasKind = volcanoPlanKindAgent, true
		case strings.Contains(t, "Coding Plan"):
			kind, hasKind = volcanoPlanKindCoding, true
		}
		if strings.Contains(t, "个人版") {
			personal, hasEdition = true, true
		} else if strings.Contains(t, "企业版") {
			personal, hasEdition = false, true
		}
		if hasKind && hasEdition {
			return volcanoKindEdition{Kind: kind, Personal: personal}, true
		}
	}
	return volcanoKindEdition{}, false
}

func containsVolcanoTitle(path []string, title string) bool {
	for _, t := range path {
		if strings.TrimSpace(t) == title {
			return true
		}
	}
	return false
}

// volcanoIndex 是某订阅（agent/coding）下个人版+企业版“套餐概览”的文档集合。
type volcanoIndex struct {
	Kind     volcanoDocKind
	Personal *volcanoPlanDocRef
	Enterprise *volcanoPlanDocRef
}

type volcanoDocKind int

const (
	volcanoAgent volcanoDocKind = iota
	volcanoCoding
)

func (k volcanoDocKind) String() string {
	if k == volcanoCoding {
		return volcanoPlanKindCoding
	}
	return volcanoPlanKindAgent
}

// fetchVolcanoPlanReports 拉取目录树→定位套餐概览→读取与 kind（agent/coding）同类的
// MDContent，返回该订阅下的个人版+企业版文档引用。
// 只处理请求订阅的“套餐概览”：Agent 与 Coding 独立同步，无关订阅的文档获取失败不得
// 让本订阅同步失败（跨 plan 去耦，P2-3）。
func (s *AccountTestService) fetchVolcanoPlanReports(ctx context.Context, kind volcanoDocKind) (*volcanoIndex, error) {
	nodes, err := s.fetchVolcanoDocTree(ctx)
	if err != nil {
		return nil, err
	}
	editions, err := locateVolcanoOverviewDocs(nodes)
	if err != nil {
		return nil, err
	}
	kindName := kind.String()
	entry := &volcanoIndex{Kind: kind}
	found := 0
	for _, e := range editions {
		if e.Kind != kindName {
			continue
		}
		found++
		detail, err := s.fetchVolcanoDocDetail(ctx, e.DocumentID)
		if err != nil {
			return nil, err
		}
		ref := &volcanoPlanDocRef{
			DocumentID:  e.DocumentID,
			Title:       detail.Title,
			URL:         fmt.Sprintf("https://docs.volcengine.com/docs/%s/%d", volcanoDocLibraryID, e.DocumentID),
			MDContent:   detail.MDContent,
			UpdatedTime: detail.UpdatedTime,
		}
		if e.Personal {
			entry.Personal = ref
		} else {
			entry.Enterprise = ref
		}
	}
	// 个人版+企业版必须双双存在：任一缺失即视为文档包不完整，失败关闭，绝不把单版
	// 当并集——否则可能因被去掉的那一版独有模型被误判“官方下线”而删除已收录模型
	// （P1-2）。
	if entry.Personal == nil || entry.Enterprise == nil {
		return nil, fmt.Errorf("volcano docs: %s Plan 套餐概览 must be present in both 个人版 and 企业版 (found %d overview(s)); refusing incomplete union",
			kindName, found)
	}
	return entry, nil
}

// extractVolcanoPlanReportModels 解析单篇“套餐概览”的 MDContent，取可直调文本模型 ID。
// 同时处理表形态（“支持模型及 Harness” 的 Agent 表 / “支持的模型” 的 Coding 表）
// 与散文形态（Coding 企业版枚举）。结构无法识别时失败关闭（返回错误，不删既有）。
func extractVolcanoPlanReportModels(md string) ([]string, error) {
	if strings.TrimSpace(md) == "" {
		return nil, errors.New("volcano doc: empty markdown")
	}
	var base []string
	// 定位“支持模型”类标题所在节。
	sectionIdx := findVolcanoModelSection(md)
	if sectionIdx < 0 {
		return nil, errors.New("volcano doc: 支持模型/支持模型及 Harness section not found")
	}
	section, ok := volcanoModelSectionAt(md, sectionIdx)
	if !ok {
		return nil, errors.New("volcano doc: cannot slice 支持模型 section")
	}

	// 优先识别表格：表头含“模型名称”或“模型”。
	if cells := parseVolcanoModelTable(section); cells != nil {
		base = cells
	} else if cells := parseVolcanoModelProse(section); cells != nil {
		base = cells
	} else {
		return nil, errors.New("volcano doc: 支持模型 section is not a recognizable table or enumeration")
	}

	normalized, err := normalizeVolcanoModelIDs(base)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

// findVolcanoModelSection 返回“支持模型”类标题所在行下标，找不到返回 -1。
func findVolcanoModelSection(md string) int {
	for i, ln := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(ln)
		if len(trimmed) < 2 || !strings.HasPrefix(trimmed, "#") {
			continue
		}
		// 去掉标题标记
		body := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		if strings.Contains(body, "支持模型及Harness") ||
			strings.Contains(body, "支持模型及 Harness") ||
			strings.Contains(body, "支持的模型") ||
			strings.Contains(body, "支持模型") {
			return i
		}
	}
	return -1
}

// volcanoModelSectionAt 截取某标题下标起、到下一同级或更高级标题前的 Markdown 节文本。
func volcanoModelSectionAt(md string, start int) (string, bool) {
	lines := strings.Split(md, "\n")
	if start < 0 || start >= len(lines) {
		return "", false
	}
	level := len([]rune(strings.TrimSpace(lines[start]))) - len([]rune(strings.TrimLeft(strings.TrimSpace(lines[start]), "#")))
	var out []string
	for i := start; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if i > start && strings.HasPrefix(trimmed, "#") {
			l := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
			if l <= level {
				break
			}
		}
		out = append(out, lines[i])
	}
	res := strings.TrimSpace(strings.Join(out, "\n"))
	if res == "" {
		return "", false
	}
	return res, true
}

// volcanoHeaderOf 返回 markdown 表头单元格列表（trim）。
func volcanoHeaderOf(ln string) []string {
	s := strings.TrimSpace(ln)
	if !strings.HasPrefix(s, "|") {
		return nil
	}
	s = strings.TrimPrefix(s, "|")
	parts := strings.Split(s, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}

// parseVolcanoModelTable 尝试把节解析为 markdown 表，返回“模型名称/模型”列原始值。
// 对 Agent 表只取“分类==文本生成”的行；无法识别表结构返回 nil。
func parseVolcanoModelTable(section string) []string {
	lines := strings.Split(section, "\n")
	tableStart := -1
	hdrCells := []string{}
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		// 下一个非空行若是分隔行 |----|，则这行是表头
		if i+1 < len(lines) && isVolcanoTableSeparator(strings.TrimSpace(lines[i+1])) {
			hdrCells = volcanoHeaderOf(trimmed)
			tableStart = i
			break
		}
	}
	if tableStart < 0 || len(hdrCells) == 0 {
		return nil
	}
	// 找“模型名称/模型”“分类”“领域”列。
	modelCol, categoryCol, domainCol := -1, -1, -1
	for ci, cell := range hdrCells {
		switch {
		case strings.Contains(cell, "模型名称"), strings.Contains(cell, "模型名"):
			modelCol = ci
		case cell == "分类" || strings.Contains(cell, "分类"):
			if categoryCol < 0 {
				categoryCol = ci
			}
		case cell == "领域":
			if domainCol < 0 {
				domainCol = ci
			}
		}
	}
	if modelCol < 0 {
		if strings.Contains(strings.Join(hdrCells, ""), "模型") {
			// 退而求其次：取含“模型”但非描述/长度的列
			for ci, cell := range hdrCells {
				if cell == "模型" || (strings.Contains(cell, "模型") && !strings.Contains(cell, "长度") && !strings.Contains(cell, "说明") && !strings.Contains(cell, "领域")) {
					modelCol = ci
					break
				}
			}
		}
	}
	if modelCol < 0 {
		return nil
	}
	var out []string
	rowStart := tableStart + 2
	for i := rowStart; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "|") {
			break // 表已结束
		}
		cells := volcanoHeaderOf(trimmed)
		if len(cells) <= modelCol {
			continue
		}
		// 分类/领域过滤（Agent 表）：只保留直调文本模型行；Harness/向量化/视频/图片/语音
		// 等非文本直调实体、以及明确非文本生成领域一律排除。实际官方表里只有首行写
		// `|模型 |文本生成（极速）…`，后续行分类列为空——因此只“正向排除”非文本实体，
		// 空分类/空领域的续行一律保留（继承首行上下文）。Coding 个人版表无这些列
		// （categoryCol/domainCol=-1），不做过滤。
		excluded := false
		if categoryCol >= 0 && categoryCol < len(cells) && strings.TrimSpace(cells[categoryCol]) != "" && !strings.Contains(cells[categoryCol], "模型") {
			excluded = true
		} else if domainCol >= 0 && domainCol < len(cells) && strings.TrimSpace(cells[domainCol]) != "" && !strings.Contains(cells[domainCol], "文本生成") {
			excluded = true
		}
		if excluded {
			continue
		}
		val := strings.TrimSpace(cells[modelCol])
		if val != "" {
			out = append(out, val)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isVolcanoTableSeparator(ln string) bool {
	if !strings.HasPrefix(ln, "|") {
		return false
	}
	for _, r := range ln {
		if r != '|' && r != '-' && r != ':' && r != ' ' {
			return false
		}
	}
	return strings.Contains(ln, "-")
}

// parseVolcanoModelProse 尝试从散文节提取模型 ID 枚举（Coding 企业版形态）。
// 官方正文是一整段“支持主流模型：…。具体的模型：A、B、C。”——只有“具体的模型：”
// 后的顿号/逗号枚举才是可直调模型；“支持主流模型”一段是说明性散文，绝不能当枚举
// 切分（否则模型名带句点会被切成 “2”/“1-turbo”，散文词也会混入候选）。因此只定位
// “具体的模型”标记，切它自己的冒号之后，分隔符不含 “.”（doubao-seed-2.1-turbo 自带
// 句点）。找不到可辨识枚举返回 nil。
func parseVolcanoModelProse(section string) []string {
	for _, ln := range strings.Split(section, "\n") {
		marker := strings.Index(ln, "具体的模型")
		if marker < 0 {
			continue
		}
		const fullWidthColon = "："
		colon := strings.Index(ln[marker:], fullWidthColon)
		if colon < 0 {
			continue
		}
		// 全角冒号是 3 字节 UTF-8，必须按 len 越过整个 rune，否则切片落在中间字节会
		// 在模型名首部混入非法 UTF-8 字节（表现为 U+FFFD 前缀）。
		raw := ln[marker+colon+len(fullWidthColon):]
		var out []string
		for _, tok := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == '、' || r == '，' || r == '。' || r == ',' || r == ' ' || r == '\t'
		}) {
			tok = strings.TrimSpace(tok)
			if tok == "" || strings.Contains(tok, "模型") {
				// 残留散文词（如“不同区域…以控制台为准”不会出现）防御性剔除。
				continue
			}
			out = append(out, tok)
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// normalizeVolcanoModelIDs 规范化候选 ID：反转义 \- → -、小写化、剥 `(alias)` 取主 ID、
// 剔除 auto/ark-code-latest/嵌入视觉等非直调实体、去重保序。
func normalizeVolcanoModelIDs(raw []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := []string{}
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		// 反转义：\+ 中的 \- 还原为 -。
		r = strings.ReplaceAll(r, `\-`, "-")
		// 截断到最早的一个分隔/注解标记：`<`（HTML 提示）、`(`（(alias) 别名）、
		// 反引号（如 doubao-seedance-1.5-pro`即将下线`）、换行/<br>。
		if idx := earliestVolcanoCutIndex(r); idx >= 0 {
			r = strings.TrimSpace(r[:idx])
		}
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" {
			continue
		}
		// 剔除动态调度别名
		if r == "auto" || r == "ark-code-latest" {
			continue
		}
		// 剔除嵌入/视觉/向量类（非文本直调）
		if strings.Contains(r, "embedding") {
			continue
		}
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, errors.New("volcano doc: no callable model IDs extracted from 支持模型 section")
	}
	return out, nil
}

// earliestVolcanoCutIndex 返回模型名后第一个注解/别名/HTML 分隔下标，无则返回 -1。
func earliestVolcanoCutIndex(s string) int {
	cut := -1
	for _, sep := range []string{"<", "(", "`", "\n", "\r"} {
		if i := strings.Index(s, sep); i >= 0 {
			if cut < 0 || i < cut {
				cut = i
			}
		}
	}
	return cut
}