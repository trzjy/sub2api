<!-- 由 scripts/gen-route-atlas.py 自动生成，请勿手改。 -->

# 管理面（/api/v1/admin）路由索引

重新生成：`python3 skills/sub2api-api-atlas/scripts/gen-route-atlas.py`

# 路由索引：admin.go

- 鉴权域：adminAuth（管理员 API Key 或管理员 JWT）+ 审计 + 合规守卫
- 端点数：542
- 源码：`backend/internal/server/routes/admin.go`

路径列里的 `:id` / `*subpath` 是 gin 路径参数；handler 列给出方法符号，
需要请求体结构时按「位置」列的 文件:行 跳转，读该 handler 里绑定的 Request struct。

## /admin/accounts（88）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/accounts` | `h.Admin.Account.List` | `admin.go:593` |
| POST | `/api/v1/admin/accounts` | `h.Admin.Account.Create` | `admin.go:606` |
| DELETE | `/api/v1/admin/accounts/:id` | `h.Admin.Account.Delete` | `admin.go:625` |
| GET | `/api/v1/admin/accounts/:id` | `h.Admin.Account.GetByID` | `admin.go:602` |
| PUT | `/api/v1/admin/accounts/:id` | `h.Admin.Account.Update` | `admin.go:612` |
| POST | `/api/v1/admin/accounts/:id/apply-oauth-credentials` | `h.Admin.Account.ApplyOAuthCredentials` | `admin.go:630` |
| GET | `/api/v1/admin/accounts/:id/claude/reset-credits` | `h.Admin.Account.ClaudeResetCredits` | `admin.go:603` |
| POST | `/api/v1/admin/accounts/:id/claude/reset-credits/redeem` | `h.Admin.Account.RedeemClaudeResetCredit` | `admin.go:605` |
| POST | `/api/v1/admin/accounts/:id/clear-error` | `h.Admin.Account.ClearError` | `admin.go:634` |
| POST | `/api/v1/admin/accounts/:id/clear-rate-limit` | `h.Admin.Account.ClearRateLimit` | `admin.go:640` |
| POST | `/api/v1/admin/accounts/:id/duplicate` | `h.Admin.Account.Duplicate` | `admin.go:607` |
| GET | `/api/v1/admin/accounts/:id/freshness` | `h.Admin.Account.GetAccountFreshness` | `admin.go:644` |
| GET | `/api/v1/admin/accounts/:id/grok-media-eligibility` | `h.Admin.Account.GetGrokMediaEligibility` | `admin.go:613` |
| PUT | `/api/v1/admin/accounts/:id/grok-media-eligibility` | `h.Admin.Account.UpdateGrokMediaEligibility` | `admin.go:614` |
| GET | `/api/v1/admin/accounts/:id/models` | `h.Admin.Account.GetAvailableModels` | `admin.go:648` |
| POST | `/api/v1/admin/accounts/:id/models/sync-upstream` | `h.Admin.Account.SyncUpstreamModels` | `admin.go:649` |
| POST | `/api/v1/admin/accounts/:id/models/sync-volcano-plan` | `h.Admin.Account.SyncVolcanoPlanModels` | `admin.go:650` |
| GET | `/api/v1/admin/accounts/:id/ollama-cloud-usage` | `h.Admin.Account.GetOllamaCloudUsage` | `admin.go:617` |
| PUT | `/api/v1/admin/accounts/:id/ollama-cloud-usage/auto-refresh` | `h.Admin.Account.SetOllamaCloudUsageAutoRefresh` | `admin.go:620` |
| POST | `/api/v1/admin/accounts/:id/ollama-cloud-usage/refresh` | `h.Admin.Account.RefreshOllamaCloudUsage` | `admin.go:621` |
| DELETE | `/api/v1/admin/accounts/:id/ollama-cloud-usage/session` | `h.Admin.Account.DeleteOllamaCloudUsageSession` | `admin.go:619` |
| PUT | `/api/v1/admin/accounts/:id/ollama-cloud-usage/session` | `h.Admin.Account.SaveOllamaCloudUsageSession` | `admin.go:618` |
| GET | `/api/v1/admin/accounts/:id/opencode-go-usage` | `h.Admin.Account.GetOpenCodeGoUsage` | `admin.go:622` |
| PUT | `/api/v1/admin/accounts/:id/opencode-go-usage/auto-refresh` | `h.Admin.Account.SetOpenCodeGoUsageAutoRefresh` | `admin.go:623` |
| POST | `/api/v1/admin/accounts/:id/opencode-go-usage/refresh` | `h.Admin.Account.RefreshOpenCodeGoUsage` | `admin.go:624` |
| POST | `/api/v1/admin/accounts/:id/recover-state` | `h.Admin.Account.RecoverState` | `admin.go:628` |
| POST | `/api/v1/admin/accounts/:id/refresh` | `h.Admin.Account.Refresh` | `admin.go:629` |
| POST | `/api/v1/admin/accounts/:id/refresh-tier` | `h.Admin.Account.RefreshTier` | `admin.go:632` |
| POST | `/api/v1/admin/accounts/:id/reset-quota` | `h.Admin.Account.ResetQuota` | `admin.go:641` |
| POST | `/api/v1/admin/accounts/:id/revert-proxy-fallback` | `h.Admin.Account.RevertProxyFallback` | `admin.go:635` |
| POST | `/api/v1/admin/accounts/:id/schedulable` | `h.Admin.Account.SetSchedulable` | `admin.go:646` |
| GET | `/api/v1/admin/accounts/:id/scheduled-test-plans` | `h.Admin.ScheduledTest.ListByAccount` | `admin.go:1009` |
| POST | `/api/v1/admin/accounts/:id/set-privacy` | `h.Admin.Account.SetPrivacy` | `admin.go:631` |
| POST | `/api/v1/admin/accounts/:id/shadow` | `h.Admin.OpenAIOAuth.CreateShadow` | `admin.go:684` |
| GET | `/api/v1/admin/accounts/:id/shadows` | `h.Admin.Account.ListShadows` | `admin.go:686` |
| GET | `/api/v1/admin/accounts/:id/stats` | `h.Admin.Account.GetStats` | `admin.go:633` |
| DELETE | `/api/v1/admin/accounts/:id/temp-unschedulable` | `h.Admin.Account.ClearTempUnschedulable` | `admin.go:645` |
| GET | `/api/v1/admin/accounts/:id/temp-unschedulable` | `h.Admin.Account.GetTempUnschedulable` | `admin.go:642` |
| POST | `/api/v1/admin/accounts/:id/test` | `h.Admin.Account.Test` | `admin.go:626` |
| GET | `/api/v1/admin/accounts/:id/today-stats` | `h.Admin.Account.GetTodayStats` | `admin.go:637` |
| POST | `/api/v1/admin/accounts/:id/upstream-billing-probe` | `h.Admin.Account.ProbeUpstreamBilling` | `admin.go:616` |
| PUT | `/api/v1/admin/accounts/:id/upstream-billing-probe` | `h.Admin.Account.SetUpstreamBillingProbeEnabled` | `admin.go:615` |
| GET | `/api/v1/admin/accounts/:id/usage` | `h.Admin.Account.GetUsage` | `admin.go:636` |
| PUT | `/api/v1/admin/accounts/:id/vision-capability` | `h.Admin.VisionCapability.Override` | `admin.go:668` |
| POST | `/api/v1/admin/accounts/:id/vision-capability/detect` | `h.Admin.VisionCapability.Detect` | `admin.go:666` |
| GET | `/api/v1/admin/accounts/antigravity/default-model-mapping` | `h.Admin.Account.GetAntigravityDefaultModelMapping` | `admin.go:681` |
| POST | `/api/v1/admin/accounts/batch` | `h.Admin.Account.BatchCreate` | `admin.go:651` |
| POST | `/api/v1/admin/accounts/batch-clear-error` | `h.Admin.Account.BatchClearError` | `admin.go:662` |
| POST | `/api/v1/admin/accounts/batch-delete` | `h.Admin.Account.BatchDelete` | `admin.go:661` |
| POST | `/api/v1/admin/accounts/batch-refresh` | `h.Admin.Account.BatchRefresh` | `admin.go:663` |
| POST | `/api/v1/admin/accounts/batch-refresh-tier` | `h.Admin.Account.BatchRefreshTier` | `admin.go:659` |
| POST | `/api/v1/admin/accounts/batch-update-credentials` | `h.Admin.Account.BatchUpdateCredentials` | `admin.go:658` |
| POST | `/api/v1/admin/accounts/bulk-update` | `h.Admin.Account.BulkUpdate` | `admin.go:660` |
| POST | `/api/v1/admin/accounts/check-mixed-channel` | `h.Admin.Account.CheckMixedChannel` | `admin.go:608` |
| POST | `/api/v1/admin/accounts/cockpit-import/commit` | `h.Admin.CockpitImport.CommitCockpitImport` | `admin.go:55` |
| POST | `/api/v1/admin/accounts/cockpit-import/preview` | `h.Admin.CockpitImport.PreviewCockpitImport` | `admin.go:58` |
| POST | `/api/v1/admin/accounts/cookie-auth` | `h.Admin.OAuth.CookieAuth` | `admin.go:693` |
| GET | `/api/v1/admin/accounts/data` | `h.Admin.Account.ExportData` | `admin.go:656` |
| POST | `/api/v1/admin/accounts/data` | `h.Admin.Account.ImportData` | `admin.go:657` |
| POST | `/api/v1/admin/accounts/exchange-code` | `h.Admin.OAuth.ExchangeCode` | `admin.go:691` |
| POST | `/api/v1/admin/accounts/exchange-setup-token-code` | `h.Admin.OAuth.ExchangeSetupTokenCode` | `admin.go:692` |
| POST | `/api/v1/admin/accounts/generate-auth-url` | `h.Admin.OAuth.GenerateAuthURL` | `admin.go:689` |
| POST | `/api/v1/admin/accounts/generate-setup-token-url` | `h.Admin.OAuth.GenerateSetupTokenURL` | `admin.go:690` |
| POST | `/api/v1/admin/accounts/import/codex-session` | `h.Admin.Account.ImportCodexSession` | `admin.go:609` |
| POST | `/api/v1/admin/accounts/models/sync-upstream-preview` | `h.Admin.Account.SyncUpstreamModelsPreview` | `admin.go:647` |
| GET | `/api/v1/admin/accounts/ollama-cloud-usage/settings` | `h.Admin.Account.GetOllamaCloudUsageSettings` | `admin.go:598` |
| PUT | `/api/v1/admin/accounts/ollama-cloud-usage/settings` | `h.Admin.Account.UpdateOllamaCloudUsageSettings` | `admin.go:599` |
| GET | `/api/v1/admin/accounts/opencode-go-usage/settings` | `h.Admin.Account.GetOpenCodeGoUsageSettings` | `admin.go:600` |
| PUT | `/api/v1/admin/accounts/opencode-go-usage/settings` | `h.Admin.Account.UpdateOpenCodeGoUsageSettings` | `admin.go:601` |
| POST | `/api/v1/admin/accounts/platform-migration` | `h.Admin.Account.RunPlatformMigration` | `admin.go:654` |
| POST | `/api/v1/admin/accounts/setup-token-cookie-auth` | `h.Admin.OAuth.SetupTokenCookieAuth` | `admin.go:694` |
| POST | `/api/v1/admin/accounts/sync/crs` | `h.Admin.Account.SyncFromCRS` | `admin.go:610` |
| POST | `/api/v1/admin/accounts/sync/crs/preview` | `h.Admin.Account.PreviewFromCRS` | `admin.go:611` |
| POST | `/api/v1/admin/accounts/today-stats/batch` | `h.Admin.Account.GetBatchTodayStats` | `admin.go:639` |
| POST | `/api/v1/admin/accounts/upstream-billing-probe/batch` | `h.Admin.Account.ProbeUpstreamBillingBatch` | `admin.go:597` |
| GET | `/api/v1/admin/accounts/upstream-billing-probe/settings` | `h.Admin.Account.GetUpstreamBillingProbeSettings` | `admin.go:595` |
| PUT | `/api/v1/admin/accounts/upstream-billing-probe/settings` | `h.Admin.Account.UpdateUpstreamBillingProbeSettings` | `admin.go:596` |
| GET | `/api/v1/admin/accounts/upstream-billing-rates` | `h.Admin.Account.GetUpstreamBillingRates` | `admin.go:594` |
| POST | `/api/v1/admin/accounts/usage/batch` | `h.Admin.Account.GetBatchUsage` | `admin.go:638` |
| POST | `/api/v1/admin/accounts/validate-web-credentials` | `h.Admin.Account.ValidateWebCredentials` | `admin.go:627` |
| POST | `/api/v1/admin/accounts/vision-capability/detect` | `h.Admin.VisionCapability.DetectBatch` | `admin.go:667` |
| POST | `/api/v1/admin/accounts/web-login-challenge/:session_id/consume` | `h.Admin.Account.WebLoginChallengeConsume` | `admin.go:678` |
| GET | `/api/v1/admin/accounts/web-login-challenge/:session_id/status` | `h.Admin.Account.WebLoginChallengeStatus` | `admin.go:677` |
| POST | `/api/v1/admin/accounts/web-login-challenge/start` | `h.Admin.Account.WebLoginChallengeStart` | `admin.go:676` |
| POST | `/api/v1/admin/accounts/web-login-password` | `h.Admin.Account.WebLoginPassword` | `admin.go:671` |
| POST | `/api/v1/admin/accounts/web-login-sms` | `h.Admin.Account.WebLoginSMS` | `admin.go:672` |
| POST | `/api/v1/admin/accounts/web-register` | `h.Admin.Account.WebRegister` | `admin.go:675` |
| POST | `/api/v1/admin/accounts/web-register-email-code` | `h.Admin.Account.WebRegisterEmailCode` | `admin.go:674` |

