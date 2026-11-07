package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/cursorupstream"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// CursorAutoCardDTO 是 Auto 卡密在管理端展示与交互的数据传输对象
type CursorAutoCardDTO struct {
	ID            int64      `json:"id"`
	CodeMasked    string     `json:"code_masked"`
	DeviceCode    string     `json:"device_code"`
	OverCount     int        `json:"over_count"`
	SumCount      int        `json:"sum_count"`
	IsUnlimited   bool       `json:"is_unlimited"`
	Status        string     `json:"status"` // active, cooling, exhausted, disabled
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
}

type batchImportCardsRequest struct {
	Cards     []string   `json:"cards" binding:"required"` // 每行一个卡密
	ExpiresAt *time.Time `json:"expires_at,omitempty"`     // 选填到期时间；提供则落库，未提供则清空为 NULL
}

type batchImportCardsResult struct {
	SuccessCount int      `json:"success_count"`
	FailedCount  int      `json:"failed_count"`
	Errors       []string `json:"errors"`
}

// ---------------------------------------------------------------------------
// 安全配置：上游基址只来自部署环境，代码内不保留任何上游地址
// ---------------------------------------------------------------------------

const (
	// autoPoolNotesPrefix 是 Auto 卡密在 quota_keys.notes 中的标记前缀。
	// notes 同时承载设备码与设备 MD5：二者合起来才是「这张卡绑定了哪台虚拟
	// 设备」的完整凭据，缺一则绑定不可复原、不可重放。
	autoPoolNotesPrefix = "auto_pool:"

	// autoPoolBindFailedMarker 标记本地行对应的上游绑定失败。
	// 失败行**从落库起**就不可选（expires_at 落在过去），因此不会被补池
	// 候选查询选中；但 notes 仍保留完整设备凭据，运维可据此人工恢复绑定。
	autoPoolBindFailedMarker = "bind=failed"
)

// newCursorAutoUpstreamClient 按环境配置构造上游客户端。
// 基址不可信（未配置 / 非安全端点）时直接失败，绝不使用包内默认地址。
func newCursorAutoUpstreamClient() (*cursorupstream.Client, error) {
	baseURL, err := service.ResolveCursorAutoUpstreamBaseURL()
	if err != nil {
		return nil, err
	}
	return cursorupstream.NewClient(baseURL), nil
}

// formatAutoPoolNotes 生成 Auto 卡密的 notes：记录卡密的虚拟设备绑定凭据。
// dev（设备码）与 md5（设备 MD5）必须同时落库，否则绑定关系不可复原。
func formatAutoPoolNotes(devCode, devMD5 string) string {
	return fmt.Sprintf("%sdev=%s;md5=%s", autoPoolNotesPrefix, devCode, devMD5)
}

// formatAutoPoolFailedNotes 生成绑定失败时的 notes：在绑定凭据之外追加失败标记，
// 供运维区分「从未绑定」与「绑定失败待恢复」。
func formatAutoPoolFailedNotes(devCode, devMD5 string) string {
	return fmt.Sprintf("%sdev=%s;md5=%s;%s", autoPoolNotesPrefix, devCode, devMD5, autoPoolBindFailedMarker)
}

