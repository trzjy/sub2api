//go:build unit

package service

// B-10 回归测试：出站鉴权 deviceId 的生成算法与并发防重。
//
// 向量来源（权威，非本实现自证）：把插件
// ~/.sub2api-acceptance/cursor-20261013/02-webcrack-out/deobfuscated.js:8070-8077
// 的 hashDeviceId 逐字复制到 node 22 执行，输出下表 expected 列。
// 逐字复制的函数体见本文件 TestHashDeviceIDMatchesPluginVectors 的注释。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
)

// pluginHashDeviceIDVectors 是「插件源码 → node 实测」的权威向量。
//
// emoji 一行是本卡存在的**关键反证**：仓库既有 hashDeviceID（按 Unicode 码点迭代）
// 对它算出 0001F600，而插件按 UTF-16 码元迭代算出 001B0D63，两者不等。
// 该行专门锁住"必须按 UTF-16 码元迭代"，防止有人日后把实现改回码点迭代而无人发现。
var pluginHashDeviceIDVectors = []struct {
	name     string
	input    string
	expected string
}{
	{"uuid_v4_典型", "d0f2c1a4-9b3e-4f7a-8c5d-1e2f3a4b5c6d", "1F26E7C0"},
	{"uuid_v4_另一枚", "2f1a9c88-3b4d-4e6f-9a7c-5d8e1f2a3b4c", "1CC000FA"},
	{"uuid_nil", "00000000-0000-4000-8000-000000000000", "0B6DD53C"},
	{"uuid_max", "ffffffff-ffff-4fff-bfff-ffffffffffff", "492F44D2"},
	{"单字符", "a", "00000061"},
	{"两字符", "ab", "00000C21"},
	{"三字符", "abc", "00017862"},
	{"空串", "", "00000000"},
	{"激活码形状", "CURSOR-ABCD-1234-EFGH-5678", "32F41B16"},
	// 下面两行是"码点迭代 vs UTF-16 码元迭代"的分叉点，见上方注释。
	{"BMP中文_码点与码元同值", "你好世界", "256ECCD3"},
	{"emoji_代理对_码元迭代", "\U0001F600", "001B0D63"},
	{"长串环绕", "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ", "503BB3C0"},
}

// TestHashDeviceIDMatchesPluginVectors 锁定 hashDeviceIDFromUUID 与插件逐字一致。
//
// 权威向量由以下 JS（deobfuscated.js:8070-8077 逐字复制，node 22 实测）生成：
//
//	function hashDeviceId(s) {
//	  let a = 0;
//	  for (let i = 0; i < s.length; i++) { a = (a << 5) - a + s.charCodeAt(i); a &= a; }
//	  return Math.abs(a).toString(16).padStart(8, "0").substring(0, 8).toUpperCase();
//	}
func TestHashDeviceIDMatchesPluginVectors(t *testing.T) {
	for _, tc := range pluginHashDeviceIDVectors {
		t.Run(tc.name, func(t *testing.T) {
			if got := hashDeviceIDFromUUID(tc.input); got != tc.expected {
				t.Errorf("hashDeviceIDFromUUID(%q) = %s, want %s (插件 node 实测值)", tc.input, got, tc.expected)
			}
		})
	}
}

// TestHashDeviceIDDiffersFromRuneIteration 显式反证：既有 hashDeviceID（码点迭代）
// 在 emoji 上与插件不等，故 B-10 不能直接复用它。
//
// 这条断言锁的是"两者确实不同"。若将来有人把既有实现也改成 UTF-16 码元迭代
// （两者合一），本测试会失败——那时应删除本测试与新实现中重复的说明，
// 而不是让它悄悄退化。
func TestHashDeviceIDDiffersFromRuneIteration(t *testing.T) {
	const emoji = "\U0001F600"
	const pluginValue = "001B0D63"
	if got := hashDeviceIDFromUUID(emoji); got != pluginValue {
		t.Fatalf("B-10 实现 = %s, want 插件值 %s", got, pluginValue)
	}
	if got := hashDeviceID(emoji); got == pluginValue {
		t.Fatalf("既有 hashDeviceID 已与插件一致（=%s），B-10 的独立实现已成冗余，应合并", got)
	}
}

// TestGenerateCursorDeviceID 校验生成物的形态约束。
func TestGenerateCursorDeviceID(t *testing.T) {
	const n = 200
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		got, err := GenerateCursorDeviceID()
		if err != nil {
			t.Fatalf("GenerateCursorDeviceID: %v", err)
		}
		if len(got) != 8 {
			t.Fatalf("deviceId %q 长度 = %d, want 8（插件 padStart(8)+substring(0,8)）", got, len(got))
		}
		if got != strings.ToUpper(got) {
			t.Errorf("deviceId %q 必须大写（插件 toUpperCase）", got)
		}
		if strings.Trim(got, "0123456789ABCDEF") != "" {
			t.Errorf("deviceId %q 含非十六进制字符", got)
		}
		seen[got] = struct{}{}
	}
	// 随机源校验：200 次几乎不可能碰撞。阈值取 1（允许恰好碰撞一次）过于敏感，
	// 故只断言"绝大多数互不相同"，不追求数学上的互异。
	if len(seen) < n*9/10 {
		t.Errorf("200 次生成仅 %d 个不同值，随机源可能退化", len(seen))
	}
}

