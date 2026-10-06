import { describe, expect, it } from 'vitest'
import type { OpsErrorDetail } from '@/api/admin/ops'
import { resolvePrimaryResponseBody, resolveUpstreamPayload } from '../errorDetailResponse'

function makeDetail(overrides: Partial<OpsErrorDetail>): OpsErrorDetail {
  return {
    id: 1,
    created_at: '2026-01-01T00:00:00Z',
    phase: 'request',
    type: 'api_error',
    error_owner: 'platform',
    error_source: 'gateway',
    severity: 'P2',
    status_code: 502,
    platform: 'openai',
    model: 'gpt-4o-mini',
    resolved: false,
    client_request_id: 'crid-1',
    request_id: 'rid-1',
    message: 'Upstream request failed',
    user_email: 'user@example.com',
    account_name: 'acc',
    group_name: 'group',
    error_body: '',
    user_agent: '',
    is_business_limited: false,
    ...overrides
  }
}

describe('errorDetailResponse', () => {
  it('prefers upstream payload for request modal when error_body is generic gateway wrapper', () => {
    const detail = makeDetail({
      error_body: JSON.stringify({
        type: 'error',
        error: {
          type: 'upstream_error',
          message: 'Upstream request failed'
        }
      }),
      upstream_error_detail: '{"provider_message":"real upstream detail"}'
    })

    expect(resolvePrimaryResponseBody(detail, 'request')).toBe('{"provider_message":"real upstream detail"}')
  })

  it('prefers upstream payload for new Chinese generic gateway wrappers', () => {
    // backend/internal/pkg/errors/downstream_messages.go 上游渠道侧中文常量。
    const chineseGenericMessages = [
      '上游渠道触发限速，请稍后重试',
      '上游服务过载，请稍后重试',
      '上游服务暂时不可用，请稍后重试',
      '上游请求失败，请稍后重试',
      '上游渠道认证失败，请联系管理员',
      '上游渠道拒绝访问，请联系管理员',
      '上游渠道余额不足或计费异常，请联系管理员'
    ]

    for (const message of chineseGenericMessages) {
      const detail = makeDetail({
        error_body: JSON.stringify({
          type: 'error',
          error: {
            type: 'upstream_error',
            message
          }
        }),
        upstream_error_detail: '{"provider_message":"real upstream detail"}'
      })

      expect(resolvePrimaryResponseBody(detail, 'request')).toBe('{"provider_message":"real upstream detail"}')
    }
  })

  it('keeps error_body for request modal when Chinese body is not a generic wrapper', () => {
    const detail = makeDetail({
      error_body: JSON.stringify({
        type: 'error',
        error: {
          type: 'upstream_error',
          message: '上游账号并发等待超时，请稍后重试'
        }
      }),
      upstream_error_detail: '{"provider_message":"real upstream detail"}'
    })

    expect(resolvePrimaryResponseBody(detail, 'request')).toBe(detail.error_body)
  })

  it('keeps error_body for request modal when body is not generic wrapper', () => {
    const detail = makeDetail({
      error_body: JSON.stringify({
        type: 'error',
        error: {
          type: 'upstream_error',
          message: 'Upstream authentication failed, please contact administrator'
        }
      }),
      upstream_error_detail: '{"provider_message":"real upstream detail"}'
    })

    expect(resolvePrimaryResponseBody(detail, 'request')).toBe(detail.error_body)
  })

  it('uses upstream payload first in upstream modal', () => {
    const detail = makeDetail({
      phase: 'upstream',
      upstream_error_message: 'provider 503 overloaded',
      error_body: '{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}'
    })

    expect(resolvePrimaryResponseBody(detail, 'upstream')).toBe('provider 503 overloaded')
  })

  it('falls back to upstream payload when request error_body is empty', () => {
    const detail = makeDetail({
      error_body: '',
      upstream_error_message: 'dial tcp timeout'
    })

    expect(resolvePrimaryResponseBody(detail, 'request')).toBe('dial tcp timeout')
  })

  it('resolves upstream payload by detail -> events -> message priority', () => {
    expect(resolveUpstreamPayload(makeDetail({
      upstream_error_detail: 'detail payload',
      upstream_errors: '[{"message":"event payload"}]',
      upstream_error_message: 'message payload'
    }))).toBe('detail payload')

    expect(resolveUpstreamPayload(makeDetail({
      upstream_error_detail: '',
      upstream_errors: '[{"message":"event payload"}]',
      upstream_error_message: 'message payload'
    }))).toBe('[{"message":"event payload"}]')

    expect(resolveUpstreamPayload(makeDetail({
      upstream_error_detail: '',
      upstream_errors: '',
      upstream_error_message: 'message payload'
    }))).toBe('message payload')
  })

  it('treats empty JSON placeholders in upstream payload as empty', () => {
    expect(resolveUpstreamPayload(makeDetail({
      upstream_error_detail: '',
      upstream_errors: '[]',
      upstream_error_message: ''
    }))).toBe('')

    expect(resolveUpstreamPayload(makeDetail({
      upstream_error_detail: '',
      upstream_errors: '{}',
      upstream_error_message: ''
    }))).toBe('')

    expect(resolveUpstreamPayload(makeDetail({
      upstream_error_detail: '',
      upstream_errors: 'null',
      upstream_error_message: ''
    }))).toBe('')
  })

  it('skips placeholder candidates and falls back to the next upstream field', () => {
    expect(resolveUpstreamPayload(makeDetail({
      upstream_error_detail: '',
      upstream_errors: '[]',
      upstream_error_message: 'fallback message'
    }))).toBe('fallback message')

    expect(resolveUpstreamPayload(makeDetail({
      upstream_error_detail: 'null',
      upstream_errors: '',
      upstream_error_message: 'fallback message'
    }))).toBe('fallback message')
  })
})
