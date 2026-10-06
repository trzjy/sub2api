package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// probeMigrationRepoStub implements SettingRepository plus the atomic migration /
// rollback capabilities with a deterministic, strictly-increasing updated_at clock
// so the CAS rollback can be exercised reliably: every Set advances the clock, so
// "after migration" edits always sort strictly after the recorded MigratedAt. The
// clock is nanosecond-granular to emulate a real DB timestamptz column whose values
// carry sub-second precision (the #2 regression), so the recorded MigratedAt must
// be stored and compared losslessly.
//
// concurrentEditBeforeCAS, when set, runs immediately before a conditional write is
// evaluated, letting a test model a human edit that lands between the rollback's
// logical "compare" and "write". Because the real repository performs both in one
// SQL statement, this hook is how the stub reproduces the interleaving deterministically.
type probeMigrationRepoStub struct {
	mu                      sync.Mutex
	values                  map[string]string
	updatedAt               map[string]time.Time
	clock                   int64
	concurrentEditBeforeCAS func()
	// failRecordWrite, when true, makes MigrateSettingAtomically fail while
	// writing the bookkeeping record. The stub then rolls the whole "transaction"
	// back, so a test can prove the main setting flip does not survive alone.
	failRecordWrite bool
	// failRecordDelete, when true, makes RollbackSettingAtomically fail while
	// deleting the bookkeeping record. The stub then rolls the whole
	// "transaction" back, so a test can prove the restored main setting does not
	// survive without the bookkeeping record being removed.
	failRecordDelete bool
	// atomicCalls counts how many times the atomic migration path was taken, so a
	// test can assert the migration used the single-transaction capability.
	atomicCalls int
	// rollbackAtomicCalls counts how many times the atomic rollback path was
	// taken, so a test can assert the rollback used the single-transaction
	// capability.
	rollbackAtomicCalls int
}

func newProbeMigrationRepoStub(initial map[string]string) *probeMigrationRepoStub {
	s := &probeMigrationRepoStub{
		values:    map[string]string{},
		updatedAt: map[string]time.Time{},
		clock:     1,
	}
	for k, v := range initial {
		s.values[k] = v
		s.updatedAt[k] = time.Unix(0, s.clock).UTC()
		s.clock++
	}
	return s
}

// tick returns the next strictly-increasing nanosecond timestamp.
func (s *probeMigrationRepoStub) tick() time.Time {
	ts := time.Unix(0, s.clock).UTC()
	s.clock++
	return ts
}

func (s *probeMigrationRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key]
	if !ok {
		return nil, ErrSettingNotFound
	}
	return &Setting{Key: key, Value: v, UpdatedAt: s.updatedAt[key]}, nil
}

func (s *probeMigrationRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}

func (s *probeMigrationRepoStub) Set(ctx context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	s.updatedAt[key] = s.tick()
	return nil
}

