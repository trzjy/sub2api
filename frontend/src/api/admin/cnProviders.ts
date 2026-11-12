/**
 * Admin CN providers (Kimi / Zhipu / DeepSeek) API endpoints.
 * Coding-plan rolling-window quota probe + payg balance probe.
 */

import { apiClient } from '../client'

/** 滚动用量窗口档（5 小时 / 每周 / 每月 / 每日），对齐后端 service.CNQuotaTier。 */
export interface CNQuotaTier {
  window: '5h' | 'weekly' | 'monthly' | 'daily'
  // used_percent 为 null 表示上游不可得（如火山周窗口：限流头仅含 5h，周额度无可靠来源），
  // 前端渲染为“—/未知”而非假 0。
  used_percent: number | null
  reset_at?: string
}

/**
 * TokenHarbor 订阅快照（账号 extra 键 `th_pass_snapshot`，方案 §4.3 契约，
 * 字段名与后端 service.TokenHarborPassSnapshot 逐字段对齐，前端按此冻结键名消费）。
 */
export interface THPassSnapshot {
  provider?: string
  has_pass: boolean
  /** Pass 档位名（如 “Agent Pass”）；缺失时上游页面未渲染出该字段。 */
  pass_name?: string
  /** 订阅续期日（≈28 天，订阅链；额度周期见 reset_at）。 */
  renews_at?: string
  /** 官方 7 天周期重置时刻（RFC3339）；徽标与 7 天窗口倒计时据此展示（D-QLM-008）。 */
  reset_at?: string
  /** 官方计费周期天数（如 7）；窗口切换与重置文案依据。 */
  window_days?: number
  /** 官方 Pass 津贴已用百分比（0-100，截断展示）；津贴进度条数据源。 */
  plan_used_pct?: number
  /** 官方 Pass 津贴是否耗尽（硬上限 exhausted 状态）。 */
  plan_exhausted?: boolean
  /** 官方账单页 spendAfterAllowance（false = Pass 额度是硬上限）。 */
  spend_after_allowance?: boolean
  auto_reload_enabled?: boolean
  /** 快照抓取时刻（RFC3339）。 */
  fetched_at?: string
}

/** TokenHarbor 官方 CSV 聚合的单窗口用量计数（请求数 + 输入/输出 tokens）。 */
export interface THUsageWindowStats {
  requests: number
  tokens_in: number
  tokens_out: number
}

/**
 * TokenHarbor 用量快照（账号 extra 键 `th_usage_snapshot`，方案 §4.3 契约：
 * 官方 /api/usage/export.csv 按今天 / 7D / 30D 窗口聚合，三键齐全）。
 * TH 官方无分母字段，只显示已用计数（L6：不做剩余估算）。
 */
export interface THUsageSnapshot {
  windows: {
    today: THUsageWindowStats
    '7d': THUsageWindowStats
  }
  /** 免费津贴已用百分比（0-100）；D-QLM-008 契约预留，组件未渲染则不新增条。 */
  used_pct?: number
  /** 免费津贴是否耗尽。 */
  exhausted?: boolean
  /** 快照聚合时刻（RFC3339）。 */
  fetched_at?: string
}

/**
 * Kira（kiraai.vn）用量快照（账号 extra 键 `kira_usage_snapshot`，方案 §4.3 契约，
 * 字段名与后端 queryKiraUsageForAccount 落库 snapshot 逐字段对齐）。
 */
export interface KiraUsageSnapshot {
  /** 固定 'daily'：Kira 免费池每日重置（站点按越南时区）。 */
  window: 'daily'
  used_percent?: number | null
  /** 当日已用 tokens。 */
  used_tokens?: number
  /** 当日免费上限 tokens（官方分母）。 */
  limit_tokens?: number
  /** 每日重置时刻（RFC3339）；上游未给出时为空串，前端不伪造倒计时。 */
  reset_at?: string
  /** 快照抓取时刻（RFC3339）。 */
  fetched_at?: string
}

/** 额度探测结果（Coding Plan / OpenCode Go / Command Code），对齐后端 CNProviderQuotaProbeResult。 */
export interface CNProviderQuotaProbeResult {
  provider: string
  source?: string
  success: boolean
  credential_valid: boolean
  tiers?: CNQuotaTier[]
  plan_level?: string
  status_code?: number
  fetched_at: number
  persisted: boolean
  error?: string
  /** 同一次探测得到的余额（Command Code 积分与窗口同源），其余供应商为空。 */
  balance?: CNProviderBalanceResult
}

/** 单币种余额明细（deepseek 双币种账号含 CNY + USD 两条）。 */
export interface CNProviderBalanceEntry {
  currency: string
  balance: number
}

