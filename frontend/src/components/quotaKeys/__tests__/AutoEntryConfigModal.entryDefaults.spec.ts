import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import AutoEntryConfigModal from '../AutoEntryConfigModal.vue'
import { adminAPI } from '@/api/admin'

// P1-B3 入库配置收口：
//   1) 模型能力改自动来源展示（提示自动来源，不提供手工录入）；
//   2) priority 只读保留（固定 1）、并发只读；
//   3) cursor 单独倍率编辑收起——UI 不再展示，但保存时原样透传后端存量值，
//      不批改历史非 1 值（后端存量不动）。

const baseConfig = {
  vendors: { openai: true, anthropic: false },
  models: [{ id: 'gpt-6-astra', vendor: 'openai', probe_status: '' }],
  // 历史非 1 的存量倍率：UI 收起后仍应原样回写。
  entry_defaults: { concurrency: null, priority: 1, rate_multiplier: 1.5 },
  available_models: [
    { id: 'gpt-6-astra', vendor: 'openai' },
    { id: 'gpt-5.6-sol', vendor: 'openai' }
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
    useI18n: () => ({
      t: (key: string, named?: Record<string, unknown>) => {
        if (named && typeof named.count === 'number') return `${key}:${named.count}`
        return key
      }
    })
  }
})

function findDefaultInput(wrapper: ReturnType<typeof mount>, labelKey: string): HTMLInputElement {
  const label = wrapper.findAll('label').find((l) => l.text() === labelKey)
  expect(label, `label not found: ${labelKey}`).toBeTruthy()
  const input = label!.element.parentElement?.querySelector('input')
  expect(input, `input not found for: ${labelKey}`).toBeTruthy()
  return input as HTMLInputElement
}

async function mountModal(cfg: unknown = baseConfig) {
  const wrapper = mount(AutoEntryConfigModal, {
    props: { show: false },
    global: {
      stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } }
    }
  })
  vi.mocked(adminAPI.quotaKeys.getAutoEntryConfig).mockResolvedValue(
    JSON.parse(JSON.stringify(cfg)) as never
  )
  vi.mocked(adminAPI.quotaKeys.updateAutoEntryConfig).mockResolvedValue({} as never)
  await wrapper.setProps({ show: true })
  await flushPromises()
  await nextTick()
  return wrapper
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

function lastPayload(): { entry_defaults: { priority: number; rate_multiplier: number } } {
  const calls = vi.mocked(adminAPI.quotaKeys.updateAutoEntryConfig).mock.calls
  return calls[calls.length - 1][0] as never
}

describe('AutoEntryConfigModal 入库默认值收口（P1-B3）', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.getAutoEntryConfig).mockReset()
    vi.mocked(adminAPI.quotaKeys.updateAutoEntryConfig).mockReset()
  })

  it('模型能力展示自动来源提示', async () => {
    const wrapper = await mountModal()
    expect(wrapper.text()).toContain('admin.quotaKeys.autoEntry.modelsSourceAuto')
  })

  it('cursor 单独倍率编辑不再展示（无倍率输入）', async () => {
    const wrapper = await mountModal()
    expect(wrapper.text()).not.toContain('admin.quotaKeys.autoEntry.rateMultiplier')
    // 全表单内不存在可编辑的倍率输入框：非复选框的输入必须是只读/禁用态。
    const editableInputs = wrapper
      .findAll('input')
      .filter(
        (input) =>
          input.attributes('disabled') === undefined &&
          input.attributes('readonly') === undefined
      )
    expect(editableInputs.map((input) => input.attributes('type'))).toEqual(
      new Array(editableInputs.length).fill('checkbox')
    )
  })

  it('priority 只读且保留 1', async () => {
    const wrapper = await mountModal()
    const priority = findDefaultInput(wrapper, 'admin.quotaKeys.autoEntry.priority')
    expect(priority.disabled).toBe(true)
    expect(priority.readOnly).toBe(true)
    expect(priority.value).toBe('1')
  })

  it('并发只读', async () => {
    const wrapper = await mountModal()
    const concurrency = findDefaultInput(wrapper, 'admin.quotaKeys.autoEntry.concurrency')
    expect(concurrency.disabled).toBe(true)
    expect(concurrency.readOnly).toBe(true)
  })

  it('保存时原样透传后端存量倍率（不批改历史非 1 值）', async () => {
    const wrapper = await mountModal()
    await clickSave(wrapper)
    const payload = lastPayload()
    expect(payload.entry_defaults.rate_multiplier).toBe(1.5)
    expect(payload.entry_defaults.priority).toBe(1)
  })

  it('后端缺省倍率为 1 时透传 1', async () => {
    const wrapper = await mountModal({
      ...baseConfig,
      entry_defaults: { concurrency: null, priority: 1, rate_multiplier: 1 }
    })
    await clickSave(wrapper)
    expect(lastPayload().entry_defaults.rate_multiplier).toBe(1)
  })
})
