import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

// 阿里 Token Plan 是聚合订阅：一份套餐同时给 qwen / glm / deepseek / kimi 等全家族模型，
// 账号挂哪个分组只是路由与伪装身份。若按分组词根过滤上游模型，qwen 等家族会被整体丢掉，
// 同步结果只剩 glm 几个模型 —— 这正是该功能的原始缺陷。
// 本测试锁住「阿里 Token Plan 跳过平台过滤、全量加入白名单」这条规则。

const {
  copyToClipboard,
  showError,
  showSuccess,
  showInfo,
  showWarning,
  syncUpstreamModels,
  syncUpstreamModelsPreview,
  syncVolcanoPlanModels
} = vi.hoisted(() => ({
  copyToClipboard: vi.fn().mockResolvedValue(true),
  showError: vi.fn(),
  showSuccess: vi.fn(),
  showInfo: vi.fn(),
  showWarning: vi.fn(),
  syncUpstreamModels: vi.fn(),
  syncUpstreamModelsPreview: vi.fn(),
  syncVolcanoPlanModels: vi.fn()
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess, showInfo, showWarning })
}))

vi.mock('@/api/admin/accounts', () => ({
  accountsAPI: { syncUpstreamModels, syncUpstreamModelsPreview, syncVolcanoPlanModels }
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard })
}))

import ModelWhitelistSelector from '../ModelWhitelistSelector.vue'

const ALIYUN_TOKEN_PLAN_BASE_URL =
  'https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1'

// 与真实上游返回一致的混合家族模型（qwen / glm / deepseek / kimi）。
const MIXED_FAMILY_MODELS = [
  'qwen3.8-flash',
  'qwen3.8-max',
  'glm-5.3-flash',
  'deepseek-v4.1-flash',
  'kimi-k3.1-pro'
]

function mountSelector(props: Record<string, unknown>) {
  return mount(ModelWhitelistSelector, {
    props: {
      modelValue: [],
      ...props
    },
    global: {
      stubs: { ModelIcon: true }
    }
  })
}

function findSyncButton(wrapper: ReturnType<typeof mountSelector>) {
  return wrapper
    .findAll('button')
    .find(button => button.text() === 'admin.accounts.syncUpstreamModels')
}

describe('ModelWhitelistSelector 阿里 Token Plan 聚合订阅上游同步', () => {
  beforeEach(() => {
    copyToClipboard.mockClear()
    showError.mockReset()
    showSuccess.mockReset()
    showInfo.mockReset()
    showWarning.mockReset()
    syncUpstreamModels.mockReset()
    syncUpstreamModelsPreview.mockReset()
    syncVolcanoPlanModels.mockReset()
  })

  it('阿里 Token Plan 账号同步上游模型时全家族模型都进白名单（不过滤 qwen）', async () => {
    syncUpstreamModels.mockResolvedValue({ models: MIXED_FAMILY_MODELS })
    // 生产实况：阿里账号平台被存成伪装身份（deepseek / kimi / zhipu），
    // 这里取 zhipu（智谱 GLM）这一档。
    const wrapper = mountSelector({
      platform: 'zhipu',
      accountId: 1012,
      baseUrl: ALIYUN_TOKEN_PLAN_BASE_URL
    })

    const button = findSyncButton(wrapper)
    expect(button).toBeDefined()
    await button!.trigger('click')
    await flushPromises()

    const emitted = wrapper.emitted('update:modelValue')
    expect(emitted).toBeTruthy()
    const models = emitted![emitted!.length - 1][0] as string[]
    // 关键断言：qwen 家族没被分组词根过滤掉。
    expect(models).toContain('qwen3.8-flash')
    expect(models).toContain('qwen3.8-max')
    expect(models).toContain('glm-5.3-flash')
    expect(models).toContain('deepseek-v4.1-flash')
    expect(models).toContain('kimi-k3.1-pro')
    wrapper.unmount()
  })

  it('阿里 Token Plan 创建预览路径同样不过滤平台', async () => {
    syncUpstreamModelsPreview.mockResolvedValue({ models: MIXED_FAMILY_MODELS })
    const wrapper = mountSelector({
      syncCredentials: {
        platform: 'zhipu',
        type: 'apikey',
        api_key: 'sk-sp-test',
        base_url: ALIYUN_TOKEN_PLAN_BASE_URL
      }
    })

    const button = findSyncButton(wrapper)
    expect(button).toBeDefined()
    await button!.trigger('click')
    await flushPromises()

    const emitted = wrapper.emitted('update:modelValue')
    expect(emitted).toBeTruthy()
    const models = emitted![emitted!.length - 1][0] as string[]
    expect(models).toContain('qwen3.8-flash')
    expect(models).toContain('kimi-k3.1-pro')
    wrapper.unmount()
  })

  it('非阿里订阅账号仍按平台过滤（规则不外溢）', async () => {
    syncUpstreamModels.mockResolvedValue({ models: MIXED_FAMILY_MODELS })
    const wrapper = mountSelector({
      platform: 'zhipu',
      accountId: 201,
      baseUrl: 'https://api.deepseek.com/v1'
    })

    const button = findSyncButton(wrapper)
    expect(button).toBeDefined()
    await button!.trigger('click')
    await flushPromises()

    const emitted = wrapper.emitted('update:modelValue')
    expect(emitted).toBeTruthy()
    const models = emitted![emitted!.length - 1][0] as string[]
    // zhipu 分组只保留 glm 家族，qwen 必须被过滤掉。
    expect(models).toContain('glm-5.3-flash')
    expect(models).not.toContain('qwen3.8-flash')
    expect(models).not.toContain('kimi-k3.1-pro')
    wrapper.unmount()
  })
})
