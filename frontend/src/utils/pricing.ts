/**
 * formatScaled formats a per-token (or per-request) USD price scaled by `scale`.
 *
 *   formatScaled(0.000003, 1_000_000)    → "$3.00"     // ≥$0.1 固定 2 位小数
 *   formatScaled(9.2e-8,   1_000_000)    → "$0.092"    // <$0.1 保留 2 位有效数字
 *   formatScaled(0.5,        1)          → "$0.50"     // per request
 *   formatScaled(null,       1_000_000)  → "-"
 *
 * 展示约定：≥$0.1 一律固定 2 位小数（历史口径）；<$0.1 退到 2 位有效数字并去掉
 * 尾零——极小价格（缓存读等）硬凑 2 位小数会有不可接受的舍入失真。计费侧仍用
 * 全精度，本函数只管展示。
 * `minFractionDigits` 把结果补齐到最少小数位（不影响已超出的小数）。
 */
export function formatScaled(value: number | null, scale: number, minFractionDigits = 0): string {
  if (value == null) return '-'
  const scaled = value * scale
  let s: string
  if (scaled !== 0 && Math.abs(scaled) < 0.1) {
    const decimals = Math.max(2, 1 - Math.floor(Math.log10(Math.abs(scaled))))
    s = scaled.toFixed(decimals).replace(/\.?0+$/, '')
  } else {
    s = scaled.toFixed(2)
  }
  if (minFractionDigits > 0 && !s.includes('e')) {
    const dot = s.indexOf('.')
    const digits = dot === -1 ? 0 : s.length - dot - 1
    if (digits < minFractionDigits) {
      s = (dot === -1 ? `${s}.` : s) + '0'.repeat(minFractionDigits - digits)
    }
  }
  return `$${s}`
}

import type { UserPricingInterval } from '@/api/channels'

type TokenPrices = Pick<UserPricingInterval, 'input_price' | 'output_price' | 'cache_write_price' | 'cache_write_1h_price' | 'cache_read_price'>

export function resolveIntervalPrices(iv: UserPricingInterval, base: TokenPrices): UserPricingInterval {
  const price = (absolute: number | null | undefined, multiplier: number | null | undefined, fallback: number | null | undefined) =>
    absolute ?? (fallback == null ? null : fallback * (multiplier ?? 1))
  return {
    ...iv,
    input_price: price(iv.input_price, iv.input_multiplier, base.input_price),
    output_price: price(iv.output_price, iv.output_multiplier, base.output_price),
    cache_write_price: price(iv.cache_write_price, iv.cache_write_multiplier, base.cache_write_price),
    // Resolver uses an explicit cache-write price for both durations unless 1h is overridden.
    cache_write_1h_price: iv.cache_write_1h_price ?? iv.cache_write_price ?? price(null, iv.cache_write_multiplier, base.cache_write_1h_price),
    cache_read_price: price(iv.cache_read_price, iv.cache_read_multiplier, base.cache_read_price)
  }
}
