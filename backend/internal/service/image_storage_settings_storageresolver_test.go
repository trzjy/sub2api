package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ─── local fakes (namespaced with `ar` so they never collide with the
// //go:build unit fixtures in image_storage_settings_test.go) ───

type arStubRepo struct {
	mu     sync.Mutex
	values map[string]string
}

func newArStubRepo() *arStubRepo { return &arStubRepo{values: map[string]string{}} }

func (r *arStubRepo) Get(context.Context, string) (*Setting, error) { return nil, nil }
func (r *arStubRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.values[key], nil
}
func (r *arStubRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	return nil
}
func (r *arStubRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *arStubRepo) SetMultiple(context.Context, map[string]string) error { return nil }
func (r *arStubRepo) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *arStubRepo) Delete(context.Context, string) error { return nil }

type arEncryptor struct{}

func (arEncryptor) Encrypt(plaintext string) (string, error) { return "enc:" + plaintext, nil }
func (arEncryptor) Decrypt(ciphertext string) (string, error) {
	rest, ok := strings.CutPrefix(ciphertext, "enc:")
	if !ok {
		return "", errors.New("not encrypted")
	}
	return rest, nil
}

// arStorage is an ImageStorage fake that records probes/deletes and can block
// inside Save for the mutual-exclusion test.
type arStorage struct {
	mu            sync.Mutex
	hasObjects    bool
	probeErr      error
	probePrefixes []string
	deletedPrefix []string
	savedKeys     []string
	saveBlock     chan struct{} // when non-nil, Save blocks until the channel is closed
	saveStarted   chan struct{} // when non-nil, closed when Save first blocks
}

func (s *arStorage) Save(_ context.Context, key, _ string, _ []byte) (string, error) {
	s.mu.Lock()
	s.savedKeys = append(s.savedKeys, key)
	block := s.saveBlock
	started := s.saveStarted
	s.mu.Unlock()
	if started != nil {
		close(started)
	}
	if block != nil {
		<-block
	}
	return "https://cdn.example.com/" + key, nil
}

func (s *arStorage) DeleteByPrefix(_ context.Context, prefix string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedPrefix = append(s.deletedPrefix, prefix)
	return 0, nil
}

func (s *arStorage) HasObjectsByPrefix(_ context.Context, prefix string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probePrefixes = append(s.probePrefixes, prefix)
	if s.probeErr != nil {
		return false, s.probeErr
	}
	return s.hasObjects, nil
}

// arFixture wires an ImageStorageSettingService with controllable fake storage
// and records every config the factory built.
type arFixture struct {
	svc          *ImageStorageSettingService
	repo         *arStubRepo
	backup       *BackupService
	mu           sync.Mutex
	hasObjects   bool
	probeErr     error
	factoryErr   error
	builtConfigs []config.ImageStorageConfig
	storages     []*arStorage
}

func newArFixture(fallback config.ImageStorageConfig) *arFixture {
	fx := &arFixture{repo: newArStubRepo()}
	fx.backup = NewBackupService(fx.repo, &config.Config{
		Totp: config.TotpConfig{EncryptionKeyConfigured: true},
	}, arEncryptor{}, nil, nil)
	fx.svc = NewImageStorageSettingService(fx.repo, arEncryptor{}, fx.backup, fx.factory, fallback)
	return fx
}

func (fx *arFixture) factory(_ context.Context, cfg *config.ImageStorageConfig) (ImageStorage, error) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.factoryErr != nil {
		return nil, fx.factoryErr
	}
	fx.builtConfigs = append(fx.builtConfigs, *cfg)
	st := &arStorage{hasObjects: fx.hasObjects, probeErr: fx.probeErr}
	fx.storages = append(fx.storages, st)
	return st, nil
}

func (fx *arFixture) builtCount() int {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return len(fx.builtConfigs)
}

// totalProbes counts HasObjectsByPrefix calls across every built storage.
func (fx *arFixture) totalProbes() int {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	total := 0
	for _, st := range fx.storages {
		st.mu.Lock()
		total += len(st.probePrefixes)
		st.mu.Unlock()
	}
	return total
}

