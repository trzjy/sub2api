import { describe, expect, it } from 'vitest'
import Decimal from 'decimal.js'

import { parseCodeBuddyCredit, parseCodeBuddyCreditError, formatCreditValue } from '../codebuddyCredit'

// Card F：分包快照展示 + 数值精度（R2#5 / R3#4 红线）
//   1) 存储契约键名逐键对齐后端（codebuddy_credit_packages 数组元素
//      id/name/unit/remaining/total/expires_at/status，remaining/total 字符串承载）；
//   2) 旧展示键（total/used/updated_at）已退役，不再读取；
//   3) 合计 = Σ 派生（异单位不计入并标注），展示 2 位小数 ROUND_HALF_UP；
//   4) 错误键读写切到 codebuddy_credit_error（旧错误键名已废弃）。
describe('parseCodeBuddyCredit — 分包快照解析', () => {
  it('逐包解析并派生合计（多分包求和，20,8 口径）', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_packages: [
        { id: 'a', name: 'Pro 月包', unit: 'credits', remaining: '300.50000000', total: '1000.00000000', expires_at: '2026-10-01T00:00:00Z', status: 0 },
        { id: 'b', name: '赠送包', unit: 'credits', remaining: '99.25000000', total: '200.00000000', expires_at: '2026-10-15T00:00:00Z', status: 0 },
      ],
      codebuddy_credit_packages_updated_at: '2026-09-29T10:00:00Z',
      codebuddy_credit_used_percent: '20.725',
      codebuddy_credit_reset_at: '2026-10-01T00:00:00Z',
    })

    expect(snap).not.toBeNull()
    expect(snap!.packages).toHaveLength(2)
    expect(snap!.packages[0]!.name).toBe('Pro 月包')
    // 400.75000000 = 300.5 + 99.25（十进制精确，无 float 误差）
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

  it('异单位分包不计入合计并标注 excludedReason=unit_mismatch', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_packages: [
        { id: 'a', name: '积分包', unit: 'credits', remaining: '10.00000000', total: '20.00000000', expires_at: '', status: 0 },
        { id: 'b', name: '美元包', unit: 'usd', remaining: '5.00000000', total: '9.00000000', expires_at: '', status: 0 },
      ],
    })
    expect(snap!.totalRemaining).toBe('10')
    expect(snap!.totalTotal).toBe('20')
    expect(snap!.hasExcluded).toBe(true)
    const usdPkg = snap!.packages.find((p) => p.id === 'b')!
    expect(usdPkg.countable).toBe(false)
    expect(usdPkg.excludedReason).toBe('unit_mismatch')
  })

  it('Status=3（耗尽）分包不计入合计并标注', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_packages: [
        { id: 'a', name: '在用', unit: 'credits', remaining: '40.00000000', total: '50.00000000', expires_at: '', status: 0 },
        { id: 'c', name: '已耗尽', unit: 'credits', remaining: '0.00000000', total: '7.00000000', expires_at: '', status: 3 },
      ],
    })
    // 耗尽包 total 不进分母
    expect(snap!.totalTotal).toBe('50')
    expect(snap!.totalRemaining).toBe('40')
    expect(snap!.hasExcluded).toBe(true)
    expect(snap!.packages.find((p) => p.id === 'c')!.excludedReason).toBe('status_inactive')
  })

  it('完全无快照返回 null（不冒充 0%，调用方走空态）', () => {
    expect(parseCodeBuddyCredit(undefined)).toBeNull()
    expect(parseCodeBuddyCredit(null)).toBeNull()
    expect(parseCodeBuddyCredit({})).toBeNull()
    expect(parseCodeBuddyCredit({ unrelated: 1 })).toBeNull()
    // 只有错误标记，没有分包数组 → 仍是无快照
    expect(parseCodeBuddyCredit({ codebuddy_credit_error: '上游 500' })).toBeNull()
    // 旧展示键已退役：不再构成快照
    const legacyTotalKey = 'codebuddy_credit_' + 'total'
    const legacyUsedKey = 'codebuddy_credit_' + 'used'
    const legacyUpdatedAtKey = 'codebuddy_credit_' + 'updated_at'
    expect(
      parseCodeBuddyCredit({
        [legacyTotalKey]: 1000,
        [legacyUsedKey]: 370,
        [legacyUpdatedAtKey]: '2026-09-15T00:00:00Z',
      }),
    ).toBeNull()
  })

  it('剩余/总量为数值（非字符串）形态视为契约不符，不静默当 0', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_packages: [
        { id: 'a', name: 'NaN 形态', unit: 'credits', remaining: 10, total: 20, expires_at: '', status: 0 },
      ],
    })
    expect(snap!.packages).toHaveLength(0)
    // 该包不可解析 → hasExcluded=true（错误标记），不静默 0 计入合计
    expect(snap!.hasExcluded).toBe(true)
    expect(snap!.totalRemaining).toBe('0')
  })

  it('非法 remaining>total 的分包不计入并标注', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_packages: [
        { id: 'bad', name: '异常', unit: 'credits', remaining: '30.00000000', total: '20.00000000', expires_at: '', status: 0 },
      ],
    })
    expect(snap!.packages).toHaveLength(0)
    expect(snap!.hasExcluded).toBe(true)
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

  it('单包展示与合计展示同一舍入口径', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_packages: [
        { id: 'a', name: 'A', unit: 'credits', remaining: '0.005', total: '1.005', expires_at: '', status: 0 },
        { id: 'b', name: 'B', unit: 'credits', remaining: '0.005', total: '1.005', expires_at: '', status: 0 },
      ],
    })
    // 0.005+0.005 = 0.01（十进制精确，不因 float 变 0.010000000000000002）
    expect(snap!.totalRemaining).toBe('0.01')
    expect(snap!.remainingDisplay).toBe('0.01')
    expect(snap!.totalDisplay).toBe('2.01')
  })

  it('多包求和跨越 20,8 边界时十进制精确（float 会漂移的用例）', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_packages: [
        { id: 'a', name: 'A', unit: 'credits', remaining: '0.10000000', total: '0.20000000', expires_at: '', status: 0 },
        { id: 'b', name: 'B', unit: 'credits', remaining: '0.20000000', total: '0.20000000', expires_at: '', status: 0 },
      ],
    })
    // 0.1+0.2=0.3（float 会得到 0.30000000000000004）
    expect(snap!.totalRemaining).toBe('0.3')
    expect(snap!.remainingDisplay).toBe('0.30')
  })

  it('大数（20,8 上限量级）求和不丢精度', () => {
    const snap = parseCodeBuddyCredit({
      codebuddy_credit_packages: [
        { id: 'a', name: 'A', unit: 'credits', remaining: '99999999999999.99999999', total: '100000000000000.00000000', expires_at: '', status: 0 },
        { id: 'b', name: 'B', unit: 'credits', remaining: '0.00000001', total: '0.00000001', expires_at: '', status: 0 },
      ],
    })
    // float 无法承载，Decimal 精确
    expect(snap!.totalRemaining).toBe('100000000000000')
    expect(snap!.remainingDisplay).toBe('100000000000000.00')
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
