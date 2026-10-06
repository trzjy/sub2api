package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// loadFixtureMD 读取 getDocDetail 夹具的 MDContent。
func loadFixtureMD(t *testing.T, name string) string {
	t.Helper()
	var payload struct {
		Result volcanoDocDetail `json:"Result"`
	}
	require.NoError(t, json.Unmarshal(loadVolcanoFixture(t, name), &payload))
	return payload.Result.MDContent
}

// TestExtractVolcanoPlanReportModelsAgentTable 用真实 Agent 个人版“支持模型及 Harness”
// 表验证：只取分类==模型/文本生成领域的直调文本模型，反转义 \-、剥 (alias)、剔除
// Harness 与空名。真实表里首行才有 `|模型 |文本生成（极速）…`，续行分类列为空——
// 必须保留（回归：不能误删续行）。
func TestExtractVolcanoPlanReportModelsAgentTable(t *testing.T) {
	md := loadFixtureMD(t, "agent_personal_2366394.json")
	models, err := extractVolcanoPlanReportModels(md)
	require.NoError(t, err)
	// 文本直调模型都在。
	for _, want := range []string{
		"doubao-seed-2.0-mini",
		"doubao-seed-2.0-lite",
		"deepseek-v4-flash",
		"glm-5.3",            // 剥 (glm-latest)
		"doubao-seed-2.1-turbo",
		"doubao-seed-evolving",
		"minimax-m3",
		"deepseek-v4-pro",
	} {
		require.Contains(t, models, want, "agent 文本直调模型应被解析到")
	}
	// 不得含动态/别名/嵌入/误切残留。
	require.NotContains(t, models, "auto")
	require.NotContains(t, models, "glm-latest")
	require.NotContains(t, models, "harness")
	require.NotContains(t, models, "-")
	// 排序去重。
	require.Equal(t, sortedCopy(models), models)
}

// TestExtractVolcanoPlanReportModelsCodingProse 用真实 Coding 企业版“支持模型”散文枚举
// 验证：大写驼峰 → 小写、反转义 \-、取个人/企业均可直调模型。
func TestExtractVolcanoPlanReportModelsCodingProse(t *testing.T) {
	md := loadFixtureMD(t, "coding_enterprise_2276791.json")
	models, err := extractVolcanoPlanReportModels(md)
	require.NoError(t, err)
	require.NotEmpty(t, models)
	hasDoubaoSeed2_0lite := false
	for _, m := range models {
		require.False(t, strings.Contains(m, "\\"), "应反转义 \\-")
		if m == "doubao-seed-2.0-lite" {
			hasDoubaoSeed2_0lite = true
		}
	}
	require.True(t, hasDoubaoSeed2_0lite)
}

// TestVolcanoPlanCandidatesFromFixtures 验证按套餐取“个人版∪企业版”候选并集，
// 且 agent 与 coding 相互独立互不串。
func TestVolcanoPlanCandidatesFromFixtures(t *testing.T) {
	server, cleanup := newVolcanoDocFixtureServer(t)
	defer cleanup()

	svc := &AccountTestService{
		cfg:               volcanoTestConfig(),
		volcanoDocClient:  server.Client(),
		volcanoDocBaseURL: server.URL,
	}

	agent, err := svc.volcanoPlanCandidates(context.Background(), volcanoPlanProfile{Kind: volcanoPlanKindAgent})
	require.NoError(t, err)
	require.NotEmpty(t, agent.models)
	require.Equal(t, volcanoPlanKindAgent, agent.evidence.Kind)
	require.Equal(t, len(agent.models), agent.evidence.CandidateCount)

	coding, err := svc.volcanoPlanCandidates(context.Background(), volcanoPlanProfile{Kind: volcanoPlanKindCoding})
	require.NoError(t, err)
	require.NotEmpty(t, coding.models)

	// Agent 与 Coding 相互独立互不串：真实官方文档里 kimi-k3 / doubao-seed-2.0-mini
	// 只在 Agent 套餐概览，绝不应进入 Coding 候选。
	require.Contains(t, agent.models, "kimi-k3")
	require.NotContains(t, coding.models, "kimi-k3")
	require.NotContains(t, coding.models, "doubao-seed-2.0-mini")
}

