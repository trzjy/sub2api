package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	gocache "github.com/patrickmn/go-cache"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// 上下文水位提醒档位与回落阈值。
const (
	contextWarnBand1     = 650000
	contextWarnBand2     = 750000
	contextWarnBand3     = 800000
	contextWarnBand4     = 850000
	contextWarnDropFloor = 600000

	contextWarnThrottleTTL = 10 * time.Minute
)

// 进程内的水位与抑制状态。水位与抑制共用同一 cache，但 key 前缀不同。
var (
	contextWarnMu    sync.Mutex
	contextWarnCache = gocache.New(gocache.NoExpiration, time.Minute)
)

const (
	warnTextApproaching = "⚠️ [corealgos] 本会话上下文已达 %d 万，即将超过可靠区间，可能返回异常短回复；请压缩上下文或开新会话接力。"
	warnTextExceeded    = "⚠️ [corealgos] 本会话上下文已达 %d 万，已经超过可靠区间，可能返回异常短回复；请压缩上下文或开新会话接力。"
)

// ContextWarning 承载一次命中后的提醒结果；Text 为空表示不注入。
type ContextWarning struct {
	Text      string
	Band      int
	CtxTokens int
}

// DecideContextWarning 依据会话上下文 token 水位与账号抑制状态决定是否注入提醒。
// 所有共享状态的读写都在包级互斥锁内完成。
func DecideContextWarning(ctx context.Context, sessionHash string, accountID int64, ctxTokens int) ContextWarning {
	contextWarnMu.Lock()
	defer contextWarnMu.Unlock()

	if sessionHash == "" || ctxTokens <= 0 {
		return ContextWarning{}
	}

	band := contextWarnBandOf(ctxTokens)
	wmKey := "wm:" + sessionHash

	if band == 0 {
		if ctxTokens < contextWarnDropFloor {
			contextWarnCache.Delete(wmKey)
		}
		return ContextWarning{}
	}

	var watermark int
	if v, ok := contextWarnCache.Get(wmKey); ok {
		watermark, _ = v.(int)
	}
	if band <= watermark {
		return ContextWarning{}
	}

	thrKey := ""
	if accountID != 0 {
		thrKey = "thr:" + strconv.FormatInt(accountID, 10) + ":" + strconv.Itoa(band)
		if _, ok := contextWarnCache.Get(thrKey); ok {
			return ContextWarning{}
		}
	}

	if accountID != 0 {
		contextWarnCache.Set(thrKey, true, contextWarnThrottleTTL)
	}
	// 水位永不过期：只由"上下文回落到 600000 以下"清除，保证同会话同档只提醒一次。
	contextWarnCache.Set(wmKey, band, gocache.NoExpiration)

	logger.FromContext(ctx).Info("gateway.context_warning",
		zap.Int64("account_id", accountID),
		zap.String("session_hash", sessionHash),
		zap.Int("band", band),
		zap.Int("ctx_tokens", ctxTokens),
	)

	tmpl := warnTextApproaching
	if band == contextWarnBand3 || band == contextWarnBand4 {
		tmpl = warnTextExceeded
	}
	return ContextWarning{
		Text:      fmt.Sprintf(tmpl, ctxTokens/10000),
		Band:      band,
		CtxTokens: ctxTokens,
	}
}

// contextWarnBandOf 返回不超过 ctxTokens 的最高档位，低于最低档返回 0。
func contextWarnBandOf(ctxTokens int) int {
	switch {
	case ctxTokens >= contextWarnBand4:
		return contextWarnBand4
	case ctxTokens >= contextWarnBand3:
		return contextWarnBand3
	case ctxTokens >= contextWarnBand2:
		return contextWarnBand2
	case ctxTokens >= contextWarnBand1:
		return contextWarnBand1
	default:
		return 0
	}
}

// BuildContextWarningSSEAt 构造三个完整的 SSE 事件，使用入参 index 作为块索引，
// 便于在流结尾按实际 content block 数量追加提醒。三个事件均以空行结尾。
func BuildContextWarningSSEAt(index int, text string) string {
	textJSON, _ := json.Marshal(text)
	idx := strconv.Itoa(index)
	return "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":` + idx + `,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":` + idx + `,"delta":{"type":"text_delta","text":` + string(textJSON) + `}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":` + idx + `}` + "\n\n"
}

// errToolUsePresent 表示 content 中存在 tool_use 块；调用方据此跳过提醒注入且不推进水位。
var errToolUsePresent = errors.New("context warning: tool_use present in content")

// AppendContextWarningToJSON 在 {"content":[...]} 的数组尾部追加一个 text block，
// 其余字段保持原样。若任一元素为 tool_use，则返回原 body 与 errToolUsePresent。
func AppendContextWarningToJSON(body []byte, text string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}

	var content []json.RawMessage
	if raw, ok := obj["content"]; ok {
		if err := json.Unmarshal(raw, &content); err != nil {
			return nil, err
		}
		for _, block := range content {
			if gjson.GetBytes(block, "type").String() == "tool_use" {
				return body, errToolUsePresent
			}
		}
	}

	warning, err := json.Marshal(map[string]string{"type": "text", "text": text})
	if err != nil {
		return nil, err
	}
	content = append(content, warning)

	newContent, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	obj["content"] = newContent

	return json.Marshal(obj)
}