## /admin/affiliates（10）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/affiliates/invites` | `h.Admin.Affiliate.ListInviteRecords` | `admin.go:1145` |
| GET | `/api/v1/admin/affiliates/rebates` | `h.Admin.Affiliate.ListRebateRecords` | `admin.go:1146` |
| GET | `/api/v1/admin/affiliates/transfers` | `h.Admin.Affiliate.ListTransferRecords` | `admin.go:1147` |
| GET | `/api/v1/admin/affiliates/users` | `h.Admin.Affiliate.ListUsers` | `admin.go:1151` |
| DELETE | `/api/v1/admin/affiliates/users/:user_id` | `h.Admin.Affiliate.ClearUserSettings` | `admin.go:1157` |
| PUT | `/api/v1/admin/affiliates/users/:user_id` | `h.Admin.Affiliate.UpdateUserSettings` | `admin.go:1156` |
| GET | `/api/v1/admin/affiliates/users/:user_id/overview` | `h.Admin.Affiliate.GetUserOverview` | `admin.go:1154` |
| POST | `/api/v1/admin/affiliates/users/:user_id/withdraw` | `h.Admin.Affiliate.WithdrawQuota` | `admin.go:1155` |
| POST | `/api/v1/admin/affiliates/users/batch-rate` | `h.Admin.Affiliate.BatchSetRate` | `admin.go:1153` |
| GET | `/api/v1/admin/affiliates/users/lookup` | `h.Admin.Affiliate.LookupUsers` | `admin.go:1152` |

## /admin/announcements（7）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/announcements` | `h.Admin.Announcement.List` | `admin.go:701` |
| POST | `/api/v1/admin/announcements` | `h.Admin.Announcement.Create` | `admin.go:702` |
| DELETE | `/api/v1/admin/announcements/:id` | `h.Admin.Announcement.Delete` | `admin.go:705` |
| GET | `/api/v1/admin/announcements/:id` | `h.Admin.Announcement.GetByID` | `admin.go:703` |
| PUT | `/api/v1/admin/announcements/:id` | `h.Admin.Announcement.Update` | `admin.go:704` |
| GET | `/api/v1/admin/announcements/:id/read-status` | `h.Admin.Announcement.ListReadStatus` | `admin.go:707` |
| POST | `/api/v1/admin/announcements/:id/upload-image` | `h.Admin.Announcement.UploadImage` | `admin.go:706` |

## /admin/antigravity（3）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/api/v1/admin/antigravity/oauth/auth-url` | `h.Admin.AntigravityOAuth.GenerateAuthURL` | `admin.go:740` |
| POST | `/api/v1/admin/antigravity/oauth/exchange-code` | `h.Admin.AntigravityOAuth.ExchangeCode` | `admin.go:741` |
| POST | `/api/v1/admin/antigravity/oauth/refresh-token` | `h.Admin.AntigravityOAuth.RefreshToken` | `admin.go:742` |

## /admin/api-keys（1）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| PUT | `/api/v1/admin/api-keys/:id` | `h.Admin.APIKey.UpdateGroup` | `admin.go:420` |

## /admin/audit-logs（3）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/audit-logs` | `h.Admin.AuditLog.List` | `admin.go:353` |
| GET | `/api/v1/admin/audit-logs/:id` | `h.Admin.AuditLog.Get` | `admin.go:354` |
| POST | `/api/v1/admin/audit-logs/clear` | `h.Admin.AuditLog.Clear` | `admin.go:356` |

## /admin/backups（14）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/backups` | `h.Admin.Backup.ListBackups` | `admin.go:929` |
| POST | `/api/v1/admin/backups` | `h.Admin.Backup.CreateBackup` | `admin.go:928` |
| DELETE | `/api/v1/admin/backups/:id` | `h.Admin.Backup.DeleteBackup` | `admin.go:931` |
| GET | `/api/v1/admin/backups/:id` | `h.Admin.Backup.GetBackup` | `admin.go:930` |
| GET | `/api/v1/admin/backups/:id/download-url` | `h.Admin.Backup.GetDownloadURL` | `admin.go:933` |
| POST | `/api/v1/admin/backups/:id/restore` | `h.Admin.Backup.RestoreBackup` | `admin.go:936` |
| GET | `/api/v1/admin/backups/image-storage` | `h.Admin.Backup.GetImageStorageConfig` | `admin.go:918` |
| PUT | `/api/v1/admin/backups/image-storage` | `h.Admin.Backup.UpdateImageStorageConfig` | `admin.go:920` |
| POST | `/api/v1/admin/backups/image-storage/test` | `h.Admin.Backup.TestImageStorageConnection` | `admin.go:921` |
| GET | `/api/v1/admin/backups/s3-config` | `h.Admin.Backup.GetS3Config` | `admin.go:912` |
| PUT | `/api/v1/admin/backups/s3-config` | `h.Admin.Backup.UpdateS3Config` | `admin.go:914` |
| POST | `/api/v1/admin/backups/s3-config/test` | `h.Admin.Backup.TestS3Connection` | `admin.go:915` |
| GET | `/api/v1/admin/backups/schedule` | `h.Admin.Backup.GetSchedule` | `admin.go:924` |
| PUT | `/api/v1/admin/backups/schedule` | `h.Admin.Backup.UpdateSchedule` | `admin.go:925` |

## /admin/channel-monitor-templates（7）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/channel-monitor-templates` | `h.Admin.ChannelMonitorTemplate.List` | `admin.go:1131` |
| POST | `/api/v1/admin/channel-monitor-templates` | `h.Admin.ChannelMonitorTemplate.Create` | `admin.go:1132` |
| DELETE | `/api/v1/admin/channel-monitor-templates/:id` | `h.Admin.ChannelMonitorTemplate.Delete` | `admin.go:1135` |
| GET | `/api/v1/admin/channel-monitor-templates/:id` | `h.Admin.ChannelMonitorTemplate.Get` | `admin.go:1133` |
| PUT | `/api/v1/admin/channel-monitor-templates/:id` | `h.Admin.ChannelMonitorTemplate.Update` | `admin.go:1134` |
| POST | `/api/v1/admin/channel-monitor-templates/:id/apply` | `h.Admin.ChannelMonitorTemplate.Apply` | `admin.go:1137` |
| GET | `/api/v1/admin/channel-monitor-templates/:id/monitors` | `h.Admin.ChannelMonitorTemplate.AssociatedMonitors` | `admin.go:1136` |

