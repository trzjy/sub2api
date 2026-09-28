package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeCockpitImportCommitRepo 是 CockpitImportCommitRepository 的内存假实现，
// 用于不依赖 DB 的 service 层编排测试。
type fakeCockpitImportCommitRepo struct {
	lastPayloads []CockpitImportAccountPayload
	lastResult   CockpitImportCommitResult
	lastErr      error
	calls        int
}

func (f *fakeCockpitImportCommitRepo) CommitCockpitImport(ctx context.Context, payloads []CockpitImportAccountPayload) (CockpitImportCommitResult, error) {
	f.calls++
	f.lastPayloads = payloads
	return f.lastResult, f.lastErr
}

// TestCockpitImportCommitPayloadsForwards 白盒验证私有写入路径 commitPayloads
// 正确转发载荷并汇总计数（v16 §1.5：载荷写入仅凭证校验后的 CommitCockpitImport 可达，
// 测试直接走私有路径钉转发语义）。
func TestCockpitImportCommitPayloadsForwards(t *testing.T) {
	fake := &fakeCockpitImportCommitRepo{
		lastResult: CockpitImportCommitResult{Created: 3, SkippedExisting: 1},
	}
	svc := &CockpitImportCommitService{repo: fake}

	payloads := []CockpitImportAccountPayload{
		{Platform: "claude", UID: "u1", Name: "n1", AccountType: "oauth", Credentials: map[string]any{"access_token": "a"}},
		{Platform: "claude", UID: "u2", Name: "n2", AccountType: "oauth", Credentials: map[string]any{"access_token": "b"}},
	}

	res, err := svc.commitPayloads(context.Background(), payloads)
	require.NoError(t, err)
	require.Equal(t, 1, fake.calls, "应恰好调用一次仓储")
	require.Equal(t, payloads, fake.lastPayloads, "应原样转发载荷")
	require.Equal(t, 3, res.Created)
	require.Equal(t, 1, res.SkippedExisting)
}

// TestCockpitImportCommitPayloadsEmpty 验证空载荷直接返回零计数（无需落库）。
func TestCockpitImportCommitPayloadsEmpty(t *testing.T) {
	fake := &fakeCockpitImportCommitRepo{lastResult: CockpitImportCommitResult{}}
	svc := &CockpitImportCommitService{repo: fake}

	res, err := svc.commitPayloads(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, fake.calls, "空载荷不应调用仓储")
	require.Equal(t, 0, res.Created)
	require.Equal(t, 0, res.SkippedExisting)
}

// TestCockpitImportCommitPayloadsPropagatesError 验证仓储错误向上传播（失败关闭，不吞错）。
func TestCockpitImportCommitPayloadsPropagatesError(t *testing.T) {
	fake := &fakeCockpitImportCommitRepo{lastErr: errBoom{}}
	svc := &CockpitImportCommitService{repo: fake}

	_, err := svc.commitPayloads(context.Background(), []CockpitImportAccountPayload{
		{Platform: "claude", UID: "u1", Name: "n1", AccountType: "oauth"},
	})
	require.Error(t, err)
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

// —— D-10b 整改 6：enabled-零载荷失败关闭闸（v16 §1.2，纯函数钉语义）——

// TestCockpitCommitPayloadGate enabled 条目存在而载荷为空 → COMMIT_PAYLOAD_MISSING 失败关闭；
// 其余组合（无 enabled 条目 / 载荷非空 / 空解析结果）放行。
func TestCockpitCommitPayloadGate(t *testing.T) {
	// enabled 条目 > 0 且载荷为空 → 失败关闭（禁止静默 created=0 假完成面）。
	err := checkCockpitCommitPayloadGate(&CockpitImportResult{
		EnabledPlatformAccounts: map[string]int{"claude": 2},
	}, nil)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitCommitPayloadMissing))
	require.Equal(t, "COMMIT_PAYLOAD_MISSING", ErrCockpitCommitPayloadMissing, "错误码值钉死")

	// enabled 条目 > 0 且载荷非空 → 放行。
	err = checkCockpitCommitPayloadGate(&CockpitImportResult{
		EnabledPlatformAccounts: map[string]int{"claude": 2},
	}, []CockpitImportAccountPayload{{Platform: "claude", UID: "u1"}})
	require.NoError(t, err)

	// 无 enabled 条目（当前线上形态：enabled 表为空）且载荷为空 → 放行。
	err = checkCockpitCommitPayloadGate(&CockpitImportResult{
		EnabledPlatformAccounts: map[string]int{},
	}, nil)
	require.NoError(t, err)

	// 空解析结果 → 放行。
	require.NoError(t, checkCockpitCommitPayloadGate(nil, nil))
}

// —— D-10b 整改 7：超时即终止（v16 §1.5）——

// buildLargeCockpitEnvelope 构造大输入信封：单个 pending slug 携带 1 条含大量填充字段的
// 条目（字段长度均低于 64KB 上限），总字节数使严格 JSON 解码耗时远超测试时限，
// 保证超时先于解析完成触发、且分类循环存在 ctx 检查点（含 ≥1 个 slug）。
func buildLargeCockpitEnvelope(t *testing.T) []byte {
	t.Helper()
	pad := strings.Repeat("p", 40*1024) // 40KB，低于 64KB 单字段上限
	entry := map[string]interface{}{"uid": "u1"}
	for i := 0; i < 200; i++ { // ~8MB 总负载
		entry[fmt.Sprintf("pad%03d", i)] = pad
	}
	return buildCockpitEnvelope(t, "cockpit-tools.data-transfer", 1, false, false,
		map[string][]map[string]interface{}{"grok": {entry}})
}

// TestRunCockpitParseWithTimeoutGoroutineExits 大输入 + 已取消 ctx：
// 外层超时返回后，解析协程必须在下一检查点响应取消退出（而非跑完整解析或泄漏）——
// 以 done 通道产出佐证（channel 佐证，不 sleep 硬等）：产出的错误必须是检查点取消错误
// PREVIEW_PARSE_TIMEOUT，证明协程在检查点终止而非自然完成。
func TestRunCockpitParseWithTimeoutGoroutineExits(t *testing.T) {
	raw := buildLargeCockpitEnvelope(t)

	canceled, cancel := context.WithCancel(context.Background())
	cancel() // 父 ctx 已取消：时限立即到期，外层走超时失败关闭分支

	_, err, done := runCockpitParseWithLimit(canceled, raw, time.Hour)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitPreviewParseTimeout),
		"已取消 ctx 下外层应超时失败关闭")

	// 协程退出佐证：阻塞等 done 产出（协程在大输入解码后首个检查点响应取消退出）。
	select {
	case o := <-done:
		require.Error(t, o.err, "解析协程不得自然完成（大输入 + 已取消 ctx）")
		require.True(t, IsCockpitErrorCode(o.err, ErrCockpitPreviewParseTimeout),
			"解析协程应在检查点以 PREVIEW_PARSE_TIMEOUT 退出")
	case <-time.After(30 * time.Second):
		t.Fatal("解析协程未在检查点退出（疑似泄漏：超时返回后协程仍持有输入运行）")
	}
}
