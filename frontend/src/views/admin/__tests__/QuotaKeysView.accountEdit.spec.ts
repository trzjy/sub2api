import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, nextTick } from 'vue'
import QuotaKeysView from '../QuotaKeysView.vue'
import { adminAPI } from '@/api/admin'

// 激活码详情弹窗内的账号编辑入口（按行）：
//   - 详情弹窗的账号行带「编辑」按钮；
//   - 点「编辑」先按 id 取完整账号（详情列表里的账号是脱敏最小投影），
//     再打开账号管理页的 EditAccountModal（复用，不复制表单）；
//   - 加载失败要报错，且不打开编辑弹窗。

const { showError, showSuccess, showInfo } = vi.hoisted(() => ({
  showError: vi.fn(),
  showSuccess: vi.fn(),
  showInfo: vi.fn()
}))

// QuotaKeysView 详情弹窗新增「查看该码的全部账号」跳转入口后，setup 内会取 router；
// 既有断言不涉及路由，这里补一个 router 桩避免注入告警（同 DashboardView.spec.ts 写法）。
vi.mock('vue-router', () => ({
  useRouter: () => ({ push: vi.fn() }),
  useRoute: () => ({ path: '/admin/quota-keys', query: {} })
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess, showInfo })
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
    },
    accounts: {
      getById: vi.fn()
    },
    proxies: {
      getAll: vi.fn()
    },
    groups: {
      getAll: vi.fn()
    }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
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

// 详情弹窗里的账号行：脱敏最小投影（只有 id/name/platform/status）
const accountRows = [
  { id: 548, name: 'kn-cy-apikey', platform: 'cursor', status: 'active' },
  { id: 491, name: 'shareai-oauth', platform: 'cursor', status: 'active' }
]

// getById 返回的完整账号：带 credentials / group_ids（EditAccountModal 表单要按它回填）
const fullAccount = {
  id: 548,
  name: 'kn-cy-apikey',
  platform: 'cursor',
  type: 'apikey',
  status: 'active',
  credentials: { base_url: 'https://kn-cy.example', api_key: 'secret' },
  group_ids: [3]
}

const EditAccountModalStub = defineComponent({
  name: 'EditAccountModal',
  props: {
    show: { type: Boolean, default: false },
    account: { type: Object, default: null },
    proxies: { type: Array, default: () => [] },
    groups: { type: Array, default: () => [] }
  },
  emits: ['close', 'updated'],
  template:
    '<div v-if="show" data-test="edit-account-modal" :data-account-id="account ? account.id : \'\'" :data-group-ids="account && account.group_ids ? account.group_ids.join(\',\') : \'\'"></div>'
})

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
        // BaseDialog 桩不按 show 隐藏内容，便于直接断言详情弹窗里的账号行
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
        EditAccountModal: EditAccountModalStub,
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

/** 走完「详情 → 展开账号列表」，返回已展开的 wrapper */
async function openDetailWithAccounts(wrapper: ReturnType<typeof mountView>) {
  await flushPromises()

  vi.mocked(adminAPI.quotaKeys.get).mockResolvedValue({
    ...stdRow,
    accounts: accountRows,
    total: accountRows.length,
    page: 1,
    page_size: 20
  } as never)

  const detailBtn = wrapper
    .findAll('button')
    .find((b) => b.attributes('title') === 'admin.quotaKeys.actions.detail')
  expect(detailBtn).toBeDefined()
  await detailBtn!.trigger('click')
  await flushPromises()

  // 账号列表默认折叠（§0.1 #7），需点开才渲染账号行。
  // 用文案定位而不是 aria-expanded（顶部「上游续费」按钮也有 aria-expanded）。
  const expandBtn = wrapper
    .findAll('button')
    .find((b) => b.text().includes('admin.quotaKeys.accountsTitle'))
  expect(expandBtn).toBeDefined()
  await expandBtn!.trigger('click')
  await nextTick()
  return wrapper
}

