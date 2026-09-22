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
//   - 2026-09-22 生产抓包修正（决定性，推翻 13 号 bundle 逆向）：zhipu 发码
//     POST /chatglm/user-api/user/login_captcha（/chatglm 前缀 + user-api 挂载点），
//     body {phone, phone_code, pic_captcha_id, tm:"pc", fr:"default", distinct_id:""}，
//     无 md5 键；成功响应 {"status":0,"message":"...","result":null,"rid":"..."}。
//     /backend-api/v1/user/send_sms 实测 405 不存在。
//     · zhipu 登录 POST /chatglm/user-api/user/phone_login（2026-09-22 22:59 登录步
//       生产抓包终证，/chatglm 前缀与发码同构），body 七键 {phone, captcha(短信码),
//       pic_captcha_id, phone_code:"+86", tm:"pc", fr:"default", sensors_id:""}，
//       **无任何签名键**；签名三件套在请求头（x-timestamp/x-nonce/x-sign）；
//       成功响应 {"status":0,"message":"success","result":{"user_id","access_token",
//       "refresh_token"}}（token 在 result 内，非 Set-Cookie）。
//     · kimi 发码 SMSService.sendVerifyCode({scene:"SCENE_LOGIN", phone:{country_code,number},
//        captcha:{captcha_id, validate}})，host=auth.kimi.com（oauth 命名空间 /api 前缀，E0 实测）
//     · kimi 登录 AuthService.loginWithSMS({phone:{country_code,number}, verify_code})，请求体不带
//        captcha、不带 session_token；响应 LoginWithSMSResponse{access_token,refresh_token,...} 顶层 snake_case
//
// 证据缺失（失败关闭点，绝不发假参数请求）：
//   - 数美滑块参数（pic_captcha_id=rid）来自前端滑块交互，无自动生成算法；
//     必须由调用方在请求内通过 WebSMSChallenge 回传，否则失败关闭（md5 可选，见上）。
//   - kimi 发码的易盾 validate 来自前端易盾组件交互，同样由调用方在请求内回传；缺失则失败关闭。
//   - 挑战值绑定当前 sub2api 请求（无状态短期会话），不调用任何外部打码助手，不引入闲鱼契约。
//
// 签名：zhipu phone_login 的签名三件套（x-timestamp/x-nonce/x-sign）算法已取证
//   - 02-sign-algorithm.md（2026-09-17，main.js 逆向 + 抓包黄金用例）并由既有
//     webZhipuComputeSign（web_zhipu_gateway_forward.go:498）实现；本文件 phone_login
//     分支直接复用，不再要求调用方传入。2026-09-22 登录步抓包终证三件套在请求头
//     （body 无任何签名键）。数美滑块 rid 仍须调用方传入（md5 可选）。
//
// 失败分类统一返回 *webLoginHTTPError（Kind=WebLoginKind*、Code=WebLoginCode*），
// 日志与错误文案绝不携带手机号以外的凭据值。
// ---------------------------------------------------------------------------

// zhipu 发码 / 登录端点。
// 发码端点（2026-09-22 21:40 生产抓包取证，决定性）：POST /chatglm/user-api/user/login_captcha，
// body {phone, phone_code:"+86", pic_captcha_id, tm:"pc", fr:"default", distinct_id:""}，
// 成功响应 {"status":0,"message":"短信验证码已发送","result":null,"rid":"..."}。
// （旧证据 13 号的 /backend-api/v1/user/send_sms 实测 405 不存在——bundle 静态逆向
// 误判调用点；E0 空探针 405 与本次抓包共同证伪。）
// 登录端点：POST /chatglm/user-api/user/phone_login（2026-09-22 22:59 登录步
// 生产抓包终证，/chatglm 前缀与发码同构；13 号 bundle 逆向的无前缀误判已推翻，
// 待证状态解除）。
const (
	// webZhipuSendSMSCodeEndpoint zhipu 发送短信验证码（2026-09-22 生产抓包）。
	webZhipuSendSMSCodeEndpoint = "/chatglm/user-api/user/login_captcha"
	// webZhipuPhoneLoginEndpoint zhipu 手机号登录（2026-09-22 22:59 登录步生产抓包终证）。
	webZhipuPhoneLoginEndpoint = "/chatglm/user-api/user/phone_login"
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
	// chatglm_refresh_token（强制要求：缺失即失败关闭，不再有兜底续期语义），
	// kimi 即 RefreshToken。
	LoginRefreshToken string
}

