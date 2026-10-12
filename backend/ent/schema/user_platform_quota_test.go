package schema

import (
	"testing"

	"entgo.io/ent"
	"entgo.io/ent/entc/load"
	"github.com/stretchr/testify/require"
)

// TestUserPlatformQuotaPlatformValidator 锁定 w31 冲突解决裁定：
// UserPlatformQuota.platform 的准入集合须逐字节复刻 ours（12 项），其中
// "other" 与 "muse" 必须被接受 —— 这是保留显式 switch 而非
// domain.IsConcretePlatform 的关键（后者依据 domain/platforms.go 的
// platformList，刻意不含 "other"，会令 other 在配额写入链路不可达）。
//
// 若误换成注册表校验，validator("other") 将返回 error，本测试会失败，
// 从而反向锁定 w31 裁定不被静默破坏。
//
// 注意：platform 字段同时挂有 NotEmpty() 与显式 switch 两个校验器，
// ent 运行期会依次执行并返回首个 error，故此处聚合全部 func(string) error
// 校验器后模拟同一语义。
func TestUserPlatformQuotaPlatformValidator(t *testing.T) {
	spec, err := (&load.Config{Path: "."}).Load()
	require.NoError(t, err)

	schemas := map[string]*load.Schema{}
	for _, schema := range spec.Schemas {
		schemas[schema.Name] = schema
	}

	quota := requireSchema(t, schemas, "UserPlatformQuota")
	requireSchemaFields(t, quota, "user_id", "platform")

	validator := platformStringValidator(t, UserPlatformQuota{}.Fields(), "platform")

	// 我方显式 switch 的 12 项准入集合，须全部通过（与 ours 逐字节一致）。
	allowed := []string{
		"anthropic", "openai", "gemini", "antigravity", "grok",
		"kimi", "zhipu", "deepseek", "minimax", "codebuddy", "other", "muse",
	}
	for _, p := range allowed {
		require.NoErrorf(t, validator(p), "platform %q should be allowed", p)
	}

	// 关键反证：若误换成 domain.IsConcretePlatform 注册表校验，"other" 会被拒绝。
	// 此处必须接受，否则 w31 裁定被破坏。
	require.NoError(t, validator("other"), `platform "other" MUST remain allowed (w31 ruling)`)

	// 未注册平台必须被拒绝。
	require.Error(t, validator("unknown"))
	require.Error(t, validator(""))
}

// platformStringValidator 收集 platform 字段上全部 func(string) error 校验器，
// 模拟 ent 运行期依次执行、返回首个 error 的语义。
func platformStringValidator(t *testing.T, fields []ent.Field, name string) func(string) error {
	t.Helper()

	for _, entField := range fields {
		descriptor := entField.Descriptor()
		if descriptor.Name != name {
			continue
		}
		require.NotEmpty(t, descriptor.Validators, "field %s should include a validator", name)

		var funcs []func(string) error
		for _, v := range descriptor.Validators {
			if fn, ok := v.(func(string) error); ok {
				funcs = append(funcs, fn)
			}
		}
		require.NotEmpty(t, funcs, "field %s should include a func(string) error validator", name)

		return func(s string) error {
			for _, fn := range funcs {
				if err := fn(s); err != nil {
					return err
				}
			}
			return nil
		}
	}

	require.Failf(t, "missing field validator", "schema should include field %s", name)
	return nil
}