// TestGenerateCursorDeviceIDIsNotDerivedFromActivationCode 反证 B-10 核心裁定：
// deviceId 与激活码无关，故同一激活码反复取值不得退化成"按激活码派生"。
// 下方 TestDeviceIDForReusesExisting 覆盖"复用"语义；此处覆盖"随机性不受输入影响"。
func TestGenerateCursorDeviceIDIsNotDerivedFromActivationCode(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 50; i++ {
		got, err := GenerateCursorDeviceID()
		if err != nil {
			t.Fatalf("GenerateCursorDeviceID: %v", err)
		}
		seen[got] = struct{}{}
	}
	if len(seen) < 45 {
		t.Errorf("50 次生成仅 %d 个不同值，deviceId 不应是激活码的确定性函数", len(seen))
	}
}

// TestDeviceIDForGeneratesAndPersists 首次调用：生成并落库。
func TestDeviceIDForGeneratesAndPersists(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.byID[7] = &ent.QuotaKey{ID: 7, CodeEncrypted: "ENC"}
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)

	got, err := svc.DeviceIDFor(context.Background(), 7)
	if err != nil {
		t.Fatalf("DeviceIDFor: %v", err)
	}
	if len(got) != 8 || got != strings.ToUpper(got) {
		t.Fatalf("deviceId %q 形态非法", got)
	}
	stored := repo.byID[7].DeviceID
	if stored == nil {
		t.Fatal("deviceId 未落库")
	}
	if *stored != got {
		t.Errorf("落库值 %q != 返回值 %q", *stored, got)
	}
}

// TestDeviceIDForReusesExisting 已有值：直接复用，且不再写库。
func TestDeviceIDForReusesExisting(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	preset := "ABCD1234"
	repo.byID[7] = &ent.QuotaKey{ID: 7, CodeEncrypted: "ENC", DeviceID: &preset}
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)

	got, err := svc.DeviceIDFor(context.Background(), 7)
	if err != nil {
		t.Fatalf("DeviceIDFor: %v", err)
	}
	if got != preset {
		t.Errorf("DeviceIDFor = %q, want 已落库的 %q（必须复用，不得重算）", got, preset)
	}
	if *repo.byID[7].DeviceID != preset {
		t.Errorf("已有值被覆盖为 %q", *repo.byID[7].DeviceID)
	}
}

// TestDeviceIDForTreatsEmptyAsAbsent 空串视为未生成，重新生成并落库。
//
// 与 SetDeviceIDIfAbsent 的 WHERE（IS NULL OR =”）语义对齐：若读侧把空串当"已有"、
// 写侧却仍可写，竞态分支就会读到本请求自己刚写的值。
func TestDeviceIDForTreatsEmptyAsAbsent(t *testing.T) {
	empty := ""
	repo := newFakeQuotaKeyRepo()
	repo.byID[7] = &ent.QuotaKey{ID: 7, CodeEncrypted: "ENC", DeviceID: &empty}
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)

	got, err := svc.DeviceIDFor(context.Background(), 7)
	if err != nil {
		t.Fatalf("DeviceIDFor: %v", err)
	}
	if got == "" {
		t.Fatal("空 deviceId 未被重新生成")
	}
	if len(got) != 8 {
		t.Errorf("重新生成的值 %q 长度非法", got)
	}
	if *repo.byID[7].DeviceID != got {
		t.Errorf("重新生成的值未落库：库中 %q，返回 %q", *repo.byID[7].DeviceID, got)
	}
}

// TestDeviceIDForConcurrentFirstUseDoesNotSplit 并发首次：N 个 goroutine 同时进入，
// 必须收敛到**同一个** deviceId（这正是 DB 条件更新要防的分裂）。
func TestDeviceIDForConcurrentFirstUseDoesNotSplit(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.byID[7] = &ent.QuotaKey{ID: 7, CodeEncrypted: "ENC"}
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)

	const n = 64
	results := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // 最大化并发首次进入的竞争
			got, err := svc.DeviceIDFor(context.Background(), 7)
			if err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
				return
			}
			results[idx] = got
		}(i)
	}
	close(start)
	wg.Wait()

	uniq := make(map[string]struct{})
	for i, got := range results {
		if got == "" {
			t.Fatalf("goroutine %d 得到空 deviceId", i)
		}
		uniq[got] = struct{}{}
	}
	if len(uniq) != 1 {
		t.Errorf("并发首次产生 %d 个不同 deviceId，期望 1 个：%v", len(uniq), uniq)
	}
	// 落库值必须与所有调用方返回值一致。
	stored := repo.byID[7].DeviceID
	if stored == nil {
		t.Fatal("deviceId 未落库")
	}
	if _, ok := uniq[*stored]; !ok {
		t.Errorf("落库值 %q 不在任何调用方返回值中", *stored)
	}
}

