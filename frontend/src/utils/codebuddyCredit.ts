/**
 * CodeBuddy 积分额度快照解析（纯函数）。
 *
 * 键名与后端 internal/service/codebuddy_quota_service.go 的 Extra 键逐一对应（Card A 存储契约）：
 * - codebuddy_credit_total_remaining：十进制字符串（NUMERIC(20,8)）→ 剩余合计；
 * - codebuddy_credit_total_total：同上 → 总量合计；
 * - codebuddy_credit_used_percent：JSON 数值（float8）→ 后端派生已用百分比；
 * - codebuddy_credit_has_excluded：JSON 布尔 → 是否有分包被排除（不计入合计）；
 * - codebuddy_credit_packages_updated_at：最近一次成功快照时间（RFC3339，唯一 freshness）；
 * - codebuddy_credit_reset_at：重置时间；
 * - codebuddy_credit_error：探测失败标记（§4.1 SSOT；旧错误键名已废弃）。
 *
 * 整改③范围 a / 整改②：逐包明细已删除，前端改读后端聚合键；
 * used_percent 纳入 status=3 由后端推导。本文件不再读取/解析任何逐包数组。
 *
 * Σtotal=0 等无有效快照场景：后端不写三键（remaining/total/used_percent 同缺）→ 与"无快照"
 * 同走空态，绝不渲染 0%（既有 spec 已钉）。聚合键缺失/形态不符按缺快照空态，不造 0（禁兜底）。
 *
 * 抽成纯函数是为了让两处读**同一套字段**、不各自解析后漂移：
 *   1) 母账号自己的额度块（AccountUsageCell.vue）；
 *   2) 影子行额外挂的「母账号」余额指示（AccountsView.vue，方案 N6）。
 * 影子账号没有自己的配额快照，因此任何情况下都不允许用本函数的结果去表示
 * 「影子自己的余额」。
 */
import Decimal from 'decimal.js'

export interface CodeBuddyCreditSnapshot {
  /** 剩余合计（可比合计），十进制字符串承载（聚合键 codebuddy_credit_total_remaining） */
  totalRemaining: string
  /** 总量合计，十进制字符串承载（聚合键 codebuddy_credit_total_total） */
  totalTotal: string
  /** 剩余合计展示值（2 位小数 ROUND_HALF_UP） */
  remainingDisplay: string
  /** 总量合计展示值（2 位小数 ROUND_HALF_UP） */
  totalDisplay: string
  /** 是否有分包被排除（不计入合计，UI 标注），来自 codebuddy_credit_has_excluded */
  hasExcluded: boolean
  /** 最后成功快照时间（codebuddy_credit_packages_updated_at），无则为 null */
  updatedAt: string | null
  /** 后端派生已用百分比（0-100+）；无快照时本函数返回 null（调用方走空态），
   *  解析到快照但 used_percent 缺/非法时取 0（真实快照下的缺字段，不冒充空态）。 */
  usedPercent: number
  /** 重置时间 */
  resetAt: string | null
  /** 摘要文案（余额条下方展示：`剩余 / 总量`） */
  summary: string
}

/** 字符串承载的 NUMERIC(20,8) 值解析（十进制安全，禁 float 换算）。
 *  聚合键 remaining/total 形态校验入口：非字符串或非法十进制视为契约不符（返回 null）。 */
function parseDecimalString(value: unknown): Decimal | null {
  if (typeof value !== 'string') {
    // 契约要求字符串承载；数值形态视为契约不符，不静默转换（禁兜底）。
    return null
  }
  const trimmed = value.trim()
  if (trimmed === '') return null
  try {
    return new Decimal(trimmed)
  } catch {
    return null
  }
}

/**
 * 后端写入口径：codebuddy_credit_used_percent 为 **JSON 数值**（float8，见
 * account_extra_conditional.go 的 to_jsonb($5::float8)）。展示仍走十进制安全——
 * 用 Decimal 构造避免二进制浮点参与格式化；used_percent 是单值展示，不参互聚合求和。
 * 非数值形态视为契约不符（禁兜底为 0）。
 */
function parseUsedPercent(value: unknown): Decimal | null {
  if (typeof value !== 'number' || !Number.isFinite(value)) return null
  return new Decimal(value)
}

/** 展示口径钉死：保留 2 位小数、ROUND_HALF_UP（Card F / R2#5 / R3#4）。
 *  用 toFixed 固定两位（toString 会丢尾随零，如 1200.00 → 1200）。 */
export function formatCreditValue(decimal: Decimal | string | null): string {
  if (decimal == null) return '0.00'
  const d = typeof decimal === 'string' ? new Decimal(decimal) : decimal
  return d.toFixed(2, Decimal.ROUND_HALF_UP)
}

/**
 * 解析母账号 extra 里的额度聚合快照；完全无快照时返回 null（调用方据此走空态）。
 *
 * 聚合三键（remaining/total/used_percent）由后端在同一次快照中同写同缺：
 * 聚合键缺失或形态不符（非字符串/非法十进制）→ 无快照空态，禁兜底为 0。
 */
export function parseCodeBuddyCredit(extra: unknown): CodeBuddyCreditSnapshot | null {
  if (!extra || typeof extra !== 'object') return null
  const e = extra as Record<string, unknown>

  // 聚合键读取（逐包明细已删除 — 整改③范围 a）。三键（remaining/total/used_percent）
  // 同写同缺：Σtotal=0 等无有效快照场景后端不写 → 此处即走空态，绝不造 0%。
  const remainingDec = parseDecimalString(e.codebuddy_credit_total_remaining)
  const totalDec = parseDecimalString(e.codebuddy_credit_total_total)
  if (remainingDec === null || totalDec === null) {
    // 聚合键缺失或形态不符（非字符串/非法十进制）→ 无快照空态，禁兜底。
    return null
  }

  const totalRemaining = remainingDec.toString()
  const totalTotal = totalDec.toString()

  const usedPercentRaw = parseUsedPercent(e.codebuddy_credit_used_percent)
  const resetAtStr = typeof e.codebuddy_credit_reset_at === 'string' ? e.codebuddy_credit_reset_at : ''
  const updatedAtStr = typeof e.codebuddy_credit_packages_updated_at === 'string' ? e.codebuddy_credit_packages_updated_at : ''
  const hasExcludedRaw = e.codebuddy_credit_has_excluded

  const remainingDisplay = formatCreditValue(totalRemaining)
  const totalDisplay = formatCreditValue(totalTotal)
  const summary = `${remainingDisplay} / ${totalDisplay}`

  return {
    totalRemaining,
    totalTotal,
    remainingDisplay,
    totalDisplay,
    hasExcluded: hasExcludedRaw === true,
    updatedAt: updatedAtStr.trim() !== '' ? updatedAtStr : null,
    usedPercent: usedPercentRaw ? usedPercentRaw.toNumber() : 0,
    resetAt: resetAtStr.trim() !== '' ? resetAtStr : null,
    summary,
  }
}

/**
 * 额度探测错误（后端 §4.1 键名 codebuddy_credit_error；旧错误键名已废弃）。
 * 前端读写切换与 Card E 后端迁移同一发布闸门（R8 权属裁定）。
 */
export function parseCodeBuddyCreditError(extra: unknown): string | null {
  if (!extra || typeof extra !== 'object') return null
  const raw = (extra as Record<string, unknown>).codebuddy_credit_error
  return typeof raw === 'string' && raw.trim() !== '' ? raw : null
}
