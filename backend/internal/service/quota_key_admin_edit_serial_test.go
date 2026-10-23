//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// §25.17.6 A07：管理员编辑也走同 key 事务。
//
// 修复前的缺陷（验收报告 A07 条目）：管理员 Update 的元数据提交走 UpdateMeta 的
// **默认 client、锁外独立事务**，而 Probe 的来源观测走每 key advisory 锁。于是管理员可以
// 在 Probe 持锁等待 lookup 期间提交新日期，Probe 提交后把该日期覆盖成旧观测值。
//
// 本文件全部落在**真实隔离 PostgreSQL**（quotaKeyTestEntClient，QUOTA_TEST_DSN 覆盖）上：
// advisory xact lock 与行锁语义在 SQLite 上不存在，用内存替身或 SQLite 证明不出串行性。
// 顺序一律由确定屏障控制（channel + 持锁方显式放行），不靠 sleep 猜竞态。
// ---------------------------------------------------------------------------

// adminEditProbeFixture 装一套真实 PG 上的 Probe + QuotaKeyService 组合。
type adminEditProbeFixture struct {
	svc    *QuotaKeyService
	auto   *CursorAutoEntryService
	client *ent.Client
	keyID  int64
	// releaseLookup 关闭后，阻塞中的 lookup 才返回上游响应。
	releaseLookup chan struct{}
	// lookupEntered 在 Probe 已持锁、正阻塞于 lookup 时关闭（证明锁确已被占用）。
	lookupEntered chan struct{}
	// lookupOnce 保证 lookupEntered 只关闭一次（Probe 内部可能重试 lookup）。
	lookupOnce sync.Once
	// releaseOnce 保证 releaseLookup 只关闭一次（用例显式放行 + cleanup 兜底）。
	releaseOnce sync.Once
}

// uniqCode 生成本用例独享的激活码明文。
//
// 固定码不可用：uq_quota_keys_code_mac 是**含软删行**的唯一索引，用例自删的软删行仍占位，
// 同一固定码第二次运行即撞唯一索引。且删除可能因用例失败而根本没跑到，故码必须每轮唯一。
func uniqCode(t *testing.T, tag string) string {
	t.Helper()
	return fmt.Sprintf("JG-CF-A07-%s-%d-%s", tag, time.Now().UnixNano(), uuid.NewString())
}

// release 放行阻塞中的 lookup（幂等）。
func (f *adminEditProbeFixture) release() {
	f.releaseOnce.Do(func() { close(f.releaseLookup) })
}

// newAdminEditProbeFixture 起一个会阻塞在 lookup 上的上游：Probe 进入 runObservationTx
// 后先取同 key 锁，再在锁内发 lookup，因此"lookup 被进入"等价于"锁已持有"。
func newAdminEditProbeFixture(t *testing.T, upstreamExpiredAt string) *adminEditProbeFixture {
	t.Helper()
	client := quotaKeyTestEntClient(t)
	// 与 Probe 共用同一 client 与同一仓储实现，保证管理员与观测落在同一个隔离库。
	repo := &testDBRepo{client: client}
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, client)

	f := &adminEditProbeFixture{
		svc:           svc,
		client:        client,
		releaseLookup: make(chan struct{}),
		lookupEntered: make(chan struct{}),
	}

	body := `{"code":0,"msg":"ok","data":{"key_info":{` +
		`"group":"Pro","key_status_code":1,"quota_key_max_quota":400,` +
		`"quota_key_used_quota":12,"device_count":1,"device_max_count":3,"mode":1`
	if upstreamExpiredAt != "" {
		body += `,"expired_at":"` + upstreamExpiredAt + `"`
	}
	body += `}}}`

	// handler 一被触达即关闭 lookupEntered：Probe 已取同 key 锁并进入锁内 lookup。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lookupOnce.Do(func() { close(f.lookupEntered) })
		w.Header().Set("Content-Type", "application/json")
		select {
		case <-f.releaseLookup:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(body))
	}))
	// 用例提前失败（Fatalf）时 Probe 可能仍阻塞在 lookup；cleanup 必须放行它，
	// 否则 httptest.Server.Close 会挂到 5 秒超时并把失败现场淹没。
	t.Cleanup(func() {
		f.release()
		srv.Close()
	})

	f.auto = NewCursorAutoEntryService(newFakeSettingRepo(), repo, NewCredcryptEncryptor(), client,
		&http.Client{Timeout: 30 * time.Second}, srv.URL)
	return f
}

