package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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
		// （status=active AND 未过期）排除：即便后续任一步失败，这一行都不会被重新消费。
		// notes 同时记录完整绑定凭据，供人工恢复绑定。
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
		// （阶段 1 已把 expires_at 置为过去）—— 无需任何中和步骤。
		// 这里只登记失败标记（bind=failed）供运维检索与人工恢复绑定。
		if bindErr := client.ValidateActivationCode(c.Request.Context(), cardKey, devCode, devMD5); bindErr != nil {
			res.FailedCount++
			msg := fmt.Sprintf("card %s bind failed: %v", maskCardKey(cardKey), bindErr)
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

		// 阶段 3：提取首号预热（GetCredentialsSecure）。
		// 失败 → 卡保持不可选（expires_at 仍在过去），错误记入 last_refresh_error，不入池。
		if _, warmErr := client.GetCredentialsSecure(c.Request.Context(), devCode, devMD5); warmErr != nil {
			res.FailedCount++
			msg := fmt.Sprintf("card %s warmup failed: %v", maskCardKey(cardKey), warmErr)
			if snapErr := h.svc.WriteAutoCardUsageSnapshot(c.Request.Context(), st.ID, nil, warmErr); snapErr != nil {
				msg += fmt.Sprintf("; last_refresh_error NOT persisted (%v)", snapErr)
				slog.Error("cursor auto: warmup failed AND refresh-error write failed, row stays unselectable",
					"quota_key_id", st.ID, "warm_err", warmErr, "snap_err", snapErr)
			}
			res.Errors = append(res.Errors, msg)
			continue
		}

		// 阶段 4：查额度（RefreshCount）。
		// 失败 → 卡保持不可选，错误记入 last_refresh_error，不入池。
		countResp, countErr := client.RefreshCount(c.Request.Context(), devCode, devMD5)
		if countErr != nil || countResp == nil {
			res.FailedCount++
			failErr := countErr
			if failErr == nil {
				failErr = fmt.Errorf("cursor auto: refresh count returned nil response")
			}
			msg := fmt.Sprintf("card %s count failed: %v", maskCardKey(cardKey), failErr)
			if snapErr := h.svc.WriteAutoCardUsageSnapshot(c.Request.Context(), st.ID, nil, failErr); snapErr != nil {
				msg += fmt.Sprintf("; last_refresh_error NOT persisted (%v)", snapErr)
				slog.Error("cursor auto: count failed AND refresh-error write failed, row stays unselectable",
					"quota_key_id", st.ID, "count_err", failErr, "snap_err", snapErr)
			}
			res.Errors = append(res.Errors, msg)
			continue
		}
		overCount := countResp.OverCount
		sumCount := countResp.SumCount

		// 阶段 5：持久化额度快照（绝不存 Token）。失败 → 卡保持不可选，不入池。
		if snapErr := h.syncAutoCardUsageSnapshot(c.Request.Context(), st.ID, overCount, sumCount); snapErr != nil {
			res.FailedCount++
			res.Errors = append(res.Errors, fmt.Sprintf("card %s snapshot persist failed: %v", maskCardKey(cardKey), snapErr))
			continue
		}

		// 阶段 6：仅当 3、4、5 全部成功后，才恢复设定的 expires_at 并进入内存池。
		if activateErr := h.activateAutoCard(c.Request.Context(), st.ID, req.ExpiresAt); activateErr != nil {
			res.FailedCount++
			msg := fmt.Sprintf("card %s bound upstream but NOT selectable locally (%v) — 已绑定已查额但未转可选，需人工处理: row %d stays unselectable (expires_at in the past) and will not be re-consumed",
				maskCardKey(cardKey), activateErr, st.ID)
			slog.Error("cursor auto: bound upstream but activation to selectable failed, row left unselectable and requires manual handling",
				"quota_key_id", st.ID,
				"activate_err", activateErr,
			)
			res.Errors = append(res.Errors, msg)
			continue
		}

		// 入池。只有「落库 + 绑定 + 预热 + 查额 + 快照回写」五步全部成功的卡密才允许进入
		// 内存池，失败卡密永不进池（避免污染池内额度统计与取号候选集，且天然保持不可选）。
	if pool := service.GetGlobalCursorDynamicPool(); pool != nil {
		pool.RegisterCard(st.ID, cardKey, devCode, devMD5, overCount, sumCount, st.Priority)
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

		// 同步回写数据库快照（单卡与全量刷新均持久化，供前端读取真实剩余额度）。
		// 失败只影响可观测性，不影响号池运行时额度。
		if snapErr := h.syncAutoCardUsageSnapshot(c.Request.Context(), card.ID, countResp.OverCount, countResp.SumCount); snapErr != nil {
			slog.Warn("cursor auto: refresh pool snapshot persist failed", "card_id", card.ID, "err", snapErr)
			failed++
			continue
		}
		refreshed++
	}

	response.Success(c, gin.H{
		"refreshed_count": refreshed,
		"failed_count":    failed,
	})
}

// autoCardUsageSnapshot 是 Auto 卡密写入 quota_keys.usage_snapshot 的 JSON 形态。
//
// 安全铁律（派发单 CARD-06）：只存聚合额度，**绝不写入任何 Bearer Token 或密钥明文**——
// Token 仅驻留动态池内存。前端读取该快照展示真实剩余额度。
type autoCardUsageSnapshot struct {
	Total       int    `json:"total"`
	Used        int    `json:"used"`
	Remaining   int    `json:"remaining"`
	OverCount   int    `json:"over_count"`
	SumCount    int    `json:"sum_count"`
	IsUnlimited bool   `json:"is_unlimited"`
	CheckedAt   string `json:"checked_at"`
}