## /admin/channel-monitor-v2（8）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/channel-monitor-v2/config` | `h.ChannelMonitorV2.GetConfig` | `admin.go:1173` |
| PUT | `/api/v1/admin/channel-monitor-v2/config` | `h.ChannelMonitorV2.UpdateConfig` | `admin.go:1174` |
| GET | `/api/v1/admin/channel-monitor-v2/dimensions` | `h.ChannelMonitorV2.Dimensions` | `admin.go:1179` |
| GET | `/api/v1/admin/channel-monitor-v2/errors` | `h.ChannelMonitorV2.Errors` | `admin.go:1183` |
| GET | `/api/v1/admin/channel-monitor-v2/matrix` | `h.ChannelMonitorV2.AdminMatrix` | `admin.go:1182` |
| GET | `/api/v1/admin/channel-monitor-v2/models` | `h.ChannelMonitorV2.AdminModels` | `admin.go:1181` |
| GET | `/api/v1/admin/channel-monitor-v2/snapshot` | `h.ChannelMonitorV2.AdminSnapshot` | `admin.go:1180` |
| GET | `/api/v1/admin/channel-monitor-v2/users` | `h.ChannelMonitorV2.AdminUsers` | `admin.go:1184` |

## /admin/channel-monitors（8）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/channel-monitors` | `h.Admin.ChannelMonitor.List` | `admin.go:1118` |
| POST | `/api/v1/admin/channel-monitors` | `h.Admin.ChannelMonitor.Create` | `admin.go:1119` |
| DELETE | `/api/v1/admin/channel-monitors/:id` | `h.Admin.ChannelMonitor.Delete` | `admin.go:1123` |
| GET | `/api/v1/admin/channel-monitors/:id` | `h.Admin.ChannelMonitor.Get` | `admin.go:1120` |
| PUT | `/api/v1/admin/channel-monitors/:id` | `h.Admin.ChannelMonitor.Update` | `admin.go:1122` |
| POST | `/api/v1/admin/channel-monitors/:id/duplicate` | `h.Admin.ChannelMonitor.Duplicate` | `admin.go:1121` |
| GET | `/api/v1/admin/channel-monitors/:id/history` | `h.Admin.ChannelMonitor.History` | `admin.go:1125` |
| POST | `/api/v1/admin/channel-monitors/:id/run` | `h.Admin.ChannelMonitor.Run` | `admin.go:1124` |

## /admin/channels（7）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/channels` | `h.Admin.Channel.List` | `admin.go:1054` |
| POST | `/api/v1/admin/channels` | `h.Admin.Channel.Create` | `admin.go:1058` |
| DELETE | `/api/v1/admin/channels/:id` | `h.Admin.Channel.Delete` | `admin.go:1060` |
| GET | `/api/v1/admin/channels/:id` | `h.Admin.Channel.GetByID` | `admin.go:1057` |
| PUT | `/api/v1/admin/channels/:id` | `h.Admin.Channel.Update` | `admin.go:1059` |
| GET | `/api/v1/admin/channels/model-pricing` | `h.Admin.Channel.GetModelDefaultPricing` | `admin.go:1055` |
| GET | `/api/v1/admin/channels/pricing/sync-models` | `h.Admin.Channel.SyncPricingModels` | `admin.go:1056` |

## /admin/cn-providers（2）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/cn-providers/accounts/:id/balance` | `h.Admin.CNProvider.QueryBalance` | `admin.go:782` |
| GET | `/api/v1/admin/cn-providers/accounts/:id/quota` | `h.Admin.CNProvider.QueryQuota` | `admin.go:780` |

## /admin/codebuddy（3）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/api/v1/admin/codebuddy/oauth/auth-url` | `h.Admin.CodeBuddyOAuth.GenerateAuthURL` | `admin.go:750` |
| POST | `/api/v1/admin/codebuddy/oauth/poll` | `h.Admin.CodeBuddyOAuth.PollToken` | `admin.go:751` |
| POST | `/api/v1/admin/codebuddy/oauth/refresh-token` | `h.Admin.CodeBuddyOAuth.RefreshToken` | `admin.go:752` |

## /admin/compliance（2）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/compliance` | `h.Admin.Compliance.GetStatus` | `admin.go:398` |
| POST | `/api/v1/admin/compliance/accept` | `h.Admin.Compliance.Accept` | `admin.go:399` |

## /admin/dashboard（13）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/api/v1/admin/dashboard/aggregation/backfill` | `h.Admin.Dashboard.BackfillAggregation` | `admin.go:529` |
| GET | `/api/v1/admin/dashboard/api-keys-trend` | `h.Admin.Dashboard.GetAPIKeyUsageTrend` | `admin.go:523` |
| POST | `/api/v1/admin/dashboard/api-keys-usage` | `h.Admin.Dashboard.GetBatchAPIKeysUsage` | `admin.go:527` |
| GET | `/api/v1/admin/dashboard/groups` | `h.Admin.Dashboard.GetGroupStats` | `admin.go:522` |
| GET | `/api/v1/admin/dashboard/models` | `h.Admin.Dashboard.GetModelStats` | `admin.go:521` |
| GET | `/api/v1/admin/dashboard/realtime` | `h.Admin.Dashboard.GetRealtimeMetrics` | `admin.go:519` |
| GET | `/api/v1/admin/dashboard/snapshot-v2` | `h.Admin.Dashboard.GetSnapshotV2` | `admin.go:517` |
| GET | `/api/v1/admin/dashboard/stats` | `h.Admin.Dashboard.GetStats` | `admin.go:518` |
| GET | `/api/v1/admin/dashboard/trend` | `h.Admin.Dashboard.GetUsageTrend` | `admin.go:520` |
| GET | `/api/v1/admin/dashboard/user-breakdown` | `h.Admin.Dashboard.GetUserBreakdown` | `admin.go:528` |
| GET | `/api/v1/admin/dashboard/users-ranking` | `h.Admin.Dashboard.GetUserSpendingRanking` | `admin.go:525` |
| GET | `/api/v1/admin/dashboard/users-trend` | `h.Admin.Dashboard.GetUserUsageTrend` | `admin.go:524` |
| POST | `/api/v1/admin/dashboard/users-usage` | `h.Admin.Dashboard.GetBatchUsersUsage` | `admin.go:526` |

## /admin/data-management（17）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/data-management/agent/health` | `h.Admin.DataManagement.GetAgentHealth` | `admin.go:887` |
| GET | `/api/v1/admin/data-management/backups` | `h.Admin.DataManagement.ListBackupJobs` | `admin.go:903` |
| POST | `/api/v1/admin/data-management/backups` | `h.Admin.DataManagement.CreateBackupJob` | `admin.go:902` |
| GET | `/api/v1/admin/data-management/backups/:job_id` | `h.Admin.DataManagement.GetBackupJob` | `admin.go:904` |
| GET | `/api/v1/admin/data-management/config` | `h.Admin.DataManagement.GetConfig` | `admin.go:888` |
| PUT | `/api/v1/admin/data-management/config` | `h.Admin.DataManagement.UpdateConfig` | `admin.go:889` |
| GET | `/api/v1/admin/data-management/s3/profiles` | `h.Admin.DataManagement.ListS3Profiles` | `admin.go:896` |
| POST | `/api/v1/admin/data-management/s3/profiles` | `h.Admin.DataManagement.CreateS3Profile` | `admin.go:898` |
| DELETE | `/api/v1/admin/data-management/s3/profiles/:profile_id` | `h.Admin.DataManagement.DeleteS3Profile` | `admin.go:900` |
| PUT | `/api/v1/admin/data-management/s3/profiles/:profile_id` | `h.Admin.DataManagement.UpdateS3Profile` | `admin.go:899` |
| POST | `/api/v1/admin/data-management/s3/profiles/:profile_id/activate` | `h.Admin.DataManagement.SetActiveS3Profile` | `admin.go:901` |
| POST | `/api/v1/admin/data-management/s3/test` | `h.Admin.DataManagement.TestS3` | `admin.go:895` |
| GET | `/api/v1/admin/data-management/sources/:source_type/profiles` | `h.Admin.DataManagement.ListSourceProfiles` | `admin.go:890` |
| POST | `/api/v1/admin/data-management/sources/:source_type/profiles` | `h.Admin.DataManagement.CreateSourceProfile` | `admin.go:891` |
| DELETE | `/api/v1/admin/data-management/sources/:source_type/profiles/:profile_id` | `h.Admin.DataManagement.DeleteSourceProfile` | `admin.go:893` |
| PUT | `/api/v1/admin/data-management/sources/:source_type/profiles/:profile_id` | `h.Admin.DataManagement.UpdateSourceProfile` | `admin.go:892` |
| POST | `/api/v1/admin/data-management/sources/:source_type/profiles/:profile_id/activate` | `h.Admin.DataManagement.SetActiveSourceProfile` | `admin.go:894` |

## /admin/error-passthrough-rules（5）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/error-passthrough-rules` | `h.Admin.ErrorPassthrough.List` | `admin.go:1015` |
| POST | `/api/v1/admin/error-passthrough-rules` | `h.Admin.ErrorPassthrough.Create` | `admin.go:1017` |
| DELETE | `/api/v1/admin/error-passthrough-rules/:id` | `h.Admin.ErrorPassthrough.Delete` | `admin.go:1019` |
| GET | `/api/v1/admin/error-passthrough-rules/:id` | `h.Admin.ErrorPassthrough.GetByID` | `admin.go:1016` |
| PUT | `/api/v1/admin/error-passthrough-rules/:id` | `h.Admin.ErrorPassthrough.Update` | `admin.go:1018` |

