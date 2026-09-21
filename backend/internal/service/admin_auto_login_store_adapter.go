package service

import (
	"context"
)

// adminAutoLoginStoreAdapter 将 AdminService 适配为自动登录服务所需的
// AutoLoginAccountStore 接口（ListAccounts / UpdateAccountCredentials /
// UpdateAccountStatus）。与 handler/admin 包的同名适配器语义一致；本 service 层
// 版本供 wire 组装共享的 WebPlatformAutoLoginService 使用（handler 层已改为
// 注入同一实例，避免重复创建）。
type adminAutoLoginStoreAdapter struct {
	svc AdminService
}

var _ AutoLoginAccountStore = (*adminAutoLoginStoreAdapter)(nil)

// NewAdminAutoLoginStoreAdapter 暴露给 wiring 层：将 AdminService 适配为自动登录
// 服务所需的 AutoLoginAccountStore 接口。
func NewAdminAutoLoginStoreAdapter(svc AdminService) AutoLoginAccountStore {
	return &adminAutoLoginStoreAdapter{svc: svc}
}

// ListAccounts 拉取全部账号（忽略分页，自动登录恢复用）。
func (a *adminAutoLoginStoreAdapter) ListAccounts(ctx context.Context) ([]*Account, error) {
	const pageSize = 100000
	accounts, _, err := a.svc.ListAccounts(ctx, 1, pageSize, "", "", "", "", 0, "", "", "")
	if err != nil {
		return nil, err
	}
	out := make([]*Account, 0, len(accounts))
	for i := range accounts {
		out = append(out, &accounts[i])
	}
	return out, nil
}

// UpdateAccountCredentials 将凭据合并写入账号（保留既有 credentials）。
// FromWebLogin 内部标记：本适配器是自动登录服务（登录恢复/续期链）的持久化管道，
// 属 web 登录链内部来源，豁免普通更新入口的 web 凭据旁路拒绝（收敛项 2）。
func (a *adminAutoLoginStoreAdapter) UpdateAccountCredentials(ctx context.Context, id int64, creds map[string]any) error {
	account, err := a.svc.GetAccount(ctx, id)
	if err != nil {
		return err
	}
	merged := make(map[string]any, len(account.Credentials)+len(creds))
	for k, v := range account.Credentials {
		merged[k] = v
	}
	for k, v := range creds {
		merged[k] = v
	}
	_, err = a.svc.UpdateAccount(ctx, id, &UpdateAccountInput{Credentials: merged, FromWebLogin: true})
	return err
}

// UpdateAccountStatus 设置账号状态；errMsg 非空则写入错误，否则在置为 active 时清错。
func (a *adminAutoLoginStoreAdapter) UpdateAccountStatus(ctx context.Context, id int64, status string, errMsg string) error {
	if _, err := a.svc.UpdateAccount(ctx, id, &UpdateAccountInput{Status: status}); err != nil {
		return err
	}
	if errMsg != "" {
		return a.svc.SetAccountError(ctx, id, errMsg)
	}
	if status == StatusActive {
		if _, err := a.svc.ClearAccountError(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
