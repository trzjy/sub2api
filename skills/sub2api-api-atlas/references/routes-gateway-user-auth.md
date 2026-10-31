<!-- 由 scripts/gen-route-atlas.py 自动生成，请勿手改。 -->

# 网关 / 用户面 / 支付 / 通用路由索引

重新生成：`python3 skills/sub2api-api-atlas/scripts/gen-route-atlas.py`

# 路由索引：auth.go

- 鉴权域：公开端点与 jwtAuth 混合，逐条以源码为准
- 端点数：59
- 源码：`backend/internal/server/routes/auth.go`

路径列里的 `:id` / `*subpath` 是 gin 路径参数；handler 列给出方法符号，
需要请求体结构时按「位置」列的 文件:行 跳转，读该 handler 里绑定的 Request struct。

## /auth（57）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/api/v1/auth/forgot-password` | `h.Auth.ForgotPassword` | `auth.go:68` |
| POST | `/api/v1/auth/login` | `h.Auth.Login` | `auth.go:38` |
| POST | `/api/v1/auth/login/2fa` | `h.Auth.Login2FA` | `auth.go:41` |
| POST | `/api/v1/auth/logout` | `h.Auth.Logout` | `auth.go:58` |
| GET | `/api/v1/auth/me` | `h.Auth.GetCurrentUser` | `auth.go:257` |
| POST | `/api/v1/auth/oauth/bind-token` | `h.Auth.PrepareOAuthBindAccessTokenCookie` | `auth.go:260` |
| POST | `/api/v1/auth/oauth/dingtalk/bind-login` | `h.Auth.BindDingTalkOAuthLogin` | `auth.go:227` |
| GET | `/api/v1/auth/oauth/dingtalk/bind/start` | `(内联函数)` | `auth.go:214` |
| GET | `/api/v1/auth/oauth/dingtalk/callback` | `h.Auth.DingTalkOAuthCallback` | `auth.go:220` |
| POST | `/api/v1/auth/oauth/dingtalk/complete-registration` | `h.Auth.CompleteDingTalkOAuthRegistration` | `auth.go:221` |
| POST | `/api/v1/auth/oauth/dingtalk/create-account` | `h.Auth.CreateDingTalkOAuthAccount` | `auth.go:233` |
| GET | `/api/v1/auth/oauth/dingtalk/start` | `h.Auth.DingTalkOAuthStart` | `auth.go:210` |
| POST | `/api/v1/auth/oauth/dingtalk/start` | `h.Auth.DingTalkOAuthStart` | `auth.go:211` |
| GET | `/api/v1/auth/oauth/github/callback` | `h.Auth.GitHubOAuthCallback` | `auth.go:83` |
| POST | `/api/v1/auth/oauth/github/complete-registration` | `h.Auth.CompleteGitHubOAuthRegistration` | `auth.go:84` |
| GET | `/api/v1/auth/oauth/github/start` | `h.Auth.GitHubOAuthStart` | `auth.go:79` |
| POST | `/api/v1/auth/oauth/github/start` | `h.Auth.GitHubOAuthStart` | `auth.go:80` |
| GET | `/api/v1/auth/oauth/google/callback` | `h.Auth.GoogleOAuthCallback` | `auth.go:94` |
| POST | `/api/v1/auth/oauth/google/complete-registration` | `h.Auth.CompleteGoogleOAuthRegistration` | `auth.go:95` |
| GET | `/api/v1/auth/oauth/google/start` | `h.Auth.GoogleOAuthStart` | `auth.go:90` |
| POST | `/api/v1/auth/oauth/google/start` | `h.Auth.GoogleOAuthStart` | `auth.go:91` |
| POST | `/api/v1/auth/oauth/linuxdo/bind-login` | `h.Auth.BindLinuxDoOAuthLogin` | `auth.go:151` |
| GET | `/api/v1/auth/oauth/linuxdo/bind/start` | `(内联函数)` | `auth.go:101` |
| GET | `/api/v1/auth/oauth/linuxdo/callback` | `h.Auth.LinuxDoOAuthCallback` | `auth.go:107` |
| POST | `/api/v1/auth/oauth/linuxdo/complete-registration` | `h.Auth.CompleteLinuxDoOAuthRegistration` | `auth.go:145` |
| POST | `/api/v1/auth/oauth/linuxdo/create-account` | `h.Auth.CreateLinuxDoOAuthAccount` | `auth.go:157` |
| GET | `/api/v1/auth/oauth/linuxdo/start` | `h.Auth.LinuxDoOAuthStart` | `auth.go:75` |
| POST | `/api/v1/auth/oauth/linuxdo/start` | `h.Auth.LinuxDoOAuthStart` | `auth.go:76` |
| POST | `/api/v1/auth/oauth/oidc/bind-login` | `h.Auth.BindOIDCOAuthLogin` | `auth.go:198` |
| GET | `/api/v1/auth/oauth/oidc/bind/start` | `(内联函数)` | `auth.go:185` |
| GET | `/api/v1/auth/oauth/oidc/callback` | `h.Auth.OIDCOAuthCallback` | `auth.go:191` |
| POST | `/api/v1/auth/oauth/oidc/complete-registration` | `h.Auth.CompleteOIDCOAuthRegistration` | `auth.go:192` |
| POST | `/api/v1/auth/oauth/oidc/create-account` | `h.Auth.CreateOIDCOAuthAccount` | `auth.go:204` |
| GET | `/api/v1/auth/oauth/oidc/start` | `h.Auth.OIDCOAuthStart` | `auth.go:181` |
| POST | `/api/v1/auth/oauth/oidc/start` | `h.Auth.OIDCOAuthStart` | `auth.go:182` |
| POST | `/api/v1/auth/oauth/pending/bind-login` | `h.Auth.BindPendingOAuthLogin` | `auth.go:139` |
| POST | `/api/v1/auth/oauth/pending/create-account` | `h.Auth.CreatePendingOAuthAccount` | `auth.go:133` |
| POST | `/api/v1/auth/oauth/pending/exchange` | `h.Auth.ExchangePendingOAuthCompletion` | `auth.go:121` |
| POST | `/api/v1/auth/oauth/pending/send-verify-code` | `h.Auth.SendPendingOAuthVerifyCode` | `auth.go:127` |
| POST | `/api/v1/auth/oauth/wechat/bind-login` | `h.Auth.BindWeChatOAuthLogin` | `auth.go:169` |
| GET | `/api/v1/auth/oauth/wechat/bind/start` | `(内联函数)` | `auth.go:112` |
| GET | `/api/v1/auth/oauth/wechat/callback` | `h.Auth.WeChatOAuthCallback` | `auth.go:118` |
| POST | `/api/v1/auth/oauth/wechat/complete-registration` | `h.Auth.CompleteWeChatOAuthRegistration` | `auth.go:163` |
| POST | `/api/v1/auth/oauth/wechat/create-account` | `h.Auth.CreateWeChatOAuthAccount` | `auth.go:175` |
| GET | `/api/v1/auth/oauth/wechat/payment/callback` | `h.Auth.WeChatPaymentOAuthCallback` | `auth.go:120` |
| GET | `/api/v1/auth/oauth/wechat/payment/start` | `h.Auth.WeChatPaymentOAuthStart` | `auth.go:119` |
| GET | `/api/v1/auth/oauth/wechat/start` | `h.Auth.WeChatOAuthStart` | `auth.go:108` |
| POST | `/api/v1/auth/oauth/wechat/start` | `h.Auth.WeChatOAuthStart` | `auth.go:109` |
| POST | `/api/v1/auth/passkey/login/begin` | `h.Passkey.BeginLogin` | `auth.go:44` |
| POST | `/api/v1/auth/passkey/login/finish` | `h.Passkey.FinishLogin` | `auth.go:47` |
| POST | `/api/v1/auth/refresh` | `h.Auth.RefreshToken` | `auth.go:54` |
| POST | `/api/v1/auth/register` | `h.Auth.Register` | `auth.go:35` |
| POST | `/api/v1/auth/reset-password` | `h.Auth.ResetPassword` | `auth.go:72` |
| POST | `/api/v1/auth/revoke-all-sessions` | `h.Auth.RevokeAllSessions` | `auth.go:259` |
| POST | `/api/v1/auth/send-verify-code` | `h.Auth.SendVerifyCode` | `auth.go:50` |
| POST | `/api/v1/auth/validate-invitation-code` | `h.Auth.ValidateInvitationCode` | `auth.go:64` |
| POST | `/api/v1/auth/validate-promo-code` | `h.Auth.ValidatePromoCode` | `auth.go:60` |

