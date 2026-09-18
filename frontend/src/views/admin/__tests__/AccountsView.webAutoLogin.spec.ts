import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'

// AccountsView 通过 useRoute() 注入读取 URL query，挂载时必须提供 router，
// 否则 Vue 会报 injection "Symbol(route location)" not found。
function createTestRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: { template: '<div />' } }]
  })
}

const {
  listAccounts,
  listWithEtag,
  getById,
  getBatchTodayStats,
  getUpstreamBillingProbeSettings,
  getAllProxies,
  getAllGroups,
  showError,
  showSuccess,
  showWarning,
  showInfo
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getById: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
  showWarning: vi.fn(),
  showInfo: vi.fn()
}))

const webAutoLogin = vi.hoisted(() => ({
  batchLogin: vi.fn(),
  batchTest: vi.fn(),
  batchDeleteBanned: vi.fn(),
  batchStatus: vi.fn(),
  exportWebAccounts: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      getById,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings,
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn()
    },
    proxies: { getAll: getAllProxies },
    groups: { getAll: getAllGroups }
  }
}))
vi.mock('@/api/admin/webAutoLogin', () => ({
  webAutoLoginAPI: webAutoLogin
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess, showWarning, showInfo })
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token', isSimpleMode: false })
}))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const DataTableStub = defineComponent({
  props: { data: { type: Array, default: () => [] } },
  template: `
    <div>
      <div v-for="row in data" :key="row.id">
        <slot name="cell-loginStatus" :row="row" />
        <slot name="cell-status" :row="row" />
      </div>
    </div>
  `
})

function mountView() {
  const router = createTestRouter()
  return mount(AccountsView, {
    attachTo: document.body,
    global: {
      plugins: [router],
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
        DataTable: DataTableStub,
        AccountTableActions: { template: '<div><slot name="after" /></div>' },
        AccountTableFilters: true,
        AccountBulkActionsBar: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountActionMenu: true,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: true,
        AccountStatsModal: true,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: true,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: true,
        AccountUsageCell: true,
        UpstreamBillingRateCell: true,
        HelpTooltip: true,
        Icon: true,
        Teleport: true
      }
    }
  })
}

import AccountsView from '../AccountsView.vue'

const webAccount = (over: Record<string, unknown> = {}) => ({
  id: 1,
  name: 'web-acct',
  platform: 'deepseek',
  type: 'apikey',
  status: 'active',
  schedulable: true,
  concurrency: 2,
  priority: 1,
  group_ids: [7],
  extra: {},
  // 平台归并后 web 账号必须带 credentials.access_mode="web" 才被判定为网页接入。
  credentials: { access_mode: 'web', cookie: 'ck=1' },
  ...over
})

beforeEach(() => {
  localStorage.clear()
  vi.spyOn(window, 'confirm').mockReturnValue(true)
  listAccounts.mockReset().mockResolvedValue({ items: [webAccount()], total: 1, page: 1, page_size: 20, pages: 1 })
  listWithEtag.mockReset().mockResolvedValue({ notModified: true, etag: 'etag', data: null })
  getById.mockReset().mockResolvedValue(webAccount())
  getBatchTodayStats.mockReset().mockResolvedValue({ stats: {} })
  getUpstreamBillingProbeSettings.mockReset().mockResolvedValue({ enabled: true })
  getAllProxies.mockReset().mockResolvedValue([])
  getAllGroups.mockReset().mockResolvedValue([{ id: 7, name: 'g', platform: 'deepseek' }])
  showError.mockReset()
  showSuccess.mockReset()
  showWarning.mockReset()
  showInfo.mockReset()
})

afterEach(() => {
  vi.restoreAllMocks()
})

async function flush() {
  await flushPromises()
  await flushPromises()
  await flushPromises()
}