// WebSMSChallenge 验证码关卡求解结果（由调用方从人工打码/前端交互回传，本文件不求解）。
// 证据缺失时对应字段必须显式为空，调用方据此失败关闭，绝不发假参数请求。
// 注意：zhipu phone_login 的签名三件套不在此列——签名算法已取证并由
// webZhipuComputeSign 内部生成、放入请求头（2026-09-22 登录步抓包终证），
// 无需调用方传入。
type WebSMSChallenge struct {
	// ZhipuCaptchaRid zhipu 数美滑块 onSuccess 的 rid（localStorage captcha_rid）。
	// 证据：发码 body.pic_captcha_id = rid。
	ZhipuCaptchaRid string
	// ZhipuCaptchaMD5 zhipu 图形校验值（发码 body.md5，可选：官方正常滑块流不带，
	// 仅落地链接 query 携带时透传；2026-09-22 线上取证）。
	ZhipuCaptchaMD5 string
	// ZhipuPhoneCode zhipu 手机号国家码（发码 body.phone_code；与 phone 组装，如 "86"）。
	ZhipuPhoneCode string
	// KimiCaptchaValidate kimi 网易易盾 validate（发码 body.captcha.validate）。
	KimiCaptchaValidate string

	// ---------------------------------------------------------------------------
	// Lane H helper result 扩展消费字段（2026-09-22 任务卡：GLM 发码上游 400 会话
	// 边界收敛）。仅存 challenge session 内存（TTL 同既有会话），绝不进日志、DTO、
	// 落库；登录完成后随会话清零。测试一律假数据（零凭据红线）。
	// ---------------------------------------------------------------------------

	// ZhipuHelperSendStatus helper 同会话发码的上游 HTTP 状态（helper result.send_status）。
	// nil = helper 未回传该字段（Lane H 未上线兼容）→ 沿用既有后端直发路径，绝不因
	// 字段缺失而失败；非 nil = 以 helper 同会话发码结果为准（2xx 且 send_body_status==0
	// 才算成功），后端不再重复发码。
	ZhipuHelperSendStatus *int
	// ZhipuHelperSendBodyStatus helper 发码响应体 status 键值（result.send_body_status）。
	// send_status 存在但本字段缺失时，2xx 也无法确认成功 → 失败关闭（不重复发码）。
	ZhipuHelperSendBodyStatus *int
	// ZhipuHelperSendMessage helper 发码失败脱敏文案（result.send_message，≤80 字符）。
	// 仅透出失败文案，绝不写日志。
	ZhipuHelperSendMessage string
	// ZhipuSessionCookie helper 取得 rid 时的同会话匿名会话 Cookie 串（result.cookies）。
	// 一旦存在必须使用（用户裁定 2026-09-22）：发码/登录出站请求附加 Cookie 头
	// （同会话边界收敛）。仅内存存储，绝不写日志。
	ZhipuSessionCookie string
	// ZhipuDeviceID helper 同会话站点 device_id（result.device_id）。一旦存在必须使用：
	// 出站请求附加 x-device-id 头。仅内存存储，绝不写日志。
	ZhipuDeviceID string
}

// SendSmsCode 向 platform 平台手机号发送登录短信验证码。
//
// challenge 承载验证码关卡求解结果（zhipu 数美滑块 rid/phone_code[+可选 md5]、kimi 易盾
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
	// 挑战值（zhipu 数美 rid；kimi 易盾 validate）由调用方在请求内回传，绑定本次请求；
	// 缺失即失败关闭（Kind=WAF），提示需在本机浏览器完成验证后回填，绝不调用外部助手。
	if miss := smsSendChallengeMissing(platform)(challenge); miss != "" {
		return "", &webLoginHTTPError{
			Platform: platform, Code: -1, Kind: WebLoginKindWAF,
			Msg: fmt.Sprintf("发码需%s，请在浏览器完成验证后回填挑战值再重试", miss),
		}
	}
	// Lane H 兼容分流（用户裁定 2026-09-22）：helper result 含 send_status 时，发码已由
	// helper 在取得 rid 的同一浏览器/设备会话内完成，后端不再重复发码（同一上游会话
	// 重复发码会打断会话边界收敛——生产 400 缺陷根因）；send_status 非成功则失败关闭
	// 透出。字段缺失（Lane H 未上线）沿用既有后端直发路径，绝不因字段缺失而失败；
	// cookies/device_id 一旦存在必须存入 challenge 会话供登录步复用。
	if platform == PlatformZhipu && challenge.ZhipuHelperSendStatus != nil {
		return s.zhipuHelperSendOutcome(challenge)
	}
	return send(challenge)
}

