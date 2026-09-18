/**
 * Web platform auto-login API endpoints (frontend-facing contract).
 *
 * 对接后端并行任务实现的网页平台自动登录接口。字段名以任务说明为准：
 * - web-login-password：deepseek 用 login_email；zhipu/kimi 用 login_phone。
 *   deepseek 成功直接返回 cookie；zhipu/kimi 返回 needs_sms + session_token。
 * - web-login-sms：本期返回 { success:false, detail:"短信码登录尚未接入发码通道" }，
 *   前端必须如实展示该细化错误，不得伪造成功。
 * - batch-*：批量登录 / 测试 / 删除封禁 / 设置状态。
 * - export：按 platform 导出的网页账号清单（JSON 下载）。
 */

import { apiClient } from '../client'

/** 网页接入平台（与 credentialsBuilder.WEB_PROVIDER_PLATFORMS 对齐）。 */
export type WebAutoLoginPlatform = 'deepseek' | 'zhipu' | 'kimi'

export interface WebLoginPasswordRequest {
  platform: string
  /** deepseek 使用登录邮箱 */
  login_email?: string
  /** zhipu / kimi 使用手机号 */
  login_phone?: string
  login_password: string
  /** 已有账号重登回填时携带，新建时省略 */
  account_id?: number
}

export interface WebLoginPasswordResponse {
  success: boolean
  /** deepseek 成功时返回整串 Cookie */
  cookie?: string
  /** zhipu / kimi 需要短信验证 */
  needs_sms?: boolean
  /** needs_sms 时返回的会话令牌，用于 web-login-sms */
  session_token?: string
  /** 业务级失败或细化错误文案（如短信通道未接入） */
  detail?: string
}

export interface WebLoginSmsRequest {
  session_token: string
  sms_code: string
}

export interface WebLoginSmsResponse {
  success: boolean
  /** 验证成功时返回登录态（Cookie / Token） */
  cookie?: string
  /** 细化错误文案 */
  detail?: string
}

export interface BatchLoginResult {
  id: number
  recovered: boolean
  needs_sms: boolean
  detail?: string
}

export interface BatchLoginResponse {
  results: BatchLoginResult[]
  summary: { success: number; failed: number }
}

export interface BatchTestResult {
  id: number
  success: boolean
  error?: string
}

export interface BatchTestResponse {
  results: BatchTestResult[]
}

export interface BannedCandidate {
  id: number
  name: string
  reason: string
}

export interface BatchDeleteBannedResponse {
  /** 先取清单（confirm=false）或实际删除后（confirm=true）的候选列表 */
  candidates: BannedCandidate[]
  /** 实际删除数量（confirm=false 时为 0） */
  deleted: number
}

export interface BatchStatusRequest {
  ids: number[]
  status: 'active' | 'inactive'
}

export interface BatchStatusResult {
  account_id: number
  success: boolean
  error?: string
}

export interface BatchStatusResponse {
  success: number
  failed: number
  results?: BatchStatusResult[]
}

/**
 * 账号密码自动登录（网页平台）。
 * 后端成功响应为 { success, cookie? | needs_sms?, session_token? }（非标准
 * ApiResponse 包裹），axios 拦截器会原样透传，故 data 即业务体。
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

/** 短信验证码登录（本期短信发码通道未接入，后端返回 success:false + detail）。 */
export async function webLoginSms(payload: WebLoginSmsRequest): Promise<WebLoginSmsResponse> {
  const { data } = await apiClient.post<WebLoginSmsResponse>('/admin/accounts/web-login-sms', payload)
  return data
}

/** 批量自动登录。 */
export async function batchLogin(ids: number[]): Promise<BatchLoginResponse> {
  const { data } = await apiClient.post<BatchLoginResponse>('/admin/accounts/batch-login', { ids })
  return data
}

/** 批量连通性测试。 */
export async function batchTest(ids: number[]): Promise<BatchTestResponse> {
  const { data } = await apiClient.post<BatchTestResponse>('/admin/accounts/batch-test', { ids })
  return data
}

/**
 * 批量删除封禁账号。两步流程：先 confirm=false 取候选清单展示给用户确认，
 * 用户确认后再 confirm=true 执行删除。
 */
export async function batchDeleteBanned(confirm = false): Promise<BatchDeleteBannedResponse> {
  const { data } = await apiClient.post<BatchDeleteBannedResponse>('/admin/accounts/batch-delete-banned', {
    confirm
  })
  return data
}

/** 批量设置账号状态（启用 / 停用）。 */
export async function batchStatus(payload: BatchStatusRequest): Promise<BatchStatusResponse> {
  const { data } = await apiClient.post<BatchStatusResponse>('/admin/accounts/batch-status', payload)
  return data
}

/**
 * 按平台导出网页账号清单（JSON）。返回 Blob 由调用方触发下载。
 */
export async function exportWebAccounts(platform: string): Promise<Blob> {
  const response = await apiClient.get('/admin/accounts/export', {
    params: { platform },
    responseType: 'blob'
  })
  return response.data as Blob
}

export const webAutoLoginAPI = {
  webLoginPassword,
  webLoginSms,
  batchLogin,
  batchTest,
  batchDeleteBanned,
  batchStatus,
  exportWebAccounts
}

export default webAutoLoginAPI