// RollbackSettingAtomically mirrors the real repository's single-transaction
// rollback: the conditional (compare-and-set) restore of the main setting and the
// removal of the bookkeeping record are applied together, and any failure
// (failRecordDelete) reverts the whole "transaction" so the restored setting
// cannot survive with the bookkeeping record dangling. The concurrentEditBeforeCAS
// hook runs before the conditional action, exactly like the real single statement
// where the compare and the write are indivisible.
//
// A zero affected-row count means the CAS refused (a human edit landed after the
// migration) and the bookkeeping record must be left untouched. The absent
// pre-image case (restoreValue == nil) disambiguates a zero-row conditional delete
// with an existence read: an already-absent row satisfies the target state
// (affected = 1); a still-present row was modified after the migration (refused).
func (s *probeMigrationRepoStub) RollbackSettingAtomically(
	ctx context.Context,
	settingsKey string,
	restoreValue *string,
	expectedUpdatedAt time.Time,
	recordKey string,
) (int, error) {
	if s.concurrentEditBeforeCAS != nil {
		s.concurrentEditBeforeCAS()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollbackAtomicCalls++

	snapshotValues := make(map[string]string, len(s.values))
	snapshotUpdatedAt := make(map[string]time.Time, len(s.updatedAt))
	for k, v := range s.values {
		snapshotValues[k] = v
	}
	for k, v := range s.updatedAt {
		snapshotUpdatedAt[k] = v
	}
	restore := func() {
		s.values = snapshotValues
		s.updatedAt = snapshotUpdatedAt
	}

	ts, ok := s.updatedAt[settingsKey]
	if restoreValue == nil {
		// Absent pre-image: delete only when the row still carries the migration's
		// updated_at. Zero rows is fine when the row is already absent (target state
		// holds) but a refusal when the row is present with a changed updated_at.
		if ok && !ts.Equal(expectedUpdatedAt) {
			return 0, nil
		}
		if ok {
			delete(s.values, settingsKey)
			delete(s.updatedAt, settingsKey)
		}
	} else {
		if !ok || !ts.Equal(expectedUpdatedAt) {
			return 0, nil
		}
		s.values[settingsKey] = *restoreValue
		s.updatedAt[settingsKey] = s.tick()
	}

	if s.failRecordDelete {
		restore()
		return 0, errors.New("injected bookkeeping delete failure")
	}
	delete(s.values, recordKey)
	delete(s.updatedAt, recordKey)
	return 1, nil
}

func (s *probeMigrationRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := s.values[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (s *probeMigrationRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	for k, v := range settings {
		if err := s.Set(ctx, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *probeMigrationRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.values {
		out[k] = v
	}
	return out, nil
}

func (s *probeMigrationRepoStub) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
	delete(s.updatedAt, key)
	return nil
}

// MigrateSettingAtomically mirrors the real repository's single-transaction
// compare-and-set commit: the main setting write is gated on the pre-image version
// condition (expectedExists / expectedUpdatedAt) inside the "transaction", and
// both writes are applied together. Any CAS refusal returns
// ErrSettingMigrationConcurrentModified and writes nothing; any failure
// (failRecordWrite) reverts the whole "transaction" so the main flip cannot
// survive without its rollback credential.
func (s *probeMigrationRepoStub) MigrateSettingAtomically(
	ctx context.Context,
	settingsKey, settingsValue string,
	recordKey string,
	buildRecord func(updatedAt time.Time) (string, error),
	expectedExists bool,
	expectedUpdatedAt time.Time,
) (time.Time, error) {
	if s.concurrentEditBeforeCAS != nil {
		s.concurrentEditBeforeCAS()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.atomicCalls++

	snapshotValues := make(map[string]string, len(s.values))
	snapshotUpdatedAt := make(map[string]time.Time, len(s.updatedAt))
	for k, v := range s.values {
		snapshotValues[k] = v
	}
	for k, v := range s.updatedAt {
		snapshotUpdatedAt[k] = v
	}
	restore := func() {
		s.values = snapshotValues
		s.updatedAt = snapshotUpdatedAt
	}

	// CAS gate: refuse when the persisted row no longer matches the pre-image the
	// service captured before building the new value.
	if expectedExists {
		ts, ok := s.updatedAt[settingsKey]
		if !ok || !ts.Equal(expectedUpdatedAt) {
			return time.Time{}, ErrSettingMigrationConcurrentModified
		}
	} else {
		// Pre-image was absent: model INSERT ... ON CONFLICT DO NOTHING judged by
		// affected rows. If a concurrent writer already created the row — with the
		// same OR a different value — the insert affects zero rows and must be
		// refused. Ownership is decided by the affected-row count, never by value
		// equality (which would mis-credit a concurrent row of the same value).
		if _, ok := s.updatedAt[settingsKey]; ok {
			return time.Time{}, ErrSettingMigrationConcurrentModified
		}
	}

	settingsUpdatedAt := s.tick()
	s.values[settingsKey] = settingsValue
	s.updatedAt[settingsKey] = settingsUpdatedAt

	if s.failRecordWrite {
		restore()
		return time.Time{}, errors.New("injected bookkeeping write failure")
	}

	recordValue, err := buildRecord(settingsUpdatedAt)
	if err != nil {
		restore()
		return time.Time{}, err
	}
	s.values[recordKey] = recordValue
	s.updatedAt[recordKey] = s.tick()
	return settingsUpdatedAt, nil
}

// plainSettingRepoStub implements only the base SettingRepository interface,
// deliberately without the CAS or atomic-migration capabilities, so tests can
// prove the migration fails closed instead of falling back to a sequential
// two-write path.
type plainSettingRepoStub struct {
	mu     sync.Mutex
	values map[string]string
}

func newPlainSettingRepoStub(initial map[string]string) *plainSettingRepoStub {
	s := &plainSettingRepoStub{values: map[string]string{}}
	for k, v := range initial {
		s.values[k] = v
	}
	return s
}

func (s *plainSettingRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key]
	if !ok {
		return nil, ErrSettingNotFound
	}
	return &Setting{Key: key, Value: v, UpdatedAt: time.Unix(0, 1).UTC()}, nil
}

func (s *plainSettingRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}

func (s *plainSettingRepoStub) Set(ctx context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	return nil
}

func (s *plainSettingRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := s.values[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (s *plainSettingRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	for k, v := range settings {
		if err := s.Set(ctx, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *plainSettingRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.values {
		out[k] = v
	}
	return out, nil
}

func (s *plainSettingRepoStub) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
	return nil
}

func TestProbeEnabledForCandidate(t *testing.T) {
	t.Run("circuit breaker keeps dual-switch short-circuit", func(t *testing.T) {
		// 顶层关即 false，不论 Probe.Enabled
		require.False(t, ProbeEnabledForCandidate(&OpenAIAPIKeyHealthBreakerSettings{
			Enabled: false,
			Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: true},
		}, ProbeCandidateCircuitBreaker))
		// 双开 → true
		require.True(t, ProbeEnabledForCandidate(&OpenAIAPIKeyHealthBreakerSettings{
			Enabled: true,
			Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: true},
		}, ProbeCandidateCircuitBreaker))
		// Probe 关 → false
		require.False(t, ProbeEnabledForCandidate(&OpenAIAPIKeyHealthBreakerSettings{
			Enabled: true,
			Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: false},
		}, ProbeCandidateCircuitBreaker))
	})

	t.Run("nil probe → disabled", func(t *testing.T) {
		require.False(t, ProbeEnabledForCandidate(&OpenAIAPIKeyHealthBreakerSettings{Enabled: true}, ProbeCandidateCircuitBreaker))
		require.False(t, ProbeEnabledForCandidate(nil, ProbeCandidateCircuitBreaker))
	})
}

func TestProbeMigrationOpenAIAPIKeyHealthBreaker(t *testing.T) {
	ctx := context.Background()

	t.Run("flips persisted false to true, admin can re-close", func(t *testing.T) {
		raw := `{"enabled":false,"probe":{"enabled":false,"interval_seconds":60,"max_attempts":10}}`
		repo := newProbeMigrationRepoStub(map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
		})
		svc := NewSettingService(repo, &config.Config{})

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))

		got, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
		require.NoError(t, err)
		require.True(t, got.Probe.Enabled, "迁移后 Probe.Enabled 应为 true")
		require.False(t, got.Enabled, "顶层熔断总开关保持 false")

		// 管理端可再关
		got.Probe.Enabled = false
		require.NoError(t, svc.SetOpenAIAPIKeyHealthBreakerSettings(ctx, got))
		got2, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
		require.NoError(t, err)
		require.False(t, got2.Probe.Enabled, "管理端可再关")
	})

	t.Run("idempotent: second run leaves bookkeeping stable", func(t *testing.T) {
		raw := `{"enabled":false,"probe":{"enabled":false}}`
		repo := newProbeMigrationRepoStub(map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
		})
		svc := NewSettingService(repo, &config.Config{})

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
		rec1, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, err)

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
		rec2, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, err)
		require.Equal(t, rec1, rec2, "幂等：记账稳定不重复生效")
	})

	t.Run("already enabled → still bookkeeps, settings byte-identical", func(t *testing.T) {
		raw := `{"enabled":false,"probe":{"enabled":true}}`
		repo := newProbeMigrationRepoStub(map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
		})
		svc := NewSettingService(repo, &config.Config{})

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))

		// Bookkeeping marker must now be written even though no flip happened.
		rec, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, err, "已启用也必须写迁移记账标记")
		require.Equal(t, 1, repo.atomicCalls, "已启用仍走单事务原子路径")

		// Settings value must be unchanged byte-for-byte.
		got, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
		require.NoError(t, err)
		require.True(t, got.Probe.Enabled, "已启用保持 true")
		cur, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
		require.NoError(t, err)
		require.Equal(t, raw, cur, "已启用设置值逐字节不变")

		// Record pre-image must capture the (already-enabled) current value.
		var parsed openAIAPIKeyHealthBreakerProbeMigrationRecord
		require.NoError(t, json.Unmarshal([]byte(rec), &parsed))
		require.Equal(t, raw, parsed.PreImage, "pre_image 保留当前(已启用)状态")
		require.True(t, parsed.PreImageExists)
	})

	t.Run("already enabled, bookkeeping exists → zero writes, idempotent early-retreat", func(t *testing.T) {
		raw := `{"enabled":false,"probe":{"enabled":true}}`
		repo := newProbeMigrationRepoStub(map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
		})
		svc := NewSettingService(repo, &config.Config{})

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
		require.Equal(t, 1, repo.atomicCalls)
		rec1, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, err)

		// Second run must short-circuit on the existing bookkeeping record: no new
		// atomic write, and the marker stays stable.
		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
		require.Equal(t, 1, repo.atomicCalls, "标记已存在 → 幂等零写入")
		rec2, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, err)
		require.Equal(t, rec1, rec2, "幂等：记账稳定不重复生效")

		cur, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
		require.NoError(t, err)
		require.Equal(t, raw, cur, "已启用设置值始终逐字节不变")
	})

	t.Run("already enabled then admin closes probe → rollback refuses (CAS)", func(t *testing.T) {
		raw := `{"enabled":false,"probe":{"enabled":true}}`
		repo := newProbeMigrationRepoStub(map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
		})
		svc := NewSettingService(repo, &config.Config{})

		// Migration writes the bookkeeping marker without flipping the value.
		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))

		// Admin manually closes the probe (advances the row's updated_at).
		got, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
		require.NoError(t, err)
		require.True(t, got.Probe.Enabled)
		got.Probe.Enabled = false
		require.NoError(t, svc.SetOpenAIAPIKeyHealthBreakerSettings(ctx, got))

		// Rollback must NOT restore to enabled: CAS sees the changed updated_at and
		// refuses, leaving the admin's manual close intact.
		err = svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx)
		require.ErrorIs(t, err, ErrSettingMigrationCannotSafelyRollback)

		got2, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
		require.NoError(t, err)
		require.False(t, got2.Probe.Enabled, "回滚不得覆盖管理员已关的 Probe")
	})

	t.Run("no atomic capability → fails closed, no partial write", func(t *testing.T) {
		raw := `{"enabled":false,"probe":{"enabled":false}}`
		repo := newPlainSettingRepoStub(map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
		})
		svc := NewSettingService(repo, &config.Config{})

		err := svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx)
		require.Error(t, err)
		require.ErrorContains(t, err, "setting repository does not support atomic migration")

		// Fail closed: neither the flip nor any bookkeeping record may appear.
		cur, gerr := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
		require.NoError(t, gerr)
		require.Equal(t, raw, cur, "缺原子能力不得写半程（主设置必须保持原状）")

		_, gerr = repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.ErrorIs(t, gerr, ErrSettingNotFound, "缺原子能力不得写记账记录")
	})

	t.Run("no atomic capability, setting absent → fails closed, nothing created", func(t *testing.T) {
		repo := newPlainSettingRepoStub(map[string]string{})
		svc := NewSettingService(repo, &config.Config{})

		err := svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx)
		require.Error(t, err)
		require.ErrorContains(t, err, "setting repository does not support atomic migration")

		_, gerr := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
		require.ErrorIs(t, gerr, ErrSettingNotFound, "缺原子能力不得创建 setting")
		_, gerr = repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.ErrorIs(t, gerr, ErrSettingNotFound, "缺原子能力不得写记账记录")
	})

	t.Run("setting absent → created enabled, bookkeeping flags pre-image absent", func(t *testing.T) {
		repo := newProbeMigrationRepoStub(map[string]string{})
		svc := NewSettingService(repo, &config.Config{})

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
		got, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
		require.NoError(t, err)
		require.True(t, got.Probe.Enabled)

		rec, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, err)
		require.Contains(t, rec, `"pre_image":""`, "原不存在 → pre_image 为空串")
		require.Contains(t, rec, `"pre_image_exists":false`, "原不存在 → 存在性标记 false")
	})
}

