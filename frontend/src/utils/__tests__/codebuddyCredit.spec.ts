import { describe, expect, it } from 'vitest'
import Decimal from 'decimal.js'

import { parseCodeBuddyCredit, parseCodeBuddyCreditError, formatCreditValue } from '../codebuddyCredit'

// CB-SUM-FE-R1：前端改读后端聚合键（逐包明细已删除）。
//   - 读侧改读 codebuddy_credit_total_remaining / total_total / used_percent /
//     has_excluded / packages_updated_at / reset_at / error；
//   - 逐包明细数组读侧全链归零；
//   - Σtotal=0 场景后端不写三键 → 与"无快照"同走空态，绝不渲染 0%。
describe('parseCodeBuddyCredit — 聚合键读取', () => {
  it('正常聚合快照：读三键派生合计与利用率（20,8 口径）', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_total_remaining: '399.75000000',
      codebuddy_credit_total_total: '1200.00000000',
      codebuddy_credit_packages_updated_at: '2026-09-29T10:00:00Z',
      codebuddy_credit_used_percent: 20.725,
      codebuddy_credit_reset_at: '2026-10-01T00:00:00Z',
    })

    expect(snap).not.toBeNull()
    // 400.75000000 = 300.5 + 99.25 已在后端聚合，前端直接读聚合值（十进制精确，无 float 误差）
    expect(snap!.totalRemaining).toBe('399.75')
    expect(snap!.totalTotal).toBe('1200')
    expect(snap!.remainingDisplay).toBe('399.75')
    expect(snap!.totalDisplay).toBe('1200.00')
    expect(snap!.summary).toBe('399.75 / 1200.00')
    expect(snap!.hasExcluded).toBe(false)
    expect(snap!.updatedAt).toBe('2026-09-29T10:00:00Z')
    expect(snap!.resetAt).toBe('2026-10-01T00:00:00Z')
    expect(snap!.usedPercent).toBeCloseTo(20.725, 6)
  })

  it('页面示例形态：remaining=600、total=2500、used_percent=76（生产账号 264 快照）', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_total_remaining: '600.00000000',
      codebuddy_credit_total_total: '2500.00000000',
      codebuddy_credit_used_percent: 76,
      codebuddy_credit_packages_updated_at: '2026-10-19T02:33:35.593+08:00',
    })
    expect(snap).not.toBeNull()
    expect(snap!.totalRemaining).toBe('600')
    expect(snap!.totalTotal).toBe('2500')
    expect(snap!.remainingDisplay).toBe('600.00')
    expect(snap!.totalDisplay).toBe('2500.00')
    expect(snap!.summary).toBe('600.00 / 2500.00')
    expect(snap!.usedPercent).toBe(76)
    // (2500-600)/2500 = 76%，前端展示百分比与剩余/总额口径一致
    expect((2500 - 600) / 2500 * 100).toBeCloseTo(snap!.usedPercent, 6)
  })

  it('has_excluded=true 表示后端有分包未计入合计', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_total_remaining: '10.00000000',
      codebuddy_credit_total_total: '20.00000000',
      codebuddy_credit_has_excluded: true,
    })
    expect(snap!.hasExcluded).toBe(true)
  })

  it('has_excluded 缺省为 false（不臆造排除标注）', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_total_remaining: '10.00000000',
      codebuddy_credit_total_total: '20.00000000',
    })
    expect(snap!.hasExcluded).toBe(false)
  })

  it('完全无快照返回 null（不冒充 0%，调用方走空态）', () => {
    expect(parseCodeBuddyCredit(undefined)).toBeNull()
    expect(parseCodeBuddyCredit(null)).toBeNull()
    expect(parseCodeBuddyCredit({})).toBeNull()
    expect(parseCodeBuddyCredit({ unrelated: 1 })).toBeNull()
    // 只有错误标记，没有三键 → 仍是无快照
    expect(parseCodeBuddyCredit({ codebuddy_credit_error: '上游 500' })).toBeNull()
    // 仅 used_percent 而无 totals → 视为无快照（三键同写同缺，绝不造 0%）
    expect(parseCodeBuddyCredit({ codebuddy_credit_used_percent: 30 })).toBeNull()
  })

  it('聚合键合计跨越 20,8 边界十进制精确（无 float 漂移）', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_total_remaining: '0.10000000',
      codebuddy_credit_total_total: '0.20000000',
    })
    // 0.1+0.2=0.3 已由后端聚合；前端直接读，验证不丢精度
    expect(snap!.totalRemaining).toBe('0.1')
    expect(snap!.remainingDisplay).toBe('0.10')
    expect(snap!.totalTotal).toBe('0.2')
    expect(snap!.totalDisplay).toBe('0.20')
  })

  it('大数（20,8 上限量级）不丢精度', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_total_remaining: '99999999999999.99999999',
      codebuddy_credit_total_total: '100000000000000.00000000',
    })
    // 聚合值原样保留为十进制字符串（若误转 JS number 会漂移成 100000000000000）
    expect(snap!.totalRemaining).toBe('99999999999999.99999999')
    // formatCreditValue 仍用 Decimal 处理大数：.99999999 → 进位到 100000000000000.00
    expect(snap!.remainingDisplay).toBe('100000000000000.00')
  })

  describe('codebuddy_credit_used_percent — 后端写入口径为 JSON 数值（float8）', () => {
    it('数值形态正常解析并十进制安全格式化（禁 float 参与）', () => {
      const snap = parseCodeBuddyCredit({
        codebuddy_credit_total_remaining: '300.50000000',
        codebuddy_credit_total_total: '1000.00000000',
        codebuddy_credit_used_percent: 30.05,
      })
      expect(snap!.usedPercent).toBeCloseTo(30.05, 6)
    })

    it('后端真实快照形态：数值 used_percent（端到端契约）', () => {
      const snap = parseCodeBuddyCredit({
        codebuddy_credit_total_remaining: '630.00000000',
        codebuddy_credit_total_total: '1000.00000000',
        codebuddy_credit_packages_updated_at: '2026-09-29T10:00:00Z',
        codebuddy_credit_used_percent: 37,
        codebuddy_credit_reset_at: '2026-09-16T00:00:00Z',
      })
      expect(snap).not.toBeNull()
      expect(snap!.totalRemaining).toBe('630')
      expect(snap!.remainingDisplay).toBe('630.00')
      expect(snap!.totalDisplay).toBe('1000.00')
      // 数值契约：used_percent 按 number 消费
      expect(snap!.usedPercent).toBe(37)
      expect(snap!.resetAt).toBe('2026-09-16T00:00:00Z')
    })

    it('非数值形态视为契约不符（禁兜底为 0，但 hasExcluded 不受影响）', () => {
      const snap = parseCodeBuddyCredit({
        codebuddy_credit_total_remaining: '300.50000000',
        codebuddy_credit_total_total: '1000.00000000',
        codebuddy_credit_used_percent: '20.725',
      })
      // 字符串形态（旧契约）不再被接受；禁静默 0 冒充真实值
      expect(snap!.usedPercent).toBe(0)
      // hasExcluded 现由独立聚合键驱动，与 used_percent 合法性解耦
      expect(snap!.hasExcluded).toBe(false)
    })

    it('used_percent 为 null 视为非法形态，usedPercent=0', () => {
      const snap = parseCodeBuddyCredit({
        codebuddy_credit_total_remaining: '300.50000000',
        codebuddy_credit_total_total: '1000.00000000',
        codebuddy_credit_used_percent: null,
      })
      expect(snap!.usedPercent).toBe(0)
    })

    it('非法数值（NaN/Infinity/缺字段）不静默转换', () => {
      for (const bad of [Number.NaN, Number.POSITIVE_INFINITY, undefined, null]) {
        const snap = parseCodeBuddyCredit({
          codebuddy_credit_total_remaining: '300.50000000',
          codebuddy_credit_total_total: '1000.00000000',
          codebuddy_credit_used_percent: bad,
        })
        expect(snap!.usedPercent).toBe(0)
      }
    })
  })

  describe('聚合键形态校验（禁兜底）', () => {
    it('total_remaining/total_total 非字符串形态 → 无快照', () => {
      expect(
        parseCodeBuddyCredit({ codebuddy_credit_total_remaining: 399, codebuddy_credit_total_total: '1200' }),
      ).toBeNull()
      expect(
        parseCodeBuddyCredit({ codebuddy_credit_total_remaining: '399', codebuddy_credit_total_total: 1200 }),
      ).toBeNull()
    })

    it('totals 缺失其一 → 无快照', () => {
      expect(parseCodeBuddyCredit({ codebuddy_credit_total_remaining: '399' })).toBeNull()
      expect(parseCodeBuddyCredit({ codebuddy_credit_total_total: '1200' })).toBeNull()
    })
  })
})

