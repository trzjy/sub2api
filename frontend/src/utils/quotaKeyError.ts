/**
 * 激活码“最近错误”展示格式化工具。
 *
 * 边界（见派发单 QKERR-A）：
 * - 仅服务管理页“最近错误”列/详情的展示文案，不修改后端落库、DB 值、
 *   isCooldownRefreshError、上游错误分类或 API 响应消息。
 * - 内部 `shareai:` 错误链前缀只在展示层剥离（不写回 DB），支持嵌套前缀。
 * - 上游固定格式刷新上限消息压缩为中文短句，文案走现有 i18n 约定
 *   `admin.quotaKeys.errorText.refreshLimitedHours` / `...refreshLimitedMinutes`，
 *   不在此处硬编码中英文。
 */

import { i18n } from '@/i18n'

type TranslateFn = (key: string, params?: Record<string, unknown>) => string

// 形如 `shareai: <ASCII 前缀>: ` 的内部错误链前缀，循环剥离以支持嵌套。
// `<ASCII 前缀>` 为非中文字符串（可含空格），其后紧跟 `: ` 作为分隔。
const SHAREAI_PREFIX_RE = /^shareai:\s*[\x20-\x7E]*?:\s*/i

// 上游固定格式（括号可为半角/全角，小时/分钟可为 0）：
// 当日刷新次数达到上限(5次)，将在每日8:00重置，还需7小时0分钟
const REFRESH_LIMIT_RE =
  /当日刷新次数达到上限[（(](\d+)次[)）]，将在每日8:00重置，还需(\d+)小时(\d+)分钟/

const HOURS_KEY = 'admin.quotaKeys.errorText.refreshLimitedHours'
const MINUTES_KEY = 'admin.quotaKeys.errorText.refreshLimitedMinutes'

/**
 * 在展示层循环剥掉 `shareai: <ASCII 前缀>: ` 前缀（支持嵌套）。
 * 只剥离符合该形状的前缀；其余内容原样返回。
 */
function stripShareAiPrefix(raw: string): string {
  let s = raw
  // 非贪婪 `[\x20-\x7E]*?` 保证每次只消费到第一个 `: `，从而逐层剥离嵌套前缀。
  while (SHAREAI_PREFIX_RE.test(s)) {
    const next = s.replace(SHAREAI_PREFIX_RE, '')
    if (next === s) break // 防御：避免理论上无法推进时的死循环
    s = next
  }
  return s
}

/**
 * 将激活码“最近错误”原始文本格式化为管理页展示文案。
 *
 * 规则：
 * 1. 空值 / 空白返回空串。
 * 2. 剥离内部 `shareai:` 错误链前缀（嵌套亦可）。
 * 3. 剥离后若匹配上游刷新上限固定格式，则按 i18n 文案输出：
 *    - 小时 > 0 用 refreshLimitedHours（约 N 小时后重置）
 *    - 小时为 0 用 refreshLimitedMinutes（约 N 分钟后重置）
 * 4. 不匹配固定格式时，返回剥离前缀后的原文（不编造、不替换为空/成功文案）。
 *
 * @param raw - 后端 last_refresh_error 原始文本
 * @param t   - 翻译函数；缺省时复用现有 i18n 访问方式（i18n.global.t），
 *              调用方也可传入自身的 t 以获得最终展示字符串。
 */
export function formatQuotaKeyError(
  raw: string | null | undefined,
  t: TranslateFn = (key, params) => i18n.global.t(key, params ?? {}),
): string {
  if (raw == null) return ''
  const cleaned = stripShareAiPrefix(raw)
  // 剥离前缀后若整体为空 / 仅空白，返回空串。
  if (cleaned.trim().length === 0) return ''

  const match = REFRESH_LIMIT_RE.exec(cleaned)
  if (match) {
    const count = Number(match[1])
    const hours = Number(match[2])
    const minutes = Number(match[3])
    if (hours > 0) {
      return t(HOURS_KEY, { count, hours })
    }
    return t(MINUTES_KEY, { count, minutes })
  }

  // 非匹配：返回剥离前缀后的原文，不做任何编造或替换。
  return cleaned
}
