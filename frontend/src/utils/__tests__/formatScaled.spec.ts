import { describe, expect, it } from 'vitest'
import { formatScaled } from '@/utils/pricing'

describe('formatScaled', () => {
  it('≥$0.1 固定 2 位小数（历史口径，不剥尾零）', () => {
    expect(formatScaled(3e-6, 1_000_000)).toBe('$3.00')
    expect(formatScaled(0.2, 1)).toBe('$0.20')
    expect(formatScaled(0.12426470588235294e-6, 1_000_000)).toBe('$0.12')
    expect(formatScaled(45, 1)).toBe('$45.00')
    expect(formatScaled(0, 1)).toBe('$0.00')
  })

  it('<$0.1 保留 2 位有效数字，极小价格不失真', () => {
    expect(formatScaled(1.863970588e-8, 1_000_000)).toBe('$0.019')
    expect(formatScaled(9.2e-9, 1_000_000)).toBe('$0.0092')
    expect(formatScaled(9.18e-11, 1_000_000)).toBe('$0.000092')
    // 恰为 2 位小数可表示的值保持原样
    expect(formatScaled(2e-8, 1_000_000)).toBe('$0.02')
  })

  it('minFractionDigits 只补齐不截断', () => {
    expect(formatScaled(3e-6, 1_000_000, 2)).toBe('$3.00')
    expect(formatScaled(0.5, 1, 2)).toBe('$0.50')
    expect(formatScaled(0.000003, 1_000_000, 2)).toBe('$3.00')
  })

  it('null 显示为 -', () => {
    expect(formatScaled(null, 1_000_000)).toBe('-')
  })
})