// VerifySmsCode 提交短信验证码完成登录。
//   - zhipu：需要 WebSMSChallenge{ZhipuCaptchaRid, ZhipuPhoneCode}（数美 rid）。
//     签名三件套不要求调用方传入——签名算法已取证并由 webZhipuComputeSign
//     （web_zhipu_gateway_forward.go:498）生成、放入请求头。成功返回
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
// 空串表示齐备（zhipu 需数美 rid；kimi 需易盾 validate）。
// md5 不再必填（2026-09-22 chatglm.cn 线上取证：官方滑块 onSuccess 仅回调
// {rid, pass}，发码 body 的 md5 是落地链接 query 的可选参数，正常滑块流省键）。
func smsSendChallengeMissing(platform string) func(WebSMSChallenge) string {
	return func(ch WebSMSChallenge) string {
		switch platform {
		case PlatformZhipu:
			if ch.ZhipuCaptchaRid == "" {
				return "数美滑块 rid（pic_captcha_id）"
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

// zhipuHelperSendOutcome 判定 helper 同会话发码结果（Lane G 消费，用户裁定
// 2026-09-22：status==0 成功；helper 已在取得 rid 的同一会话内发码，后端绝不重复发码）：
//   - HTTP 非 2xx（send_status < 200 或 >= 300）→ 失败关闭（经 smsHTTPStatusError
//     统一状态码分类，绝不暴露响应体）；
//   - 2xx 但 send_body_status 缺失 → 失败关闭（无成功证据，绝不放行）；
//   - send_body_status != 0 → 失败关闭透出（附 helper 脱敏文案 send_message，如有）；
//   - send_body_status == 0 → 成功（返回空串，与后端直发成功语义一致）。
func (s *WebPlatformAutoLoginService) zhipuHelperSendOutcome(challenge WebSMSChallenge) (string, error) {
	sendStatus := *challenge.ZhipuHelperSendStatus
	if sendStatus < 200 || sendStatus >= 300 {
		return "", s.smsHTTPStatusError(PlatformZhipu, sendStatus, "zhipu 短信验证码发送（helper 同会话）")
	}
	if challenge.ZhipuHelperSendBodyStatus == nil {
		s.logger.Info("zhipu helper 同会话发码结果诊断",
			"platform", PlatformZhipu,
			"send_status", sendStatus,
			"has_body_status", false,
			"has_message", strings.TrimSpace(challenge.ZhipuHelperSendMessage) != "",
		)
		return "", &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindLogin,
			Msg: "helper 同会话发码响应缺少 status 判定字段（失败关闭，不重复发码）",
		}
	}
	bodyStatus := *challenge.ZhipuHelperSendBodyStatus
	if bodyStatus != 0 {
		msg := fmt.Sprintf("helper 同会话发码失败（status=%d）", bodyStatus)
		if m := strings.TrimSpace(challenge.ZhipuHelperSendMessage); m != "" {
			msg += "：" + m
		}
		return "", &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindLogin,
			Msg: msg,
		}
	}
	return "", nil
}

// applyZhipuSessionContext 把 helper 同会话上下文（匿名会话 Cookie 串 + 站点
// device_id）附加到出站请求（发码/登录共用；Lane G，用户裁定 2026-09-22：cookies/
// device_id 一旦存在必须使用）。两者均可能缺失（字段缺失沿用现行为，不失败、不写
// 任何值）；值仅来自 challenge session 内存，绝不写日志。
func applyZhipuSessionContext(req *http.Request, challenge WebSMSChallenge) {
	if cookie := strings.TrimSpace(challenge.ZhipuSessionCookie); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if deviceID := strings.TrimSpace(challenge.ZhipuDeviceID); deviceID != "" {
		req.Header.Set("x-device-id", deviceID)
	}
}

// sendSmsCodeZhipu zhipu 发码。证据：2026-09-22 21:40 生产抓包（决定性）——
// POST /chatglm/user-api/user/login_captcha，body {phone, phone_code:"+86",
// pic_captcha_id, tm:"pc", fr:"default", distinct_id:""}；无 md5 键（13 号证据的
// /backend-api/v1/user/send_sms 实测 405，bundle 逆向误判调用点，已证伪）。
// 数美滑块 rid 必填，缺失即失败关闭；md5 按旧证据链保留可选透传（抓包 body 未含，
// 不再写入）。
//
// 响应判定（抓包契约 + 失败关闭，发码专用判定）：成功 ⇔ HTTP 2xx 且 body 解析出
// status==0（2026-09-22 抓包成功体为 {"status":0,...}）/ code==0 / ret==0 /
// success==true（后三者为历史白名单形态，向后兼容）。2xx 但无可判定字段 →
// 失败关闭，附 smsJSONShape 零凭据诊断日志；body 明确失败标志（status 非 0 /
// success=false / code 非 0）→ 失败关闭透出文案。
func (s *WebPlatformAutoLoginService) sendSmsCodeZhipu(ctx context.Context, phone string, challenge WebSMSChallenge, account *Account) (string, error) {
	if challenge.ZhipuCaptchaRid == "" {
		return "", &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindLogin,
			Msg: "zhipu 发码需数美滑块参数（pic_captcha_id），证据缺失或未求解，无法自动发码",
		}
	}
	base := strings.TrimRight(DefaultWebZhipuBaseURL, "/")
	target := base + webZhipuSendSMSCodeEndpoint
	if _, err := s.validateUpstreamURL(target); err != nil {
		return "", fmt.Errorf("zhipu 发码目标被 URL 白名单拒绝: %w", err)
	}
	xTimestamp, xNonce, xSign := s.webZhipuSignTriplet()
	// 与 2026-09-22 抓包 body 完全对齐：无 md5；tm/fr/distinct_id 为官方固定值。
	// phone_code 归一：抓包（2026-09-22）发码 body 中 phone_code="+86"（带加号），
	// SplitSMSPhone 返回 "86"（无加号），组包前统一补 "+" 前缀（登录路径
	// 2026-09-22 22:59 抓包终证 phone_code 同为 "+86"，两路径共用同一归一函数）。
	body := map[string]string{
		"phone":          phone,
		"phone_code":     zhipuNormalizePhoneCode(challenge.ZhipuPhoneCode),
		"pic_captcha_id": challenge.ZhipuCaptchaRid,
		"tm":             "pc",
		"fr":             "default",
		"distinct_id":    "",
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	applyZhipuFingerprintHeaders(req, xTimestamp, xNonce, xSign)
	// Lane H 会话边界收敛：helper result 携带的同会话 Cookie/x-device-id 一旦存在必须
	// 附加（用户裁定 2026-09-22）；字段缺失不附加，沿用既有头形态。登录步是否强制同
	// 会话 = context_gap（上游无业务响应证据），活体验收判定。
	applyZhipuSessionContext(req, challenge)
	resp, err := s.httpUpstream.Do(req, accountProxyURL(account), smsAccountID(account), smsAccountConcurrency(account))
	if err != nil {
		return "", fmt.Errorf("zhipu 短信验证码发送网络错误: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("zhipu 短信验证码发送响应读取失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		// 非 2xx 最小脱敏诊断（零凭据：状态码 + json_keys + 上下文存在布尔）。
		s.logSMSHTTPStatusDiagnostics(PlatformZhipu, "zhipu 短信验证码发送", resp.StatusCode, raw, challenge)
		return "", s.smsHTTPStatusError(PlatformZhipu, resp.StatusCode, "zhipu 短信验证码发送")
	}
	if msg, failed := zhipuSendBizFailure(raw); failed {
		return "", &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindLogin,
			Msg: "zhipu 短信验证码发送失败：" + msg,
		}
	}
	// 发码专用判定（2026-09-22 抓包成功体 {"status":0}）：status==0 优先，
	// 其余历史白名单（code==0 / ret==0 / success==true）经回退保持成功；
	// 2xx 但无可判定字段 → 失败关闭（不再放行），附零凭据诊断日志。
	if !zhipuSendBizSuccess(raw) {
		s.logger.Info("zhipu 短信验证码发送响应诊断",
			"platform", PlatformZhipu,
			"status", resp.StatusCode,
			"content_type", resp.Header.Get("Content-Type"),
			"body_bytes", len(raw),
			"json_keys", smsJSONShape(raw),
		)
		return "", &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindLogin,
			Msg: "发码响应无法确认成功（缺少已取证成功标志）",
		}
	}
	return "", nil
}

// verifySmsCodeZhipu zhipu 短信登录。证据：2026-09-22 22:59 登录步生产抓包（决定性，
// 推翻 13 号 bundle 逆向）——POST /chatglm/user-api/user/phone_login，
// body 七键 {phone, captcha(短信码), pic_captcha_id, phone_code:"+86", tm:"pc",
// fr:"default", sensors_id:""}，**无任何签名键**；签名三件套 x-timestamp/x-nonce/x-sign
// 在请求头（与发码请求头同构，由共享 applyZhipuFingerprintHeaders 组装、
// webZhipuComputeSign 生成，见 web_zhipu_gateway_forward.go:498）。数美滑块 rid
// 仍须调用方传入，缺失即失败关闭。
//
// 成功判定（2026-09-22 登录步抓包终证契约 + 失败关闭，正向条件齐全才算成功）：
//  1. HTTP 2xx 且顶层 status==0（与发码同族成功标志）且 result.access_token 与
//     result.refresh_token 均非空 → 成功；任一缺失失败关闭（绝不发假登录态）。
//     token 在响应体 result 内，非 Set-Cookie；
//  2. body 候选业务失败标志（顶层 status 非 0、success=false / code/ret 或嵌套
//     data.code、data.status 为非 0 数值或字符串数字）→ 失败关闭透出文案；
//     失败拦截由共享 zhipuBizFailure 在成功判定前调用（已含顶层 status 非 0、
//     嵌套 data.code/status、success=false，杜绝 {"status":500,"success":true}
//     冲突响应判成功）；
//  3. 2xx 但 status 非 0 或 result 缺任一 token → 失败关闭 + INFO 诊断日志
//     （smsJSONShape 零凭据），不依赖 Set-Cookie、绝不采纳 Cookie 为登录态。
//
// 结果提取（单一路径，来自 body，不依赖 Set-Cookie）：
//   - SMSLoginResult.Cookie = "chatglm_token=<access_token>; chatglm_refresh_token=<refresh_token>"
//     （cookie 键名证据：登录请求 Cookie 中既有同名键承载同族 JWT）；
//   - ChatGLMToken = result.access_token；LoginRefreshToken = result.refresh_token。
func (s *WebPlatformAutoLoginService) verifySmsCodeZhipu(ctx context.Context, phone, code string, challenge WebSMSChallenge, account *Account) (*SMSLoginResult, error) {
	if challenge.ZhipuCaptchaRid == "" {
		return nil, &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindLogin,
			Msg: "zhipu 短信登录缺少数美滑块参数（pic_captcha_id），无法登录",
		}
	}
	base := strings.TrimRight(DefaultWebZhipuBaseURL, "/")
	target := base + webZhipuPhoneLoginEndpoint
	if _, err := s.validateUpstreamURL(target); err != nil {
		return nil, fmt.Errorf("zhipu 短信登录目标被 URL 白名单拒绝: %w", err)
	}
	xTimestamp, xNonce, xSign := s.webZhipuSignTriplet()
	// 2026-09-22 登录步抓包 body 七键，无任何签名键（三件套在请求头）。
	// phone_code 归一与发码同一函数（"86" → "+86"，抓包登录 body.phone_code="+86"）。
	payload, err := json.Marshal(map[string]string{
		"phone":          phone,
		"captcha":        code,
		"pic_captcha_id": challenge.ZhipuCaptchaRid,
		"phone_code":     zhipuNormalizePhoneCode(challenge.ZhipuPhoneCode),
		"tm":             "pc",
		"fr":             "default",
		"sensors_id":     "",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	applyZhipuFingerprintHeaders(req, xTimestamp, xNonce, xSign)
	// Lane H 会话边界收敛：复用发码会话的 Cookie/x-device-id（存在于 challenge session
	// 即必须使用；context_gap：登录步是否强制同会话待活体验收判定）。
	applyZhipuSessionContext(req, challenge)

	resp, err := s.httpUpstream.Do(req, accountProxyURL(account), smsAccountID(account), smsAccountConcurrency(account))
	if err != nil {
		return nil, fmt.Errorf("zhipu 短信登录网络错误: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("zhipu 短信登录响应读取失败: %w", err)
	}
	// 正向成功条件 1：仅 HTTP 2xx 可继续（3xx/4xx/5xx 一律失败关闭）。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 非 2xx 最小脱敏诊断（零凭据：状态码 + json_keys + 上下文存在布尔）。
		s.logSMSHTTPStatusDiagnostics(PlatformZhipu, "zhipu 短信登录", resp.StatusCode, raw, challenge)
		return nil, s.smsHTTPStatusError(PlatformZhipu, resp.StatusCode, "zhipu 短信登录")
	}
	// 失败拦截（共享 zhipuBizFailure，先于成功判定）：顶层 status 非 0、
	// success=false、code/ret 非 0（含字符串数字）、嵌套 data.code/data.status 非 0。
	if msg, failed := zhipuBizFailure(raw); failed {
		return nil, &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindLogin,
			Msg: "zhipu 短信登录失败：" + msg,
		}
	}
	// 正向成功条件 2：顶层 status==0（2026-09-22 登录步抓包成功体 {"status":0,...}）。
	if !zhipuTopStatusZero(raw) {
		s.logger.Info("zhipu 短信登录响应诊断",
			"platform", PlatformZhipu,
			"status", resp.StatusCode,
			"content_type", resp.Header.Get("Content-Type"),
			"body_bytes", len(raw),
			"json_keys", smsJSONShape(raw),
		)
		return nil, &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindLogin,
			Msg: "GLM 登录响应缺少已取证成功标志（status==0）",
		}
	}
	// 正向成功条件 3：result.access_token 与 result.refresh_token 均非空（token 在
	// body result 内，单一路径提取，不依赖 Set-Cookie）；任一缺失失败关闭。
	accessToken, refreshToken := zhipuResultTokens(raw)
	if accessToken == "" || refreshToken == "" {
		s.logger.Info("zhipu 短信登录响应诊断",
			"platform", PlatformZhipu,
			"status", resp.StatusCode,
			"content_type", resp.Header.Get("Content-Type"),
			"body_bytes", len(raw),
			"json_keys", smsJSONShape(raw),
			"has_access_token", accessToken != "",
			"has_refresh_token", refreshToken != "",
		)
		if accessToken == "" {
			return nil, &webLoginHTTPError{
				Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindLogin,
				Msg: "zhipu 短信登录响应缺少 result.access_token（不可重试）",
			}
		}
		return nil, &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindLogin,
			Msg: "GLM 登录成功但响应缺少 result.refresh_token（续期凭证），拒绝建号——请重试或检查账号风控状态",
		}
	}
	// 结果组装：Cookie 键名沿用登录请求 Cookie 中同族 JWT 的既有键（chatglm_token /
	// chatglm_refresh_token）；token 值绝不进日志与错误文案（零凭据约束）。
	return &SMSLoginResult{
		Cookie:            "chatglm_token=" + accessToken + "; chatglm_refresh_token=" + refreshToken,
		ChatGLMToken:      accessToken,
		LoginRefreshToken: refreshToken,
	}, nil
}