// BatchImportCards 批量录入并激活绑定卡密
// POST /api/v1/admin/quota_keys/auto/batch_import
//
// 事务闭环（派发单 CARD-02 第 1 项，修复孤儿绑定 P1）：
//
//	上游绑定是对外部系统的不可逆副作用，本地 quota_keys 行是绑定关系的唯一凭据。
//	因此**先落库、后绑定**：只有本地已持久化的卡密才会去上游绑定设备码。
//	这样「上游已绑定但本地无记录」的孤儿绑定在结构上不可能出现——
//	落库失败时上游尚未被触碰，内存池也不会被污染。
//
//	单卡失败隔离：每张卡密独立走「落库 → 绑定 → 转可选 → 入池」四段，任一段失败
//	只影响该卡（计入 failed_count 并回写可定位的错误文案），不影响同批其它卡密，
//	也不会把半成品卡留在内存池里。
//
// 不可选不变量（派发单 CARD-06 P2，终审「人工告警不等于关闭」）：
//
//	**落库即不可选，绑成功才转可选**。阶段 1 落库时把 expires_at 置为过去，
//	该行从出生起就被补池候选查询（status=active AND 未过期）排除；阶段 3 只在
//	上游绑定成功后才把 expires_at 清空使其重新可选。于是：
//
//	  - 绑定失败的行天然不可选 —— 不再依赖任何可能失败的中和步骤，
//	    「中和失败导致行仍 active+未过期、随后被重消费」这条路径被结构性消除；
//	  - 绝不产生「active+未过期却从未绑定上游」的可重消费行；
//	  - 转可选失败只会把行留在「不可选」安全态（行不可选、不会被重消费），
//	    属可恢复的硬失败，需人工处理。
//
//	notes 始终保留完整设备凭据（dev + md5），运维可据此人工恢复绑定。
func (h *QuotaKeyHandler) BatchImportCards(c *gin.Context) {
	var req batchImportCardsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}

	// 上游基址不可信时整批失败关闭：绝不在未知端点上消耗卡密或写入绑定关系。
	client, err := newCursorAutoUpstreamClient()
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Cursor auto upstream unavailable: "+err.Error())
		return
	}

	res := batchImportCardsResult{
		Errors: make([]string, 0),
	}

	for _, rawCard := range req.Cards {
		cardKey := strings.TrimSpace(rawCard)
		if cardKey == "" {
			continue
		}

		devCode, devMD5 := cursorupstream.GenerateVirtualDeviceCode()

		// 阶段 1：先落库，且**落库即不可选**。
		// 失败即结束本卡：上游尚未绑定 → 无孤儿绑定；内存池不写入 → 无池污染。
		//
		// expires_at 置为过去（now-1min）使该行从出生起就被补池候选查询
		// （status=active AND 未过期）排除：即便后续任一步失败、即便 notes 标记
		// 也写不进去，这一行都不会被重新消费。notes 同时记录完整绑定凭据，
		// 供人工恢复绑定。
		notes := formatAutoPoolNotes(devCode, devMD5)
		unselectableUntil := time.Now().Add(-time.Minute)
		st, createErr := h.svc.Create(c.Request.Context(), cardKey, &unselectableUntil, &notes, 1)
		if createErr != nil {
			res.FailedCount++
			res.Errors = append(res.Errors, fmt.Sprintf("card %s persist failed: %v", maskCardKey(cardKey), createErr))
			continue
		}

		// 阶段 2：绑定上游设备码。
		// 失败时上游并未绑定（卡未被消耗），但本地行已经存在且**天然不可选**
		// （阶段 1 已把 expires_at 置为过去）—— 无需任何中和步骤，
		// 因此不再有「中和失败导致行仍 active+未过期」的路径。
		// 这里只登记失败标记（bind=failed）供运维检索与人工恢复绑定。
		if bindErr := client.ValidateActivationCode(c.Request.Context(), cardKey, devCode, devMD5); bindErr != nil {
			res.FailedCount++
			msg := fmt.Sprintf("card %s bind failed: %v", maskCardKey(cardKey), bindErr)
			// 标记写失败不影响安全性：行仍是不可选态（expires_at 在过去），
			// 不会被重消费；但运维失去了「这张卡从未绑定」的显式信号，如实上报。
			if markErr := h.markAutoCardBindFailed(c.Request.Context(), st.ID, devCode, devMD5); markErr != nil {
				msg += fmt.Sprintf("; failed-marker NOT persisted (%v): row %d stays unselectable (expires_at in the past) but carries no bind=failed marker", markErr, st.ID)
				slog.Error("cursor auto: bind failed AND failed-marker write failed, row stays unselectable and needs manual recovery",
					"quota_key_id", st.ID,
					"bind_err", bindErr,
					"marker_err", markErr,
				)
			}
			res.Errors = append(res.Errors, msg)
			continue
		}

		// 阶段 3：转可选 —— 只有绑成功才把该行放回补池候选集。
		// 失败 = 卡已绑上游但本地仍不可选：属**可恢复**的硬失败（行不可选、
		// 不会被重消费），报错并标注需人工处理；该卡不进内存池。
		if activateErr := h.activateAutoCard(c.Request.Context(), st.ID, req.ExpiresAt); activateErr != nil {
			res.FailedCount++
			msg := fmt.Sprintf("card %s bound upstream but NOT selectable locally (%v) — 已绑定但未转可选，需人工处理: row %d stays unselectable (expires_at in the past) and will not be re-consumed",
				maskCardKey(cardKey), activateErr, st.ID)
			slog.Error("cursor auto: bound upstream but activation to selectable failed, row left unselectable and requires manual handling",
				"quota_key_id", st.ID,
				"activate_err", activateErr,
			)
			res.Errors = append(res.Errors, msg)
			continue
		}

		// 阶段 4：查一次额度（尽力而为）并入池。
		// 额度未知不阻断导入：卡密已绑定、已落库且已转可选，后续 refresh_all 会补齐额度。
		overCount := 0
		sumCount := 0
		countResp, countErr := client.RefreshCount(c.Request.Context(), devCode, devMD5)
		if countErr == nil && countResp != nil {
			overCount = countResp.OverCount
			sumCount = countResp.SumCount
		}

		// 入池。只有「本地已落库 + 上游已绑定 + 本地已转可选」的卡密才允许进入
		// 内存池，失败卡密永不进池（避免污染池内额度统计与取号候选集）。
		// st.ID 来自本次新建行，池内不存在同 ID 的旧状态，整卡登记不会覆盖他人状态。
		if pool := service.GetGlobalCursorDynamicPool(); pool != nil {
			pool.RegisterCard(st.ID, cardKey, devCode, devMD5, overCount, sumCount)
		}

		res.SuccessCount++
	}

	response.Success(c, res)
}

