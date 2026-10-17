import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import AutoEntryConfigModal from '../AutoEntryConfigModal.vue'
import { adminAPI } from '@/api/admin'

// 模拟配置：openai 启用、anthropic 启用；其余厂商关闭。
// models = 已勾选集合（gpt-6-astra / claude-opus-5-5）；available_models = 可选全集。
const mockConfig = {
  vendors: {
    openai: true,
    anthropic: true,
    google: false,
    xai: false,
    deepseek: false,
    mistral: false,
    muse: true,
    composer: true
  },
  models: [
    { id: 'gpt-6-astra', vendor: 'openai', probe_status: '' },
    { id: 'claude-opus-5-5', vendor: 'anthropic', probe_status: '' }
  ],
  entry_defaults: { concurrency: null, priority: 1, rate_multiplier: 1.0 },
  available_models: [
    { id: 'gpt-6-astra', vendor: 'openai' },
    { id: 'gpt-5.6-sol', vendor: 'openai' },
    { id: 'claude-opus-5-5', vendor: 'anthropic' },
    { id: 'claude-fable-5.1', vendor: 'anthropic' },
    { id: 'muse-spark-1.3', vendor: 'muse' },
    { id: 'composer-2.5', vendor: 'composer' }
  ]
}

vi.mock('@/api/admin', () => ({
  adminAPI: {
    quotaKeys: {
      getAutoEntryConfig: vi.fn(),
      updateAutoEntryConfig: vi.fn()
    }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    // 支持命名参数插值（R12-2 的 staleModelsCleaned 提示需要 {count}），其余返回 key 原样。
    useI18n: () => ({
      t: (key: string, named?: Record<string, unknown>) => {
        if (named && typeof named.count === 'number') {
          return `${key}:${named.count}`
        }
        return key
      }
    })
  }
})

function findModelInput(wrapper: ReturnType<typeof mount>, id: string) {
  const label = wrapper
    .findAll('label')
    .find((l) => l.text().includes(id))
  expect(label).toBeTruthy()
  return label!.find('input[type="checkbox"]')
}

async function setModelChecked(
  wrapper: ReturnType<typeof mount>,
  id: string,
  checked: boolean
) {
  const input = findModelInput(wrapper, id)
  input.element.checked = checked
  await input.trigger('change')
  await nextTick()
}

async function clickSave(wrapper: ReturnType<typeof mount>) {
  const btn = wrapper
    .findAll('button')
    .find((b) => b.text() === 'admin.quotaKeys.autoEntry.save')
  expect(btn).toBeTruthy()
  await btn!.trigger('click')
  await flushPromises()
  await nextTick()
}

function lastSavedModelIDs(): string[] {
  const calls = vi.mocked(adminAPI.quotaKeys.updateAutoEntryConfig).mock.calls
  const payload = calls[calls.length - 1][0] as { models: { id: string }[] }
  return payload.models.map((m) => m.id)
}

async function mountModal() {
  const wrapper = mount(AutoEntryConfigModal, {
    props: { show: false },
    global: {
      stubs: {
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }
      }
    }
  })
  vi.mocked(adminAPI.quotaKeys.getAutoEntryConfig).mockResolvedValue(
    JSON.parse(JSON.stringify(mockConfig)) as any
  )
  vi.mocked(adminAPI.quotaKeys.updateAutoEntryConfig).mockResolvedValue(
    {} as any
  )
  await wrapper.setProps({ show: true })
  await flushPromises()
  await nextTick()
  return wrapper
}

