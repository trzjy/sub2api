package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// uploadStorageFake 是公告图片上传 handler 测试的存储替身。
type uploadStorageFake struct {
	saveURL         string
	deletedPrefixes []string
}

func (f *uploadStorageFake) Save(_ context.Context, _, _ string, _ []byte) (string, error) {
	return f.saveURL, nil
}

func (f *uploadStorageFake) DeleteByPrefix(_ context.Context, prefix string) (int, error) {
	f.deletedPrefixes = append(f.deletedPrefixes, prefix)
	return 1, nil
}

func (f *uploadStorageFake) HasObjectsByPrefix(context.Context, string) (bool, error) {
	return false, nil
}

// announcementUploadRepoFake 只实现公告图片上传路径用到的仓储方法。
type announcementUploadRepoFake struct {
	service.AnnouncementRepository
	item *service.Announcement
}

func (r *announcementUploadRepoFake) GetByID(_ context.Context, id int64) (*service.Announcement, error) {
	if r.item == nil {
		return nil, service.ErrAnnouncementNotFound
	}
	return r.item, nil
}

func (r *announcementUploadRepoFake) List(_ context.Context, _ pagination.PaginationParams, _ service.AnnouncementListFilters) ([]service.Announcement, *pagination.PaginationResult, error) {
	return nil, nil, nil
}

func newAnnouncementUploadTestRouter(repo *announcementUploadRepoFake) *gin.Engine {
	gin.SetMode(gin.TestMode)
	st := &uploadStorageFake{saveURL: "https://cdn.example.com/announcements/1/x.png"}
	svc := service.NewAnnouncementService(repo, nil, nil, nil,
		func(ctx context.Context, fn func(*service.ResolvedImageStorage) error) (bool, error) {
			if err := fn(&service.ResolvedImageStorage{Storage: st, DirectLink: true}); err != nil {
				return true, err
			}
			return true, nil
		})
	handler := NewAnnouncementHandler(svc)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
		c.Next()
	})
	router.POST("/admin/announcements/:id/upload-image", handler.UploadImage)
	return router
}

func buildAnnouncementUploadRequest(t *testing.T, target string, filename string, payload []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = part.Write(payload)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, target, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

func TestAdminAnnouncementUploadImageSuccess(t *testing.T) {
	repo := &announcementUploadRepoFake{item: &service.Announcement{ID: 1, Status: service.AnnouncementStatusActive}}
	router := newAnnouncementUploadTestRouter(repo)

	payload := append(append([]byte{}, pngMagic...), bytes.Repeat([]byte{0}, 512)...)
	req := buildAnnouncementUploadRequest(t, "/admin/announcements/1/upload-image", "pic.png", payload)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Equal(t, "https://cdn.example.com/announcements/1/x.png", resp.Data["url"])
}

func TestAdminAnnouncementUploadImageRejectsOversize(t *testing.T) {
	repo := &announcementUploadRepoFake{item: &service.Announcement{ID: 1, Status: service.AnnouncementStatusDraft}}
	router := newAnnouncementUploadTestRouter(repo)

	// 10 MiB 上限（方案 3.4 #7）：11 MiB 载荷必须被服务端 MaxBytesReader 拒绝。
	payload := append(append([]byte{}, pngMagic...), bytes.Repeat([]byte{0}, 11<<20)...)
	req := buildAnnouncementUploadRequest(t, "/admin/announcements/1/upload-image", "big.png", payload)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAdminAnnouncementUploadImageRejectsNonImage(t *testing.T) {
	repo := &announcementUploadRepoFake{item: &service.Announcement{ID: 1, Status: service.AnnouncementStatusDraft}}
	router := newAnnouncementUploadTestRouter(repo)

	// 不信任客户端 Content-Type：纯文本载荷被嗅探为 text/plain 拒绝。
	req := buildAnnouncementUploadRequest(t, "/admin/announcements/1/upload-image", "note.txt", []byte("just some plain text, definitely not an image"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAdminAnnouncementUploadImageNotFound(t *testing.T) {
	repo := &announcementUploadRepoFake{} // GetByID 返回 ErrAnnouncementNotFound
	router := newAnnouncementUploadTestRouter(repo)

	payload := append(append([]byte{}, pngMagic...), bytes.Repeat([]byte{0}, 512)...)
	req := buildAnnouncementUploadRequest(t, "/admin/announcements/999/upload-image", "pic.png", payload)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdminAnnouncementUploadImageRequiresAuthSubject(t *testing.T) {
	repo := &announcementUploadRepoFake{item: &service.Announcement{ID: 1, Status: service.AnnouncementStatusDraft}}
	gin.SetMode(gin.TestMode)
	svc := service.NewAnnouncementService(repo, nil, nil, nil, nil) // 未启用存储：未登录先返回 401，不会触达存储
	handler := NewAnnouncementHandler(svc)
	router := gin.New()
	router.POST("/admin/announcements/:id/upload-image", handler.UploadImage)

	payload := append(append([]byte{}, pngMagic...), bytes.Repeat([]byte{0}, 512)...)
	req := buildAnnouncementUploadRequest(t, "/admin/announcements/1/upload-image", "pic.png", payload)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
