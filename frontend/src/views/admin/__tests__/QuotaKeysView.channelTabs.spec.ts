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

// QuotaKeysView 详情弹窗新增「查看该码的全部账号」跳转入口后，setup 内会取 router；
// 既有断言不涉及路由，这里补一个 router 桩避免注入告警（同 DashboardView.spec.ts 写法）。
vi.mock('vue-router', () => ({
  useRouter: () => ({ push: vi.fn() }),
  useRoute: () => ({ path: '/admin/quota-keys', query: {} })
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
        TextArea: {
          props: ['modelValue', 'disabled'],
          emits: ['update:modelValue'],
          template: '<textarea :value="modelValue" :disabled="disabled" @input="$emit(\'update:modelValue\', $event.target.value)" />'
        },
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

  it('Auto 行提供编辑入口，且编辑时 notes 得到保护（CARD-C）', async () => {
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

    // Auto Tab：Auto 行也提供编辑按钮（CARD-C 恢复入口）。
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
    expect(editButtons(autoWrapper)).toHaveLength(1)

    // 点击编辑按钮打开弹窗。
    await editButtons(autoWrapper)[0].trigger('click')
    await nextTick()
    await flushPromises()

    // notes 输入框在 Auto 编辑时展示锁定保护提示（绑定凭据由系统管理）。
    expect(autoWrapper.text()).toContain(
      'Auto 动态号池绑定凭据由系统管理，不可直接改写（已锁定保护）。'
    )

    // Auto 编辑弹窗中的 notes 输入框处于 disabled 状态（保护绑定凭据）。
    expect(autoWrapper.find('textarea').attributes('disabled')).toBeDefined()

    // 提交：更新请求的 payload 不应包含 notes 字段，保护绑定的虚拟设备凭据。
    vi.mocked(adminAPI.quotaKeys.update).mockResolvedValue({} as never)
    const saveBtn = autoWrapper
      .findAll('button')
      .find((b) => b.text().includes('common.save'))
    expect(saveBtn).toBeDefined()
    await saveBtn!.trigger('click')
    await flushPromises()

    expect(adminAPI.quotaKeys.update).toHaveBeenCalledTimes(1)
    const updatePayload = vi.mocked(adminAPI.quotaKeys.update).mock.calls[0][1] as Record<string, unknown>
    expect(updatePayload).not.toHaveProperty('notes')
    expect(updatePayload).toHaveProperty('expires_at')
    expect(updatePayload).toHaveProperty('priority')

    autoWrapper.unmount()
  })

  it('Auto 批量导入弹窗支持选填到期时间并转换为 ISO 字符串提交', async () => {
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue(listWith([autoRow]))
    vi.mocked(adminAPI.quotaKeys.batchImportAutoCards).mockResolvedValue(
      { success_count: 0, failed_count: 0 } as never
    )

    const wrapper = mountView()
    await flushPromises()

    // 切到 Auto Tab。
    const autoBtn = wrapper
      .findAll('button')
      .find((b) => b.text().includes('Cursor 极速号池 (Auto动态池)'))
    expect(autoBtn).toBeDefined()
    await autoBtn!.trigger('click')
    await nextTick()
    await flushPromises()

    // 打开「批量导入 Auto 卡密」弹窗。
    const openBatchBtn = wrapper
      .findAll('button')
      .find((b) => b.text().includes('批量导入Auto卡密'))
    expect(openBatchBtn).toBeDefined()
    await openBatchBtn!.trigger('click')
    await nextTick()
    await flushPromises()

    // 找到最后一个 textarea（即批量导入弹窗内的 textarea）并赋值。
    const textareas = wrapper.findAll('textarea')
    const batchTextarea = textareas[textareas.length - 1]
    if (batchTextarea) {
      await batchTextarea.setValue('sky_cc_test123')
    }

    // 找到批量导入弹窗内的到期时间输入框（最后一个 datetime-local）并赋值。
    const dateInputs = wrapper.findAll('input[type="datetime-local"]')
    const dateInput = dateInputs[dateInputs.length - 1]
    if (dateInput && dateInput.exists()) {
      await dateInput.setValue('2026-12-31T12:00')
    }

    await nextTick()
    await flushPromises()

    // 提交：点击「开始批量绑定入库」。
    const submitBtn = wrapper
      .findAll('button')
      .find((b) => b.text().includes('开始批量绑定入库'))
    expect(submitBtn).toBeDefined()
    await submitBtn!.trigger('click')
    await flushPromises()

    expect(adminAPI.quotaKeys.batchImportAutoCards).toHaveBeenCalled()
    const callArgs = vi.mocked(adminAPI.quotaKeys.batchImportAutoCards).mock.calls[0]
    expect(callArgs[0]).toContain('sky_cc_test123')
    expect(callArgs[1]).toMatch(/^2026-12-31T\d{2}:00:00\.000Z$/)

    wrapper.unmount()
  })
})