describe('AutoEntryConfigModal 模型多选（CUR-F2 裁定 1）', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.getAutoEntryConfig).mockReset()
    vi.mocked(adminAPI.quotaKeys.updateAutoEntryConfig).mockReset()
  })

  it('渲染源为可选全集：未勾选模型也出现在列表中', async () => {
    const wrapper = await mountModal()
    // gpt-5.6-sol 不在已勾选 models 中，但应仍在列表中（来自 available_models）。
    const unchecked = findModelInput(wrapper, 'gpt-5.6-sol')
    expect(unchecked.exists()).toBe(true)
    expect(unchecked.element.checked).toBe(false)
    // 已勾选项默认选中。
    expect(findModelInput(wrapper, 'gpt-6-astra').element.checked).toBe(true)
  })

  it('勾选 → 进 localModels；取消 → 仍在列表且复选框未选中；再勾选 → 重新进 localModels', async () => {
    const wrapper = await mountModal()

    // 1) 勾选未勾选项 gpt-5.6-sol。
    await setModelChecked(wrapper, 'gpt-5.6-sol', true)
    await clickSave(wrapper)
    expect(lastSavedModelIDs()).toContain('gpt-5.6-sol')
    expect(lastSavedModelIDs()).toContain('gpt-6-astra')

    // 2) 取消已勾选项 gpt-6-astra：该项仍在列表中、复选框未选中，但提交中不再含它。
    await setModelChecked(wrapper, 'gpt-6-astra', false)
    const afterDeselect = findModelInput(wrapper, 'gpt-6-astra')
    expect(afterDeselect.exists()).toBe(true) // 仍在列表
    expect(afterDeselect.element.checked).toBe(false) // 复选框未选中
    await clickSave(wrapper)
    expect(lastSavedModelIDs()).not.toContain('gpt-6-astra')
    expect(lastSavedModelIDs()).toContain('gpt-5.6-sol')

    // 3) 再次勾选 gpt-6-astra：重新进 localModels。
    await setModelChecked(wrapper, 'gpt-6-astra', true)
    await clickSave(wrapper)
    expect(lastSavedModelIDs()).toContain('gpt-6-astra')
  })

  it('save() 只提交已勾选厂商下的已勾选模型', async () => {
    const wrapper = await mountModal()
    // anthropic 启用且 claude-opus-5-5 已勾选；取消 gpt-6-astra（openai）。
    await setModelChecked(wrapper, 'gpt-6-astra', false)
    await clickSave(wrapper)
    const ids = lastSavedModelIDs()
    // 仅 claude-opus-5-5 保留（gpt-6-astra 取消、gpt-5.6-sol 从未勾选）。
    expect(ids).toEqual(['claude-opus-5-5'])
  })

  it('厂商复选框含 muse/composer，且其后端实供模型出现在对应厂商列表（CUR-F3）', async () => {
    const wrapper = await mountModal()
    // 厂商复选框由 vendorList 渲染（t() 在 mock 中返回 key），应含 muse/composer。
    const vendorLabels = wrapper
      .findAll('label')
      .map((l) => l.text())
    expect(vendorLabels).toContain('admin.quotaKeys.autoEntry.vendorMuse')
    expect(vendorLabels).toContain('admin.quotaKeys.autoEntry.vendorComposer')
    // 对应厂商（已启用）的模型多选区应含其后端实供模型。
    expect(findModelInput(wrapper, 'muse-spark-1.3').exists()).toBe(true)
    expect(findModelInput(wrapper, 'composer-2.5').exists()).toBe(true)
  })
})

// ---------------------------------------------------------------------------
// R12-2 / CUR-F15（外审 R12 must_fix）：前端保存时清理清单外失效项。
// 旧配置勾了官方模型名 gpt-4.1（不在后端实供清单 available_models 内）→ 复选框不渲染
// （渲染源为实供全集）→ 管理员无法取消 → save() 原样提交 → 补池持续报配置错误。
// 修复后：loadConfig 剔除幽灵项（UI 态不含）；save() 静默丢弃不在实供清单的勾选并提示。
// ---------------------------------------------------------------------------

// staleConfig：在 mockConfig 基础上额外勾选 gpt-4.1（openai，但不在 available_models 内）。
const staleConfig = JSON.parse(JSON.stringify(mockConfig)) as any
staleConfig.models.push({ id: 'gpt-4.1', vendor: 'openai', probe_status: '' })

async function mountModalWithConfig(cfg: any) {
  const wrapper = mount(AutoEntryConfigModal, {
    props: { show: false },
    global: {
      stubs: {
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }
      }
    }
  })
  vi.mocked(adminAPI.quotaKeys.getAutoEntryConfig).mockResolvedValue(cfg)
  vi.mocked(adminAPI.quotaKeys.updateAutoEntryConfig).mockResolvedValue({} as any)
  await wrapper.setProps({ show: true })
  await flushPromises()
  await nextTick()
  return wrapper
}

describe('AutoEntryConfigModal 清单外失效项清理（R12-2/CUR-F15）', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.getAutoEntryConfig).mockReset()
    vi.mocked(adminAPI.quotaKeys.updateAutoEntryConfig).mockReset()
  })

  it('不在实供清单内的失效勾选（gpt-4.1）加载后不渲染复选框，且 save() payload 不含它、含其余正常勾选', async () => {
    const wrapper = await mountModalWithConfig(JSON.parse(JSON.stringify(staleConfig)))

    // 渲染源为 available_models（实供全集）：gpt-4.1 不在其中 → 复选框不渲染。
    const labels = wrapper.findAll('label').map((l) => l.text())
    expect(labels.some((t) => t.includes('gpt-4.1'))).toBe(false)

    // 保存：gpt-4.1 被静默丢弃；其余正常勾选（gpt-6-astra / claude-opus-5-5）保留。
    await clickSave(wrapper)
    const ids = lastSavedModelIDs()
    expect(ids).not.toContain('gpt-4.1')
    expect(ids).toContain('gpt-6-astra')
    expect(ids).toContain('claude-opus-5-5')

    // 丢弃发生时提示条出现且计数正确（mock t 形如 key:count）。
    expect(wrapper.text()).toContain('admin.quotaKeys.autoEntry.staleModelsCleaned:1')
  })

  it('无失效勾选（默认 mockConfig）时保存不弹出清理提示条', async () => {
    const wrapper = await mountModal()
    await clickSave(wrapper)
    expect(wrapper.text()).not.toContain('admin.quotaKeys.autoEntry.staleModelsCleaned')
  })

  it('loadConfig 仅剔除幽灵项、不自动保存（未点保存则不调用 updateAutoEntryConfig）', async () => {
    const wrapper = await mountModalWithConfig(JSON.parse(JSON.stringify(staleConfig)))
    // 加载完成后尚未点保存 → 不应有任何落库调用（清理结果仅在保存时落库）。
    expect(adminAPI.quotaKeys.updateAutoEntryConfig).not.toHaveBeenCalled()
    // 且 UI 态 localModels 不含 gpt-4.1（组件实例可见）。
    expect((wrapper.vm as any).localModels.some((m: any) => m.id === 'gpt-4.1')).toBe(false)
  })
})

