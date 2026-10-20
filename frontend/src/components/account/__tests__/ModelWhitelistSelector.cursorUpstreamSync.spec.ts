import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

// P1-B3 前端管理收口：cursor 平台必须能发起上游模型同步（canSyncUpstream 为真）。
// 覆盖两条入口：已保存账号（accountId 走 syncUpstreamModels）与创建预览
// （syncCredentials 走 syncUpstreamModelsPreview）。

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

describe('ModelWhitelistSelector cursor 上游模型同步入口（P1-B3）', () => {
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

  it('cursor 已保存账号渲染同步按钮（canSyncUpstream 为真）', () => {
    const wrapper = mountSelector({ platform: 'cursor', accountId: 46 })
    expect(findSyncButton(wrapper)).toBeDefined()
    wrapper.unmount()
  })

  it('cursor 创建预览（syncCredentials）渲染同步按钮', () => {
    const wrapper = mountSelector({
      syncCredentials: { platform: 'cursor', type: 'apikey', api_key: 'test-key' }
    })
    expect(findSyncButton(wrapper)).toBeDefined()
    wrapper.unmount()
  })

  it('cursor 多平台数组入口同样可同步', () => {
    const wrapper = mountSelector({ platforms: ['cursor'], accountId: 46 })
    expect(findSyncButton(wrapper)).toBeDefined()
    wrapper.unmount()
  })

  it('点击同步对 cursor 已保存账号发起 syncUpstreamModels', async () => {
    syncUpstreamModels.mockResolvedValue({ models: ['cursor-auto-1'] })
    const wrapper = mountSelector({ platform: 'cursor', accountId: 46 })

    const button = findSyncButton(wrapper)
    expect(button).toBeDefined()
    await button!.trigger('click')
    await flushPromises()

    expect(syncUpstreamModels).toHaveBeenCalledWith(46)
    expect(syncUpstreamModelsPreview).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('点击同步对 cursor 创建预览发起 syncUpstreamModelsPreview', async () => {
    syncUpstreamModelsPreview.mockResolvedValue({ models: ['cursor-auto-1'] })
    const credentials = { platform: 'cursor', type: 'apikey', api_key: 'test-key' }
    const wrapper = mountSelector({ syncCredentials: credentials })

    const button = findSyncButton(wrapper)
    expect(button).toBeDefined()
    await button!.trigger('click')
    await flushPromises()

    expect(syncUpstreamModelsPreview).toHaveBeenCalledOnce()
    expect(syncUpstreamModels).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