// zhipuTopStatusZero 报告 zhipu 响应体顶层 status 是否为 0（宽容解析：数值或字符串
// 数字均接受）。status 字段缺失 / 解析失败 / 非 0 一律 false，由调用方失败关闭。
// 2026-09-22 登录步抓包终证登录成功体为 {"status":0,"message":"success","result":{...}}。
func zhipuTopStatusZero(raw []byte) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return false
	}
	n, ok := zhipuNumericField(top["status"])
	return ok && n == 0
}

// zhipuResultTokens 从登录成功响应体 result 内提取 access_token / refresh_token
// （单一路径，来自 body，不依赖 Set-Cookie）。仅返回字符串值；缺失/非字符串返回空串，
// 由调用方失败关闭。token 值仅用于组装返回结果，绝不写入日志与错误文案。
func zhipuResultTokens(raw []byte) (accessToken, refreshToken string) {
	var top struct {
		Result struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", ""
	}
	return top.Result.AccessToken, top.Result.RefreshToken
}

// webZhipuSignTriplet 即时生成签名三件套（同一次生成，nonce 一致性保证 x-sign 有效）。
func (s *WebPlatformAutoLoginService) webZhipuSignTriplet() (xTimestamp, xNonce, xSign string) {
	return webZhipuComputeSign(time.Now().UnixMilli())
}