// activateAutoCard 把已绑定成功的行从「不可选」转为「可选」。
//
// 做法：清空 expires_at（显式清空 = 永不过期）→ 该行重新满足补池候选查询的
// 「status=active AND 未过期」条件，回到候选集。清空而非置未来：Auto 卡密的
// 有效期由上游额度（over/sum count）决定，本地不应再设一个到期日把它扫成 expired。
//
// 失败语义：卡已绑上游但本地仍不可选 —— 行停留在「不可选」安全态
// （expires_at 仍在过去），不会被补池重消费，属可恢复的硬失败，
// 调用方必须按硬失败上报（明确标注需人工处理）。
//
// 额外校验（防清扫器竞态）：清空 expires_at 后回读状态，确认该行**确实**可选。
// 若清扫器在「落库 → 转可选」窗口内把该行标成了 expired，清空 expires_at 也
// 救不回来（status 已是终态）—— 此时必须显式失败，不能谎报成功。
func (h *QuotaKeyHandler) activateAutoCard(ctx context.Context, quotaKeyID int64, expiresAt *time.Time) error {
	var targetExpiresAt *time.Time
	if expiresAt != nil {
		tVal := *expiresAt
		targetExpiresAt = &tVal
	}
	updated, err := h.svc.Update(ctx, quotaKeyID, nil, &targetExpiresAt, nil, nil)
	if err != nil {
		return err
	}
	if updated == nil {
		return fmt.Errorf("quota_key_auto: activation returned empty status for row %d", quotaKeyID)
	}
	if updated.Status != domain.QuotaKeyStatusActive ||
		(updated.ExpiresAt != nil && !updated.ExpiresAt.After(time.Now())) {
		return fmt.Errorf("quota_key_auto: row %d not selectable after activation (status=%s)", quotaKeyID, updated.Status)
	}
	return nil
}

// markAutoCardBindFailed 把「上游绑定失败」标记写进 notes（**不**写 expires_at）。
//
// 用途：绑定失败时登记这一行，让运维能按 notes 检索出这些行，凭 dev+md5 人工恢复绑定。
//
// 写标记**不**承担任何安全性职责：这一行从落库起就不可选（阶段 1 已把 expires_at
// 置为过去），无论标记写成功还是失败，它都不会被补池候选查询选中、不会被重消费。
// 因此标记写失败只影响可观测性（运维少了检索信号），不改变安全态 —— 如实上报即可。
//
// 运维恢复路径：改 expires_at 为 null（或未来时间）即可让该行重新可选；
// 但 bind=failed 标记的行**从未绑定上游**，恢复前必须先按 dev+md5 完成绑定。
func (h *QuotaKeyHandler) markAutoCardBindFailed(ctx context.Context, quotaKeyID int64, devCode, devMD5 string) error {
	notesValue := formatAutoPoolFailedNotes(devCode, devMD5)
	notes := &notesValue
	_, err := h.svc.Update(ctx, quotaKeyID, nil, nil, &notes, nil)
	return err
}

