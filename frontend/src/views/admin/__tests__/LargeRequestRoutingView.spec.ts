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

type GroupPlatformLike = 'deepseek' | 'zhipu' | 'kimi' | 'codebuddy' | string

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

async function expandFamily(wrapper: Awaited<ReturnType<typeof mountView>>, platform: string) {
  const toggle = wrapper.find(`[data-testid="family-toggle-${platform}"]`)
  if (toggle.exists()) {
    await toggle.trigger('click')
    await flushPromises()
  }
}

beforeEach(() => {
  vi.clearAllMocks()
  const byPlatform = accountsByPlatform()
  getAllIncludingInactive.mockResolvedValue(GROUPS)
  listAccounts.mockImplementation(async (_page: number, _size: number, opts: { platform: string }) => {
    const items = byPlatform[opts.platform] ?? []
    return { items, total: items.length }
  })
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
  it('默认只展开第一个家族，其余折叠', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-testid="family-deepseek"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="family-zhipu"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="family-kimi"]').exists()).toBe(true)

    // 折叠内容用 v-show 控制，检查计算样式前的 aria-expanded 更稳定
    expect(wrapper.find('[data-testid="family-toggle-deepseek"]').attributes('aria-expanded')).toBe('true')
    expect(wrapper.find('[data-testid="family-toggle-zhipu"]').attributes('aria-expanded')).toBe('false')
    expect(wrapper.find('[data-testid="family-toggle-kimi"]').attributes('aria-expanded')).toBe('false')
  })

  it('展开后渲染该家族账号，三态下拉可选', async () => {
    const wrapper = mountView()
    await flushPromises()
    await expandFamily(wrapper, 'zhipu')

    const select = wrapper.find('[data-testid="lro-scope-808"]')
    expect(select.exists()).toBe(true)
    expect((select.element as HTMLSelectElement).value).toBe('any')
    expect((select.element as HTMLSelectElement).disabled).toBe(false)
  })

  it('切换三态调用接口并更新本地状态', async () => {
    const wrapper = mountView()
    await flushPromises()

    const select = wrapper.find('[data-testid="lro-scope-807"]')
    await select.setValue('large_only')
    await flushPromises()

    expect(setAccountRequestSizeScope).toHaveBeenCalledWith(807, 'large_only')
    expect((wrapper.find('[data-testid="lro-scope-807"]').element as HTMLSelectElement).value).toBe('large_only')
  })

  it('接口失败时回滚选择并显示错误条', async () => {
    setAccountRequestSizeScope.mockRejectedValueOnce({ code: 'REQUEST_SIZE_SCOPE_POOL_EXHAUSTED', message: 'pool empty' })
    const wrapper = mountView()
    await flushPromises()

    const select = wrapper.find('[data-testid="lro-scope-807"]')
    await select.setValue('small_only')
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

  it('池内最后一个未受限账号不可设置限制', async () => {
    const byPlatform: Record<string, AccountListItem[]> = {
      deepseek: [makeAccount(900, 'deepseek', [49])],
      zhipu: [],
      kimi: [],
      codebuddy: []
    }
    listAccounts.mockImplementation(async (_page: number, _size: number, opts: { platform: string }) => {
      const items = byPlatform[opts.platform] ?? []
      return { items, total: items.length }
    })

    const wrapper = mountView()
    await flushPromises()

    const select = wrapper.find('[data-testid="lro-scope-900"]')
    expect(select.exists()).toBe(true)
    expect((select.element as HTMLSelectElement).disabled).toBe(true)
  })
})
