import { describe, expect, it } from 'vitest'
import zh from '@/i18n/locales/zh/admin/accounts'
import en from '@/i18n/locales/en/admin/accounts'

type Dict = Record<string, unknown>

function getPath(obj: Dict, path: string): unknown {
  return path.split('.').reduce<unknown>((acc, seg) => {
    if (acc && typeof acc === 'object') return (acc as Dict)[seg]
    return undefined
  }, obj)
}

function flatten(obj: unknown, prefix = ''): string[] {
  if (!obj || typeof obj !== 'object' || Array.isArray(obj)) return []
  return Object.entries(obj as Dict).flatMap(([k, v]) => {
    const key = prefix ? `${prefix}.${k}` : k
    if (v && typeof v === 'object' && !Array.isArray(v)) return flatten(v, key)
    return [key]
  })
}

const subtrees = ['webLogin.autoLogin', 'webLogin.tabs', 'loginStatus', 'batch'] as const

describe('web auto-login i18n keys (zh/en aligned)', () => {
  for (const path of subtrees) {
    it(`subtree "${path}" has identical keys in zh and en`, () => {
      const zhKeys = flatten(getPath(zh.accounts as Dict, path)).sort()
      const enKeys = flatten(getPath(en.accounts as Dict, path)).sort()
      expect(enKeys).toEqual(zhKeys)
    })
  }

  it('columns.loginStatus exists in both locales', () => {
    expect((zh.accounts as Dict).columns?.['loginStatus']).toBeTruthy()
    expect((en.accounts as Dict).columns?.['loginStatus']).toBeTruthy()
  })
})
