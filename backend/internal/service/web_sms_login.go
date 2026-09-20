package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const webSMSRequestTimeout = 30 * time.Second

type webKimiSendSMSResponse struct{}

type webKimiLoginWithSMSResponse struct {
	AccessToken  string          `json:"accessToken"`
	RefreshToken string          `json:"refreshToken"`
	NewUser      json.RawMessage `json:"newUser"`
	UserID       json.RawMessage `json:"userId"`
	Deactivating json.RawMessage `json:"deactivating"`
}

// ---------------------------------------------------------------------------
// 短信登录（半自动）内核：zhipu / kimi 通过手机号验证码完成登录。
//
// 协议依据（已取证，非猜测）：
//   - 13-zhipu-kimi-login-probe.md（2026-09-18，main.js 逆向）：
//     · zhipu 发码 POST /backend-api/v1/user/send_sms，body {phone, pic_captcha_id, md5, phone_code}
//     · zhipu 登录 POST /user-api/user/phone_login，body {phone, captcha(短信码), pic_captcha_id,
//       phone_code, tI 签名三件套 timestamp/xNonce/sign}
//     · kimi 发码 SMSService.sendVerifyCode({scene:"SCENE_LOGIN", phone:{country_code,number},
//        captcha:{captcha_id, validate}})，host=auth.kimi.com（oauth 命名空间 /api 前缀，E0 实测）
//     · kimi 登录 AuthService.loginWithSMS({phone:{country_code,number}, verify_code})，请求体不带
//        captcha、不带 session_token；响应 LoginWithSMSResponse{access_token,refresh_token,...} 顶层 snake_case
//
// 证据缺失（失败关闭点，绝不发假参数请求）：
//   - 数美滑块参数（pic_captcha_id=rid、md5）来自前端滑块交互，无自动生成算法；
//     必须由调用方在请求内通过 WebSMSChallenge 回传，否则失败关闭。
//   - kimi 发码的易盾 validate 来自前端易盾组件交互，同样由调用方在请求内回传；缺失则失败关闭。
//   - 挑战值绑定当前 sub2api 请求（无状态短期会话），不调用任何外部打码助手，不引入闲鱼契约。
//
// 签名：zhipu phone_login 的 tI 签名三件套（timestamp/xNonce/sign）算法已取证
//   - 02-sign-algorithm.md（2026-09-17，main.js 逆向 + 抓包黄金用例）并由既有
//     webZhipuComputeSign（web_zhipu_gateway_forward.go:498）实现；本文件 phone_login
//     分支直接复用，不再要求调用方传入。数美滑块（rid/md5）仍须调用方传入。
//
// 失败分类统一返回 *webLoginHTTPError（Kind=WebLoginKind*、Code=WebLoginCode*），
// 日志与错误文案绝不携带手机号以外的凭据值。
// ---------------------------------------------------------------------------

// zhipu 发码 / 登录端点（13 号证据，已取证）。
const (
	// webZhipuSendSMSCodeEndpoint zhipu 发送短信验证码（证据：POST /backend-api/v1/user/send_sms，
	// body {phone, pic_captcha_id, md5, phone_code}，data 原样透传）。
	webZhipuSendSMSCodeEndpoint = "/backend-api/v1/user/send_sms"
	// webZhipuPhoneLoginEndpoint zhipu 手机号登录（证据：POST /user-api/user/phone_login，
	// body {phone, captcha(短信码), pic_captcha_id, phone_code, tI 签名三件套}）。
	webZhipuPhoneLoginEndpoint = "/user-api/user/phone_login"
)