## /settings（2）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/settings/email-unsubscribe` | `h.Setting.UnsubscribeNotificationEmail` | `auth.go:247` |
| GET | `/api/v1/settings/public` | `h.Setting.GetPublicSettings` | `auth.go:246` |


---

# 路由索引：common.go

- 鉴权域：无面板鉴权（健康检查、内部服务间令牌）
- 端点数：8
- 源码：`backend/internal/server/routes/common.go`

路径列里的 `:id` / `*subpath` 是 gin 路径参数；handler 列给出方法符号，
需要请求体结构时按「位置」列的 文件:行 跳转，读该 handler 里绑定的 Request struct。

## /api（1）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/api/event_logging/batch` | `(内联函数)` | `common.go:18` |

## /health（1）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/health` | `(内联函数)` | `common.go:13` |

## /internal（4）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/api/v1/internal/xianyu/delivery-results` | `h.XianyuDelivery.DeliveryResult` | `common.go:42` |
| POST | `/api/v1/internal/xianyu/redeem-codes/claim` | `h.XianyuDelivery.Claim` | `common.go:41` |
| POST | `/api/v1/internal/xianyu/refund-events` | `h.XianyuDelivery.RefundEvent` | `common.go:44` |
| POST | `/api/v1/internal/xianyu/worker-deliveries` | `h.XianyuDelivery.EnsureWorkerDeliveryRecord` | `common.go:43` |