// TestVolcanoPlanCandidatesDocFailureFailsClosed 验证文档获取/解析失败同步失败关闭，
// 绝不回落静态清单。
func TestVolcanoPlanCandidatesDocFailureFailsClosed(t *testing.T) {
	// 空 MDContent 的 detail → 解析失败。
	t.Run("bad_markdown", func(t *testing.T) {
		md := "# 支持模型及 Harness\n没有可解析的模型表（坏结构）。"
		_, err := extractVolcanoPlanReportModels(md)
		require.Error(t, err)
	})
	t.Run("empty_markdown", func(t *testing.T) {
		_, err := extractVolcanoPlanReportModels("")
		require.Error(t, err)
	})
}

// TestVolcanoPlanCandidatesFetchesOnlyRequestedKind 验证候选提取只 fetch 与请求订阅
// 同类（agent/coding）的套餐概览详情，绝不拉无关订阅（P2-3：Agent 与 Coding 独立同步，
// 无关订阅文档获取失败不得影响本订阅）。
func TestVolcanoPlanCandidatesFetchesOnlyRequestedKind(t *testing.T) {
	doclist := loadVolcanoFixture(t, "doclist_82379.json")
	docs := map[string]string{
		"2366394": "agent_personal_2366394.json",
		"2374452": "agent_enterprise_2374452.json",
		"1925114": "coding_personal_1925114.json",
		"2276791": "coding_enterprise_2276791.json",
	}
	loaded := map[string][]byte{}
	for id, file := range docs {
		loaded[id] = loadVolcanoFixture(t, file)
	}
	var mu sync.Mutex
	var fetched []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case volcanoDocGetListPath:
			_, _ = w.Write(doclist)
		case volcanoDocGetDetailPath:
			id := r.URL.Query().Get("DocumentID")
			mu.Lock()
			fetched = append(fetched, id)
			mu.Unlock()
			body, ok := loaded[id]
			if !ok {
				http.Error(w, "unknown", http.StatusNotFound)
				return
			}
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	svc := &AccountTestService{
		cfg:               volcanoTestConfig(),
		volcanoDocClient:  server.Client(),
		volcanoDocBaseURL: server.URL,
	}
	_, err := svc.volcanoPlanCandidates(context.Background(), volcanoPlanProfile{Kind: volcanoPlanKindCoding})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.ElementsMatch(t, []string{"1925114", "2276791"}, fetched,
		"coding 候选提取不得拉取 agent 套餐概览详情")
}

// TestVolcanoPlanCandidatesMissingEditionFailsClosed 验证某订阅缺少个人版或企业版任一
// “套餐概览”时失败关闭：单一版本绝不当作完整并集，否则可能因缺编提名删除已收录模型
// （P1-2）。
func TestVolcanoPlanCandidatesMissingEditionFailsClosed(t *testing.T) {
	// 最小目录树：只含 Coding Plan 个人版“套餐概览”，缺企业版。
	nodes := `{"Result":[` +
		`{"DocumentID":1,"ParentID":0,"Title":"订阅 [Agent/Coding Plan]"},` +
		`{"DocumentID":4,"ParentID":1,"Title":"Coding Plan 个人版"},` +
		`{"DocumentID":5,"ParentID":4,"Title":"套餐概览"}]}`
	const detailMD = "## 支持模型及 Harness\n" +
		"| 分类 | 领域 | 模型名称 |\n" +
		"| --- | --- | --- |\n" +
		"| 文本生成 | 对话 | deepseek-v4-flash |\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case volcanoDocGetListPath:
			_, _ = w.Write([]byte(nodes))
		case volcanoDocGetDetailPath:
			detail := struct {
				Result volcanoDocDetail `json:"Result"`
			}{Result: volcanoDocDetail{
				DocumentID:  5,
				Title:       "套餐概览",
				MDContent:   detailMD,
				UpdatedTime: "2026-01-01",
			}}
			_ = json.NewEncoder(w).Encode(detail)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	svc := &AccountTestService{
		cfg:               volcanoTestConfig(),
		volcanoDocClient:  server.Client(),
		volcanoDocBaseURL: server.URL,
	}
	_, err := svc.volcanoPlanCandidates(context.Background(), volcanoPlanProfile{Kind: volcanoPlanKindCoding})
	require.Error(t, err, "缺一个版本必须失败关闭，不以单版当并集")
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}