import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import AccountActionMenu from '../AccountActionMenu.vue'
import type { Account } from '@/types'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

const account = {
  id: 1,
  platform: 'openai',
  type: 'oauth',
  status: 'active',
  rate_limit_reset_at: '2099-01-01T00:00:00Z'
} as Account

let menuHeight: number
let resizeMenu: ResizeObserverCallback

const getMenu = () => document.body.querySelector<HTMLElement>('.action-menu-content')!
const getTop = () => Number.parseFloat(getMenu().style.top)
const getLeft = () => Number.parseFloat(getMenu().style.left)

function setViewport(width: number, height: number) {
  vi.stubGlobal('innerWidth', width)
  vi.stubGlobal('innerHeight', height)
  window.dispatchEvent(new Event('resize'))
}

async function mountMenu(anchorRect = new DOMRect(950, 720, 32, 24)) {
  const wrapper = mount(AccountActionMenu, {
    props: { show: true, account, anchorRect },
    global: { stubs: { Icon: true } }
  })
  await flushPromises()
  return wrapper
}

enableAutoUnmount(afterEach)

describe('AccountActionMenu viewport positioning', () => {
  beforeEach(() => {
    menuHeight = 305
    setViewport(1024, 768)
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
      if (!this.classList.contains('action-menu-content')) return new DOMRect()
      return new DOMRect(0, 0, Math.min(208, window.innerWidth - 16), Math.min(menuHeight, window.innerHeight - 16))
    })
    vi.stubGlobal('ResizeObserver', class {
      constructor(callback: ResizeObserverCallback) { resizeMenu = callback }
      observe = vi.fn()
      disconnect = vi.fn()
      unobserve = vi.fn()
    })
  })

  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('measures a long OAuth menu and opens above the last row', async () => {
    await mountMenu()

    expect(getTop()).toBe(720 - 305 - 4)
    expect(getLeft()).toBe(982 - 208)
    expect(getTop() + menuHeight).toBeLessThanOrEqual(window.innerHeight - 8)
    expect(getMenu().textContent).toContain('admin.accounts.recoverState')
  })

  it('opens below the trigger when the complete menu fits', async () => {
    await mountMenu(new DOMRect(500, 100, 32, 24))

    expect(getTop()).toBe(128)
    expect(getLeft()).toBe(324)
  })

  it('centers a mobile menu on its trigger and flips above the last row', async () => {
    setViewport(390, 600)
    await mountMenu(new DOMRect(179, 550, 32, 24))

    expect(getTop()).toBe(241)
    expect(getLeft()).toBe(91)
  })

  it.each([0, 370])('keeps mobile menus within the horizontal margins at x=%i', async (x) => {
    setViewport(390, 600)
    await mountMenu(new DOMRect(x, 100, 24, 24))

    expect(getLeft()).toBeGreaterThanOrEqual(8)
    expect(getLeft() + 208).toBeLessThanOrEqual(382)
  })

  it('constrains an oversized menu and preserves the recover-state action', async () => {
    setViewport(180, 220)
    const wrapper = await mountMenu(new DOMRect(150, 180, 24, 24))

    expect(getTop()).toBe(8)
    expect(getLeft()).toBe(8)
    expect(getMenu().style.maxHeight).toBe('204px')
    expect(getMenu().style.maxWidth).toBe('164px')
    expect(getMenu().classList.contains('overflow-y-auto')).toBe(true)
    expect(getMenu().classList.contains('overscroll-contain')).toBe(true)

    const recover = Array.from(getMenu().querySelectorAll('button'))
      .find(button => button.textContent?.includes('admin.accounts.recoverState'))!
    recover.click()
    expect(wrapper.emitted('recover-state')).toEqual([[account]])
    expect(wrapper.emitted('close')).toHaveLength(1)
  })

  it('repositions when menu content grows', async () => {
    await mountMenu()
    menuHeight = 420
    resizeMenu([], {} as ResizeObserver)
    await flushPromises()

    expect(getTop()).toBe(296)
  })

  it('repositions and updates size limits after a viewport resize', async () => {
    await mountMenu()
    setViewport(320, 240)
    await flushPromises()

    expect(getTop()).toBe(8)
    expect(getLeft()).toBe(104)
    expect(getMenu().style.maxHeight).toBe('224px')
    expect(getMenu().style.maxWidth).toBe('304px')
  })

  it('remeasures after reopening on another row', async () => {
    const wrapper = await mountMenu()
    await wrapper.setProps({ show: false })
    expect(getMenu()).toBeNull()
    await wrapper.setProps({ show: true, anchorRect: new DOMRect(400, 100, 32, 24) })
    await flushPromises()

    expect(getTop()).toBe(128)
    expect(getLeft()).toBe(224)
  })

  it('still closes on Escape and backdrop clicks', async () => {
    const wrapper = await mountMenu()
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    const backdrop = getMenu().previousElementSibling as HTMLElement
    backdrop.click()

    expect(wrapper.emitted('close')).toHaveLength(2)
  })
})