## /admin/gemini（3）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/api/v1/admin/gemini/oauth/auth-url` | `h.Admin.GeminiOAuth.GenerateAuthURL` | `admin.go:731` |
| GET | `/api/v1/admin/gemini/oauth/capabilities` | `h.Admin.GeminiOAuth.GetCapabilities` | `admin.go:733` |
| POST | `/api/v1/admin/gemini/oauth/exchange-code` | `h.Admin.GeminiOAuth.ExchangeCode` | `admin.go:732` |

## /admin/grok（13）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/grok/accounts/:id/quota` | `h.Admin.GrokOAuth.QueryQuota` | `admin.go:769` |
| POST | `/api/v1/admin/grok/accounts/:id/refresh` | `h.Admin.GrokOAuth.RefreshAccountToken` | `admin.go:768` |
| POST | `/api/v1/admin/grok/accounts/:id/reset-quota` | `h.Admin.GrokOAuth.ResetQuota` | `admin.go:770` |
| POST | `/api/v1/admin/grok/oauth/auth-url` | `h.Admin.GrokOAuth.GenerateAuthURL` | `admin.go:760` |
| GET | `/api/v1/admin/grok/oauth/capabilities` | `h.Admin.GrokOAuth.GetCapabilities` | `admin.go:759` |
| POST | `/api/v1/admin/grok/oauth/create-from-oauth` | `h.Admin.GrokOAuth.CreateAccountFromOAuth` | `admin.go:765` |
| POST | `/api/v1/admin/grok/oauth/exchange-code` | `h.Admin.GrokOAuth.ExchangeCode` | `admin.go:761` |
| POST | `/api/v1/admin/grok/oauth/password` | `h.Admin.GrokOAuth.AuthorizePassword` | `admin.go:764` |
| POST | `/api/v1/admin/grok/oauth/reconcile` | `h.Admin.GrokOAuth.ReconcileOAuthAccounts` | `admin.go:767` |
| POST | `/api/v1/admin/grok/oauth/refresh-token` | `h.Admin.GrokOAuth.RefreshToken` | `admin.go:762` |
| POST | `/api/v1/admin/grok/oauth/sso-token` | `h.Admin.GrokOAuth.ValidateSSOToken` | `admin.go:763` |
| GET | `/api/v1/admin/grok/runtime-sanity` | `h.Admin.GrokOAuth.RuntimeSanity` | `admin.go:771` |
| POST | `/api/v1/admin/grok/sso-to-oauth` | `h.Admin.GrokOAuth.CreateAccountsFromSSO` | `admin.go:766` |

## /admin/groups（25）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/groups` | `h.Admin.Group.List` | `admin.go:563` |
| POST | `/api/v1/admin/groups` | `h.Admin.Group.Create` | `admin.go:576` |
| DELETE | `/api/v1/admin/groups/:id` | `h.Admin.Group.Delete` | `admin.go:579` |
| GET | `/api/v1/admin/groups/:id` | `h.Admin.Group.GetByID` | `admin.go:575` |
| PUT | `/api/v1/admin/groups/:id` | `h.Admin.Group.Update` | `admin.go:578` |
| GET | `/api/v1/admin/groups/:id/api-keys` | `h.Admin.Group.GetGroupAPIKeys` | `admin.go:586` |
| GET | `/api/v1/admin/groups/:id/composite-routes` | `h.Admin.Group.ListCompositeRoutes` | `admin.go:570` |
| POST | `/api/v1/admin/groups/:id/composite-routes` | `h.Admin.Group.CreateCompositeRoute` | `admin.go:571` |
| DELETE | `/api/v1/admin/groups/:id/composite-routes/:route_id` | `h.Admin.Group.DeleteCompositeRoute` | `admin.go:574` |
| PUT | `/api/v1/admin/groups/:id/composite-routes/:route_id` | `h.Admin.Group.UpdateCompositeRoute` | `admin.go:573` |
| POST | `/api/v1/admin/groups/:id/composite-routes/preview` | `h.Admin.Group.PreviewCompositeRoute` | `admin.go:572` |
| POST | `/api/v1/admin/groups/:id/duplicate` | `h.Admin.Group.Duplicate` | `admin.go:577` |
| GET | `/api/v1/admin/groups/:id/model-allowlist-candidates` | `h.Admin.Group.GetGroupModelAllowlistCandidates` | `admin.go:569` |
| DELETE | `/api/v1/admin/groups/:id/rate-multipliers` | `h.Admin.Group.ClearGroupRateMultipliers` | `admin.go:583` |
| GET | `/api/v1/admin/groups/:id/rate-multipliers` | `h.Admin.Group.GetGroupRateMultipliers` | `admin.go:581` |
| PUT | `/api/v1/admin/groups/:id/rate-multipliers` | `h.Admin.Group.BatchSetGroupRateMultipliers` | `admin.go:582` |
| DELETE | `/api/v1/admin/groups/:id/rpm-overrides` | `h.Admin.Group.ClearGroupRPMOverrides` | `admin.go:585` |
| PUT | `/api/v1/admin/groups/:id/rpm-overrides` | `h.Admin.Group.BatchSetGroupRPMOverrides` | `admin.go:584` |
| GET | `/api/v1/admin/groups/:id/stats` | `h.Admin.Group.GetStats` | `admin.go:580` |
| GET | `/api/v1/admin/groups/:id/subscriptions` | `h.Admin.Subscription.ListByGroup` | `admin.go:969` |
| GET | `/api/v1/admin/groups/all` | `h.Admin.Group.GetAll` | `admin.go:564` |
| GET | `/api/v1/admin/groups/capacity-summary` | `h.Admin.Group.GetCapacitySummary` | `admin.go:566` |
| GET | `/api/v1/admin/groups/live-capability` | `h.Admin.Group.GetLiveCapability` | `admin.go:567` |
| PUT | `/api/v1/admin/groups/sort-order` | `h.Admin.Group.UpdateSortOrder` | `admin.go:568` |
| GET | `/api/v1/admin/groups/usage-summary` | `h.Admin.Group.GetUsageSummary` | `admin.go:565` |

## /admin/model-plaza（2）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/model-plaza/official-pricing` | `h.ModelPlaza.GetOfficialPricingOverrides` | `admin.go:1230` |
| PUT | `/api/v1/admin/model-plaza/official-pricing` | `h.ModelPlaza.SaveOfficialPricingOverrides` | `admin.go:1231` |

## /admin/openai（11）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/openai/accounts/:id/quota` | `h.Admin.OpenAIOAuth.QueryQuota` | `admin.go:720` |
| POST | `/api/v1/admin/openai/accounts/:id/quota/refresh` | `h.Admin.OpenAIOAuth.RefreshQuota` | `admin.go:721` |
| POST | `/api/v1/admin/openai/accounts/:id/referrals/invite` | `h.Admin.OpenAIOAuth.SendReferralInvite` | `admin.go:724` |
| POST | `/api/v1/admin/openai/accounts/:id/referrals/refresh` | `h.Admin.OpenAIOAuth.RefreshReferrals` | `admin.go:723` |
| POST | `/api/v1/admin/openai/accounts/:id/refresh` | `h.Admin.OpenAIOAuth.RefreshAccountToken` | `admin.go:717` |
| POST | `/api/v1/admin/openai/accounts/:id/reset-quota` | `h.Admin.OpenAIOAuth.ResetQuota` | `admin.go:722` |
| POST | `/api/v1/admin/openai/create-from-codex-pat` | `h.Admin.OpenAIOAuth.CreateAccountFromCodexPAT` | `admin.go:719` |
| POST | `/api/v1/admin/openai/create-from-oauth` | `h.Admin.OpenAIOAuth.CreateAccountFromOAuth` | `admin.go:718` |
| POST | `/api/v1/admin/openai/exchange-code` | `h.Admin.OpenAIOAuth.ExchangeCode` | `admin.go:715` |
| POST | `/api/v1/admin/openai/generate-auth-url` | `h.Admin.OpenAIOAuth.GenerateAuthURL` | `admin.go:714` |
| POST | `/api/v1/admin/openai/refresh-token` | `h.Admin.OpenAIOAuth.RefreshToken` | `admin.go:716` |

