import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import QuotaKeysView from '../QuotaKeysView.vue'
import { adminAPI } from '@/api/admin'
import * as adminUpstreams from '@/api/admin/quotaKeys'

// KC20-02：创建激活码对话框新增「所属上游」原生下拉，随创建请求提交 upstream_key。
// 脚手架照抄同目录既有 QuotaKeysView.upstreamStore.spec.ts。
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
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

function mountView() {
  return mount(QuotaKeysView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template: '<div><slot name="filters" /><slot name="table" /></div>'
        },
        DataTable: true,
        ConfirmDialog: true,
        EmptyState: true,
        TextArea: true,
        Icon: true,
        AutoEntryConfigModal: true,
        // BaseDialog 按 show 条件渲染，才能拿到对话框内的 DOM（含 upstream-select）。
        BaseDialog: {
          props: ['show', 'title'],
          template: `<div v-if="show" class="dialog-stub" :data-title="title">
            <slot />
            <slot name="footer" />
          </div>`
        }
      }
    }
  })
}

const upstreamsFixture = [
  { id: 1, key: 'shareai', name: 'ShareAI 号池', type: 'shareai', renewal_url: null, notes: null },
  { id: 2, key: 'kn_cy', name: 'kn-cy.com 网关', type: 'kn_cy', renewal_url: 'https://shop.kn-cy.com/products', notes: null }
]

function findButtonByText(wrapper: ReturnType<typeof mountView>, text: string) {
  return wrapper.findAll('button').find((b) => b.text().trim() === text)
}

describe('QuotaKeysView 创建对话框所属上游下拉（KC20-02）', () => {
  beforeEach(() => {
    vi.mocked(adminUpstreams.listUpstreams).mockReset()
    vi.mocked(adminUpstreams.listUpstreams).mockResolvedValue({ items: upstreamsFixture } as never)
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [],
      total: 0,
      page: 1,
      page_size: 20,
      pages: 1
    } as never)
    vi.mocked(adminAPI.quotaKeys.create).mockReset()
    vi.mocked(adminAPI.quotaKeys.create).mockResolvedValue({ id: 1 } as never)
  })

  it('打开创建对话框后存在 upstream-select，且选项含 kn_cy', async () => {
    const wrapper = mountView()
    await flushPromises()

    const createBtn = findButtonByText(wrapper, 'admin.quotaKeys.create')
    expect(createBtn).toBeTruthy()
    await createBtn!.trigger('click')
    await nextTick()
    await flushPromises()

    const select = wrapper.find('[data-testid="upstream-select"]')
    expect(select.exists()).toBe(true)

    const options = wrapper.findAll('[data-testid="upstream-select"] option')
    const kncyOption = options.find((o) => o.attributes('value') === 'kn_cy')
    expect(kncyOption).toBeTruthy()
    expect(kncyOption!.text()).toContain('kn-cy.com 网关 (kn_cy)')
    wrapper.unmount()
  })

  it('选中 kn_cy 后提交，create 调用参数含 upstream_key="kn_cy"', async () => {
    const wrapper = mountView()
    await flushPromises()

    const createBtn = findButtonByText(wrapper, 'admin.quotaKeys.create')
    await createBtn!.trigger('click')
    await nextTick()
    await flushPromises()

    // 激活码为必填，先填明文 code。
    const codeInput = wrapper.find('input[type="text"]')
    expect(codeInput.exists()).toBe(true)
    await codeInput.setValue('SKY_TEST_CODE_001')
    await nextTick()

    // 选中 kn_cy。
    const select = wrapper.find('[data-testid="upstream-select"]')
    await select.setValue('kn_cy')
    await nextTick()

    const saveBtn = findButtonByText(wrapper, 'common.save')
    expect(saveBtn).toBeTruthy()
    await saveBtn!.trigger('click')
    await flushPromises()
    await nextTick()
    await flushPromises()

    expect(adminAPI.quotaKeys.create).toHaveBeenCalledTimes(1)
    const callArg = vi.mocked(adminAPI.quotaKeys.create).mock.calls[0][0] as Record<string, unknown>
    expect(callArg.upstream_key).toBe('kn_cy')
    wrapper.unmount()
  })

  it('编辑态打开对话框时 upstream-select 不存在', async () => {
    const wrapper = mountView()
    await flushPromises()

    // 直接调用组件方法打开编辑态（DataTable 桩不渲染行操作）。
    await (wrapper.vm as unknown as { openEditDialog: (row: Record<string, unknown>) => void }).openEditDialog({
      id: 1,
      status: 'active',
      exhausted: false,
      code_masked: '****edit0',
      notes: null,
      expires_at: null,
      priority: 1
    })
    await nextTick()
    await flushPromises()

    const select = wrapper.find('[data-testid="upstream-select"]')
    expect(select.exists()).toBe(false)
    wrapper.unmount()
  })

  it('创建态未选上游且上游列表非空时，提交被拦截（create 未调用）', async () => {
    const wrapper = mountView()
    await flushPromises()

    const createBtn = findButtonByText(wrapper, 'admin.quotaKeys.create')
    await createBtn!.trigger('click')
    await nextTick()
    await flushPromises()

    // 填明文 code 但不选上游（保持空串），以排除“code 为空”这一更早的拦截分支。
    const codeInput = wrapper.find('input[type="text"]')
    await codeInput.setValue('SKY_TEST_CODE_002')
    await nextTick()

    const select = wrapper.find('[data-testid="upstream-select"]')
    expect(select.exists()).toBe(true)
    // 默认即空串，无需额外操作；确认上游列表非空。
    expect((wrapper.vm as unknown as { upstreams: { length: number } }).upstreams.length).toBeGreaterThan(0)

    const saveBtn = findButtonByText(wrapper, 'common.save')
    await saveBtn!.trigger('click')
    await flushPromises()
    await nextTick()
    await flushPromises()

    expect(adminAPI.quotaKeys.create).not.toHaveBeenCalled()
    // 应提示必须选择上游。
    expect(wrapper.text()).toContain('admin.quotaKeys.upstream.selectRequired')
    wrapper.unmount()
  })

  it('上游列表加载失败（为空）时提交也被拦截，create 未调用', async () => {
    // 该用例内把上游列表桩改为空（加载失败/被软删为空）。
    vi.mocked(adminUpstreams.listUpstreams).mockResolvedValue({ items: [] } as never)

    const wrapper = mountView()
    await flushPromises()

    const createBtn = findButtonByText(wrapper, 'admin.quotaKeys.create')
    await createBtn!.trigger('click')
    await nextTick()
    await flushPromises()

    // 填明文 code 但不选上游（保持空串），以排除“code 为空”这一更早的拦截分支。
    const codeInput = wrapper.find('input[type="text"]')
    await codeInput.setValue('SKY_TEST_CODE_003')
    await nextTick()

    const select = wrapper.find('[data-testid="upstream-select"]')
    expect(select.exists()).toBe(true)
    // 确认上游列表为空。
    expect((wrapper.vm as unknown as { upstreams: { length: number } }).upstreams.length).toBe(0)

    const saveBtn = findButtonByText(wrapper, 'common.save')
    await saveBtn!.trigger('click')
    await flushPromises()
    await nextTick()
    await flushPromises()

    expect(adminAPI.quotaKeys.create).not.toHaveBeenCalled()
    // 应提示必须选择上游（失败关闭，不静默回落 shareai）。
    expect(wrapper.text()).toContain('admin.quotaKeys.upstream.selectRequired')
    wrapper.unmount()
  })
})
