package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// noAccountErrorClassification describes the HTTP response to emit when
// account selection failed with ErrNoAvailableAccounts. Handlers obtain it
// via classifyNoAccountError and choose between:
//
//   - 404 model_not_found — the group has accounts, but none of them are
//     configured to serve the requested model (config / typo / unsupported
//     model). Returning 503 here misleads operators and trips reverse-proxy
//     health checks; 404 lets the client surface the real problem.
//
//   - 503 api_error — accounts that could serve the model exist but are
//     temporarily exhausted (rate limit, quota auto-pause, runtime block) OR
//     the group has no accounts at all. Both stay on 503 because retrying
//     after a backoff can plausibly succeed (or, in the empty-pool case, the
//     operator may be in the middle of adding accounts).
type noAccountErrorClassification struct {
	Status        int
	ErrType       string
	Message       string
	ModelNotFound bool // true when this is a 404 model_not_found classification
}

// classifySelectionFailureError preserves the scheduler's compact reason when
// every model-capable account is temporarily rate limited.
//
// 原因只从 *service.NoAvailableAccountsError 的字段读取。历史实现用正则反解
// Error() 文案，那等于把「给人看的日志文本」当成「给机器看的协议」：措辞一改
// 分类就地失效，且 Go duration 的 "3h20m12s" 会被逐段正则只截到 "3h"。
func classifySelectionFailureError(err error, fallback noAccountErrorClassification) noAccountErrorClassification {
	if err == nil {
		return fallback
	}
	// A 404 model_not_found fallback is authoritative and must not be downgraded
	// to a rate-limit verdict. classifyNoAccountError only reaches it through
	// DiagnoseModelAvailabilityForPlatform, a dedicated database query over
	// persistent eligibility (active + schedulable + model_mapping) that already
	// established no account in the group can serve this model at all. A transient
	// per-model cooldown on one of the remaining candidates does not make "all
	// available accounts are rate-limited" true.
	//
	// Overriding this 404 with the rate-limit verdict is actively harmful:
	// retrying can never succeed, and clients that treat the rate-limit verdict
	// as a transient condition retry hard and swallow the body (Codex surfaces
	// only "exceeded retry limit, last status: 503"), losing the one message
	// that names the real problem. It also flips the ops attribution from a local
	// model-configuration issue to routing capacity, because call sites gate
	// markOpsRoutingCapacityLimitedIfNoAvailable on ModelNotFound.
	if fallback.ModelNotFound {
		return fallback
	}
	var selectionErr *service.NoAvailableAccountsError
	if !errors.As(err, &selectionErr) {
		return fallback
	}
	if selectionErr.ModelRateLimited > 0 {
		return noAccountErrorClassification{
			Status:  http.StatusServiceUnavailable,
			ErrType: "service_unavailable",
			Message: allAccountsCoolingMessage(selectionErr),
		}
	}
	// 账号本身可用、只是会话/并发槽被占满：与「账号不可用」是两种处置，
	// 不能共用「暂无可用账号」的说法，否则运维会去查号池而不是查并发。
	if selectionErr.SessionSlotsFull > 0 {
		return noAccountErrorClassification{
			Status:  http.StatusServiceUnavailable,
			ErrType: "service_unavailable",
			Message: infraerrors.AccountSlotConcurrencyLimit,
		}
	}
	return fallback
}

// allAccountsCoolingMessage 在选号原因能给出恢复时间时，把「哪个模型、还要等多久」
// 写进文案；给不出就退回既有的通用限速文案，行为与历史一致。
func allAccountsCoolingMessage(selectionErr *service.NoAvailableAccountsError) string {
	if selectionErr.Model == "" {
		return infraerrors.AllAccountsRateLimited
	}
	if selectionErr.NextModelRecovery > 0 {
		return fmt.Sprintf(infraerrors.AllAccountsModelCooling, selectionErr.Model, humanizeRecoveryWindow(selectionErr.NextModelRecovery))
	}
	return infraerrors.AllAccountsRateLimited
}

// humanizeRecoveryWindow 把剩余时间折算成中文口语单位，避免把 3h20m12.3s 这种
// 机器格式直接丢给调用方。
func humanizeRecoveryWindow(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()+0.5))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()+0.5))
	}
	hours := int(d / time.Hour)
	minutes := int((d % time.Hour) / time.Minute)
	if minutes == 0 {
		return fmt.Sprintf("%d 小时", hours)
	}
	return fmt.Sprintf("%d 小时 %d 分钟", hours, minutes)
}