## /setup（1）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/setup/status` | `(内联函数)` | `common.go:24` |

## /webhook（1）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/api/v1/webhook/xianguanjia` | `h.XianguanjiaPush.Push` | `common.go:46` |


---

# 路由索引：gateway.go

- 鉴权域：apiKeyAuth（用户 API Key）+ 平台/分组中间件
- 端点数：76
- 源码：`backend/internal/server/routes/gateway.go`

路径列里的 `:id` / `*subpath` 是 gin 路径参数；handler 列给出方法符号，
需要请求体结构时按「位置」列的 文件:行 跳转，读该 handler 里绑定的 Request struct。

## /antigravity（8）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/antigravity/models` | `h.Gateway.AntigravityModels` | `gateway.go:534` |
| POST | `/antigravity/v1/messages` | `h.Gateway.Messages` | `gateway.go:548` |
| POST | `/antigravity/v1/messages/count_tokens` | `h.Gateway.CountTokens` | `gateway.go:549` |
| GET | `/antigravity/v1/models` | `h.Gateway.AntigravityModels` | `gateway.go:550` |
| GET | `/antigravity/v1/usage` | `h.Gateway.Usage` | `gateway.go:551` |
| GET | `/antigravity/v1beta/models` | `h.Gateway.GeminiV1BetaListModels` | `gateway.go:565` |
| POST | `/antigravity/v1beta/models/*modelAction` | `h.Gateway.GeminiV1BetaModels` | `gateway.go:567` |
| GET | `/antigravity/v1beta/models/:model` | `h.Gateway.GeminiV1BetaGetModel` | `gateway.go:566` |

## /backend-api（7）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/backend-api/codex/:call_id` | `h.OpenAIGateway.LiveSideband` | `gateway.go:434` |
| POST | `/backend-api/codex/alpha/search` | `h.OpenAIGateway.AlphaSearch` | `gateway.go:437` |
| GET | `/backend-api/codex/models` | `codexModelsHandler` | `gateway.go:441` |
| POST | `/backend-api/codex/realtime/calls` | `h.OpenAIGateway.Live` | `gateway.go:433` |
| GET | `/backend-api/codex/responses` | `(内联函数)` | `gateway.go:438` |
| POST | `/backend-api/codex/responses` | `responsesHandler` | `gateway.go:435` |
| POST | `/backend-api/codex/responses/*subpath` | `guardResponsesSubpath(responsesHandler` | `gateway.go:436` |