// syncAutoCardUsageSnapshot 把单卡上游额度持久化写入 quota_keys.usage_snapshot。
//
// 计算 isUnlimited := sumCount >= 90000000 || sumCount == 99999999 || sumCount == 99999990，
// 构造 JSON 快照并通过 service 层写回数据库，成功时清空 last_refresh_error。
// 绝不写入 Token —— 该函数仅搬运转译上游聚合计数。
func (h *QuotaKeyHandler) syncAutoCardUsageSnapshot(ctx context.Context, id int64, overCount, sumCount int) error {
	isUnlimited := sumCount >= 90000000 || sumCount == 99999999 || sumCount == 99999990
	used := sumCount - overCount
	if used < 0 {
		used = 0
	}
	snap := autoCardUsageSnapshot{
		Total:       sumCount,
		Used:        used,
		Remaining:   overCount,
		OverCount:   overCount,
		SumCount:    sumCount,
		IsUnlimited: isUnlimited,
		CheckedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return h.svc.WriteAutoCardUsageSnapshot(ctx, id, data, nil)
}

// RefreshAutoCard 单卡刷新：解析设备凭据，向上游查额度，更新号池与数据库快照，返回最新额度。
// POST /api/v1/admin/quota_keys/auto/:id/refresh
func (h *QuotaKeyHandler) RefreshAutoCard(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid id")
		return
	}

	client, err := newCursorAutoUpstreamClient()
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Cursor auto upstream unavailable: "+err.Error())
		return
	}

	devCode, devMD5, derr := h.resolveAutoCardDevice(c.Request.Context(), id)
	if derr != nil {
		response.Error(c, http.StatusNotFound, derr.Error())
		return
	}

	countResp, cerr := client.RefreshCount(c.Request.Context(), devCode, devMD5)
	if cerr != nil || countResp == nil {
		failErr := cerr
		if failErr == nil {
			failErr = fmt.Errorf("cursor auto: refresh count returned nil response")
		}
		// 失败也登记 last_refresh_error，保持可观测。
		if snapErr := h.svc.WriteAutoCardUsageSnapshot(c.Request.Context(), id, nil, failErr); snapErr != nil {
			slog.Warn("cursor auto: single-card refresh error persist failed", "card_id", id, "err", snapErr)
		}
		response.Error(c, http.StatusBadGateway, "refresh count failed: "+failErr.Error())
		return
	}

	// 更新号池内存额度（只改额度，保留冷却/禁用状态）。
	if pool := service.GetGlobalCursorDynamicPool(); pool != nil {
		pool.UpdateCardCounts(id, countResp.OverCount, countResp.SumCount)
	}
	// 持久化回写数据库快照，供前端读取真实剩余额度。
	// 快照写库失败时，内存与数据库已分叉，必须直接返回 500 报错，禁止虚假成功。
	if snapErr := h.syncAutoCardUsageSnapshot(c.Request.Context(), id, countResp.OverCount, countResp.SumCount); snapErr != nil {
		slog.Error("cursor auto: single-card refresh snapshot persist failed", "card_id", id, "err", snapErr)
		response.Error(c, http.StatusInternalServerError, "persist usage snapshot failed: "+snapErr.Error())
		return
	}

	response.Success(c, gin.H{
		"id":           id,
		"over_count":   countResp.OverCount,
		"sum_count":    countResp.SumCount,
		"is_unlimited": countResp.SumCount >= 90000000 || countResp.SumCount == 99999999 || countResp.SumCount == 99999990,
	})
}

// resolveAutoCardDevice 解析 Auto 卡密的设备凭据（dev / md5）。
// 优先取自动态号池（已入池卡）；池内无（导入失败未入池）则从 DB notes 回解。
func (h *QuotaKeyHandler) resolveAutoCardDevice(ctx context.Context, id int64) (string, string, error) {
	if pool := service.GetGlobalCursorDynamicPool(); pool != nil {
		for _, card := range pool.ListCards() {
			if card.ID == id {
				return card.DeviceCode, card.DeviceCodeMD5, nil
			}
		}
	}
	status, err := h.svc.GetStatus(ctx, id)
	if err != nil {
		return "", "", fmt.Errorf("quota key %d not found: %w", id, err)
	}
	if status == nil || status.Notes == nil {
		return "", "", fmt.Errorf("quota key %d has no device credentials", id)
	}
	devCode, devMD5, ok := parseAutoPoolDeviceFromNotes(*status.Notes)
	if !ok {
		return "", "", fmt.Errorf("quota key %d notes missing device credentials", id)
	}
	return devCode, devMD5, nil
}

// parseAutoPoolDeviceFromNotes 从 notes（auto_pool:dev=...;md5=...[;bind=failed]）解析设备凭据。
// 失败返回 ok=false（凭据缺失或格式不符）。
func parseAutoPoolDeviceFromNotes(notes string) (devCode, devMD5 string, ok bool) {
	if !strings.HasPrefix(notes, autoPoolNotesPrefix) {
		return "", "", false
	}
	body := notes[len(autoPoolNotesPrefix):]
	for _, p := range strings.Split(body, ";") {
		p = strings.TrimSpace(p)
		switch {
		case strings.HasPrefix(p, "dev="):
			devCode = strings.TrimPrefix(p, "dev=")
		case strings.HasPrefix(p, "md5="):
			devMD5 = strings.TrimPrefix(p, "md5=")
		}
	}
	if devCode == "" || devMD5 == "" {
		return "", "", false
	}
	return devCode, devMD5, true
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
