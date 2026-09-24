import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, RouterLinkStub } from '@vue/test-utils'

import HomeView from '../HomeView.vue'

type FooterLink = { id: string; name: string; url: string; sort_order: number }

const footerLinks: FooterLink[] = [
  { id: 'b', name: 'Link B', url: 'https://b.example.com', sort_order: 2 },
  { id: 'a', name: 'Link A', url: 'https://a.example.com', sort_order: 1 },
]

const { appStore, authStore } = vi.hoisted(() => ({
  appStore: {
    cachedPublicSettings: {} as Record<string, unknown>,
    siteName: 'Fallback site',
    siteLogo: '',
    docUrl: '',
    publicSettingsLoaded: true,
    footerLinks: [] as FooterLink[],
    fetchPublicSettings: vi.fn(),
  },
  authStore: {
    isAuthenticated: false,
    isAdmin: false,
    user: null as { email?: string } | null,
    checkAuth: vi.fn(),
  },
}))

vi.mock('@/stores', () => ({
  useAppStore: () => appStore,
  useAuthStore: () => authStore,
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => appStore,
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

function mountHome(settings: Record<string, unknown> = {}) {
  appStore.cachedPublicSettings = {
    site_name: 'Test site',
    site_subtitle: 'Test subtitle',
    ...settings,
  }

  return mount(HomeView, {
    global: {
      stubs: {
        RouterLink: RouterLinkStub,
        LocaleSwitcher: { template: '<div data-testid="locale-switcher" />' },
        Icon: { template: '<span data-testid="icon" />' },
      },
    },
  })
}

function footerAnchors(wrapper: ReturnType<typeof mountHome>) {
  return wrapper.find('footer').findAll('a[target="_blank"]')
}

describe('HomeView footer links (友情链接)', () => {
  beforeEach(() => {
    authStore.isAuthenticated = false
    authStore.isAdmin = false
    authStore.user = null
    authStore.checkAuth.mockClear()
    appStore.fetchPublicSettings.mockClear()
    appStore.footerLinks = []
    localStorage.clear()
    vi.spyOn(window, 'matchMedia').mockReturnValue({ matches: false } as MediaQueryList)
  })

  it('compact mode renders footer links sorted by sort_order with rel/target and no hardcoded legacy link', () => {
    appStore.footerLinks = footerLinks
    const wrapper = mountHome({ compact_home_enabled: true })
    const footer = wrapper.find('footer')

    // © line preserved
    expect(footer.text()).toContain('©')
    expect(footer.text()).toContain('Test site')

    const anchors = footerAnchors(wrapper)
    const names = anchors.map((a) => a.text())
    expect(names).toContain('Link A')
    expect(names).toContain('Link B')
    // 按 sort_order 升序：Link A(1) 在 Link B(2) 之前
    expect(names.indexOf('Link A')).toBeLessThan(names.indexOf('Link B'))

    const aAnchor = anchors.find((a) => a.text() === 'Link A')!
    expect(aAnchor.attributes('rel')).toBe('noopener noreferrer')
    expect(aAnchor.attributes('href')).toBe('https://a.example.com')
  })

  it('compact mode renders no footer links when empty and keeps © line', () => {
    appStore.footerLinks = []
    const wrapper = mountHome({ compact_home_enabled: true })
    const footer = wrapper.find('footer')

    expect(footer.text()).toContain('©')
    expect(footer.text()).toContain('Test site')
    expect(footerAnchors(wrapper)).toHaveLength(0)
  })

  it('default page renders footer links sorted with rel/target; © and GitHub unaffected', () => {
    appStore.footerLinks = footerLinks
    const wrapper = mountHome({})
    const footer = wrapper.find('footer')

    expect(footer.text()).toContain('©')
    expect(footer.text()).toContain('Test site')
    expect(footer.text()).toContain('GitHub')

    const anchors = footerAnchors(wrapper)
    const names = anchors.map((a) => a.text())
    expect(names).toContain('Link A')
    expect(names).toContain('Link B')
    expect(names.indexOf('Link A')).toBeLessThan(names.indexOf('Link B'))

    const aAnchor = anchors.find((a) => a.text() === 'Link A')!
    expect(aAnchor.attributes('rel')).toBe('noopener noreferrer')
    expect(aAnchor.attributes('href')).toBe('https://a.example.com')
  })

  it('default page renders no footer links when empty; © and GitHub unaffected', () => {
    appStore.footerLinks = []
    const wrapper = mountHome({})
    const footer = wrapper.find('footer')

    expect(footer.text()).toContain('©')
    expect(footer.text()).toContain('GitHub')

    const names = footerAnchors(wrapper).map((a) => a.text())
    expect(names).not.toContain('Link A')
    expect(names).not.toContain('Link B')
  })
})