## /v1（53）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/v1/alpha/search` | `h.OpenAIGateway.AlphaSearch` | `gateway.go:270` |
| POST | `/v1/chat/completions` | `(内联函数)` | `gateway.go:275` |
| GET | `/v1/custom-voices` | `voiceHandler("custom-voices"` | `gateway.go:348` |
| POST | `/v1/custom-voices` | `voiceHandler("custom-voices"` | `gateway.go:339` |
| DELETE | `/v1/custom-voices/:voice_id` | `customVoicePathHandler` | `gateway.go:352` |
| GET | `/v1/custom-voices/:voice_id` | `customVoicePathHandler` | `gateway.go:350` |
| PATCH | `/v1/custom-voices/:voice_id` | `customVoicePathHandler` | `gateway.go:351` |
| GET | `/v1/custom-voices/:voice_id/audio` | `customVoicePathHandler` | `gateway.go:349` |
| POST | `/v1/embeddings` | `(内联函数)` | `gateway.go:282` |
| GET | `/v1/images/batches` | `h.BatchImage.List` | `gateway.go:301` |
| POST | `/v1/images/batches` | `h.BatchImage.Submit` | `gateway.go:300` |
| DELETE | `/v1/images/batches/:id` | `h.BatchImage.DeleteRecord` | `gateway.go:308` |
| GET | `/v1/images/batches/:id` | `h.BatchImage.Get` | `gateway.go:303` |
| POST | `/v1/images/batches/:id/cancel` | `h.BatchImage.Cancel` | `gateway.go:307` |
| GET | `/v1/images/batches/:id/download` | `h.BatchImage.Download` | `gateway.go:306` |
| GET | `/v1/images/batches/:id/items` | `h.BatchImage.Items` | `gateway.go:304` |
| GET | `/v1/images/batches/:id/items/:custom_id/content` | `h.BatchImage.ItemContent` | `gateway.go:305` |
| DELETE | `/v1/images/batches/:id/outputs` | `h.BatchImage.DeleteOutputs` | `gateway.go:309` |
| GET | `/v1/images/batches/models` | `h.BatchImage.Models` | `gateway.go:302` |
| POST | `/v1/images/edits` | `imagesHandler` | `gateway.go:296` |
| POST | `/v1/images/edits/async` | `h.AsyncImage.Submit` | `gateway.go:298` |
| POST | `/v1/images/generations` | `imagesHandler` | `gateway.go:295` |
| POST | `/v1/images/generations/async` | `h.AsyncImage.Submit` | `gateway.go:297` |
| GET | `/v1/images/tasks/:task_id` | `h.AsyncImage.Get` | `gateway.go:299` |
| POST | `/v1/live` | `h.OpenAIGateway.Live` | `gateway.go:253` |
| GET | `/v1/live/:call_id` | `h.OpenAIGateway.LiveSideband` | `gateway.go:254` |
| POST | `/v1/messages` | `(内联函数)` | `gateway.go:234` |
| POST | `/v1/messages/count_tokens` | `countTokensHandler` | `gateway.go:245` |
| GET | `/v1/models` | `modelsHandler` | `gateway.go:249` |
| GET | `/v1/models/:model` | `h.Gateway.Models` | `gateway.go:251` |
| GET | `/v1/realtime` | `(内联函数)` | `gateway.go:353` |
| GET | `/v1/responses` | `(内联函数)` | `gateway.go:271` |
| POST | `/v1/responses` | `(内联函数)` | `gateway.go:256` |
| POST | `/v1/responses/*subpath` | `guardResponsesSubpath(func(c *gin.Context) {
			if isOpenAIResponsesCompatibleGatewayPlatform(c) {
				h.OpenAIGateway.Responses(c)
				return
			}
			h.Gateway.Responses(c)
		}` | `gateway.go:263` |
| POST | `/v1/stt` | `voiceHandler("stt"` | `gateway.go:338` |
| GET | `/v1/sub2api/billing` | `h.Gateway.KeyBillingInfo` | `gateway.go:227` |
| POST | `/v1/systemone` | `h.Gateway.SystemOne` | `gateway.go:242` |
| POST | `/v1/tts` | `voiceHandler("tts"` | `gateway.go:337` |
| GET | `/v1/usage` | `h.Gateway.Usage` | `gateway.go:252` |
| POST | `/v1/videos` | `videoGenerationHandler` | `gateway.go:312` |
| GET | `/v1/videos/:request_id` | `videoStatusHandler` | `gateway.go:322` |
| GET | `/v1/videos/:request_id/content` | `videoContentHandler` | `gateway.go:323` |
| POST | `/v1/videos/edits` | `videoEditHandler` | `gateway.go:314` |
| GET | `/v1/videos/edits/:request_id` | `videoStatusHandler` | `gateway.go:320` |
| GET | `/v1/videos/edits/:request_id/content` | `videoContentHandler` | `gateway.go:317` |
| POST | `/v1/videos/extensions` | `videoExtensionHandler` | `gateway.go:315` |
| GET | `/v1/videos/extensions/:request_id` | `videoStatusHandler` | `gateway.go:321` |
| GET | `/v1/videos/extensions/:request_id/content` | `videoContentHandler` | `gateway.go:318` |
| POST | `/v1/videos/generations` | `videoGenerationHandler` | `gateway.go:313` |
| GET | `/v1/videos/generations/:request_id` | `videoStatusHandler` | `gateway.go:319` |
| GET | `/v1/videos/generations/:request_id/content` | `videoContentHandler` | `gateway.go:316` |
| POST | `/v1/web_search` | `(内联函数)` | `gateway.go:361` |
| POST | `/v1/x_search` | `(内联函数)` | `gateway.go:369` |