// lastBuiltBucket returns the bucket of the most recent factory-built config.
func (fx *arFixture) lastBuiltBucket() string {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.builtConfigs) == 0 {
		return ""
	}
	return fx.builtConfigs[len(fx.builtConfigs)-1].Bucket
}

// lastBuiltAccessKey returns the access key of the most recent factory-built config.
func (fx *arFixture) lastBuiltAccessKey() string {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.builtConfigs) == 0 {
		return ""
	}
	return fx.builtConfigs[len(fx.builtConfigs)-1].AccessKeyID
}

func (fx *arFixture) probes() []string {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.storages) == 0 {
		return nil
	}
	last := fx.storages[len(fx.storages)-1]
	last.mu.Lock()
	defer last.mu.Unlock()
	return last.probePrefixes
}

func arSeedBackupS3(t *testing.T, repo *arStubRepo, cfg BackupS3Config) {
	t.Helper()
	cfg.SecretAccessKey = "enc:" + cfg.SecretAccessKey
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, repo.Set(context.Background(), settingKeyBackupS3Config, string(data)))
}

var arOwnCreds = ImageStorageSettings{
	Enabled: true, Bucket: "image-bucket", Prefix: "images/",
	PublicBaseURL: "https://cdn.example.com",
	Endpoint:      "https://acct.r2.cloudflarestorage.com", Region: "auto",
	AccessKeyID: "ak-1", SecretAccessKey: "sk-1",
}

// ─── WithAnnouncementStorage 三态 ───