## /admin/ops（48）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/ops/account-availability` | `h.Admin.Ops.GetAccountAvailability` | `admin.go:430` |
| GET | `/api/v1/admin/ops/advanced-settings` | `h.Admin.Ops.GetAdvancedSettings` | `admin.go:458` |
| PUT | `/api/v1/admin/ops/advanced-settings` | `h.Admin.Ops.UpdateAdvancedSettings` | `admin.go:459` |
| GET | `/api/v1/admin/ops/alert-events` | `h.Admin.Ops.ListAlertEvents` | `admin.go:438` |
| GET | `/api/v1/admin/ops/alert-events/:id` | `h.Admin.Ops.GetAlertEvent` | `admin.go:439` |
| PUT | `/api/v1/admin/ops/alert-events/:id/status` | `h.Admin.Ops.UpdateAlertEventStatus` | `admin.go:440` |
| GET | `/api/v1/admin/ops/alert-rules` | `h.Admin.Ops.ListAlertRules` | `admin.go:434` |
| POST | `/api/v1/admin/ops/alert-rules` | `h.Admin.Ops.CreateAlertRule` | `admin.go:435` |
| DELETE | `/api/v1/admin/ops/alert-rules/:id` | `h.Admin.Ops.DeleteAlertRule` | `admin.go:437` |
| PUT | `/api/v1/admin/ops/alert-rules/:id` | `h.Admin.Ops.UpdateAlertRule` | `admin.go:436` |
| POST | `/api/v1/admin/ops/alert-silences` | `h.Admin.Ops.CreateAlertSilence` | `admin.go:441` |
| GET | `/api/v1/admin/ops/auth-cache-invalidation/health` | `h.Admin.Ops.GetAuthCacheInvalidationHealth` | `admin.go:488` |
| GET | `/api/v1/admin/ops/concurrency` | `h.Admin.Ops.GetConcurrencyStats` | `admin.go:428` |
| GET | `/api/v1/admin/ops/dashboard/error-distribution` | `h.Admin.Ops.GetDashboardErrorDistribution` | `admin.go:509` |
| GET | `/api/v1/admin/ops/dashboard/error-trend` | `h.Admin.Ops.GetDashboardErrorTrend` | `admin.go:508` |
| GET | `/api/v1/admin/ops/dashboard/latency-histogram` | `h.Admin.Ops.GetDashboardLatencyHistogram` | `admin.go:507` |
| GET | `/api/v1/admin/ops/dashboard/openai-token-stats` | `h.Admin.Ops.GetDashboardOpenAITokenStats` | `admin.go:510` |
| GET | `/api/v1/admin/ops/dashboard/overview` | `h.Admin.Ops.GetDashboardOverview` | `admin.go:505` |
| GET | `/api/v1/admin/ops/dashboard/snapshot-v2` | `h.Admin.Ops.GetDashboardSnapshotV2` | `admin.go:504` |
| GET | `/api/v1/admin/ops/dashboard/throughput-trend` | `h.Admin.Ops.GetDashboardThroughputTrend` | `admin.go:506` |
| GET | `/api/v1/admin/ops/email-notification/config` | `h.Admin.Ops.GetEmailNotificationConfig` | `admin.go:444` |
| PUT | `/api/v1/admin/ops/email-notification/config` | `h.Admin.Ops.UpdateEmailNotificationConfig` | `admin.go:445` |
| GET | `/api/v1/admin/ops/errors` | `h.Admin.Ops.GetErrorLogs` | `admin.go:475` |
| GET | `/api/v1/admin/ops/errors/:id` | `h.Admin.Ops.GetErrorLogByID` | `admin.go:476` |
| PUT | `/api/v1/admin/ops/errors/:id/resolve` | `h.Admin.Ops.UpdateErrorResolution` | `admin.go:477` |
| GET | `/api/v1/admin/ops/ingress-rejections` | `h.Admin.Ops.ListIngressRejects` | `admin.go:486` |
| GET | `/api/v1/admin/ops/ingress-rejections/health` | `h.Admin.Ops.GetIngressRejectHealth` | `admin.go:487` |
| GET | `/api/v1/admin/ops/realtime-traffic` | `h.Admin.Ops.GetRealtimeTrafficSummary` | `admin.go:431` |
| GET | `/api/v1/admin/ops/request-errors` | `h.Admin.Ops.ListRequestErrors` | `admin.go:480` |
| GET | `/api/v1/admin/ops/request-errors/:id` | `h.Admin.Ops.GetRequestError` | `admin.go:481` |
| PUT | `/api/v1/admin/ops/request-errors/:id/resolve` | `h.Admin.Ops.ResolveRequestError` | `admin.go:483` |
| GET | `/api/v1/admin/ops/request-errors/:id/upstream-errors` | `h.Admin.Ops.ListRequestErrorUpstreamErrors` | `admin.go:482` |
| GET | `/api/v1/admin/ops/requests` | `h.Admin.Ops.ListRequestDetails` | `admin.go:496` |
| GET | `/api/v1/admin/ops/runtime/alert` | `h.Admin.Ops.GetAlertRuntimeSettings` | `admin.go:450` |
| PUT | `/api/v1/admin/ops/runtime/alert` | `h.Admin.Ops.UpdateAlertRuntimeSettings` | `admin.go:451` |
| GET | `/api/v1/admin/ops/runtime/logging` | `h.Admin.Ops.GetRuntimeLogConfig` | `admin.go:452` |
| PUT | `/api/v1/admin/ops/runtime/logging` | `h.Admin.Ops.UpdateRuntimeLogConfig` | `admin.go:453` |
| POST | `/api/v1/admin/ops/runtime/logging/reset` | `h.Admin.Ops.ResetRuntimeLogConfig` | `admin.go:454` |
| GET | `/api/v1/admin/ops/settings/metric-thresholds` | `h.Admin.Ops.GetMetricThresholds` | `admin.go:464` |
| PUT | `/api/v1/admin/ops/settings/metric-thresholds` | `h.Admin.Ops.UpdateMetricThresholds` | `admin.go:465` |
| GET | `/api/v1/admin/ops/system-logs` | `h.Admin.Ops.ListSystemLogs` | `admin.go:499` |
| POST | `/api/v1/admin/ops/system-logs/cleanup` | `h.Admin.Ops.CleanupSystemLogs` | `admin.go:500` |
| GET | `/api/v1/admin/ops/system-logs/health` | `h.Admin.Ops.GetSystemLogIngestionHealth` | `admin.go:501` |
| GET | `/api/v1/admin/ops/upstream-errors` | `h.Admin.Ops.ListUpstreamErrors` | `admin.go:491` |
| GET | `/api/v1/admin/ops/upstream-errors/:id` | `h.Admin.Ops.GetUpstreamError` | `admin.go:492` |
| PUT | `/api/v1/admin/ops/upstream-errors/:id/resolve` | `h.Admin.Ops.ResolveUpstreamError` | `admin.go:493` |
| GET | `/api/v1/admin/ops/user-concurrency` | `h.Admin.Ops.GetUserConcurrencyStats` | `admin.go:429` |
| GET | `/api/v1/admin/ops/ws/qps` | `h.Admin.Ops.QPSWSHandler` | `admin.go:471` |

## /admin/plugins（11）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/plugins` | `h.Admin.Plugin.List` | `admin.go:1037` |
| DELETE | `/api/v1/admin/plugins/:id` | `h.Admin.Plugin.Delete` | `admin.go:1042` |
| GET | `/api/v1/admin/plugins/:id` | `h.Admin.Plugin.Get` | `admin.go:1038` |
| GET | `/api/v1/admin/plugins/:id/config` | `h.Admin.Plugin.GetConfig` | `admin.go:1043` |
| PUT | `/api/v1/admin/plugins/:id/config` | `h.Admin.Plugin.SaveConfig` | `admin.go:1045` |
| POST | `/api/v1/admin/plugins/:id/disable` | `h.Admin.Plugin.Disable` | `admin.go:1041` |
| POST | `/api/v1/admin/plugins/:id/enable` | `h.Admin.Plugin.Enable` | `admin.go:1040` |
| GET | `/api/v1/admin/plugins/:id/status` | `h.Admin.Plugin.Status` | `admin.go:1044` |
| POST | `/api/v1/admin/plugins/:id/test` | `h.Admin.Plugin.Test` | `admin.go:1046` |
| POST | `/api/v1/admin/plugins/:id/ui-session` | `h.Admin.Plugin.CreateUISession` | `admin.go:1047` |
| POST | `/api/v1/admin/plugins/upload` | `h.Admin.Plugin.Upload` | `admin.go:1039` |

## /admin/pricing（14）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/pricing/catalog` | `h.Admin.Pricing.GetCatalog` | `admin.go:1074` |
| GET | `/api/v1/admin/pricing/cost-basis` | `h.ModelPlaza.GetPricingCostBasis` | `admin.go:1077` |
| PUT | `/api/v1/admin/pricing/cost-basis` | `h.ModelPlaza.SavePricingCostBasis` | `admin.go:1078` |
| GET | `/api/v1/admin/pricing/custom` | `h.Admin.Pricing.ListCustom` | `admin.go:1081` |
| POST | `/api/v1/admin/pricing/custom` | `h.Admin.Pricing.CreateCustom` | `admin.go:1083` |
| DELETE | `/api/v1/admin/pricing/custom/:id` | `h.Admin.Pricing.DeleteCustom` | `admin.go:1085` |
| GET | `/api/v1/admin/pricing/custom/:id` | `h.Admin.Pricing.GetCustom` | `admin.go:1082` |
| PUT | `/api/v1/admin/pricing/custom/:id` | `h.Admin.Pricing.UpdateCustom` | `admin.go:1084` |
| POST | `/api/v1/admin/pricing/experiment/start` | `h.ModelPlaza.StartPricingCostExperiment` | `admin.go:1079` |
| GET | `/api/v1/admin/pricing/experiment/status` | `h.ModelPlaza.GetPricingCostExperimentState` | `admin.go:1080` |
| GET | `/api/v1/admin/pricing/preview` | `h.Admin.Pricing.GetPreview` | `admin.go:1076` |
| GET | `/api/v1/admin/pricing/status` | `h.Admin.Pricing.GetStatus` | `admin.go:1072` |
| POST | `/api/v1/admin/pricing/sync` | `h.Admin.Pricing.SyncNow` | `admin.go:1073` |
| GET | `/api/v1/admin/pricing/uncovered` | `h.Admin.Pricing.GetUncovered` | `admin.go:1075` |

## /admin/promo-codes（6）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/promo-codes` | `h.Admin.Promo.List` | `admin.go:830` |
| POST | `/api/v1/admin/promo-codes` | `h.Admin.Promo.Create` | `admin.go:832` |
| DELETE | `/api/v1/admin/promo-codes/:id` | `h.Admin.Promo.Delete` | `admin.go:834` |
| GET | `/api/v1/admin/promo-codes/:id` | `h.Admin.Promo.GetByID` | `admin.go:831` |
| PUT | `/api/v1/admin/promo-codes/:id` | `h.Admin.Promo.Update` | `admin.go:833` |
| GET | `/api/v1/admin/promo-codes/:id/usages` | `h.Admin.Promo.GetUsages` | `admin.go:835` |