func TestProbeRollbackOpenAIAPIKeyHealthBreaker(t *testing.T) {
	ctx := context.Background()

	t.Run("not modified → restore pre-image and drop bookkeeping", func(t *testing.T) {
		raw := `{"enabled":false,"probe":{"enabled":false}}`
		repo := newProbeMigrationRepoStub(map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
		})
		svc := NewSettingService(repo, &config.Config{})

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
		require.NoError(t, svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx))

		got, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
		require.NoError(t, err)
		require.False(t, got.Probe.Enabled, "回滚到迁移前 false")

		_, err = repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.ErrorIs(t, err, ErrSettingNotFound, "记账 key 被删")
	})

	t.Run("modified after migration → cannot rollback, no overwrite", func(t *testing.T) {
		raw := `{"enabled":false,"probe":{"enabled":false}}`
		repo := newProbeMigrationRepoStub(map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
		})
		svc := NewSettingService(repo, &config.Config{})

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))

		// 人工修改：管理端再关
		got, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
		require.NoError(t, err)
		got.Probe.Enabled = false
		require.NoError(t, svc.SetOpenAIAPIKeyHealthBreakerSettings(ctx, got))

		err = svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx)
		require.ErrorIs(t, err, ErrSettingMigrationCannotSafelyRollback)

		got2, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
		require.NoError(t, err)
		require.False(t, got2.Probe.Enabled, "不覆盖人工修改")

		_, berr := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, berr, "记账 key 保留")
	})

	t.Run("sub-second DB timestamp → rollback succeeds (#2 regression)", func(t *testing.T) {
		// Seed the settings row with an updated_at that carries a non-zero sub-second
		// component, exactly like a Postgres timestamptz column. A second-level
		// RFC3339 record would truncate this and the rollback would wrongly refuse.
		raw := `{"enabled":false,"probe":{"enabled":false}}`
		repo := newProbeMigrationRepoStub(map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
		})
		seed := time.Date(2026, 10, 3, 4, 30, 15, 123456789, time.UTC)
		repo.mu.Lock()
		repo.updatedAt[SettingKeyOpenAIAPIKeyHealthBreakerSettings] = seed
		repo.mu.Unlock()
		svc := NewSettingService(repo, &config.Config{})

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))

		// The recorded audit fact must preserve the sub-second digits.
		recRaw, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, err)
		var rec openAIAPIKeyHealthBreakerProbeMigrationRecord
		require.NoError(t, json.Unmarshal([]byte(recRaw), &rec))
		parsed, err := time.Parse(time.RFC3339Nano, rec.MigratedAt)
		require.NoError(t, err)
		require.NotZero(t, parsed.Nanosecond(), "迁移记录必须保留 DB 小数秒，否则回滚恒失败")
		require.True(t, parsed.Equal(repo.updatedAt[SettingKeyOpenAIAPIKeyHealthBreakerSettings]),
			"记录值必须无损等于 DB updated_at")

		require.NoError(t, svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx),
			"未修改且 DB 含小数秒 → 必须能安全回滚")

		got, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
		require.NoError(t, err)
		require.False(t, got.Probe.Enabled, "回滚到迁移前 false")
	})

	t.Run("concurrent edit between compare and write → refuse, manual change preserved (#3 regression)", func(t *testing.T) {
		raw := `{"enabled":false,"probe":{"enabled":false}}`
		repo := newProbeMigrationRepoStub(map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
		})
		svc := NewSettingService(repo, &config.Config{})
		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))

		// Model a human edit that lands after the rollback read the migration record
		// but before its conditional write executes. The conditional update then sees
		// a changed updated_at and must affect zero rows.
		humanRaw := `{"enabled":false,"probe":{"enabled":false},"window_minutes":45}`
		repo.concurrentEditBeforeCAS = func() {
			repo.concurrentEditBeforeCAS = nil // fire once
			require.NoError(t, repo.Set(context.Background(), SettingKeyOpenAIAPIKeyHealthBreakerSettings, humanRaw))
		}

		err := svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx)
		require.ErrorIs(t, err, ErrSettingMigrationCannotSafelyRollback)

		// The manual change must be intact (not overwritten by the pre-image).
		cur, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
		require.NoError(t, err)
		require.Equal(t, humanRaw, cur, "并发人工修改必须保留")

		_, berr := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, berr, "记账 key 保留")
	})

	t.Run("not applied → ErrSettingMigrationNotApplied", func(t *testing.T) {
		repo := newProbeMigrationRepoStub(map[string]string{})
		svc := NewSettingService(repo, &config.Config{})
		err := svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx)
		require.ErrorIs(t, err, ErrSettingMigrationNotApplied)
	})

	t.Run("not modified, originally absent → setting deleted", func(t *testing.T) {
		repo := newProbeMigrationRepoStub(map[string]string{})
		svc := NewSettingService(repo, &config.Config{})

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
		_, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
		require.NoError(t, err, "迁移创建了 setting")

		require.NoError(t, svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx))
		_, err = repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
		require.ErrorIs(t, err, ErrSettingNotFound, "原不存在 → 回滚后删除")
	})

	t.Run("pre-image present with empty value → restore empty, not delete (#5 regression)", func(t *testing.T) {
		// The setting row exists but its persisted value is the legal empty
		// string. PreImage alone cannot tell this apart from "absent", so the
		// rollback must consult PreImageExists: restoring the empty value (not
		// deleting the row) is the only correct target state, otherwise the CAS
		// rollback destroys an operator's existing row.
		repo := newProbeMigrationRepoStub(map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: "",
		})
		svc := NewSettingService(repo, &config.Config{})

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))

		// The record must flag the pre-image as present despite its empty value.
		recRaw, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, err)
		var rec openAIAPIKeyHealthBreakerProbeMigrationRecord
		require.NoError(t, json.Unmarshal([]byte(recRaw), &rec))
		require.Empty(t, rec.PreImage)
		require.True(t, rec.PreImageExists, "空串 pre-image 必须标记为存在")

		require.NoError(t, svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx))

		cur, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
		require.NoError(t, err, "存在且值为空串的 setting 不得被回滚删除")
		require.Equal(t, "", cur, "回滚恢复迁移前的空串值")

		_, err = repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.ErrorIs(t, err, ErrSettingNotFound, "记账 key 被删")
	})

	t.Run("pre-image present empty, modified after migration → still refuse (#5 regression)", func(t *testing.T) {
		// The empty pre-image must not weaken CAS refusal: a human edit after the
		// migration still moves updated_at and must abort the rollback.
		repo := newProbeMigrationRepoStub(map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: "",
		})
		svc := NewSettingService(repo, &config.Config{})

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
		humanRaw := `{"enabled":false,"probe":{"enabled":false}}`
		require.NoError(t, repo.Set(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings, humanRaw))

		err := svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx)
		require.ErrorIs(t, err, ErrSettingMigrationCannotSafelyRollback)

		cur, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
		require.NoError(t, err)
		require.Equal(t, humanRaw, cur, "不覆盖人工修改")

		_, err = repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, err, "记账 key 保留")
	})

	t.Run("idempotency consumes pre-image flag, absent pre-image still deletes (#5 regression)", func(t *testing.T) {
		// A second migration run must not rewrite the bookkeeping record, and the
		// flag it already stored (absent) must keep driving the delete branch.
		repo := newProbeMigrationRepoStub(map[string]string{})
		svc := NewSettingService(repo, &config.Config{})

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
		rec1, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, err)

		require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
		rec2, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
		require.NoError(t, err)
		require.Equal(t, rec1, rec2, "幂等：含存在性标记的记账稳定不重复生效")

		require.NoError(t, svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx))
		_, err = repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
		require.ErrorIs(t, err, ErrSettingNotFound, "存在性标记 false → 仍走删除分支")
	})
}

