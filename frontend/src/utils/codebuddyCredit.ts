/**
 * CodeBuddy 积分额度快照解析（纯函数）。
 *
 * 键名与后端 internal/service/codebuddy_quota_service.go 的 Extra 键逐一对应：
 * 探测成功整组写入数值/时间快照，探测失败只写 codebuddy_quota_error。
 *
 * 抽成纯函数是为了让两处读**同一套字段**、不各自解析后漂移：
 *   1) 母账号自己的额度块（AccountUsageCell.vue）；
 *   2) 影子行额外挂的「母账号」余额指示（AccountsView.vue，方案 N6）。
 * 影子账号没有自己的配额快照，因此任何情况下都不允许用本函数的结果去表示
 * 「影子自己的余额」。
 */
import { formatCompactNumber } from './format'

export interface CodeBuddyCreditSnapshot {
  /** 已用百分比（0-100+）；缺快照时为 0，不冒充真实值 */
  usedPercent: number
  resetAt: string | null
  /** `已用 / 总量`（或更新时间）摘要，供余额条下方展示 */
  summary: string
}

function toNum(v: unknown): number | null {
  if (typeof v === 'number') return Number.isFinite(v) ? v : null
  if (typeof v === 'string' && v.trim() !== '') {
    const n = Number(v)
    return Number.isFinite(n) ? n : null
  }
  return null
}

/** 解析母账号 extra 里的额度快照；完全无快照时返回 null（调用方据此走空态）。 */
export function parseCodeBuddyCredit(extra: unknown): CodeBuddyCreditSnapshot | null {
  if (!extra || typeof extra !== 'object') return null
  const e = extra as Record<string, unknown>
  const usedPercent = toNum(e.codebuddy_credit_used_percent)
  const total = toNum(e.codebuddy_credit_total)
  const used = toNum(e.codebuddy_credit_used)
  const resetAt = typeof e.codebuddy_credit_reset_at === 'string' ? e.codebuddy_credit_reset_at : null
  const updatedAt = typeof e.codebuddy_credit_updated_at === 'string' ? e.codebuddy_credit_updated_at : null
  if (usedPercent === null && total === null && used === null && !resetAt && !updatedAt) {
    return null
  }
  let summary = ''
  if (total != null && used != null) {
    summary = `${formatCompactNumber(used)} / ${formatCompactNumber(total)}`
  } else if (updatedAt) {
    summary = updatedAt
  }
  return {
    usedPercent: usedPercent ?? 0,
    resetAt,
    summary,
  }
}

/**
 * 额度探测错误（后端 codebuddy_quota_error）。
 * 此前前端读的是 codebuddy_credit_error —— 与后端键名不一致，导致探测失败态永不显示。
 */
export function parseCodeBuddyCreditError(extra: unknown): string | null {
  if (!extra || typeof extra !== 'object') return null
  const raw = (extra as Record<string, unknown>).codebuddy_quota_error
  return typeof raw === 'string' && raw.trim() !== '' ? raw : null
}
