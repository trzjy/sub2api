/**
 * CodeBuddy 积分额度快照解析（纯函数）。
 *
 * 键名与后端 internal/service/codebuddy_quota_service.go 的 Extra 键逐一对应（Card A 存储契约）：
 * - codebuddy_credit_packages：分包快照数组，元素字段钉死为
 *   id/name/unit/remaining/total/expires_at/status；remaining/total 为**字符串**承载
 *   NUMERIC(20,8) 口径（decimal.js 序列化），禁直接将字符串转 JS number 参与求和。
 * - codebuddy_credit_packages_updated_at：最近一次成功快照时间（RFC3339，唯一 freshness）。
 * - codebuddy_credit_used_percent / codebuddy_credit_reset_at：后端派生/保留值。
 * - codebuddy_credit_error：探测失败标记（§4.1 SSOT；旧错误键名已废弃，
 *   Card E 迁移后端、Card F 切换前端读写）。
 *
 * 旧展示键（total / used / updated_at 三键）已退役（Card F），本文件不再读取。
 *
 * 抽成纯函数是为了让两处读**同一套字段**、不各自解析后漂移：
 *   1) 母账号自己的额度块（AccountUsageCell.vue）；
 *   2) 影子行额外挂的「母账号」余额指示（AccountsView.vue，方案 N6）。
 * 影子账号没有自己的配额快照，因此任何情况下都不允许用本函数的结果去表示
 * 「影子自己的余额」。
 */
import Decimal from 'decimal.js'

/** 分包快照单条（与后端 MarshalJSON 字段逐键对齐，remaining/total 为字符串）。 */
export interface CodeBuddyCreditPackage {
  id: string
  name: string
  unit: string
  remaining: string
  total: string
  expires_at: string
  status: number
}

/** 解析后的分包展示项（值为十进制安全的 Decimal，或由字符串格式化）。 */
export interface CodeBuddyCreditDisplayPackage {
  id: string
  name: string
  unit: string
  remaining: string
  total: string
  expires_at: string
  status: number
  /** 该分包是否计入合计（基础快照有效性谓词：unit==credits 且状态有效）。 */
  countable: boolean
  /** 异单位/不可数原因（用于 UI 标注），可数或无理由时为 null。 */
  excludedReason: string | null
}

export interface CodeBuddyCreditSnapshot {
  /** 分包列表（按后端数组顺序） */
  packages: CodeBuddyCreditDisplayPackage[]
  /** Σremaining（可比合计），十进制字符串承载；无可比分包时为 '0' */
  totalRemaining: string
  /** Σtotal（可比合计），十进制字符串承载；无可比分包时为 '0' */
  totalTotal: string
  /** 剩余合计展示值（2 位小数 ROUND_HALF_UP） */
  remainingDisplay: string
  /** 总量合计展示值（2 位小数 ROUND_HALF_UP） */
  totalDisplay: string
  /** 是否有异单位/不可比分包被排除（UI 标注） */
  hasExcluded: boolean
  /** 最后成功快照时间（codebuddy_credit_packages_updated_at），无则为 null */
  updatedAt: string | null
  /** 后端派生已用百分比（0-100+）；缺快照时 0（调用方据此走空态，不冒充真实值） */
  usedPercent: number
  /** 重置时间 */
  resetAt: string | null
  /** 摘要文案（余额条下方展示：`剩余 / 总量`） */
  summary: string
}

/** 后端规范单位（§4.1 addendum 钉死唯一合法值 credits）。异单位分包失败关闭（后端已剔除）。
 *  本层仅用于展示层合计的可比性标注——后端存储契约保证剩余的都是 credits，但为防
 *  契约漂移导致静默混加异单位，仍按单位过滤。 */
export const CODEBUDDY_UNIT_CREDITS = 'credits'

/** 后端合法 Status（§4.1）：0 = 在用（参与比重），3 = 耗尽/过期（不参与）。 */
export const CODEBUDDY_STATUS_ACTIVE = 0

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
 * 用 Decimal 构造避免二进制浮点参与格式化；used_percent 是单值展示，不参与分包求和。
 * 非数值形态视为契约不符（禁兜底为 0）。
 */
function parseUsedPercent(value: unknown): Decimal | null {
  if (typeof value !== 'number' || !Number.isFinite(value)) return null
  return new Decimal(value)
}

/** 字符串承载的 NUMERIC(20,8) 值求和（十进制安全，禁 float 换算）。 */
function sumDecimalStrings(values: string[]): Decimal {
  return values.reduce((acc, v) => acc.plus(v), new Decimal(0))
}