func TestImageStorageWithAnnouncementStorageEnabledWithDirectLink(t *testing.T) {
	fx := newArFixture(config.ImageStorageConfig{})
	ctx := context.Background()
	_, err := fx.svc.Update(ctx, arOwnCreds)
	require.NoError(t, err)

	var got *ResolvedImageStorage
	ok, err := fx.svc.WithAnnouncementStorage(ctx, func(r *ResolvedImageStorage) error {
		got = r
		return nil
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, got)
	require.NotNil(t, got.Storage)
	require.True(t, got.DirectLink, "public_base_url is set, so Save returns long-lived direct links")
}

func TestImageStorageWithAnnouncementStorageDisabledDoesNotRunFn(t *testing.T) {
	fx := newArFixture(config.ImageStorageConfig{})

	ran := false
	ok, err := fx.svc.WithAnnouncementStorage(context.Background(), func(*ResolvedImageStorage) error {
		ran = true
		return nil
	})
	require.NoError(t, err)
	require.False(t, ok, "storage not enabled")
	require.False(t, ran, "fn must not run when storage is disabled")
}

func TestImageStorageWithAnnouncementStorageEnabledWithoutPublicBaseURL(t *testing.T) {
	fx := newArFixture(config.ImageStorageConfig{})
	ctx := context.Background()
	own := arOwnCreds
	own.PublicBaseURL = ""
	_, err := fx.svc.Update(ctx, own)
	require.NoError(t, err)

	var got *ResolvedImageStorage
	ok, err := fx.svc.WithAnnouncementStorage(ctx, func(r *ResolvedImageStorage) error {
		got = r
		return nil
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, got.DirectLink, "presigned-only: the caller must reject announcement uploads")
}

func TestImageStorageWithAnnouncementStoragePropagatesFnError(t *testing.T) {
	fx := newArFixture(config.ImageStorageConfig{})
	ctx := context.Background()
	_, err := fx.svc.Update(ctx, arOwnCreds)
	require.NoError(t, err)

	fnErr := errors.New("save failed")
	ok, err := fx.svc.WithAnnouncementStorage(ctx, func(*ResolvedImageStorage) error { return fnErr })
	require.True(t, ok, "storage was enabled; the failure belongs to fn")
	require.ErrorIs(t, err, fnErr)
}

// ─── 并发（R3-3）：配置变更期间上传不得向旧绑定写入 ───

func TestImageStorageAnnouncementCriticalSectionExcludesConfigChange(t *testing.T) {
	fx := newArFixture(config.ImageStorageConfig{})
	ctx := context.Background()
	_, err := fx.svc.Update(ctx, arOwnCreds)
	require.NoError(t, err)

	var eventsMu sync.Mutex
	var events []string
	record := func(e string) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, e)
	}

	// Upload that blocks inside Save while holding the announcement critical section.
	release := make(chan struct{})
	saveStarted := make(chan struct{})
	fx.mu.Lock()
	fx.storages = nil // only look at storages built from here on
	fx.mu.Unlock()

	updateDone := make(chan error, 1)
	go func() {
		_, err := fx.svc.Update(ctx, ImageStorageSettings{
			Enabled: true, Bucket: "image-bucket-2", Prefix: "images/",
			PublicBaseURL: "https://cdn.example.com",
			Endpoint:      "https://acct.r2.cloudflarestorage.com", Region: "auto",
			AccessKeyID: "ak-1", SecretAccessKey: "sk-1",
		})
		updateDone <- err
	}()

	// First upload: blocks inside Save under the lock (built from the old binding).
	uploadErr := make(chan error, 1)
	go func() {
		_, err := fx.svc.WithAnnouncementStorage(ctx, func(r *ResolvedImageStorage) error {
			if st, ok := r.Storage.(*arStorage); ok {
				st.mu.Lock()
				st.saveBlock = release
				st.saveStarted = saveStarted
				st.mu.Unlock()
			}
			_, err := r.Storage.Save(ctx, AnnouncementImagesPrefix+"1/a.png", "image/png", []byte("x"))
			return err
		})
		record("upload-done")
		uploadErr <- err
	}()

	<-saveStarted
	record("save-blocked")

	// While the upload holds the lock, the config change cannot complete — this
	// select is sound because the lock is genuinely held until release closes.
	select {
	case err := <-updateDone:
		t.Fatalf("Update must not complete while an upload holds the critical section: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	// The config change has not persisted: the stored binding is still the old one.
	raw, _ := fx.repo.GetValue(ctx, settingKeyImageStorageConfig)
	require.Contains(t, raw, `"image-bucket"`, "old binding still persisted while upload runs")

	close(release)
	require.NoError(t, <-uploadErr)
	require.NoError(t, <-updateDone)
	record("update-done")

	// An upload started after the change resolves the NEW binding — nothing
	// writes to the old one across a config change.
	secondRan := make(chan string, 1)
	go func() {
		_, err := fx.svc.WithAnnouncementStorage(ctx, func(*ResolvedImageStorage) error {
			record("second-upload-ran")
			secondRan <- fx.lastBuiltBucket()
			return nil
		})
		if err != nil {
			close(secondRan)
		}
	}()

	select {
	case bucket := <-secondRan:
		require.Equal(t, "image-bucket-2", bucket, "the next upload must resolve the NEW binding")
	case <-time.After(150 * time.Millisecond):
		t.Fatal("second upload never ran after the config change")
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	require.Equal(t, []string{"save-blocked", "upload-done", "update-done", "second-upload-ran"}, events,
		"uploads and config changes are serialized: no upload writes across a binding change")
}

// ─── 配置守卫：Update 的新旧有效绑定对比（3.2(f)） ───

func TestImageStorageSettingUpdateGuardRejectsBindingChangesWhenAnnouncementObjectsExist(t *testing.T) {
	cases := map[string]func(*ImageStorageSettings){
		"disable":          func(s *ImageStorageSettings) { s.Enabled = false },
		"endpoint":         func(s *ImageStorageSettings) { s.Endpoint = "https://other.example.com" },
		"bucket":           func(s *ImageStorageSettings) { s.Bucket = "other-bucket" },
		"region":           func(s *ImageStorageSettings) { s.Region = "wnam" },
		"access_key":       func(s *ImageStorageSettings) { s.AccessKeyID = "ak-2" },
		"secret_key":       func(s *ImageStorageSettings) { s.SecretAccessKey = "sk-2" },
		"force_path_style": func(s *ImageStorageSettings) { s.ForcePathStyle = true },
		"public_base_url":  func(s *ImageStorageSettings) { s.PublicBaseURL = "https://cdn2.example.com" },
		"reuse_mode_on":    func(s *ImageStorageSettings) { s.ReuseBackupS3 = true; s.Bucket = "backup-bucket" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newArFixture(config.ImageStorageConfig{})
			ctx := context.Background()
			arSeedBackupS3(t, fx.repo, BackupS3Config{
				Endpoint: "https://backup.example.com", Region: "auto", Bucket: "backup-bucket",
				AccessKeyID: "bak-ak", SecretAccessKey: "bak-sk", Prefix: "backups/",
			})
			_, err := fx.svc.Update(ctx, arOwnCreds)
			require.NoError(t, err)

			fx.mu.Lock()
			fx.hasObjects = true // old location still holds announcement images
			fx.mu.Unlock()

			in := arOwnCreds
			mutate(&in)
			_, err = fx.svc.Update(ctx, in)
			require.ErrorIs(t, err, ErrAnnouncementImageObjectsExist)

			raw, _ := fx.repo.GetValue(ctx, settingKeyImageStorageConfig)
			require.Contains(t, raw, `"image-bucket"`, "the change must not be persisted")
		})
	}
}

func TestImageStorageSettingUpdateGuardRejectsReuseModeSwitchOff(t *testing.T) {
	fx := newArFixture(config.ImageStorageConfig{})
	ctx := context.Background()
	arSeedBackupS3(t, fx.repo, BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "backup-bucket",
		AccessKeyID: "bak-ak", SecretAccessKey: "bak-sk", Prefix: "backups/",
	})
	reuse := ImageStorageSettings{Enabled: true, ReuseBackupS3: true, Prefix: "images/", PublicBaseURL: "https://cdn.example.com"}
	_, err := fx.svc.Update(ctx, reuse)
	require.NoError(t, err)

	fx.mu.Lock()
	fx.hasObjects = true
	fx.mu.Unlock()

	// Switching from the reused backup bucket to own credentials changes the
	// effective binding even though endpoint/bucket stay "similar" in form fields.
	own := arOwnCreds
	own.Bucket = "backup-bucket"
	_, err = fx.svc.Update(ctx, own)
	require.ErrorIs(t, err, ErrAnnouncementImageObjectsExist)
}

func TestImageStorageSettingUpdateGuardAllowsPrefixChangeAndNoObjects(t *testing.T) {
	fx := newArFixture(config.ImageStorageConfig{})
	ctx := context.Background()
	_, err := fx.svc.Update(ctx, arOwnCreds)
	require.NoError(t, err)

	// Prefix changes never touch the announcement namespace (fixed constant), so
	// they pass even while announcement objects exist.
	fx.mu.Lock()
	fx.hasObjects = true
	fx.mu.Unlock()

	in := arOwnCreds
	in.Prefix = "renamed/"
	saved, err := fx.svc.Update(ctx, in)
	require.NoError(t, err)
	require.Equal(t, "renamed/", saved.Prefix)
	require.Equal(t, 0, fx.totalProbes(), "no probe runs when the binding did not change")

	// With no announcement objects, every binding change goes through.
	fx.mu.Lock()
	fx.hasObjects = false
	fx.mu.Unlock()
	in = arOwnCreds
	in.Bucket = "image-bucket-3"
	_, err = fx.svc.Update(ctx, in)
	require.NoError(t, err)
	require.Equal(t, 1, fx.totalProbes(), "the old location was probed before allowing the change")
	require.Equal(t, []string{AnnouncementImagesPrefix}, fx.probes())

	// The resolver cache was invalidated: the next resolution rebuilds with the new bucket.
	_, enabled := fx.svc.resolve()
	require.True(t, enabled)
	require.Equal(t, "image-bucket-3", fx.lastBuiltBucket())
}

func TestImageStorageSettingUpdateGuardFailsClosedOnProbeError(t *testing.T) {
	for name, inject := range map[string]func(*arFixture){
		"probe_error":   func(fx *arFixture) { fx.probeErr = errors.New("probe down") },
		"factory_error": func(fx *arFixture) { fx.factoryErr = errors.New("client build down") },
	} {
		t.Run(name, func(t *testing.T) {
			fx := newArFixture(config.ImageStorageConfig{})
			ctx := context.Background()
			_, err := fx.svc.Update(ctx, arOwnCreds)
			require.NoError(t, err)

			inject(fx)
			in := arOwnCreds
			in.Bucket = "image-bucket-2"
			_, err = fx.svc.Update(ctx, in)
			require.Error(t, err, "probe failure must fail the change (fail-closed)")
			require.NotErrorIs(t, err, ErrAnnouncementImageObjectsExist)

			raw, _ := fx.repo.GetValue(ctx, settingKeyImageStorageConfig)
			require.Contains(t, raw, `"image-bucket"`, "nothing is persisted on a failed probe")
		})
	}
}

// ─── 配置守卫：GuardBackupS3Change（备份 S3 写入口） ───

func TestImageStorageSettingGuardBackupS3ChangeRejectsWhenReusingAndObjectsExist(t *testing.T) {
	fx := newArFixture(config.ImageStorageConfig{})
	ctx := context.Background()
	arSeedBackupS3(t, fx.repo, BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "backup-bucket",
		AccessKeyID: "bak-ak", SecretAccessKey: "bak-sk", Prefix: "backups/",
	})
	_, err := fx.svc.Update(ctx, ImageStorageSettings{
		Enabled: true, ReuseBackupS3: true, Prefix: "images/", PublicBaseURL: "https://cdn.example.com",
	})
	require.NoError(t, err)

	fx.mu.Lock()
	fx.hasObjects = true
	fx.mu.Unlock()

	candidate := BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "other-backups",
		AccessKeyID: "bak-ak", SecretAccessKey: "bak-sk", Prefix: "backups/",
	}
	err = fx.svc.GuardBackupS3Change(ctx, candidate)
	require.ErrorIs(t, err, ErrAnnouncementImageObjectsExist)

	// Credential-only change on the reused bucket points at a different object
	// space and must be rejected the same way.
	candidate = BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "backup-bucket",
		AccessKeyID: "bak-ak2", SecretAccessKey: "bak-sk2", Prefix: "backups/",
	}
	err = fx.svc.GuardBackupS3Change(ctx, candidate)
	require.ErrorIs(t, err, ErrAnnouncementImageObjectsExist)

	// Unchanged candidate passes without probing.
	candidate = BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "backup-bucket",
		AccessKeyID: "bak-ak", SecretAccessKey: "bak-sk", Prefix: "backups/",
	}
	require.NoError(t, fx.svc.GuardBackupS3Change(ctx, candidate))
	require.Equal(t, 2, fx.totalProbes(), "exactly one probe per rejected candidate")
}