// kimi 发码 / 登录端点（E0 决定性取证，2026-09-19：auth.kimi.com 空 body 实测 400/404）。
// 命名空间 = oauth（account 登录），baseUrl = ${AUTH_API_HOST}/api（AUTH_API_HOST 默认
// https://auth.kimi.com）。完整路径已实测可达：
//
//	· 发码 POST https://auth.kimi.com/api/account.gateway.v1.SMSService/SendVerifyCode
//	· 登录 POST https://auth.kimi.com/api/account.gateway.v1.AuthService/LoginWithSMS
//
// 任务候选 /apiv2/ 前缀实测 404 不成立（/apiv2/ 是 kimi chat 命名空间）。
// host 复用 web_platform_auto_login.go 的 webKimiAuthBaseURL（= https://auth.kimi.com）。
const (
	// webKimiSendSMSCodeEndpoint kimi 发送短信验证码（SMSService.sendVerifyCode）。
	// 请求体 {scene:"SCENE_LOGIN", phone:{country_code,number}, captcha:{captcha_id, validate}}（snake_case，
	// useProtoFieldName:true；400 必填字段解码验证 scene+phone，captcha 非强制）。
	webKimiSendSMSCodeEndpoint = "/api/account.gateway.v1.SMSService/SendVerifyCode"
	// webKimiLoginWithSMSEndpoint kimi 短信登录（AuthService.loginWithSMS）。
	// 请求体 {phone:{country_code,number}, verify_code}（**无 captcha 字段**）。
	webKimiLoginWithSMSEndpoint = "/api/account.gateway.v1.AuthService/LoginWithSMS"
)

// webKimiCaptchaID 网易易盾验证码 ID（13 号证据固定值，已取证）。
const webKimiCaptchaID = "2752f01d87dc45948de1a1e0ac2b7160"

// SMSLoginResult 短信登录成功后的业务结果（字段按平台语义填充，未使用的为空串）。
type SMSLoginResult struct {
	// Cookie zhipu 成功返回：整串 Cookie（含 chatglm_token / chatglm_refresh_token）。
	Cookie string
	// AccessToken / RefreshToken kimi 成功返回。
	AccessToken  string
	RefreshToken string
	// ChatGLMToken zhipu 的 chatglm_token 单键（从 Cookie 中提取，供 forward 侧直接消费）。
	ChatGLMToken string
	// LoginRefreshToken 显式 login_refresh_token 落库值：zhipu 从整串 Cookie 提取
	// chatglm_refresh_token（缺失则为空，续期由 refreshZhipu 的 cookie 兜底），
	// kimi 即 RefreshToken。
	LoginRefreshToken string
}

// WebSMSChallenge 验证码关卡求解结果（由调用方从人工打码/前端交互回传，本文件不求解）。
// 证据缺失时对应字段必须显式为空，调用方据此失败关闭，绝不发假参数请求。
// 注意：zhipu phone_login 的 tI 签名三件套不在此列——签名算法已取证并由
// webZhipuComputeSign 内部生成（见 httpUpstream curl 详情），无需调用方传入。
type WebSMSChallenge struct {
	// ZhipuCaptchaRid zhipu 数美滑块 onSuccess 的 rid（localStorage captcha_rid）。
	// 证据：发码 body.pic_captcha_id = rid。
	ZhipuCaptchaRid string
	// ZhipuCaptchaMD5 zhipu 图形校验值（发码 body.md5）。
	ZhipuCaptchaMD5 string
	// ZhipuPhoneCode zhipu 手机号国家码（发码 body.phone_code；与 phone 组装，如 "86"）。
	ZhipuPhoneCode string
	// KimiCaptchaValidate kimi 网易易盾 validate（发码 body.captcha.validate）。
	KimiCaptchaValidate string
}

