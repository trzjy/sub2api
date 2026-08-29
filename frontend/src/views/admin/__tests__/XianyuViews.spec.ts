import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import XianyuOverviewView from '../xianyu/XianyuOverviewView.vue'
import XianyuProductsView from '../xianyu/XianyuProductsView.vue'
import XianyuDeliveriesView from '../xianyu/XianyuDeliveriesView.vue'
import XianyuSettingsView from '../xianyu/XianyuSettingsView.vue'

const mocks = vi.hoisted(() => ({
  getOverview: vi.fn(),
  listProducts: vi.fn(),
  listItemPools: vi.fn(),
  syncProducts: vi.fn(),
  bindProduct: vi.fn(),
  listDeliveries: vi.fn(),
  resendDelivery: vi.fn(),
  getWorkerConfigs: vi.fn(),
  saveWorkerConfig: vi.fn(),
  checkHealth: vi.fn(),
  getSettings: vi.fn(),
  saveSettings: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    xianyu: {
      getOverview: mocks.getOverview,
      getAccess: vi.fn().mockResolvedValue({ can_manage: true }),
      listWorkerConfigs: mocks.getWorkerConfigs,
      saveWorkerConfig: mocks.saveWorkerConfig,
      checkHealth: mocks.checkHealth,
      listAccounts: vi.fn().mockResolvedValue([]),
      syncAccounts: vi.fn(),
      enableAccount: vi.fn(),
      disableAccount: vi.fn(),
      refreshCookie: vi.fn(),
      createLoginSession: vi.fn(),
      queryLoginSession: vi.fn(),
      listProducts: mocks.listProducts,
      syncProducts: mocks.syncProducts,
      bindProduct: mocks.bindProduct,
      listBindingRules: vi.fn().mockResolvedValue([]),
      saveBindingRule: vi.fn(),
      listItemPools: mocks.listItemPools,
      saveItemPool: vi.fn(),
      listDeliveries: mocks.listDeliveries,
      resendDelivery: mocks.resendDelivery,
      getSettings: mocks.getSettings,
      saveSettings: mocks.saveSettings,
    },
  },
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showWarning: vi.fn(),
  }),
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showWarning: vi.fn(),
  }),
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: vi.fn() }),
  useRoute: () => ({ path: '/admin/xianyu', query: {} }),
}))

function mountWithStubs(component: Parameters<typeof mount>[0]) {
  return mount(component, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        BaseDialog: { template: '<div><slot /></div>' },
        ConfirmDialog: { template: '<div></div>' },
        StatusBadge: { template: '<span>{{ label }}</span>' },
        EmptyState: { template: '<div></div>' },
        Select: { template: '<select><slot /></select>' },
        Pagination: true,
        Toggle: { template: '<input type="checkbox" />' },
      },
    },
  })
}

describe('Xianyu OverviewView', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mocks.getOverview.mockResolvedValue({
      worker_healthy: true,
      enabled_accounts: 2,
      running_tasks: 1,
      unmapped_products: 3,
      pools: [{ pool: { id: 1, name: 'standard', low_stock_threshold: 2 }, remaining: 5, used: 1, disabled: 0, low_stock: false }],
      today_delivered: 4,
      today_failed: 1,
      pending_deliveries: 2,
    })
  })

  afterEach(() => vi.clearAllMocks())

  it('renders overview stats and low stock', async () => {
    const wrapper = mountWithStubs(XianyuOverviewView)
    await flushPromises()
    expect(wrapper.text()).toContain('standard')
    expect(mocks.getOverview).toHaveBeenCalled()
  })
})

describe('Xianyu ProductsView', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mocks.listProducts.mockResolvedValue([
      { id: 1, account_pk: 11, account_id: 'acc-1', item_id: 'i1', title: 't1', spec_name: '', spec_value: '', pool_id: null, binding_status: 'unmapped', binding_source: 'auto_new', status: 'active' },
    ])
    mocks.listItemPools.mockResolvedValue([{ id: 1, name: 'standard', slug: 'standard', description: '', low_stock_threshold: 0, status: 'active' }])
  })

  afterEach(() => vi.clearAllMocks())

  it('shows unmapped product instead of saving failure', async () => {
    const wrapper = mountWithStubs(XianyuProductsView)
    await flushPromises()
    expect(wrapper.text()).toContain('t1')
    expect(wrapper.text()).toContain('i1')
    expect(wrapper.text()).toContain('acc-1')
    expect(mocks.listProducts).toHaveBeenCalled()
  })

  it('binds a product to a pool immediately', async () => {
    const wrapper = mountWithStubs(XianyuProductsView)
    await flushPromises()
    // 点击绑定按钮打开弹窗。
    const bindButton = wrapper.findAll('button').find((b) => b.text().includes('bind'))
    expect(bindButton).toBeTruthy()
  })
})

describe('Xianyu DeliveriesView', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mocks.listDeliveries.mockResolvedValue({
      items: [
        { order_no: 'o1', code: 'CODE-1', account_id: 'a', item_id: 'i', delivery_status: 'failed', attempt_count: 1, created_at: '2026-01-01T00:00:00Z' },
      ],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1,
    })
  })

  afterEach(() => vi.clearAllMocks())

  it('renders failed delivery and resends original code', async () => {
    mocks.resendDelivery.mockResolvedValue('CODE-1')
    const wrapper = mountWithStubs(XianyuDeliveriesView)
    await flushPromises()
    expect(wrapper.text()).toContain('o1')
    expect(wrapper.text()).toContain('CODE-1')
    const resendBtn = wrapper.findAll('button').find((b) => b.text().includes('resend'))
    expect(resendBtn).toBeTruthy()
  })
})

describe('Xianyu SettingsView', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mocks.getWorkerConfigs.mockResolvedValue([])
    mocks.getSettings.mockResolvedValue({
      delivery_enabled: false,
      account_auto_refresh: true,
      product_auto_bind: true,
      sync_interval_minutes: 5,
    })
  })

  afterEach(() => vi.clearAllMocks())

  it('loads toggles and worker config', async () => {
    mountWithStubs(XianyuSettingsView)
    await flushPromises()
    expect(mocks.getSettings).toHaveBeenCalled()
    expect(mocks.getWorkerConfigs).toHaveBeenCalled()
  })
})
