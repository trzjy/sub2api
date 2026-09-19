/**
 * Web platform auto-login API endpoints (frontend-facing contract).
 *
 * 对接后端 E6 定稿的网页平台自动登录接口。字段名以 E6 后端契约为准：
 * - web-login-password：仅 deepseek 使用 login_email；成功直接返回 cookie。
 *   zhipu/kimi 官方网页端无密码登录，后端对该分支返回失败（提示改用短信登录），
 *   前端不再等待短信发码阶段回传的会话令牌等桩字段。
 * - web-login-sms：zhipu / kimi 唯一用户入口，手机号 + 短信码双步骤：
 *   action=send_code → { success:true }（不回传会话令牌，E0 取证确认）；
 *   action=login      → { success, account_id, cookie?|access_token?, login_refresh_token? }。
 *   失败关闭（HTTP 400）由原样透传的拦截器平面错误承载 metadata.{ detail, hint, needs_challenge }。
 */

import { apiClient } from '../client'

/** 网页接入平台（与 credentialsBuilder.WEB_PROVIDER_PLATFORMS 对齐）。 */
export type WebAutoLoginPlatform = 'deepseek' | 'zhipu' | 'kimi'

export interface WebLoginPasswordRequest {
  platform: string
  /** deepseek 使用登录邮箱 */
  login_email?: string
  /** zhipu / kimi 使用手机号（后端对该平台返回失败，提示改用短信登录） */
  login_phone?: string
  login_password: string
  /** 已有账号重登回填时携带，新建时省略 */
  account_id?: number
}

export interface WebLoginPasswordResponse {
  success: boolean
  /** deepseek 成功时返回整串 Cookie */
  cookie?: string
  /** 业务级失败或细化错误文案 */
  detail?: string
}

/** 短信登录平台（仅 zhipu / kimi）。 */
export type WebLoginSmsPlatform = 'zhipu' | 'kimi'

export interface WebLoginSmsRequest {
  /** send_code（发码）| login（提交短信码） */
  action: 'send_code' | 'login'
  /** 仅 zhipu / kimi */
  platform: WebLoginSmsPlatform
  /** 手机号（必填） */
  phone: string
  /** 仅 action=login 时使用 */
  sms_code?: string
  /** 既有账号重登回填；省略则后端新建（access_mode=web，服务端再次校验平台凭证） */
  account_id?: number
  /** 新建时账号名；省略则后端自动生成 */
  name?: string
  /** 挑战求解值（后端内部主导挑战时前端无需提供，原样透传即可） */
  zhipu_captcha_rid?: string
  zhipu_captcha_md5?: string
  zhipu_phone_code?: string
  kimi_captcha_validate?: string
}

export interface WebLoginSmsResponse {
  success: boolean
  /** 登录成功时返回的账号 ID（后端新建或回填） */
  account_id?: number
  /** 仅 zhipu：整串 Cookie */
  cookie?: string
  /** 仅 kimi：访问令牌 */
  access_token?: string
  login_refresh_token?: string
  /** 业务级失败细化文案（按需） */
  detail?: string
}

/**
 * 账号密码自动登录（仅 deepseek 网页端支持）。
 * 后端成功响应为 { success, cookie? }（非标准 ApiResponse 包裹），axios 拦截器会原样透传，
 * 故 data 即业务体。zhipu/kimi 走 web-login-sms，不应调用本接口。
 */
export async function webLoginPassword(
  payload: WebLoginPasswordRequest
): Promise<WebLoginPasswordResponse> {
  const { data } = await apiClient.post<WebLoginPasswordResponse>(
    '/admin/accounts/web-login-password',
    payload
  )
  return data
}

/**
 * 短信验证码登录（zhipu / kimi 唯一用户入口）。
 * 后端成功响应为 { success, account_id, cookie?|access_token?, login_refresh_token? }
 * （非标准 ApiResponse 包裹），axios 拦截器原样透传，故 data 即业务体。
 * 失败关闭（HTTP 400）由拦截器以平面错误 reject：{ message, metadata:{ detail, hint, needs_challenge? } }。
 */
export async function webLoginSms(payload: WebLoginSmsRequest): Promise<WebLoginSmsResponse> {
  const { data } = await apiClient.post<WebLoginSmsResponse>('/admin/accounts/web-login-sms', payload)
  return data
}

export const webAutoLoginAPI = {
  webLoginPassword,
  webLoginSms
}

export default webAutoLoginAPI