describe('QuotaKeysView 详情弹窗账号编辑入口', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.get).mockReset()
    vi.mocked(adminAPI.accounts.getById).mockReset()
    vi.mocked(adminAPI.proxies.getAll).mockReset()
    vi.mocked(adminAPI.groups.getAll).mockReset()
    showError.mockReset()
    showSuccess.mockReset()
    showInfo.mockReset()

    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [stdRow],
      total: 1,
      page: 1,
      page_size: 20,
      quota_key_summaries: []
    } as never)
    vi.mocked(adminAPI.proxies.getAll).mockResolvedValue([] as never)
    vi.mocked(adminAPI.groups.getAll).mockResolvedValue([] as never)
  })

  it('详情弹窗的每个账号行都有编辑入口', async () => {
    const wrapper = await openDetailWithAccounts(mountView())

    const editButtons = wrapper.findAll('[data-testid="quota-key-account-edit"]')
    expect(editButtons).toHaveLength(accountRows.length)
    // 未点编辑前不打开账号编辑弹窗
    expect(wrapper.find('[data-test="edit-account-modal"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('点编辑按 id 拉取完整账号并打开账号编辑弹窗', async () => {
    vi.mocked(adminAPI.accounts.getById).mockResolvedValue(fullAccount as never)

    const wrapper = await openDetailWithAccounts(mountView())
    const editButtons = wrapper.findAll('[data-testid="quota-key-account-edit"]')
    await editButtons[0].trigger('click')
    await flushPromises()

    // 按账号 id 取完整账号（详情行只是脱敏最小投影）
    expect(adminAPI.accounts.getById).toHaveBeenCalledWith(accountRows[0].id)
    // 编辑弹窗拿到的是完整账号（含 group_ids），且已打开
    const modal = wrapper.find('[data-test="edit-account-modal"]')
    expect(modal.exists()).toBe(true)
    expect(modal.attributes('data-account-id')).toBe(String(fullAccount.id))
    expect(modal.attributes('data-group-ids')).toBe('3')
    wrapper.unmount()
  })

  it('账号详情加载失败时报错且不打开编辑弹窗', async () => {
    // 拦截器 reject 的是普通对象，不是 Error 实例
    vi.mocked(adminAPI.accounts.getById).mockRejectedValue({ message: 'boom' } as never)

    const wrapper = await openDetailWithAccounts(mountView())
    const editButtons = wrapper.findAll('[data-testid="quota-key-account-edit"]')
    await editButtons[0].trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('boom')
    expect(wrapper.find('[data-test="edit-account-modal"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('账号保存后重新拉取激活码详情', async () => {
    vi.mocked(adminAPI.accounts.getById).mockResolvedValue(fullAccount as never)

    const wrapper = await openDetailWithAccounts(mountView())
    await wrapper.findAll('[data-testid="quota-key-account-edit"]')[0].trigger('click')
    await flushPromises()

    const callsBefore = vi.mocked(adminAPI.quotaKeys.get).mock.calls.length
    wrapper.findComponent(EditAccountModalStub).vm.$emit('updated', fullAccount)
    await flushPromises()

    // 编辑后账号 status 等可能变化 → 重拉详情
    expect(vi.mocked(adminAPI.quotaKeys.get).mock.calls.length).toBe(callsBefore + 1)
    wrapper.unmount()
  })

  it('关闭账号编辑弹窗不影响详情弹窗', async () => {
    vi.mocked(adminAPI.accounts.getById).mockResolvedValue(fullAccount as never)

    const wrapper = await openDetailWithAccounts(mountView())
    await wrapper.findAll('[data-testid="quota-key-account-edit"]')[0].trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-test="edit-account-modal"]').exists()).toBe(true)

    // 关内层：EditAccountModal 自身 emit('close')
    wrapper.findComponent(EditAccountModalStub).vm.$emit('close')
    await flushPromises()

    expect(wrapper.find('[data-test="edit-account-modal"]').exists()).toBe(false)
    // 详情弹窗仍开着（账号行还在）
    expect(wrapper.findAll('[data-testid="quota-key-account-edit"]').length).toBe(accountRows.length)
    wrapper.unmount()
  })
})
