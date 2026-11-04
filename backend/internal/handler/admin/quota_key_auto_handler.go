package admin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

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
	Cards []string `json:"cards" binding:"required"` // 每行一个卡密
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
	// envCursorAutoUpstreamBaseURL 是 Cursor Auto 上游卡密服务的基址环境变量名。
	//
	// 硬约束（派发单 CARD-02 第 3 项）：上游基址只能来自部署环境配置，代码内
	// 不得出现任何上游地址常量（尤其禁止明文公网地址）。未配置即失败关闭，
	// 绝不回落到 cursorupstream 包内的默认地址。
	envCursorAutoUpstreamBaseURL = "CURSOR_AUTO_UPSTREAM_BASE_URL"

	// autoPoolNotesPrefix 是 Auto 卡密在 quota_keys.notes 中的标记前缀。
	// notes 同时承载设备码与设备 MD5：二者合起来才是「这张卡绑定了哪台虚拟
	// 设备」的完整凭据，缺一则绑定不可复原、不可重放。
	autoPoolNotesPrefix = "auto_pool:"

	// autoPoolBindFailedMarker 标记本地行对应的上游绑定失败。
	// 失败行会被 neutralize（expires_at 置为过去），因此不会被补池器选中，
	// 但 notes 仍保留完整设备凭据，运维可据此人工恢复绑定。
	autoPoolBindFailedMarker = "bind=failed"
)

// resolveCursorAutoUpstreamBaseURL 从部署环境解析 Cursor Auto 上游基址。
//
// 校验口径：
//   - 未配置 → 失败关闭（不回落到包内默认的明文公网地址）；
//   - 必须是绝对的 http(s) URL 且带主机名，且不得内嵌用户凭据；
//   - 明文 http 只放行本机回环与内网地址；公网端点必须 https。
//
// 返回值已去掉尾部斜杠：cursorupstream 客户端按 "<baseURL><baseAPI>/<path>"
// 拼接请求路径，保留尾部斜杠会产生双斜杠。
func resolveCursorAutoUpstreamBaseURL() (string, error) {
	raw := strings.TrimSpace(os.Getenv(envCursorAutoUpstreamBaseURL))
	if raw == "" {
		return "", fmt.Errorf("%s is not configured", envCursorAutoUpstreamBaseURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid %s: %w", envCursorAutoUpstreamBaseURL, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid %s: missing host", envCursorAutoUpstreamBaseURL)
	}
	if u.User != nil {
		return "", fmt.Errorf("invalid %s: embedded credentials are not allowed", envCursorAutoUpstreamBaseURL)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isPrivateOrLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("invalid %s: https is required for public endpoints", envCursorAutoUpstreamBaseURL)
		}
	default:
		return "", fmt.Errorf("invalid %s: only http/https are allowed", envCursorAutoUpstreamBaseURL)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// isPrivateOrLoopbackHost 判断主机名是否指向本机回环或内网地址。
//
// 只接受 IP 字面量与 "localhost"：域名无法静态判定是否内网，一律按公网处理
// （要求 https），避免把明文流量发往不可信公网端点。
func isPrivateOrLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

// newCursorAutoUpstreamClient 按环境配置构造上游客户端。
// 基址不可信（未配置 / 非安全端点）时直接失败，绝不使用包内默认地址。
func newCursorAutoUpstreamClient() (*cursorupstream.Client, error) {
	baseURL, err := resolveCursorAutoUpstreamBaseURL()
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
//	落库失败时上游尚未被触碰，内存池也不会被污染；
//	绑定失败时本地行保留完整设备凭据（dev + md5）并被移出补池候选集，可人工恢复。
//
//	单卡失败隔离：每张卡密独立走「落库 → 绑定 → 入池」三段，任一段失败只影响
//	该卡（计入 failed_count 并回写可定位的错误文案），不影响同批其它卡密，
//	也不会把半成品卡留在内存池里。
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

		// 阶段 1：先落库（写前记录绑定意图）。
		// 失败即结束本卡：上游尚未绑定 → 无孤儿绑定；内存池不写入 → 无池污染。
		notes := formatAutoPoolNotes(devCode, devMD5)
		st, createErr := h.svc.Create(c.Request.Context(), cardKey, nil, &notes, 1)
		if createErr != nil {
			res.FailedCount++
			res.Errors = append(res.Errors, fmt.Sprintf("card %s persist failed: %v", maskCardKey(cardKey), createErr))
			continue
		}

		// 阶段 2：绑定上游设备码。
		// 失败时上游并未绑定（卡未被消耗），但本地行已经存在；把该行 neutralize
		// （expires_at 置为过去）使其退出补池候选集，同时保留设备凭据以便人工恢复。
		if bindErr := client.ValidateActivationCode(c.Request.Context(), cardKey, devCode, devMD5); bindErr != nil {
			res.FailedCount++
			msg := fmt.Sprintf("card %s bind failed: %v", maskCardKey(cardKey), bindErr)
			if neutralizeErr := h.neutralizeFailedAutoCard(c.Request.Context(), st.ID, devCode, devMD5); neutralizeErr != nil {
				msg += fmt.Sprintf(" (local row %d left active for manual recovery: %v)", st.ID, neutralizeErr)
			}
			res.Errors = append(res.Errors, msg)
			continue
		}

		// 阶段 3：查一次额度（尽力而为）。
		// 额度未知不阻断导入：卡密已绑定且已落库，后续 refresh_all 会补齐额度。
		overCount := 0
		sumCount := 0
		countResp, countErr := client.RefreshCount(c.Request.Context(), devCode, devMD5)
		if countErr == nil && countResp != nil {
			overCount = countResp.OverCount
			sumCount = countResp.SumCount
		}

		// 阶段 4：入池。只有「本地已落库 + 上游已绑定」的卡密才允许进入内存池，
		// 失败卡密永不进池（避免污染池内额度统计与取号候选集）。
		// st.ID 来自本次新建行，池内不存在同 ID 的旧状态，整卡登记不会覆盖他人状态。
		if pool := service.GetGlobalCursorDynamicPool(); pool != nil {
			pool.RegisterCard(st.ID, cardKey, devCode, devMD5, overCount, sumCount)
		}

		res.SuccessCount++
	}

	response.Success(c, res)
}

// neutralizeFailedAutoCard 把绑定失败的本地行移出补池候选集。
//
// 做法：expires_at 置为过去 → 补池候选查询（status=active AND exhausted=false
// AND 未过期）不再选中它，清扫器随后把它归入 expired 终态。notes 里保留完整
// 设备凭据（dev + md5），运维可据此人工恢复绑定。
//
// 该行不会被物理删除：code_mac 上有唯一索引，删除会让同一张卡密永远无法再次导入。
func (h *QuotaKeyHandler) neutralizeFailedAutoCard(ctx context.Context, quotaKeyID int64, devCode, devMD5 string) error {
	past := time.Now().Add(-time.Minute)
	expiresAt := &past
	notesValue := formatAutoPoolFailedNotes(devCode, devMD5)
	notes := &notesValue
	_, err := h.svc.Update(ctx, quotaKeyID, nil, &expiresAt, &notes, nil)
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
