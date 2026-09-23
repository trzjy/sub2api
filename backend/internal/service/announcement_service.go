package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

var (
	// ErrAnnouncementImageStorageDisabled 表示对象存储未启用（构造参数为 nil，或
	// WithAnnouncementStorage 临界区解析出 ok=false）。见方案 3.2(c)。
	ErrAnnouncementImageStorageDisabled = infraerrors.BadRequest(
		"ANNOUNCEMENT_IMAGE_STORAGE_DISABLED",
		"图片存储未启用",
	)
	// ErrAnnouncementImageRequiresDirectLink 表示存储已启用但未配置 public_base_url，
	// Save 只能返回 presigned 临时链接，与公告图片长期可访问语义矛盾（方案 3.1 直链强制）。
	ErrAnnouncementImageRequiresDirectLink = infraerrors.BadRequest(
		"ANNOUNCEMENT_IMAGE_REQUIRES_DIRECT_LINK",
		"公告图片要求配置 public_base_url 直链",
	)
)

// AnnouncementStorageCriticalSection 是公告图片存储临界区（方案 3.2(b)）：
// 在互斥锁内解析当前存储绑定并执行 fn；ok=false 表示存储未启用（不执行 fn）。
type AnnouncementStorageCriticalSection = func(ctx context.Context, fn func(*ResolvedImageStorage) error) (ok bool, err error)

type AnnouncementService struct {
	announcementRepo AnnouncementRepository
	readRepo         AnnouncementReadRepository
	userRepo         UserRepository
	userSubRepo      UserSubscriptionRepository
	// withAnnouncementStorage 为 nil 表示公告图片功能未启用（测试传 nil；生产由 wire
	// 注入 ImageStorageSettingService.WithAnnouncementStorage 方法值）。
	withAnnouncementStorage AnnouncementStorageCriticalSection
}

func NewAnnouncementService(
	announcementRepo AnnouncementRepository,
	readRepo AnnouncementReadRepository,
	userRepo UserRepository,
	userSubRepo UserSubscriptionRepository,
	withAnnouncementStorage AnnouncementStorageCriticalSection,
) *AnnouncementService {
	return &AnnouncementService{
		announcementRepo:        announcementRepo,
		readRepo:                readRepo,
		userRepo:                userRepo,
		userSubRepo:             userSubRepo,
		withAnnouncementStorage: withAnnouncementStorage,
	}
}

type CreateAnnouncementInput struct {
	Title      string
	Content    string
	Status     string
	NotifyMode string
	Targeting  AnnouncementTargeting
	StartsAt   *time.Time
	EndsAt     *time.Time
	ActorID    *int64 // 管理员用户ID
}

type UpdateAnnouncementInput struct {
	Title      *string
	Content    *string
	Status     *string
	NotifyMode *string
	Targeting  *AnnouncementTargeting
	StartsAt   **time.Time
	EndsAt     **time.Time
	ActorID    *int64 // 管理员用户ID
}

type UserAnnouncement struct {
	Announcement Announcement
	ReadAt       *time.Time
}

type AnnouncementUserReadStatus struct {
	UserID   int64      `json:"user_id"`
	Email    string     `json:"email"`
	Username string     `json:"username"`
	Balance  float64    `json:"balance"`
	Eligible bool       `json:"eligible"`
	ReadAt   *time.Time `json:"read_at,omitempty"`
}

func (s *AnnouncementService) Create(ctx context.Context, input *CreateAnnouncementInput) (*Announcement, error) {
	if input == nil {
		return nil, ErrAnnouncementNilInput
	}

	if !isJSONTimeInRange(input.StartsAt) || !isJSONTimeInRange(input.EndsAt) {
		return nil, ErrAnnouncementInvalidSchedule
	}

	title := strings.TrimSpace(input.Title)
	content := strings.TrimSpace(input.Content)
	if title == "" || len(title) > 200 {
		return nil, ErrAnnouncementInvalidTitle
	}
	if content == "" {
		return nil, ErrAnnouncementContentRequired
	}

	status := strings.TrimSpace(input.Status)
	if status == "" {
		status = AnnouncementStatusDraft
	}
	if !isValidAnnouncementStatus(status) {
		return nil, ErrAnnouncementInvalidStatus
	}

	targeting, err := domain.AnnouncementTargeting(input.Targeting).NormalizeAndValidate()
	if err != nil {
		return nil, err
	}

	notifyMode := strings.TrimSpace(input.NotifyMode)
	if notifyMode == "" {
		notifyMode = AnnouncementNotifyModeSilent
	}
	if !isValidAnnouncementNotifyMode(notifyMode) {
		return nil, ErrAnnouncementInvalidNotifyMode
	}

	if input.StartsAt != nil && input.EndsAt != nil {
		if !input.StartsAt.Before(*input.EndsAt) {
			return nil, ErrAnnouncementInvalidSchedule
		}
	}

	a := &Announcement{
		Title:      title,
		Content:    content,
		Status:     status,
		NotifyMode: notifyMode,
		Targeting:  targeting,
		StartsAt:   input.StartsAt,
		EndsAt:     input.EndsAt,
	}
	if input.ActorID != nil && *input.ActorID > 0 {
		a.CreatedBy = input.ActorID
		a.UpdatedBy = input.ActorID
	}

	if err := s.announcementRepo.Create(ctx, a); err != nil {
		return nil, fmt.Errorf("create announcement: %w", err)
	}
	return a, nil
}