// ---------------------------------------------------------------------------
// R13-1（外审 R13 must_fix #1）：存量配置「错误归属」的勾选在弹窗内自动修正 + 可保存。
// 存量配置 gpt-6-sol 记 vendor=xai（F8 校验前保存的）：UI 按 ID 显示已勾选（挂在 xai 组下），
// 管理员不操作直接保存 → 后端 R8 归属校验拒绝 → 卡死无法自愈。
// 修复后：loadConfig 对留在 localModels 的每项，若同 ID 真实 vendor 与自报 vendor 不符 →
// 就地修正为真实 vendor（数据源 = 后端 available_models 的 id→vendor 映射，与后端
// CursorRealSupplyVendorByID 同源口径）；save() 兜底过滤层同步修正。
// ---------------------------------------------------------------------------

// misConfig：在 mockConfig 基础上额外勾选 gpt-6-sol，但其自报 vendor=xai（错误），
// available_models 中 gpt-6-sol 的真实 vendor=openai（与后端实供清单一致）。
const misConfig = JSON.parse(JSON.stringify(mockConfig)) as any
misConfig.models.push({ id: 'gpt-6-sol', vendor: 'xai', probe_status: '' })
misConfig.available_models.push({ id: 'gpt-6-sol', vendor: 'openai' })

describe('AutoEntryConfigModal 存量归属修正（R13-1）', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.getAutoEntryConfig).mockReset()
    vi.mocked(adminAPI.quotaKeys.updateAutoEntryConfig).mockReset()
  })

  it('自报 vendor 与真实 vendor 不符的已勾选模型在 loadConfig 后被就地修正，staleCleanedCount 含这次修正', async () => {
    const wrapper = await mountModalWithConfig(JSON.parse(JSON.stringify(misConfig)))

    // 该项仍渲染（在实供清单内），但 vendor 已从 xai 修正为 openai。
    const item = (wrapper.vm as any).localModels.find((m: any) => m.id === 'gpt-6-sol')
    expect(item).toBeTruthy()
    expect(item.vendor).toBe('openai')

    // 该项挂在 openai 厂商组下（而非错误的 xai）。
    const openaiInputs = wrapper
      .findAll('label')
      .filter((l) => l.text().includes('gpt-6-sol'))
    expect(openaiInputs.length).toBeGreaterThan(0)
    expect(openaiInputs[0].find('input[type="checkbox"]').element.checked).toBe(true)

    // 无幽灵项、仅 1 处归属修正 → staleCleanedCount 含这次修正。
    expect((wrapper.vm as any).staleCleanedCount).toBe(1)
    expect((wrapper.vm as any).staleVendorFixedCount).toBe(1)
  })

  it('save() payload 中该模型 vendor=openai（可直接落库，不再被后端 R8 归属校验拒绝），且提示归属修正', async () => {
    const wrapper = await mountModalWithConfig(JSON.parse(JSON.stringify(misConfig)))

    await clickSave(wrapper)
    const saved = vi.mocked(adminAPI.quotaKeys.updateAutoEntryConfig).mock.calls[
      vi.mocked(adminAPI.quotaKeys.updateAutoEntryConfig).mock.calls.length - 1
    ][0] as { models: { id: string; vendor: string }[] }
    const gptSol = saved.models.find((m) => m.id === 'gpt-6-sol')
    expect(gptSol).toBeTruthy()
    expect(gptSol!.vendor).toBe('openai')

    // 其余正常勾选仍然保留。
    expect(saved.models.map((m) => m.id)).toContain('gpt-6-astra')
    expect(saved.models.map((m) => m.id)).toContain('claude-opus-5-5')

    // 提示条出现且含归属修正计数（mock t 形如 key:count）。
    expect(wrapper.text()).toContain('admin.quotaKeys.autoEntry.vendorAttributionFixed:1')
  })

  it('无幽灵项也无归属修正时 staleCleanedCount=0 且不弹提示条', async () => {
    const wrapper = await mountModal()
    expect((wrapper.vm as any).staleCleanedCount).toBe(0)
    await clickSave(wrapper)
    expect(wrapper.text()).not.toContain('admin.quotaKeys.autoEntry.vendorAttributionFixed')
    expect(wrapper.text()).not.toContain('admin.quotaKeys.autoEntry.staleModelsCleaned')
  })
})
