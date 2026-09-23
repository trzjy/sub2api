package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

// announcementImageStorageFake 是公告图片测试专用的存储替身：记录 Save/DeleteByPrefix 调用。
type announcementImageStorageFake struct {
	saveURL         string
	saveErr         error
	savedKeys       []string
	deletedPrefixes []string
	deleteErr       error
}

func (f *announcementImageStorageFake) Save(_ context.Context, key, contentType string, _ []byte) (string, error) {
	if f.saveErr != nil {
		return "", f.saveErr
	}
	f.savedKeys = append(f.savedKeys, key+"|"+contentType)
	return f.saveURL, nil
}

func (f *announcementImageStorageFake) DeleteByPrefix(_ context.Context, prefix string) (int, error) {
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	f.deletedPrefixes = append(f.deletedPrefixes, prefix)
	return 1, nil
}

func (f *announcementImageStorageFake) HasObjectsByPrefix(context.Context, string) (bool, error) {
	return false, nil
}

// announcementStorageOK 返回一个始终可用的临界区替身。
func announcementStorageOK(st *ResolvedImageStorage) AnnouncementStorageCriticalSection {
	return func(_ context.Context, fn func(*ResolvedImageStorage) error) (bool, error) {
		if err := fn(st); err != nil {
			return true, err
		}
		return true, nil
	}
}

// announcementStorageNotEnabled 返回 ok=false 的临界区替身（存储未启用，不执行 fn）。
func announcementStorageNotEnabled() AnnouncementStorageCriticalSection {
	return func(context.Context, func(*ResolvedImageStorage) error) (bool, error) {
		return false, nil
	}
}

// announcementImageRepoFake 支持按 GetByID 调用序号注入"并发删除/归档发生在预检查与
// Save 之间"的窗口（R3-4/R4-4 时序测试）：UploadImage 预检查是第 1 次 GetByID，
// Save 后复读是第 2 次。
type announcementImageRepoFake struct {
	item             *Announcement
	deleteErr        error
	deleted          bool
	getByIDCalls     int
	vanishOnGetByID  int // 第 N 次 GetByID 起返回 ErrAnnouncementNotFound（行已不存在）
	archiveOnGetByID int // 第 N 次 GetByID 起把 item 状态改写为 archived（并发归档）
}

func (r *announcementImageRepoFake) Create(_ context.Context, a *Announcement) error {
	r.item = a
	return nil
}

func (r *announcementImageRepoFake) GetByID(_ context.Context, _ int64) (*Announcement, error) {
	r.getByIDCalls++
	if r.vanishOnGetByID > 0 && r.getByIDCalls >= r.vanishOnGetByID {
		return nil, ErrAnnouncementNotFound
	}
	if r.archiveOnGetByID > 0 && r.getByIDCalls >= r.archiveOnGetByID && r.item != nil {
		r.item.Status = AnnouncementStatusArchived
	}
	if r.item == nil {
		return nil, ErrAnnouncementNotFound
	}
	return r.item, nil
}

func (r *announcementImageRepoFake) Update(_ context.Context, a *Announcement) error {
	r.item = a
	return nil
}

func (r *announcementImageRepoFake) Delete(_ context.Context, _ int64) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	r.deleted = true
	return nil
}

func (r *announcementImageRepoFake) List(context.Context, pagination.PaginationParams, AnnouncementListFilters) ([]Announcement, *pagination.PaginationResult, error) {
	return nil, nil, nil
}

func (r *announcementImageRepoFake) ListActive(context.Context, time.Time) ([]Announcement, error) {
	return nil, nil
}

func newAnnouncementImageService(repo *announcementImageRepoFake, withStorage AnnouncementStorageCriticalSection) *AnnouncementService {
	return NewAnnouncementService(repo, nil, nil, nil, withStorage)
}

func directLinkStorage(st *announcementImageStorageFake) *ResolvedImageStorage {
	return &ResolvedImageStorage{Storage: st, DirectLink: true}
}

func TestAnnouncementServiceUploadImageDisabled(t *testing.T) {
	repo := &announcementImageRepoFake{item: &Announcement{ID: 1, Status: AnnouncementStatusDraft}}
	// 构造参数 nil：功能未启用
	svc := newAnnouncementImageService(repo, nil)
	_, err := svc.UploadImage(context.Background(), 1, "image/png", []byte("x"))
	require.ErrorIs(t, err, ErrAnnouncementImageStorageDisabled)

	// 临界区解析出 ok=false：同样视为未启用
	svc = newAnnouncementImageService(repo, announcementStorageNotEnabled())
	_, err = svc.UploadImage(context.Background(), 1, "image/png", []byte("x"))
	require.ErrorIs(t, err, ErrAnnouncementImageStorageDisabled)
}

func TestAnnouncementServiceUploadImageRequiresDirectLink(t *testing.T) {
	repo := &announcementImageRepoFake{item: &Announcement{ID: 1, Status: AnnouncementStatusDraft}}
	st := &announcementImageStorageFake{}
	svc := newAnnouncementImageService(repo, announcementStorageOK(&ResolvedImageStorage{Storage: st, DirectLink: false}))

	_, err := svc.UploadImage(context.Background(), 1, "image/png", []byte("x"))
	require.ErrorIs(t, err, ErrAnnouncementImageRequiresDirectLink)
	require.Empty(t, st.savedKeys, "no object may be written in presigned-only mode")
}