describe('AccountsView web auto-login batch actions', () => {
  it('one-click login calls batchLogin and shows a success toast', async () => {
    webAutoLogin.batchLogin.mockResolvedValue({ results: [{ id: 1, recovered: true, needs_sms: false }], summary: { success: 1, failed: 0 } })
    const wrapper = mountView()
    await flush()
    wrapper.vm.setSelectedIds([1])
    await wrapper.vm.handleBulkWebLogin()
    await flush()

    expect(webAutoLogin.batchLogin).toHaveBeenCalledWith([1])
    expect(showSuccess).toHaveBeenCalled()
  })

  it('one-click test calls batchTest', async () => {
    webAutoLogin.batchTest.mockResolvedValue({ results: [{ id: 1, success: true }] })
    const wrapper = mountView()
    await flush()
    wrapper.vm.setSelectedIds([1])
    await wrapper.vm.handleBulkWebTest()
    await flush()

    expect(webAutoLogin.batchTest).toHaveBeenCalledWith([1])
    expect(showSuccess).toHaveBeenCalled()
  })

  it('delete banned runs the two-step confirm flow', async () => {
    webAutoLogin.batchDeleteBanned
      .mockResolvedValueOnce({ candidates: [{ id: 1, name: 'web-acct', reason: 'banned' }], deleted: 0 })
      .mockResolvedValueOnce({ candidates: [{ id: 1, name: 'web-acct', reason: 'banned' }], deleted: 1 })
    const wrapper = mountView()
    await flush()
    wrapper.vm.setSelectedIds([1])
    await wrapper.vm.handleBulkDeleteBanned()
    await flush()

    expect(webAutoLogin.batchDeleteBanned).toHaveBeenCalledTimes(2)
    expect(webAutoLogin.batchDeleteBanned).toHaveBeenLastCalledWith(true)
    expect(showSuccess).toHaveBeenCalled()
  })

  it('batch enable calls batchStatus with active', async () => {
    webAutoLogin.batchStatus.mockResolvedValue({ success: 1, failed: 0 })
    const wrapper = mountView()
    await flush()
    wrapper.vm.setSelectedIds([1])
    await wrapper.vm.handleBulkWebStatus('active')
    await flush()

    expect(webAutoLogin.batchStatus).toHaveBeenCalledWith({ ids: [1], status: 'active' })
    expect(showSuccess).toHaveBeenCalled()
  })

  it('export web requires a web-platform selection', async () => {
    webAutoLogin.exportWebAccounts.mockResolvedValue(new Blob(['{}'], { type: 'application/json' }))
    // 初始列表加载一个非 web（openai）账号：选中项必须真的是非 web 平台，
    // 否则 handleBulkExportWeb 会从 accounts.value 读到 web 平台而误触发导出。
    listAccounts.mockResolvedValue({ items: [webAccount({ id: 1, platform: 'openai' })], total: 1, page: 1, page_size: 20, pages: 1 })
    const wrapper = mountView()
    await flush()
    wrapper.vm.setSelectedIds([1])
    await wrapper.vm.handleBulkExportWeb()
    await flush()
    expect(webAutoLogin.exportWebAccounts).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalled()
  })

  it('renders a login-status badge for each account', async () => {
    listAccounts.mockResolvedValue({
      items: [
        webAccount({ id: 1, status: 'active', credentials: { access_mode: 'web', cookie: 'ck' } }),
        webAccount({ id: 2, status: 'error', error_message: 'token expired', credentials: { access_mode: 'web', cookie: 'ck' } }),
        webAccount({ id: 3, credentials: { access_mode: 'web' }, error_message: null }),
        webAccount({ id: 4, status: 'active', credentials: { access_mode: 'web', cookie: 'ck' }, error_message: '账号已被封禁' })
      ],
      total: 4,
      page: 1,
      page_size: 20,
      pages: 1
    })
    const wrapper = mountView()
    await flush()

    const badges = wrapper.findAll('[data-testid="login-status-badge"]')
    expect(badges.length).toBe(4)
    // active -> green, failed -> amber, unconfigured web -> gray, banned -> red
    expect(badges[0].classes()).toContain('bg-green-100')
    expect(badges[1].classes()).toContain('bg-amber-100')
    expect(badges[2].classes()).toContain('bg-gray-100')
    expect(badges[3].classes()).toContain('bg-red-100')
  })

  it('does not surface web login state for a same-platform ordinary API account', async () => {
    // zhipu + apikey、无 access_mode：platform 命中官方平台，但属于普通 API 账号，
    // 不得显示 web 专属的 unconfigured 登录态。
    listAccounts.mockResolvedValue({
      items: [webAccount({ id: 1, platform: 'zhipu', type: 'apikey', status: 'active', credentials: {} })],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })
    const wrapper = mountView()
    await flush()

    const badges = wrapper.findAll('[data-testid="login-status-badge"]')
    expect(badges.length).toBe(1)
    expect(badges[0].text()).toBe('admin.accounts.loginStatus.active')
    expect(badges[0].classes()).toContain('bg-green-100')
    expect(badges[0].classes()).not.toContain('bg-gray-100')
  })

  it('blocks web export for a same-platform ordinary API account', async () => {
    webAutoLogin.exportWebAccounts.mockResolvedValue(new Blob(['{}'], { type: 'application/json' }))
    listAccounts.mockResolvedValue({
      items: [webAccount({ id: 1, platform: 'zhipu', type: 'apikey', credentials: {} })],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })
    const wrapper = mountView()
    await flush()
    wrapper.vm.setSelectedIds([1])
    await wrapper.vm.handleBulkExportWeb()
    await flush()

    expect(webAutoLogin.exportWebAccounts).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.accounts.batch.selectWebOnly')
  })

  it('blocks web export when the selection mixes a web account with an API account', async () => {
    // 第一个选中项是 web 账号：必须校验全部选中项，不能只看第一个。
    webAutoLogin.exportWebAccounts.mockResolvedValue(new Blob(['{}'], { type: 'application/json' }))
    listAccounts.mockResolvedValue({
      items: [
        webAccount({ id: 1, platform: 'deepseek', credentials: { access_mode: 'web', cookie: 'ck' } }),
        webAccount({ id: 2, platform: 'zhipu', type: 'apikey', credentials: {} })
      ],
      total: 2,
      page: 1,
      page_size: 20,
      pages: 1
    })
    const wrapper = mountView()
    await flush()
    wrapper.vm.setSelectedIds([1, 2])
    await wrapper.vm.handleBulkExportWeb()
    await flush()

    expect(webAutoLogin.exportWebAccounts).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.accounts.batch.selectWebOnly')
  })
})