// WebZhipuComputeSignForHelper 导出给 handler 的签名三件套生成入口（Lane G：sdk-start
// body 下发，zhipu 同会话发码用）。同一次生成（timestamp/nonce/sign 配对），算法与
// 既有 webZhipuComputeSign（web_zhipu_gateway_forward.go:498，02-sign-algorithm.md
// 取证）完全一致，不发明第二套。值只进 sdk-start 请求体，绝不写日志。
func WebZhipuComputeSignForHelper() (xTimestamp, xNonce, xSign string) {
	return webZhipuComputeSign(time.Now().UnixMilli())
}

// applyZhipuFingerprintHeaders 按 2026-09-17 登录态抓包 + 2026-09-22 发码抓包对齐的
// 指纹头组装（与 buildWebZhipuUpstreamRequest 同源；登录前无 Cookie/token，故无
// Authorization/Cookie；x-device-id 抓包确认携带——22 号发码抓包带匿名 guest token
// 的 device_id，本链路无 token 可解析则不携带）。签名三件套 header 形态抓包确认。
func applyZhipuFingerprintHeaders(req *http.Request, xTimestamp, xNonce, xSign string) {
	origin := strings.TrimRight(DefaultWebZhipuBaseURL, "/")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", webZhipuClientUA)
	req.Header.Set("app-name", "chatglm")
	req.Header.Set("x-app-platform", "pc")
	req.Header.Set("x-app-version", "0.0.1")
	req.Header.Set("x-app-fr", "default")
	req.Header.Set("x-lang", "zh")
	req.Header.Set("X-Timestamp", xTimestamp)
	req.Header.Set("X-Nonce", xNonce)
	req.Header.Set("X-Sign", xSign)
	req.Header.Set("X-Request-Id", webZhipuUUIDHex())
}

