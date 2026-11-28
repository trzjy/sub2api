import { describe, expect, it } from 'vitest'
import zhAccounts from '../locales/zh/admin/accounts'
import enAccounts from '../locales/en/admin/accounts'

const zh = (zhAccounts as any).accounts.usageWindow
const en = (enAccounts as any).accounts.usageWindow

const devinKeys = [
  'devinDaily',
  'devinWeekly',
  'devinDailyTooltip',
  'devinWeeklyTooltip',
  'devinLocalState',
  'devinUpstreamState',
  'devinUpstreamAvailable',
  'devinUpstreamExhausted',
  'devinUpstreamUnknown',
  'devinQuotaExhausted',
  'devinQuotaExhaustedHint',
  'devinExhaustedReason',
  'devinExhaustedWindowUnknown',
  'devinWindowUnknown',
  'devinNoData',
  'devinQueryFailed',
  'devinFetchedAt',
  'devinStale',
  'devinExhaustedShort',
  'devinRemaining'
] as const

function placeholders(value: string): string[] {
  return [...value.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort()
}

describe('Devin 用量窗口中英文文案', () => {
  it('中英两侧文案齐全且非空', () => {
    for (const key of devinKeys) {
      expect(typeof zh[key], `zh.${key}`).toBe('string')
      expect(typeof en[key], `en.${key}`).toBe('string')
      expect(zh[key].trim().length, `zh.${key}`).toBeGreaterThan(0)
      expect(en[key].trim().length, `en.${key}`).toBeGreaterThan(0)
    }
  })

  it('窗口名称明确表达每日与每周两个独立窗口（不是 5h/7d）', () => {
    expect(zh.devinDaily).toContain('每日')
    expect(zh.devinWeekly).toContain('每周')
    expect(en.devinDaily).toMatch(/daily/i)
    expect(en.devinWeekly).toMatch(/weekly/i)
    expect(zh.devinDaily).not.toContain('5h')
    expect(en.devinWeekly).not.toContain('7d')
  })

  it('耗尽、查询失败、过期与 tooltip 文案表达正确语义', () => {
    expect(zh.devinQuotaExhausted).toContain('额度耗尽')
    expect(en.devinQuotaExhausted).toMatch(/quota exhausted/i)
    expect(zh.devinQueryFailed).toContain('查询失败')
    expect(en.devinQueryFailed).toMatch(/Query failed/i)
    expect(zh.devinStale).toContain('过期')
    expect(en.devinStale).toMatch(/stale/i)
    // tooltip 交代权威数据来源与「探测窗口 ≠ 恢复时间」
    expect(zh.devinDailyTooltip).toContain('GetUserStatus')
    expect(en.devinWeeklyTooltip).toMatch(/exhausted/i)
    expect(zh.devinQuotaExhaustedHint).toContain('下一次探测时间')
    expect(en.devinQuotaExhaustedHint).toMatch(/next probe time/i)
  })

  it('带占位符的文案在两侧集合一致', () => {
    const withParams = devinKeys.filter((key) => placeholders(zh[key]).length > 0)
    expect(withParams.length).toBeGreaterThan(0)
    for (const key of withParams) {
      expect(placeholders(zh[key]), key).toEqual(placeholders(en[key]))
    }
    expect(placeholders(zh.devinLocalState)).toEqual(['active', 'schedulable'])
    expect(placeholders(en.devinLocalState)).toEqual(['active', 'schedulable'])
    expect(placeholders(zh.devinRemaining)).toEqual(['percent'])
  })
})