// seedProbeKey 建一个可被 Probe 真实读取的来源行（明文经同一加密器写入，Probe 才能解密）。
func seedProbeKey(t *testing.T, f *adminEditProbeFixture, code string) *ent.QuotaKey {
	t.Helper()
	enc, err := NewCredcryptEncryptor().Encrypt([]byte(code))
	require.NoError(t, err)
	qk, err := f.client.QuotaKey.Create().
		SetCodeEncrypted(string(enc)).
		SetCodeMAC(quotaKeyMACOfPlaintext(code)).
		SetStatus(domain.QuotaKeyStatusActive).
		Save(context.Background())
	require.NoError(t, err)
	f.keyID = qk.ID
	t.Cleanup(func() { _ = f.client.QuotaKey.DeleteOneID(qk.ID).Exec(context.Background()) })
	return qk
}

// ---------------------------------------------------------------------------
// 1) Probe 持锁等待 lookup → 管理员改日期 → Probe 提交 → 最终保留管理员值。
//    这是修复前必然失败（管理员请求绕过同 key 锁）的那个场景。
// ---------------------------------------------------------------------------

// TestQuotaKeyService_AdminEditExpiresAtWaitsForProbeLock_AdminValueWins 钉死：
// 管理员的到期日期编辑进入**同一个**每 key 事务，因此在 Probe 持锁期间只能等待；
// Probe 提交之后管理员才写，最终 expires_at 等于管理员提交的值，不被观测覆盖。
func TestQuotaKeyService_AdminEditExpiresAtWaitsForProbeLock_AdminValueWins(t *testing.T) {
	f := newAdminEditProbeFixture(t, "2026-12-31T00:00:00Z")
	seedProbeKey(t, f, uniqCode(t, "SERIAL"))
	ctx := context.Background()

	adminValue := time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC)
	adminExpires := &adminValue
	adminArg := &adminExpires

	var (
		wg        sync.WaitGroup
		probeErr  error
		adminErr  error
		adminDone = make(chan struct{})
	)

	// Probe 先起：它取同 key 锁后阻塞在 lookup。
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, probeErr = f.auto.ProbeKeyQuota(ctx, f.keyID)
	}()

	// 等 Probe 确实已持锁（lookup 已被阻塞 = 锁在握手中）。
	requireBlockedProbeLock(t, f)

	// 管理员请求并发进入：此时锁被别人持有，它必须在锁上等待而不是锁外独立提交。
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, adminErr = f.svc.Update(ctx, f.keyID, nil, adminArg, nil)
		close(adminDone)
	}()

	// 管理员不得在 Probe 放行前完成（证明它确实在等锁，而非绕开锁外提交）。
	select {
	case <-adminDone:
		t.Fatalf("管理员请求在 Probe 持锁期间就完成了，说明它绕过了同 key 锁：adminErr=%v probeErr=%v",
			adminErr, probeErr)
	case <-time.After(300 * time.Millisecond):
	}

	// 放行 lookup，Probe 提交观测；随后管理员取得锁并写入自己的日期。
	f.release()
	wg.Wait()

	require.NoError(t, probeErr, "Probe 应正常完成")
	require.NoError(t, adminErr, "管理员编辑应正常完成")

	row, err := f.client.QuotaKey.Get(ctx, f.keyID)
	require.NoError(t, err)
	require.NotNil(t, row.ExpiresAt)
	require.Equal(t, adminValue.UTC(), row.ExpiresAt.UTC(),
		"Probe 提交后管理员才写，最终必须保留管理员提交的值（不得被旧观测覆盖）")
}

// ---------------------------------------------------------------------------
// 2) 管理员先提交，随后才开始的 Probe 仍可按既有契约更新供应商合法期限。
//    本修复不表示"人工设置永久高于未来观测"。
// ---------------------------------------------------------------------------