func TestImageStorageSettingGuardBackupS3ChangeAllowsWhenNoObjectsOrNotReusing(t *testing.T) {
	ctx := context.Background()

	// Reusing, but the old location holds no announcement objects.
	fx := newArFixture(config.ImageStorageConfig{})
	arSeedBackupS3(t, fx.repo, BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "backup-bucket",
		AccessKeyID: "bak-ak", SecretAccessKey: "bak-sk", Prefix: "backups/",
	})
	_, err := fx.svc.Update(ctx, ImageStorageSettings{
		Enabled: true, ReuseBackupS3: true, Prefix: "images/", PublicBaseURL: "https://cdn.example.com",
	})
	require.NoError(t, err)
	err = fx.svc.GuardBackupS3Change(ctx, BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "other-backups",
		AccessKeyID: "bak-ak", SecretAccessKey: "bak-sk", Prefix: "backups/",
	})
	require.NoError(t, err)

	// Own credentials: backup changes are irrelevant — zero overhead, no probe.
	own := newArFixture(config.ImageStorageConfig{})
	_, err = own.svc.Update(ctx, arOwnCreds)
	require.NoError(t, err)
	own.mu.Lock()
	own.hasObjects = true
	own.mu.Unlock()
	before := own.builtCount()
	err = own.svc.GuardBackupS3Change(ctx, BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "other-backups",
		AccessKeyID: "bak-ak", SecretAccessKey: "bak-sk", Prefix: "backups/",
	})
	require.NoError(t, err)
	require.Equal(t, before, own.builtCount(), "not reusing the backup bucket passes without building a client")

	// Image storage disabled: same zero-overhead pass.
	disabled := newArFixture(config.ImageStorageConfig{})
	before = disabled.builtCount()
	require.NoError(t, disabled.svc.GuardBackupS3Change(ctx, BackupS3Config{Bucket: "x"}))
	require.Equal(t, before, disabled.builtCount())

	// Probe failure fails closed.
	fx.mu.Lock()
	fx.probeErr = errors.New("probe down")
	fx.mu.Unlock()
	err = fx.svc.GuardBackupS3Change(ctx, BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "other-backups",
		AccessKeyID: "bak-ak", SecretAccessKey: "bak-sk", Prefix: "backups/",
	})
	require.Error(t, err, "probe failure must fail the change (fail-closed)")
}