// SendSmsCode 向 platform 平台手机号发送登录短信验证码。
//
// challenge 承载验证码关卡求解结果（zhipu 数美滑块 rid/md5/phone_code、kimi 易盾
// validate），由调用方在请求内随手机号一并回传（挑战值绑定本次请求）。缺失即失败关闭
// （Kind=WAF，提示需在本机浏览器完成验证后回填），绝不调用外部助手、绝不发假参数请求。
//
// account 用于挑战推送（账号 ID 通知展示 / 官方登录页 URL）；无既有账号时可传仅含
// Platform 的临时账号或 nil。
//
// 成功返回空串（zhipu / kimi 发码成功均无需回传客户端 token：zhipu 协议无 session_token
// 概念，登录时以 phone+code 直接验证；kimi SendVerifyCodeResponse 为 proto 空消息，
// E0 取证确认无 session_token）。遇 WAF / PoW / 限流返回 *webLoginHTTPError。
func (s *WebPlatformAutoLoginService) SendSmsCode(ctx context.Context, platform, phone string, challenge WebSMSChallenge, account *Account) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, webSMSRequestTimeout)
	defer cancel()
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return "", &webLoginHTTPError{
			Platform: platform, Code: -1, Kind: WebLoginKindLogin,
			Msg: "短信登录缺少手机号（不可重试）",
		}
	}
	var send func(WebSMSChallenge) (string, error)
	switch platform {
	case PlatformZhipu:
		send = func(c WebSMSChallenge) (string, error) { return s.sendSmsCodeZhipu(ctx, phone, c, account) }
	case PlatformKimi:
		send = func(c WebSMSChallenge) (string, error) { return s.sendSmsCodeKimi(ctx, phone, c, account) }
	default:
		return "", &webLoginHTTPError{
			Platform: platform, Code: -1, Kind: WebLoginKindLogin,
			Msg: fmt.Sprintf("短信登录暂不支持平台 %q", platform),
		}
	}
	// 挑战值（zhipu 数美 rid+md5；kimi 易盾 validate）由调用方在请求内回传，绑定本次请求；
	// 缺失即失败关闭（Kind=WAF），提示需在本机浏览器完成验证后回填，绝不调用外部助手。
	if miss := smsSendChallengeMissing(platform)(challenge); miss != "" {
		return "", &webLoginHTTPError{
			Platform: platform, Code: -1, Kind: WebLoginKindWAF,
			Msg: fmt.Sprintf("发码需%s，请在浏览器完成验证后回填挑战值再重试", miss),
		}
	}
	return send(challenge)
}

// VerifySmsCode 提交短信验证码完成登录。
//   - zhipu：需要 WebSMSChallenge{ZhipuCaptchaRid, ZhipuPhoneCode}（数美 rid）。
//     tI 签名三件套不要求调用方传入——签名算法已取证并由 webZhipuComputeSign
//     （web_zhipu_gateway_forward.go:498）在方法内部生成。成功返回
//     SMSLoginResult{Cookie, ChatGLMToken, LoginRefreshToken}；
//   - kimi：不需要 captcha（E0 证据：loginWithSMS 请求体不带 captcha、不带 session_token），
//     成功返回 SMSLoginResult{AccessToken, RefreshToken, LoginRefreshToken}。
//
// 求解值（zhipu 数美 rid）由调用方在请求内随短信码回传，缺失即失败关闭（Kind=WAF），
// 绝不调用外部助手、绝不伪造成功。遇 WAF / PoW / 限流返回 *webLoginHTTPError。
func (s *WebPlatformAutoLoginService) VerifySmsCode(ctx context.Context, platform, phone, code string, challenge WebSMSChallenge, account *Account) (*SMSLoginResult, error) {
	ctx, cancel := context.WithTimeout(ctx, webSMSRequestTimeout)
	defer cancel()
	phone = strings.TrimSpace(phone)
	code = strings.TrimSpace(code)
	if phone == "" || code == "" {
		return nil, &webLoginHTTPError{
			Platform: platform, Code: -1, Kind: WebLoginKindLogin,
			Msg: "短信登录缺少手机号或验证码（不可重试）",
		}
	}
	var verify func(WebSMSChallenge) (*SMSLoginResult, error)
	switch platform {
	case PlatformZhipu:
		verify = func(c WebSMSChallenge) (*SMSLoginResult, error) {
			return s.verifySmsCodeZhipu(ctx, phone, code, c, account)
		}
	case PlatformKimi:
		verify = func(WebSMSChallenge) (*SMSLoginResult, error) {
			return s.verifySmsCodeKimi(ctx, phone, code, account)
		}
	default:
		return nil, &webLoginHTTPError{
			Platform: platform, Code: -1, Kind: WebLoginKindLogin,
			Msg: fmt.Sprintf("短信登录暂不支持平台 %q", platform),
		}
	}
	// zhipu 登录仍需数美 rid（kimi loginWithSMS 请求体不带 captcha，无需求解值）。
	// 缺失即失败关闭（Kind=WAF），挑战值由调用方在请求内回传，绝不调用外部助手。
	if miss := smsVerifyChallengeMissing(platform)(challenge); miss != "" {
		return nil, &webLoginHTTPError{
			Platform: platform, Code: -1, Kind: WebLoginKindWAF,
			Msg: fmt.Sprintf("登录需%s，请回填挑战值再重试", miss),
		}
	}
	return verify(challenge)
}

