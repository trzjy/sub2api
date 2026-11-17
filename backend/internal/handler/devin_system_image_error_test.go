//go:build unit

package handler

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestDevinClientFacingError 覆盖 devinClientFacingError 的映射边界：
// 仅 ErrDevinSystemImageUnsupported（含被包裹形态）映射为 400，其它错误不接管。
func TestDevinClientFacingError(t *testing.T) {
	t.Run("sentinel maps to 400", func(t *testing.T) {
		status, msg, ok := devinClientFacingError(service.ErrDevinSystemImageUnsupported)
		require.True(t, ok)
		require.Equal(t, http.StatusBadRequest, status)
		require.Equal(t, service.ErrDevinSystemImageUnsupported.Error(), msg)
	})

	t.Run("wrapped sentinel maps to 400", func(t *testing.T) {
		wrapped := fmt.Errorf("forward devin: %w", service.ErrDevinSystemImageUnsupported)
		status, msg, ok := devinClientFacingError(wrapped)
		require.True(t, ok)
		require.Equal(t, http.StatusBadRequest, status)
		require.Equal(t, wrapped.Error(), msg)
	})

	t.Run("unrelated error is not handled", func(t *testing.T) {
		status, msg, ok := devinClientFacingError(errors.New("some upstream error"))
		require.False(t, ok)
		require.Equal(t, 0, status)
		require.Empty(t, msg)
	})

	t.Run("nil error is not handled", func(t *testing.T) {
		status, msg, ok := devinClientFacingError(nil)
		require.False(t, ok)
		require.Equal(t, 0, status)
		require.Empty(t, msg)
	})
}
