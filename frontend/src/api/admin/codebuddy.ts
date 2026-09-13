/**
 * Admin CodeBuddy API endpoints
 * Handles CodeBuddy (Tencent) OAuth device-flow for administrators.
 *
 * Unlike Antigravity (paste-code exchange), CodeBuddy uses a true polling loop:
 * the management UI opens the auth URL in a browser, then repeatedly calls
 * /oauth/poll until the upstream login completes. While login is pending the
 * backend responds with HTTP 400 + "登录未完成…", which the frontend must treat
 * as "keep polling" rather than a fatal error (see §9.8 of the plan).
 */

import { apiClient } from '../client'

/** CodeBuddy 站点：cn=国内版（copilot.tencent.com），intl=国际版（www.codebuddy.ai）。
 *  缺省 cn —— 与存量账号凭据无 site 键的后端行为一致。 */
export type CodeBuddySite = 'cn' | 'intl'

export interface CodeBuddyAuthUrlResponse {
  auth_url: string
  state: string
}

export interface CodeBuddyAuthUrlRequest {
  proxy_id?: number
  /** 站点；缺省 cn。选 intl 时 auth-url 指向 www.codebuddy.ai/login。 */
  site?: CodeBuddySite
}

export interface CodeBuddyPollRequest {
  state: string
  /** 登录选定的代理；须与 auth-url 一致，保证 poll 与登录同一出口 IP。 */
  proxy_id?: number
  /** 站点；必须与 auth-url 请求一致，否则 state 在另一站点不存在。 */
  site?: CodeBuddySite
}

export interface CodeBuddyRefreshTokenRequest {
  refresh_token: string
  uid?: string
  enterprise_id?: string
  domain?: string
  /** 账号绑定的代理；使 token 校验与日常调用同一出口 IP。 */
  proxy_id?: number
  /** 站点；缺省 cn。 */
  site?: CodeBuddySite
}

export interface CodeBuddyTokenInfo {
  access_token?: string
  refresh_token?: string
  expires_in?: number
  expires_at?: number | string
  domain?: string
  uid?: string
  enterprise_id?: string
  nickname?: string
  [key: string]: unknown
}

/** 生成授权链接（上游签发 state，无 PKCE）。 */
export async function generateAuthUrl(
  payload: CodeBuddyAuthUrlRequest
): Promise<CodeBuddyAuthUrlResponse> {
  const { data } = await apiClient.post<CodeBuddyAuthUrlResponse>(
    '/admin/codebuddy/oauth/auth-url',
    payload
  )
  return data
}

/** 轮询登录结果；pending 时后端返回 400 + "登录未完成…"，由调用方决定是否继续轮询。 */
export async function pollToken(
  payload: CodeBuddyPollRequest
): Promise<CodeBuddyTokenInfo> {
  const { data } = await apiClient.post<CodeBuddyTokenInfo>(
    '/admin/codebuddy/oauth/poll',
    payload
  )
  return data
}

/** 用 refresh token 刷新并返回完整 token 信息。 */
export async function refreshCodeBuddyToken(
  payload: CodeBuddyRefreshTokenRequest
): Promise<CodeBuddyTokenInfo> {
  const { data } = await apiClient.post<CodeBuddyTokenInfo>(
    '/admin/codebuddy/oauth/refresh-token',
    payload
  )
  return data
}

export default { generateAuthUrl, pollToken, refreshCodeBuddyToken }
