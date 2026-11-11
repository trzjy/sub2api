package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// fakeAuthCacheInvalidator 是 APIKeyAuthCacheInvalidator 的内存测试替身，仅存在于测试代码。
// 不连接任何缓存后端、不发起请求；仅用于装配断言中证明 provider 把该依赖注入到了
// QuotaKeyService（QKG-B9 强制增补：runMode 与 authCache 必须在同一装配点注入）。
type fakeAuthCacheInvalidator struct{}

func (fakeAuthCacheInvalidator) InvalidateAuthCacheByKey(_ context.Context, _ string)    {}
func (fakeAuthCacheInvalidator) InvalidateAuthCacheByUserID(_ context.Context, _ int64)  {}
func (fakeAuthCacheInvalidator) InvalidateAuthCacheByGroupID(_ context.Context, _ int64) {}

// TestProvideQuotaKeyService_InjectsRunModeAndAuthCache 是 QKG-B9 的装配断言用例：
// 直接调用 wire 装配点 ProvideQuotaKeyService，验证它确实把 cfg.RunMode（Simple 模式）
// 与分组认证缓存失效器注入到 *QuotaKeyService，而非只写注释。
//
// 背景（强制增补，卡外已核实缺陷）：若装配点不注入 runMode，则 runMode 恒为空，
// Simple 模式下 composite 分组不可绑定的守卫成为死代码。本用例是其在测试层的证据。
func TestProvideQuotaKeyService_InjectsRunModeAndAuthCache(t *testing.T) {
	const wantRunMode = config.RunModeSimple
	cfg := &config.Config{RunMode: wantRunMode}

	svc, err := ProvideQuotaKeyService(
		newFakeQuotaKeyRepo(),
		NewCredcryptEncryptor(),
		nil, // *ent.Client：构造期不触碰，仅存储引用
		nil, // *CursorAutoEntryService：可选依赖
		nil, // QuotaUpstreamRegistry：可选依赖
		fakeAuthCacheInvalidator{},
		cfg,
	)
	if err != nil {
		t.Fatalf("ProvideQuotaKeyService 返回错误: %v", err)
	}
	if svc == nil {
		t.Fatal("ProvideQuotaKeyService 返回 nil 服务")
	}

	// 断言 1：runMode 已被注入（非空且等于配置值）。空串会被守卫视为 standard，
	// 无法触发 Simple 模式 composite 不可绑定逻辑，故必须严格等于注入值。
	if svc.runMode != wantRunMode {
		t.Fatalf("runMode 未被正确注入: got %q, want %q", svc.runMode, wantRunMode)
	}

	// 断言 2：分组认证缓存失效器已被注入（非 nil），保证白名单改动后能即时失效缓存。
	if svc.authCache == nil {
		t.Fatal("authCache 未被注入: svc.authCache 为 nil")
	}
}