func (s *AnnouncementService) Update(ctx context.Context, id int64, input *UpdateAnnouncementInput) (*Announcement, error) {
	if input == nil {
		return nil, ErrAnnouncementNilInput
	}

	if (input.StartsAt != nil && !isJSONTimeInRange(*input.StartsAt)) ||
		(input.EndsAt != nil && !isJSONTimeInRange(*input.EndsAt)) {
		return nil, ErrAnnouncementInvalidSchedule
	}

	a, err := s.announcementRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	if input.Title != nil {
		title := strings.TrimSpace(*input.Title)
		if title == "" || len(title) > 200 {
			return nil, ErrAnnouncementInvalidTitle
		}
		a.Title = title
	}
	if input.Content != nil {
		content := strings.TrimSpace(*input.Content)
		if content == "" {
			return nil, ErrAnnouncementContentRequired
		}
		a.Content = content
	}
	if input.Status != nil {
		status := strings.TrimSpace(*input.Status)
		if !isValidAnnouncementStatus(status) {
			return nil, ErrAnnouncementInvalidStatus
		}
		a.Status = status
	}

	if input.NotifyMode != nil {
		notifyMode := strings.TrimSpace(*input.NotifyMode)
		if !isValidAnnouncementNotifyMode(notifyMode) {
			return nil, ErrAnnouncementInvalidNotifyMode
		}
		a.NotifyMode = notifyMode
	}

	if input.Targeting != nil {
		targeting, err := domain.AnnouncementTargeting(*input.Targeting).NormalizeAndValidate()
		if err != nil {
			return nil, err
		}
		a.Targeting = targeting
	}

	if input.StartsAt != nil {
		a.StartsAt = *input.StartsAt
	}
	if input.EndsAt != nil {
		a.EndsAt = *input.EndsAt
	}

	if a.StartsAt != nil && a.EndsAt != nil {
		if !a.StartsAt.Before(*a.EndsAt) {
			return nil, ErrAnnouncementInvalidSchedule
		}
	}

	if input.ActorID != nil && *input.ActorID > 0 {
		a.UpdatedBy = input.ActorID
	}

	if err := s.announcementRepo.Update(ctx, a); err != nil {
		return nil, fmt.Errorf("update announcement: %w", err)
	}
	return a, nil
}

func (s *AnnouncementService) Delete(ctx context.Context, id int64) error {
	if err := s.announcementRepo.Delete(ctx, id); err != nil {
		return fmt.Errorf("delete announcement: %w", err)
	}
	// 先删 DB 后删对象（方案 3.4 #1）：对象删除失败只记日志，公告删除仍成功返回。
	s.DeleteImages(ctx, id)
	return nil
}