// smsSendChallengeMissing 返回发码操作必需求解值的缺失检查函数：返回缺失项名称，
// 空串表示齐备（zhipu 需数美 rid+md5；kimi 需易盾 validate）。
func smsSendChallengeMissing(platform string) func(WebSMSChallenge) string {
	return func(ch WebSMSChallenge) string {
		switch platform {
		case PlatformZhipu:
			if ch.ZhipuCaptchaRid == "" {
				return "数美滑块 rid（pic_captcha_id）"
			}
			if ch.ZhipuCaptchaMD5 == "" {
				return "图形校验 md5"
			}
		case PlatformKimi:
			if ch.KimiCaptchaValidate == "" {
				return "易盾 validate"
			}
		}
		return ""
	}
}

// smsVerifyChallengeMissing 返回登录提交操作必需求解值的缺失检查函数
// （zhipu 需数美 rid；kimi 登录请求体不带 captcha，无需求解值）。
func smsVerifyChallengeMissing(platform string) func(WebSMSChallenge) string {
	return func(ch WebSMSChallenge) string {
		if platform == PlatformZhipu && ch.ZhipuCaptchaRid == "" {
			return "数美滑块 rid（pic_captcha_id）"
		}
		return ""
	}
}

// ---------------------------------------------------------------------------
// zhipu 短信登录
// ---------------------------------------------------------------------------

// sendSmsCodeZhipu zhipu 发码。证据：POST /backend-api/v1/user/send_sms，
// body {phone, pic_captcha_id, md5, phone_code}。
// 数美滑块参数（rid/md5）来自前端滑块交互，证据 02-sign-algorithm.md 缺失生成算法；
// challenge 必须携带已求解值，缺失即失败关闭。
func (s *WebPlatformAutoLoginService) sendSmsCodeZhipu(ctx context.Context, phone string, challenge WebSMSChallenge, account *Account) (string, error) {
	if challenge.ZhipuCaptchaRid == "" || challenge.ZhipuCaptchaMD5 == "" {
		return "", &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindLogin,
			Msg: "zhipu 发码需数美滑块+图形验证参数（pic_captcha_id/md5），证据缺失或未求解，无法自动发码",
		}
	}
	return "", smsContextGapError(PlatformZhipu, "发码响应业务成功字段尚未取证")
}

// verifySmsCodeZhipu zhipu 短信登录。证据：POST /user-api/user/phone_login，
// body {phone, captcha(短信码), pic_captcha_id, phone_code, tI 签名三件套}。
// tI 签名三件套在方法内部用 webZhipuComputeSign 生成（签名算法已取证
// 02-sign-algorithm.md，web_zhipu_gateway_forward.go:498 实现）；
// 数美滑块 rid 仍须调用方传入，缺失即失败关闭。
func (s *WebPlatformAutoLoginService) verifySmsCodeZhipu(ctx context.Context, phone, code string, challenge WebSMSChallenge, account *Account) (*SMSLoginResult, error) {
	if challenge.ZhipuCaptchaRid == "" {
		return nil, &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindLogin,
			Msg: "zhipu 短信登录缺少数美滑块参数（pic_captcha_id），无法登录",
		}
	}
	return nil, smsContextGapError(PlatformZhipu, "登录响应业务成功字段尚未取证")
}

// ---------------------------------------------------------------------------
// kimi 短信登录
// ---------------------------------------------------------------------------

