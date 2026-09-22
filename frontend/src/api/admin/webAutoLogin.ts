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

/**
 * DeepSeek 邮箱注册接口（契约：docs/deepseek-email-register-plan.md §1.2）。
 * 成功响应为原始 JSON（不包裹 code/message/data）；失败关闭（HTTP 4xx）由拦截器
 * 以平面错误 reject：{ message, metadata:{ detail, ... } }。
 */

export interface WebRegisterEmailCodeResponse {
  success: boolean
  /** 发送窗口秒数：窗口内不允许重复发码（缺省 60s，由 UI 兜底） */
  send_window_secs?: number
}

export interface WebRegisterRequest {
  platform: string
  email: string
  email_verification_code: string
  password: string
  /** 与既有 webLoginPassword 的 account_draft 同型 */
  account_draft: CreateAccountRequest
}

export interface WebRegisterResponse {
  success: boolean
  /** 注册并建号成功时返回账号 ID */
  account_id?: number
}

/** 发送注册验证码（POST /admin/accounts/web-register-email-code）。
 * proxyId 非空时随请求传 proxy_id：外审 R3-P4，发码与注册/登录同口径走草稿指定代理。 */
export async function webRegisterEmailCode(
  platform: string,
  email: string,
  proxyId?: number | null
): Promise<WebRegisterEmailCodeResponse> {
  const { data } = await apiClient.post<WebRegisterEmailCodeResponse>(
    '/admin/accounts/web-register-email-code',
    { platform, email, ...(proxyId != null && proxyId !== 0 ? { proxy_id: proxyId } : {}) },
    { headers: { 'Idempotency-Key': newWebRegisterIdempotencyKey('email-code') } }
  )
  return data
}

/**
 * 邮箱验证码注册并创建账号（POST /admin/accounts/web-register）。
 *
 * 幂等键语义（外审 2026-09-22 R2-P2）：键由调用方通过 options.idempotencyKey 显式
 * 注入并在「结果不明」的重试间复用——若服务端已注册建号但响应在浏览器侧丢失，同键
 * 重试会重放首次结果（success/account_id 或 F4/409 终态），而不是再打上游收到
 * EMAIL_EXISTS。调用方（表单）负责：开始一次新注册操作时生成新键，请求返回明确结果
 * （成功或后端明确业务终态响应）后废弃，仅在结果不明（网络错误/超时/5xx）时复用。
 */
export async function webRegister(
  payload: WebRegisterRequest,
  options?: { idempotencyKey?: string }
): Promise<WebRegisterResponse> {
  const key = options?.idempotencyKey ?? newWebRegisterIdempotencyKey('register')
  const { data } = await apiClient.post<WebRegisterResponse>('/admin/accounts/web-register', payload, {
    headers: { 'Idempotency-Key': key }
  })
  return data
}

/**
 * 生成注册链幂等键（外审 2026-09-22 P1-1：后端 executeAdminIdempotent RequireKey=true，
 * 缺键请求直接 400，且响应丢失后无法重放）。每次用户操作生成新键；同一次调用的
 * axios 层重试共享同一键（键在调用开始时生成一次）。
 */
export function newWebRegisterIdempotencyKey(operation: 'email-code' | 'register'): string {
  const requestID = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`
  return `web-register-${operation}-${requestID}`
}

export const webAutoLoginAPI = {
  webLoginPassword,
  startWebLoginChallenge,
  getWebLoginChallengeStatus,
  consumeWebLoginChallenge,
  webLoginSms,
  webRegisterEmailCode,
  webRegister
}

export default webAutoLoginAPI
