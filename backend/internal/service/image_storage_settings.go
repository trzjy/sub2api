package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const settingKeyImageStorageConfig = "image_storage_config"

// ErrImageStorageIncomplete 表示开关已打开但凭证不全，无法启用异步生图。
var ErrImageStorageIncomplete = errors.New("image storage is enabled but bucket/access_key_id/secret_access_key are incomplete")

// ErrAnnouncementImageObjectsExist 表示旧存储位置仍存在公告图片对象，本次存储配置变更被守卫拒绝。
// 管理员需先按运维手册孤儿清理步骤清空公告图片对象，再变更配置。
var ErrAnnouncementImageObjectsExist = infraerrors.BadRequest(
	"ANNOUNCEMENT_IMAGE_OBJECTS_EXIST",
	"存在公告图片对象，禁止变更图片存储配置（先清理公告图片，见运维手册孤儿清理步骤）",
)

// ImageStorageFactory 由 repository 层提供，把配置变成一个可用的对象存储实现。
// 与 BackupObjectStoreFactory 同样的注入方式，避免 service 反向依赖 repository。
type ImageStorageFactory func(ctx context.Context, cfg *config.ImageStorageConfig) (ImageStorage, error)

// ImageStorageSettings 是后台可编辑的异步生图对象存储配置。
//
// ReuseBackupS3 为真时不保存自己的凭证，直接借用数据库备份已配置的 S3 端点与密钥，
// 只用自己的 Bucket/Prefix 区分对象；这样"数据走 backups/、图片走 images/"无需重复配置。
type ImageStorageSettings struct {
	Enabled       bool `json:"enabled"`
	ReuseBackupS3 bool `json:"reuse_backup_s3"`

	Bucket           string `json:"bucket"` // 留空且复用备份时，沿用备份桶
	Prefix           string `json:"prefix"`
	PublicBaseURL    string `json:"public_base_url"`
	PresignExpiry    int    `json:"presign_expiry_hours"`
	MaxDownloadBytes int64  `json:"max_download_bytes"`

	// 以下仅在 ReuseBackupS3 为假时使用
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key,omitempty"` //nolint:revive // field name follows AWS convention
	ForcePathStyle  bool   `json:"force_path_style"`
}

// ImageStorageSettingService 读写后台设置，并把结果解析成一个可直接使用的 uploader。
//
// 解析结果带缓存：网关每次请求都要判断功能是否开启，不能每次都查库。保存设置时调用
// Invalidate 清缓存，下一次请求即重建客户端——这是"后台开关立即生效、无需重启"的实现。
type ImageStorageSettingService struct {
	settingRepo SettingRepository
	encryptor   SecretEncryptor
	backup      *BackupService
	factory     ImageStorageFactory

	// fallback 是 config.yaml 里的配置。后台从未保存过设置时沿用它，
	// 保证升级前已用配置文件开启该功能的部署不被打断。
	fallback config.ImageStorageConfig

	mu       sync.Mutex
	resolved bool
	uploader *ImageResultUploader
	enabled  bool

	// announceMu 是公告图片存储互斥锁（方案 3.2(b)/(f) R3-3）：WithAnnouncementStorage
	//（解析 + 执行 fn 全程持锁）、Update 守卫路径（比较 + 探测 + 持久化 + 失效缓存持锁）、
	// GuardBackupS3Change 共用，保证配置变更期间不会有上传继续向旧绑定写入。
	// 作用域为单进程（部署硬约束：后端单副本，见方案 §6）。
	announceMu sync.Mutex
}

func NewImageStorageSettingService(
	settingRepo SettingRepository,
	encryptor SecretEncryptor,
	backup *BackupService,
	factory ImageStorageFactory,
	fallback config.ImageStorageConfig,
) *ImageStorageSettingService {
	return &ImageStorageSettingService{
		settingRepo: settingRepo,
		encryptor:   encryptor,
		backup:      backup,
		factory:     factory,
		fallback:    fallback,
	}
}