## /admin/promo-intel（12）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/promo-intel/api-keys` | `h.Admin.PromoIntel.ListAPIKeys` | `admin.go:1109` |
| GET | `/api/v1/admin/promo-intel/briefing` | `h.Admin.PromoIntel.GetBriefing` | `admin.go:1104` |
| GET | `/api/v1/admin/promo-intel/items` | `h.Admin.PromoIntel.ListItems` | `admin.go:1102` |
| PUT | `/api/v1/admin/promo-intel/items/:id/status` | `h.Admin.PromoIntel.UpdateItemStatus` | `admin.go:1103` |
| GET | `/api/v1/admin/promo-intel/settings` | `h.Admin.PromoIntel.GetSettings` | `admin.go:1106` |
| PUT | `/api/v1/admin/promo-intel/settings` | `h.Admin.PromoIntel.UpdateSettings` | `admin.go:1107` |
| POST | `/api/v1/admin/promo-intel/settings/test` | `h.Admin.PromoIntel.TestSettings` | `admin.go:1108` |
| GET | `/api/v1/admin/promo-intel/sources` | `h.Admin.PromoIntel.ListSources` | `admin.go:1096` |
| POST | `/api/v1/admin/promo-intel/sources` | `h.Admin.PromoIntel.CreateSource` | `admin.go:1097` |
| DELETE | `/api/v1/admin/promo-intel/sources/:id` | `h.Admin.PromoIntel.DeleteSource` | `admin.go:1099` |
| PUT | `/api/v1/admin/promo-intel/sources/:id` | `h.Admin.PromoIntel.UpdateSource` | `admin.go:1098` |
| POST | `/api/v1/admin/promo-intel/sources/:id/fetch` | `h.Admin.PromoIntel.FetchSourceNow` | `admin.go:1100` |

## /admin/prompt-audit（10）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/prompt-audit/config` | `h.Admin.PromptAudit.GetConfig` | `admin.go:337` |
| PUT | `/api/v1/admin/prompt-audit/config` | `h.Admin.PromptAudit.UpdateConfig` | `admin.go:338` |
| POST | `/api/v1/admin/prompt-audit/endpoints/probe` | `h.Admin.PromptAudit.ProbeEndpoint` | `admin.go:339` |
| GET | `/api/v1/admin/prompt-audit/events` | `h.Admin.PromptAudit.ListEvents` | `admin.go:341` |
| DELETE | `/api/v1/admin/prompt-audit/events/:id` | `h.Admin.PromptAudit.DeleteEvent` | `admin.go:343` |
| GET | `/api/v1/admin/prompt-audit/events/:id` | `h.Admin.PromptAudit.GetEvent` | `admin.go:342` |
| POST | `/api/v1/admin/prompt-audit/events/batch-delete` | `h.Admin.PromptAudit.BatchDelete` | `admin.go:344` |
| POST | `/api/v1/admin/prompt-audit/events/delete-by-filter` | `h.Admin.PromptAudit.DeleteByFilter` | `admin.go:346` |
| POST | `/api/v1/admin/prompt-audit/events/delete-preview` | `h.Admin.PromptAudit.DeletePreview` | `admin.go:345` |
| GET | `/api/v1/admin/prompt-audit/runtime` | `h.Admin.PromptAudit.GetRuntime` | `admin.go:340` |

## /admin/proxies（14）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/proxies` | `h.Admin.Proxy.List` | `admin.go:789` |
| POST | `/api/v1/admin/proxies` | `h.Admin.Proxy.Create` | `admin.go:795` |
| DELETE | `/api/v1/admin/proxies/:id` | `h.Admin.Proxy.Delete` | `admin.go:797` |
| GET | `/api/v1/admin/proxies/:id` | `h.Admin.Proxy.GetByID` | `admin.go:794` |
| PUT | `/api/v1/admin/proxies/:id` | `h.Admin.Proxy.Update` | `admin.go:796` |
| GET | `/api/v1/admin/proxies/:id/accounts` | `h.Admin.Proxy.GetProxyAccounts` | `admin.go:801` |
| POST | `/api/v1/admin/proxies/:id/quality-check` | `h.Admin.Proxy.CheckQuality` | `admin.go:799` |
| GET | `/api/v1/admin/proxies/:id/stats` | `h.Admin.Proxy.GetStats` | `admin.go:800` |
| POST | `/api/v1/admin/proxies/:id/test` | `h.Admin.Proxy.Test` | `admin.go:798` |
| GET | `/api/v1/admin/proxies/all` | `h.Admin.Proxy.GetAll` | `admin.go:790` |
| POST | `/api/v1/admin/proxies/batch` | `h.Admin.Proxy.BatchCreate` | `admin.go:803` |
| POST | `/api/v1/admin/proxies/batch-delete` | `h.Admin.Proxy.BatchDelete` | `admin.go:802` |
| GET | `/api/v1/admin/proxies/data` | `h.Admin.Proxy.ExportData` | `admin.go:792` |
| POST | `/api/v1/admin/proxies/data` | `h.Admin.Proxy.ImportData` | `admin.go:793` |

## /admin/quota_keys（11）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/quota_keys` | `h.Admin.QuotaKey.List` | `admin.go:381` |
| POST | `/api/v1/admin/quota_keys` | `h.Admin.QuotaKey.Create` | `admin.go:383` |
| DELETE | `/api/v1/admin/quota_keys/:id` | `h.Admin.QuotaKey.Delete` | `admin.go:385` |
| GET | `/api/v1/admin/quota_keys/:id` | `h.Admin.QuotaKey.Get` | `admin.go:382` |
| PUT | `/api/v1/admin/quota_keys/:id` | `h.Admin.QuotaKey.Update` | `admin.go:384` |
| POST | `/api/v1/admin/quota_keys/:id/clear_exhausted` | `h.Admin.QuotaKey.ClearExhausted` | `admin.go:387` |
| POST | `/api/v1/admin/quota_keys/:id/probe_quota` | `h.Admin.QuotaKey.ProbeQuota` | `admin.go:388` |
| POST | `/api/v1/admin/quota_keys/:id/refresh` | `h.Admin.QuotaKey.RefreshAccounts` | `admin.go:386` |
| GET | `/api/v1/admin/quota_keys/auto_entry_config` | `h.Admin.QuotaKey.GetAutoEntryConfig` | `admin.go:379` |
| PUT | `/api/v1/admin/quota_keys/auto_entry_config` | `h.Admin.QuotaKey.UpdateAutoEntryConfig` | `admin.go:380` |
| POST | `/api/v1/admin/quota_keys/sweep_expired` | `h.Admin.QuotaKey.SweepExpired` | `admin.go:391` |

## /admin/redeem-codes（14）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/redeem-codes` | `h.Admin.Redeem.List` | `admin.go:810` |
| DELETE | `/api/v1/admin/redeem-codes/:id` | `h.Admin.Redeem.Delete` | `admin.go:820` |
| GET | `/api/v1/admin/redeem-codes/:id` | `h.Admin.Redeem.GetByID` | `admin.go:814` |
| POST | `/api/v1/admin/redeem-codes/:id/expire` | `h.Admin.Redeem.Expire` | `admin.go:823` |
| POST | `/api/v1/admin/redeem-codes/batch-delete` | `h.Admin.Redeem.BatchDelete` | `admin.go:821` |
| POST | `/api/v1/admin/redeem-codes/batch-update` | `h.Admin.Redeem.BatchUpdate` | `admin.go:822` |
| POST | `/api/v1/admin/redeem-codes/create-and-redeem` | `h.Admin.Redeem.CreateAndRedeem` | `admin.go:815` |
| GET | `/api/v1/admin/redeem-codes/export` | `h.Admin.Redeem.Export` | `admin.go:813` |
| POST | `/api/v1/admin/redeem-codes/generate` | `h.Admin.Redeem.Generate` | `admin.go:816` |
| GET | `/api/v1/admin/redeem-codes/stats` | `h.Admin.Redeem.GetStats` | `admin.go:811` |
| GET | `/api/v1/admin/redeem-codes/values` | `h.Admin.Redeem.ListValues` | `admin.go:812` |
| GET | `/api/v1/admin/redeem-codes/welfare-batches` | `h.Admin.Redeem.ListWelfareBatches` | `admin.go:817` |
| POST | `/api/v1/admin/redeem-codes/welfare-batches` | `h.Admin.Redeem.GenerateWelfareBatch` | `admin.go:818` |
| GET | `/api/v1/admin/redeem-codes/welfare-batches/:id/codes` | `h.Admin.Redeem.ListWelfareBatchCodes` | `admin.go:819` |

## /admin/risk-control（8）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/api/v1/admin/risk-control/api-keys/test` | `h.Admin.ContentModeration.TestAPIKeys` | `admin.go:408` |
| GET | `/api/v1/admin/risk-control/config` | `h.Admin.ContentModeration.GetConfig` | `admin.go:406` |
| PUT | `/api/v1/admin/risk-control/config` | `h.Admin.ContentModeration.UpdateConfig` | `admin.go:407` |
| DELETE | `/api/v1/admin/risk-control/hashes` | `h.Admin.ContentModeration.DeleteFlaggedHash` | `admin.go:412` |
| DELETE | `/api/v1/admin/risk-control/hashes/all` | `h.Admin.ContentModeration.ClearFlaggedHashes` | `admin.go:413` |
| GET | `/api/v1/admin/risk-control/logs` | `h.Admin.ContentModeration.ListLogs` | `admin.go:410` |
| GET | `/api/v1/admin/risk-control/status` | `h.Admin.ContentModeration.GetStatus` | `admin.go:409` |
| POST | `/api/v1/admin/risk-control/users/:user_id/unban` | `h.Admin.ContentModeration.UnbanUser` | `admin.go:411` |

## /admin/scheduled-test-plans（4）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/api/v1/admin/scheduled-test-plans` | `h.Admin.ScheduledTest.Create` | `admin.go:1003` |
| DELETE | `/api/v1/admin/scheduled-test-plans/:id` | `h.Admin.ScheduledTest.Delete` | `admin.go:1005` |
| PUT | `/api/v1/admin/scheduled-test-plans/:id` | `h.Admin.ScheduledTest.Update` | `admin.go:1004` |
| GET | `/api/v1/admin/scheduled-test-plans/:id/results` | `h.Admin.ScheduledTest.ListResults` | `admin.go:1006` |

