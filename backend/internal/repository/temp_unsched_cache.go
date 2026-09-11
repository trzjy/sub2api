package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const tempUnschedPrefix = "temp_unsched:account:"

const openAIAPIKeyHealthFailurePrefix = "openai_apikey_health:"

// openAIAPIKeyHealthFailureScript records one failure in the account's rolling
// window and reports which health-breaker tiers (watch/warning/trip) this record
// newly crossed. The tier state is persisted in a companion key so that repeated
// failures inside the same window only escalate/alert once (debounce): the stored
// tier only ever goes up. On reaching the trip tier the whole window is cleared so
// a fresh window starts after the account is released from cooldown.
//
// ARGV: [windowMinutes, tripThreshold, warningThreshold, watchThreshold]
// KEYS: [failureZSetKey, tierStateKey]
// Returns: { count, trippedTrip, trippedWatch, trippedWarning, trippedTripOnce }
var openAIAPIKeyHealthFailureScript = redis.NewScript(`
	local key = KEYS[1]
	local tier_key = KEYS[2]
	local sequence_key = key .. ':sequence'
	local now = redis.call('TIME')
	local now_ms = (tonumber(now[1]) * 1000) + math.floor(tonumber(now[2]) / 1000)
	local window_ms = tonumber(ARGV[1]) * 60 * 1000
	local trip_threshold = tonumber(ARGV[2])
	local warning_threshold = tonumber(ARGV[3])
	local watch_threshold = tonumber(ARGV[4])
	local sequence = redis.call('INCR', sequence_key)

	redis.call('ZREMRANGEBYSCORE', key, '-inf', now_ms - window_ms)
	redis.call('ZADD', key, now_ms, tostring(now_ms) .. ':' .. tostring(sequence))
	local count = redis.call('ZCARD', key)
	local ttl = math.max(60, (tonumber(ARGV[1]) + 1) * 60)

	local function tier_of(c)
		if c >= trip_threshold then return 3 end
		if c >= warning_threshold then return 2 end
		if c >= watch_threshold then return 1 end
		return 0
	end

	local current_tier = tier_of(count)
	local stored_tier = tonumber(redis.call('GET', tier_key)) or 0
	local tripped_watch = (current_tier >= 1 and stored_tier < 1)
	local tripped_warning = (current_tier >= 2 and stored_tier < 2)
	local tripped_trip = (current_tier >= 3 and stored_tier < 3)

	if current_tier > stored_tier then
		redis.call('SET', tier_key, tostring(current_tier), 'EX', ttl)
	end

	if current_tier >= 3 then
		redis.call('DEL', key, sequence_key, tier_key)
		return {count, 1, (tripped_watch and 1 or 0), (tripped_warning and 1 or 0), (tripped_trip and 1 or 0)}
	end

	redis.call('EXPIRE', key, ttl)
	redis.call('EXPIRE', sequence_key, ttl)
	return {count, 0, (tripped_watch and 1 or 0), (tripped_warning and 1 or 0), (tripped_trip and 1 or 0)}
`)

var tempUnschedSetScript = redis.NewScript(`
	local key = KEYS[1]
	local new_until = tonumber(ARGV[1])
	local new_value = ARGV[2]
	local new_ttl = tonumber(ARGV[3])

	local existing = redis.call('GET', key)
	if existing then
		local ok, existing_data = pcall(cjson.decode, existing)
		if ok and existing_data and existing_data.until_unix then
			local existing_until = tonumber(existing_data.until_unix)
			if existing_until and new_until <= existing_until then
				return 0
			end
		end
	end

	redis.call('SET', key, new_value, 'EX', new_ttl)
	return 1
`)

type tempUnschedCache struct {
	rdb *redis.Client
}

func NewTempUnschedCache(rdb *redis.Client) service.TempUnschedCache {
	return &tempUnschedCache{rdb: rdb}
}