// Resolver 返回可注入 ImageTaskService 的解析函数。
func (s *ImageStorageSettingService) Resolver() ImageStorageResolver {
	return func() (*ImageResultUploader, bool) {
		return s.resolve()
	}
}

func (s *ImageStorageSettingService) resolve() (*ImageResultUploader, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolved {
		return s.uploader, s.enabled
	}

	ctx := context.Background()
	s.resolved = true
	s.uploader, s.enabled = nil, false

	cfg, err := s.effectiveConfig(ctx)
	if err != nil {
		logger.L().Warn("image_storage.settings_load_failed; async image tasks stay disabled", zap.Error(err))
		return nil, false
	}
	if !cfg.Enabled {
		return nil, false
	}
	if !cfg.IsConfigured() {
		logger.L().Warn("image_storage is enabled but not fully configured; async image tasks are disabled",
			zap.Strings("missing_keys", cfg.MissingCredentialKeys()))
		return nil, false
	}

	storage, err := s.factory(ctx, cfg)
	if err != nil {
		logger.L().Error("image_storage.client_build_failed; async image tasks stay disabled", zap.Error(err))
		return nil, false
	}
	s.uploader = NewImageResultUploader(storage, cfg.Prefix, cfg.MaxDownloadByte, nil)
	s.enabled = true
	return s.uploader, true
}

// Invalidate 丢弃缓存，使下一次请求按最新设置重新解析。
func (s *ImageStorageSettingService) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.resolved = false
	s.uploader = nil
	s.enabled = false
	s.mu.Unlock()
}

// InvalidateResolverCache 失效公告图片 resolver 缓存（配置变更持久化成功后由调用方触发，
// 例如 BackupService.UpdateS3Config 在守卫通过并持久化成功后调用）。
func (s *ImageStorageSettingService) InvalidateResolverCache() {
	if s == nil {
		return
	}
	s.Invalidate()
}

// WithAnnouncementStorage 在公告图片存储互斥锁内解析当前存储绑定并执行 fn。
// ok=false 表示存储未启用（未启用时不执行 fn）；fn 返回的错误原样透出。
// 3.2(f) 配置守卫的"探测 → 持久化 → 缓存失效"持同一把锁，保证：
//
//	a) 单次上传/删除全程使用同一存储绑定，不被并发配置变更打断（消 R3-3 探测-上传竞态窗口）；
//	b) 配置变更期间不会有上传继续向旧绑定写入。
//
// 不引入独立"租约"抽象——一把 sync.Mutex + 两个入口方法即为全部机制（防过度设计）。
func (s *ImageStorageSettingService) WithAnnouncementStorage(ctx context.Context, fn func(*ResolvedImageStorage) error) (bool, error) {
	if s == nil {
		return false, nil
	}
	s.announceMu.Lock()
	defer s.announceMu.Unlock()

	cfg, err := s.effectiveConfig(ctx)
	if err != nil {
		logger.L().Warn("image_storage.announcement_resolve_failed; announcement image storage stays disabled", zap.Error(err))
		return false, nil
	}
	if !cfg.Active() {
		return false, nil
	}
	storage, err := s.factory(ctx, cfg)
	if err != nil {
		logger.L().Error("image_storage.announcement_client_build_failed; announcement image storage stays disabled", zap.Error(err))
		return false, nil
	}
	resolved := &ResolvedImageStorage{Storage: storage, DirectLink: cfg.PublicBaseURL != ""}
	if err := fn(resolved); err != nil {
		return true, err
	}
	return true, nil
}

