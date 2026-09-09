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
	"sync"
	"time"

	"net/url"
	"github.com/tidwall/gjson"
)

// 订阅成本实测：控制变量实验。
// 流程（每模型）：探针官方月配额绝对 Used → 注入已知量调用（输入重载突发）→
// 等待计量入账 → 再次探针 → 差值 ÷ 注入 token 数 = 该模型扣减权重（units/M tokens）。
// 权重回写成本核算计划，成本/倍率由前端按计划参数换算。

const (
	experimentBurstRequests    = 8
	experimentBurstPromptTokens = 35000
	experimentBurstMaxTokens   = 32
	experimentBurstConcurrency = 4
	experimentSettleWait       = 15 * time.Minute
	experimentChatTimeout      = 300 * time.Second
)

// PricingExperimentResult 单模型实测结果。
type PricingExperimentResult struct {
	Model      string  `json:"model"`
	TokensIn   int64   `json:"tokens_in"`
	TokensOut  int64   `json:"tokens_out"`
	UsedBefore float64 `json:"used_before"`
	UsedAfter  float64 `json:"used_after"`
	DeltaUnits float64 `json:"delta_units"`
	WeightPerM float64 `json:"weight_per_m"` // units / M tokens
	Skipped    bool    `json:"skipped"`
	Note       string  `json:"note,omitempty"`
}

// PricingExperimentState 实测任务状态（进程内，单实例部署）。
type PricingExperimentState struct {
	PlanIndex    int                       `json:"plan_index"`
	PlanProvider string                    `json:"plan_provider"`
	Status       string                    `json:"status"` // running / completed / failed
	StartedAt    time.Time                 `json:"started_at"`
	UpdatedAt    time.Time                 `json:"updated_at"`
	CurrentModel string                    `json:"current_model,omitempty"`
	Log          []string                  `json:"log"`
	Results      []PricingExperimentResult `json:"results"`
	Err          string                    `json:"error,omitempty"`
}

type pricingExperimentRunner struct {
	mu        sync.Mutex
	running   bool
	state     *PricingExperimentState
}

// StartPricingCostExperiment 启动一轮实测（同一时间仅允许一个）。
func (s *ModelPlazaService) StartPricingCostExperiment(ctx context.Context, planIndex int) error {
	if s == nil || s.settingService == nil || s.accountRepo == nil {
		return fmt.Errorf("pricing experiment unavailable")
	}
	s.expMu.Lock()
	defer s.expMu.Unlock()
	if s.expRunning {
		return fmt.Errorf("experiment already running")
	}
	basis, err := s.settingService.GetPricingCostBasis(ctx)
	if err != nil {
		return err
	}
	if planIndex < 0 || planIndex >= len(basis.Plans) {
		return fmt.Errorf("plan index out of range")
	}
	plan := &basis.Plans[planIndex]
	if len(plan.Accounts) == 0 {
		return fmt.Errorf("plan has no bound accounts")
	}
	if len(plan.Weights) == 0 {
		return fmt.Errorf("plan has no models to measure")
	}

	s.expRunning = true
	state := &PricingExperimentState{
		PlanIndex:    planIndex,
		PlanProvider: plan.Provider,
		Status:       "running",
		StartedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
		Log:          []string{},
		Results:      []PricingExperimentResult{},
	}
	s.expState = state

	go func() {
		defer func() {
			s.expMu.Lock()
			s.expRunning = false
			s.expMu.Unlock()
		}()
		s.runPricingExperiment(ctx, plan, basis, state)
	}()
	return nil
}

// GetPricingCostExperimentState 返回实测任务状态。
func (s *ModelPlazaService) GetPricingCostExperimentState() *PricingExperimentState {
	s.expMu.Lock()
	defer s.expMu.Unlock()
	if s.expState == nil {
		return &PricingExperimentState{Status: "idle"}
	}
	cp := *s.expState
	cp.Log = append([]string(nil), s.expState.Log...)
	cp.Results = append([]PricingExperimentResult(nil), s.expState.Results...)
	return &cp
}

func (s *ModelPlazaService) expLog(state *PricingExperimentState, format string, args ...any) {
	state.Log = append(state.Log, time.Now().UTC().Format("15:04:05 ")+fmt.Sprintf(format, args...))
	if len(state.Log) > 200 {
		state.Log = state.Log[len(state.Log)-200:]
	}
	state.UpdatedAt = time.Now().UTC()
}

