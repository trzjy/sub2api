import { describe, expect, it } from 'vitest'
import {
  ACCOUNT_CURRENCY,
  DEFAULT_PAYMENT_CURRENCY,
  currencySymbol,
  formatAccountMoney,
  formatMoney,
  formatPaymentAmount,
  normalizePaymentCurrency,
  toGatewayAmount,
  toUSDAmount,
} from '@/utils/money'

describe('formatPaymentAmount', () => {
  it('uses the currency default fraction digits', () => {
    expect(formatPaymentAmount(100, 'JPY', 'en-US')).not.toContain('.00')
    expect(formatPaymentAmount(100, 'KRW', 'en-US')).not.toContain('.00')
    expect(formatPaymentAmount(100, 'HKD', 'en-US')).toContain('.00')
  })
})

describe('currencySymbol', () => {
  it('maps common payment currencies and falls back safely', () => {
    expect(currencySymbol('USD')).toBe('$')
    expect(currencySymbol('cny')).toBe('¥')
    expect(currencySymbol('EUR')).toBe('€')
    expect(currencySymbol('')).toBe('¥')
    expect(currencySymbol('XYZ')).toBe('XYZ')
    expect(currencySymbol('HKD')).toBe('HK$')
  })
})

describe('ACCOUNT_CURRENCY / formatAccountMoney', () => {
  it('is USD and formats with the $ symbol', () => {
    expect(ACCOUNT_CURRENCY).toBe('USD')
    expect(formatAccountMoney(13.99)).toBe('$13.99')
    expect(formatAccountMoney(0)).toBe('$0.00')
  })
})

describe('formatMoney', () => {
  it('defaults to account currency and honors an explicit currency', () => {
    expect(formatMoney(10)).toBe('$10.00')
    expect(formatMoney(10, 'CNY')).toBe('¥10.00')
  })
})

describe('normalizePaymentCurrency', () => {
  it('normalizes to uppercase and falls back to CNY', () => {
    expect(normalizePaymentCurrency('hkd')).toBe('HKD')
    expect(normalizePaymentCurrency(undefined)).toBe(DEFAULT_PAYMENT_CURRENCY)
    expect(normalizePaymentCurrency('')).toBe(DEFAULT_PAYMENT_CURRENCY)
    expect(normalizePaymentCurrency('US')).toBe(DEFAULT_PAYMENT_CURRENCY)
  })
})

describe('toGatewayAmount / toUSDAmount', () => {
  const fx = { CNY: 7.15, HKD: 7.8 }

  it('converts USD price to gateway currency at the FX rate', () => {
    expect(toGatewayAmount(1, 'CNY', fx)).toBe(7.15)
    expect(toGatewayAmount(9.99, 'CNY', fx)).toBe(71.43)
    expect(toGatewayAmount(1, 'HKD', fx)).toBe(7.8)
  })

  it('returns NaN when the currency is missing from the FX table', () => {
    expect(Number.isNaN(toGatewayAmount(1, 'JPY', fx))).toBe(true)
  })

  it('returns 0 for non-positive amounts', () => {
    expect(toGatewayAmount(0, 'CNY', fx)).toBe(0)
    expect(toGatewayAmount(-1, 'CNY', fx)).toBe(0)
  })

  it('converts gateway amount back to USD', () => {
    expect(toUSDAmount(7.15, 'CNY', fx)).toBe(1)
    expect(toUSDAmount(100, 'CNY', fx)).toBe(13.99)
  })

  it('treats USD as implied 1.0 without a table entry', () => {
    expect(toUSDAmount(10, 'USD', {})).toBe(10)
    expect(toGatewayAmount(10, 'USD', {})).toBe(10)
  })
})