// ─── UpdateS3Config 接线：守卫 + afterCommit（guard nil 时行为与现状一致） ───

func TestBackupUpdateS3ConfigWithoutGuardBehavesAsBefore(t *testing.T) {
	repo := newArStubRepo()
	backup := NewBackupService(repo, &config.Config{
		Totp: config.TotpConfig{EncryptionKeyConfigured: true},
	}, arEncryptor{}, nil, nil)

	saved, err := backup.UpdateS3Config(context.Background(), BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "backup-bucket",
		AccessKeyID: "bak-ak", SecretAccessKey: "bak-sk", Prefix: "backups/",
	})
	require.NoError(t, err)
	require.Empty(t, saved.SecretAccessKey, "the response must mask the secret")

	raw, _ := repo.GetValue(context.Background(), settingKeyBackupS3Config)
	require.Contains(t, raw, `"backup-bucket"`)
	require.Contains(t, raw, "enc:bak-sk", "the secret is encrypted at rest")
}

func TestBackupUpdateS3ConfigCallsGuardAndAfterCommit(t *testing.T) {
	repo := newArStubRepo()
	backup := NewBackupService(repo, &config.Config{
		Totp: config.TotpConfig{EncryptionKeyConfigured: true},
	}, arEncryptor{}, nil, nil)

	var gotCandidate BackupS3Config
	guardErr := errors.New("")
	guardCalled := false
	afterCommitCalled := false
	backup.SetS3ChangeGuard(func(_ context.Context, candidate BackupS3Config) error {
		guardCalled = true
		gotCandidate = candidate
		if guardErr.Error() != "" {
			return guardErr
		}
		return nil
	}, func() { afterCommitCalled = true })

	cfg := BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "backup-bucket",
		AccessKeyID: "bak-ak", SecretAccessKey: "bak-sk", Prefix: "backups/",
	}

	// Guard rejection: nothing is persisted.
	guardErr = errors.New("guarded")
	_, err := backup.UpdateS3Config(context.Background(), cfg)
	require.ErrorIs(t, err, guardErr)
	raw, _ := repo.GetValue(context.Background(), settingKeyBackupS3Config)
	require.Empty(t, raw, "a rejected change is not persisted")
	require.False(t, afterCommitCalled)

	// Guard pass: persisted, and the after-commit hook runs.
	guardErr = errors.New("")
	_, err = backup.UpdateS3Config(context.Background(), cfg)
	require.NoError(t, err)
	require.True(t, guardCalled)
	require.Equal(t, "bak-sk", gotCandidate.SecretAccessKey, "the guard sees the plaintext candidate secret")
	require.True(t, afterCommitCalled, "afterCommit invalidates the resolver cache after a successful persist")
	raw, _ = repo.GetValue(context.Background(), settingKeyBackupS3Config)
	require.Contains(t, raw, `"backup-bucket"`)
}

