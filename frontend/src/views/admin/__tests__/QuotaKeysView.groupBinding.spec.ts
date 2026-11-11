import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import QuotaKeysView from '../QuotaKeysView.vue'
import { adminAPI } from '@/api/admin'

// D2a：QuotaKeysView 创建/编辑弹窗接入分组选择器（GroupSelector），
// 并落地「三态」分组绑定提交裁定：
//   - 编辑态回填 row.group_ids；
//   - 未触碰分组时 update payload 不含 group_ids（保持后端既有绑定）；
//   - 触碰后含 group_ids；
//   - 创建态默认 [] 且随创建请求发送。
// 脚手架照抄同目录 QuotaKeysView.settingsAndModels.spec.ts。

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
      syncUpstreamModels: vi.fn()
    },
    groups: {
      getAll: vi.fn()
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
        ModelWhitelistSelector: true,
        // GroupSelector 在创建/编辑弹窗内被渲染，桩化处理避免依赖 authStore 等真实逻辑。
        GroupSelector: true,
        // BaseDialog 按 show 条件渲染，才能拿到对话框内的 DOM。
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

function findButtonByText(wrapper: ReturnType<typeof mountView>, text: string) {
  return wrapper.findAll('button').find((b) => b.text().trim() === text)
}

describe('QuotaKeysView 分组选择器（D2a）', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [],
      total: 0,
      page: 1,
      page_size: 20,
      pages: 1
    } as never)
    vi.mocked(adminAPI.quotaKeys.update).mockReset()
    vi.mocked(adminAPI.quotaKeys.update).mockResolvedValue({ id: 1 } as never)
    vi.mocked(adminAPI.quotaKeys.create).mockReset()
    vi.mocked(adminAPI.quotaKeys.create).mockResolvedValue({ id: 1 } as never)
    vi.mocked(adminAPI.groups.getAll).mockReset()
    vi.mocked(adminAPI.groups.getAll).mockResolvedValue([] as never)
  })

  describe('编辑态回填 group_ids', () => {
    it('回填 row.group_ids 到 form.group_ids', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 1,
        status: 'active',
        exhausted: false,
        code_masked: '****',
        group_ids: [3, 7]
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const vm = wrapper.vm as unknown as { form: { group_ids: number[] }; groupTouched: boolean }
      expect(vm.form.group_ids).toEqual([3, 7])
      expect(vm.groupTouched).toBe(false)
      wrapper.unmount()
    })

    it('row.group_ids 缺省时回填空数组', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 2,
        status: 'active',
        exhausted: false,
        code_masked: '****'
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const vm = wrapper.vm as unknown as { form: { group_ids: number[] } }
      expect(vm.form.group_ids).toEqual([])
      wrapper.unmount()
    })
  })

  describe('编辑态三态提交', () => {
    it('未触碰分组时 update payload 不含 group_ids', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 3,
        status: 'active',
        exhausted: false,
        code_masked: '****',
        model_mapping: null,
        group_ids: [3, 7]
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const saveBtn = findButtonByText(wrapper, 'common.save')
      expect(saveBtn).toBeTruthy()
      await saveBtn!.trigger('click')
      await flushPromises()
      await nextTick()
      await flushPromises()

      expect(adminAPI.quotaKeys.update).toHaveBeenCalledTimes(1)
      const arg = vi.mocked(adminAPI.quotaKeys.update).mock.calls[0][1] as Record<string, unknown>
      expect(arg.group_ids).toBeUndefined()
      wrapper.unmount()
    })

    it('触碰分组后 update payload 含 group_ids', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 4,
        status: 'active',
        exhausted: false,
        code_masked: '****',
        model_mapping: null,
        group_ids: [3, 7]
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const vm = wrapper.vm as unknown as { groupTouched: boolean }
      vm.groupTouched = true
      await nextTick()

      const saveBtn = findButtonByText(wrapper, 'common.save')
      expect(saveBtn).toBeTruthy()
      await saveBtn!.trigger('click')
      await flushPromises()
      await nextTick()
      await flushPromises()

      expect(adminAPI.quotaKeys.update).toHaveBeenCalledTimes(1)
      const arg = vi.mocked(adminAPI.quotaKeys.update).mock.calls[0][1] as Record<string, unknown>
      expect(arg.group_ids).toEqual([3, 7])
      wrapper.unmount()
    })
  })

  describe('创建态提交', () => {
    it('默认 group_ids 为 [] 且随创建请求发送', async () => {
      const wrapper = mountView()
      await flushPromises()

      await (wrapper.vm as unknown as { openCreateDialog: () => void }).openCreateDialog()
      await nextTick()

      const vm = wrapper.vm as unknown as { form: { code: string; upstream_key: string; group_ids: number[] } }
      expect(vm.form.group_ids).toEqual([])
      vm.form.code = 'CODE_X'
      vm.form.upstream_key = 'shareai'
      await nextTick()

      const saveBtn = findButtonByText(wrapper, 'common.save')
      expect(saveBtn).toBeTruthy()
      await saveBtn!.trigger('click')
      await flushPromises()
      await nextTick()
      await flushPromises()

      expect(adminAPI.quotaKeys.create).toHaveBeenCalledTimes(1)
      const arg = vi.mocked(adminAPI.quotaKeys.create).mock.calls[0][0] as Record<string, unknown>
      expect(arg.group_ids).toEqual([])
      wrapper.unmount()
    })
  })

  // QKG-D2-FIX：adminAPI.groups.getAll 失败（未提供/网络拒绝）时，
  // 打开创建弹窗不得产生未捕获 Promise 异常，分组候选回落空数组，组件仍可正常渲染。
  describe('分组候选懒加载失败路径（QKG-D2-FIX）', () => {
    it('getAll reject 时打开创建弹窗无未捕获异常、候选为空、组件可渲染', async () => {
      vi.mocked(adminAPI.groups.getAll).mockRejectedValue(new Error('groups service unavailable'))

      const unhandled: unknown[] = []
      const onUnhandled = (e: unknown) => unhandled.push(e)
      window.addEventListener('unhandledrejection', onUnhandled)

      const wrapper = mountView()
      await flushPromises()

      await (wrapper.vm as unknown as { openCreateDialog: () => void }).openCreateDialog()
      await flushPromises()
      await nextTick()
      await flushPromises()

      window.removeEventListener('unhandledrejection', onUnhandled)

      expect(unhandled).toHaveLength(0)
      const vm = wrapper.vm as unknown as { accountGroups: unknown[] }
      expect(vm.accountGroups).toEqual([])
      // 创建弹窗正常打开且组件未崩溃（DOM 已渲染）。
      expect(wrapper.html().length).toBeGreaterThan(0)
      wrapper.unmount()
    })

    it('getAll 同步抛错（方法缺失）时打开创建弹窗同样无未捕获异常', async () => {
      // 模拟派发单复现场景：运行环境未提供可用的 adminAPI.groups.getAll
      // （与 statusToggle.spec 同源），调用即同步抛错。
      vi.mocked(adminAPI.groups.getAll).mockImplementation(() => {
        throw new Error('groups.getAll is not a function')
      })

      const unhandled: unknown[] = []
      const onUnhandled = (e: unknown) => unhandled.push(e)
      window.addEventListener('unhandledrejection', onUnhandled)

      const wrapper = mountView()
      await flushPromises()

      await (wrapper.vm as unknown as { openCreateDialog: () => void }).openCreateDialog()
      await flushPromises()
      await nextTick()
      await flushPromises()

      window.removeEventListener('unhandledrejection', onUnhandled)

      expect(unhandled).toHaveLength(0)
      const vm = wrapper.vm as unknown as { accountGroups: unknown[] }
      expect(vm.accountGroups).toEqual([])
      expect(wrapper.html().length).toBeGreaterThan(0)
      wrapper.unmount()
    })
  })
})