// TestMigrateOpenAIAPIKeyHealthBreakerProbeAtomicCommit proves the one-shot R1
// migration performs both writes (the flipped setting and its bookkeeping
// record) through the single-transaction capability, so they cannot diverge.
func TestMigrateOpenAIAPIKeyHealthBreakerProbeAtomicCommit(t *testing.T) {
	ctx := context.Background()
	raw := `{"enabled":false,"probe":{"enabled":false}}`
	repo := newProbeMigrationRepoStub(map[string]string{
		SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
	})
	svc := NewSettingService(repo, &config.Config{})

	require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
	require.Equal(t, 1, repo.atomicCalls, "迁移必须走单事务原子能力")

	got, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
	require.NoError(t, err)
	require.True(t, got.Probe.Enabled, "迁移后 Probe.Enabled 应为 true")

	recRaw, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
	require.NoError(t, err, "原子事务同时写入迁移记录")

	// The recorded MigratedAt must equal the main setting row's updated_at, the
	// fact the CAS rollback later compares against.
	var rec openAIAPIKeyHealthBreakerProbeMigrationRecord
	require.NoError(t, json.Unmarshal([]byte(recRaw), &rec))
	require.Equal(t, raw, rec.PreImage, "pre_image 保留迁移前状态")
	parsed, err := time.Parse(time.RFC3339Nano, rec.MigratedAt)
	require.NoError(t, err)
	require.True(t, parsed.Equal(repo.updatedAt[SettingKeyOpenAIAPIKeyHealthBreakerSettings]),
		"记账的 MigratedAt 必须等于事务内读回的主设置 updated_at")
}

