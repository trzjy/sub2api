package repository

import (
	"context"
	"errors"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/setting"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type settingRepository struct {
	client *dbent.Client
}

func NewSettingRepository(client *dbent.Client) service.SettingRepository {
	return &settingRepository{client: client}
}

func (r *settingRepository) Get(ctx context.Context, key string) (*service.Setting, error) {
	m, err := r.client.Setting.Query().Where(setting.KeyEQ(key)).Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrSettingNotFound
		}
		return nil, err
	}
	return &service.Setting{
		ID:        m.ID,
		Key:       m.Key,
		Value:     m.Value,
		UpdatedAt: m.UpdatedAt,
	}, nil
}

func (r *settingRepository) GetValue(ctx context.Context, key string) (string, error) {
	setting, err := r.Get(ctx, key)
	if err != nil {
		return "", err
	}
	return setting.Value, nil
}

func (r *settingRepository) Set(ctx context.Context, key, value string) error {
	now := time.Now()
	return r.client.Setting.
		Create().
		SetKey(key).
		SetValue(value).
		SetUpdatedAt(now).
		OnConflictColumns(setting.FieldKey).
		UpdateNewValues().
		Exec(ctx)
}

func (r *settingRepository) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	if len(keys) == 0 {
		return map[string]string{}, nil
	}
	settings, err := r.client.Setting.Query().Where(setting.KeyIn(keys...)).All(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for _, s := range settings {
		result[s.Key] = s.Value
	}
	return result, nil
}

func (r *settingRepository) SetMultiple(ctx context.Context, settings map[string]string) error {
	if len(settings) == 0 {
		return nil
	}

	now := time.Now()
	builders := make([]*dbent.SettingCreate, 0, len(settings))
	for key, value := range settings {
		builders = append(builders, r.client.Setting.Create().SetKey(key).SetValue(value).SetUpdatedAt(now))
	}
	return r.client.Setting.
		CreateBulk(builders...).
		OnConflictColumns(setting.FieldKey).
		UpdateNewValues().
		Exec(ctx)
}

func (r *settingRepository) GetAll(ctx context.Context) (map[string]string, error) {
	settings, err := r.client.Setting.Query().All(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for _, s := range settings {
		result[s.Key] = s.Value
	}
	return result, nil
}

func (r *settingRepository) Delete(ctx context.Context, key string) error {
	_, err := r.client.Setting.Delete().Where(setting.KeyEQ(key)).Exec(ctx)
	return err
}

// RollbackSettingAtomically commits the migration rollback's two writes — the
// conditional (compare-and-set) restore of the main setting and the removal of
// its bookkeeping record — in a single database transaction, so a process/DB
// failure between them can never leave the setting restored but the bookkeeping
// record dangling. That dangling state is unrecoverable: the migration's
// idempotency check would skip the still-present record, and the CAS rollback
// would refuse because the setting's updated_at already moved.
//
// The restore is a true CAS performed as a single conditional statement, so the
// compare and the write cannot be interleaved by a concurrent writer (no
// read-compare-write TOCTOU):
//   - restoreValue != nil: write it back only where updated_at == expectedUpdatedAt.
//   - restoreValue == nil: the pre-image was "absent", so delete only where
//     updated_at == expectedUpdatedAt.
//
// The affected-row count is the safety gate: zero rows means a human changed the
// setting after the migration, so the transaction is rolled back and the
// bookkeeping record is left untouched for the operator to decide. The one
// exception is the absent pre-image: a zero-row conditional delete is
// disambiguated inside the transaction by an existence read — when the row is
// already absent the target state holds and the rollback is treated as applied
// (returns 1) with the bookkeeping record removed; when the row is still present
// it was modified after the migration and the rollback is refused (returns 0).
// That read only disambiguates and gates no write, so it adds no TOCTOU.
//
// When the context already carries a transaction (dbent.TxFromContext) this method
// participates in it instead of opening its own. It returns the affected-row
// count of the conditional setting action (0 = refused).
func (r *settingRepository) RollbackSettingAtomically(
	ctx context.Context,
	settingsKey string,
	restoreValue *string,
	expectedUpdatedAt time.Time,
	recordKey string,
) (int, error) {
	if r == nil || r.client == nil {
		return 0, errors.New("setting repository client is nil")
	}

	contextTx := dbent.TxFromContext(ctx)
	client := r.client
	var tx *dbent.Tx
	if contextTx != nil {
		client = contextTx.Client()
	} else {
		var err error
		tx, err = r.client.Tx(ctx)
		if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
			return 0, err
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			client = tx.Client()
		}
	}

	var (
		affected int
		err      error
	)
	if restoreValue == nil {
		affected, err = client.Setting.
			Delete().
			Where(setting.KeyEQ(settingsKey), setting.UpdatedAtEQ(expectedUpdatedAt)).
			Exec(ctx)
		if err == nil && affected == 0 {
			// Disambiguate inside the transaction: a row that is already absent
			// means the target state holds; a row that is still present means it
			// was modified after the migration.
			exists, xerr := client.Setting.Query().Where(setting.KeyEQ(settingsKey)).Exist(ctx)
			if xerr != nil {
				return 0, xerr
			}
			if !exists {
				affected = 1
			}
		}
	} else {
		affected, err = client.Setting.
			Update().
			Where(setting.KeyEQ(settingsKey), setting.UpdatedAtEQ(expectedUpdatedAt)).
			SetValue(*restoreValue).
			SetUpdatedAt(time.Now()).
			Save(ctx)
	}
	if err != nil {
		return 0, err
	}
	if affected == 0 {
		// CAS refused: a human changed the setting after the migration. Leave the
		// bookkeeping record untouched; the deferred rollback discards the tx.
		return 0, nil
	}

	if _, err := client.Setting.
		Delete().
		Where(setting.KeyEQ(recordKey)).
		Exec(ctx); err != nil {
		return 0, err
	}

	if tx != nil {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
	}
	return affected, nil
}

