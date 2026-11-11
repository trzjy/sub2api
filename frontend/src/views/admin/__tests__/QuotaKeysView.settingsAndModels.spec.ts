import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick, defineComponent, type PropType } from 'vue'
import QuotaKeysView from '../QuotaKeysView.vue'
import { adminAPI } from '@/api/admin'
import * as adminUpstreams from '@/api/admin/quotaKeys'

// c1p1-B3modal：QuotaKeysView 创建/编辑弹窗接入激活码设置（并发/负载因子/计费倍率）
// 与模型限制（白名单/映射），并复用「禁止空模型落库」裁定。
// 脚手架照抄同目录 QuotaKeysView.upstreamSelect.spec.ts。

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
      syncUpstreamModels: vi.fn(),
      refreshAutoPool: vi.fn(),
      batchImportAutoCards: vi.fn()
    },
    // D2a：创建/编辑弹窗现在会调 adminAPI.groups.getAll() 懒加载分组候选，
    // 该测试此前未覆盖此路径，补上桩以免 openCreateDialog/openEditDialog 抛错。
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

// ModelWhitelistSelector 桩：暴露一个「同步上游支持的模型」按钮，点击时调用 customSync
// prop（码级预览接口）并把返回的 models 经 update:modelValue 回填到父级 allowedModels。
const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: {
    modelValue: { type: Array as PropType<string[]>, default: () => [] },
    modelMappings: { type: Array, default: () => [] },
    platform: { type: String, default: '' },
    customSync: { type: Function as PropType<() => Promise<{ models: string[] }>>, default: undefined }
  },
  emits: ['update:modelValue'],
  setup(props, { emit }) {
    const onSync = async () => {
      if (props.customSync) {
        const res = await props.customSync()
        emit('update:modelValue', res.models)
      }
    }
    return { onSync }
  },
  template: `<div><button data-testid="mws-sync" type="button" @click="onSync">sync</button></div>`
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
        ModelWhitelistSelector: ModelWhitelistSelectorStub,
        // D2a：创建/编辑弹窗内新增 GroupSelector；桩化避免渲染真实组件
        // （真实组件依赖 Pinia 的 authStore，本测试未安装 Pinia）。
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

const upstreamsFixture = [
  { id: 1, key: 'shareai', name: 'ShareAI 号池', type: 'shareai', renewal_url: null, notes: null },
  { id: 2, key: 'kn_cy', name: 'kn-cy.com 网关', type: 'kn_cy', renewal_url: 'https://shop.kn-cy.com/products', notes: null }
]

function findButtonByText(wrapper: ReturnType<typeof mountView>, text: string) {
  return wrapper.findAll('button').find((b) => b.text().trim() === text)
}