// GuardBackupS3Change 以候选备份配置计算图片存储新有效绑定，与旧有效绑定做守卫对比
// （含 HasObjectsByPrefix 探测，fail-closed）。对比在公告图片存储互斥锁内执行。
// 图片存储未启用或未复用备份桶时直接放行（零开销）。
// candidateBackupCfg 为 UpdateS3Config 持久化前的候选配置，SecretAccessKey 须为解密后明文。
func (s *ImageStorageSettingService) GuardBackupS3Change(ctx context.Context, candidateBackupCfg BackupS3Config) error {
	if s == nil {
		return nil
	}
	s.announceMu.Lock()
	defer s.announceMu.Unlock()

	settings, err := s.load(ctx)
	if err != nil {
		return fmt.Errorf("load image storage settings for announcement guard: %w", err)
	}
	if settings == nil {
		settings = settingsFromConfig(s.fallback)
	}
	if !settings.ReuseBackupS3 {
		return nil // 图片存储未复用备份桶，备份配置变更与公告图片无关 → 零开销放行
	}
	oldCfg, err := s.resolveAnnouncementBinding(ctx, settings)
	if err != nil {
		return fmt.Errorf("resolve current image storage config for announcement guard: %w", err)
	}
	if oldCfg == nil {
		return nil // 当前不存在可用绑定，公告图片不可能已按其落盘 → 放行
	}
	// 以候选备份配置替换复用模式下借用的字段，得到新有效绑定
	//（public_base_url 是图片存储自己的配置，不受备份配置变更影响）。
	newCfg := *oldCfg
	newCfg.Endpoint = candidateBackupCfg.Endpoint
	newCfg.Region = candidateBackupCfg.Region
	newCfg.AccessKeyID = candidateBackupCfg.AccessKeyID
	newCfg.SecretAccessKey = candidateBackupCfg.SecretAccessKey
	newCfg.ForcePathStyle = candidateBackupCfg.ForcePathStyle
	if settings.Bucket == "" {
		newCfg.Bucket = candidateBackupCfg.Bucket
	}
	if !announcementBindingFromConfig(oldCfg).differsFrom(announcementBindingFromConfig(&newCfg)) {
		return nil
	}
	return s.rejectIfAnnouncementObjectsExistLocked(ctx, oldCfg)
}

// announcementBinding 是守卫对比所用的有效存储绑定：全部影响 S3 客户端寻址与访问权的
// 字段加启用状态（R4-5）。Prefix 不在其中——公告图片 key 用固定命名空间 announcements/，
// prefix 变更放行。
type announcementBinding struct {
	enabled         bool
	endpoint        string
	bucket          string
	region          string
	accessKeyID     string
	secretAccessKey string
	forcePathStyle  bool
	publicBaseURL   string
}

func announcementBindingFromConfig(cfg *config.ImageStorageConfig) announcementBinding {
	return announcementBinding{
		enabled:         cfg.Enabled,
		endpoint:        cfg.Endpoint,
		bucket:          cfg.Bucket,
		region:          cfg.Region,
		accessKeyID:     cfg.AccessKeyID,
		secretAccessKey: cfg.SecretAccessKey,
		forcePathStyle:  cfg.ForcePathStyle,
		publicBaseURL:   cfg.PublicBaseURL,
	}
}

func (b announcementBinding) differsFrom(other announcementBinding) bool {
	return b.enabled != other.enabled ||
		b.endpoint != other.endpoint ||
		b.bucket != other.bucket ||
		b.region != other.region ||
		b.accessKeyID != other.accessKeyID ||
		b.secretAccessKey != other.secretAccessKey ||
		b.forcePathStyle != other.forcePathStyle ||
		b.publicBaseURL != other.publicBaseURL
}

// resolveAnnouncementBinding 把一份图片存储设置解析成守卫对比所用的有效绑定。
// 返回 (nil, nil) 表示"该绑定位置当前不可能存在"——图片存储未启用、凭证不全，
// 或复用模式下尚无备份 S3 配置：此时公告图片不可能已按该位置落盘，无需守卫
// （这也是"先存图片设置、后配备份桶"引导顺序不被守卫卡死的零回归路径）。
// 返回非空 error 仅用于硬性失败（如存储的设置损坏）——fail-closed。
func (s *ImageStorageSettingService) resolveAnnouncementBinding(ctx context.Context, settings *ImageStorageSettings) (*config.ImageStorageConfig, error) {
	if !settings.Enabled {
		return nil, nil
	}
	if settings.ReuseBackupS3 {
		backupCfg, err := s.backupCredentials(ctx)
		if err != nil || backupCfg == nil {
			return nil, nil // 被借用的备份位置尚不存在
		}
	}
	cfg, err := s.toImageStorageConfig(ctx, settings)
	if err != nil {
		return nil, err
	}
	if !cfg.Active() {
		return nil, nil
	}
	return cfg, nil
}

