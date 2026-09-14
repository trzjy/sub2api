import { describe, expect, it } from 'vitest'

import { parseCodeBuddyCredit, parseCodeBuddyCreditError } from '../codebuddyCredit'

// 方案 N6：母账号额度快照的键名/语义以本文件为单一真源
// （AccountUsageCell 的母账号额度块与 AccountsView 的影子行「母账号余额」指示共用）。
describe('parseCodeBuddyCredit', () => {
  it('解析完整快照：利用率 / 周期截止 / 已用-总量摘要', () => {
    expect(
      parseCodeBuddyCredit({
        codebuddy_credit_used_percent: 37,
        codebuddy_credit_used: 370,
        codebuddy_credit_total: 1000,
        codebuddy_credit_reset_at: '2026-09-16T00:00:00Z',
        codebuddy_credit_updated_at: '2026-09-15T00:00:00Z',
      }),
    ).toEqual({
      usedPercent: 37,
      resetAt: '2026-09-16T00:00:00Z',
      summary: '370 / 1.0K',
    })
  })

  it('字符串数值一并接受（后端 Extra 为 JSON，可能序列化为字符串）', () => {
    expect(parseCodeBuddyCredit({ codebuddy_credit_used_percent: '42' })?.usedPercent).toBe(42)
  })

  it('只有更新时间时利用率按 0 展示、摘要退化为更新时间', () => {
    expect(parseCodeBuddyCredit({ codebuddy_credit_updated_at: '2026-09-15T00:00:00Z' })).toEqual({
      usedPercent: 0,
      resetAt: null,
      summary: '2026-09-15T00:00:00Z',
    })
  })

  it('完全无快照返回 null，调用方据此走空态（不冒充 0%）', () => {
    expect(parseCodeBuddyCredit(undefined)).toBeNull()
    expect(parseCodeBuddyCredit(null)).toBeNull()
    expect(parseCodeBuddyCredit({})).toBeNull()
    expect(parseCodeBuddyCredit({ unrelated: 1 })).toBeNull()
    // 只有探测错误、没有快照时也是「无快照」
    expect(parseCodeBuddyCredit({ codebuddy_quota_error: '上游 500' })).toBeNull()
  })
})

describe('parseCodeBuddyCreditError', () => {
  it('读取后端实际写入的 codebuddy_quota_error', () => {
    expect(parseCodeBuddyCreditError({ codebuddy_quota_error: '上游 500' })).toBe('上游 500')
  })

  it('旧键名 codebuddy_credit_error 不再驱动 UI（后端从未写过该键）', () => {
    expect(parseCodeBuddyCreditError({ codebuddy_credit_error: '旧键' })).toBeNull()
  })

  it('空串/空白视为无错误', () => {
    expect(parseCodeBuddyCreditError({ codebuddy_quota_error: '   ' })).toBeNull()
    expect(parseCodeBuddyCreditError({ codebuddy_quota_error: '' })).toBeNull()
    expect(parseCodeBuddyCreditError(undefined)).toBeNull()
  })
})
