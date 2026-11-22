package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// cursorSessionSnapshotRedisStore 是 cursorSessionSnapshotStore 的生产实现：
// 把快照 JSON 写进 Redis，键前缀与 TTL 在构造时固定（方案 §2.3，用户裁定 4 小时）。
type cursorSessionSnapshotRedisStore struct {
	rdb    *redis.Client
	prefix string
	ttl    time.Duration
}

func newCursorSessionSnapshotRedisStore(rdb *redis.Client) *cursorSessionSnapshotRedisStore {
	return &cursorSessionSnapshotRedisStore{
		rdb:    rdb,
		prefix: "cursor:session:",
		ttl:    cursorSessionSnapshotTTL,
	}
}

func (s *cursorSessionSnapshotRedisStore) key(id string) string {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, s.prefix) {
		return id
	}
	return s.prefix + id
}

func (s *cursorSessionSnapshotRedisStore) Set(ctx context.Context, id string, value any) error {
	if s == nil || s.rdb == nil {
		return errors.New("cursor session snapshot store not configured")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return s.rdb.Set(ctx, s.key(id), raw, s.ttl).Err()
}

func (s *cursorSessionSnapshotRedisStore) Get(ctx context.Context, id string, dest any) (bool, error) {
	if s == nil || s.rdb == nil {
		return false, errors.New("cursor session snapshot store not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := s.rdb.Get(ctx, s.key(id)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return false, err
	}
	return true, nil
}

func (s *cursorSessionSnapshotRedisStore) Del(ctx context.Context, id string) error {
	if s == nil || s.rdb == nil {
		return errors.New("cursor session snapshot store not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return s.rdb.Del(ctx, s.key(id)).Err()
}