// guardAnnouncementStorageChangeLocked 以新旧有效绑定对比实现公告图片配置守卫（方案 3.2(f)）：
// 任一影响绑定的字段变化且旧有效存储位置仍存在公告图片对象 → 拒绝；prefix 变更放行。
// 探测经工厂以旧有效配置构造存储实例执行，探测失败使本次变更失败（fail-closed）。
// 调用方必须已持有 announceMu。
func (s *ImageStorageSettingService) guardAnnouncementStorageChangeLocked(ctx context.Context, candidate ImageStorageSettings) error {
	stored, err := s.load(ctx)
	if err != nil {
		return fmt.Errorf("load image storage settings for announcement guard: %w", err)
	}
	if stored == nil {
		stored = settingsFromConfig(s.fallback)
	}
	oldCfg, err := s.resolveAnnouncementBinding(ctx, stored)
	if err != nil {
		return fmt.Errorf("resolve current image storage config for announcement guard: %w", err)
	}
	if oldCfg == nil {
		return nil // 当前不存在可用绑定，公告图片不可能已按其落盘 → 放行
	}
	newCfg, err := s.resolveAnnouncementBinding(ctx, &candidate)
	if err != nil {
		return fmt.Errorf("resolve candidate image storage config for announcement guard: %w", err)
	}
	if newCfg == nil {
		// 候选指向一个不可能存在的位置（禁用/未配置/复用尚无备份桶），
		// 与旧有效绑定必然不同：按绑定变化探测旧位置后裁决。
		return s.rejectIfAnnouncementObjectsExistLocked(ctx, oldCfg)
	}
	if !announcementBindingFromConfig(oldCfg).differsFrom(announcementBindingFromConfig(newCfg)) {
		return nil
	}
	return s.rejectIfAnnouncementObjectsExistLocked(ctx, oldCfg)
}

// rejectIfAnnouncementObjectsExistLocked 用旧有效配置构造存储实例并探测公告图片前缀，
// 存在对象时返回 ErrAnnouncementImageObjectsExist。探测失败 fail-closed。
// 调用方必须已持有 announceMu。
func (s *ImageStorageSettingService) rejectIfAnnouncementObjectsExistLocked(ctx context.Context, oldCfg *config.ImageStorageConfig) error {
	storage, err := s.factory(ctx, oldCfg)
	if err != nil {
		return fmt.Errorf("probe announcement image objects: %w", err)
	}
	has, err := storage.HasObjectsByPrefix(ctx, AnnouncementImagesPrefix)
	if err != nil {
		return fmt.Errorf("probe announcement image objects: %w", err)
	}
	if has {
		return ErrAnnouncementImageObjectsExist
	}
	return nil
}

// Get 返回后台设置（SecretAccessKey 已脱敏）。从未保存过时返回 config.yaml 的等价值。
func (s *ImageStorageSettingService) Get(ctx context.Context) (*ImageStorageSettings, error) {
	settings, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	if settings == nil {
		settings = settingsFromConfig(s.fallback)
	}
	settings.SecretAccessKey = ""
	return settings, nil
}

// SecretConfigured 供前端展示"已配置"占位符。
func (s *ImageStorageSettingService) SecretConfigured(ctx context.Context) bool {
	settings, err := s.load(ctx)
	if err != nil || settings == nil {
		return s.fallback.SecretAccessKey != ""
	}
	if settings.ReuseBackupS3 {
		cfg, err := s.backupCredentials(ctx)
		return err == nil && cfg != nil && cfg.SecretAccessKey != ""
	}
	return settings.SecretAccessKey != ""
}