// TestMigrateOpenAIAPIKeyHealthBreakerProbeAtomicFailureRollsBackBoth injects a
// failure while writing the bookkeeping record and asserts the whole
// transaction rolls back: the setting must NOT be left flipped without its
// rollback credential.
func TestMigrateOpenAIAPIKeyHealthBreakerProbeAtomicFailureRollsBackBoth(t *testing.T) {
	ctx := context.Background()
	raw := `{"enabled":false,"probe":{"enabled":false}}`
	repo := newProbeMigrationRepoStub(map[string]string{
		SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
	})
	repo.failRecordWrite = true
	svc := NewSettingService(repo, &config.Config{})

	err := svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx)
	require.Error(t, err, "记账写入失败必须让整个迁移失败")

	// Main setting must be rolled back to its pre-migration state.
	cur, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
	require.NoError(t, err)
	require.Equal(t, raw, cur, "失败回滚后主设置保持迁移前状态（不允许启用却无凭据）")

	// No dangling bookkeeping record.
	_, err = repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
	require.ErrorIs(t, err, ErrSettingNotFound, "失败回滚后不得留下迁移记录")
}

// TestMigrateOpenAIAPIKeyHealthBreakerProbeCASRefusesConcurrentModify proves the
// forward migration's compare-and-set refuses when the persisted setting is
// modified between the service's read and the repository's conditional write: the
// human change is preserved, no bookkeeping record appears, and the error is the
// distinct ErrSettingMigrationConcurrentModified (not a plain storage error).
func TestMigrateOpenAIAPIKeyHealthBreakerProbeCASRefusesConcurrentModify(t *testing.T) {
	ctx := context.Background()
	raw := `{"enabled":false,"probe":{"enabled":false}}`
	repo := newProbeMigrationRepoStub(map[string]string{
		SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
	})
	svc := NewSettingService(repo, &config.Config{})

	// A concurrent admin edit lands between the service's Get and the repository's
	// conditional write, moving updated_at away from the captured pre-image.
	humanRaw := `{"enabled":false,"probe":{"enabled":false},"window_minutes":45}`
	repo.concurrentEditBeforeCAS = func() {
		repo.concurrentEditBeforeCAS = nil // fire once
		require.NoError(t, repo.Set(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings, humanRaw))
	}

	err := svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx)
	require.ErrorIs(t, err, ErrSettingMigrationConcurrentModified,
		"读-写间并发修改 → 迁移必须拒绝返回明确错误")

	// Current value untouched, no bookkeeping row: the migration wrote nothing.
	cur, gerr := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
	require.NoError(t, gerr)
	require.Equal(t, humanRaw, cur, "并发人工修改必须保留，迁移不得覆盖")
	_, berr := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
	require.ErrorIs(t, berr, ErrSettingNotFound, "CAS 拒绝不得写记账记录")
}