func TestBackupS3ChangeEndToEndRejectsThenInvalidatesCache(t *testing.T) {
	fx := newArFixture(config.ImageStorageConfig{})
	ctx := context.Background()
	arSeedBackupS3(t, fx.repo, BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "backup-bucket",
		AccessKeyID: "bak-ak", SecretAccessKey: "bak-sk", Prefix: "backups/",
	})
	_, err := fx.svc.Update(ctx, ImageStorageSettings{
		Enabled: true, ReuseBackupS3: true, Prefix: "images/", PublicBaseURL: "https://cdn.example.com",
	})
	require.NoError(t, err)
	_, enabled := fx.svc.resolve()
	require.True(t, enabled)
	require.Equal(t, "bak-ak", fx.builtConfigs[len(fx.builtConfigs)-1].AccessKeyID)

	// Wire the guard exactly like the production provider does.
	fx.backup.SetS3ChangeGuard(fx.svc.GuardBackupS3Change, fx.svc.InvalidateResolverCache)

	fx.mu.Lock()
	fx.hasObjects = true
	fx.mu.Unlock()

	// A reused-bucket credential change is rejected while announcement images exist.
	_, err = fx.backup.UpdateS3Config(ctx, BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "backup-bucket",
		AccessKeyID: "bak-ak2", SecretAccessKey: "bak-sk2", Prefix: "backups/",
	})
	require.ErrorIs(t, err, ErrAnnouncementImageObjectsExist)
	raw, _ := fx.repo.GetValue(ctx, settingKeyBackupS3Config)
	require.Contains(t, raw, "bak-ak", "the rejected change is not persisted")

	// With the objects gone, the change goes through and the resolver cache is
	// invalidated: the next resolution rebuilds with the new credentials.
	fx.mu.Lock()
	fx.hasObjects = false
	fx.mu.Unlock()
	_, err = fx.backup.UpdateS3Config(ctx, BackupS3Config{
		Endpoint: "https://backup.example.com", Region: "auto", Bucket: "backup-bucket",
		AccessKeyID: "bak-ak2", SecretAccessKey: "bak-sk2", Prefix: "backups/",
	})
	require.NoError(t, err)

	_, enabled = fx.svc.resolve()
	require.True(t, enabled)
	require.Equal(t, "bak-ak2", fx.lastBuiltAccessKey(),
		"resolver cache was invalidated after the backup config change")
}