// zhipuBizSuccess 报告 zhipu 响应体是否携带已取证的成功标志：
// code==0 或 success==true（二者任一，字段名与成功值均已取证）。
// 其余形状（无字段、解析失败、非对象体）一律视为不可确认成功，由调用方失败关闭。
//
// 注意：status==0 不在共享白名单内——2026-09-22 抓包终证发码与登录成功体均为
// {"status":0,...}，status==0 判定分别经发码专用 zhipuSendBizSuccess 与登录路径
// zhipuTopStatusZero 采信；共享白名单保持仅认 code/ret/success。
func zhipuBizSuccess(raw []byte) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return false
	}
	if v, ok := top["success"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err == nil && b {
			return true
		}
	}
	for _, key := range []string{"code", "ret"} {
		v, ok := top[key]
		if !ok {
			continue
		}
		var n int64
		if err := json.Unmarshal(v, &n); err == nil && n == 0 {
			return true
		}
	}
	return false
}

// zhipuSendBizSuccess 发码专用成功判定：2026-09-22 生产抓包终证发码成功体为
// {"status":0,...}，故先查 status==0，再回退共享 zhipuBizSuccess（code==0 /
// ret==0 / success==true 历史白名单）。登录路径成功判定使用 zhipuTopStatusZero
// （2026-09-22 22:59 登录步抓包终证同一 status 字段契约），不经由此函数。
func zhipuSendBizSuccess(raw []byte) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return false
	}
	if v, ok := top["status"]; ok {
		var n int64
		if err := json.Unmarshal(v, &n); err == nil && n == 0 {
			return true
		}
	}
	return zhipuBizSuccess(raw)
}