func TestAnnouncementServiceUploadImageRejectsUnknownAndArchived(t *testing.T) {
	st := &announcementImageStorageFake{}

	// 公告不存在
	repo := &announcementImageRepoFake{}
	svc := newAnnouncementImageService(repo, announcementStorageOK(directLinkStorage(st)))
	_, err := svc.UploadImage(context.Background(), 1, "image/png", []byte("x"))
	require.ErrorIs(t, err, ErrAnnouncementNotFound)
	require.Empty(t, st.savedKeys)

	// archived 拒绝
	repo = &announcementImageRepoFake{item: &Announcement{ID: 1, Status: AnnouncementStatusArchived}}
	svc = newAnnouncementImageService(repo, announcementStorageOK(directLinkStorage(st)))
	_, err = svc.UploadImage(context.Background(), 1, "image/png", []byte("x"))
	require.ErrorIs(t, err, ErrAnnouncementNotFound)
	require.Empty(t, st.savedKeys)
}

func TestAnnouncementServiceUploadImageSuccess(t *testing.T) {
	repo := &announcementImageRepoFake{item: &Announcement{ID: 42, Status: AnnouncementStatusDraft}}
	st := &announcementImageStorageFake{saveURL: "https://cdn.example.com/announcements/42/abc.png"}
	svc := newAnnouncementImageService(repo, announcementStorageOK(directLinkStorage(st)))

	url, err := svc.UploadImage(context.Background(), 42, "image/png", []byte("png-bytes"))
	require.NoError(t, err)
	require.Equal(t, "https://cdn.example.com/announcements/42/abc.png", url)
	require.Len(t, st.savedKeys, 1)
	parts := strings.SplitN(st.savedKeys[0], "|", 2)
	require.True(t, strings.HasPrefix(parts[0], "announcements/42/"), "key must bind announcement id under fixed namespace, got %q", parts[0])
	require.True(t, strings.HasSuffix(parts[0], ".png"), "ext must derive from detected content type, got %q", parts[0])
	require.Equal(t, "image/png", parts[1], "detected content type must be passed through to storage")
}

func TestAnnouncementServiceUploadImageSelfCleansWhenAnnouncementDeleted(t *testing.T) {
	// 并发删除窗口：删除发生在预检查（第 1 次 GetByID）与 Save 后复读（第 2 次）之间。
	repo := &announcementImageRepoFake{
		item:            &Announcement{ID: 7, Status: AnnouncementStatusDraft},
		vanishOnGetByID: 2,
	}
	st := &announcementImageStorageFake{saveURL: "https://cdn.example.com/x.png"}
	svc := newAnnouncementImageService(repo, announcementStorageOK(directLinkStorage(st)))

	_, err := svc.UploadImage(context.Background(), 7, "image/png", []byte("png-bytes"))
	require.ErrorIs(t, err, ErrAnnouncementNotFound)
	require.Len(t, st.savedKeys, 1, "Save succeeded before the re-read")
	require.Equal(t, []string{"announcements/7/"}, st.deletedPrefixes, "orphan objects must be self-cleaned with the same storage binding")
}

func TestAnnouncementServiceUploadImageKeepsObjectsWhenAnnouncementArchived(t *testing.T) {
	// 并发归档窗口：归档发生在预检查与 Save 之间 → 返回错误但绝不清理前缀（方案 3.4 #8）。
	repo := &announcementImageRepoFake{
		item:             &Announcement{ID: 7, Status: AnnouncementStatusDraft},
		archiveOnGetByID: 2,
	}
	st := &announcementImageStorageFake{saveURL: "https://cdn.example.com/x.png"}
	svc := newAnnouncementImageService(repo, announcementStorageOK(directLinkStorage(st)))

	_, err := svc.UploadImage(context.Background(), 7, "image/png", []byte("png-bytes"))
	require.ErrorIs(t, err, ErrAnnouncementNotFound)
	require.Len(t, st.savedKeys, 1)
	require.Empty(t, st.deletedPrefixes, "archived announcements keep their images; prefix must not be cleaned")
}

func TestAnnouncementServiceDeleteCascadesImages(t *testing.T) {
	repo := &announcementImageRepoFake{item: &Announcement{ID: 9, Status: AnnouncementStatusActive}}
	st := &announcementImageStorageFake{}
	svc := newAnnouncementImageService(repo, announcementStorageOK(directLinkStorage(st)))

	require.NoError(t, svc.Delete(context.Background(), 9))
	require.True(t, repo.deleted, "DB row deleted first (plan 3.4 #1)")
	require.Equal(t, []string{"announcements/9/"}, st.deletedPrefixes)
}

func TestAnnouncementServiceDeleteCascadeErrorDoesNotBlock(t *testing.T) {
	repo := &announcementImageRepoFake{item: &Announcement{ID: 9, Status: AnnouncementStatusActive}}
	st := &announcementImageStorageFake{deleteErr: context.DeadlineExceeded}
	svc := newAnnouncementImageService(repo, announcementStorageOK(directLinkStorage(st)))

	require.NoError(t, svc.Delete(context.Background(), 9), "object deletion failure must not block announcement deletion")
	require.True(t, repo.deleted)
	// fake 在报错路径不记录前缀；DeleteImages 必须吞掉该错误（仅 WARN），不向调用方返回失败。
	require.Empty(t, st.deletedPrefixes)
}

func TestAnnouncementServiceDeleteImagesNoopWhenStorageDisabled(t *testing.T) {
	repo := &announcementImageRepoFake{}
	svc := newAnnouncementImageService(repo, nil)

	// nil 临界区（功能未启用）：WARN 后返回，不 panic、不触碰存储。
	svc.DeleteImages(context.Background(), 1)

	// ok=false 同样只记 WARN。
	st := &announcementImageStorageFake{}
	svc = newAnnouncementImageService(repo, announcementStorageNotEnabled())
	svc.DeleteImages(context.Background(), 1)
	require.Empty(t, st.deletedPrefixes)
}