describe('QuotaKeysView 创建/编辑弹窗：激活码设置与模型限制', () => {
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
    vi.mocked(adminAPI.quotaKeys.update).mockReset()
    vi.mocked(adminAPI.quotaKeys.update).mockResolvedValue({ id: 1 } as never)
    vi.mocked(adminAPI.quotaKeys.create).mockReset()
    vi.mocked(adminAPI.quotaKeys.create).mockResolvedValue({ id: 1 } as never)
    vi.mocked(adminAPI.quotaKeys.syncUpstreamModels).mockReset()
    vi.mocked(adminAPI.quotaKeys.syncUpstreamModels).mockResolvedValue({
      models: ['cursor-model-a', 'cursor-model-b']
    } as never)
    // D2a：创建/编辑弹窗懒加载分组候选（adminAPI.groups.getAll）。
    vi.mocked(adminAPI.groups.getAll).mockReset()
    vi.mocked(adminAPI.groups.getAll).mockResolvedValue([] as never)
  })

  describe('打开编辑回填', () => {
    it('回填并发/负载因子/计费倍率', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 10,
        status: 'active',
        exhausted: false,
        code_masked: '****edit0',
        notes: null,
        expires_at: null,
        priority: 5,
        concurrency: 8,
        load_factor: 200,
        rate_multiplier: 2.5,
        model_mapping: null
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const vm = wrapper.vm as unknown as {
        form: { concurrency: number | null; load_factor: number | null; rate_multiplier: number }
      }
      expect(vm.form.concurrency).toBe(8)
      expect(vm.form.load_factor).toBe(200)
      expect(vm.form.rate_multiplier).toBe(2.5)
      wrapper.unmount()
    })

    it('回填 model_mapping（whitelist 模式：全部 k===v）', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 11,
        status: 'active',
        exhausted: false,
        code_masked: '****edit1',
        model_mapping: { 'gpt-4': 'gpt-4', 'claude-3': 'claude-3' }
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const vm = wrapper.vm as unknown as {
        modelRestrictionMode: string
        allowedModels: string[]
        modelMappings: { from: string; to: string }[]
      }
      expect(vm.modelRestrictionMode).toBe('whitelist')
      expect(vm.allowedModels).toEqual(['gpt-4', 'claude-3'])
      expect(vm.modelMappings).toEqual([])
      wrapper.unmount()
    })

    it('回填 model_mapping（共存：非恒等条目进映射、恒等条目进白名单，模式 whitelist）', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 12,
        status: 'active',
        exhausted: false,
        code_masked: '****edit2',
        model_mapping: { 'claude-opus-*': 'claude-sonnet-4-5', 'gpt-4': 'gpt-4' }
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const vm = wrapper.vm as unknown as {
        modelRestrictionMode: string
        allowedModels: string[]
        modelMappings: { from: string; to: string }[]
      }
      // 方案 §3 C1：恒等与非恒等共存，不再整体切 mapping；仅当没有任何恒等条目时才切 mapping。
      expect(vm.modelRestrictionMode).toBe('whitelist')
      expect(vm.allowedModels).toEqual(['gpt-4'])
      expect(vm.modelMappings).toEqual([
        { from: 'claude-opus-*', to: 'claude-sonnet-4-5' }
      ])
      wrapper.unmount()
    })

    it('model_mapping 为 null 时回填空列表 + whitelist 模式', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 13,
        status: 'active',
        exhausted: false,
        code_masked: '****edit3',
        model_mapping: null
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const vm = wrapper.vm as unknown as {
        modelRestrictionMode: string
        allowedModels: string[]
        modelMappings: { from: string; to: string }[]
        modelRestrictionTouched: boolean
      }
      expect(vm.modelRestrictionMode).toBe('whitelist')
      expect(vm.allowedModels).toEqual([])
      expect(vm.modelMappings).toEqual([])
      expect(vm.modelRestrictionTouched).toBe(false)
      wrapper.unmount()
    })
  })

  describe('编辑提交字段', () => {
    it('修改并发/负载因子/计费倍率并保留非空模型时，update 提交正确字段', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 20,
        status: 'active',
        exhausted: false,
        code_masked: '****edit0',
        model_mapping: { 'gpt-4': 'gpt-4' }
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const vm = wrapper.vm as unknown as {
        form: { concurrency: number | null; load_factor: number | null; rate_multiplier: number }
      }
      vm.form.concurrency = 12
      vm.form.load_factor = 500
      vm.form.rate_multiplier = 0
      await nextTick()

      const saveBtn = findButtonByText(wrapper, 'common.save')
      expect(saveBtn).toBeTruthy()
      await saveBtn!.trigger('click')
      await flushPromises()
      await nextTick()
      await flushPromises()

      expect(adminAPI.quotaKeys.update).toHaveBeenCalledTimes(1)
      const callArg = vi.mocked(adminAPI.quotaKeys.update).mock.calls[0][1] as Record<string, unknown>
      expect(callArg.concurrency).toBe(12)
      expect(callArg.load_factor).toBe(500)
      expect(callArg.rate_multiplier).toBe(0)
      expect(callArg.model_mapping).toEqual({ 'gpt-4': 'gpt-4' })
      wrapper.unmount()
    })

    it('编辑已有模型限制的激活码时清空模型（0 个）后保存：拦截提交且不调 update', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 21,
        status: 'active',
        exhausted: false,
        code_masked: '****edit0',
        model_mapping: { 'gpt-4': 'gpt-4' }
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      // 清空白名单模型并标记已触碰。
      const vm = wrapper.vm as unknown as {
        allowedModels: string[]
        modelRestrictionTouched: boolean
      }
      vm.allowedModels = []
      vm.modelRestrictionTouched = true
      await nextTick()

      const saveBtn = findButtonByText(wrapper, 'common.save')
      expect(saveBtn).toBeTruthy()
      await saveBtn!.trigger('click')
      await flushPromises()
      await nextTick()
      await flushPromises()

      expect(adminAPI.quotaKeys.update).not.toHaveBeenCalled()
      expect(wrapper.text()).toContain('admin.quotaKeys.modelRestrictionRequired')
      wrapper.unmount()
    })

    it('编辑态仅改备注/优先级（原无模型限制、未触碰模型）时不发送 model_mapping', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 22,
        status: 'active',
        exhausted: false,
        code_masked: '****edit0',
        notes: 'old',
        model_mapping: null
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const vm = wrapper.vm as unknown as { form: { notes: string } }
      vm.form.notes = 'new note'
      await nextTick()

      const saveBtn = findButtonByText(wrapper, 'common.save')
      await saveBtn!.trigger('click')
      await flushPromises()
      await nextTick()
      await flushPromises()

      expect(adminAPI.quotaKeys.update).toHaveBeenCalledTimes(1)
      const callArg = vi.mocked(adminAPI.quotaKeys.update).mock.calls[0][1] as Record<string, unknown>
      expect(callArg.model_mapping).toBeUndefined()
      wrapper.unmount()
    })
  })

  describe('同步上游支持的模型', () => {
    it('点击「同步上游支持的模型」调用 syncUpstreamModels(id) 并填入 allowedModels', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = { id: 30, status: 'active', exhausted: false, code_masked: '****edit0' }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const syncBtn = wrapper.find('[data-testid="mws-sync"]')
      expect(syncBtn.exists()).toBe(true)
      await syncBtn.trigger('click')
      await flushPromises()
      await nextTick()
      await flushPromises()

      expect(adminAPI.quotaKeys.syncUpstreamModels).toHaveBeenCalledTimes(1)
      expect(adminAPI.quotaKeys.syncUpstreamModels).toHaveBeenCalledWith(30)
      const vm = wrapper.vm as unknown as { allowedModels: string[] }
      expect(vm.allowedModels).toEqual(['cursor-model-a', 'cursor-model-b'])
      wrapper.unmount()
    })
  })

  describe('创建态模型限制', () => {
    it('未触碰模型时不传 model_mapping（沿用上游默认）', async () => {
      const wrapper = mountView()
      await flushPromises()

      await (wrapper.vm as unknown as { openCreateDialog: () => void }).openCreateDialog()
      await nextTick()

      const vm = wrapper.vm as unknown as { form: { code: string; upstream_key: string } }
      vm.form.code = 'CREATE_CODE_001'
      vm.form.upstream_key = 'shareai'
      await nextTick()

      const saveBtn = findButtonByText(wrapper, 'common.save')
      await saveBtn!.trigger('click')
      await flushPromises()
      await nextTick()
      await flushPromises()

      expect(adminAPI.quotaKeys.create).toHaveBeenCalledTimes(1)
      const callArg = vi.mocked(adminAPI.quotaKeys.create).mock.calls[0][0] as Record<string, unknown>
      expect(callArg.model_mapping).toBeUndefined()
      wrapper.unmount()
    })

    it('修改了模型并清成 0 个则拦截保存（不调 create）', async () => {
      const wrapper = mountView()
      await flushPromises()

      await (wrapper.vm as unknown as { openCreateDialog: () => void }).openCreateDialog()
      await nextTick()

      const vm = wrapper.vm as unknown as {
        form: { code: string; upstream_key: string }
        allowedModels: string[]
        modelRestrictionTouched: boolean
      }
      vm.form.code = 'CREATE_CODE_002'
      vm.form.upstream_key = 'shareai'
      // 触碰模型后清空为 0 个。
      vm.allowedModels = []
      vm.modelRestrictionTouched = true
      await nextTick()

      const saveBtn = findButtonByText(wrapper, 'common.save')
      await saveBtn!.trigger('click')
      await flushPromises()
      await nextTick()
      await flushPromises()

      expect(adminAPI.quotaKeys.create).not.toHaveBeenCalled()
      expect(wrapper.text()).toContain('admin.quotaKeys.modelRestrictionRequired')
      wrapper.unmount()
    })
  })

  describe('模型限制：白名单 + 映射共存（方案 §3 C1）', () => {
    // 构造 17 条恒等 + 1 条非恒等的存量 model_mapping。
    function makeMixedMapping(): Record<string, string> {
      const map: Record<string, string> = {}
      for (let i = 0; i < 17; i++) {
        map[`m-${i}`] = `m-${i}`
      }
      map['claude-opus-*'] = 'claude-sonnet-4-5'
      return map
    }

    it('17 恒等 + 1 非恒等：回填后白名单 17 条、映射 1 条，模式 whitelist', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 40,
        status: 'active',
        exhausted: false,
        code_masked: '****mix40',
        model_mapping: makeMixedMapping()
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const vm = wrapper.vm as unknown as {
        modelRestrictionMode: string
        allowedModels: string[]
        modelMappings: { from: string; to: string }[]
      }
      expect(vm.modelRestrictionMode).toBe('whitelist')
      expect(vm.allowedModels).toHaveLength(17)
      expect(vm.allowedModels).toEqual(Array.from({ length: 17 }, (_, i) => `m-${i}`))
      expect(vm.modelMappings).toEqual([
        { from: 'claude-opus-*', to: 'claude-sonnet-4-5' }
      ])
      wrapper.unmount()
    })

    it('17 恒等 + 1 非恒等：保存提交的 model_mapping 同时含 18 条（恒等 + 映射）', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 41,
        status: 'active',
        exhausted: false,
        code_masked: '****mix41',
        model_mapping: makeMixedMapping()
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
      const callArg = vi.mocked(adminAPI.quotaKeys.update).mock.calls[0][1] as Record<string, unknown>
      const sent = callArg.model_mapping as Record<string, string>
      expect(Object.keys(sent)).toHaveLength(18)
      // 17 条恒等
      for (let i = 0; i < 17; i++) {
        expect(sent[`m-${i}`]).toBe(`m-${i}`)
      }
      // 1 条映射
      expect(sent['claude-opus-*']).toBe('claude-sonnet-4-5')
      wrapper.unmount()
    })

    it('纯映射（0 恒等 + 1 非恒等）：模式 mapping', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 42,
        status: 'active',
        exhausted: false,
        code_masked: '****map42',
        model_mapping: { 'claude-opus-*': 'claude-sonnet-4-5' }
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const vm = wrapper.vm as unknown as {
        modelRestrictionMode: string
        allowedModels: string[]
        modelMappings: { from: string; to: string }[]
      }
      expect(vm.modelRestrictionMode).toBe('mapping')
      expect(vm.allowedModels).toEqual([])
      expect(vm.modelMappings).toEqual([
        { from: 'claude-opus-*', to: 'claude-sonnet-4-5' }
      ])
      wrapper.unmount()
    })

    it('空映射 + 未触碰：不发送 model_mapping（回归）', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 43,
        status: 'active',
        exhausted: false,
        code_masked: '****empty43',
        model_mapping: null
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const saveBtn = findButtonByText(wrapper, 'common.save')
      await saveBtn!.trigger('click')
      await flushPromises()
      await nextTick()
      await flushPromises()

      expect(adminAPI.quotaKeys.update).toHaveBeenCalledTimes(1)
      const callArg = vi.mocked(adminAPI.quotaKeys.update).mock.calls[0][1] as Record<string, unknown>
      expect(callArg.model_mapping).toBeUndefined()
      wrapper.unmount()
    })

    it('存量含 foo-* -> foo-*（白名单带通配）：保存被阻断并提示，update 未被调用', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 44,
        status: 'active',
        exhausted: false,
        code_masked: '****wild44',
        // foo-* 恒等于自身，回填进白名单；但白名单含 * 无法持久化。
        model_mapping: { 'foo-*': 'foo-*' }
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const saveBtn = findButtonByText(wrapper, 'common.save')
      await saveBtn!.trigger('click')
      await flushPromises()
      await nextTick()
      await flushPromises()

      expect(adminAPI.quotaKeys.update).not.toHaveBeenCalled()
      expect(wrapper.text()).toContain('admin.quotaKeys.modelEntryNotPersistable')
      wrapper.unmount()
    })

    it('存量含 a*b -> b（通配不在末尾）：保存被阻断', async () => {
      const wrapper = mountView()
      await flushPromises()

      const row = {
        id: 45,
        status: 'active',
        exhausted: false,
        code_masked: '****wild45',
        // from 通配符不在末尾（a*b），buildModelMappingObject 会跳过，静默删减被拦截。
        model_mapping: { 'a*b': 'b' }
      }
      await (wrapper.vm as unknown as { openEditDialog: (r: Record<string, unknown>) => void }).openEditDialog(row)
      await nextTick()

      const saveBtn = findButtonByText(wrapper, 'common.save')
      await saveBtn!.trigger('click')
      await flushPromises()
      await nextTick()
      await flushPromises()

      expect(adminAPI.quotaKeys.update).not.toHaveBeenCalled()
      expect(wrapper.text()).toContain('admin.quotaKeys.modelEntryNotPersistable')
      wrapper.unmount()
    })
  })
})