// zhipuBizFailure 从 zhipu user-api 响应体提取明确的业务失败标志（宽容解析：
// 仅识别确定性失败形态，未知形状不算失败）。返回 (文案, 是否失败)。
// 候选形态（与既有取证错误体一致）：success=false；顶层或 data 嵌套的
// code/ret 为非 0 数值，或为字符串数字（如 "code":"403"，解析为数值后
// 非 0 即失败）。文案取 message/msg/detail 候选，绝不携带凭据值。
//
// 注意：顶层 status 非 0 的失败识别于 2026-09-22 整改轮恢复至共享函数（登录
// 路径原有语义：status 非 0 先于 zhipuBizSuccess 的 success 放行被拦截，
// 杜绝 {"status":500,"success":true} 冲突响应判成功）；发码路径经
// zhipuSendBizFailure 先行检查，判定结果不变。
func zhipuBizFailure(raw []byte) (string, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", false
	}
	if v, ok := top["success"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err == nil && !b {
			return smsFirstStringValue(raw, "message", "msg", "detail"), true
		}
	}
	// 顶层 status 非 0 即失败（与 code/ret 检查同构；先于成功判定被调用，
	// 见 verifySmsCodeZhipu 的调用顺序）。
	if n, ok := zhipuNumericField(top["status"]); ok && n != 0 {
		return fmt.Sprintf("status=%d", n), true
	}
	for _, key := range []string{"code", "ret"} {
		if n, ok := zhipuNumericField(top[key]); ok && n != 0 {
			return fmt.Sprintf("%s=%d", key, n), true
		}
	}
	// 嵌套错误体常见位置：data.code / data.status（数值或字符串数字，非 0 即失败）。
	if dataRaw, ok := top["data"]; ok {
		var data map[string]json.RawMessage
		if err := json.Unmarshal(dataRaw, &data); err == nil {
			for _, key := range []string{"code", "status"} {
				if n, ok := zhipuNumericField(data[key]); ok && n != 0 {
					return fmt.Sprintf("data.%s=%d", key, n), true
				}
			}
		}
	}
	return "", false
}

// zhipuSendBizFailure 发码专用失败判定：先查顶层 status 非 0（2026-09-22 发码
// 抓包响应使用 status 字段），再回退共享 zhipuBizFailure（status/code/ret/success
// + data 嵌套）。共享判定器已恢复顶层 status 非 0 检查，此处的先行检查成为
// 重复但无行为变化，保留以保证发码路径不依赖共享判定器实现顺序。
func zhipuSendBizFailure(raw []byte) (string, bool) {
	if len(bytes.TrimSpace(raw)) != 0 {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err == nil {
			if n, ok := zhipuNumericField(top["status"]); ok && n != 0 {
				return fmt.Sprintf("status=%d", n), true
			}
		}
	}
	return zhipuBizFailure(raw)
}

// zhipuNormalizePhoneCode 把国家码归一为抓包契约形态（2026-09-22 生产抓包：
// 发码与登录 body.phone_code 均为 "+86"，带加号）：去空白后，已带 "+" 前缀原样
// 返回，否则补 "+" 前缀（"86" → "+86"；空值原样返回空串）。发码与登录组包处
// 共用同一函数（单一口径）。
func zhipuNormalizePhoneCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || strings.HasPrefix(code, "+") {
		return code
	}
	return "+" + code
}

// zhipuNumericField 把 JSON 字段值宽容解析为数值：接受数值字面量与字符串数字
// （如 "403"），其余形状（null/bool/对象/字符串非数字）返回 not-ok，不算失败。
func zhipuNumericField(raw json.RawMessage) (int64, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		var n2 int64
		if _, err := fmt.Sscan(s, &n2); err == nil {
			return n2, true
		}
	}
	return 0, false
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
	countryCode, number := SplitSMSPhone(phone)
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
		// Connect 错误信封的真实原因在 details[0]，仅凭 status/json_keys 无法定位，
		// 故补 err_code 与 err_details（脱敏：不含 value 载荷与 debug 值）。
		errCode, errDetails := smsConnectErrorShape(raw)
		s.logger.Info("kimi 短信验证码发送 HTTP 错误诊断",
			"platform", PlatformKimi,
			"status", resp.StatusCode,
			"content_type", resp.Header.Get("Content-Type"),
			"body_bytes", len(raw),
			"json_keys", smsJSONShape(raw),
			"err_code", errCode,
			"err_details", errDetails,
		)
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
	countryCode, number := SplitSMSPhone(phone)
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
		bizMsg := smsFirstStringValue(raw, "message", "msg", "detail", "error", "error_message", "description")
		// 上游响应体无可读文案键（生产实证 has_biz_message=false），失败原因只存在于
		// Connect 错误信封的 details[0]，故补 err_code 与 err_details（脱敏形状）。
		errCode, errDetails := smsConnectErrorShape(raw)
		s.logger.Info("kimi 短信登录 HTTP 错误诊断",
			"platform", PlatformKimi,
			"status", resp.StatusCode,
			"content_type", resp.Header.Get("Content-Type"),
			"body_bytes", len(raw),
			"json_keys", smsJSONShape(raw),
			"has_biz_message", bizMsg != "",
			"err_code", errCode,
			"err_details", errDetails,
		)
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