// TestDeviceIDForRaceLoserReturnsWinnerValue 反证"0 行必须重读而不能返回候选值"：
// 构造 SetDeviceIDIfAbsent 返回 0 且库中已存在赢家值的场景，断言返回的是赢家值。
func TestDeviceIDForRaceLoserReturnsWinnerValue(t *testing.T) {
	// 用一个在首次 GetByID 返回空值后、SetDeviceIDIfAbsent 之前才被写入的替身，
	// 稳定地走到"0 行 → 重读"分支（真并发下该分支由其它 goroutine 触发）。
	repo := newFakeQuotaKeyRepo()
	repo.byID[7] = &ent.QuotaKey{ID: 7, CodeEncrypted: "ENC"}
	winner := "11111111"
	svc := NewQuotaKeyService(&raceRepo{fakeQuotaKeyRepo: repo, winner: winner}, NewCredcryptEncryptor(), nil, nil)

	got, err := svc.DeviceIDFor(context.Background(), 7)
	if err != nil {
		t.Fatalf("DeviceIDFor: %v", err)
	}
	if got != winner {
		t.Errorf("落败方返回 %q, want 赢家值 %q（0 行时必须重读，绝不能返回自己的候选值）", got, winner)
	}
}

// TestDeviceIDForFailsClosed 各种不可得场景一律失败关闭，绝不返回空串或兜底值。
func TestDeviceIDForFailsClosed(t *testing.T) {
	t.Run("quota_key 不存在", func(t *testing.T) {
		svc := NewQuotaKeyService(newFakeQuotaKeyRepo(), NewCredcryptEncryptor(), nil, nil)
		if _, err := svc.DeviceIDFor(context.Background(), 404); !errors.Is(err, ErrCursorDeviceIDUnavailable) {
			t.Errorf("err = %v, want ErrCursorDeviceIDUnavailable", err)
		}
	})
	t.Run("条件写 0 行但库内仍为空", func(t *testing.T) {
		repo := newFakeQuotaKeyRepo()
		repo.byID[7] = &ent.QuotaKey{ID: 7, CodeEncrypted: "ENC"}
		svc := NewQuotaKeyService(&silentZeroRepo{fakeQuotaKeyRepo: repo}, NewCredcryptEncryptor(), nil, nil)
		// 不得返回候选值：否则同一激活码会分裂出两份 deviceId。
		if _, err := svc.DeviceIDFor(context.Background(), 7); !errors.Is(err, ErrCursorDeviceIDUnavailable) {
			t.Errorf("err = %v, want ErrCursorDeviceIDUnavailable（0 行却读不到值属异常态，须失败关闭）", err)
		}
	})
	t.Run("写库失败", func(t *testing.T) {
		repo := newFakeQuotaKeyRepo()
		repo.byID[7] = &ent.QuotaKey{ID: 7, CodeEncrypted: "ENC"}
		svc := NewQuotaKeyService(&failingWriteRepo{fakeQuotaKeyRepo: repo}, NewCredcryptEncryptor(), nil, nil)
		if _, err := svc.DeviceIDFor(context.Background(), 7); err == nil {
			t.Error("写库失败必须返回错误，绝不静默返回候选值")
		}
	})
}

// ---------------------------------------------------------------------------
// 以下三个替身只用于制造 DeviceIDFor 的竞态分支。
// ---------------------------------------------------------------------------

// raceRepo 在首次 GetByID 返回空值后、SetDeviceIDIfAbsent 之前注入赢家值，
// 模拟"另一个并发请求抢先写入"，从而稳定地走到 0 行 → 重读分支。
type raceRepo struct {
	*fakeQuotaKeyRepo
	winner    string
	injected  bool
	writeOnce sync.Once
}

func (r *raceRepo) GetByID(ctx context.Context, id int64) (*ent.QuotaKey, error) {
	qk, err := r.fakeQuotaKeyRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// 第一次读（探测阶段）返回 device_id 为空的行。
	if !r.injected {
		clone := *qk
		clone.DeviceID = nil
		return &clone, nil
	}
	return qk, nil
}

func (r *raceRepo) SetDeviceIDIfAbsent(_ context.Context, id int64, _ string) (int, error) {
	r.injected = true
	qk := r.byID[id]
	winner := r.winner
	qk.DeviceID = &winner
	r.writeOnce.Do(func() {})
	return 0, nil // 声称条件未命中（已被并发写入）
}

// silentZeroRepo 恒返回 0 行且不写入，模拟"条件未命中但库内仍为空"的异常态。
type silentZeroRepo struct{ *fakeQuotaKeyRepo }

func (r *silentZeroRepo) SetDeviceIDIfAbsent(_ context.Context, _ int64, _ string) (int, error) {
	return 0, nil
}

// failingWriteRepo 模拟写库失败。
type failingWriteRepo struct{ *fakeQuotaKeyRepo }

func (r *failingWriteRepo) SetDeviceIDIfAbsent(_ context.Context, _ int64, _ string) (int, error) {
	return 0, errors.New("db down")
}