// TestMigrateOpenAIAPIKeyHealthBreakerProbeCASRefusesConcurrentCreate proves the
// forward migration's compare-and-set also refuses when the pre-image was absent
// but a concurrent writer created the setting first: the concurrent value is
// preserved, no bookkeeping record appears, and the error is
// ErrSettingMigrationConcurrentModified.
func TestMigrateOpenAIAPIKeyHealthBreakerProbeCASRefusesConcurrentCreate(t *testing.T) {
	ctx := context.Background()
	repo := newProbeMigrationRepoStub(map[string]string{})
	svc := NewSettingService(repo, &config.Config{})

	// A concurrent writer creates the setting before our transaction's write.
	concurrentRaw := `{"enabled":true,"probe":{"enabled":true}}`
	repo.concurrentEditBeforeCAS = func() {
		repo.concurrentEditBeforeCAS = nil // fire once
		require.NoError(t, repo.Set(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings, concurrentRaw))
	}

	err := svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx)
	require.ErrorIs(t, err, ErrSettingMigrationConcurrentModified,
		"前置不存在但事务内已出现行 → 迁移必须拒绝")

	cur, gerr := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
	require.NoError(t, gerr)
	require.Equal(t, concurrentRaw, cur, "并发创建不得被迁移覆盖")
	_, berr := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
	require.ErrorIs(t, berr, ErrSettingNotFound, "CAS 拒绝不得写记账记录")
}

