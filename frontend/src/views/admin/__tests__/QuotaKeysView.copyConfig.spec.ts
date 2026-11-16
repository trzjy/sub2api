import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import QuotaKeysView from '../QuotaKeysView.vue'
import { adminAPI } from '@/api/admin'

// QKCOPY-FE01：激活码列表行「复制配置」→ 同上游多目标一键写入。
// 把源码激活码的 priority/concurrency/load_factor/rate_multiplier/model_mapping/group_ids
// 一键写入同 upstream_key 的其它激活码（目标可多选），沿用既有 update 逐个保存；
// 不复制 expires_at / notes，不新增后端接口 / API 客户端方法。
// 脚手架照抄同目录 QuotaKeysView.groupBinding.spec.ts。

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
      // t 桩直接返回键名，便于断言文案键（如 'admin.quotaKeys.copy.partial'）。
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

// 通过 vm 访问复制配置相关状态/函数（DataTable 被桩化，行按钮不渲染）。
interface CopyConfigVm {
  openCopyDialog: (row: Record<string, unknown>) => void
  closeCopyDialog: () => void
  submitCopy: () => Promise<void>
  showCopyDialog: boolean
  copySource: { id: number; code_masked: string } | null
  copyTargets: { id: number; code_masked: string }[]
  copyTargetIds: number[]
  allCopyTargetsSelected: boolean
  copyError: string
  copySaving: boolean
}

function vmOf(wrapper: ReturnType<typeof mountView>) {
  return wrapper.vm as unknown as CopyConfigVm
}

function makeRow(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    status: 'active',
    exhausted: false,
    code_masked: 'KEY---1',
    priority: 1,
    upstream_key: 'shareai',
    concurrency: null,
    load_factor: null,
    rate_multiplier: 1,
    model_mapping: null,
    group_ids: null,
    ...overrides
  }
}

// 让列表返回给定行，等待 mount 后的 load() 完成。
async function mountWithRows(rows: Record<string, unknown>[]) {
  vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
    items: rows,
    total: rows.length,
    page: 1,
    page_size: 20,
    pages: 1
  } as never)
  const wrapper = mountView()
  await flushPromises()
  return wrapper
}

