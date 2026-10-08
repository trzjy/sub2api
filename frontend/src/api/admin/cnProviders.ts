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

/** Coding Plan 额度探测结果（kimi / zhipu），对齐后端 CNProviderQuotaProbeResult。 */
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
  /** 上游订阅分组名（如「DeepSeek 订阅」）。 */
  plan_name?: string
  /** 上游订阅/额度到期时间（RFC3339）。 */
  expires_at?: string
  /** 订阅制账号当日/当月用量（USD）；非订阅账号缺省。 */
  daily_usage?: number
  monthly_usage?: number
  status_code?: number
  fetched_at: number
  persisted: boolean
  error?: string
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

export default {
  queryQuota,
  queryBalance
}
