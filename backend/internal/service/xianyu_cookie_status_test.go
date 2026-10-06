package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 回归：Cookie 状态必须从 Worker 最近一次自动续期结果推导，
// 不再被同步逻辑写死为 unknown（否则 UI 永远显示"未知"、失效告警永远无法触发）。
func TestDeriveCookieStatus(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Hour).Format("2006-01-02T15:04:05")
	stale := now.Add(-25 * time.Hour).Format("2006-01-02T15:04:05")

	cases := []struct {
		name string
		acc  XianyuWorkerAccountStatus
		want string
	}{
		{"worker suspended wins", XianyuWorkerAccountStatus{Status: "suspended", LastRenewStatus: "success", LastRenewAt: fresh}, XianyuCookieStatusInvalid},
		{"renew failed", XianyuWorkerAccountStatus{Status: "active", LastRenewStatus: "failed", LastRenewAt: fresh}, XianyuCookieStatusInvalid},
		{"need password login", XianyuWorkerAccountStatus{Status: "active", LastRenewStatus: "need_password_login"}, XianyuCookieStatusInvalid},
		{"fresh success", XianyuWorkerAccountStatus{Status: "active", LastRenewStatus: "success", LastRenewAt: fresh}, XianyuCookieStatusValid},
		{"cookie updated fresh", XianyuWorkerAccountStatus{Status: "active", LastRenewStatus: "cookie_updated", LastRenewAt: fresh}, XianyuCookieStatusValid},
		{"stale success", XianyuWorkerAccountStatus{Status: "active", LastRenewStatus: "success", LastRenewAt: stale}, XianyuCookieStatusExpiring},
		{"unparsable renew time treated fresh", XianyuWorkerAccountStatus{Status: "active", LastRenewStatus: "success", LastRenewAt: "not-a-time"}, XianyuCookieStatusValid},
		{"no renew data stays unknown", XianyuWorkerAccountStatus{Status: "active"}, XianyuCookieStatusUnknown},
		{"worker disabled without renew data stays unknown", XianyuWorkerAccountStatus{Status: "disabled"}, XianyuCookieStatusUnknown},
		{"fresh login counts as valid without renew logs", XianyuWorkerAccountStatus{Status: "active", LastLoginAt: fresh}, XianyuCookieStatusValid},
		{"stale login without renew logs stays unknown", XianyuWorkerAccountStatus{Status: "active", LastLoginAt: stale}, XianyuCookieStatusUnknown},
		{"renew failure wins over fresh login", XianyuWorkerAccountStatus{Status: "active", LastRenewStatus: "failed", LastLoginAt: fresh}, XianyuCookieStatusInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, deriveCookieStatus(tc.acc, now))
		})
	}
}

type syncAccountControlStub struct {
	*xianyuWorkerControlStub
	upserts       map[string]XianyuAccount
	localAccounts []XianyuAccount
	updated       []XianyuAccount
}

func (s *syncAccountControlStub) GetAccountByWorkerAndAccountID(context.Context, int64, string) (*XianyuAccount, error) {
	return nil, ErrXianyuAccountNotFound
}

func (s *syncAccountControlStub) UpsertAccount(_ context.Context, a XianyuAccount) (*XianyuAccount, error) {
	s.upserts[a.AccountID] = a
	return &a, nil
}

func (s *syncAccountControlStub) ListAccounts(context.Context, int64) ([]XianyuAccount, error) {
	return s.localAccounts, nil
}

func (s *syncAccountControlStub) UpdateAccount(_ context.Context, a XianyuAccount) (*XianyuAccount, error) {
	s.updated = append(s.updated, a)
	return &a, nil
}

// 回归：本地有、Worker 侧已不存在的账号，必须在同步对账时收敛为"已退出登录"，
// 不能等管理员点启用/刷新触发 404 自愈——否则账号列表长期残留"已停用/可启用"的误导状态。
func TestSyncAccountsConvergesMissingAccountsToLoggedOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":[` +
			`{"account_id":"1001","nickname":"a","enabled":true,"status":"active"}` +
			`]}`))
	}))
	defer srv.Close()

	ctrl := &syncAccountControlStub{
		xianyuWorkerControlStub: &xianyuWorkerControlStub{cfg: &XianyuWorkerConfig{
			ID: 1, BaseURL: srv.URL, APITokenEncrypted: "ENC:token", Status: XianyuWorkerStatusActive,
		}},
		upserts: map[string]XianyuAccount{},
		localAccounts: []XianyuAccount{
			{WorkerConfigID: 1, AccountID: "1001", Status: XianyuAccountStatusEnabled},
			{WorkerConfigID: 1, AccountID: "2002", Status: XianyuAccountStatusDisabled, TaskStatus: XianyuTaskStatusStopped},
			{WorkerConfigID: 1, AccountID: "3003", Status: XianyuAccountStatusLoggedOut, TaskStatus: XianyuTaskStatusStopped},
		},
	}
	svc := &XianyuWorkerService{
		control:    ctrl,
		encryptor:  testSecretEncryptor{},
		forbidLoop: true,
		clientFor: func(baseURL, token string) *XianyuWorkerClient {
			return NewXianyuWorkerClient(baseURL, token, 5*time.Second)
		},
	}

	require.NoError(t, svc.SyncAccounts(context.Background()))

	// 只有 Worker 侧消失且尚未收敛的 2002 被收敛为已退出登录。
	require.Len(t, ctrl.updated, 1)
	require.Equal(t, "2002", ctrl.updated[0].AccountID)
	require.Equal(t, XianyuAccountStatusLoggedOut, ctrl.updated[0].Status)
	require.Equal(t, XianyuTaskStatusStopped, ctrl.updated[0].TaskStatus)
}

