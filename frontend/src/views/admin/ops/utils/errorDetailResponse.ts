import type { OpsErrorDetail } from '@/api/admin/ops'

// 通用包装错误 message 全文匹配表（精确匹配，大小写不敏感）。
// 英文条目：存量 ops 记录仍为英文，保留。
// 中文条目：backend/internal/pkg/errors/downstream_messages.go 上游渠道侧常量，
// 后端下游文案中文化后新数据只可能出现中文，需保持启发式命中。
const GENERIC_UPSTREAM_MESSAGES = new Set([
  'upstream request failed',
  'upstream request failed after retries',
  'upstream gateway error',
  'upstream service temporarily unavailable',
  '上游渠道触发限速，请稍后重试',
  '上游服务过载，请稍后重试',
  '上游服务暂时不可用，请稍后重试',
  '上游请求失败，请稍后重试',
  '上游渠道认证失败，请联系管理员',
  '上游渠道拒绝访问，请联系管理员',
  '上游渠道余额不足或计费异常，请联系管理员'
])

type ParsedGatewayError = {
  type: string
  message: string
}

function parseGatewayErrorBody(raw: string): ParsedGatewayError | null {
  const text = String(raw || '').trim()
  if (!text) return null

  try {
    const parsed = JSON.parse(text) as Record<string, any>
    const err = parsed?.error as Record<string, any> | undefined
    if (!err || typeof err !== 'object') return null

    const type = typeof err.type === 'string' ? err.type.trim() : ''
    const message = typeof err.message === 'string' ? err.message.trim() : ''
    if (!type && !message) return null

    return { type, message }
  } catch {
    return null
  }
}

function isGenericGatewayUpstreamError(raw: string): boolean {
  const parsed = parseGatewayErrorBody(raw)
  if (!parsed) return false
  if (parsed.type !== 'upstream_error') return false
  return GENERIC_UPSTREAM_MESSAGES.has(parsed.message.toLowerCase())
}

export function resolveUpstreamPayload(
  detail: Pick<OpsErrorDetail, 'upstream_error_detail' | 'upstream_errors' | 'upstream_error_message'> | null | undefined
): string {
  if (!detail) return ''

  const candidates = [
    detail.upstream_error_detail,
    detail.upstream_errors,
    detail.upstream_error_message
  ]

  for (const candidate of candidates) {
    const payload = String(candidate || '').trim()
    if (!payload) continue

    // Normalize common "empty but present" JSON placeholders.
    if (payload === '[]' || payload === '{}' || payload.toLowerCase() === 'null') {
      continue
    }

    return payload
  }

  return ''
}

export function resolvePrimaryResponseBody(
  detail: OpsErrorDetail | null,
  errorType?: 'request' | 'upstream'
): string {
  if (!detail) return ''

  const upstreamPayload = resolveUpstreamPayload(detail)
  const errorBody = String(detail.error_body || '').trim()

  if (errorType === 'upstream') {
    return upstreamPayload || errorBody
  }

  if (!errorBody) {
    return upstreamPayload
  }

  // For request detail modal, keep client-visible body by default.
  // But if that body is a generic gateway wrapper, show upstream payload first.
  if (upstreamPayload && isGenericGatewayUpstreamError(errorBody)) {
    return upstreamPayload
  }

  return errorBody
}