## /admin/settings（30）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/settings` | `h.Admin.Setting.GetSettings` | `admin.go:842` |
| PUT | `/api/v1/admin/settings` | `h.Admin.Setting.UpdateSettings` | `admin.go:843` |
| DELETE | `/api/v1/admin/settings/admin-api-key` | `h.Admin.Setting.DeleteAdminAPIKey` | `admin.go:854` |
| GET | `/api/v1/admin/settings/admin-api-key` | `h.Admin.Setting.GetAdminAPIKey` | `admin.go:852` |
| POST | `/api/v1/admin/settings/admin-api-key/regenerate` | `h.Admin.Setting.RegenerateAdminAPIKey` | `admin.go:853` |
| GET | `/api/v1/admin/settings/beta-policy` | `h.Admin.Setting.GetBetaPolicySettings` | `admin.go:874` |
| PUT | `/api/v1/admin/settings/beta-policy` | `h.Admin.Setting.UpdateBetaPolicySettings` | `admin.go:875` |
| POST | `/api/v1/admin/settings/email-template-preview` | `h.Admin.Setting.PreviewEmailTemplate` | `admin.go:847` |
| GET | `/api/v1/admin/settings/email-templates` | `h.Admin.Setting.ListEmailTemplates` | `admin.go:846` |
| GET | `/api/v1/admin/settings/email-templates/:event/:locale` | `h.Admin.Setting.GetEmailTemplate` | `admin.go:848` |
| PUT | `/api/v1/admin/settings/email-templates/:event/:locale` | `h.Admin.Setting.UpdateEmailTemplate` | `admin.go:849` |
| POST | `/api/v1/admin/settings/email-templates/:event/:locale/restore-official` | `h.Admin.Setting.RestoreOfficialEmailTemplate` | `admin.go:850` |
| GET | `/api/v1/admin/settings/openai-images-oauth-unavailable-cooldown` | `h.Admin.Setting.GetOpenAIImagesOAuthUnavailableCooldownSettings` | `admin.go:862` |
| PUT | `/api/v1/admin/settings/openai-images-oauth-unavailable-cooldown` | `h.Admin.Setting.UpdateOpenAIImagesOAuthUnavailableCooldownSettings` | `admin.go:863` |
| GET | `/api/v1/admin/settings/overload-cooldown` | `h.Admin.Setting.GetOverloadCooldownSettings` | `admin.go:856` |
| PUT | `/api/v1/admin/settings/overload-cooldown` | `h.Admin.Setting.UpdateOverloadCooldownSettings` | `admin.go:857` |
| GET | `/api/v1/admin/settings/panel-rate-limit` | `h.Admin.Setting.GetPanelRateLimitSettings` | `admin.go:865` |
| PUT | `/api/v1/admin/settings/panel-rate-limit` | `h.Admin.Setting.UpdatePanelRateLimitSettings` | `admin.go:866` |
| GET | `/api/v1/admin/settings/rate-limit-429-cooldown` | `h.Admin.Setting.GetRateLimit429CooldownSettings` | `admin.go:859` |
| PUT | `/api/v1/admin/settings/rate-limit-429-cooldown` | `h.Admin.Setting.UpdateRateLimit429CooldownSettings` | `admin.go:860` |
| GET | `/api/v1/admin/settings/rectifier` | `h.Admin.Setting.GetRectifierSettings` | `admin.go:871` |
| PUT | `/api/v1/admin/settings/rectifier` | `h.Admin.Setting.UpdateRectifierSettings` | `admin.go:872` |
| POST | `/api/v1/admin/settings/send-test-email` | `h.Admin.Setting.SendTestEmail` | `admin.go:845` |
| GET | `/api/v1/admin/settings/stream-timeout` | `h.Admin.Setting.GetStreamTimeoutSettings` | `admin.go:868` |
| PUT | `/api/v1/admin/settings/stream-timeout` | `h.Admin.Setting.UpdateStreamTimeoutSettings` | `admin.go:869` |
| POST | `/api/v1/admin/settings/test-smtp` | `h.Admin.Setting.TestSMTPConnection` | `admin.go:844` |
| GET | `/api/v1/admin/settings/web-search-emulation` | `h.Admin.Setting.GetWebSearchEmulationConfig` | `admin.go:877` |
| PUT | `/api/v1/admin/settings/web-search-emulation` | `h.Admin.Setting.UpdateWebSearchEmulationConfig` | `admin.go:878` |
| POST | `/api/v1/admin/settings/web-search-emulation/reset-usage` | `h.Admin.Setting.ResetWebSearchUsage` | `admin.go:880` |
| POST | `/api/v1/admin/settings/web-search-emulation/test` | `h.Admin.Setting.TestWebSearchEmulation` | `admin.go:879` |

## /admin/subscriptions（11）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/subscriptions` | `h.Admin.Subscription.List` | `admin.go:955` |
| DELETE | `/api/v1/admin/subscriptions/:id` | `h.Admin.Subscription.Revoke` | `admin.go:965` |
| GET | `/api/v1/admin/subscriptions/:id` | `h.Admin.Subscription.GetByID` | `admin.go:956` |
| POST | `/api/v1/admin/subscriptions/:id/extend` | `h.Admin.Subscription.Extend` | `admin.go:961` |
| GET | `/api/v1/admin/subscriptions/:id/progress` | `h.Admin.Subscription.GetProgress` | `admin.go:957` |
| POST | `/api/v1/admin/subscriptions/:id/reset-quota` | `h.Admin.Subscription.ResetQuota` | `admin.go:962` |
| POST | `/api/v1/admin/subscriptions/:id/restore` | `h.Admin.Subscription.Restore` | `admin.go:964` |
| POST | `/api/v1/admin/subscriptions/:id/revoke` | `h.Admin.Subscription.Revoke` | `admin.go:963` |
| POST | `/api/v1/admin/subscriptions/assign` | `h.Admin.Subscription.Assign` | `admin.go:958` |
| POST | `/api/v1/admin/subscriptions/bulk-action` | `h.Admin.Subscription.BulkAction` | `admin.go:960` |
| POST | `/api/v1/admin/subscriptions/bulk-assign` | `h.Admin.Subscription.BulkAssign` | `admin.go:959` |

## /admin/system（6）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/system/check-updates` | `h.Admin.System.CheckUpdates` | `admin.go:944` |
| POST | `/api/v1/admin/system/restart` | `h.Admin.System.RestartService` | `admin.go:948` |
| POST | `/api/v1/admin/system/rollback` | `h.Admin.System.Rollback` | `admin.go:947` |
| GET | `/api/v1/admin/system/rollback-versions` | `h.Admin.System.GetRollbackVersions` | `admin.go:945` |
| POST | `/api/v1/admin/system/update` | `h.Admin.System.PerformUpdate` | `admin.go:946` |
| GET | `/api/v1/admin/system/version` | `h.Admin.System.GetVersion` | `admin.go:943` |

## /admin/tls-fingerprint-profiles（5）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/tls-fingerprint-profiles` | `h.Admin.TLSFingerprintProfile.List` | `admin.go:1026` |
| POST | `/api/v1/admin/tls-fingerprint-profiles` | `h.Admin.TLSFingerprintProfile.Create` | `admin.go:1028` |
| DELETE | `/api/v1/admin/tls-fingerprint-profiles/:id` | `h.Admin.TLSFingerprintProfile.Delete` | `admin.go:1030` |
| GET | `/api/v1/admin/tls-fingerprint-profiles/:id` | `h.Admin.TLSFingerprintProfile.GetByID` | `admin.go:1027` |
| PUT | `/api/v1/admin/tls-fingerprint-profiles/:id` | `h.Admin.TLSFingerprintProfile.Update` | `admin.go:1029` |

## /admin/usage（7）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/usage` | `h.Admin.Usage.List` | `admin.go:978` |
| GET | `/api/v1/admin/usage/cleanup-tasks` | `h.Admin.Usage.ListCleanupTasks` | `admin.go:982` |
| POST | `/api/v1/admin/usage/cleanup-tasks` | `h.Admin.Usage.CreateCleanupTask` | `admin.go:983` |
| POST | `/api/v1/admin/usage/cleanup-tasks/:id/cancel` | `h.Admin.Usage.CancelCleanupTask` | `admin.go:984` |
| GET | `/api/v1/admin/usage/search-api-keys` | `h.Admin.Usage.SearchAPIKeys` | `admin.go:981` |
| GET | `/api/v1/admin/usage/search-users` | `h.Admin.Usage.SearchUsers` | `admin.go:980` |
| GET | `/api/v1/admin/usage/stats` | `h.Admin.Usage.Stats` | `admin.go:979` |

## /admin/usage-risk（6）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/usage-risk/reports` | `ur.ListReports` | `admin.go:251` |
| GET | `/api/v1/admin/usage-risk/reports/:report_id` | `ur.GetReport` | `admin.go:252` |
| POST | `/api/v1/admin/usage-risk/reports/:report_id/status` | `ur.UpdateStatus` | `admin.go:253` |
| GET | `/api/v1/admin/usage-risk/run-status` | `ur.GetRunStatus` | `admin.go:254` |
| GET | `/api/v1/admin/usage-risk/settings` | `ur.GetSettings` | `admin.go:255` |
| PUT | `/api/v1/admin/usage-risk/settings` | `ur.PutSettings` | `admin.go:256` |

## /admin/user-attributes（6）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/user-attributes` | `h.Admin.UserAttribute.ListDefinitions` | `admin.go:991` |
| POST | `/api/v1/admin/user-attributes` | `h.Admin.UserAttribute.CreateDefinition` | `admin.go:992` |
| DELETE | `/api/v1/admin/user-attributes/:id` | `h.Admin.UserAttribute.DeleteDefinition` | `admin.go:996` |
| PUT | `/api/v1/admin/user-attributes/:id` | `h.Admin.UserAttribute.UpdateDefinition` | `admin.go:995` |
| POST | `/api/v1/admin/user-attributes/batch` | `h.Admin.UserAttribute.GetBatchUserAttributes` | `admin.go:993` |
| PUT | `/api/v1/admin/user-attributes/reorder` | `h.Admin.UserAttribute.ReorderDefinitions` | `admin.go:994` |

