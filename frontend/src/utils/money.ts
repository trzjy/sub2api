/**
 * Money display utilities — single source of truth (SSOT).
 *
 * ACCOUNT_CURRENCY is the internal ledger currency ("USD").
 * All balance/cost/quota/value/rebate amounts are USD.
 * Payment amounts follow the order's explicit currency.
 */

export const ACCOUNT_CURRENCY = 'USD'

/**
 * Default payment currency constant (kept for backwards compat with callers
 * that referenced DEFAULT_PAYMENT_CURRENCY directly).  The canonical ledger
 * currency is still {@link ACCOUNT_CURRENCY}.
 */
export const DEFAULT_PAYMENT_CURRENCY = 'CNY'

const CURRENCY_SYMBOLS: Record<string, string> = {
  USD: '$',
  CNY: '¥',
  RMB: '¥',
  EUR: '€',
  GBP: '£',
  JPY: '¥',
  HKD: 'HK$',
  TWD: 'NT$',
  KRW: '₩',
  AUD: 'A$',
  CAD: 'C$',
  SGD: 'S$',
  NZD: 'NZ$',
  MOP: 'MOP$',
  MYR: 'RM',
  THB: '฿',
  PHP: '₱',
  INR: '₹',
}

/** Normalize an arbitrary currency string; returns DEFAULT_PAYMENT_CURRENCY on failure. */
export function normalizePaymentCurrency(currency?: string | null): string {
  const normalized = String(currency || '').trim().toUpperCase()
  return /^[A-Z]{3}$/.test(normalized) ? normalized : DEFAULT_PAYMENT_CURRENCY
}

/** Return a human-readable short symbol for an ISO-4217 currency code. */
export function currencySymbol(currency?: string | null): string {
  const normalized = normalizePaymentCurrency(currency)
  return CURRENCY_SYMBOLS[normalized] || normalized
}

// ── Fraction-digits helpers ─────────────────────────────────────────────

function currencyFractionDigits(currency: string): number {
  try {
    return new Intl.NumberFormat(undefined, {
      style: 'currency',
      currency,
    }).resolvedOptions().maximumFractionDigits ?? 2
  } catch {
    return 2
  }
}

function roundToCurrencyFraction(value: number, currency: string): number {
  if (!Number.isFinite(value)) return 0
  const factor = 10 ** currencyFractionDigits(currency)
  return Math.round(value * factor) / factor
}

function ceilToCurrencyFraction(value: number, currency: string): number {
  if (!Number.isFinite(value)) return 0
  const factor = 10 ** currencyFractionDigits(currency)
  return Math.ceil(value * factor) / factor
}

// ── Public formatters ───────────────────────────────────────────────────

/**
 * Format a payment amount with the correct symbol and fraction digits.
 * Use for amounts that carry an explicit currency (order pay_amount, etc.).
 */
export function formatPaymentAmount(
  amount: number,
  currency?: string | null,
  locale?: string,
  fractionDigitsOverride?: number,
): string {
  const normalized = normalizePaymentCurrency(currency)
  const fractionDigits = fractionDigitsOverride ?? currencyFractionDigits(normalized)
  try {
    return new Intl.NumberFormat(locale || undefined, {
      style: 'currency',
      currency: normalized,
      currencyDisplay: 'narrowSymbol',
      minimumFractionDigits: fractionDigits,
      maximumFractionDigits: fractionDigits,
    }).format(Number.isFinite(amount) ? amount : 0)
  } catch {
    return `${normalized} ${(Number.isFinite(amount) ? amount : 0).toFixed(fractionDigits)}`
  }
}

/**
 * Format a general money amount for display.
 * Delegates to Intl currency formatting.
 */
export function formatMoney(
  amount: number,
  currency: string = ACCOUNT_CURRENCY,
  locale?: string,
  fractionDigitsOverride?: number,
): string {
  return formatPaymentAmount(amount, currency, locale, fractionDigitsOverride)
}

/**
 * Format an **account (ledger) amount** — always USD.
 * Use for balance, cost, quota, value, rebate, etc.
 * `fractionDigitsOverride` allows preserving sub-cent precision (e.g. cost tooltips).
 */
export function formatAccountMoney(amount: number, locale?: string, fractionDigitsOverride?: number): string {
  return formatPaymentAmount(amount, ACCOUNT_CURRENCY, locale, fractionDigitsOverride)
}

// ── FX helpers (used by PaymentView / PlanEditDialog / PricingView) ──────

export function roundPaymentAmount(value: number, currency: string): number {
  return roundToCurrencyFraction(value, currency)
}

export function ceilPaymentAmount(value: number, currency: string): number {
  return ceilToCurrencyFraction(value, currency)
}

/**
 * Convert a subscription price (USD) to the gateway currency amount.
 * Uses `fxRates[ccy]` (1 USD = X ccy).  Returns NaN when fxRates is missing
 * the required key — callers should guard accordingly.
 */
export function toGatewayAmount(priceUSD: number, currency: string, fxRates: Record<string, number>): number {
  if (!Number.isFinite(priceUSD) || priceUSD <= 0) return 0
  if (currency === ACCOUNT_CURRENCY) return priceUSD
  const rate = fxRates[currency]
  if (!rate || rate <= 0) return NaN
  return roundPaymentAmount(priceUSD * rate, currency)
}

/**
 * Convert a gateway amount back to USD using the FX table.
 * USD is implied 1.0 and never needs a table entry.
 */
export function toUSDAmount(amount: number, currency: string, fxRates: Record<string, number>): number {
  if (!Number.isFinite(amount) || amount <= 0) return 0
  if (currency === ACCOUNT_CURRENCY) return amount
  const rate = fxRates[currency]
  if (!rate || rate <= 0) return NaN
  return Math.round((amount / rate) * 100) / 100
}