describe('QuotaKeysView 复制配置（QKCOPY-FE01）', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.update).mockReset()
    vi.mocked(adminAPI.quotaKeys.update).mockResolvedValue({ id: 2 } as never)
    vi.mocked(adminAPI.quotaKeys.create).mockReset()
    vi.mocked(adminAPI.quotaKeys.create).mockResolvedValue({ id: 1 } as never)
    vi.mocked(adminAPI.groups.getAll).mockReset()
    vi.mocked(adminAPI.groups.getAll).mockResolvedValue([] as never)
  })

  describe('候选隔离', () => {
    it('源码自身与跨上游不在候选，只保留同上游的其它激活码', async () => {
      const wrapper = await mountWithRows([
        makeRow({ id: 1, code_masked: 'KEY---A', upstream_key: 'shareai' }),
        makeRow({ id: 2, code_masked: 'KEY---B', upstream_key: 'shareai' }),
        makeRow({ id: 3, code_masked: 'KEY---C', upstream_key: 'kn-cy' })
      ])

      const vm = vmOf(wrapper)
      vm.openCopyDialog({ id: 1, code_masked: 'KEY---A', upstream_key: 'shareai' })
      await nextTick()

      expect(vm.copySource?.id).toBe(1)
      expect(vm.copyTargets.map((r) => r.id)).toEqual([2])
      // 源码自身（id=1）与跨上游 id=3 均不在候选。
      expect(vm.copyTargets.map((r) => r.id)).not.toContain(1)
      expect(vm.copyTargets.map((r) => r.id)).not.toContain(3)
      wrapper.unmount()
    })

    it('源码 upstream_key 为空时无任何候选', async () => {
      const wrapper = await mountWithRows([
        makeRow({ id: 1, code_masked: 'KEY---A', upstream_key: null }),
        makeRow({ id: 2, code_masked: 'KEY---B', upstream_key: null })
      ])

      const vm = vmOf(wrapper)
      vm.openCopyDialog({ id: 1, code_masked: 'KEY---A', upstream_key: null })
      await nextTick()

      expect(vm.copyTargets).toEqual([])
      wrapper.unmount()
    })
  })

  describe('复制 payload', () => {
    it('勾选目标后确认：恰好以 (2, payload) 调用一次 update，payload 精确等于源码配置且不含 expires_at/notes', async () => {
      const wrapper = await mountWithRows([
        makeRow({
          id: 1,
          code_masked: 'KEY---A',
          upstream_key: 'shareai',
          priority: 5,
          concurrency: 7,
          load_factor: 200,
          rate_multiplier: 0.5,
          model_mapping: { a: 'b' },
          group_ids: [3]
        }),
        makeRow({ id: 2, code_masked: 'KEY---B', upstream_key: 'shareai' })
      ])
      const updateSpy = vi.mocked(adminAPI.quotaKeys.update)

      const vm = vmOf(wrapper)
      vm.openCopyDialog({
        id: 1,
        code_masked: 'KEY---A',
        upstream_key: 'shareai',
        priority: 5,
        concurrency: 7,
        load_factor: 200,
        rate_multiplier: 0.5,
        model_mapping: { a: 'b' },
        group_ids: [3]
      })
      await nextTick()

      vm.copyTargetIds = [2]
      await nextTick()

      const confirmBtn = findButtonByText(wrapper, 'admin.quotaKeys.copy.confirm')
      expect(confirmBtn).toBeTruthy()
      await confirmBtn!.trigger('click')
      await flushPromises()

      expect(updateSpy).toHaveBeenCalledTimes(1)
      const [targetId, payload] = updateSpy.mock.calls[0]
      expect(targetId).toBe(2)
      expect(payload).toEqual({
        priority: 5,
        concurrency: 7,
        load_factor: 200,
        rate_multiplier: 0.5,
        model_mapping: { a: 'b' },
        group_ids: [3]
      })
      // 不复制 expires_at / notes。
      expect((payload as Record<string, unknown>).expires_at).toBeUndefined()
      expect((payload as Record<string, unknown>).notes).toBeUndefined()
      // 全部成功 → 弹窗关闭。
      expect(vmOf(wrapper).showCopyDialog).toBe(false)
      wrapper.unmount()
    })

    it('全选复选框可一键选中全部候选目标', async () => {
      const wrapper = await mountWithRows([
        makeRow({ id: 1, code_masked: 'KEY---A', upstream_key: 'shareai', priority: 3 }),
        makeRow({ id: 2, code_masked: 'KEY---B', upstream_key: 'shareai' }),
        makeRow({ id: 3, code_masked: 'KEY---C', upstream_key: 'shareai' })
      ])

      const vm = vmOf(wrapper)
      vm.openCopyDialog({ id: 1, code_masked: 'KEY---A', upstream_key: 'shareai', priority: 3 })
      await nextTick()

      expect(vm.allCopyTargetsSelected).toBe(false)
      // 模拟「全选」复选框 change：点击后全部候选被勾选。
      const selectAllLabel = wrapper.findAll('label').find((l) => l.text().includes('admin.quotaKeys.copy.selectAll'))
      const checkbox = selectAllLabel?.find('input[type="checkbox"]')
      expect(checkbox).toBeTruthy()
      await checkbox!.setValue(true)
      await nextTick()

      expect(vm.allCopyTargetsSelected).toBe(true)
      expect(vm.copyTargetIds).toEqual([2, 3])
      wrapper.unmount()
    })
  })

  describe('空值边界', () => {
    it('源码无 model_mapping、group_ids 为 null、concurrency 为 null → payload 只含基础四键', async () => {
      const wrapper = await mountWithRows([
        makeRow({
          id: 1,
          code_masked: 'KEY---A',
          upstream_key: 'shareai',
          priority: 3,
          concurrency: null,
          load_factor: 50,
          rate_multiplier: 1,
          model_mapping: null,
          group_ids: null
        }),
        makeRow({ id: 2, code_masked: 'KEY---B', upstream_key: 'shareai' })
      ])
      const updateSpy = vi.mocked(adminAPI.quotaKeys.update)

      const vm = vmOf(wrapper)
      vm.openCopyDialog({
        id: 1,
        code_masked: 'KEY---A',
        upstream_key: 'shareai',
        priority: 3,
        concurrency: null,
        load_factor: 50,
        rate_multiplier: 1,
        model_mapping: null,
        group_ids: null
      })
      await nextTick()
      vm.copyTargetIds = [2]
      await nextTick()

      await vm.submitCopy()
      await flushPromises()

      const [, payload] = updateSpy.mock.calls[0]
      expect(payload).toEqual({ priority: 3, concurrency: null, load_factor: 50, rate_multiplier: 1 })
      // 不含 model_mapping 与 group_ids 键。
      expect(payload).not.toHaveProperty('model_mapping')
      expect(payload).not.toHaveProperty('group_ids')
      wrapper.unmount()
    })

    it('源码 group_ids 为空数组 → payload.group_ids 为 []（显式解绑全部也复制）', async () => {
      const wrapper = await mountWithRows([
        makeRow({
          id: 1,
          code_masked: 'KEY---A',
          upstream_key: 'shareai',
          priority: 3,
          concurrency: null,
          load_factor: 50,
          rate_multiplier: 1,
          model_mapping: null,
          group_ids: []
        }),
        makeRow({ id: 2, code_masked: 'KEY---B', upstream_key: 'shareai' })
      ])
      const updateSpy = vi.mocked(adminAPI.quotaKeys.update)

      const vm = vmOf(wrapper)
      vm.openCopyDialog({
        id: 1,
        code_masked: 'KEY---A',
        upstream_key: 'shareai',
        priority: 3,
        concurrency: null,
        load_factor: 50,
        rate_multiplier: 1,
        model_mapping: null,
        group_ids: []
      })
      await nextTick()
      vm.copyTargetIds = [2]
      await nextTick()

      await vm.submitCopy()
      await flushPromises()

      const [, payload] = updateSpy.mock.calls[0]
      expect((payload as Record<string, unknown>).group_ids).toEqual([])
      wrapper.unmount()
    })

    it('未勾选任何目标时提交 → copyError 为 needTarget', async () => {
      const wrapper = await mountWithRows([
        makeRow({ id: 1, code_masked: 'KEY---A', upstream_key: 'shareai', priority: 3 }),
        makeRow({ id: 2, code_masked: 'KEY---B', upstream_key: 'shareai' })
      ])

      const vm = vmOf(wrapper)
      vm.openCopyDialog({ id: 1, code_masked: 'KEY---A', upstream_key: 'shareai', priority: 3 })
      await nextTick()

      await vm.submitCopy()
      await flushPromises()

      expect(vm.copyError).toBe('admin.quotaKeys.copy.needTarget')
      expect(vi.mocked(adminAPI.quotaKeys.update)).not.toHaveBeenCalled()
      wrapper.unmount()
    })
  })

  describe('部分失败', () => {
    it('目标 update 被 reject → copyError 含 partial 文案、弹窗保持打开、列表被重新加载', async () => {
      const wrapper = await mountWithRows([
        makeRow({ id: 1, code_masked: 'KEY---A', upstream_key: 'shareai', priority: 3 }),
        makeRow({ id: 2, code_masked: 'KEY---B', upstream_key: 'shareai' })
      ])
      const listSpy = vi.mocked(adminAPI.quotaKeys.list)
      const updateSpy = vi.mocked(adminAPI.quotaKeys.update)
      updateSpy.mockRejectedValue(new Error('boom'))

      const vm = vmOf(wrapper)
      vm.openCopyDialog({ id: 1, code_masked: 'KEY---A', upstream_key: 'shareai', priority: 3 })
      await nextTick()
      vm.copyTargetIds = [2]
      await nextTick()

      await vm.submitCopy()
      await flushPromises()

      // partial 文案键名（t 桩直接返回键名）。
      expect(vm.copyError).toContain('admin.quotaKeys.copy.partial')
      // 弹窗保持打开。
      expect(vm.showCopyDialog).toBe(true)
      // 列表被刷新（mount 时一次 + submitCopy 后一次）。
      expect(listSpy).toHaveBeenCalledTimes(2)
      wrapper.unmount()
    })
  })

  describe('保存期间关闭保护', () => {
    it('提交进行中调用 closeCopyDialog 不关闭弹窗、不清空选择；批次完成后正常关闭', async () => {
      const wrapper = await mountWithRows([
        makeRow({ id: 1, code_masked: 'KEY---A', upstream_key: 'shareai', priority: 3 }),
        makeRow({ id: 2, code_masked: 'KEY---B', upstream_key: 'shareai' })
      ])
      const updateSpy = vi.mocked(adminAPI.quotaKeys.update)
      let resolveUpdate: (value: unknown) => void = () => {}
      const pending = new Promise((resolve) => {
        resolveUpdate = resolve
      })
      updateSpy.mockReturnValue(pending as never)

      const vm = vmOf(wrapper)
      vm.openCopyDialog({ id: 1, code_masked: 'KEY---A', upstream_key: 'shareai', priority: 3 })
      await nextTick()
      vm.copyTargetIds = [2]
      await nextTick()

      const submitPromise = vm.submitCopy()
      await nextTick()
      expect(vm.copySaving).toBe(true)

      // 保存中关闭入口（按钮/Escape 之外的程序化路径）必须被拒绝。
      vm.closeCopyDialog()
      await nextTick()
      expect(vm.showCopyDialog).toBe(true)
      expect(vm.copyTargetIds).toEqual([2])

      // 保存中重复提交同样被拒绝。
      await vm.submitCopy()
      expect(updateSpy).toHaveBeenCalledTimes(1)

      // 保存中重新打开（背景行按钮经键盘可达）同样被拒绝：来源与选中不得被覆盖。
      vm.openCopyDialog({ id: 2, code_masked: 'KEY---B', upstream_key: 'shareai', priority: 99 })
      await nextTick()
      expect(vm.copySource?.id).toBe(1)
      expect(vm.copyTargetIds).toEqual([2])
      expect(vm.copySaving).toBe(true)

      resolveUpdate({ id: 2 })
      await submitPromise
      await flushPromises()
      expect(vm.showCopyDialog).toBe(false)
      wrapper.unmount()
    })
  })
})