func TestSyncAccountsDerivesCookieStatusAndDetail(t *testing.T) {
	fresh := time.Now().Add(-time.Hour).Format("2006-01-02T15:04:05")
	longReason := strings.Repeat("长", 600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/internal/cookies/details", r.URL.Path)
		_, _ = w.Write([]byte(`{"success":true,"data":[` +
			`{"account_id":"1001","nickname":"a","enabled":true,"status":"active","last_renew_status":"failed","last_renew_at":"` + fresh + `","last_renew_error":"续期接口未返回有效Cookie"},` +
			`{"account_id":"1002","nickname":"b","enabled":true,"status":"active","last_renew_status":"success","last_renew_at":"` + fresh + `"},` +
			`{"account_id":"1003","nickname":"c","enabled":true,"status":"active"},` +
			`{"account_id":"1004","nickname":"d","enabled":false,"status":"suspended","disable_reason":"违规暂停"},` +
			`{"account_id":"1005","nickname":"e","enabled":false,"status":"suspended","disable_reason":"` + longReason + `"}` +
			`]}`))
	}))
	defer srv.Close()

	ctrl := &syncAccountControlStub{
		xianyuWorkerControlStub: &xianyuWorkerControlStub{cfg: &XianyuWorkerConfig{
			ID: 1, BaseURL: srv.URL, APITokenEncrypted: "ENC:token", Status: XianyuWorkerStatusActive,
		}},
		upserts: map[string]XianyuAccount{},
	}
	svc := &XianyuWorkerService{
		control:    ctrl,
		encryptor:  testSecretEncryptor{},
		forbidLoop: true,
		clientFor: func(baseURL, token string) *XianyuWorkerClient {
			return NewXianyuWorkerClient(baseURL, token, 5*time.Second)
		},
	}

	require.NoError(t, svc.SyncAccounts(context.Background()))

	failed := ctrl.upserts["1001"]
	require.Equal(t, XianyuCookieStatusInvalid, failed.CookieStatus)
	require.Equal(t, "续期接口未返回有效Cookie", failed.CookieDetail)

	healthy := ctrl.upserts["1002"]
	require.Equal(t, XianyuCookieStatusValid, healthy.CookieStatus)
	require.Equal(t, "", healthy.CookieDetail)

	noData := ctrl.upserts["1003"]
	require.Equal(t, XianyuCookieStatusUnknown, noData.CookieStatus)
	require.Equal(t, "", noData.CookieDetail)

	// Worker 侧账号状态导致的失效没有续期错误，回退到 disable_reason。
	suspended := ctrl.upserts["1004"]
	require.Equal(t, XianyuCookieStatusInvalid, suspended.CookieStatus)
	require.Equal(t, "违规暂停", suspended.CookieDetail)

	// 超长原因按 500 字符截断，避免超出 cookie_detail 列宽导致写库失败。
	truncated := ctrl.upserts["1005"]
	require.Equal(t, XianyuCookieStatusInvalid, truncated.CookieStatus)
	require.Equal(t, 500, len([]rune(truncated.CookieDetail)))
}

type refreshCookieControlStub struct {
	*xianyuWorkerControlStub
	account *XianyuAccount
	updated XianyuAccount
}

func (s *refreshCookieControlStub) GetAccountByWorkerAndAccountID(context.Context, int64, string) (*XianyuAccount, error) {
	return s.account, nil
}

func (s *refreshCookieControlStub) UpdateAccount(_ context.Context, a XianyuAccount) (*XianyuAccount, error) {
	s.updated = a
	return &a, nil
}

func TestRefreshCookieMarksStatusOnRenewResult(t *testing.T) {
	account := &XianyuAccount{
		WorkerConfigID: 1,
		AccountID:      "838831211",
		Status:         XianyuAccountStatusDisabled,
		CookieStatus:   XianyuCookieStatusUnknown,
		TaskStatus:     XianyuTaskStatusStopped,
	}

	newService := func(t *testing.T, handler http.HandlerFunc) *XianyuWorkerService {
		t.Helper()
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		return &XianyuWorkerService{
			control: &refreshCookieControlStub{
				xianyuWorkerControlStub: &xianyuWorkerControlStub{cfg: &XianyuWorkerConfig{
					ID: 1, BaseURL: srv.URL, APITokenEncrypted: "ENC:token", Status: XianyuWorkerStatusActive,
				}},
				account: account,
			},
			encryptor:  testSecretEncryptor{},
			forbidLoop: true,
			clientFor: func(baseURL, token string) *XianyuWorkerClient {
				return NewXianyuWorkerClient(baseURL, token, 5*time.Second)
			},
		}
	}

	t.Run("renewal success marks cookie valid", func(t *testing.T) {
		svc := newService(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"success":true,"data":{"results":[{"account_id":"838831211","success":true}],"success_count":1,"failed_count":0}}`))
		})
		_, err := svc.RefreshCookie(context.Background(), "838831211")
		require.NoError(t, err)
		require.Equal(t, XianyuCookieStatusValid, account.CookieStatus)
		require.Equal(t, "", account.CookieDetail)
	})

	t.Run("renewal failure marks cookie invalid with detail", func(t *testing.T) {
		svc := newService(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"success":true,"data":{"results":[{"account_id":"838831211","success":false,"message":"续期接口未返回有效Cookie"}],"success_count":0,"failed_count":1}}`))
		})
		_, err := svc.RefreshCookie(context.Background(), "838831211")
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "续期接口未返回有效Cookie"))
		require.Equal(t, XianyuCookieStatusInvalid, account.CookieStatus)
		require.Equal(t, "续期接口未返回有效Cookie", account.CookieDetail)
	})
}
