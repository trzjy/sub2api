import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

import en from '../../../i18n/locales/en'
import zh from '../../../i18n/locales/zh'

// CARD-KC-09：管理员侧边栏的上游续费/采购快捷入口。
// 与同目录 AppSidebar.spec.ts 保持一致的源码断言方式（该组件依赖 store/router，
// 挂载成本高于本用例需要覆盖的结构性事实）。
const componentPath = resolve(dirname(fileURLToPath(import.meta.url)), '../AppSidebar.vue')
const componentSource = readFileSync(componentPath, 'utf8')

function navItem(): Record<string, unknown> {
  return (zh as Record<string, any>).nav ?? {}
}

describe('AppSidebar 上游续费入口（CARD-KC-09 / KC-18）', () => {
  it('从上游注册表读取续费链接（单一事实源），不再硬编码常量', () => {
    expect(componentSource).toContain("import { listUpstreams } from '@/api/admin/quotaKeys'")
    expect(componentSource).toContain('const data = await listUpstreams()')
    expect(componentSource).toMatch(/if \(u\.renewal_url\) map\[u\.key\] = u\.renewal_url/)
    expect(componentSource).not.toContain('KNCY_STORE_URL')
  })

  it('在激活码管理旁新增「上游续费」分组入口', () => {
    expect(componentSource).toMatch(/path: '\/admin\/quota-keys'[^\n]*label: t\('nav\.quotaKeys'\)/)
    expect(componentSource).toMatch(/label: t\('nav\.upstreamStore'\)/)
    expect(componentSource).toMatch(/path: '\/admin\/quota-keys-upstream'/)
  })

  it('两个子项的 href 均来自 upstreamRenewalUrls 且以新标签页打开', () => {
    expect(componentSource).toMatch(/href: upstreamRenewalUrls\.value\['kn_cy'\]/)
    expect(componentSource).toMatch(/href: upstreamRenewalUrls\.value\['shareai'\]/)
    expect(componentSource).toContain('target="_blank"')
    expect(componentSource).toContain('rel="noopener noreferrer"')
  })

  it('未配置续费链接的子项退化为占位（placeholder 由缺失 url 推导）', () => {
    expect(componentSource).toMatch(/placeholder: !upstreamRenewalUrls\.value\['kn_cy'\]/)
    expect(componentSource).toMatch(/placeholder: !upstreamRenewalUrls\.value\['shareai'\]/)
    expect(componentSource).toContain('handlePlaceholderNav')
  })

  it('占位提示使用通用「待配置」文案（不写死 ShareAI）', () => {
    expect(componentSource).toContain("t('admin.quotaKeys.upstreamStore.pending')")
    expect(componentSource).not.toContain("t('admin.quotaKeys.upstreamStore.shareaiUnconfigured')")
  })

  it('外链/占位子项不落成 router-link（避免跳转到不存在的路由）', () => {
    expect(componentSource).toMatch(/<a\s+v-if="child\.href"/)
    expect(componentSource).toMatch(/<button\s+v-else-if="child\.placeholder"/)
    expect(componentSource).toMatch(/<router-link\s+v-else/)
  })
})

describe('上游续费入口多语言词条（CARD-KC-09）', () => {
  it('中英文 nav 命名空间都包含续费入口文案', () => {
    for (const locale of [navItem(), (en as Record<string, any>).nav ?? {}]) {
      expect(typeof locale.upstreamStore).toBe('string')
      expect(locale.upstreamStore.length).toBeGreaterThan(0)
      expect(typeof locale.kncyStore).toBe('string')
      expect(typeof locale.shareaiStore).toBe('string')
    }
  })

  it('中英文 admin.quotaKeys.upstreamStore 词条齐全', () => {
    const required = [
      'button',
      'kncy',
      'shareai',
      'pending',
      'openExternal',
      'shareaiUnconfiguredTitle',
      'shareaiUnconfigured'
    ]
    const locales = [
      (zh as Record<string, any>).admin.quotaKeys.upstreamStore,
      (en as Record<string, any>).admin.quotaKeys.upstreamStore
    ]
    for (const store of locales) {
      for (const key of required) {
        expect(typeof store[key]).toBe('string')
        expect(store[key].trim().length).toBeGreaterThan(0)
      }
    }
  })
})