## /v1beta（8）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/v1beta/cachedContents` | `h.Gateway.GeminiCachedContentsList` | `gateway.go:397` |
| POST | `/v1beta/cachedContents` | `h.Gateway.GeminiCachedContentsCreate` | `gateway.go:396` |
| DELETE | `/v1beta/cachedContents/:cacheID` | `h.Gateway.GeminiCachedContentsDelete` | `gateway.go:400` |
| GET | `/v1beta/cachedContents/:cacheID` | `h.Gateway.GeminiCachedContentsGet` | `gateway.go:398` |
| PATCH | `/v1beta/cachedContents/:cacheID` | `h.Gateway.GeminiCachedContentsPatch` | `gateway.go:399` |
| GET | `/v1beta/models` | `h.Gateway.GeminiV1BetaListModels` | `gateway.go:391` |
| POST | `/v1beta/models/*modelAction` | `h.Gateway.GeminiV1BetaModels` | `gateway.go:394` |
| GET | `/v1beta/models/:model` | `h.Gateway.GeminiV1BetaGetModel` | `gateway.go:392` |


---

# 路由索引：model_plaza.go

- 鉴权域：optionalJWTAuth
- 端点数：1
- 源码：`backend/internal/server/routes/model_plaza.go`

路径列里的 `:id` / `*subpath` 是 gin 路径参数；handler 列给出方法符号，
需要请求体结构时按「位置」列的 文件:行 跳转，读该 handler 里绑定的 Request struct。

## /model-plaza（1）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/model-plaza` | `h.ModelPlaza.Get` | `model_plaza.go:28` |


---

# 路由索引：payment.go

- 鉴权域：用户 jwtAuth / 公开 webhook / 管理端 adminAuth 混合
- 端点数：38
- 源码：`backend/internal/server/routes/payment.go`

路径列里的 `:id` / `*subpath` 是 gin 路径参数；handler 列给出方法符号，
需要请求体结构时按「位置」列的 文件:行 跳转，读该 handler 里绑定的 Request struct。

## /admin/payment（17）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/payment/config` | `adminPaymentHandler.GetConfig` | `payment.go:103` |
| PUT | `/api/v1/admin/payment/config` | `adminPaymentHandler.UpdateConfig` | `payment.go:104` |
| GET | `/api/v1/admin/payment/dashboard` | `adminPaymentHandler.GetDashboard` | `payment.go:100` |
| GET | `/api/v1/admin/payment/orders` | `adminPaymentHandler.ListOrders` | `payment.go:109` |
| GET | `/api/v1/admin/payment/orders/:id` | `adminPaymentHandler.GetOrderDetail` | `payment.go:110` |
| POST | `/api/v1/admin/payment/orders/:id/cancel` | `adminPaymentHandler.CancelOrder` | `payment.go:111` |
| POST | `/api/v1/admin/payment/orders/:id/refund` | `adminPaymentHandler.ProcessRefund` | `payment.go:113` |
| POST | `/api/v1/admin/payment/orders/:id/refund/query` | `adminPaymentHandler.QueryAndFinalizeRefund` | `payment.go:114` |
| POST | `/api/v1/admin/payment/orders/:id/retry` | `adminPaymentHandler.RetryFulfillment` | `payment.go:112` |
| GET | `/api/v1/admin/payment/plans` | `adminPaymentHandler.ListPlans` | `payment.go:120` |
| POST | `/api/v1/admin/payment/plans` | `adminPaymentHandler.CreatePlan` | `payment.go:121` |
| DELETE | `/api/v1/admin/payment/plans/:id` | `adminPaymentHandler.DeletePlan` | `payment.go:123` |
| PUT | `/api/v1/admin/payment/plans/:id` | `adminPaymentHandler.UpdatePlan` | `payment.go:122` |
| GET | `/api/v1/admin/payment/providers` | `adminPaymentHandler.ListProviders` | `payment.go:129` |
| POST | `/api/v1/admin/payment/providers` | `adminPaymentHandler.CreateProvider` | `payment.go:130` |
| DELETE | `/api/v1/admin/payment/providers/:id` | `adminPaymentHandler.DeleteProvider` | `payment.go:132` |
| PUT | `/api/v1/admin/payment/providers/:id` | `adminPaymentHandler.UpdateProvider` | `payment.go:131` |

