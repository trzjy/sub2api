package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrSettingMigrationNotApplied is returned by the CAS rollback when no migration
// bookkeeping record exists, i.e. MigrateOpenAIAPIKeyHealthBreakerProbeEnabled was
// never applied (or was already rolled back).
var ErrSettingMigrationNotApplied = errors.New("openai apikey health breaker probe migration not applied")

// ErrSettingMigrationCannotSafelyRollback is returned by the CAS rollback when the
// health-breaker setting was modified by a human after the migration wrote it.
// Restoring the pre-image would clobber that manual change, so we refuse to do so
// and require an explicit operator decision.
var ErrSettingMigrationCannotSafelyRollback = errors.New("openai apikey health breaker probe migration cannot be safely rolled back: setting modified after migration")

// ErrSettingMigrationConcurrentModified is returned by the forward migration's
// compare-and-set when the persisted setting no longer matches the pre-image the
// service captured before building the new value. A concurrent writer (an admin or
// another instance) changed or created the setting between the service's read and
// the repository's conditional write, so the migration refuses rather than
// clobbering that change. The caller can distinguish it from an ordinary storage
// error and re-read before retrying.
var ErrSettingMigrationConcurrentModified = errors.New("openai apikey health breaker probe migration refused: setting concurrently modified")

// openAIAPIKeyHealthBreakerProbeMigrationTimeout bounds the DB calls inside the
// one-shot migration / rollback. It reuses the existing codex migration timeout.
const openAIAPIKeyHealthBreakerProbeMigrationTimeout = codexRestrictionPolicyDBTimeout

// openAIAPIKeyHealthBreakerProbeMigrationRecord is the JSON shape stored under
// SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration. PreImage is the raw persisted
// JSON of the health-breaker settings before the migration; PreImageExists
// disambiguates "setting absent" from "setting present with a legal empty value"
// (an empty PreImage on its own cannot tell those apart, and treating a present
// empty value as absent would make the CAS rollback delete the original row).
// MigratedAt is the setting row's updated_at at migration time and is the audit
// fact the CAS rollback compares against.
//
// MigratedAt is stored at full (nanosecond) precision via RFC3339Nano so that the
// value round-trips losslessly against the DB timestamptz column. A second-level
// RFC3339 format would silently drop sub-second digits, making the rollback's
// equality check fail even when nothing changed.
type openAIAPIKeyHealthBreakerProbeMigrationRecord struct {
	PreImage       string `json:"pre_image"`
	PreImageExists bool   `json:"pre_image_exists"`
	MigratedAt     string `json:"migrated_at"`
}

// settingMigrationRollbackAtomicRepository is the narrow repository capability the
// migration rollback needs on top of the base SettingRepository: it commits the
// rollback's two writes — the conditional (compare-and-set) restore of the main
// setting and the removal of its bookkeeping record — in one database
// transaction, reporting the conditional action's affected-row count. Keeping it
// separate from SettingRepository means repositories (and test stubs) that do not
// implement it fail the rollback closed instead of falling back to a non-atomic
// two-write sequence.
type settingMigrationRollbackAtomicRepository interface {
	RollbackSettingAtomically(
		ctx context.Context,
		settingsKey string,
		restoreValue *string,
		expectedUpdatedAt time.Time,
		recordKey string,
	) (int, error)
}

// settingMigrationAtomicRepository is the narrow repository capability that
// commits the migration's two writes (the main setting and its bookkeeping
// record) in one database transaction. MigratedAt must be derived from the main
// setting row's updated_at read back inside that same transaction, so the CAS
// rollback's equality check stays exact and the two rows can never diverge
// (setting flipped but un-bookkept).
type settingMigrationAtomicRepository interface {
	MigrateSettingAtomically(
		ctx context.Context,
		settingsKey, settingsValue string,
		recordKey string,
		buildRecord func(updatedAt time.Time) (string, error),
		expectedExists bool,
		expectedUpdatedAt time.Time,
	) (time.Time, error)
}

// rollbackAtomicRepo returns the atomic-rollback capability of the setting
// repository, if available. The rollback fails closed when it is not: a
// repository without the atomic capability cannot commit the restore and the
// bookkeeping-record deletion together, so the rollback refuses to run rather
// than risk a non-crash-atomic two-write sequence.
func (s *SettingService) rollbackAtomicRepo() (settingMigrationRollbackAtomicRepository, bool) {
	if s == nil || s.settingRepo == nil {
		return nil, false
	}
	repo, ok := s.settingRepo.(settingMigrationRollbackAtomicRepository)
	return repo, ok
}

