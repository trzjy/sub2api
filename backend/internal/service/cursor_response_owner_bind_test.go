//go:build unit

package service

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestCursorBindHTTPResponseOwner 覆盖 CursorExecutionService 续轮归属登记回调的定向单测。
// 全部走真实生产函数 bindHTTPResponseOwner，仅替换注入的回调，不 mock 任何生产逻辑。
func TestCursorBindHTTPResponseOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 1) 未注入回调：不 panic、不调用（无 binder 时内部静默返回）。
	t.Run("no binder injected", func(t *testing.T) {
		s := &CursorExecutionService{}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())

		// 即便传入非空 responseID，未注入 binder 也必须静默返回，不 panic。
		require.NotPanics(t, func() {
			s.bindHTTPResponseOwner(c, "resp-123")
		})
		require.Nil(t, s.httpResponseOwnerBinder, "未注入时 binder 必须保持为 nil，印证没有任何调用点")
	})

	// 2) 注入回调：被调用一次，收到的 responseID 与传入一致（用闭包记录）。
	t.Run("binder called once with matching id", func(t *testing.T) {
		s := &CursorExecutionService{}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())

		var (
			calls  int
			gotID  string
			gotCtx *gin.Context
		)
		s.SetHTTPResponseOwnerBinder(func(cc *gin.Context, id string) error {
			calls++
			gotID = id
			gotCtx = cc
			return nil
		})

		s.bindHTTPResponseOwner(c, "resp-xyz")

		require.Equal(t, 1, calls)
		require.Equal(t, "resp-xyz", gotID)
		require.Same(t, c, gotCtx)
	})

	// 3) responseID 为空串/全空白：不调用回调。
	t.Run("empty or whitespace responseID skips binder", func(t *testing.T) {
		s := &CursorExecutionService{}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())

		calls := 0
		s.SetHTTPResponseOwnerBinder(func(cc *gin.Context, id string) error {
			calls++
			return nil
		})

		s.bindHTTPResponseOwner(c, "")
		require.Equal(t, 0, calls, "空串不应调用回调")

		s.bindHTTPResponseOwner(c, "   ")
		require.Equal(t, 0, calls, "全空白不应调用回调")
	})

	// 4) 回调返回 error：bindHTTPResponseOwner 不 panic、不上抛（失败只记 warn）。
	t.Run("binder error is swallowed", func(t *testing.T) {
		s := &CursorExecutionService{}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		// 错误路径会访问 c.Request.Context()，必须有真实 Request。
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

		calls := 0
		s.SetHTTPResponseOwnerBinder(func(cc *gin.Context, id string) error {
			calls++
			return errors.New("bind failed")
		})

		require.NotPanics(t, func() {
			s.bindHTTPResponseOwner(c, "resp-err")
		})
		// 回调确实被调用了一次（错误被吞掉，而不是上抛）。
		require.Equal(t, 1, calls)
	})
}