// Bootstrapping order must not be blocked by the guard: switching to reuse mode
// (or configuring anything) while no backup S3 config exists yet means the old
// binding cannot hold announcement objects — the change goes through when the
// old location is empty, and is probed (and rejected with objects) otherwise.
func TestImageStorageSettingUpdateGuardAllowsBootstrapWithoutBackupConfig(t *testing.T) {
	fx := newArFixture(config.ImageStorageConfig{})
	ctx := context.Background()
	_, err := fx.svc.Update(ctx, arOwnCreds)
	require.NoError(t, err)

	fx.mu.Lock()
	fx.hasObjects = false
	fx.mu.Unlock()

	// Switch to reuse mode with no backup S3 config stored: the candidate
	// location cannot exist, the old location has no objects → allowed.
	_, err = fx.svc.Update(ctx, ImageStorageSettings{
		Enabled: true, ReuseBackupS3: true, Prefix: "images/", PublicBaseURL: "https://cdn.example.com",
	})
	require.NoError(t, err)
	require.Equal(t, 1, fx.totalProbes(), "the old own-creds location was probed")

	// With objects in the old location, the same switch is rejected.
	fx2 := newArFixture(config.ImageStorageConfig{})
	_, err = fx2.svc.Update(ctx, arOwnCreds)
	require.NoError(t, err)
	fx2.mu.Lock()
	fx2.hasObjects = true
	fx2.mu.Unlock()
	_, err = fx2.svc.Update(ctx, ImageStorageSettings{
		Enabled: true, ReuseBackupS3: true, Prefix: "images/", PublicBaseURL: "https://cdn.example.com",
	})
	require.ErrorIs(t, err, ErrAnnouncementImageObjectsExist)
}