// classifyNoAccountError decides between 404 model_not_found and 503
// api_error for "no available accounts" failures.
//
// The classifier intentionally does not consume the original error: the
// selection layer never tells us *why* the pool came up empty (rate-limited
// vs. unsupported model are both wrapped as ErrNoAvailableAccounts). Instead
// we re-check pool composition through DiagnoseModelAvailabilityForPlatform.
// Its dedicated database query considers only persistent eligibility
// (active status + schedulable setting) and model_mapping, bypassing scheduler
// snapshots and transient filters. That guarantees a 404 is only returned
// when persistent account/group/model configuration must change before the
// request can succeed.
//
// routingModel is the model name that account selection actually compared
// against (i.e. after group-level dispatch mapping). displayModel is the
// raw model the caller asked for; it is used only in the user-facing error
// message so that internal mapping details don't leak. Most callers pass
// the same value for both.
//
// platform is the platform the request was routed to (use
// service.PlatformOpenAI / PlatformAnthropic / PlatformGemini). It is
// required because Anthropic/Gemini routes additionally surface
// mixed-scheduled Antigravity accounts; passing the wrong platform would
// flip a legitimate 503 to a misleading 404 (or vice versa).
func classifyNoAccountError(
	ctx context.Context,
	diag service.ModelAvailabilityDiagnoser,
	apiKey *service.APIKey,
	routingModel string,
	displayModel string,
	platform string,
) noAccountErrorClassification {
	fallback := noAccountErrorClassification{
		Status:  http.StatusServiceUnavailable,
		ErrType: "api_error",
		Message: infraerrors.NoAvailableAccounts,
	}

	routingModel = strings.TrimSpace(routingModel)
	displayModel = strings.TrimSpace(displayModel)
	if displayModel == "" {
		displayModel = routingModel
	}
	if diag == nil || apiKey == nil || apiKey.GroupID == nil || routingModel == "" {
		return fallback
	}

	result := diag.DiagnoseModelAvailabilityForPlatform(ctx, apiKey.GroupID, routingModel, platform)
	if result.HasAccountsInPool && !result.HasModelSupport {
		message := fmt.Sprintf(infraerrors.ModelNotInGroup, displayModel)
		if result.HasRetiredSubscriptionModel {
			message = fmt.Sprintf(infraerrors.ModelRetiredSubscription, displayModel)
		}
		return noAccountErrorClassification{
			Status:        http.StatusNotFound,
			ErrType:       "model_not_found",
			Message:       message,
			ModelNotFound: true,
		}
	}
	return fallback
}

// classifyNoAccountErrorFromGin is a thin wrapper that forwards the gin
// context's underlying request context.
//
// cause 是选号层返回的原始错误。它必须从这里进来而不是由各调用点各自再补一行
// classifySelectionFailureError：历史上 12 个入口里只有 5 个补了，另外 7 个
// （含 /v1/messages）拿到的永远是兜底文案「该分组暂无可用账号」，同一件事在
// 不同协议上说法不一致。收敛到唯一入口后，新增入口不可能再漏。
func classifyNoAccountErrorFromGin(
	c *gin.Context,
	diag service.ModelAvailabilityDiagnoser,
	apiKey *service.APIKey,
	routingModel string,
	displayModel string,
	platform string,
	cause error,
) noAccountErrorClassification {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	classification := classifyNoAccountError(ctx, diag, apiKey, routingModel, displayModel, platform)
	classification = classifySelectionFailureError(cause, classification)
	if classification.ModelNotFound {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
	}
	return classification
}

func classifyOpenAICompatibleNoAccountErrorFromGin(
	c *gin.Context,
	diag service.ModelAvailabilityDiagnoser,
	apiKey *service.APIKey,
	routingModel string,
	displayModel string,
	cause error,
) noAccountErrorClassification {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	return classifyNoAccountErrorFromGin(
		c,
		diag,
		apiKey,
		routingModel,
		displayModel,
		openAICompatibleRequestPlatform(ctx, apiKey),
		cause,
	)
}

func openAICompatibleSelectionErrorForLog(err error, platform string) error {
	if err == nil || platform != service.PlatformGrok {
		return err
	}
	message := strings.ReplaceAll(err.Error(), "OpenAI accounts", "Grok accounts")
	if message == err.Error() {
		return err
	}
	return fmt.Errorf("%s", message)
}
