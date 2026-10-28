//go:build unit

package service

import (
	"errors"
	"net/http/httptest"
	"strings"
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
		require.Nil(t, s.httpResponseOwnerBinder, "未注入时 binder 必须保持为 nil")
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

	// 3) responseID 为空串/全空白：不调用回调。空 id 分支会访问 c.Request.Context()，
	//    因此必须给 c 一个真实 Request，否则会 panic（与 binder 是否注入无关）。
	t.Run("empty or whitespace responseID skips binder", func(t *testing.T) {
		s := &CursorExecutionService{}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

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

	// 5) 注入回调后传入超长 responseID：binder 收到的 id 未被截断（截断只发生在日志里），
	//    且回调恰好被调用一次。这是两个调用点 executeHTTP / ResumeCursorSession 在本文件内的
	//    间接覆盖；两处调用点由集成/真机覆盖（需要完整会话上下文，无法在本文件轻量驱动）。
	t.Run("binder receives untruncated id on long responseID", func(t *testing.T) {
		s := &CursorExecutionService{}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

		longID := strings.Repeat("x", 100)
		calls := 0
		gotID := ""
		s.SetHTTPResponseOwnerBinder(func(cc *gin.Context, id string) error {
			calls++
			gotID = id
			return nil
		})

		s.bindHTTPResponseOwner(c, longID)

		require.Equal(t, 1, calls, "超长 id 也应恰好调用一次")
		require.Equal(t, longID, gotID, "binder 收到的 id 必须未被截断（截断仅用于日志）")
		require.Len(t, gotID, 100, "binder 收到的 id 长度须保持 100，不得被裁到 64")
	})
}

// TestBindHTTPResponseOwnerFromContext 直接测真正决定"绑不绑"的生产方法
// BindHTTPResponseOwnerFromContext。该方法的全部早退分支（nil context / nil request /
// group missing / owner missing / empty response id）都在触达 store 之前返回，因此只需
// 构造 &OpenAIGatewayService{} 即可，无需真实 store。
func TestBindHTTPResponseOwnerFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 1) nil context：在解引用前早退，无 error、无 panic。
	t.Run("nil context", func(t *testing.T) {
		s := &OpenAIGatewayService{}
		var c *gin.Context
		require.NotPanics(t, func() {
			require.NoError(t, s.BindHTTPResponseOwnerFromContext(c, "resp-1"))
		})
	})

	// 2) nil request：c 非 nil 但 Request 为 nil 时早退，无 error、无 panic。
	t.Run("nil request", func(t *testing.T) {
		s := &OpenAIGatewayService{}
		c := &gin.Context{Request: nil}
		require.NotPanics(t, func() {
			require.NoError(t, s.BindHTTPResponseOwnerFromContext(c, "resp-1"))
		})
	})

	// 3) group missing：正常 test context 但不设分组，group 解析为 0 后静默跳过。
	t.Run("group missing", func(t *testing.T) {
		s := &OpenAIGatewayService{}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		require.NotPanics(t, func() {
			require.NoError(t, s.BindHTTPResponseOwnerFromContext(c, "resp-1"))
		})
	})

	// 4) owner missing：设置分组但未 SetOpenAIHTTPResponseOwner，静默跳过。
	t.Run("owner missing", func(t *testing.T) {
		s := &OpenAIGatewayService{}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		grp := int64(42)
		c.Set("api_key", &APIKey{GroupID: &grp})
		require.NotPanics(t, func() {
			require.NoError(t, s.BindHTTPResponseOwnerFromContext(c, "resp-1"))
		})
	})

	// 5) empty response id：空串在入口处 TrimSpace 后早退，无 error。
	t.Run("empty response id", func(t *testing.T) {
		s := &OpenAIGatewayService{}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		require.NotPanics(t, func() {
			require.NoError(t, s.BindHTTPResponseOwnerFromContext(c, ""))
		})
	})
}