// sendSmsCodeKimi kimi 发码。E0 决定性证据：SMSService.sendVerifyCode，
// host = webKimiAuthBaseURL（auth.kimi.com），baseUrl 路径 /api/account.gateway.v1.SMSService/SendVerifyCode。
// 线格式 snake_case（useProtoFieldName:true）：{scene:"SCENE_LOGIN", phone:{country_code,number},
// captcha:{captcha_id, validate}}。易盾 validate 来自前端易盾组件交互，由调用方回传；缺失即失败关闭。
// 响应：SendVerifyCodeResponse 为 proto 空消息 —— 无 session_token 概念；发码成功即 HTTP 2xx 返回空串。
// 头：unary ⇒ Content-Type: application/json + Connect-Protocol-Version: 1。
// x-msh-* 设备头（x-msh-device-id / x-msh-session-id 等）依赖 DeviceService.RegisterDevice 响应契约，
// E0 取证未覆盖，按缺口处理：不臆造 id，不加 x-msh-* 头簇，待实测。
func (s *WebPlatformAutoLoginService) sendSmsCodeKimi(ctx context.Context, phone string, challenge WebSMSChallenge, account *Account) (string, error) {
	if challenge.KimiCaptchaValidate == "" {
		return "", &webLoginHTTPError{
			Platform: PlatformKimi, Code: -1, Kind: WebLoginKindLogin,
			Msg: "kimi 发码需网易易盾验证（validate），证据缺失或未求解，无法自动发码",
		}
	}
	target := strings.TrimRight(webKimiAuthBaseURL, "/") + webKimiSendSMSCodeEndpoint
	if _, err := s.validateUpstreamURL(target); err != nil {
		return "", fmt.Errorf("kimi 短信登录目标被 URL 白名单拒绝: %w", err)
	}
	// body 字段按 E0 snake_case：scene + phone{country_code,number} + captcha{captcha_id 固定值, validate}。
	// scene 枚举值经线上实测为 "SCENE_LOGIN"（proto enum 名，非 "LOGIN"；错值会被 buf.validate
	// 判为 required 违反 → HTTP 400 invalid_argument）。
	countryCode, number := splitSMSPhone(phone)
	payload, err := json.Marshal(map[string]any{
		"scene": "SCENE_LOGIN",
		"phone": map[string]string{
			"country_code": countryCode,
			"number":       number,
		},
		"captcha": map[string]string{
			"captcha_id": webKimiCaptchaID,
			"validate":   challenge.KimiCaptchaValidate,
		},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")

	resp, err := s.httpUpstream.Do(req, accountProxyURL(account), smsAccountID(account), smsAccountConcurrency(account))
	if err != nil {
		return "", fmt.Errorf("kimi 短信验证码发送网络错误: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("kimi 短信验证码发送响应读取失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", s.smsHTTPStatusError(PlatformKimi, resp.StatusCode, "kimi 短信验证码发送")
	}
	var parsed webKimiSendSMSResponse
	if err := decodeStrictSMSJSON(raw, &parsed); err != nil {
		return "", &webLoginHTTPError{
			Platform: PlatformKimi, Code: -1, Kind: WebLoginKindLogin,
			Msg: "kimi 短信验证码发送响应不符合 SendVerifyCodeResponse 契约",
		}
	}

	// E0 取证：SendVerifyCodeResponse 是空 proto message；严格 JSON 解码为 {} 才算成功。
	s.logger.Debug("kimi 短信验证码发送成功", "platform", PlatformKimi)
	return "", nil
}

// verifySmsCodeKimi kimi 短信登录。E0 决定性证据：AuthService.loginWithSMS，
// host = webKimiAuthBaseURL（auth.kimi.com），请求体 {phone:{country_code,number}, verify_code}
// （**无 captcha、无 session_token 字段**）。响应 LoginWithSMSResponse{access_token, refresh_token,
// ...} 为 Connect unary 成功响应 = proto message 顶层 JSON（snake_case）。
// 头：unary ⇒ Content-Type: application/json + Connect-Protocol-Version: 1。
// x-msh-* 设备头同 sendSmsCodeKimi，按缺口处理不加。
func (s *WebPlatformAutoLoginService) verifySmsCodeKimi(ctx context.Context, phone, code string, account *Account) (*SMSLoginResult, error) {
	target := strings.TrimRight(webKimiAuthBaseURL, "/") + webKimiLoginWithSMSEndpoint
	if _, err := s.validateUpstreamURL(target); err != nil {
		return nil, fmt.Errorf("kimi 短信登录目标被 URL 白名单拒绝: %w", err)
	}
	// body 字段按 E0 snake_case：phone{country_code, number} + verify_code。
	// countryCode 从手机号国家码推导；缺省按 86（中国大陆）建模。
	countryCode, number := splitSMSPhone(phone)
	body := map[string]any{
		"phone": map[string]string{
			"country_code": countryCode,
			"number":       number,
		},
		"verify_code": code,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")

	resp, err := s.httpUpstream.Do(req, accountProxyURL(account), smsAccountID(account), smsAccountConcurrency(account))
	if err != nil {
		return nil, fmt.Errorf("kimi 短信登录网络错误: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("kimi 短信登录响应读取失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, s.smsHTTPStatusError(PlatformKimi, resp.StatusCode, "kimi 短信登录")
	}

	// Kimi AuthService.loginWithSMS 的 Connect unary 成功响应为 proto message 顶层 JSON，
	// 无 data 包裹。线上实测字段名为 camelCase（accessToken/refreshToken/userId 等），与
	// 早期 snake_case 取证不同。使用普通 Unmarshal 忽略未知字段，仅在必填凭据缺失或 JSON
	// 本身非法时失败关闭。
	var parsed webKimiLoginWithSMSResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, &webLoginHTTPError{
			Platform: PlatformKimi, Code: -1, Kind: WebLoginKindLogin,
			Msg: "kimi 短信登录响应解析失败（不可重试）",
		}
	}
	accessToken := strings.TrimSpace(parsed.AccessToken)
	refreshToken := strings.TrimSpace(parsed.RefreshToken)
	if accessToken == "" || refreshToken == "" {
		// HTTP 2xx 但缺凭据：大概率是业务错误体（如 code/message）。
		// 提取上游可读文案返回给前端；同时记 INFO 级诊断日志（只记结构，不落值）。
		bizMsg := smsFirstStringValue(raw, "message", "msg", "detail", "error", "error_message", "description")
		errText := "kimi 短信登录响应缺少 access_token/refresh_token（不可重试）"
		if bizMsg != "" {
			errText = fmt.Sprintf("kimi 短信登录失败：%s", bizMsg)
		}
		s.logger.Info("kimi 短信登录响应诊断",
			"platform", PlatformKimi,
			"status", resp.StatusCode,
			"content_type", resp.Header.Get("Content-Type"),
			"body_bytes", len(raw),
			"json_keys", smsJSONShape(raw),
			"has_biz_message", bizMsg != "",
		)
		return nil, &webLoginHTTPError{
			Platform: PlatformKimi, Code: -1, Kind: WebLoginKindLogin,
			Msg: errText,
		}
	}
	return &SMSLoginResult{
		AccessToken:       accessToken,
		RefreshToken:      refreshToken,
		LoginRefreshToken: refreshToken,
	}, nil
}

// smsJSONShape 返回 JSON 响应的结构摘要（顶层键名 + 值类型 + 字符串长度），
// **绝不返回字段值**：用于诊断"HTTP 2xx 但缺凭据"的响应形状，满足日志零凭据约束。
// 嵌套对象只描述一行（键名与类型），避免把深层结构展开成可复原内容。
func smsJSONShape(raw []byte) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return "not-an-object"
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := bytes.TrimSpace(top[k])
		switch {
		case len(v) > 0 && v[0] == '{':
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(v, &nested); err == nil {
				nk := make([]string, 0, len(nested))
				for n := range nested {
					nk = append(nk, n)
				}
				sort.Strings(nk)
				parts = append(parts, fmt.Sprintf("%s=object{%s}", k, strings.Join(nk, ",")))
				continue
			}
			parts = append(parts, k+"=object")
		case len(v) > 0 && v[0] == '[':
			parts = append(parts, fmt.Sprintf("%s=array(len=%d)", k, arrayLen(v)))
		case len(v) > 0 && v[0] == '"':
			var s string
			if err := json.Unmarshal(v, &s); err == nil {
				parts = append(parts, fmt.Sprintf("%s=string(len=%d)", k, len(s)))
				continue
			}
			parts = append(parts, k+"=string")
		case string(v) == "null":
			parts = append(parts, k+"=null")
		default:
			parts = append(parts, k+"=scalar")
		}
	}
	return strings.Join(parts, " ")
}

// smsFirstStringValue 从 JSON 体中按候选键顺序提取第一个非空字符串值，
// 用于读取 Kimi/平台业务错误文案（不泄露 token 等凭据）。
func smsFirstStringValue(raw []byte, keys ...string) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return ""
	}
	want := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		want[k] = struct{}{}
	}
	// 优先按传入顺序
	for _, k := range keys {
		v, ok := top[k]
		if !ok {
			continue
		}
		v = bytes.TrimSpace(v)
		if len(v) == 0 || v[0] != '"' {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func arrayLen(raw json.RawMessage) int {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return -1
	}
	return len(arr)
}

func decodeStrictSMSJSON(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func smsContextGapError(platform, detail string) *webLoginHTTPError {
	return smsContextGapErrorKind(platform, WebLoginKindLogin, detail)
}

func smsContextGapErrorKind(platform, kind, detail string) *webLoginHTTPError {
	return &webLoginHTTPError{Platform: platform, Code: -1, Kind: kind, Msg: "context_gap: " + detail}
}

func smsAccountID(account *Account) int64 {
	if account == nil {
		return 0
	}
	return account.ID
}

func smsAccountConcurrency(account *Account) int {
	if account == nil {
		return 0
	}
	return account.Concurrency
}

// splitSMSPhone 拆分手机号为国家码 + 号码（kimi loginWithSMS 的 phone{countryCode,number}）。
// 支持 "86-138..." / "+86 138..." / 纯 11 位（默认 countryCode="86"）等形态。
// 证据未给 countryCode 推导规则，缺省按中国大陆 86 建模，待联调确认。
func splitSMSPhone(phone string) (countryCode, number string) {
	phone = strings.TrimSpace(phone)
	phone = strings.TrimPrefix(phone, "+")
	if i := strings.IndexAny(phone, "- "); i > 0 {
		cc := strings.TrimSpace(phone[:i])
		num := strings.TrimSpace(phone[i+1:])
		if cc != "" && num != "" {
			return cc, num
		}
	}
	if len(phone) >= 11 {
		return "86", phone
	}
	return "86", phone
}

// smsHTTPStatusError 把 HTTP 状态码映射为 webLoginHTTPError：
// 429→RateLimited；403→WAF（可能为 PoW，见注释）；其余→Kind=Login + 状态码。
// 绝不暴露响应体原文。
func (s *WebPlatformAutoLoginService) smsHTTPStatusError(platform string, status int, what string) *webLoginHTTPError {
	switch status {
	case http.StatusTooManyRequests:
		return &webLoginHTTPError{
			Platform: platform, Code: WebLoginCodeHTTPTooMany, Kind: WebLoginKindLogin,
			Msg: fmt.Sprintf("%s触发限流（HTTP 429）", what),
		}
	case http.StatusForbidden:
		// 403 未实测具体语义：可能为 WAF，也可能为 PoW 验证。按保守分类归 WAF，
		// 提示可重试/需人工过验证，待实测确认。
		return &webLoginHTTPError{
			Platform: platform, Code: WebLoginCodePoW1, Kind: WebLoginKindWAF,
			Msg: fmt.Sprintf("%s被安全拦截（HTTP 403，可能需人机验证）", what),
		}
	default:
		return &webLoginHTTPError{
			Platform: platform, Code: int64(status), Kind: WebLoginKindLogin,
			Msg: fmt.Sprintf("%s返回 HTTP %d", what, status),
		}
	}
}

// 编译期断言：确保 *webLoginHTTPError 满足 error 接口（防御未来重构遗漏）。
var _ error = (*webLoginHTTPError)(nil)