// runPricingExperiment 执行逐模型实测（阻塞，goroutine 内运行）。
func (s *ModelPlazaService) runPricingExperiment(ctx context.Context, plan *PricingCostBasisPlan, basis *PricingCostBasis, state *PricingExperimentState) {

	// 探针与突发账号：按绑定账号解析（AK/SK 探针 + 推理密钥/端点）
	accounts, err := s.accountRepo.ListActive(ctx)
	if err != nil {
		state.Status = "failed"
		state.Err = fmt.Sprintf("list accounts: %v", err)
		return
	}
	accByID := map[int64]*Account{}
	for i := range accounts {
		accByID[accounts[i].ID] = &accounts[i]
	}
	var probeAcc *Account
	for _, id := range plan.Accounts {
		if a := accByID[id]; a != nil {
			if a.GetCredential("access_key") != "" && a.GetCredential("secret_key") != "" {
				probeAcc = a
				break
			}
		}
	}
	if probeAcc == nil {
		state.Status = "failed"
		state.Err = "bound accounts have no AK/SK credentials"
		return
	}

	used0, err := s.probeAFPMonthlyUsed(probeAcc)
	if err != nil {
		state.Status = "failed"
		state.Err = fmt.Sprintf("initial probe: %v", err)
		return
	}
	s.expLog(state, "月配额 Used 起始: %.4f", used0)

	models := make([]string, 0, len(plan.Weights))
	for m := range plan.Weights {
		models = append(models, m)
	}
	sort.Strings(models)

	results := make([]PricingExperimentResult, 0, len(models))
	for _, model := range models {
		state.CurrentModel = model
		s.expLog(state, "开始实测 %s", model)

		// 突发账号：绑定账号中映射包含该模型且带推理密钥者，否则首个有密钥账号
		burstAcc := s.pickBurstAccount(plan, accounts, accByID, model)
		if burstAcc == nil {
			results = append(results, PricingExperimentResult{Model: model, Skipped: true, Note: "无可用突发账号"})
			s.expLog(state, "%s 无可用突发账号，跳过", model)
			continue
		}

		ub, err := s.probeAFPMonthlyUsed(probeAcc)
		if err != nil {
			results = append(results, PricingExperimentResult{Model: model, Skipped: true, Note: fmt.Sprintf("probe: %v", err)})
			continue
		}
		burst := s.runInputBurst(ctx, burstAcc, model)
		if burst.err != nil {
			results = append(results, PricingExperimentResult{Model: model, Skipped: true, Note: fmt.Sprintf("burst: %v", burst.err)})
			s.expLog(state, "%s 突发失败: %v", model, burst.err)
			continue
		}
		time.Sleep(experimentSettleWait)
		ua, err := s.probeAFPMonthlyUsed(probeAcc)
		if err != nil {
			results = append(results, PricingExperimentResult{Model: model, Skipped: true, Note: fmt.Sprintf("probe: %v", err)})
			continue
		}

		tokensM := float64(burst.promptTokens+burst.completionTok) / 1e6
		delta := ua - ub
		w := 0.0
		if tokensM > 0 {
			w = delta / tokensM
		}
		res := PricingExperimentResult{
			Model:      model,
			TokensIn:   burst.promptTokens,
			TokensOut:  burst.completionTok,
			UsedBefore: ub,
			UsedAfter:  ua,
			DeltaUnits: delta,
			WeightPerM: w,
		}
		if w <= 0 {
			res.Skipped = true
			res.Note = "Δ≤0（计量滞后或无扣减），建议重测"
		}
		results = append(results, res)
		s.expLog(state, "%s 实测: Δ%.4f units / %.3fM tokens = %.1f units/M", model, delta, tokensM, w)
		state.Results = results
		state.UpdatedAt = time.Now().UTC()
	}

	state.Results = results
	// 权重回写计划（仅成功实测的模型）
	updated := 0
	for i := range results {
		r := results[i]
		if r.Skipped || r.WeightPerM <= 0 {
			continue
		}
		plan.Weights[r.Model] = r.WeightPerM
		updated++
	}
	plan.UpdatedAt = time.Now().UTC().Format("2006-01-02 15:04")
	if saveErr := s.settingService.SavePricingCostBasis(ctx, basis); saveErr != nil {
		s.expLog(state, "成本核算保存失败: %v", saveErr)
	} else {
		s.expLog(state, "实测完成：%d 个模型权重已回写成本核算", updated)
	}
	s.InvalidatePlazaOverrideCache()
	state.Status = "completed"
	state.UpdatedAt = time.Now().UTC()
}