// UploadImage 把管理员上传的图片存入对象存储，key 绑定公告 ID（方案 3.2(c)）。
// announcementID 必须已存在且状态为 draft/active（archived 拒绝）；要求直链模式，
// 返回可直接插入 Markdown 的长期图片 URL。
func (s *AnnouncementService) UploadImage(ctx context.Context, announcementID int64, contentType string, data []byte) (string, error) {
	if s.withAnnouncementStorage == nil {
		return "", ErrAnnouncementImageStorageDisabled
	}

	// 锁外预检查：公告存在性 + 状态（archived 拒绝）；并发删除/归档窗口由 Save 后复读闭合。
	a, err := s.announcementRepo.GetByID(ctx, announcementID)
	if err != nil {
		return "", err
	}
	if a.Status != AnnouncementStatusDraft && a.Status != AnnouncementStatusActive {
		return "", ErrAnnouncementNotFound
	}

	var url string
	ok, err := s.withAnnouncementStorage(ctx, func(st *ResolvedImageStorage) error {
		if !st.DirectLink {
			return ErrAnnouncementImageRequiresDirectLink
		}
		// 固定顶层命名空间常量（R2），不拼 resolver 字段；key 全服务端生成。
		key := fmt.Sprintf("%s%d/%s%s", AnnouncementImagesPrefix, announcementID, uuid.NewString(), extensionForContentType(contentType))
		saved, err := st.Storage.Save(ctx, key, contentType, data)
		if err != nil {
			return err
		}

		// Save 成功后复读（R3-4/R4-4 must_fix）：闭合"预检查与 Save 之间"的并发窗口。
		latest, err := s.announcementRepo.GetByID(ctx, announcementID)
		if err != nil {
			if errors.Is(err, ErrAnnouncementNotFound) {
				// 公告行已不存在（并发删除窗口）→ 用同一存储绑定自清理前缀（幂等），
				// 不得为已删除公告返回成功 URL 留孤儿对象。清理失败只记日志。
				if _, delErr := st.Storage.DeleteByPrefix(ctx, announcementImagesPrefixFor(announcementID)); delErr != nil {
					logger.L().Warn("announcement_image.self_clean_failed",
						zap.Int64("announcement_id", announcementID), zap.Error(delErr))
				}
				return ErrAnnouncementNotFound
			}
			// 复读遇到其他错误：无法确认公告已删除，不清理（误删既有图片比留孤儿对象更严重），原样返回错误。
			return err
		}
		if latest.Status == AnnouncementStatusArchived {
			// 并发归档窗口 → 仅拒绝，不清理前缀：归档公告既有图片必须保留（方案 3.4 #8）。
			return ErrAnnouncementNotFound
		}
		url = saved
		return nil
	})
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrAnnouncementImageStorageDisabled
	}
	return url, nil
}

// DeleteImages 按公告 ID 前缀删除其关联图片对象。尽力而为：失败记 WARN 日志，
// 不阻塞、不返回错误（方案 3.2(c)/(d)）。经 WithAnnouncementStorage 临界区执行；
// 存储不可用（nil 或 ok=false）时显式 WARN 后返回——不静默吞掉（R2）。
func (s *AnnouncementService) DeleteImages(ctx context.Context, announcementID int64) {
	if s.withAnnouncementStorage == nil {
		logger.L().Warn("announcement_images.storage_disabled_skip_delete",
			zap.Int64("announcement_id", announcementID))
		return
	}
	ok, err := s.withAnnouncementStorage(ctx, func(st *ResolvedImageStorage) error {
		_, err := st.Storage.DeleteByPrefix(ctx, announcementImagesPrefixFor(announcementID))
		return err
	})
	if !ok {
		logger.L().Warn("announcement_images.storage_not_enabled_skip_delete",
			zap.Int64("announcement_id", announcementID))
		return
	}
	if err != nil {
		logger.L().Warn("announcement_images.delete_failed",
			zap.Int64("announcement_id", announcementID), zap.Error(err))
	}
}

// announcementImagesPrefixFor 返回指定公告的图片对象 key 前缀（含尾部斜杠）。
func announcementImagesPrefixFor(announcementID int64) string {
	return fmt.Sprintf("%s%d/", AnnouncementImagesPrefix, announcementID)
}

func (s *AnnouncementService) GetByID(ctx context.Context, id int64) (*Announcement, error) {
	return s.announcementRepo.GetByID(ctx, id)
}

func (s *AnnouncementService) List(ctx context.Context, params pagination.PaginationParams, filters AnnouncementListFilters) ([]Announcement, *pagination.PaginationResult, error) {
	return s.announcementRepo.List(ctx, params, filters)
}