/** payg 余额探测结果（kimi / deepseek），对齐后端 CNProviderBalanceResult。 */
export interface CNProviderBalanceResult {
  provider: string
  success: boolean
  /** 主币种余额（balances 首条，兼容单币种展示）。 */
  balance: number
  currency?: string
  /** 多币种明细；缺省时按主币种展示。 */
  balances?: CNProviderBalanceEntry[]
  available: boolean
  /** 同程序中转订阅制不限量（remaining<0）：无数字余额，展示 plan_name。 */
  unlimited?: boolean
  /**
   * 计费形态三态（对齐后端 CNProviderBalanceResult.Kind，缺省按 'balance' 处理）：
   * - 'subscription'：订阅不限量（上游 remaining<0 + mode=unrestricted），没有数字余额，
   *   展示 plan_name；等价于既有 unlimited 布尔为真的情形。
   * - 'count_pack'：次卡（上游 mode=quota_limited），balance 字段装的是「剩余次数」
   *   而非金额。此时后端保证 currency 为空串、balances 为空数组——因为上游 unit
   *   字段在次卡下也误标 "USD"（已实证 quota.used 与 usage.today.requests 逐位相等），
   *   不可信，故前端必须走次卡专属文案、不得复用带币种前缀的金额格式化。
   * - 'balance'：金额余额（官方 kimi/deepseek 账号）。
   * 旧快照 / 官方原生路径未升级时该字段缺省，消费方按 'balance' 回落。
   */
  kind?: 'subscription' | 'count_pack' | 'balance'
  /** 次卡总量（单位：次）。仅 'count_pack' 形态下发，非次卡缺省或为 0；缺省按 0 处理。 */
  quota_limit?: number
  /** 次卡已用（单位：次）。仅 'count_pack' 形态下发，非次卡缺省或为 0；缺省按 0 处理。 */
  quota_used?: number
  /** 上游订阅分组名（如「DeepSeek 订阅」）。 */
  plan_name?: string
  /** 上游订阅/额度到期时间（RFC3339）。次卡同样适用（卡有效期）。 */
  expires_at?: string
  /** 订阅制账号当日/当月用量（USD）；非订阅账号缺省。 */
  daily_usage?: number
  monthly_usage?: number
  status_code?: number
  fetched_at: number
  persisted: boolean
  error?: string
  /**
   * 上游订阅已到期状态标记，对齐后端 SubscriptionExpired（Go: SubscriptionExpired bool,
   * json:"subscription_expired,omitempty"）。仅在「上游订阅已到期」这一账号状态上有值
   * （true）；其余成功 / 失败分支缺省（JSON 里不出现该键）。命中时前端渲染「订阅已到期」
   * 状态标签而非红色报错行（这是账号状态，不是探测故障）。
   */
  subscription_expired?: boolean
}

/** 查询 Coding Plan 滚动窗口用量（5h + weekly）。 */
export async function queryQuota(id: number): Promise<CNProviderQuotaProbeResult> {
  const { data } = await apiClient.get<CNProviderQuotaProbeResult>(
    `/admin/cn-providers/accounts/${id}/quota`
  )
  return data
}

/** 查询 payg 账号余额。 */
export async function queryBalance(id: number): Promise<CNProviderBalanceResult> {
  const { data } = await apiClient.get<CNProviderBalanceResult>(
    `/admin/cn-providers/accounts/${id}/balance`
  )
  return data
}

/** Web 控制台额度探测结果（对齐后端 service.WebConsoleQuotaProbeResult）。 */
export interface WebConsoleQuotaProbeResult {
  provider: string
  success: boolean
  auth_state: 'ok' | 'needs_interactive_auth' | 'auth_failed' | 'error'
  auth_reason?: string
  auth_error?: string
  exhausted: boolean
  until?: string
  persisted: boolean
  fetched_at: number
  error?: string
  snapshot?: import('@/types').WebConsoleQuotaSnapshot
}

/** 手动触发一次 Web 控制台额度探测。 */
export async function probeWebConsoleQuota(id: number): Promise<WebConsoleQuotaProbeResult> {
  const { data } = await apiClient.post<WebConsoleQuotaProbeResult>(
    `/admin/accounts/${id}/web-console-quota/probe`
  )
  return data
}

/** 发起本地浏览器交互登录（需要在用户工作站上完成滑块/扫码）。 */
export async function startWebConsoleAuth(id: number): Promise<{ session_id: string; expires_in: number }> {
  const { data } = await apiClient.post<{ session_id: string; expires_in: number }>(
    `/admin/accounts/${id}/web-console-auth/start`
  )
  return data
}

/** 轮询交互登录进展；成功后后端会自动刷新快照并返回最新结果。 */
export async function pollWebConsoleAuth(
  id: number,
  sessionId: string
): Promise<{ status: 'pending' | 'succeeded' | 'failed'; probe?: WebConsoleQuotaProbeResult; message?: string }> {
  const { data } = await apiClient.get<{
    status: 'pending' | 'succeeded' | 'failed'
    probe?: WebConsoleQuotaProbeResult
    message?: string
  }>(`/admin/accounts/${id}/web-console-auth/${sessionId}`)
  return data
}

export default {
  queryQuota,
  queryBalance,
  probeWebConsoleQuota,
  startWebConsoleAuth,
  pollWebConsoleAuth,
  startWebConsoleLogin,
  getWebConsoleLoginFrame,
  postWebConsoleLoginAction,
  finishWebConsoleLogin,
  cancelWebConsoleLogin
}