## /payment（21）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/payment/checkout-info` | `paymentHandler.GetCheckoutInfo` | `payment.go:47` |
| GET | `/api/v1/payment/config` | `paymentHandler.GetPaymentConfig` | `payment.go:46` |
| GET | `/api/v1/payment/limits` | `paymentHandler.GetLimits` | `payment.go:49` |
| POST | `/api/v1/payment/orders` | `paymentHandler.CreateOrder` | `payment.go:53` |
| GET | `/api/v1/payment/orders/:id` | `paymentHandler.GetOrder` | `payment.go:56` |
| POST | `/api/v1/payment/orders/:id/cancel` | `paymentHandler.CancelOrder` | `payment.go:57` |
| POST | `/api/v1/payment/orders/:id/refund-request` | `paymentHandler.RequestRefund` | `payment.go:58` |
| GET | `/api/v1/payment/orders/my` | `paymentHandler.GetMyOrders` | `payment.go:55` |
| GET | `/api/v1/payment/orders/refund-eligible-providers` | `paymentHandler.GetRefundEligibleProviders` | `payment.go:59` |
| POST | `/api/v1/payment/orders/verify` | `paymentHandler.VerifyOrder` | `payment.go:54` |
| GET | `/api/v1/payment/plans` | `paymentHandler.GetPlans` | `payment.go:48` |
| POST | `/api/v1/payment/public/orders/resolve` | `paymentHandler.ResolveOrderPublicByResumeToken` | `payment.go:76` |
| POST | `/api/v1/payment/public/orders/verify` | `paymentHandler.VerifyOrderPublic` | `payment.go:73` |
| POST | `/api/v1/payment/webhook/airwallex` | `webhookHandler.AirwallexWebhook` | `payment.go:90` |
| POST | `/api/v1/payment/webhook/alipay` | `webhookHandler.AlipayNotify` | `payment.go:87` |
| GET | `/api/v1/payment/webhook/easypay` | `webhookHandler.EasyPayNotify` | `payment.go:83` |
| POST | `/api/v1/payment/webhook/easypay` | `webhookHandler.EasyPayNotify` | `payment.go:84` |
| POST | `/api/v1/payment/webhook/stripe` | `webhookHandler.StripeWebhook` | `payment.go:89` |
| POST | `/api/v1/payment/webhook/wxpay` | `webhookHandler.WxpayNotify` | `payment.go:88` |
| GET | `/api/v1/payment/webhook/xunhupay` | `webhookHandler.XunhupayNotify` | `payment.go:85` |
| POST | `/api/v1/payment/webhook/xunhupay` | `webhookHandler.XunhupayNotify` | `payment.go:86` |


---

# 路由索引：user.go

- 鉴权域：jwtAuth（登录用户）
- 端点数：62
- 源码：`backend/internal/server/routes/user.go`

路径列里的 `:id` / `*subpath` 是 gin 路径参数；handler 列给出方法符号，
需要请求体结构时按「位置」列的 文件:行 跳转，读该 handler 里绑定的 Request struct。

## /announcements（2）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/announcements` | `h.Announcement.List` | `user.go:118` |
| POST | `/api/v1/announcements/:id/read` | `h.Announcement.MarkRead` | `user.go:119` |

## /channel-monitor-v2（6）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/channel-monitor-v2/dimensions` | `h.ChannelMonitorV2.Dimensions` | `user.go:151` |
| GET | `/api/v1/channel-monitor-v2/errors` | `h.ChannelMonitorV2.Errors` | `user.go:155` |
| GET | `/api/v1/channel-monitor-v2/matrix` | `h.ChannelMonitorV2.Matrix` | `user.go:154` |
| GET | `/api/v1/channel-monitor-v2/models` | `h.ChannelMonitorV2.Models` | `user.go:153` |
| GET | `/api/v1/channel-monitor-v2/snapshot` | `h.ChannelMonitorV2.Snapshot` | `user.go:152` |
| GET | `/api/v1/channel-monitor-v2/users` | `h.ChannelMonitorV2.Users` | `user.go:156` |

## /channel-monitors（2）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/channel-monitors` | `h.ChannelMonitor.List` | `user.go:142` |
| GET | `/api/v1/channel-monitors/:id/status` | `h.ChannelMonitor.GetStatus` | `user.go:143` |

## /channels（1）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/channels/available` | `h.AvailableChannel.List` | `user.go:95` |

## /groups（2）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/groups/available` | `h.APIKey.GetAvailableGroups` | `user.go:88` |
| GET | `/api/v1/groups/rates` | `h.APIKey.GetUserGroupRates` | `user.go:89` |

## /keys（5）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/keys` | `h.APIKey.List` | `user.go:78` |
| POST | `/api/v1/keys` | `h.APIKey.Create` | `user.go:80` |
| DELETE | `/api/v1/keys/:id` | `h.APIKey.Delete` | `user.go:82` |
| GET | `/api/v1/keys/:id` | `h.APIKey.GetByID` | `user.go:79` |
| PUT | `/api/v1/keys/:id` | `h.APIKey.Update` | `user.go:81` |

## /redeem（3）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/api/v1/redeem` | `h.Redeem.Redeem` | `user.go:125` |
| GET | `/api/v1/redeem/history` | `h.Redeem.GetHistory` | `user.go:126` |
| GET | `/api/v1/redeem/welfare-summary` | `h.Redeem.GetWelfareSummary` | `user.go:127` |