// TestMigrateOpenAIAPIKeyHealthBreakerProbeCASRefusesConcurrentCreateSameValue is
// the E31 regression case: the pre-image was absent and a concurrent writer created
// the row with the EXACT same value the migration would have written. The old code
// judged ownership by row.Value == settingsValue, so it mis-credited that concurrent
// row as its own write, bookkept it, and then could clobber the concurrent config on
// rollback. Ownership must instead come from the INSERT's affected-row count (0 here
// because ON CONFLICT DO NOTHING left the insert to the concurrent writer), so the
// migration refuses, leaves the concurrent value untouched, and writes no bookkeeping.
func TestMigrateOpenAIAPIKeyHealthBreakerProbeCASRefusesConcurrentCreateSameValue(t *testing.T) {
	ctx := context.Background()
	repo := newProbeMigrationRepoStub(map[string]string{})
	svc := NewSettingService(repo, &config.Config{})

	// The migration, for an absent pre-image, writes DefaultOpenAIAPIKeyHealthBreakerSettings
	// with Probe.Enabled flipped true (the default already has it true), so this is
	// precisely the value the INSERT would have produced.
	sameValue, err := json.Marshal(DefaultOpenAIAPIKeyHealthBreakerSettings())
	require.NoError(t, err)
	concurrentRaw := string(sameValue)

	repo.concurrentEditBeforeCAS = func() {
		repo.concurrentEditBeforeCAS = nil // fire once
		require.NoError(t, repo.Set(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings, concurrentRaw))
	}

	err = svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx)
	require.ErrorIs(t, err, ErrSettingMigrationConcurrentModified,
		"前置不存在但并发创建相同值行 → 受影响行数为 0 → 迁移必须拒绝（不可用值相等误认归属）")

	// The concurrent writer's value is preserved untouched.
	cur, gerr := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
	require.NoError(t, gerr)
	require.Equal(t, concurrentRaw, cur, "相同值并发创建不得被迁移覆盖")
	_, berr := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
	require.ErrorIs(t, berr, ErrSettingNotFound, "受影响行数 0 → 不得写记账记录")
}

