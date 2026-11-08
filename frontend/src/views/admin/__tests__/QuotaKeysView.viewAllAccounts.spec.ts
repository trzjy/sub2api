import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import QuotaKeysView from '../QuotaKeysView.vue'
import { adminAPI } from '@/api/admin'
import * as adminUpstreams from '@/api/admin/quotaKeys'

// 激活码详情弹窗 → 账号管理页深链跳转：
//   - 详情弹窗的账号列表区域提供「查看该码的全部账号」入口；
//   - 点击跳到 /admin/accounts，并带 quota_key_id=<该激活码 id>，
//     账号页据此自动应用「所属激活码」筛选（接收端在 AccountsView，本页不改）；
//   - 入口只在详情弹窗打开后出现（详情内容受 v-if="detail" 保护）。
const { routerPush } = vi.hoisted(() => ({ routerPush: vi.fn() }))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: routerPush }),
  useRoute: () => ({ path: '/admin/quota-keys', query: {} })
}))

// QuotaKeysView 直接从 '@/api/admin/quotaKeys' 取上游列表/更新方法，
// 与 '@/api/admin' 的命名空间 mock 并存（与 upstreamStore.spec.ts 同型）。
vi.mock('@/api/admin/quotaKeys', () => ({
  listUpstreams: vi.fn(),
  updateUpstream: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    quotaKeys: {
      list: vi.fn(),
      get: vi.fn(),
      probeQuota: vi.fn(),
      refresh: vi.fn(),
      clearExhausted: vi.fn(),
      remove: vi.fn(),
      sweepExpired: vi.fn(),
      update: vi.fn(),
      create: vi.fn(),
      refreshAutoPool: vi.fn(),
      batchImportAutoCards: vi.fn()
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    // 与既有 spec 一致：未特殊处理的 key 回显 key 本身，便于断言可见文本。
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const stdRow = {
  id: 7,
  status: 'active',
  exhausted: false,
  code_masked: '****std0',
  notes: null,
  last_refresh_at: null,
  last_refresh_error: null,
  expires_at: null,
  account_count: 2,
  working_count: 2,
  priority: 1
}

function mountView() {
  return mount(QuotaKeysView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template: '<div><slot name="filters" /><slot name="table" /></div>'
        },
        DataTable: {
          props: ['columns', 'data', 'loading'],
          template: `<div class="datatable-stub">
            <template v-for="(row, i) in data" :key="i">
              <slot name="cell-actions" :row="row" />
            </template>
          </div>`
        },
        // BaseDialog 桩不按 show 隐藏内容，便于直接断言详情弹窗里的入口。
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
        PriorityStepper: true,
        QuotaKeyUsageCell: true,
        Pagination: true,
        ConfirmDialog: true,
        EmptyState: true,
        TextArea: true,
        Icon: true,
        AutoEntryConfigModal: true
      }
    }
  })
}

describe('QuotaKeysView 详情弹窗「查看该码的全部账号」跳转入口', () => {
  beforeEach(() => {
    routerPush.mockReset()
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.get).mockReset()
    vi.mocked(adminUpstreams.listUpstreams).mockReset()
    vi.mocked(adminUpstreams.updateUpstream).mockReset()

    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [stdRow],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    } as never)
    vi.mocked(adminAPI.quotaKeys.get).mockResolvedValue({
      ...stdRow,
      accounts: [],
      total: 0,
      page: 1,
      page_size: 20
    } as never)
    vi.mocked(adminUpstreams.listUpstreams).mockResolvedValue({ items: [] } as never)
  })

  it('入口只在详情弹窗打开后出现', async () => {
    const wrapper = mountView()
    await flushPromises()

    // 详情未加载时，入口不存在（详情内容受 v-if="detail" 保护）。
    expect(wrapper.find('[data-testid="quota-key-view-all-accounts"]').exists()).toBe(false)

    const detailBtn = wrapper
      .findAll('button')
      .find((b) => b.attributes('title') === 'admin.quotaKeys.actions.detail')
    expect(detailBtn).toBeDefined()
    await detailBtn!.trigger('click')
    await flushPromises()

    const entry = wrapper.find('[data-testid="quota-key-view-all-accounts"]')
    expect(entry.exists()).toBe(true)
    expect(entry.text()).toContain('admin.quotaKeys.detail.viewAllAccounts')
    wrapper.unmount()
  })

  it('点击入口跳到 /admin/accounts 并带上该激活码的 quota_key_id', async () => {
    const wrapper = mountView()
    await flushPromises()

    const detailBtn = wrapper
      .findAll('button')
      .find((b) => b.attributes('title') === 'admin.quotaKeys.actions.detail')
    await detailBtn!.trigger('click')
    await flushPromises()

    const entry = wrapper.find('[data-testid="quota-key-view-all-accounts"]')
    expect(entry.exists()).toBe(true)
    await entry.trigger('click')
    await flushPromises()

    expect(routerPush).toHaveBeenCalledTimes(1)
    expect(routerPush).toHaveBeenCalledWith({
      path: '/admin/accounts',
      query: { quota_key_id: '7' }
    })
    wrapper.unmount()
  })
})
