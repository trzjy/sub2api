package admin

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// TestCursorGroupPlatformValidation 钉死 gin 平台校验放行 cursor（建组 / 改组 / composite 路由）。
func TestCursorGroupPlatformValidation(t *testing.T) {
	require.True(t, domain.IsGroupPlatform(domain.PlatformCursor),
		"cursor must pass group_platform validation (Create/Update group), else creating a cursor group returns 400")
	require.True(t, domain.IsConcretePlatform(domain.PlatformCursor),
		"cursor must pass concrete_platform validation (composite route target_platform)")
}
