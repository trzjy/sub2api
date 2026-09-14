package service

import (
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestProvideCodeBuddyDirectOrigin 验证 wire provider 正确投影配置（供
// NewCodeBuddyOAuthService 注入，避免 OAuth 侧直连包装静默丢失）。
func TestProvideCodeBuddyDirectOrigin(t *testing.T) {
	direct := map[string]config.CodeBuddyDirectOriginConfig{
		"cn": {Enabled: true, IP: "43.159.104.94", Host: "copilot.tencent.com"},
	}
	cfg := &config.Config{
		Gateway: config.GatewayConfig{CodeBuddy: config.GatewayCodeBuddyConfig{DirectOrigin: direct}},
	}
	require.Equal(t, direct, ProvideCodeBuddyDirectOrigin(cfg))
	require.Nil(t, ProvideCodeBuddyDirectOrigin(&config.Config{}))
}

// TestWireGenPassesCodeBuddyDirectOrigin 是 wire 重生成防护门：NewCodeBuddyOAuthService
// 的参数已改为非 variadic，且 ProviderSet 注册了 ProvideCodeBuddyDirectOrigin——若未来
// wire 重生成漏传该参数，生成代码将**编译失败**（响亮报错而非静默丢参）。此测试进一步
// 断言 wire_gen.go 确实把 codebuddy direct-origin 配置传给了构造函数（防手工回退为 variadic）。
func TestWireGenPassesCodeBuddyDirectOrigin(t *testing.T) {
	b, err := os.ReadFile("../../cmd/server/wire_gen.go")
	require.NoError(t, err, "读取 wire_gen.go 失败（相对路径应为 backend/cmd/server/wire_gen.go）")
	src := string(b)

	require.Contains(t, src, "service.ProvideCodeBuddyDirectOrigin(configConfig)",
		"wire_gen.go 必须经 ProvideCodeBuddyDirectOrigin 注入 codebuddy direct-origin 配置")
	require.Regexp(t, `service\.NewCodeBuddyOAuthService\(proxyRepository,\s*codeBuddyDirectOrigin\)`, src,
		"wire_gen.go 必须把 codebuddyDirectOrigin 传给 NewCodeBuddyOAuthService（非 variadic 双参调用）")
}