/** 展示口径钉死：保留 2 位小数、ROUND_HALF_UP（Card F / R2#5 / R3#4）。
 *  用 toFixed 固定两位（toString 会丢尾随零，如 1200.00 → 1200）。 */
export function formatCreditValue(decimal: Decimal | string | null): string {
  if (decimal == null) return '0.00'
  const d = typeof decimal === 'string' ? new Decimal(decimal) : decimal
  return d.toFixed(2, Decimal.ROUND_HALF_UP)
}

/** 解析单条分包；契约形态不符返回 null（禁兜底为 0）。 */
function parsePackage(raw: unknown): CodeBuddyCreditDisplayPackage | null {
  if (!raw || typeof raw !== 'object') return null
  const p = raw as Record<string, unknown>
  const id = typeof p.id === 'string' ? p.id : ''
  const name = typeof p.name === 'string' ? p.name : ''
  const unit = typeof p.unit === 'string' ? p.unit : ''
  const remaining = typeof p.remaining === 'string' && p.remaining.trim() !== '' ? p.remaining : null
  const total = typeof p.total === 'string' && p.total.trim() !== '' ? p.total : null
  const expiresAt = typeof p.expires_at === 'string' ? p.expires_at : ''
  const status = typeof p.status === 'number' ? p.status : Number.NaN

  if (remaining === null || total === null) {
    // 数值字段必须是字符串承载（契约不符 → 本包不可解析，展示错误态而非静默当 0）。
    return null
  }
  const remainingDec = parseDecimalString(remaining)
  const totalDec = parseDecimalString(total)
  if (remainingDec === null || totalDec === null) return null
  if (remainingDec.isNegative() || totalDec.isNegative()) return null
  if (remainingDec.greaterThan(totalDec)) return null

  // 基础快照有效性谓词（与后端 Card A 一致，展示侧仅作只读标注，不做二次校验剔除。
  // 契约保证存储的分包均已通过后端校验；此处单位/状态用于「合计不计入」标注）。
  const countable = unit === CODEBUDDY_UNIT_CREDITS && status === CODEBUDDY_STATUS_ACTIVE
  const excludedReason = countable
    ? null
    : unit !== CODEBUDDY_UNIT_CREDITS
      ? 'unit_mismatch'
      : 'status_inactive'

  return {
    id,
    name,
    unit,
    remaining,
    total,
    expires_at: expiresAt,
    status,
    countable,
    excludedReason,
  }
}

/**
 * 解析母账号 extra 里的分包额度快照；完全无快照时返回 null（调用方据此走空态）。
 */
export function parseCodeBuddyCredit(extra: unknown): CodeBuddyCreditSnapshot | null {
  if (!extra || typeof extra !== 'object') return null
  const e = extra as Record<string, unknown>

  const packagesRaw = e.codebuddy_credit_packages
  if (!Array.isArray(packagesRaw)) {
    // 无分包快照（可能只有失败错误标记）→ 无快照，调用方走空态。
    return null
  }

  const packages: CodeBuddyCreditDisplayPackage[] = []
  for (const raw of packagesRaw) {
    const parsed = parsePackage(raw)
    if (parsed) packages.push(parsed)
  }

  // 展示不可解析的分包（契约不符）：不静默丢弃也不当 0，置错误标记由调用方展示。
  const parseFailure = packagesRaw.length > packages.length

  // 不可数分包：后端存储契约已保证 unit==credits、Status 合法（Card A 失败关闭）；
  // 但展示侧「合计」只统计 Status=0 的在用包（与后端基础有效谓词同一口径）。
  const countable = packages.filter((p) => p.countable)
  const totalRemaining = countable.length > 0
    ? sumDecimalStrings(countable.map((p) => p.remaining)).toString()
    : '0'
  const totalTotal = countable.length > 0
    ? sumDecimalStrings(countable.map((p) => p.total)).toString()
    : '0'

  const usedPercentRaw = parseUsedPercent(e.codebuddy_credit_used_percent)
  const resetAtStr = typeof e.codebuddy_credit_reset_at === 'string' ? e.codebuddy_credit_reset_at : ''
  const updatedAtStr = typeof e.codebuddy_credit_packages_updated_at === 'string' ? e.codebuddy_credit_packages_updated_at : ''

  const remainingDisplay = formatCreditValue(totalRemaining)
  const totalDisplay = formatCreditValue(totalTotal)
  const summary = `${remainingDisplay} / ${totalDisplay}`

  return {
    packages,
    totalRemaining,
    totalTotal,
    remainingDisplay,
    totalDisplay,
    hasExcluded: packages.some((p) => !p.countable) || parseFailure || usedPercentRaw === null,
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