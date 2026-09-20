/**
 * Web platform auto-login API endpoints (frontend-facing contract).
 *
 * 对接后端 E6 定稿的网页平台自动登录接口。字段名以 E6 后端契约为准：
 * - web-login-password：仅 deepseek 使用 login_email；成功返回 account_id。
 *   zhipu/kimi 官方网页端无密码登录，后端对该分支返回失败（提示改用短信登录）。
 * - web-login-sms：zhipu / kimi 唯一用户入口，手机号 + 短信码双步骤：
 *   action=send_code → { success:true }；action=login → { success, account_id }。
 *   登录成功返回原始 JSON（顶层仅含 success/account_id）；失败关闭（HTTP 400）由原样透传的拦截器平面错误承载 metadata.{ detail, hint, needs_challenge }。
 */

import { apiClient } from '../client'
import type { CreateAccountRequest } from '@/types'

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
  /** 新建账号草稿；已有 account_id 重登时省略 */
  account_draft?: CreateAccountRequest
}

export interface WebLoginPasswordResponse {
  success: boolean
  /** 登录成功时返回账号 ID */
  account_id?: number
  /** 业务级失败或细化错误文案 */
  detail?: string
}

/** 短信登录平台（仅 zhipu / kimi）。 */
export type WebLoginSmsPlatform = 'zhipu' | 'kimi'

export interface WebLoginChallengeRequest {
  platform: WebLoginSmsPlatform
  phone: string
  stage?: string
  account_id?: number
  account_draft?: CreateAccountRequest
}

export type WebLoginChallengeStatus = 'pending' | 'succeeded' | 'failed' | 'expired' | 'context_gap' | 'consumed'

export interface WebLoginChallengeResponse {
  success: boolean
  session_id?: string
  status?: WebLoginChallengeStatus
  detail?: string
}

export interface WebLoginSmsRequest {
  /** send_code（发码）| login（提交短信码） */
  action: 'send_code' | 'login'
  /** 仅 zhipu / kimi */
  platform: WebLoginSmsPlatform
  /** 手机号（必填） */
  phone: string
  /** 仅 action=login 时使用 */
  sms_code?: string
  /** 人工挑战会话 opaque ID；永不传挑战明文 */
  challenge_session_id?: string
}

export interface WebLoginSmsResponse {
  success: boolean
  /** 登录成功时返回的账号 ID（后端新建或回填） */
  account_id?: number
  /** 业务级失败细化文案（按需） */
  detail?: string
}

/**
 * 账号密码自动登录（仅 deepseek 网页端支持）。
 * 后端成功响应为原始 JSON { success, account_id }（不包裹 code/message/data）。
 * zhipu/kimi 走 web-login-sms，不应调用本接口。
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
 * 后端成功响应为原始 JSON { success, account_id }（不包裹 code/message/data）。
 * 失败关闭（HTTP 400）由拦截器以平面错误 reject：{ message, metadata:{ detail, hint, needs_challenge? } }。
 */
export async function startWebLoginChallenge(payload: WebLoginChallengeRequest): Promise<WebLoginChallengeResponse> {
  const { data } = await apiClient.post<WebLoginChallengeResponse>(
    '/admin/accounts/web-login-challenge/start',
    payload
  )
  return data
}

export async function getWebLoginChallengeStatus(sessionId: string): Promise<WebLoginChallengeResponse> {
  const { data } = await apiClient.get<WebLoginChallengeResponse>(
    `/admin/accounts/web-login-challenge/${encodeURIComponent(sessionId)}/status`
  )
  return data
}

export async function consumeWebLoginChallenge(
  sessionId: string,
  payload: WebLoginChallengeRequest
): Promise<WebLoginChallengeResponse> {
  const { data } = await apiClient.post<WebLoginChallengeResponse>(
    `/admin/accounts/web-login-challenge/${encodeURIComponent(sessionId)}/consume`,
    payload
  )
  return data
}

export async function webLoginSms(payload: WebLoginSmsRequest): Promise<WebLoginSmsResponse> {
  const { data } = await apiClient.post<WebLoginSmsResponse>('/admin/accounts/web-login-sms', payload)
  return data
}

export const webAutoLoginAPI = {
  webLoginPassword,
  startWebLoginChallenge,
  getWebLoginChallengeStatus,
  consumeWebLoginChallenge,
  webLoginSms
}

export default webAutoLoginAPI