// TestRollbackOpenAIAPIKeyHealthBreakerProbeMigrationAfterAtomicMigrate verifies
// the CAS rollback still restores the pre-image precisely when the migration ran
// through the atomic path.
func TestRollbackOpenAIAPIKeyHealthBreakerProbeMigrationAfterAtomicMigrate(t *testing.T) {
	ctx := context.Background()
	raw := `{"enabled":false,"probe":{"enabled":false}}`
	repo := newProbeMigrationRepoStub(map[string]string{
		SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
	})
	svc := NewSettingService(repo, &config.Config{})

	require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
	require.Equal(t, 1, repo.atomicCalls)

	require.NoError(t, svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx),
		"原子迁移后未修改 → CAS 精确回滚")

	got, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
	require.NoError(t, err)
	require.False(t, got.Probe.Enabled, "回滚到迁移前 false")

	_, err = repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
	require.ErrorIs(t, err, ErrSettingNotFound, "记账 key 被删")
}

// TestRollbackOpenAIAPIKeyHealthBreakerProbeAtomicCommit proves the rollback's two
// writes (the restored setting and the bookkeeping-record deletion) go through the
// single-transaction capability, so they cannot diverge.
func TestRollbackOpenAIAPIKeyHealthBreakerProbeAtomicCommit(t *testing.T) {
	ctx := context.Background()
	raw := `{"enabled":false,"probe":{"enabled":false}}`
	repo := newProbeMigrationRepoStub(map[string]string{
		SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
	})
	svc := NewSettingService(repo, &config.Config{})

	require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
	require.NoError(t, svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx))

	require.Equal(t, 1, repo.rollbackAtomicCalls, "回滚必须走单事务原子能力")

	cur, err := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
	require.NoError(t, err)
	require.Equal(t, raw, cur, "同一事务内恢复 pre-image")

	_, err = repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
	require.ErrorIs(t, err, ErrSettingNotFound, "同一事务内删除记账记录")
}

// TestRollbackOpenAIAPIKeyHealthBreakerProbeAtomicFailureRollsBackBoth injects a
// failure while deleting the bookkeeping record and asserts the whole transaction
// rolls back: the setting must NOT be left restored while the bookkeeping record
// dangles (the unrecoverable state the dispatch card targets).
func TestRollbackOpenAIAPIKeyHealthBreakerProbeAtomicFailureRollsBackBoth(t *testing.T) {
	ctx := context.Background()
	raw := `{"enabled":false,"probe":{"enabled":false}}`
	repo := newProbeMigrationRepoStub(map[string]string{
		SettingKeyOpenAIAPIKeyHealthBreakerSettings: raw,
	})
	svc := NewSettingService(repo, &config.Config{})

	require.NoError(t, svc.MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx))
	repo.failRecordDelete = true

	err := svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx)
	require.Error(t, err, "记账删除失败必须让整个回滚失败")

	// The setting must remain in its post-migration (flipped) state: the restore
	// is not allowed to survive without the bookkeeping record being removed.
	got, gerr := svc.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
	require.NoError(t, gerr)
	require.True(t, got.Probe.Enabled, "失败回滚后主设置保持迁移后状态（不允许恢复却残留记账）")

	// The bookkeeping record must still be present.
	_, rerr := repo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
	require.NoError(t, rerr, "失败回滚后记账记录仍在（两写同生共死）")
}

// TestRollbackOpenAIAPIKeyHealthBreakerProbeNoAtomicCapabilityFailsClosed proves a
// repository without the atomic rollback capability fails the rollback closed with
// an explicit error instead of falling back to a non-atomic two-write sequence.
func TestRollbackOpenAIAPIKeyHealthBreakerProbeNoAtomicCapabilityFailsClosed(t *testing.T) {
	ctx := context.Background()
	repo := newPlainSettingRepoStub(map[string]string{})
	svc := NewSettingService(repo, &config.Config{})

	err := svc.RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx)
	require.Error(t, err)
	require.ErrorContains(t, err, "setting repository does not support atomic rollback")
}