describe('parseCodeBuddyCredit — 展示舍入（2 位小数 ROUND_HALF_UP）', () => {
  it("0.005 → 0.01（ROUND_HALF_UP，非银行家舍入）", () => {
    expect(formatCreditValue('0.005')).toBe('0.01')
    expect(formatCreditValue('0.00499999')).toBe('0.00')
  })

  it('1.005 → 1.01（ROUND_HALF_UP）', () => {
    expect(formatCreditValue('1.005')).toBe('1.01')
    expect(formatCreditValue('1.00499999')).toBe('1.00')
  })

  it('合计展示同一舍入口径', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_total_remaining: '0.005',
      codebuddy_credit_total_total: '1.005',
    })
    expect(snap!.remainingDisplay).toBe('0.01')
    expect(snap!.totalDisplay).toBe('1.01')
  })
})

describe('parseCodeBuddyCreditError — 错误键切换到 codebuddy_credit_error', () => {
  it('读取 §4.1 SSOT 键 codebuddy_credit_error', () => {
    expect(parseCodeBuddyCreditError({ codebuddy_credit_error: '上游 500' })).toBe('上游 500')
  })

  it('旧错误键名不再驱动 UI（后端迁移后已废弃）', () => {
    const legacyErrorKey = 'codebuddy_' + 'quota_error'
    expect(parseCodeBuddyCreditError({ [legacyErrorKey]: '旧键' })).toBeNull()
  })

  it('空串/空白视为无错误', () => {
    expect(parseCodeBuddyCreditError({ codebuddy_credit_error: '   ' })).toBeNull()
    expect(parseCodeBuddyCreditError({ codebuddy_credit_error: '' })).toBeNull()
    expect(parseCodeBuddyCreditError(undefined)).toBeNull()
  })
})

describe('formatCreditValue — ROUND_HALF_UP 语义', () => {
  it("与 decimal.js 显式 ROUND_HALF_UP 一致（非银行家舍入）", () => {
    // 负数侧同样 ROUND_HALF_UP（远离零）
    expect(formatCreditValue(new Decimal('2.675').negated())).toBe('-2.68')
    expect(formatCreditValue('2.675')).toBe('2.68')
  })
})