// RefreshAutoPool 批量刷新所有 Auto 卡密在上游的剩余额度
// POST /api/v1/admin/quota_keys/auto/refresh_all
//
// 状态保留（派发单 CARD-02 第 2 项，修复刷新覆盖状态 P2）：
// 刷新只更新额度，必须调用 pool.UpdateCardCounts 只写 OverCount/SumCount。
// 严禁用 pool.RegisterCard 整卡重建——那会把单卡的 CooldownUntil（冷却中）与
// Disabled（已禁用）状态抹掉，导致冷却中的卡被立刻重新取号、已禁用的卡被复活。
func (h *QuotaKeyHandler) RefreshAutoPool(c *gin.Context) {
	pool := service.GetGlobalCursorDynamicPool()
	if pool == nil {
		response.Success(c, gin.H{"status": "pool_not_initialized"})
		return
	}

	client, err := newCursorAutoUpstreamClient()
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Cursor auto upstream unavailable: "+err.Error())
		return
	}

	refreshed := 0
	failed := 0

	cards := pool.ListCards()
	for _, card := range cards {
		countResp, err := client.RefreshCount(c.Request.Context(), card.DeviceCode, card.DeviceCodeMD5)
		if err != nil || countResp == nil {
			failed++
			continue
		}
		// 只更新额度，保留单卡的冷却/禁用状态。
		pool.UpdateCardCounts(card.ID, countResp.OverCount, countResp.SumCount)
		refreshed++
	}

	response.Success(c, gin.H{
		"refreshed_count": refreshed,
		"failed_count":    failed,
	})
}

// GetAutoPoolStatus 查询当前 Auto 动态号池的在线健康指标
// GET /api/v1/admin/quota_keys/auto/status
func (h *QuotaKeyHandler) GetAutoPoolStatus(c *gin.Context) {
	pool := service.GetGlobalCursorDynamicPool()
	if pool == nil {
		response.Success(c, gin.H{
			"active_tokens": 0,
			"total_cards":   0,
			"status":        "uninitialized",
		})
		return
	}

	stats := pool.GetStats()
	response.Success(c, stats)
}

func maskCardKey(k string) string {
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}

// ---------------------------------------------------------------------------
// kn-cy.com 卡密激活入口（CARD-KC 激活入库）
// ---------------------------------------------------------------------------

const (
	// knCyDefaultAccountBaseURL 是 kn-cy 账号凭据中的 OpenAI 兼容转发基址
	// （与激活接口 host api.kn-cy.com 不同：前者是 Chat Completions 出站目标）。
	// 仅作为缺省值；请求可显式覆盖。
	knCyDefaultAccountBaseURL = "https://kn-cy.com/v1"

	// knCyActivationAccountPriority 是 kn-cy 激活账号的调度优先级。
	knCyActivationAccountPriority = 10
	// knCyActivationAccountConcurrency 是 kn-cy 激活账号的并发槽数。
	knCyActivationAccountConcurrency = 3
)

// knCyClientFactory 构造上游 kn-cy 客户端，提取为包级变量以便定向测试替换为指向
// httptest 的替身（不污染生产默认 host api.kn-cy.com）。
var knCyClientFactory = func() *service.KnCyClient {
	return service.NewKnCyClient("")
}

// claimKnCyActivationRequest 是 kn-cy 卡密激活入参。
type claimKnCyActivationRequest struct {
	// Code 是 kn-cy.com 卡密（一次性 Bearer 消费，仅用于向上游激活，绝不回显/落库原文以外用途）。
	Code string `json:"code" binding:"required"`
	// Name 账号展示名（可选）；缺省由系统生成。
	Name string `json:"name"`
	// AccountID 可选：提供则更新该账号（仍须为 cursor/apikey 账号），否则新建。
	AccountID *int64 `json:"account_id"`
	// BaseURL 可选：覆盖账号凭据中的 OpenAI 兼容转发基址。
	BaseURL string `json:"base_url"`
	// ModelMapping 可选：账号级模型映射（客户端请求模型 → 上游模型）。
	ModelMapping map[string]string `json:"model_mapping"`
}

// claimKnCyActivationResponse 是 kn-cy 卡密激活的回执。
// 设计铁律：绝不回显凭据（api_key / 激活码原文）。
type claimKnCyActivationResponse struct {
	AccountID      int64   `json:"account_id"`
	Status         string  `json:"status"` // created | updated
	ExpiresAt      *int64  `json:"expires_at"`
	TotalQuota     float64 `json:"total_quota,omitempty"`
	RemainingQuota float64 `json:"remaining_quota,omitempty"`
}