// atomicMigrationRepo returns the atomic-write capability of the setting
// repository, if available. The migration fails closed when it is not:
// a repository without the atomic capability cannot commit the flip and its
// bookkeeping record together, so the migration refuses to run rather than
// risk a non-crash-atomic two-write sequence.
func (s *SettingService) atomicMigrationRepo() (settingMigrationAtomicRepository, bool) {
	if s == nil || s.settingRepo == nil {
		return nil, false
	}
	repo, ok := s.settingRepo.(settingMigrationAtomicRepository)
	return repo, ok
}

// MigrateOpenAIAPIKeyHealthBreakerProbeEnabled performs the one-shot R1 flip of the
// persisted Probe.Enabled from false (or unset) to true, so that the recovery probe
// is enabled on upgrade without forcing the unrelated circuit-breaker master switch
// on. It is idempotent: a second call with the bookkeeping record already present
// is a no-op.
//
// It records the pre-image (the raw persisted JSON before the flip) together with
// whether the setting existed at all (PreImageExists, which is what tells an absent
// pre-image apart from a present one whose value is the empty string), plus the
// setting row's updated_at (MigratedAt), so that
// RollbackOpenAIAPIKeyHealthBreakerProbeMigration can CAS-restore only the automatic
// flip and never clobber a later manual change.
func (s *SettingService) MigrateOpenAIAPIKeyHealthBreakerProbeEnabled(ctx context.Context) error {
	if s == nil || s.settingRepo == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIAPIKeyHealthBreakerProbeMigrationTimeout)
	defer cancel()

	// Idempotency: if the bookkeeping key already exists, the migration has run.
	if v, err := s.settingRepo.GetValue(dbCtx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration); err == nil && strings.TrimSpace(v) != "" {
		return nil
	} else if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return fmt.Errorf("get %s setting: %w", SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration, err)
	}

	// Read the current persisted settings (with UpdatedAt for the CAS audit fact).
	current, err := s.settingRepo.Get(dbCtx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
	var preImage, currentRaw string
	var preImageExists bool
	var preImageUpdatedAt time.Time
	if err != nil {
		if !errors.Is(err, ErrSettingNotFound) {
			return fmt.Errorf("get %s setting: %w", SettingKeyOpenAIAPIKeyHealthBreakerSettings, err)
		}
		// Setting did not exist: pre-image is the empty sentinel with the
		// existence flag off, so the rollback deletes instead of restoring.
		preImage = ""
		preImageExists = false
		currentRaw = ""
		preImageUpdatedAt = time.Time{}
	} else {
		currentRaw = current.Value
		preImage = current.Value
		preImageExists = true
		preImageUpdatedAt = current.UpdatedAt
	}

	// Decide whether a flip is needed: only when Probe is nil or Probe.Enabled is false.
	needMigration := true
	if currentRaw != "" {
		var stored OpenAIAPIKeyHealthBreakerSettings
		if jsonErr := json.Unmarshal([]byte(currentRaw), &stored); jsonErr == nil {
			if stored.Probe != nil && stored.Probe.Enabled {
				needMigration = false
			}
		}
	}
	// Build the value to persist. When a flip is needed we marshal a new document
	// with Probe.Enabled set true. When the probe is already enabled
	// (needMigration == false, which is only possible when currentRaw is non-empty
	// and Probe.Enabled is already true), we must still persist the bookkeeping
	// marker through the same atomic path — but without flipping the setting value
	// itself. We reuse the raw persisted bytes verbatim so the value stays
	// byte-for-byte identical. PreImage / preImageExists / preImageUpdatedAt already
	// describe the (present, already-enabled) row, so the CAS rollback continues to
	// protect any later manual change by an admin.
	var data []byte
	if needMigration {
		var newSettings OpenAIAPIKeyHealthBreakerSettings
		if currentRaw != "" {
			if jsonErr := json.Unmarshal([]byte(currentRaw), &newSettings); jsonErr != nil {
				return fmt.Errorf("parse %s setting: %w", SettingKeyOpenAIAPIKeyHealthBreakerSettings, jsonErr)
			}
		} else {
			newSettings = *DefaultOpenAIAPIKeyHealthBreakerSettings()
		}
		if newSettings.Probe == nil {
			newSettings.Probe = &OpenAIAPIKeyHealthBreakerProbeSettings{}
		}
		newSettings.Probe.Enabled = true
		marshaled, err := json.Marshal(newSettings)
		if err != nil {
			return fmt.Errorf("marshal %s setting: %w", SettingKeyOpenAIAPIKeyHealthBreakerSettings, err)
		}
		data = marshaled
	} else {
		// Already enabled: write the bookkeeping marker via the same atomic path
		// without touching the setting value. expectedExists / expectedUpdatedAt
		// keep their meaning (the settings row is present with its captured
		// updated_at), so a later manual change by an admin is still protected by
		// the CAS rollback.
		data = []byte(currentRaw)
	}
	buildRecord := func(updatedAt time.Time) (string, error) {
		record := openAIAPIKeyHealthBreakerProbeMigrationRecord{
			PreImage:       preImage,
			PreImageExists: preImageExists,
			MigratedAt:     updatedAt.UTC().Format(time.RFC3339Nano),
		}
		recordData, err := json.Marshal(record)
		if err != nil {
			return "", fmt.Errorf("marshal migration record: %w", err)
		}
		return string(recordData), nil
	}

	// Fail closed: without the atomic capability the migration cannot commit
	// the flip and its bookkeeping record in one transaction. Falling back to a
	// sequential two-write path would risk a process/DB failure between the two
	// writes leaving the probe enabled without a rollback credential, so the
	// migration refuses to run instead.
	if atomic, ok := s.atomicMigrationRepo(); ok {
		if _, err := atomic.MigrateSettingAtomically(
			dbCtx,
			SettingKeyOpenAIAPIKeyHealthBreakerSettings, string(data),
			SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration,
			buildRecord,
			preImageExists,
			preImageUpdatedAt,
		); err != nil {
			return fmt.Errorf("persist %s setting with migration record: %w", SettingKeyOpenAIAPIKeyHealthBreakerSettings, err)
		}
		s.openAIAPIKeyHealthBreakerCache.Store((*cachedOpenAIAPIKeyHealthBreakerSettings)(nil))
		return nil
	}
	return fmt.Errorf("migrate %s: setting repository does not support atomic migration", SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
}

// RollbackOpenAIAPIKeyHealthBreakerProbeMigration reverses the one-shot R1 flip
// using a repository-level compare-and-set on the setting row's updated_at,
// committed together with the bookkeeping-record removal in a single database
// transaction. The compare and the write happen in a single conditional
// statement, so a concurrent manual edit between "check" and "write" can no
// longer be clobbered (there is no application-layer read-compare-write TOCTOU);
// the two writes commit or roll back as one, so a crash between them can no
// longer leave the setting restored while the bookkeeping record dangles:
//   - If no bookkeeping record exists → ErrSettingMigrationNotApplied.
//   - If the conditional update/delete affects exactly one row (the row still
//     carries the migration's updated_at) → the pre-image is restored (written
//     back, or the key deleted when PreImageExists is false) and the bookkeeping
//     record is removed, both in the same transaction.
//   - If it affects zero rows (a human edited the setting after the migration) →
//     ErrSettingMigrationCannotSafelyRollback, leaving the manual change and the
//     bookkeeping record untouched.
//
// The repository must expose the atomic rollback capability; if it does not, the
// rollback fails closed rather than falling back to the racy read-compare-write
// path or a non-atomic two-write sequence.
func (s *SettingService) RollbackOpenAIAPIKeyHealthBreakerProbeMigration(ctx context.Context) error {
	if s == nil || s.settingRepo == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIAPIKeyHealthBreakerProbeMigrationTimeout)
	defer cancel()

	atomic, ok := s.rollbackAtomicRepo()
	if !ok {
		return fmt.Errorf("rollback %s: setting repository does not support atomic rollback", SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
	}

	recordRaw, err := s.settingRepo.GetValue(dbCtx, SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return ErrSettingMigrationNotApplied
		}
		return fmt.Errorf("get migration record: %w", err)
	}
	var record openAIAPIKeyHealthBreakerProbeMigrationRecord
	if err := json.Unmarshal([]byte(recordRaw), &record); err != nil {
		return fmt.Errorf("parse migration record: %w", err)
	}
	migratedAt, err := time.Parse(time.RFC3339Nano, record.MigratedAt)
	if err != nil {
		return fmt.Errorf("parse migrated_at: %w", err)
	}

	//	PreImageExists=false (absent before the migration) → the rollback target is
//	deletion (restoreValue nil); otherwise restore the recorded raw value even
//	when it is the empty string (a present empty pre-image must be restored, not
//	deleted). The repository decides "refused" by affected-row count (0),
//	including the absent-pre-image case where an already-absent row satisfies the
//	target state.
	var restoreValue *string
	if record.PreImageExists {
		preImage := record.PreImage
		restoreValue = &preImage
	}

	affected, err := atomic.RollbackSettingAtomically(
		dbCtx,
		SettingKeyOpenAIAPIKeyHealthBreakerSettings,
		restoreValue,
		migratedAt,
		SettingKeyOpenAIAPIKeyHealthBreakerProbeMigration,
	)
	if err != nil {
		return fmt.Errorf("rollback %s setting atomically: %w", SettingKeyOpenAIAPIKeyHealthBreakerSettings, err)
	}
	if affected == 0 {
		return ErrSettingMigrationCannotSafelyRollback
	}

	// Invalidate the in-process read cache.
	s.openAIAPIKeyHealthBreakerCache.Store((*cachedOpenAIAPIKeyHealthBreakerSettings)(nil))
	return nil
}