// smsConnectUnparsed 用于 smsConnectErrorShape 的降级标记：响应体不是对象 / JSON 解析失败 /
// details 不是数组时，用它占位，保证诊断字段永不为 nil 也不 panic。
const smsConnectUnparsed = "<unparsed>"

// smsConnectErrorShape 从 Connect-Go 标准错误信封 {"code": "...", "details": [...]} 提取
// 定位失败原因所需的结构摘要，返回 (code, details)。
// 生产实证动机（镜像 e74fe85cf-w，2026-09-21）：kimi 上游 auth.kimi.com 的 HTTP 4xx 响应体
// 顶层只有 code + details 两个键（code 为 15 字符即 Connect 码 unauthenticated），既没有
// message/msg/detail/error 等可读文案键，smsJSONShape 也只能给出 details=array(len=1)。
// 真实原因只存在于 details[0]（Connect ErrorDetail 形态 {type, value, debug}），
// 只打长度等于仍然瞎，因此本函数把 details 展开到「类型 + 是否含 value + debug 键名」这一层。
//
// 零凭据红线：返回值**绝不**包含 details[].value 载荷（base64 proto，可能含手机号/验证码/token）
// 与 debug 的具体值；只输出 code 字符串（错误码枚举名，非凭据）、type 字符串、value 的存在性
// 布尔与 debug 的键名列表。
func smsConnectErrorShape(raw []byte) (string, string) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", smsConnectUnparsed
	}
	code := ""
	if v := bytes.TrimSpace(top["code"]); len(v) > 0 && v[0] == '"' {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			code = s
		}
	}
	detailsRaw, ok := top["details"]
	if !ok {
		return code, ""
	}
	var items []json.RawMessage
	if err := json.Unmarshal(detailsRaw, &items); err != nil {
		return code, smsConnectUnparsed
	}
	parts := make([]string, 0, len(items))
	for i, item := range items {
		parts = append(parts, fmt.Sprintf("#%d%s", i, smsConnectDetailShape(item)))
	}
	return code, strings.Join(parts, " ")
}

// smsConnectDetailShape 描述单个 Connect ErrorDetail 的形状，与 smsConnectErrorShape 同红线：
// 只输出 type 值、value 的存在性、debug 的键名，绝不输出 value 载荷与 debug 的值。
func smsConnectDetailShape(raw json.RawMessage) string {
	var detail map[string]json.RawMessage
	if err := json.Unmarshal(raw, &detail); err != nil {
		return "{<unparsed>}"
	}
	keys := make([]string, 0, len(detail))
	for k := range detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		switch k {
		case "value":
			// value 是 base64 编码的 proto 载荷，可能含手机号/验证码/token：只报存在性。
			parts = append(parts, "value_present=true")
		case "debug":
			parts = append(parts, "debug_keys="+smsDebugKeys(detail[k]))
		case "type":
			var s string
			if err := json.Unmarshal(detail[k], &s); err == nil {
				parts = append(parts, "type="+s)
				continue
			}
			parts = append(parts, "type=<non-string>")
		default:
			// 其余键（未知扩展字段）一律只报键名，避免值泄露。
			parts = append(parts, k+"=<omitted>")
		}
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// smsDebugKeys 返回 debug 子对象的键名列表（只留键名，不留值），非对象时降级。
func smsDebugKeys(raw json.RawMessage) string {
	var debug map[string]json.RawMessage
	if err := json.Unmarshal(raw, &debug); err != nil {
		return "<non-object>"
	}
	if len(debug) == 0 {
		return "<empty>"
	}
	keys := make([]string, 0, len(debug))
	for k := range debug {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
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

// SplitSMSPhone 拆分手机号为国家码 + 号码（kimi loginWithSMS 的 phone{countryCode,number}，
// 以及 zhipu 人工挑战 helper 的 phone_code）。支持 "86-138..." / "+86 138..." / 纯 11 位
// （默认 countryCode="86"）等形态。
// 证据未给 countryCode 推导规则，缺省按中国大陆 86 建模，待联调确认。
func SplitSMSPhone(phone string) (countryCode, number string) {
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

// logSMSHTTPStatusDiagnostics 非 2xx 最小脱敏诊断（Lane G，用户裁定 2026-09-22）：
// 仅记录 HTTP 状态码、json_keys 形状（smsJSONShape，键名/类型/长度）与同会话上下文
// 存在性布尔，绝不落原始响应体、Cookie、device_id 等任何凭据值（零凭据红线）。
// kimi 路径已有自带诊断（Connect 信封形状），本函数仅服务 zhipu 发码/登录路径。
func (s *WebPlatformAutoLoginService) logSMSHTTPStatusDiagnostics(platform, what string, status int, raw []byte, challenge WebSMSChallenge) {
	s.logger.Info(what+" HTTP 错误诊断",
		"platform", platform,
		"status", status,
		"json_keys", smsJSONShape(raw),
		"has_session_cookie", strings.TrimSpace(challenge.ZhipuSessionCookie) != "",
		"has_session_device_id", strings.TrimSpace(challenge.ZhipuDeviceID) != "",
	)
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
