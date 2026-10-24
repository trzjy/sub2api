import { describe, expect, it } from 'vitest'
import { resolveExpiryDisplay } from '@/utils/format'

// resolveExpiryDisplay 是到期展示的纯函数（不持有 i18n），本用例锁死其分支语义：
// - null/undefined → unknown（上游未给出，"不等于永不过期"）
// - 空串 → unknown（激活码不存在永不过期形态，见 2026-10-24 裁定）
// - 有效日期 → date（含格式化文本）
describe('resolveExpiryDisplay 到期时间展示解析（纯函数）', () => {
  it('null → unknown（上游未给出到期时间，绝不等于永不过期）', () => {
    expect(resolveExpiryDisplay(null)).toEqual({ status: 'unknown' })
  })

  it('undefined → unknown', () => {
    expect(resolveExpiryDisplay(undefined)).toEqual({ status: 'unknown' })
  })

  it('空串 → unknown（激活码不存在永不过期形态，见 2026-10-24 裁定）', () => {
    expect(resolveExpiryDisplay('')).toEqual({ status: 'unknown' })
  })

  it('有效 RFC3339 串 → date（含格式化文本）', () => {
    const r = resolveExpiryDisplay('2026-12-31T00:00:00Z')
    expect(r.status).toBe('date')
    expect(typeof r.text).toBe('string')
    expect(r.text.length).toBeGreaterThan(0)
  })
})