// E44：isRateLimited 与后端 ActiveTokenHarborFreeTierScopes 权威语义逐位对齐（D5 锁定）。
// tokenharbor 免费档无信号哨兵（precise_reset=false）即使已过期仍算限流；非 tokenharbor
// 条目过期则恢复。通过 recover-state 按钮（hasRecoverableState 受 isRateLimited 驱动）断言。
describe('AccountActionMenu isRateLimited tokenharbor semantics', () => {
  beforeEach(() => {
    setViewport(1024, 768)
    vi.stubGlobal('ResizeObserver', class {
      observe = vi.fn()
      disconnect = vi.fn()
      unobserve = vi.fn()
    })
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('tokenharbor 哨兵（precise_reset=false）过期 → isRateLimited 仍 true（recover-state 出现）', async () => {
    const thAccount = {
      ...account,
      id: 11,
      platform: 'kimi',
      type: 'apikey',
      status: 'active',
      rate_limit_reset_at: null,
      extra: {
        model_rate_limits: {
          'deepseek-v4.1-flash:free': {
            rate_limited_at: '2026-03-15T00:00:00Z',
            rate_limit_reset_at: '2020-03-15T00:00:00Z', // 已过期
            reason: 'tokenharbor_free_tier_exhausted:x',
            precise_reset: false // 无信号哨兵
          }
        }
      }
    } as unknown as Account

    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account: thAccount, anchorRect: new DOMRect(950, 720, 32, 24) },
      global: { stubs: { Icon: true } }
    })
    await flushPromises()

    const recover = Array.from(getMenu().querySelectorAll('button'))
      .find(button => button.textContent?.includes('admin.accounts.recoverState'))
    expect(recover).toBeTruthy()
    expect(wrapper.emitted('recover-state')).toBeUndefined()
  })

  it('tokenharbor 条目 precise_reset=true 且已过期 → isRateLimited 为 false（recover-state 不出现）', async () => {
    const thAccount = {
      ...account,
      id: 12,
      platform: 'kimi',
      type: 'apikey',
      status: 'active',
      rate_limit_reset_at: null,
      extra: {
        model_rate_limits: {
          'deepseek-v4.1-flash:free': {
            rate_limited_at: '2026-03-15T00:00:00Z',
            rate_limit_reset_at: '2020-03-15T00:00:00Z', // 已过期
            reason: 'tokenharbor_free_tier_exhausted:x',
            precise_reset: true // 权威恢复标记
          }
        }
      }
    } as unknown as Account

    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account: thAccount, anchorRect: new DOMRect(950, 720, 32, 24) },
      global: { stubs: { Icon: true } }
    })
    await flushPromises()

    const recover = Array.from(getMenu().querySelectorAll('button'))
      .find(button => button.textContent?.includes('admin.accounts.recoverState'))
    expect(recover).toBeFalsy()
  })

  it('非 tokenharbor 条目过期 → isRateLimited 为 false（recover-state 不出现，不回归）', async () => {
    const nonThAccount = {
      ...account,
      id: 13,
      platform: 'kimi',
      type: 'apikey',
      status: 'active',
      rate_limit_reset_at: null,
      extra: {
        model_rate_limits: {
          'claude-sonnet-4-5': {
            rate_limited_at: '2026-03-15T00:00:00Z',
            rate_limit_reset_at: '2020-03-15T00:00:00Z' // 已过期
          }
        }
      }
    } as unknown as Account

    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account: nonThAccount, anchorRect: new DOMRect(950, 720, 32, 24) },
      global: { stubs: { Icon: true } }
    })
    await flushPromises()

    const recover = Array.from(getMenu().querySelectorAll('button'))
      .find(button => button.textContent?.includes('admin.accounts.recoverState'))
    expect(recover).toBeFalsy()
  })
})
