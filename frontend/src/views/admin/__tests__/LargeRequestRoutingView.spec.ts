import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import LargeRequestRoutingView from '../LargeRequestRoutingView.vue'
import type { AccountListItem, AdminGroup } from '@/types'

const { listAccounts, getAllIncludingInactive, setAccountRequestSizeScope } = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  getAllIncludingInactive: vi.fn(),
  setAccountRequestSizeScope: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      setAccountRequestSizeScope
    },
    groups: {
      getAllIncludingInactive
    }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

type GroupPlatformLike = 'deepseek' | 'zhipu' | 'kimi' | 'codebuddy' | string

function makeGroup(
  id: number,
  name: string,
  platform: GroupPlatformLike,
  subscriptionType: 'standard' | 'subscription'
): AdminGroup {
  return {
    id,
    name,
    platform,
    subscription_type: subscriptionType
  } as unknown as AdminGroup
}

function makeAccount(
  id: number,
  platform: string,
  groupIDs: number[],
  overrides: Partial<AccountListItem> = {}
): AccountListItem {
  return {
    id,
    name: `account-${id}`,
    platform,
    status: 'active',
    schedulable: true,
    priority: 10,
    group_ids: groupIDs,
    ...overrides
  } as unknown as AccountListItem
}

const GROUPS: AdminGroup[] = [
  makeGroup(49, 'DeepSeek 计量分组', 'deepseek', 'standard'),
  makeGroup(115, 'Deepseek 月卡订阅', 'deepseek', 'subscription'),
  makeGroup(116, 'Glm 月卡订阅', 'zhipu', 'subscription'),
  makeGroup(117, 'Kimi 月卡订阅', 'kimi', 'subscription')
]

function accountsByPlatform(): Record<string, AccountListItem[]> {
  return {
    deepseek: [
      makeAccount(807, 'deepseek', [49, 115]),
      makeAccount(239, 'deepseek', [49, 115])
    ],
    zhipu: [makeAccount(808, 'zhipu', [116]), makeAccount(240, 'zhipu', [116])],
    kimi: [makeAccount(817, 'kimi', [117]), makeAccount(238, 'kimi', [117])],
    // 跨平台账号：绑在 deepseek 月卡订阅里，但平台是 codebuddy
    codebuddy: [makeAccount(386, 'codebuddy', [115])]
  }
}

function mountView() {
  return mount(LargeRequestRoutingView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template: '<div><slot name="filters" /><slot name="table" /></div>'
        }
      }
    }
  })
}

function setAccountsByPlatform(byPlatform: Record<string, AccountListItem[]>) {
  listAccounts.mockImplementation(async (_page: number, _size: number, opts: { platform: string }) => {
    const items = byPlatform[opts.platform] ?? []
    return { items, total: items.length }
  })
}

function optionDisabled(select: { element: Element }, value: string): boolean {
  const option = select.element.querySelector(`option[value="${value}"]`)
  return (option as HTMLOptionElement | null)?.disabled ?? false
}

beforeEach(() => {
  vi.clearAllMocks()
  getAllIncludingInactive.mockResolvedValue(GROUPS)
  setAccountsByPlatform(accountsByPlatform())
  setAccountRequestSizeScope.mockImplementation(async (id: number, scope: string) => ({
    id,
    name: `account-${id}`,
    platform: 'deepseek',
    status: 'active',
    schedulable: true,
    priority: 10,
    group_ids: [],
    extra: { large_request_only: scope === 'large_only', small_request_only: scope === 'small_only' }
  }))
})

describe('LargeRequestRoutingView', () => {
  it('三个家族默认全部展开，不会把平台藏起来', async () => {
    const wrapper = mountView()
    await flushPromises()

    for (const family of ['deepseek', 'zhipu', 'kimi']) {
      expect(wrapper.find(`[data-testid="family-${family}"]`).exists()).toBe(true)
      expect(wrapper.find(`[data-testid="family-toggle-${family}"]`).attributes('aria-expanded')).toBe('true')
    }
  })

  it('每个账号在每个家族只出现一行，分组列合并全部绑定分组', async () => {
    const wrapper = mountView()
    await flushPromises()

    // 807 同时绑 49(计量) 与 115(订阅)，旧设计会出现两行，现在只应有一行
    const rows = wrapper.findAll('[data-testid="row-deepseek-807"]')
    expect(rows).toHaveLength(1)
    expect(rows[0].text()).toContain('DeepSeek 计量分组')
    expect(rows[0].text()).toContain('Deepseek 月卡订阅')
  })

  it('三态下拉渲染且可用', async () => {
    const wrapper = mountView()
    await flushPromises()

    const select = wrapper.find('[data-testid="lro-scope-808"]')
    expect(select.exists()).toBe(true)
    expect((select.element as HTMLSelectElement).value).toBe('any')
    expect((select.element as HTMLSelectElement).disabled).toBe(false)
  })

  it('切换三态调用接口并更新本地状态', async () => {
    const wrapper = mountView()
    await flushPromises()

    await wrapper.find('[data-testid="lro-scope-807"]').setValue('large_only')
    await flushPromises()

    expect(setAccountRequestSizeScope).toHaveBeenCalledWith(807, 'large_only')
    expect((wrapper.find('[data-testid="lro-scope-807"]').element as HTMLSelectElement).value).toBe('large_only')
  })

  it('接口失败时回滚选择并显示错误条', async () => {
    setAccountRequestSizeScope.mockRejectedValueOnce({ code: 'REQUEST_SIZE_SCOPE_POOL_EXHAUSTED', message: 'pool empty' })
    const wrapper = mountView()
    await flushPromises()

    await wrapper.find('[data-testid="lro-scope-807"]').setValue('small_only')
    await flushPromises()

    expect((wrapper.find('[data-testid="lro-scope-807"]').element as HTMLSelectElement).value).toBe('any')
    expect(wrapper.find('[data-testid="lro-error-banner"]').text()).toContain('pool empty')
  })

  it('跨平台账号显示在所属分组中且开关禁用', async () => {
    const wrapper = mountView()
    await flushPromises()

    const select = wrapper.find('[data-testid="lro-scope-386"]')
    expect(select.exists()).toBe(true)
    expect((select.element as HTMLSelectElement).disabled).toBe(true)
  })

  it('池内唯一账号：两个限制选项都被禁用', async () => {
    setAccountsByPlatform({
      deepseek: [makeAccount(900, 'deepseek', [49])],
      zhipu: [],
      kimi: [],
      codebuddy: []
    })

    const wrapper = mountView()
    await flushPromises()

    const select = wrapper.find('[data-testid="lro-scope-900"]')
    expect(select.exists()).toBe(true)
    expect(optionDisabled(select, 'large_only')).toBe(true)
    expect(optionDisabled(select, 'small_only')).toBe(true)
  })

  it('对端为 small_only 时仍允许把本账号设为 large_only', async () => {
    // 对端只要不是同一侧就可以承接：small_only 对端能接小请求，large_only 必须可选。
    setAccountsByPlatform({
      deepseek: [
        makeAccount(910, 'deepseek', [49]),
        makeAccount(911, 'deepseek', [49], { extra: { small_request_only: true } })
      ],
      zhipu: [],
      kimi: [],
      codebuddy: []
    })

    const wrapper = mountView()
    await flushPromises()

    const select = wrapper.find('[data-testid="lro-scope-910"]')
    expect(optionDisabled(select, 'large_only')).toBe(false)
    expect(optionDisabled(select, 'small_only')).toBe(true)
  })
})