// pickBurstAccount 选择突发账号：优先模型映射包含该模型者，其次任一带推理密钥的绑定账号。
func (s *ModelPlazaService) pickBurstAccount(plan *PricingCostBasisPlan, accounts []Account, accByID map[int64]*Account, model string) *Account {
	var fallback *Account
	for _, id := range plan.Accounts {
		a := accByID[id]
		if a == nil || a.Status != StatusActive {
			continue
		}
		if a.GetCredential("api_key") == "" || a.GetCredential("base_url") == "" {
			continue
		}
		mapping := a.GetModelMapping()
		for k, v := range mapping {
			if strings.EqualFold(k, model) || strings.EqualFold(v, model) {
				return a
			}
		}
		if fallback == nil {
			fallback = a
		}
	}
	return fallback
}

type experimentBurstOut struct {
	promptTokens  int64
	completionTok int64
	err           error
}

// runInputBurst 输入重载突发：唯一 prompt × N 请求，回报实测 tokens。
func (s *ModelPlazaService) runInputBurst(ctx context.Context, acc *Account, model string) experimentBurstOut {
	base := strings.TrimSuffix(acc.GetCredential("base_url"), "/")
	apiKey := acc.GetCredential("api_key")
	u := base + "/v3/chat/completions"
	promptTokens := experimentBurstPromptTokens
	var mu sync.Mutex
	out := experimentBurstOut{}
	sem := make(chan struct{}, experimentBurstConcurrency)
	var wg sync.WaitGroup
	client := &http.Client{Timeout: experimentChatTimeout}

	for i := 0; i < experimentBurstRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			prompt := experimentPrompt(tagForExperiment(idx), idx, promptTokens)
			body, _ := json.Marshal(map[string]any{
				"model":      model,
				"messages":   []map[string]string{{"role": "user", "content": prompt}},
				"max_tokens": experimentBurstMaxTokens,
			})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
			if err != nil {
				mu.Lock()
				out.err = err
				mu.Unlock()
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+apiKey)
			var lastErr error
			for attempt := 0; attempt < 3; attempt++ {
				resp, err := client.Do(req)
				if err != nil {
					lastErr = err
					time.Sleep(5 * time.Second)
					continue
				}
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				if resp.StatusCode == 429 || resp.StatusCode >= 500 {
					lastErr = fmt.Errorf("status %d", resp.StatusCode)
					time.Sleep(20 * time.Second)
					continue
				}
				if resp.StatusCode != 200 {
					mu.Lock()
					out.err = fmt.Errorf("status %d: %s", resp.StatusCode, string(b[:min(len(b), 160)]))
					mu.Unlock()
					return
				}
				usage := gjson.GetBytes(b, "usage")
				mu.Lock()
				out.promptTokens += usage.Get("prompt_tokens").Int()
				out.completionTok += usage.Get("completion_tokens").Int()
				mu.Unlock()
				return
			}
			mu.Lock()
			out.err = lastErr
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	return out
}

// probeAFPMonthlyUsed 探针 Agent Plan 月配额绝对 Used（AK/SK 管理面）。
func (s *ModelPlazaService) probeAFPMonthlyUsed(acc *Account) (float64, error) {
	ak := strings.TrimSpace(acc.GetCredential("access_key"))
	sk := strings.TrimSpace(acc.GetCredential("secret_key"))
	if ak == "" || sk == "" {
		return 0, fmt.Errorf("account %d has no AK/SK", acc.ID)
	}
	base := acc.GetCredential("base_url")
	action := volcanoUsageAction(base)
	if action != "GetAFPUsage" {
		return 0, fmt.Errorf("plan probe unsupported for base %s (percent-only)", base)
	}
	query := url.Values{}
	query.Set("Action", action)
	query.Set("Version", volcanoQuotaVersion)
	canonQuery, signedHeaders, err := volcEngineSignQuery(ak, sk, volcanoQuotaRegion, volcanoQuotaService, volcanoQuotaHost, query, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+volcanoQuotaHost+"/?"+canonQuery, nil)
	if err != nil {
		return 0, err
	}
	for name, values := range signedHeaders {
		for _, v := range values {
			req.Header.Set(name, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 160)])))
	}
	used := gjson.GetBytes(body, "Result.AFPMonthly.Used").Float()
	return used, nil
}

// experimentPrompt 生成唯一长文本（约 targetTokens tokens，数字串 4 chars/token）。
func experimentPrompt(tag string, idx, targetTokens int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s-%d-%d] ", tag, time.Now().UnixNano(), idx)
	i := 0
	for b.Len() < targetTokens*4 {
		fmt.Fprintf(&b, "%08d ", i)
		i++
	}
	b.WriteString("\n只需回复:OK")
	return b.String()
}

func tagForExperiment(idx int) string {
	return fmt.Sprintf("cost-exp-%d", idx)
}