// Update 保存设置并立即生效。SecretAccessKey 留空表示沿用已保存的值。
// 守卫路径（有效绑定对比 + 探测 + 持久化 + 失效缓存）在同一把公告图片互斥锁内完成，
// 保证配置变更期间不会有上传继续向旧绑定写入（方案 3.2(f)）。
func (s *ImageStorageSettingService) Update(ctx context.Context, in ImageStorageSettings) (*ImageStorageSettings, error) {
	normalizeImageStorageSettings(&in)

	if in.ReuseBackupS3 {
		// 复用备份凭证时不落自己的密钥，避免同一份密钥在库里存两份。
		in.Endpoint, in.Region, in.AccessKeyID, in.SecretAccessKey = "", "", "", ""
		in.ForcePathStyle = false
	} else if in.SecretAccessKey == "" {
		if old, err := s.load(ctx); err == nil && old != nil {
			in.SecretAccessKey = old.SecretAccessKey
		}
	} else {
		// 拒绝用自动生成的临时密钥加密：重启后密文无法解密（#4524）。
		// 与备份 S3 配置共用同一把密钥，故复用其配置状态判断。
		if s.backup == nil || !s.backup.EncryptionKeyConfigured() {
			return nil, ErrSecretEncryptionKeyNotConfigured
		}
		encrypted, err := s.encryptor.Encrypt(in.SecretAccessKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt secret: %w", err)
		}
		in.SecretAccessKey = encrypted
	}

	data, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal image storage settings: %w", err)
	}

	s.announceMu.Lock()
	defer s.announceMu.Unlock()

	if err := s.guardAnnouncementStorageChangeLocked(ctx, in); err != nil {
		return nil, err
	}
	if err := s.settingRepo.Set(ctx, settingKeyImageStorageConfig, string(data)); err != nil {
		return nil, fmt.Errorf("save image storage settings: %w", err)
	}
	s.Invalidate()

	in.SecretAccessKey = ""
	return &in, nil
}

// TestConnection 用给定设置试建一次客户端，用于后台的"测试连接"按钮。
// 与 Update 一样支持留空 SecretAccessKey 表示沿用已保存的值。
func (s *ImageStorageSettingService) TestConnection(ctx context.Context, in ImageStorageSettings) error {
	normalizeImageStorageSettings(&in)
	if !in.ReuseBackupS3 && in.SecretAccessKey == "" {
		old, err := s.load(ctx)
		if err == nil && old != nil {
			in.SecretAccessKey = old.SecretAccessKey
		}
	}
	cfg, err := s.toImageStorageConfig(ctx, &in)
	if err != nil {
		return err
	}
	if !cfg.IsConfigured() {
		return ErrImageStorageIncomplete
	}
	if _, err := s.factory(ctx, cfg); err != nil {
		return err
	}
	return nil
}

// effectiveConfig 把后台设置（或 config.yaml 回落）解析成运行时配置。
func (s *ImageStorageSettingService) effectiveConfig(ctx context.Context) (*config.ImageStorageConfig, error) {
	settings, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	if settings == nil {
		fallback := s.fallback
		return &fallback, nil
	}
	return s.toImageStorageConfig(ctx, settings)
}