// ClaimKnCyActivation 激活 kn-cy.com 卡密并创建/更新一个 cursor(api_key) 账号。
// POST /api/v1/admin/quota_keys/kn_cy/claim_activation
//
// 安全与隔离约束（CARD-KC 激活入库）：
//   - 与 cursorupstream ShareAI Auto 链**完全隔离**：本端点不调用 ResolveCursorAutoUpstreamBaseURL、
//     不触碰 auto_pool 虚拟设备码机制，也不复用 QuotaKeyService 的激活码落库路径；
//     kn-cy 走独立上游客户端 service.KnCyClient。两者凭据来源、落库目标互不相交。
//   - 凭据（api_key 即激活码、base_url、model_mapping）只经 AdminService 落库，绝不进日志、
//     绝不回响应（claimKnCyActivationResponse 不携带任何凭据字段）。
//   - 激活码仅作为上游 Bearer 一次性消费；上游错误信息由 KnCyClient 自带脱敏（credentials 不回显）。
func (h *QuotaKeyHandler) ClaimKnCyActivation(c *gin.Context) {
	if h.adminService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Admin service unavailable for kn-cy activation")
		return
	}

	var req claimKnCyActivationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		response.BadRequest(c, "code is required")
		return
	}

	// 独立 kn-cy 上游客户端（默认 host api.kn-cy.com），与 ShareAI Auto 链无交集。
	client := knCyClientFactory()
	data, err := client.ClaimActivation(c.Request.Context(), code)
	if err != nil {
		// 错误信息已由 KnCyClient 对凭据脱敏；此处只透传失败原因，绝不附带 code 原文。
		response.Error(c, http.StatusBadGateway, "kn-cy activation failed: "+err.Error())
		return
	}

	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" {
		baseURL = knCyDefaultAccountBaseURL
	}
	modelMapping := req.ModelMapping
	if modelMapping == nil {
		modelMapping = map[string]string{}
	}
	creds := map[string]any{
		"base_url":      baseURL,
		"api_key":       code,
		"model_mapping": modelMapping,
	}

	expiresAt := data.ExpiresAt
	prio := knCyActivationAccountPriority
	conc := knCyActivationAccountConcurrency

	if req.AccountID != nil {
		// 更新路径：只覆盖凭据与上游到期时间，保留既有账号的优先级/并发等运维调参。
		updated, upErr := h.adminService.UpdateAccount(c.Request.Context(), *req.AccountID, &service.UpdateAccountInput{
			Credentials: creds,
			ExpiresAt:   &expiresAt,
		})
		if upErr != nil {
			respondAdminServiceError(c, upErr)
			return
		}
		response.Success(c, claimKnCyActivationResponse{
			AccountID:      updated.ID,
			Status:         "updated",
			ExpiresAt:      &expiresAt,
			TotalQuota:     data.TotalQuota,
			RemainingQuota: data.RemainingQuota,
		})
		return
	}

	// 新建路径：cursor 平台 + apikey 类型（第三方 OpenAI 兼容反代，不依赖 quota_key 归属）。
	created, createErr := h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
		Name:                 req.Name,
		Platform:             domain.PlatformCursor,
		Type:                 domain.AccountTypeAPIKey,
		Credentials:          creds,
		Priority:             prio,
		Concurrency:          conc,
		ExpiresAt:            &expiresAt,
		SkipDefaultGroupBind: false,
	})
	if createErr != nil {
		respondAdminServiceError(c, createErr)
		return
	}
	response.Success(c, claimKnCyActivationResponse{
		AccountID:      created.ID,
		Status:         "created",
		ExpiresAt:      &expiresAt,
		TotalQuota:     data.TotalQuota,
		RemainingQuota: data.RemainingQuota,
	})
}

// respondAdminServiceError 统一映射 AdminService 写账号错误到 HTTP（与 quota_key_handler.go
// 的 respondQuotaKeyError 同口径；kn-cy 激活入口不引入新的错误信封）。
func respondAdminServiceError(c *gin.Context, err error) {
	var upe *service.UpstreamPullError
	if errors.As(err, &upe) {
		response.Error(c, http.StatusBadGateway, upe.Message())
		return
	}
	response.ErrorFrom(c, err)
}