// ---------------------------------------------------------------------------
// 服务端登录会话（扫码 / 短信 / 滑块）
//
// 与 startWebConsoleAuth（本机 helper 弹窗）并存：本组接口把登录页搬到服务端，
// 管理端网页直接显示画面并转发点击/输入，用户不需要在自己电脑上装任何工具。
// ---------------------------------------------------------------------------

/** 页面上的一个可见输入框（worker 现场枚举得到，不是写死的站点结构）。 */
export interface WebConsoleLoginField {
  index: number
  /** tel / number / text / password ... */
  type: string
  /** 优先取 placeholder，其次是 aria-label / name；保底「输入框 N」。 */
  label: string
  filled: boolean
  masked: boolean
}

/** 页面上的一个可点按钮。 */
export interface WebConsoleLoginButton {
  index: number
  text: string
}

/** 一帧登录画面（后端 service.WebConsoleRemoteLoginFrame）。 */
export interface WebConsoleLoginFrame {
  session_id: string
  /** pending = 还在登录页；authenticated = 已进入控制台，可点「完成」落库。 */
  status: 'pending' | 'authenticated'
  /**
   * 页面生命周期，与 status 是两个独立事实：
   * opening = 服务端正在打开登录页；ready = 可操作；failed = 打不开。
   * 打开登录页要几十秒，所以 start 只建会话立即返回，进度靠这个字段给。
   */
  phase?: 'opening' | 'ready' | 'failed'
  /** phase=failed 时的原因码（仅供界面显示人话，不要拿来做逻辑判断）。 */
  message?: string
  /** base64 JPEG（不带 data: 前缀）。 */
  screenshot?: string
  url?: string
  viewport_width: number
  viewport_height: number
  expires_in: number
  /** 当前页面上的输入框与按钮，用来渲染真表单（避免让用户点缩略图坐标）。 */
  fields?: WebConsoleLoginField[]
  buttons?: WebConsoleLoginButton[]
  /** 仅 fill / click 返回：动作是否真的落到了目标元素上。 */
  applied?: boolean
}

/** 远程操作动作：坐标点击 / 打字 / 按键 / 滚动 / 重载 / 按字段填写 / 按文字点按钮。 */
export type WebConsoleLoginAction =
  | { kind: 'tap'; x: number; y: number }
  | { kind: 'type'; text: string }
  | { kind: 'key'; key: string }
  | { kind: 'scroll'; delta_y: number }
  | { kind: 'reload' }
  | { kind: 'fill'; index: number; label: string; text: string }
  | { kind: 'click'; index: number; text: string }
  | { kind: 'toggle'; index: number; label: string }

/** 打开一扇服务端登录画面。 */
export async function startWebConsoleLogin(id: number): Promise<WebConsoleLoginFrame> {
  const { data } = await apiClient.post<WebConsoleLoginFrame>(
    `/admin/accounts/${id}/web-console-login/start`
  )
  return data
}

/** 拉取最新画面（前端定时轮询）。 */
export async function getWebConsoleLoginFrame(
  id: number,
  sessionId: string
): Promise<WebConsoleLoginFrame> {
  const { data } = await apiClient.get<WebConsoleLoginFrame>(
    `/admin/accounts/${id}/web-console-login/${sessionId}/frame`
  )
  return data
}

/** 把点击 / 输入 / 回车 / 滚动转发到登录页面。
 *
 * fill / click 会让 worker 回传刷新后的整帧，直接拿去更新表单，不必等下一次轮询；
 * 其他动作没有回帧，返回 null。
 */
export async function postWebConsoleLoginAction(
  id: number,
  sessionId: string,
  action: WebConsoleLoginAction
): Promise<WebConsoleLoginFrame | null> {
  const { data } = await apiClient.post<WebConsoleLoginFrame | { ok: boolean }>(
    `/admin/accounts/${id}/web-console-login/${sessionId}/action`,
    action
  )
  if (data && typeof data === 'object' && 'session_id' in data) {
    return data as WebConsoleLoginFrame
  }
  return null
}

/** 登录完成后取回票据并立即探测；未登录时返回 status=pending。 */
export async function finishWebConsoleLogin(
  id: number,
  sessionId: string
): Promise<{
  status: 'succeeded' | 'pending'
  probe?: WebConsoleQuotaProbeResult
  account_label?: string
  message?: string
}> {
  const { data } = await apiClient.post<{
    status: 'succeeded' | 'pending'
    probe?: WebConsoleQuotaProbeResult
    account_label?: string
    message?: string
  }>(`/admin/accounts/${id}/web-console-login/${sessionId}/finish`)
  return data
}

/** 取消登录并释放服务端浏览器。 */
export async function cancelWebConsoleLogin(id: number, sessionId: string): Promise<void> {
  await apiClient.delete(`/admin/accounts/${id}/web-console-login/${sessionId}`)
}