## /subscriptions（4）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/subscriptions` | `h.Subscription.List` | `user.go:133` |
| GET | `/api/v1/subscriptions/active` | `h.Subscription.GetActive` | `user.go:134` |
| GET | `/api/v1/subscriptions/progress` | `h.Subscription.GetProgress` | `user.go:135` |
| GET | `/api/v1/subscriptions/summary` | `h.Subscription.GetSummary` | `user.go:136` |

## /usage（10）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/usage` | `h.Usage.List` | `user.go:102` |
| GET | `/api/v1/usage/:id` | `h.Usage.GetByID` | `user.go:105` |
| POST | `/api/v1/usage/dashboard/api-keys-usage` | `h.Usage.DashboardAPIKeysUsage` | `user.go:112` |
| GET | `/api/v1/usage/dashboard/models` | `h.Usage.DashboardModels` | `user.go:110` |
| GET | `/api/v1/usage/dashboard/snapshot-v2` | `h.Usage.DashboardSnapshotV2` | `user.go:111` |
| GET | `/api/v1/usage/dashboard/stats` | `h.Usage.DashboardStats` | `user.go:108` |
| GET | `/api/v1/usage/dashboard/trend` | `h.Usage.DashboardTrend` | `user.go:109` |
| GET | `/api/v1/usage/errors` | `h.Usage.ListErrors` | `user.go:103` |
| GET | `/api/v1/usage/errors/:id` | `h.Usage.GetErrorDetail` | `user.go:104` |
| GET | `/api/v1/usage/stats` | `h.Usage.Stats` | `user.go:106` |

## /user（27）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| PUT | `/api/v1/user` | `h.User.UpdateProfile` | `user.go:33` |
| DELETE | `/api/v1/user/account-bindings/:provider` | `h.User.UnbindIdentity` | `user.go:38` |
| POST | `/api/v1/user/account-bindings/email` | `h.User.BindEmailIdentity` | `user.go:37` |
| POST | `/api/v1/user/account-bindings/email/send-code` | `h.User.SendEmailBindingCode` | `user.go:36` |
| GET | `/api/v1/user/aff` | `h.User.GetAffiliate` | `user.go:34` |
| POST | `/api/v1/user/aff/transfer` | `h.User.TransferAffiliateQuota` | `user.go:35` |
| GET | `/api/v1/user/api-keys/:id/usage/daily` | `h.Usage.GetMyAPIKeyDailyUsage` | `user.go:40` |
| POST | `/api/v1/user/auth-identities/bind/start` | `h.User.StartIdentityBinding` | `user.go:39` |
| DELETE | `/api/v1/user/notify-email` | `h.User.RemoveNotifyEmail` | `user.go:49` |
| POST | `/api/v1/user/notify-email/send-code` | `h.User.SendNotifyEmailCode` | `user.go:46` |
| PUT | `/api/v1/user/notify-email/toggle` | `h.User.ToggleNotifyEmail` | `user.go:48` |
| POST | `/api/v1/user/notify-email/verify` | `h.User.VerifyNotifyEmail` | `user.go:47` |
| GET | `/api/v1/user/passkeys` | `h.Passkey.List` | `user.go:67` |
| DELETE | `/api/v1/user/passkeys/:id` | `h.Passkey.Delete` | `user.go:71` |
| PATCH | `/api/v1/user/passkeys/:id` | `h.Passkey.Rename` | `user.go:70` |
| POST | `/api/v1/user/passkeys/register/begin` | `h.Passkey.BeginRegistration` | `user.go:68` |
| POST | `/api/v1/user/passkeys/register/finish` | `h.Passkey.FinishRegistration` | `user.go:69` |
| PUT | `/api/v1/user/password` | `h.User.ChangePassword` | `user.go:32` |
| GET | `/api/v1/user/platform-quotas` | `h.User.GetMyPlatformQuotas` | `user.go:41` |
| GET | `/api/v1/user/profile` | `h.User.GetProfile` | `user.go:31` |
| POST | `/api/v1/user/totp/disable` | `h.Totp.Disable` | `user.go:60` |
| POST | `/api/v1/user/totp/enable` | `h.Totp.Enable` | `user.go:59` |
| POST | `/api/v1/user/totp/send-code` | `h.Totp.SendVerifyCode` | `user.go:57` |
| POST | `/api/v1/user/totp/setup` | `h.Totp.InitiateSetup` | `user.go:58` |
| GET | `/api/v1/user/totp/status` | `h.Totp.GetStatus` | `user.go:55` |
| POST | `/api/v1/user/totp/step-up` | `h.Totp.StepUp` | `user.go:62` |
| GET | `/api/v1/user/totp/verification-method` | `h.Totp.GetVerificationMethod` | `user.go:56` |


