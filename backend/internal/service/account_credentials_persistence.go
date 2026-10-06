package service

import (
	"context"
	"log/slog"
)

type accountCredentialsUpdater interface {
	UpdateCredentials(ctx context.Context, id int64, credentials map[string]any) error
}

func persistAccountCredentials(ctx context.Context, repo AccountRepository, account *Account, credentials map[string]any) error {
	if repo == nil || account == nil {
		return nil
	}

	// 安全不变量:spark 影子账号恒不持凭据(凭据透传母账号)。这是凭据写入的唯一汇聚点
	// (token 刷新 / 订阅补全 / CRS 创建后刷新等全部经此),在此对影子早返 no-op 是
	// defense-in-depth——即便某条上游路径漏判,也不会把凭据落到影子行(外审第6轮 P1)。
	if account.IsCredentialShadow() {
		slog.Warn("skip persisting credentials to spark shadow account",
			"account_id", account.ID, "parent_id", *account.ParentAccountID)
		return nil
	}

	account.Credentials = shallowCopyMap(credentials)
	if updater, ok := any(repo).(accountCredentialsUpdater); ok {
		return updater.UpdateCredentials(ctx, account.ID, account.Credentials)
	}
	return repo.Update(ctx, account)
}

// shadowAllowedCredentialKeys 是影子账号（spark 与 codebuddy 维度）唯一可写的凭据键集合
// （仅模型映射）。校验(isAllowed)与 sanitize 共用此单一来源,避免两处独立硬编码列表漂移。
// 两种影子都不持 auth token，仅允许在影子侧配置模型映射（model_mapping）。
var shadowAllowedCredentialKeys = map[string]struct{}{
	"model_mapping":         {},
	"compact_model_mapping": {},
}

func isAllowedShadowCredentialsUpdate(credentials map[string]any) bool {
	if credentials == nil {
		return true
	}
	for key := range credentials {
		if _, ok := shadowAllowedCredentialKeys[key]; !ok {
			return false
		}
	}
	return true
}

func sanitizeShadowCredentials(credentials map[string]any) map[string]any {
	if len(credentials) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(shadowAllowedCredentialKeys))
	for key := range shadowAllowedCredentialKeys {
		if value, ok := credentials[key]; ok && value != nil {
			out[key] = value
		}
	}
	return out
}
