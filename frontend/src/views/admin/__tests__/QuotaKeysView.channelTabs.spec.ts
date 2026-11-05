import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import QuotaKeysView from '../QuotaKeysView.vue'
import { adminAPI } from '@/api/admin'

// 激活码页 Tab 数据分流回归测试（派发单 F）：
//   - 标准 Tab 只显示 notes 非 `auto_pool:` 前缀的卡密；
//   - Auto Tab 只显示 notes 以 `auto_pool:` 前缀的卡密；
//   - Auto Tab 空状态不渲染「新建激活码」动作（action-text 为 undefined）。
// 复用既有 spec 的 mock 方式（@/api/admin、@/stores/app、vue-i18n），
// 并沿用其挂载结构，仅额外把 DataTable / EmptyState 桩渲染出可供包含性断言的文本。

const stdRow = {
  id: 1,
  status: 'active',
  exhausted: false,
  code_masked: '****std0',
  notes: null,
  last_refresh_at: null,
  last_refresh_error: null,
  expires_at: '2026-12-31T00:00:00.000Z',
  account_count: 0,
  working_count: 0
}

const autoRow = {
  id: 2,
  status: 'active',
  exhausted: false,
  code_masked: '****auto',
  notes: 'auto_pool:dev=AAA;md5=BBB',
  last_refresh_at: null,
  last_refresh_error: null,
  expires_at: '2026-12-31T00:00:00.000Z',
  account_count: 0,
  working_count: 0
}

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

// 渲染 DataTable 的行数据（code_masked）与 empty 槽，供包含性断言使用。
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
              <span class="cell-code">{{ row.code_masked }}</span>
              <slot name="cell-actions" :row="row" />
            </template>
            <slot name="empty" />
          </div>`
        },
        PriorityStepper: true,
        QuotaKeyUsageCell: true,
        Pagination: true,
        ConfirmDialog: true,
        // 渲染标题与 action 文本，便于断言 Auto Tab 空状态无「新建激活码」动作。
        EmptyState: {
          props: ['title', 'description', 'actionText'],
          template: `<div class="empty-state-stub">
            <span class="empty-title">{{ title }}</span>
            <span v-if="actionText" class="empty-action">{{ actionText }}</span>
          </div>`
        },
        TextArea: true,
        Icon: true,
        AutoEntryConfigModal: true,
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }
      }
    }
  })
}

function listWith(items: unknown[]) {
  return { items, total: items.length, page: 1, page_size: 20, pages: 1 } as never
}

describe('QuotaKeysView 渠道 Tab 数据分流（派发单 F）', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.get).mockReset()
    vi.mocked(adminAPI.quotaKeys.probeQuota).mockReset()
  })

  it('标准 Tab 只显示非 Auto 卡密', async () => {
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue(
      listWith([autoRow, stdRow])
    )

    const wrapper = mountView()
    await flushPromises()

    const text = wrapper.text()
    // 标准 Tab（默认）应渲染非 Auto 行。
    expect(text).toContain('****std0')
    // 不应渲染 Auto 行。
    expect(text).not.toContain('****auto')
    wrapper.unmount()
  })

  it('切到 Auto Tab 只显示 Auto 卡密', async () => {
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue(
      listWith([autoRow, stdRow])
    )

    const wrapper = mountView()
    await flushPromises()

    // 切到「Cursor 极速号池 (Auto动态池)」Tab。
    const autoBtn = wrapper
      .findAll('button')
      .find((b) => b.text().includes('Cursor 极速号池 (Auto动态池)'))
    expect(autoBtn).toBeDefined()
    await autoBtn!.trigger('click')
    await nextTick()
    await flushPromises()

    const text = wrapper.text()
    // Auto Tab 应渲染 Auto 行。
    expect(text).toContain('****auto')
    // 不应渲染非 Auto 行。
    expect(text).not.toContain('****std0')
    wrapper.unmount()
  })

  it('Auto Tab 空状态不提供「新建激活码」动作', async () => {
    // 仅返回标准行（无 Auto 行）。
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue(listWith([stdRow]))

    const wrapper = mountView()
    await flushPromises()

    const autoBtn = wrapper
      .findAll('button')
      .find((b) => b.text().includes('Cursor 极速号池 (Auto动态池)'))
    expect(autoBtn).toBeDefined()
    await autoBtn!.trigger('click')
    await nextTick()
    await flushPromises()

    const text = wrapper.text()
    // 显示空状态文案（i18n key 回显值）。
    expect(text).toContain('admin.quotaKeys.emptyTitle')
    // Auto Tab 空状态不提供「新建激活码」动作文本。
    expect(text).not.toContain('admin.quotaKeys.create')
    wrapper.unmount()
  })

  it('Auto 行不提供编辑入口（notes 承载设备绑定凭据，禁止改写）', async () => {
    const editButtons = (wrapper: ReturnType<typeof mountView>) =>
      wrapper
        .findAll('button')
        .filter((b) => b.attributes('title') === 'admin.quotaKeys.actions.edit')

    // 标准 Tab：标准行有编辑按钮。
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue(listWith([stdRow]))
    const stdWrapper = mountView()
    await flushPromises()
    expect(editButtons(stdWrapper)).toHaveLength(1)
    stdWrapper.unmount()

    // Auto Tab：Auto 行没有编辑按钮（优先级仍在列表内直接改）。
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue(listWith([autoRow]))
    const autoWrapper = mountView()
    await flushPromises()
    const autoBtn = autoWrapper
      .findAll('button')
      .find((b) => b.text().includes('Cursor 极速号池 (Auto动态池)'))
    expect(autoBtn).toBeDefined()
    await autoBtn!.trigger('click')
    await nextTick()
    await flushPromises()
    expect(autoWrapper.text()).toContain('****auto')
    expect(editButtons(autoWrapper)).toHaveLength(0)
    autoWrapper.unmount()
  })
})
