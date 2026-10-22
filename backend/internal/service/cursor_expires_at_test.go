//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 到期时间事实源回写与展示收敛（C-U7-B / W2-B1）：lookup / refresh → quota_keys.expires_at。
// quota_keys.expires_at 是唯一持久化事实源（快照内不复制期限）；空值不写（保留旧值），
// 不伪造 / 清空。观测侧期限写入与 usage_snapshot 同事务（§25.14-C 步骤 4）。
// （parseKeyExpiresAt 的解析规则单测见 cursor_upstream_client_test.go:TestParseKeyExpiresAt，
//  同包复用、共用同一 RFC3339 解析，本卡不复制第二套测试。）
// ---------------------------------------------------------------------------

// TestCursorExpiresAt_ProbeWritesBack：lookup 成功返回有效 expired_at → 经 UpdateMeta
// 回写同一 quota_keys.expires_at 字段。
func TestCursorExpiresAt_ProbeWritesBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"key_info":{` +
			`"group":"Pro","key_status_code":1,"quota_key_max_quota":400,` +
			`"quota_key_used_quota":12,"device_count":1,"device_max_count":3,` +
			`"mode":1,"expired_at":"2026-12-31T00:00:00Z"}}}`))
	}))
	defer srv.Close()

	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST"}
	qkID := int64(42)
	repo.accounts = []*ent.Account{{ID: 100, QuotaKeyID: &qkID}}

	svc := NewCursorAutoEntryService(newFakeSettingRepo(), repo, testEncryptor{}, nil, &http.Client{Timeout: 5 * time.Second}, srv.URL)

	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err)
	require.NotNil(t, repo.byID[42].ExpiresAt, "lookup 有效 expired_at 必须回写到 quota_keys.expires_at")
	require.Equal(t, 2026, repo.byID[42].ExpiresAt.Year())
	require.Equal(t, time.December, repo.byID[42].ExpiresAt.Month())
	require.Equal(t, 31, repo.byID[42].ExpiresAt.Day())
}

// TestCursorExpiresAt_ProbeEmptyKeepsOld：lookup 空值 + 已有旧期限 → 旧期限保留（不覆盖不清空）。
func TestCursorExpiresAt_ProbeEmptyKeepsOld(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 上游 key_info 未给出 expired_at（字段完全缺失）。
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"key_info":{` +
			`"group":"Pro","key_status_code":1,"quota_key_max_quota":400,` +
			`"quota_key_used_quota":12,"device_count":1,"device_max_count":3,"mode":1}}}`))
	}))
	defer srv.Close()

	repo := newFakeQuotaKeyRepo()
	old := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST", ExpiresAt: &old}
	qkID := int64(42)
	repo.accounts = []*ent.Account{{ID: 100, QuotaKeyID: &qkID}}

	svc := NewCursorAutoEntryService(newFakeSettingRepo(), repo, testEncryptor{}, nil, &http.Client{Timeout: 5 * time.Second}, srv.URL)

	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err)
	require.NotNil(t, repo.byID[42].ExpiresAt, "lookup 空值不得清空已有旧期限")
	require.Equal(t, old.UTC(), repo.byID[42].ExpiresAt.UTC(), "lookup 空值必须保留旧期限")
}

// TestCursorExpiresAt_ProbeEmptyNoOldStaysNil：lookup 空值 + 无旧值 → 保持 nil。
// 前端据此展示"到期时间未知"；后端绝不伪造 / 清空任何日期。
func TestCursorExpiresAt_ProbeEmptyNoOldStaysNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"key_info":{` +
			`"group":"Pro","key_status_code":1,"quota_key_max_quota":400,` +
			`"quota_key_used_quota":12,"device_count":1,"device_max_count":3,"mode":1}}}`))
	}))
	defer srv.Close()

	repo := newFakeQuotaKeyRepo()
	// ExpiresAt 初始为 nil（无旧值）。
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST"}
	qkID := int64(42)
	repo.accounts = []*ent.Account{{ID: 100, QuotaKeyID: &qkID}}

	svc := NewCursorAutoEntryService(newFakeSettingRepo(), repo, testEncryptor{}, nil, &http.Client{Timeout: 5 * time.Second}, srv.URL)

	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err)
	require.Nil(t, repo.byID[42].ExpiresAt, "lookup 空值且无旧值 → 保持 nil（前端展示 到期时间未知）")
}

// TestQuotaKeyExpiresAt_RefreshWritesBack：refresh（B-11）返回有效 expired_at → 经
// SetNillableExpiresAt 落库到 quota_keys.expires_at（真实 Postgres 集成，证明写入）。
func TestQuotaKeyExpiresAt_RefreshWritesBack(t *testing.T) {
	const expiresAt = "2026-12-31T00:00:00Z"
	parsed, err := time.Parse(time.RFC3339, expiresAt)
	require.NoError(t, err)
	expiresAtPtr := parsed

	upstream := &fakeUpstreamClient{batch: &UpstreamPullResult{
		Accounts:  []*UpstreamAccount{{Name: "pulled-refresh", Platform: domain.PlatformCursor, Type: AccountTypeAPIKey, Credentials: map[string]any{}}},
		ExpiresAt: &expiresAtPtr,
	}}
	svc, client := qkNewTestService(t, upstream)
	ctx := context.Background()
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	t.Cleanup(func() { _ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx) })

	require.NoError(t, svc.PullAccounts(ctx, qk.ID), "refresh 拉号应成功")

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt, "refresh 有效 expired_at 必须写入 quota_keys.expires_at")
	require.Equal(t, expiresAtPtr.UTC(), got.ExpiresAt.UTC())
}