// SetTempUnsched 设置临时不可调度状态（只延长不缩短）
func (c *tempUnschedCache) SetTempUnsched(ctx context.Context, accountID int64, state *service.TempUnschedState) error {
	key := fmt.Sprintf("%s%d", tempUnschedPrefix, accountID)

	stateJSON, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	ttl := time.Until(time.Unix(state.UntilUnix, 0))
	if ttl <= 0 {
		return nil // 已过期，不设置
	}

	ttlSeconds := int(ttl.Seconds())
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}

	_, err = tempUnschedSetScript.Run(ctx, c.rdb, []string{key}, state.UntilUnix, string(stateJSON), ttlSeconds).Result()
	return err
}

// GetTempUnsched 获取临时不可调度状态
func (c *tempUnschedCache) GetTempUnsched(ctx context.Context, accountID int64) (*service.TempUnschedState, error) {
	key := fmt.Sprintf("%s%d", tempUnschedPrefix, accountID)

	val, err := c.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var state service.TempUnschedState
	if err := json.Unmarshal([]byte(val), &state); err != nil {
		return nil, fmt.Errorf("unmarshal state: %w", err)
	}

	return &state, nil
}

// DeleteTempUnsched 删除临时不可调度状态
func (c *tempUnschedCache) DeleteTempUnsched(ctx context.Context, accountID int64) error {
	key := fmt.Sprintf("%s%d", tempUnschedPrefix, accountID)
	return c.rdb.Del(ctx, key).Err()
}

func (c *tempUnschedCache) openAIAPIKeyHealthKey(accountID int64) string {
	// The hash tag keeps the rolling window and sequence key in one Redis
	// Cluster slot even though the Lua script derives the latter dynamically.
	return fmt.Sprintf("%s{%d}:failures", openAIAPIKeyHealthFailurePrefix, accountID)
}

func (c *tempUnschedCache) openAIAPIKeyHealthTierKey(accountID int64) string {
	// Companion key storing the highest tier escalated within the current window
	// (used for debounce). Shares the same hash tag as the failure zset.
	return fmt.Sprintf("%s{%d}:tier", openAIAPIKeyHealthFailurePrefix, accountID)
}

func (c *tempUnschedCache) RecordOpenAIAPIKeyHealthFailure(ctx context.Context, accountID int64, windowMinutes, watchThreshold, warningThreshold, tripThreshold int) (service.OpenAIAPIKeyHealthRecordResult, error) {
	if windowMinutes < 1 {
		windowMinutes = 1
	}
	if watchThreshold < 1 {
		watchThreshold = 1
	}
	if warningThreshold < 1 {
		warningThreshold = 1
	}
	if tripThreshold < 1 {
		tripThreshold = 1
	}
	// Safety clamp so the Lua tier_of evaluates monotonically.
	if warningThreshold >= tripThreshold {
		warningThreshold = tripThreshold - 1
	}
	if watchThreshold >= warningThreshold {
		watchThreshold = warningThreshold - 1
	}
	if watchThreshold < 1 {
		watchThreshold = 1
	}

	result, err := openAIAPIKeyHealthFailureScript.Run(ctx, c.rdb,
		[]string{c.openAIAPIKeyHealthKey(accountID), c.openAIAPIKeyHealthTierKey(accountID)},
		windowMinutes, tripThreshold, warningThreshold, watchThreshold,
	).Slice()
	if err != nil {
		return service.OpenAIAPIKeyHealthRecordResult{}, fmt.Errorf("record OpenAI API key health failure: %w", err)
	}
	if len(result) != 5 {
		return service.OpenAIAPIKeyHealthRecordResult{}, fmt.Errorf("record OpenAI API key health failure: unexpected result length %d", len(result))
	}
	count, countOK := result[0].(int64)
	watch, _ := result[2].(int64)
	warning, _ := result[3].(int64)
	trip, _ := result[4].(int64)
	if !countOK {
		return service.OpenAIAPIKeyHealthRecordResult{}, fmt.Errorf("record OpenAI API key health failure: unexpected result types %T", result[0])
	}
	return service.OpenAIAPIKeyHealthRecordResult{
		Count:          count,
		TrippedWatch:   watch == 1,
		TrippedWarning: warning == 1,
		TrippedTrip:    trip == 1,
	}, nil
}

// ClearOpenAIAPIKeyHealth was removed: the breaker no longer resets the rolling
// failure window on a successful schedule, because that defeated the window for
// flaky channels and added a hot-path Redis write. The window decays via TTL, and
// the L3 block path persists its own state.