---

# 路由索引：xgj_supply.go

- 鉴权域：六段验签中间件（闲管家货源模式）
- 端点数：16
- 源码：`backend/internal/server/routes/xgj_supply.go`

路径列里的 `:id` / `*subpath` 是 gin 路径参数；handler 列给出方法符号，
需要请求体结构时按「位置」列的 文件:行 跳转，读该 handler 里绑定的 Request struct。

## /xgj-supply（16）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/api/v1/xgj-supply/goofish/goods/detail` | `h.SupplyCatalog.GoodsDetail` | `xgj_supply.go:38` |
| POST | `/api/v1/xgj-supply/goofish/goods/detail/` | `h.SupplyCatalog.GoodsDetail` | `xgj_supply.go:38` |
| POST | `/api/v1/xgj-supply/goofish/goods/list` | `h.SupplyCatalog.ListGoods` | `xgj_supply.go:37` |
| POST | `/api/v1/xgj-supply/goofish/goods/list/` | `h.SupplyCatalog.ListGoods` | `xgj_supply.go:37` |
| POST | `/api/v1/xgj-supply/goofish/open/info` | `h.SupplyCatalog.PlatformInfo` | `xgj_supply.go:35` |
| POST | `/api/v1/xgj-supply/goofish/open/info/` | `h.SupplyCatalog.PlatformInfo` | `xgj_supply.go:35` |
| POST | `/api/v1/xgj-supply/goofish/order/detail` | `h.XgjSupplyOrder.GetOrder` | `xgj_supply.go:40` |
| POST | `/api/v1/xgj-supply/goofish/order/detail/` | `h.XgjSupplyOrder.GetOrder` | `xgj_supply.go:40` |
| POST | `/api/v1/xgj-supply/goofish/order/purchase/create` | `h.XgjSupplyOrder.CreateOrder` | `xgj_supply.go:39` |
| POST | `/api/v1/xgj-supply/goofish/order/purchase/create/` | `h.XgjSupplyOrder.CreateOrder` | `xgj_supply.go:39` |
| POST | `/api/v1/xgj-supply/goofish/order/refund/apply` | `h.XgjSupplyOrder.RefundNotify` | `xgj_supply.go:41` |
| POST | `/api/v1/xgj-supply/goofish/order/refund/apply/` | `h.XgjSupplyOrder.RefundNotify` | `xgj_supply.go:41` |
| POST | `/api/v1/xgj-supply/goofish/order/refund/notify` | `h.XgjSupplyOrder.RefundResultNotify` | `xgj_supply.go:42` |
| POST | `/api/v1/xgj-supply/goofish/order/refund/notify/` | `h.XgjSupplyOrder.RefundResultNotify` | `xgj_supply.go:42` |
| POST | `/api/v1/xgj-supply/goofish/user/info` | `h.SupplyCatalog.MerchantInfo` | `xgj_supply.go:36` |
| POST | `/api/v1/xgj-supply/goofish/user/info/` | `h.SupplyCatalog.MerchantInfo` | `xgj_supply.go:36` |


---

# 路由索引：页面内容（handler 包内注册）

- 鉴权域：jwtAuth / 无鉴权（图片）/ adminAuth 混合
- 端点数：3
- 源码：`backend/internal/handler/page_handler.go`

路径列里的 `:id` / `*subpath` 是 gin 路径参数；handler 列给出方法符号，
需要请求体结构时按「位置」列的 文件:行 跳转，读该 handler 里绑定的 Request struct。

## /pages（3）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| GET | `/api/v1/pages` | `h.ListPages` | `page_handler.go:282` |
| GET | `/api/v1/pages/:slug` | `h.GetPageContent` | `page_handler.go:268` |
| GET | `/api/v1/pages/:slug/images/*filename` | `h.ServePageImage` | `page_handler.go:274` |


---

# 路由索引：安装向导（仅未初始化实例）

- 鉴权域：setupGuard（仅 NeedsSetup 为真时注册）
- 端点数：4
- 源码：`backend/internal/setup/handler.go`

路径列里的 `:id` / `*subpath` 是 gin 路径参数；handler 列给出方法符号，
需要请求体结构时按「位置」列的 文件:行 跳转，读该 handler 里绑定的 Request struct。

## /setup（4）

| 方法 | 路径 | handler | 位置 |
| --- | --- | --- | --- |
| POST | `/setup/install` | `install` | `handler.go:35` |
| GET | `/setup/status` | `getStatus` | `handler.go:27` |
| POST | `/setup/test-db` | `testDatabase` | `handler.go:33` |
| POST | `/setup/test-redis` | `testRedis` | `handler.go:34` |


---