func (s *ImageStorageSettingService) toImageStorageConfig(ctx context.Context, in *ImageStorageSettings) (*config.ImageStorageConfig, error) {
	cfg := &config.ImageStorageConfig{
		Enabled:         in.Enabled,
		Bucket:          in.Bucket,
		Prefix:          in.Prefix,
		PublicBaseURL:   in.PublicBaseURL,
		PresignExpiry:   in.PresignExpiry,
		MaxDownloadByte: in.MaxDownloadBytes,
		Endpoint:        in.Endpoint,
		Region:          in.Region,
		AccessKeyID:     in.AccessKeyID,
		SecretAccessKey: in.SecretAccessKey,
		ForcePathStyle:  in.ForcePathStyle,
	}

	if in.ReuseBackupS3 {
		backupCfg, err := s.backupCredentials(ctx)
		if err != nil {
			return nil, err
		}
		if backupCfg == nil {
			return nil, errors.New("image storage is set to reuse the backup S3 configuration, but no backup S3 configuration exists")
		}
		cfg.Endpoint = backupCfg.Endpoint
		cfg.Region = backupCfg.Region
		cfg.AccessKeyID = backupCfg.AccessKeyID
		cfg.SecretAccessKey = backupCfg.SecretAccessKey
		cfg.ForcePathStyle = backupCfg.ForcePathStyle
		if cfg.Bucket == "" {
			cfg.Bucket = backupCfg.Bucket
		}
	} else if cfg.SecretAccessKey != "" {
		decrypted, err := s.encryptor.Decrypt(cfg.SecretAccessKey)
		if err != nil {
			// 兼容未加密的旧数据，与备份配置的处理保持一致。
			logger.L().Warn("image_storage secret decrypt failed; treating the stored value as plaintext", zap.Error(err))
		} else {
			cfg.SecretAccessKey = decrypted
		}
	}
	return cfg, nil
}

// backupCredentials 取备份已配置的 S3 凭证（已解密）。
func (s *ImageStorageSettingService) backupCredentials(ctx context.Context) (*BackupS3Config, error) {
	if s.backup == nil {
		return nil, errors.New("backup service is unavailable")
	}
	return s.backup.loadS3Config(ctx)
}

// load 读出后台设置；从未保存过时返回 nil。
func (s *ImageStorageSettingService) load(ctx context.Context) (*ImageStorageSettings, error) {
	if s.settingRepo == nil {
		return nil, nil //nolint:nilnil // no repository means no stored settings
	}
	raw, err := s.settingRepo.GetValue(ctx, settingKeyImageStorageConfig)
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, nil //nolint:nilnil // never configured is a valid state
	}
	var settings ImageStorageSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return nil, fmt.Errorf("parse image storage settings: %w", err)
	}
	return &settings, nil
}

func settingsFromConfig(cfg config.ImageStorageConfig) *ImageStorageSettings {
	return &ImageStorageSettings{
		Enabled:          cfg.Enabled,
		Bucket:           cfg.Bucket,
		Prefix:           cfg.Prefix,
		PublicBaseURL:    cfg.PublicBaseURL,
		PresignExpiry:    cfg.PresignExpiry,
		MaxDownloadBytes: cfg.MaxDownloadByte,
		Endpoint:         cfg.Endpoint,
		Region:           cfg.Region,
		AccessKeyID:      cfg.AccessKeyID,
		SecretAccessKey:  cfg.SecretAccessKey,
		ForcePathStyle:   cfg.ForcePathStyle,
	}
}

func normalizeImageStorageSettings(in *ImageStorageSettings) {
	in.Bucket = strings.TrimSpace(in.Bucket)
	in.Endpoint = strings.TrimSpace(in.Endpoint)
	in.Region = strings.TrimSpace(in.Region)
	in.AccessKeyID = strings.TrimSpace(in.AccessKeyID)
	in.SecretAccessKey = strings.TrimSpace(in.SecretAccessKey)
	in.PublicBaseURL = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(in.PublicBaseURL), "/"))

	in.Prefix = strings.TrimSpace(in.Prefix)
	if in.Prefix == "" {
		in.Prefix = "images/"
	}
	if !strings.HasSuffix(in.Prefix, "/") {
		in.Prefix += "/"
	}
	if in.Region == "" {
		in.Region = "auto"
	}
	if in.PresignExpiry <= 0 {
		in.PresignExpiry = 24
	}
	if in.MaxDownloadBytes <= 0 {
		in.MaxDownloadBytes = defaultImageMaxDownloadBytes
	}
}
