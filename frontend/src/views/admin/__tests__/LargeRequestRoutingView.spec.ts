import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import LargeRequestRoutingView from '../LargeRequestRoutingView.vue'
import type { AccountListItem, AdminGroup } from '@/types'

const { listAccounts, getAllIncludingInactive, setAccountLargeRequestOnly } = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  getAllIncludingInactive: vi.fn(),
  setAccountLargeRequestOnly: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      setAccountLargeRequestOnly
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
  platform: 'deepseek' | 'zhipu' | 'kimi',
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
  platform: 'deepseek' | 'zhipu' | 'kimi',
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
    extra: {},
    ...overrides
  } as unknown as AccountListItem
}

function mockBackend(groups: AdminGroup[], accounts: AccountListItem[]): void {
  getAllIncludingInactive.mockResolvedValue(groups)
  listAccounts.mockImplementation(
    async (_page: number, _pageSize: number, filters?: { platform?: string }) => {
      const items = accounts.filter((account) => account.platform === filters?.platform)
      return { items, total: items.length, page: 1, page_size: 200, pages: 1 }
    }
  )
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

function toggleCheckbox(element: Element, checked: boolean): void {
  const input = element as HTMLInputElement
  input.checked = checked
  input.dispatchEvent(new Event('change'))
}

describe('admin LargeRequestRoutingView', () => {
  beforeEach(() => {
    listAccounts.mockReset()
    getAllIncludingInactive.mockReset()
    setAccountLargeRequestOnly.mockReset()
  })

  it('renders three families with metered and subscription segments', async () => {
    mockBackend(
      [
        makeGroup(1, 'deepseek-metered', 'deepseek', 'standard'),
        makeGroup(2, 'deepseek-subscription', 'deepseek', 'subscription'),
        makeGroup(3, 'glm-metered', 'zhipu', 'standard'),
        makeGroup(4, 'kimi-metered', 'kimi', 'standard')
      ],
      [
        makeAccount(101, 'deepseek', [1, 2]),
        makeAccount(102, 'deepseek', [1]),
        makeAccount(201, 'zhipu', [3]),
        makeAccount(301, 'kimi', [4])
      ]
    )

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-testid="family-deepseek"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="family-zhipu"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="family-kimi"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="family-deepseek"] h2').text()).toBe('DeepSeek')
    expect(wrapper.get('[data-testid="family-zhipu"] h2').text()).toBe('GLM')
    expect(wrapper.get('[data-testid="family-kimi"] h2').text()).toBe('Kimi')

    for (const platform of ['deepseek', 'zhipu', 'kimi']) {
      expect(wrapper.find(`[data-testid="segment-${platform}-standard"]`).exists()).toBe(true)
      expect(wrapper.find(`[data-testid="segment-${platform}-subscription"]`).exists()).toBe(true)
    }

    // 计量段：同一家族的账号按 group_ids 落位；订阅段只收订阅型分组。
    expect(
      wrapper.findAll('[data-testid="segment-deepseek-standard"] [data-testid^="row-deepseek-standard-"]')
    ).toHaveLength(2)
    expect(wrapper.find('[data-testid="row-deepseek-standard-101"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="row-deepseek-standard-102"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="row-deepseek-subscription-101"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="row-deepseek-subscription-102"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="row-zhipu-standard-201"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="row-kimi-standard-301"]').exists()).toBe(true)
  })

  it('calls setAccountLargeRequestOnly and syncs every row of the account', async () => {
    mockBackend(
      [
        makeGroup(1, 'deepseek-metered', 'deepseek', 'standard'),
        makeGroup(2, 'deepseek-subscription', 'deepseek', 'subscription')
      ],
      [
        makeAccount(101, 'deepseek', [1, 2]),
        makeAccount(102, 'deepseek', [1]),
        makeAccount(103, 'deepseek', [2])
      ]
    )
    setAccountLargeRequestOnly.mockResolvedValue({ id: 101, extra: { large_request_only: true } })

    const wrapper = mountView()
    await flushPromises()

    const toggles = wrapper.findAll('[data-testid="lro-toggle-101"]')
    expect(toggles).toHaveLength(2)

    toggleCheckbox(toggles[0].element, true)
    await flushPromises()

    expect(setAccountLargeRequestOnly).toHaveBeenCalledTimes(1)
    expect(setAccountLargeRequestOnly).toHaveBeenCalledWith(101, true)

    for (const toggle of wrapper.findAll('[data-testid="lro-toggle-101"]')) {
      expect((toggle.element as HTMLInputElement).checked).toBe(true)
    }
  })

  it('disables the switch of the last account left in the pool', async () => {
    mockBackend(
      [makeGroup(1, 'deepseek-metered', 'deepseek', 'standard')],
      [
        makeAccount(101, 'deepseek', [1]),
        makeAccount(102, 'deepseek', [1], { extra: { large_request_only: true } })
      ]
    )

    const wrapper = mountView()
    await flushPromises()

    const lastAccount = wrapper.get('[data-testid="lro-toggle-101"]').element as HTMLInputElement
    expect(lastAccount.disabled).toBe(true)

    // 已启用的账号始终允许取消，不受池内最后一个保护限制。
    const enabledAccount = wrapper.get('[data-testid="lro-toggle-102"]').element as HTMLInputElement
    expect(enabledAccount.disabled).toBe(false)
    expect(enabledAccount.checked).toBe(true)
  })

  it('shows the error banner and rolls the toggle back when the pool is exhausted', async () => {
    mockBackend(
      [makeGroup(1, 'deepseek-metered', 'deepseek', 'standard')],
      [makeAccount(101, 'deepseek', [1]), makeAccount(102, 'deepseek', [1])]
    )
    setAccountLargeRequestOnly.mockRejectedValue({
      code: 'LARGE_REQUEST_ONLY_POOL_EXHAUSTED',
      message: '池内最后一个可用账号不可开启大请求专属'
    })

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-testid="lro-error-banner"]').exists()).toBe(false)

    const toggle = wrapper.get('[data-testid="lro-toggle-101"]')
    expect((toggle.element as HTMLInputElement).disabled).toBe(false)
    toggleCheckbox(toggle.element, true)
    await flushPromises()

    expect(setAccountLargeRequestOnly).toHaveBeenCalledWith(101, true)
    const banner = wrapper.get('[data-testid="lro-error-banner"]')
    expect(banner.text()).toContain('池内最后一个可用账号不可开启大请求专属')
    expect((wrapper.get('[data-testid="lro-toggle-101"]').element as HTMLInputElement).checked).toBe(
      false
    )
  })
})
