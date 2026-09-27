package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	// accountModelCapabilityCachePrefix Redis 键前缀；账号维度分片。
	accountModelCapabilityCachePrefix = "account_model_capabilities:"
	// accountModelCapabilityPubSubKey 跨实例缓存失效广播 channel。
	accountModelCapabilityPubSubKey = "account_model_capabilities_invalidated"
	// accountModelCapabilityCacheTTL 缓存 TTL，对齐 error_passthrough 24h 习惯。
	accountModelCapabilityCacheTTL = 24 * time.Hour
)

// accountModelCapabilityCache 账号模型能力标记的 local+Redis 二级缓存。
//
// 对齐 error_passthrough_cache.go 的既有模式：本地缓存优先，Redis 兜底；
// Invalidate 双清；NotifyUpdate 走 Redis Pub/Sub 做跨实例失效广播
// （能力标记检测落库后必须即时生效，方案 §3.3）。
type accountModelCapabilityCache struct {
	rdb        *redis.Client
	local      map[int64][]*model.AccountModelCapability
	localMu    sync.RWMutex
	subscribed bool
}

// NewAccountModelCapabilityCache 创建能力标记二级缓存。
func NewAccountModelCapabilityCache(rdb *redis.Client) service.AccountModelCapabilityCache {
	return &accountModelCapabilityCache{
		rdb:   rdb,
		local: make(map[int64][]*model.AccountModelCapability),
	}
}

func capabilityCacheKey(accountID int64) string {
	return fmt.Sprintf("%s%d", accountModelCapabilityCachePrefix, accountID)
}

// GetAccount 从缓存获取某账号的标记列表。
func (c *accountModelCapabilityCache) GetAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, bool) {
	// 本地缓存优先。
	c.localMu.RLock()
	if caps, ok := c.local[accountID]; ok {
		c.localMu.RUnlock()
		return caps, true
	}
	c.localMu.RUnlock()

	// Redis 兜底。
	data, err := c.rdb.Get(ctx, capabilityCacheKey(accountID)).Bytes()
	if err != nil {
		if err != redis.Nil {
			// 非空错误：打印但不阻断读路径（回源 DB 兜底）。
			log.Printf("[AccountModelCapabilityCache] Failed to get from Redis: %v", err)
		}
		return nil, false
	}

	var caps []*model.AccountModelCapability
	if err := json.Unmarshal(data, &caps); err != nil {
		log.Printf("[AccountModelCapabilityCache] Failed to unmarshal: %v", err)
		return nil, false
	}

	c.localMu.Lock()
	c.local[accountID] = caps
	c.localMu.Unlock()
	return caps, true
}

// SetAccount 设置某账号的标记列表。
func (c *accountModelCapabilityCache) SetAccount(ctx context.Context, accountID int64, caps []*model.AccountModelCapability) error {
	data, err := json.Marshal(caps)
	if err != nil {
		return err
	}
	if err := c.rdb.Set(ctx, capabilityCacheKey(accountID), data, accountModelCapabilityCacheTTL).Err(); err != nil {
		return err
	}

	c.localMu.Lock()
	c.local[accountID] = caps
	c.localMu.Unlock()
	return nil
}

// InvalidateAccount 使某账号的缓存失效（清 local + Redis）。
func (c *accountModelCapabilityCache) InvalidateAccount(ctx context.Context, accountID int64) error {
	c.localMu.Lock()
	delete(c.local, accountID)
	c.localMu.Unlock()

	return c.rdb.Del(ctx, capabilityCacheKey(accountID)).Err()
}

// capabilityInvalidatePayload 跨实例缓存失效广播的消息体。
// account_id 为失效账号；suspect_redis=true 表示发布方 Redis 失效失败，订阅方须绕开
// Redis 直读 DB（方案 §3.3 失败关闭；合同 §5 禁兜底）。
type capabilityInvalidatePayload struct {
	AccountID    int64 `json:"account_id"`
	SuspectRedis bool  `json:"suspect_redis"`
}

// NotifyUpdate 广播某账号的缓存失效（跨实例通知）。
// 发布 JSON payload 以携带 suspectRedis 标志；滚动升级期旧订阅方解析失败时按保守的
// suspectRedis=true 处理（强制 DB 回源，见 SubscribeUpdates）。
func (c *accountModelCapabilityCache) NotifyUpdate(ctx context.Context, accountID int64, suspectRedis bool) error {
	payload, err := json.Marshal(capabilityInvalidatePayload{AccountID: accountID, SuspectRedis: suspectRedis})
	if err != nil {
		return err
	}
	return c.rdb.Publish(ctx, accountModelCapabilityPubSubKey, string(payload)).Err()
}

// parseCapabilityInvalidatePayload 解析广播消息体。
// 优先 JSON；解析失败回退 fmt.Sscanf 纯数字（滚动升级期旧发布者只发 accountID）→
// 一律按 suspectRedis=true（保守，强制 DB 回源）；两者都失败返回 error（调用方跳过该消息）。
func parseCapabilityInvalidatePayload(raw string, accountID *int64, suspectRedis *bool) error {
	var p capabilityInvalidatePayload
	if err := json.Unmarshal([]byte(raw), &p); err == nil {
		*accountID = p.AccountID
		*suspectRedis = p.SuspectRedis
		return nil
	}
	// 回退：旧格式纯数字 payload → 保守视为 Redis 可疑。
	var legacyID int64
	if _, err := fmt.Sscanf(raw, "%d", &legacyID); err == nil {
		*accountID = legacyID
		*suspectRedis = true
		return nil
	}
	return fmt.Errorf("invalid capability invalidate payload: %q", raw)
}

// SubscribeUpdates 订阅缓存失效广播；回调携带失效账号 ID 与发布方 Redis 失效是否成功。
// 订阅端清本地缓存的既有行为保留；suspectRedis 由上层 service 订阅回调据方案 §3.3 处理。
func (c *accountModelCapabilityCache) SubscribeUpdates(ctx context.Context, handler func(accountID int64, suspectRedis bool)) {
	if c.subscribed {
		return
	}
	c.subscribed = true

	go func() {
		sub := c.rdb.Subscribe(ctx, accountModelCapabilityPubSubKey)
		defer func() { _ = sub.Close() }()

		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-ch:
				if msg == nil {
					return
				}
				var accountID int64
				var suspectRedis bool
				if err := parseCapabilityInvalidatePayload(msg.Payload, &accountID, &suspectRedis); err != nil {
					// 解析失败（既非 JSON 也非纯数字）→ 跳过该消息，维持现状。
					continue
				}
				// 清空本地缓存，下次访问时从 Redis 或数据库重新加载。
				c.localMu.Lock()
				delete(c.local, accountID)
				c.localMu.Unlock()

				handler(accountID, suspectRedis)
			}
		}
	}()
}