// TestQuotaKeyService_ProbeAfterAdminCommitStillWritesUpstreamExpiry 钉死相反次序：
// 管理员先完成提交，之后才开始的 Probe 仍按原观察契约把上游合法期限写回。
func TestQuotaKeyService_ProbeAfterAdminCommitStillWritesUpstreamExpiry(t *testing.T) {
	f := newAdminEditProbeFixture(t, "2026-12-31T00:00:00Z")
	seedProbeKey(t, f, uniqCode(t, "AFTER"))
	ctx := context.Background()

	adminValue := time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC)
	adminExpires := &adminValue
	_, err := f.svc.Update(ctx, f.keyID, nil, &adminExpires, nil)
	require.NoError(t, err)

	// Probe 在管理员提交**之后**才开始：不得被锁挡住，也不得被当作冲突拒绝。
	go func() { _, _ = f.auto.ProbeKeyQuota(ctx, f.keyID) }()
	requireBlockedProbeLock(t, f)
	f.release()

	require.Eventually(t, func() bool {
		row, gerr := f.client.QuotaKey.Get(ctx, f.keyID)
		return gerr == nil && row.ExpiresAt != nil && row.ExpiresAt.UTC().Year() == 2026
	}, 10*time.Second, 20*time.Millisecond,
		"管理员提交之后才开始的 Probe 仍应按原契约写入上游合法期限")
}

// ---------------------------------------------------------------------------
// 3) code + 日期同请求：任一步失败整笔回滚，不留部分提交。
// ---------------------------------------------------------------------------

// TestQuotaKeyService_UpdateCodeAndMetaSingleTransaction_NoPartialCommit 钉死同请求
// 的原子性：凭据与元数据收进同一笔事务，锁内重读失败时两者都不落库（修复前是分离的
// 两笔提交，可能出现"凭据已换、元数据没改"的半成功）。
func TestQuotaKeyService_UpdateCodeAndMetaSingleTransaction_NoPartialCommit(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	svc := NewQuotaKeyService(&testDBRepo{client: client}, NewCredcryptEncryptor(), nil, client)
	ctx := context.Background()

	origCode := uniqCode(t, "ORIG")
	enc, err := NewCredcryptEncryptor().Encrypt([]byte(origCode))
	require.NoError(t, err)
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted(string(enc)).
		SetCodeMAC(quotaKeyMACOfPlaintext(origCode)).
		SetCodeLast4("0000").
		SetStatus(domain.QuotaKeyStatusActive).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx) })

	// 指向不存在来源的请求：锁内重读即失败，整笔回滚。
	missingID := qk.ID + 987654
	newCode := uniqCode(t, "CHANGED")
	d := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	dArg := &d
	_, err = svc.Update(ctx, missingID, &newCode, &dArg, nil)
	require.Error(t, err, "不存在的来源必须失败")

	// 真正被创建的那一行不得有任何部分提交。
	row, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.Equal(t, string(enc), row.CodeEncrypted, "凭据不得被部分提交")
	require.Equal(t, "0000", row.CodeLast4)
	require.Nil(t, row.ExpiresAt, "日期不得被部分提交")
	require.Nil(t, row.UsageSnapshot, "快照不得被部分提交")
}

// ---------------------------------------------------------------------------
// 4) 同/异 MAC 的快照语义在同 key 事务内保持成立。
// ---------------------------------------------------------------------------

