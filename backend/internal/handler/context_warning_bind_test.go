//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func TestBindContextWarningSession(t *testing.T) {
	t.Run("non-empty hash is bound into request context", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

		bindContextWarningSession(c, "session-hash-123")

		if got := service.RequestSizeSessionHash(c.Request.Context()); got != "session-hash-123" {
			t.Fatalf("RequestSizeSessionHash() = %q, want %q", got, "session-hash-123")
		}
	})

	t.Run("empty hash leaves request context unchanged", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

		bindContextWarningSession(c, "")

		if got := service.RequestSizeSessionHash(c.Request.Context()); got != "" {
			t.Fatalf("RequestSizeSessionHash() = %q, want empty string", got)
		}
	})
}
