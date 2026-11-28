import { describe, expect, it } from 'vitest'
import zhAccounts from '../locales/zh/admin/accounts'
import enAccounts from '../locales/en/admin/accounts'

const zh = (zhAccounts as any).accounts.usageWindow
const en = (enAccounts as any).accounts.usageWindow

const genericKeys = [
  'remaining',
  'exhausted',
  'upstreamQuotaExhausted',
  'upstreamQuotaExhaustedHint',
  'exhaustedWindows',
  'windowDataUnavailable',
  'queryFailed',
  'fetchedAt',
  'stale'
] as const

function placeholders(value: string): string[] {
  return [...value.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort()
}

describe('Devin 用量窗口中文映射（复用通用订阅文案）', () => {
  it('窗口标签把上游 Daily/Weekly 翻成中文，且不改成 5h/7d', () => {
    expect(zh.windows.daily).toBe('每日')
    expect(zh.windows.weekly).toBe('每周')
    expect(en.windows.daily).toBe('Daily')
    expect(en.windows.weekly).toBe('Weekly')
    expect(zh.windows.daily).not.toContain('5h')
    expect(zh.windows.weekly).not.toContain('7d')
  })

  it('通用文案两侧齐全、非空且占位符一致', () => {
    for (const key of genericKeys) {
      expect(typeof zh[key], `zh.${key}`).toBe('string')
      expect(typeof en[key], `en.${key}`).toBe('string')
      expect(zh[key].trim().length, `zh.${key}`).toBeGreaterThan(0)
      expect(en[key].trim().length, `en.${key}`).toBeGreaterThan(0)
      expect(placeholders(zh[key]), key).toEqual(placeholders(en[key]))
    }
    expect(placeholders(zh.remaining)).toEqual(['percent'])
    expect(placeholders(zh.exhaustedWindows)).toEqual(['windows'])
    expect(placeholders(zh.queryFailed)).toEqual(['reason'])
    expect(placeholders(zh.fetchedAt)).toEqual(['time'])
  })

  it('已耗尽与查询失败语义明确', () => {
    expect(zh.upstreamQuotaExhausted).toContain('额度耗尽')
    expect(en.upstreamQuotaExhausted).toMatch(/quota exhausted/i)
    expect(zh.queryFailed).toContain('查询失败')
    expect(en.queryFailed).toMatch(/Query failed/i)
    expect(zh.stale).toContain('过期')
    expect(en.stale).toMatch(/stale/i)
  })

  it('旧的 Devin 专用文案键已全部移除', () => {
    const legacyKeys = [
      'devinDaily', 'devinWeekly', 'devinDailyTooltip', 'devinWeeklyTooltip',
      'devinLocalState', 'devinUpstreamState', 'devinUpstreamAvailable',
      'devinUpstreamExhausted', 'devinUpstreamUnknown', 'devinQuotaExhausted',
      'devinQuotaExhaustedHint', 'devinExhaustedReason', 'devinExhaustedWindowUnknown',
      'devinWindowUnknown', 'devinNoData', 'devinQueryFailed', 'devinFetchedAt',
      'devinStale', 'devinExhaustedShort', 'devinRemaining'
    ]
    for (const key of legacyKeys) {
      expect(Object.prototype.hasOwnProperty.call(zh, key), `zh.${key}`).toBe(false)
      expect(Object.prototype.hasOwnProperty.call(en, key), `en.${key}`).toBe(false)
    }
  })
})