func (s *AnnouncementService) ListForUser(ctx context.Context, userID int64, unreadOnly bool) ([]UserAnnouncement, error) {
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}

	activeSubs, err := s.userSubRepo.ListActiveByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list active subscriptions: %w", err)
	}
	activeGroupIDs := make(map[int64]struct{}, len(activeSubs))
	for i := range activeSubs {
		activeGroupIDs[activeSubs[i].GroupID] = struct{}{}
	}

	now := time.Now()
	anns, err := s.announcementRepo.ListActive(ctx, now)
	if err != nil {
		return nil, fmt.Errorf("list active announcements: %w", err)
	}

	visible := make([]Announcement, 0, len(anns))
	ids := make([]int64, 0, len(anns))
	for i := range anns {
		a := anns[i]
		if !a.IsActiveAt(now) {
			continue
		}
		if !a.Targeting.Matches(user.Balance, activeGroupIDs) {
			continue
		}
		visible = append(visible, a)
		ids = append(ids, a.ID)
	}

	if len(visible) == 0 {
		return []UserAnnouncement{}, nil
	}

	readMap, err := s.readRepo.GetReadMapByUser(ctx, userID, ids)
	if err != nil {
		return nil, fmt.Errorf("get read map: %w", err)
	}

	out := make([]UserAnnouncement, 0, len(visible))
	for i := range visible {
		a := visible[i]
		readAt, ok := readMap[a.ID]
		if unreadOnly && ok {
			continue
		}
		var ptr *time.Time
		if ok {
			t := readAt
			ptr = &t
		}
		out = append(out, UserAnnouncement{
			Announcement: a,
			ReadAt:       ptr,
		})
	}

	// 未读优先、同状态按创建时间倒序
	sort.Slice(out, func(i, j int) bool {
		ai, aj := out[i], out[j]
		if (ai.ReadAt == nil) != (aj.ReadAt == nil) {
			return ai.ReadAt == nil
		}
		return ai.Announcement.ID > aj.Announcement.ID
	})

	return out, nil
}

func (s *AnnouncementService) MarkRead(ctx context.Context, userID, announcementID int64) error {
	// 安全：仅允许标记当前用户“可见”的公告
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}

	a, err := s.announcementRepo.GetByID(ctx, announcementID)
	if err != nil {
		return err
	}

	now := time.Now()
	if !a.IsActiveAt(now) {
		return ErrAnnouncementNotFound
	}

	activeSubs, err := s.userSubRepo.ListActiveByUserID(ctx, userID)
	if err != nil {
		return fmt.Errorf("list active subscriptions: %w", err)
	}
	activeGroupIDs := make(map[int64]struct{}, len(activeSubs))
	for i := range activeSubs {
		activeGroupIDs[activeSubs[i].GroupID] = struct{}{}
	}

	if !a.Targeting.Matches(user.Balance, activeGroupIDs) {
		return ErrAnnouncementNotFound
	}

	if err := s.readRepo.MarkRead(ctx, announcementID, userID, now); err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	return nil
}

func (s *AnnouncementService) ListUserReadStatus(
	ctx context.Context,
	announcementID int64,
	params pagination.PaginationParams,
	search string,
) ([]AnnouncementUserReadStatus, *pagination.PaginationResult, error) {
	ann, err := s.announcementRepo.GetByID(ctx, announcementID)
	if err != nil {
		return nil, nil, err
	}

	filters := UserListFilters{
		Search: strings.TrimSpace(search),
	}

	users, page, err := s.userRepo.ListWithFilters(ctx, params, filters)
	if err != nil {
		return nil, nil, fmt.Errorf("list users: %w", err)
	}

	userIDs := make([]int64, 0, len(users))
	for i := range users {
		userIDs = append(userIDs, users[i].ID)
	}

	readMap, err := s.readRepo.GetReadMapByUsers(ctx, announcementID, userIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("get read map: %w", err)
	}

	out := make([]AnnouncementUserReadStatus, 0, len(users))
	for i := range users {
		u := users[i]
		subs, err := s.userSubRepo.ListActiveByUserID(ctx, u.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("list active subscriptions: %w", err)
		}
		activeGroupIDs := make(map[int64]struct{}, len(subs))
		for j := range subs {
			activeGroupIDs[subs[j].GroupID] = struct{}{}
		}

		readAt, ok := readMap[u.ID]
		var ptr *time.Time
		if ok {
			t := readAt
			ptr = &t
		}

		out = append(out, AnnouncementUserReadStatus{
			UserID:   u.ID,
			Email:    u.Email,
			Username: u.Username,
			Balance:  u.Balance,
			Eligible: domain.AnnouncementTargeting(ann.Targeting).Matches(u.Balance, activeGroupIDs),
			ReadAt:   ptr,
		})
	}

	return out, page, nil
}

func isValidAnnouncementStatus(status string) bool {
	switch status {
	case AnnouncementStatusDraft, AnnouncementStatusActive, AnnouncementStatusArchived:
		return true
	default:
		return false
	}
}

func isValidAnnouncementNotifyMode(mode string) bool {
	switch mode {
	case AnnouncementNotifyModeSilent, AnnouncementNotifyModePopup:
		return true
	default:
		return false
	}
}