// TestQuotaKeyService_UpdateSameMACKeepsSnapshotDifferentMACClears钉死换码快照语义
// 未被同 key 事务改造破坏：异 MAC 清快照、同 MAC 保留。
func TestQuotaKeyService_UpdateSameMACKeepsSnapshotDifferentMACClears(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	svc := NewQuotaKeyService(&testDBRepo{client: client}, NewCredcryptEncryptor(), nil, client)
	ctx := context.Background()

	mk := func(tag, snapshot string) (*ent.QuotaKey, string) {
		code := uniqCode(t, tag)
		enc, err := NewCredcryptEncryptor().Encrypt([]byte(code))
		require.NoError(t, err)
		q := client.QuotaKey.Create().
			SetCodeEncrypted(string(enc)).
			SetCodeMAC(quotaKeyMACOfPlaintext(code)).
			SetStatus(domain.QuotaKeyStatusActive)
		if snapshot != "" {
			q = q.SetUsageSnapshot([]byte(snapshot))
		}
		qk, err := q.Save(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx) })
		return qk, code
	}

	const snap = `{"total":400,"used":86,"status_code":200,"checked_at":"2026-10-22T02:00:00Z",` +
		`"attempted_at":"2026-10-22T02:00:00Z","error_code":""}`

	// 异 MAC：清空快照。
	rotated, _ := mk("SRC1", snap)
	rotatedTo := uniqCode(t, "SRC2")
	_, err := svc.Update(ctx, rotated.ID, &rotatedTo, nil, nil)
	require.NoError(t, err)
	row, err := client.QuotaKey.Get(ctx, rotated.ID)
	require.NoError(t, err)
	require.Empty(t, string(row.UsageSnapshot), "异 MAC 换码必须清空旧来源快照")

	// 同 MAC：保留快照。
	// 同 MAC：明文必须与建行时**逐字相同**（MAC 只对明文负责），故复用建行码。
	kept, keptCode := mk("SRC3", snap)
	sameCode := keptCode
	_, err = svc.Update(ctx, kept.ID, &sameCode, nil, nil)
	require.NoError(t, err)
	row, err = client.QuotaKey.Get(ctx, kept.ID)
	require.NoError(t, err)
	require.JSONEq(t, snap, string(row.UsageSnapshot), "同 MAC 只重加密必须保留已提交观测")
}

// ---------------------------------------------------------------------------
// 5) 异 key 不互锁。
// ---------------------------------------------------------------------------

// TestQuotaKeyService_AdminEditDifferentKeyDoesNotBlock 钉死同 key 互斥不放大成全局锁：
// key A 持锁阻塞在 lookup 时，key B 的管理员编辑必须照常立即完成。
func TestQuotaKeyService_AdminEditDifferentKeyDoesNotBlock(t *testing.T) {
	f := newAdminEditProbeFixture(t, "2026-12-31T00:00:00Z")
	seedProbeKey(t, f, uniqCode(t, "KEYA"))
	ctx := context.Background()

	otherEnc, err := NewCredcryptEncryptor().Encrypt([]byte(uniqCode(t, "KEYB")))
	require.NoError(t, err)
	other, err := f.client.QuotaKey.Create().
		SetCodeEncrypted(string(otherEnc)).
		SetStatus(domain.QuotaKeyStatusActive).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.client.QuotaKey.DeleteOneID(other.ID).Exec(ctx) })

	probeDone := make(chan error, 1)
	go func() {
		_, perr := f.auto.ProbeKeyQuota(ctx, f.keyID)
		probeDone <- perr
	}()
	requireBlockedProbeLock(t, f)

	b := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	bArg := &b
	otherDone := make(chan error, 1)
	go func() {
		_, uerr := f.svc.Update(ctx, other.ID, nil, &bArg, nil)
		otherDone <- uerr
	}()

	select {
	case uerr := <-otherDone:
		require.NoError(t, uerr, "异 key 的管理员编辑必须不被异 key 的 Probe 阻塞")
	case <-time.After(15 * time.Second):
		t.Fatal("异 key 的管理员编辑被阻塞：同 key 互斥被放大成了跨 key 阻塞")
	}

	f.release()
	require.NoError(t, <-probeDone)
}

// requireBlockedProbeLock 用确定性屏障确认 Probe 已持锁：阻塞中的上游 handler 一被触达
// 就关闭 lookupEntered，说明 Probe 已进入锁内 lookup 阶段。
func requireBlockedProbeLock(t *testing.T, f *adminEditProbeFixture) {
	t.Helper()
	select {
	case <-f.lookupEntered:
	case <-time.After(30 * time.Second):
		t.Fatal("Probe 未能在超时内进入锁内 lookup（advisory 锁或上游装配异常）")
	}
}