## /admin/users（20）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/users` | `h.Admin.User.List` | `admin.go:536` |
| POST | `/api/v1/admin/users` | `h.Admin.User.Create` | `admin.go:539` |
| DELETE | `/api/v1/admin/users/:id` | `h.Admin.User.Delete` | `admin.go:541` |
| GET | `/api/v1/admin/users/:id` | `h.Admin.User.GetByID` | `admin.go:537` |
| PUT | `/api/v1/admin/users/:id` | `h.Admin.User.Update` | `admin.go:540` |
| GET | `/api/v1/admin/users/:id/api-keys` | `h.Admin.User.GetUserAPIKeys` | `admin.go:543` |
| GET | `/api/v1/admin/users/:id/attributes` | `h.Admin.UserAttribute.GetUserAttributes` | `admin.go:555` |
| PUT | `/api/v1/admin/users/:id/attributes` | `h.Admin.UserAttribute.UpdateUserAttributes` | `admin.go:556` |
| POST | `/api/v1/admin/users/:id/auth-identities` | `h.Admin.User.BindAuthIdentity` | `admin.go:538` |
| POST | `/api/v1/admin/users/:id/balance` | `h.Admin.User.UpdateBalance` | `admin.go:542` |
| GET | `/api/v1/admin/users/:id/balance-history` | `h.Admin.User.GetBalanceHistory` | `admin.go:545` |
| GET | `/api/v1/admin/users/:id/platform-quotas` | `h.Admin.User.GetUserPlatformQuotas` | `admin.go:550` |
| PUT | `/api/v1/admin/users/:id/platform-quotas` | `h.Admin.User.UpdateUserPlatformQuotas` | `admin.go:551` |
| POST | `/api/v1/admin/users/:id/platform-quotas/reset` | `h.Admin.User.ResetUserPlatformQuotaWindow` | `admin.go:552` |
| POST | `/api/v1/admin/users/:id/replace-group` | `h.Admin.User.ReplaceGroup` | `admin.go:546` |
| GET | `/api/v1/admin/users/:id/rpm-status` | `h.Admin.User.GetUserRPMStatus` | `admin.go:547` |
| GET | `/api/v1/admin/users/:id/subscriptions` | `h.Admin.Subscription.ListByUser` | `admin.go:972` |
| GET | `/api/v1/admin/users/:id/usage` | `h.Admin.User.GetUserUsage` | `admin.go:544` |
| POST | `/api/v1/admin/users/batch-concurrency` | `h.Admin.User.BatchUpdateConcurrency` | `admin.go:548` |
| POST | `/api/v1/admin/users/batch-limits` | `h.Admin.User.BatchUpdateLimits` | `admin.go:549` |

## /admin/xianguanjia（8）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/xianguanjia/config` | `h.Admin.XianguanjiaConfig.Get` | `admin.go:210` |
| PUT | `/api/v1/admin/xianguanjia/config` | `h.Admin.XianguanjiaConfig.Put` | `admin.go:211` |
| POST | `/api/v1/admin/xianguanjia/health-check` | `h.Admin.XianguanjiaConfig.HealthCheck` | `admin.go:212` |
| GET | `/api/v1/admin/xianguanjia/pool/kind` | `h.Admin.XianguanjiaPool.KindGet` | `admin.go:192` |
| POST | `/api/v1/admin/xianguanjia/pool/kind` | `h.Admin.XianguanjiaPool.KindCreate` | `admin.go:191` |
| POST | `/api/v1/admin/xianguanjia/pool/push` | `h.Admin.XianguanjiaPool.PoolPush` | `admin.go:193` |
| GET | `/api/v1/admin/xianguanjia/supply-config` | `h.Admin.XianguanjiaSupply.Get` | `admin.go:228` |
| PUT | `/api/v1/admin/xianguanjia/supply-config` | `h.Admin.XianguanjiaSupply.Put` | `admin.go:229` |

## /admin/xianyu（38）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/xianyu/access` | `h.Admin.Xianyu.Access` | `admin.go:268` |
| GET | `/api/v1/admin/xianyu/accounts` | `h.Admin.Xianyu.Accounts` | `admin.go:278` |
| POST | `/api/v1/admin/xianyu/accounts/clear-credentials` | `h.Admin.Xianyu.ClearCredentials` | `admin.go:283` |
| POST | `/api/v1/admin/xianyu/accounts/disable` | `h.Admin.Xianyu.DisableAccount` | `admin.go:281` |
| POST | `/api/v1/admin/xianyu/accounts/enable` | `h.Admin.Xianyu.EnableAccount` | `admin.go:280` |
| POST | `/api/v1/admin/xianyu/accounts/login-session` | `h.Admin.Xianyu.CreateLoginSession` | `admin.go:285` |
| GET | `/api/v1/admin/xianyu/accounts/login-session/:session_id` | `h.Admin.Xianyu.QueryLoginSession` | `admin.go:286` |
| POST | `/api/v1/admin/xianyu/accounts/refresh-cookie` | `h.Admin.Xianyu.RefreshCookie` | `admin.go:282` |
| PUT | `/api/v1/admin/xianyu/accounts/remark/:id` | `h.Admin.Xianyu.UpdateAccountRemark` | `admin.go:284` |
| POST | `/api/v1/admin/xianyu/accounts/sync` | `h.Admin.Xianyu.SyncAccounts` | `admin.go:279` |
| GET | `/api/v1/admin/xianyu/binding-rules` | `h.Admin.Xianyu.BindingRules` | `admin.go:293` |
| POST | `/api/v1/admin/xianyu/binding-rules` | `h.Admin.Xianyu.SaveBindingRule` | `admin.go:294` |
| DELETE | `/api/v1/admin/xianyu/binding-rules/:id` | `h.Admin.Xianyu.DeleteBindingRule` | `admin.go:276` |
| PUT | `/api/v1/admin/xianyu/binding-rules/:id` | `h.Admin.Xianyu.SaveBindingRule` | `admin.go:295` |
| GET | `/api/v1/admin/xianyu/deliveries` | `h.Admin.Xianyu.Deliveries` | `admin.go:302` |
| POST | `/api/v1/admin/xianyu/deliveries/mark-sent` | `h.Admin.Xianyu.MarkDeliverySent` | `admin.go:304` |
| POST | `/api/v1/admin/xianyu/deliveries/resend` | `h.Admin.Xianyu.ResendDelivery` | `admin.go:303` |
| GET | `/api/v1/admin/xianyu/delivery-template` | `h.Admin.Xianyu.GetDeliveryTemplate` | `admin.go:273` |
| PUT | `/api/v1/admin/xianyu/delivery-template` | `h.Admin.Xianyu.UpdateDeliveryTemplate` | `admin.go:274` |
| POST | `/api/v1/admin/xianyu/exposure/test-push` | `h.Admin.Xianyu.TestExposurePush` | `admin.go:311` |
| POST | `/api/v1/admin/xianyu/health/check` | `h.Admin.Xianyu.CheckHealth` | `admin.go:313` |
| GET | `/api/v1/admin/xianyu/item-pools` | `h.Admin.Xianyu.ItemPools` | `admin.go:297` |
| POST | `/api/v1/admin/xianyu/item-pools` | `h.Admin.Xianyu.SaveItemPool` | `admin.go:298` |
| DELETE | `/api/v1/admin/xianyu/item-pools/:id` | `h.Admin.Xianyu.DeleteItemPool` | `admin.go:275` |
| PUT | `/api/v1/admin/xianyu/item-pools/:id` | `h.Admin.Xianyu.SaveItemPool` | `admin.go:299` |
| POST | `/api/v1/admin/xianyu/item-pools/:id/stock` | `h.Admin.Xianyu.StockItemPool` | `admin.go:300` |
| GET | `/api/v1/admin/xianyu/overview` | `h.Admin.Xianyu.Overview` | `admin.go:269` |
| GET | `/api/v1/admin/xianyu/products` | `h.Admin.Xianyu.Products` | `admin.go:288` |
| POST | `/api/v1/admin/xianyu/products/:id/title-suggestion` | `h.Admin.Xianyu.TitleSuggestion` | `admin.go:310` |
| POST | `/api/v1/admin/xianyu/products/bind` | `h.Admin.Xianyu.BindProduct` | `admin.go:290` |
| POST | `/api/v1/admin/xianyu/products/spec-binding` | `h.Admin.Xianyu.CreateProductSpecBinding` | `admin.go:291` |
| POST | `/api/v1/admin/xianyu/products/sync` | `h.Admin.Xianyu.SyncProducts` | `admin.go:289` |
| GET | `/api/v1/admin/xianyu/settings` | `h.Admin.Xianyu.GetSettings` | `admin.go:307` |
| PUT | `/api/v1/admin/xianyu/settings` | `h.Admin.Xianyu.SaveSettings` | `admin.go:308` |
| GET | `/api/v1/admin/xianyu/worker-configs` | `h.Admin.Xianyu.WorkerConfigs` | `admin.go:270` |
| POST | `/api/v1/admin/xianyu/worker-configs` | `h.Admin.Xianyu.SaveWorkerConfig` | `admin.go:271` |
| PUT | `/api/v1/admin/xianyu/worker-configs/:id` | `h.Admin.Xianyu.SaveWorkerConfig` | `admin.go:272` |
| GET | `/api/v1/admin/xianyu/worker-deliveries` | `h.Admin.Xianyu.WorkerDeliveries` | `admin.go:305` |

## /plugin-ui（1）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/plugin-ui/:token/*path` | `h.Admin.Plugin.ServeUIAsset` | `admin.go:25` |


---