// MigrateSettingAtomically commits the one-shot migration's two settings writes
// (the main setting and its bookkeeping record) in a single database transaction,
// so a process/DB failure between them can never leave the setting flipped but
// un-bookkept. It performs a compare-and-set on the main setting inside that same
// transaction before writing anything:
//   - expectedExists=true: the setting must still exist with
//     updated_at == expectedUpdatedAt (the pre-image captured by the service when
//     it read the current value). The write is a single conditional UPDATE guarded
//     by UpdatedAtEQ, so no application-layer read-compare-write TOCTOU can clobber
//     a concurrent edit; zero affected rows means a concurrent writer changed or
//     removed the setting and the transaction rolls back with
//     service.ErrSettingMigrationConcurrentModified.
//   - expectedExists=false: the setting must still be absent. If a concurrent
//     writer created it first, the transaction rolls back with
//     service.ErrSettingMigrationConcurrentModified instead of overwriting that new
//     value.
//
// Only after the CAS passes does it:
//   - read the row back inside the transaction to obtain the persisted updated_at,
//   - ask buildRecord for the bookkeeping value derived from that updated_at
//     (keeps the record JSON shape in the service layer), and
//   - write recordKey=recordValue in the same transaction.
//
// Any failure (including a CAS refusal) rolls the whole transaction back. When the
// context already carries a transaction (dbent.TxFromContext) this method
// participates in it instead of opening its own, matching the repository's
// existing TxFromContext convention. It returns the main setting row's post-write
// updated_at.
func (r *settingRepository) MigrateSettingAtomically(
	ctx context.Context,
	settingsKey, settingsValue string,
	recordKey string,
	buildRecord func(updatedAt time.Time) (string, error),
	expectedExists bool,
	expectedUpdatedAt time.Time,
) (time.Time, error) {
	if r == nil || r.client == nil {
		return time.Time{}, errors.New("setting repository client is nil")
	}
	if buildRecord == nil {
		return time.Time{}, errors.New("setting migration record builder is nil")
	}

	contextTx := dbent.TxFromContext(ctx)
	client := r.client
	var tx *dbent.Tx
	if contextTx != nil {
		client = contextTx.Client()
	} else {
		var err error
		tx, err = r.client.Tx(ctx)
		if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
			return time.Time{}, err
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			client = tx.Client()
		}
	}

	var row *dbent.Setting
	if expectedExists {
		// CAS: overwrite the row only where it still carries the pre-image's
		// updated_at. A zero affected-row count means a concurrent writer changed
		// or removed the setting since the service read it, so refuse and let the
		// deferred rollback discard the whole transaction.
		affected, err := client.Setting.
			Update().
			Where(setting.KeyEQ(settingsKey), setting.UpdatedAtEQ(expectedUpdatedAt)).
			SetValue(settingsValue).
			SetUpdatedAt(time.Now()).
			Save(ctx)
		if err != nil {
			return time.Time{}, err
		}
		if affected == 0 {
			return time.Time{}, service.ErrSettingMigrationConcurrentModified
		}
		row, err = client.Setting.Query().Where(setting.KeyEQ(settingsKey)).Only(ctx)
		if err != nil {
			return time.Time{}, err
		}
	} else {
		// Pre-image was absent: the migration may only create the row when it is
		// still absent. If a concurrent writer created it first, refuse so we do
		// not clobber that new value.
		exists, err := client.Setting.Query().Where(setting.KeyEQ(settingsKey)).Exist(ctx)
		if err != nil {
			return time.Time{}, err
		}
		if exists {
			return time.Time{}, service.ErrSettingMigrationConcurrentModified
		}
		// Create the row only when it is still absent. We let the write outcome
		// decide ownership instead of comparing values: a plain CREATE that hits
		// the unique-constraint violation means a concurrent writer already
		// created the row (with the same OR a different value) between our Exist
		// check and this insert, so the row was not ours to bookkeep.
		created, err := client.Setting.
			Create().
			SetKey(settingsKey).
			SetValue(settingsValue).
			SetUpdatedAt(time.Now()).
			Save(ctx)
		if err != nil {
			if isUniqueViolation(err) || dbent.IsConstraintError(err) {
				return time.Time{}, service.ErrSettingMigrationConcurrentModified
			}
			return time.Time{}, err
		}
		// Ownership confirmed by the successful insert (affected rows == 1); no
		// value-equality check, which would mis-credit a concurrent row carrying
		// the same value. Use the returned node directly.
		row = created
	}

	// Re-read inside the transaction so the recorded updated_at is exactly the
	// persisted value; the rollback's CAS equality check then matches without
	// lossy rounding.
	recordValue, err := buildRecord(row.UpdatedAt)
	if err != nil {
		return time.Time{}, err
	}
	if err := client.Setting.
		Create().
		SetKey(recordKey).
		SetValue(recordValue).
		SetUpdatedAt(time.Now()).
		OnConflictColumns(setting.FieldKey).
		UpdateNewValues().
		Exec(ctx); err != nil {
		return time.Time{}, err
	}

	if tx != nil {
		if err := tx.Commit(); err != nil {
			return time.Time{}, err
		}
	}
	return row.UpdatedAt, nil
}
