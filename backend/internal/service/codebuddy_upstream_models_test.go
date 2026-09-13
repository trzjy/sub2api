package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tidwall/gjson"
)

// loadCodeBuddyTestModels 解析 testdata 信封（data.models）为 []CodeBuddyModel，
// 复用与生产 fetchUpstreamModelList 相同的报文形态（含嵌套 reasoning 字段）。
func loadCodeBuddyTestModels(t *testing.T, rel string) []CodeBuddyModel {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatalf("read testdata %s: %v", rel, err)
	}
	data := gjson.GetBytes(raw, "data")
	if !data.Exists() {
		data = gjson.ParseBytes(raw)
	}
	modelsRaw := data.Get("models").Raw
	if modelsRaw == "" {
		modelsRaw = gjson.GetBytes(raw, "models").Raw
	}
	var models []CodeBuddyModel
	if err := json.Unmarshal([]byte(modelsRaw), &models); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	return models
}

// TestCodeBuddyUpstreamCatalogBody_PersonalFixture 覆盖 F5 目录报文映射：
// 启用模型按字典序去重、disabled 与空 ID 剔除、嵌套 reasoning 档位正确解析。
func TestCodeBuddyUpstreamCatalogBody_PersonalFixture(t *testing.T) {
	models := loadCodeBuddyTestModels(t, "codebuddy_models_personal.json")
	body, ids, err := codeBuddyUpstreamCatalogBody(models)
	if err != nil {
		t.Fatalf("codeBuddyUpstreamCatalogBody error: %v", err)
	}

	// 期望启用模型（剔除 disabled-model 与空 ID）。
	wantIDs := []string{"deepseek-v3-0324", "deepseek-v3.2", "glm-4.5"}
	if len(ids) != len(wantIDs) {
		t.Fatalf("expected ids %v, got %v", wantIDs, ids)
	}
	for i := range wantIDs {
		if ids[i] != wantIDs[i] {
			t.Fatalf("expected ids %v, got %v", wantIDs, ids)
		}
	}

	catalog := gjson.ParseBytes(body)
	if got := catalog.Get("data.#").Int(); got != int64(len(wantIDs)) {
		t.Fatalf("expected %d catalog entries, got %d", len(wantIDs), got)
	}

	// deepseek-v3-0324：嵌套 reasoning.supportedEfforts=[low,medium,high]。
	entry, ok := findCatalogEntry(catalog, "deepseek-v3-0324")
	if !ok {
		t.Fatalf("missing catalog entry deepseek-v3-0324")
	}
	if got := entry.Get("reasoning").Bool(); !got {
		t.Fatalf("expected reasoning=true for deepseek-v3-0324")
	}
	if got := entry.Get("supported_reasoning_levels").Array(); len(got) != 3 {
		t.Fatalf("expected 3 reasoning levels, got %d", len(got))
	}
	if got := entry.Get("default_reasoning_level").String(); got != "low" {
		t.Fatalf("expected default level low, got %q", got)
	}

	// glm-4.5：嵌套 reasoning.supportedEfforts=[high] → 单档 high 视为有推理。
	glm, ok := findCatalogEntry(catalog, "glm-4.5")
	if !ok {
		t.Fatalf("missing catalog entry glm-4.5")
	}
	if got := glm.Get("reasoning").Bool(); !got {
		t.Fatalf("expected reasoning=true for glm-4.5")
	}
	if got := glm.Get("supported_reasoning_levels.0").String(); got != "high" {
		t.Fatalf("expected high level, got %q", got)
	}

	// deepseek-v3.2：无 reasoning 字段 → reasoning=false，不输出档位。
	ds, ok := findCatalogEntry(catalog, "deepseek-v3.2")
	if !ok {
		t.Fatalf("missing catalog entry deepseek-v3.2")
	}
	if got := ds.Get("reasoning").Bool(); got {
		t.Fatalf("expected reasoning=false for deepseek-v3.2")
	}
	if got := ds.Get("supported_reasoning_levels").Exists(); got {
		t.Fatalf("expected no reasoning levels for deepseek-v3.2")
	}

	// context_window 兜底：deepseek-v3-0324 maxInputTokens=65536 应保留。
	if got := entry.Get("context_window").Int(); got != 65536 {
		t.Fatalf("expected context_window 65536, got %d", got)
	}
}

// TestCodeBuddyUpstreamCatalogBody_Empty 覆盖空/全 disabled 输入返回空目录。
func TestCodeBuddyUpstreamCatalogBody_Empty(t *testing.T) {
	body, ids, err := codeBuddyUpstreamCatalogBody(nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected no ids, got %v", ids)
	}
	if got := gjson.ParseBytes(body).Get("data.#").Int(); got != 0 {
		t.Fatalf("expected empty data, got %d", got)
	}
}

func findCatalogEntry(catalog gjson.Result, id string) (gjson.Result, bool) {
	for _, e := range catalog.Get("data").Array() {
		if e.Get("id").String() == id {
			return e, true
		}
	}
	return gjson.Result{}, false
}
