import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

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
    useI18n: () => ({
      t: (key: string) => (key === 'common.copy' ? '复制' : key)
    })
  }
})

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess,
    showInfo,
    showWarning
  })
}))

vi.mock('@/api/admin/accounts', () => ({
  accountsAPI: {
    syncUpstreamModels,
    syncUpstreamModelsPreview,
    syncVolcanoPlanModels
  }
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard
  })
}))

import ModelWhitelistSelector from '../ModelWhitelistSelector.vue'

function mountSelector(props: Record<string, unknown> = {}) {
  return mount(ModelWhitelistSelector, {
    props: {
      modelValue: [],
      platform: 'openai',
      ...props,
    },
    global: {
      stubs: {
        ModelIcon: true
      }
    }
  })
}

function findModelRow(wrapper: ReturnType<typeof mountSelector>, modelId: string) {
  const row = wrapper
    .findAll('[data-testid="model-option"]')
    .find(candidate => candidate.text().includes(modelId))

  if (!row) {
    throw new Error(`Model row not found: ${modelId}`)
  }

  return row
}

describe('ModelWhitelistSelector', () => {
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

  function volcanoPreviewFull() {
    return {
      kind: 'coding',
      confirmed: ['glm-5.3', 'kimi-k2.7-code', 'minimax-m3'],
      unavailable: [],
      unverified: [],
      will_add: ['glm-5.3', 'kimi-k2.7-code', 'minimax-m3'],
      will_remove: [],
      full_confirm: true,
      applied: false,
      evidence: { kind: 'coding', urls: [], document_ids: [], titles: [], updated_times: [], candidate_count: 3 }
    }
  }

  async function clickSync(wrapper: ReturnType<typeof mountSelector>) {
    const syncButton = wrapper
      .findAll('button')
      .find(button => button.text() === 'admin.accounts.syncUpstreamModels')
    expect(syncButton).toBeDefined()
    await syncButton!.trigger('click')
    await flushPromises()
  }

  it('full-confirm volcano sync previews then replaces the whitelist on confirm', async () => {
    syncVolcanoPlanModels
      .mockResolvedValueOnce(volcanoPreviewFull())
      .mockResolvedValueOnce({ ...volcanoPreviewFull(), applied: true, confirmed: ['glm-5.3', 'kimi-k2.7-code', 'minimax-m3'] })
    vi.spyOn(window, 'confirm').mockReturnValue(true)

    const wrapper = mountSelector({
      modelValue: ['old-manual-model'],
      platform: 'deepseek',
      accountId: 101,
      baseUrl: 'https://ark.cn-beijing.volces.com/api/coding'
    })
    await clickSync(wrapper)

    expect(syncVolcanoPlanModels).toHaveBeenCalledWith(101, { apply: false })
    expect(syncVolcanoPlanModels).toHaveBeenLastCalledWith(101, { apply: true, expected_removals: [] })
    // 完全确认 → 替换后仍保留既有白名单中未下架的（人工 identity 不得被反向丢弃）。
    expect(wrapper.emitted('update:modelValue')).toEqual([[['old-manual-model', 'glm-5.3', 'kimi-k2.7-code', 'minimax-m3']]])
    expect(showSuccess).toHaveBeenCalled()
  })

  it('partial-confirm volcano sync merges confirmed models (add-only) and surfaces unverified', async () => {
    const base = volcanoPreviewFull()
    syncVolcanoPlanModels
      .mockResolvedValueOnce({
        ...base,
        confirmed: ['glm-5.3'],
        will_add: ['glm-5.3'],
        unverified: ['kimi-k2.7-code'],
        will_remove: [],
        full_confirm: false
      })
      .mockResolvedValueOnce({
        ...base,
        applied: true,
        confirmed: ['glm-5.3'],
        full_confirm: false
      })
    vi.spyOn(window, 'confirm').mockReturnValue(true)

    const wrapper = mountSelector({
      modelValue: ['existing-model'],
      platform: 'deepseek',
      accountId: 101,
      baseUrl: 'https://ark.cn-beijing.volces.com/api/plan'
    })
    await clickSync(wrapper)

    expect(syncVolcanoPlanModels).toHaveBeenCalledWith(101, { apply: false })
    expect(syncVolcanoPlanModels).toHaveBeenLastCalledWith(101, { apply: true, expected_removals: [] })
    // 部分确认 → 旧值 ∪ 新确认（合并）。
    expect(wrapper.emitted('update:modelValue')).toEqual([[['existing-model', 'glm-5.3']]])
    expect(showSuccess).toHaveBeenCalled()
  })

  it('cancels volcano sync apply when confirm is dismissed', async () => {
    syncVolcanoPlanModels.mockResolvedValue(volcanoPreviewFull())
    vi.spyOn(window, 'confirm').mockReturnValue(false)

    const wrapper = mountSelector({
      modelValue: [],
      platform: 'deepseek',
      accountId: 101,
      baseUrl: 'https://ark.cn-beijing.volces.com/api/coding'
    })
    await clickSync(wrapper)

    expect(syncVolcanoPlanModels).toHaveBeenCalledTimes(1) // 仅预览，不 apply
    expect(syncVolcanoPlanModels).not.toHaveBeenCalledWith(101, { apply: true })
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(showSuccess).not.toHaveBeenCalled()
  })

  it('copies a model ID without selecting the model', async () => {
    const wrapper = mountSelector()
    await wrapper.get('div.cursor-pointer').trigger('click')

    const row = findModelRow(wrapper, 'gpt-5.6-sol')

    const copyButton = row.get('[data-testid="copy-model-id"]')
    expect(copyButton.attributes('aria-label')).toBe('复制 gpt-5.6-sol')

    await copyButton.trigger('click')
    await flushPromises()

    expect(copyToClipboard).toHaveBeenCalledWith('gpt-5.6-sol')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })

  it('keeps the existing model selection behavior', async () => {
    const wrapper = mountSelector()
    await wrapper.get('div.cursor-pointer').trigger('click')

    const row = findModelRow(wrapper, 'gpt-5.6-sol')
    await row.get('[data-testid="select-model"]').trigger('click')

    expect(wrapper.emitted('update:modelValue')).toEqual([[['gpt-5.6-sol']]])
    expect(copyToClipboard).not.toHaveBeenCalled()
  })

  it('warns when model IDs sync but capability metadata is incomplete', async () => {
    syncUpstreamModels.mockResolvedValue({
      models: ['x-preview-f-free'],
      warnings: [
        {
          code: 'upstream_model_metadata_incomplete',
          message: 'Model IDs were synced, but capability metadata could not be updated.'
        }
      ]
    })
    const wrapper = mount(ModelWhitelistSelector, {
      props: {
        modelValue: [],
        platform: 'openai',
        accountId: 46
      },
      global: {
        stubs: {
          ModelIcon: true
        }
      }
    })

    const syncButton = wrapper
      .findAll('button')
      .find(button => button.text() === 'admin.accounts.syncUpstreamModels')
    expect(syncButton).toBeDefined()
    await syncButton!.trigger('click')
    await flushPromises()

    expect(wrapper.emitted('update:modelValue')).toEqual([[['x-preview-f-free']]])
    expect(showWarning).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsMetadataIncomplete')
    expect(showSuccess).not.toHaveBeenCalled()
  })

  it('warns on partial volcano model sync (transient probe failures)', async () => {
    syncUpstreamModels.mockResolvedValue({
      models: ['glm-5.3'],
      warnings: [
        {
          code: 'volcano_model_sync_partial',
          message: 'some volcano plan models could not be confirmed (transient failures); 1 models available'
        }
      ]
    })
    const wrapper = mount(ModelWhitelistSelector, {
      props: {
        modelValue: [],
        platform: 'deepseek',
        accountId: 47
      },
      global: {
        stubs: {
          ModelIcon: true
        }
      }
    })

    const syncButton = wrapper
      .findAll('button')
      .find(button => button.text() === 'admin.accounts.syncUpstreamModels')
    expect(syncButton).toBeDefined()
    await syncButton!.trigger('click')
    await flushPromises()

    expect(wrapper.emitted('update:modelValue')).toEqual([[['glm-5.3']]])
    expect(showWarning).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsVolcanoPartial')
    expect(showSuccess).not.toHaveBeenCalled()
  })

  it('does not enter the volcano flow for a same-suffix hostname', async () => {
    syncUpstreamModels.mockResolvedValue({ models: [] })
    const wrapper = mountSelector({
      modelValue: [],
      platform: 'deepseek',
      accountId: 101,
      baseUrl: 'https://notark.cn-beijing.volces.com/api/coding'
    })
    await clickSync(wrapper)

    // 相似子串主机不再误判为火山订阅号 → 走普通 syncUpstreamModels。
    expect(syncVolcanoPlanModels).not.toHaveBeenCalled()
    expect(syncUpstreamModels).toHaveBeenCalled()
  })

  it('applies a no-op full confirm to initialize managed state on an already-populated account', async () => {
    const noop = {
      kind: 'coding',
      confirmed: [],
      unavailable: [],
      unverified: [],
      will_add: [],
      will_remove: [],
      full_confirm: true,
      applied: false,
      evidence: { kind: 'coding', urls: [], document_ids: [], titles: [], updated_times: [], candidate_count: 0 }
    }
    syncVolcanoPlanModels
      .mockResolvedValueOnce(noop)
      .mockResolvedValueOnce({ ...noop, applied: true })
    vi.spyOn(window, 'confirm').mockReturnValue(true)

    const wrapper = mountSelector({
      modelValue: ['glm-5.3'],
      platform: 'deepseek',
      accountId: 101,
      baseUrl: 'https://ark.cn-beijing.volces.com/api/coding'
    })
    await clickSync(wrapper)

    // 全已收录 no-op 仍走 apply 以初始化/更新托管快照，不回退为“空即可退出”。
    expect(syncVolcanoPlanModels).toHaveBeenLastCalledWith(101, { apply: true, expected_removals: [] })
    expect(wrapper.emitted('update:modelValue')).toEqual([[['glm-5.3']]])
    expect(showSuccess).toHaveBeenCalled()
  })

  it('reports a successful preview so account creation can persist metadata', async () => {
    syncUpstreamModelsPreview.mockResolvedValue({
      models: ['x-preview-f-free'],
      metadata: {
        'x-preview-f-free': {
          id: 'x-preview-f-free',
          reasoning: true,
          supported_reasoning_levels: ['low', 'high', 'max'],
        },
      },
    })
    const wrapper = mountSelector({
      syncCredentials: {
        platform: 'openai',
        type: 'apikey',
        base_url: 'https://opencode.ai/zen/v1',
        api_key: 'test-key',
      },
    })
    const syncButton = wrapper
      .findAll('button')
      .find(button => button.text() === 'admin.accounts.syncUpstreamModels')

    expect(syncButton).toBeDefined()
    await syncButton?.trigger('click')
    await flushPromises()

    expect(syncUpstreamModelsPreview).toHaveBeenCalledOnce()
    expect(wrapper.emitted('upstream-synced')).toEqual([[]])
    expect(wrapper.emitted('update:modelValue')).toEqual([[['x-preview-f-free']]])
  })

  it('rejects volcano apply when the applied diff drifts beyond the confirmed preview', async () => {
    // preview 为部分确认（unverified 存在、无下架）；apply 时临时探活恢复升级为完全确认，
    // 出现 preview 未提示的下架 old-manual-model → 未获用户确认的破坏性收紧，必须拒绝。
    const base = volcanoPreviewFull()
    syncVolcanoPlanModels
      .mockResolvedValueOnce({
        ...base,
        confirmed: ['kimi-k2.7-code'],
        will_add: ['kimi-k2.7-code'],
        will_remove: [],
        unverified: ['glm-5.3'],
        full_confirm: false
      })
      .mockResolvedValueOnce({
        ...base,
        confirmed: ['kimi-k2.7-code'],
        will_add: ['kimi-k2.7-code'],
        will_remove: ['old-manual-model'],
        unverified: [],
        full_confirm: true,
        applied: true
      })
    vi.spyOn(window, 'confirm').mockReturnValue(true)

    const wrapper = mountSelector({
      modelValue: ['old-manual-model'],
      platform: 'deepseek',
      accountId: 101,
      baseUrl: 'https://ark.cn-beijing.volces.com/api/coding'
    })
    await clickSync(wrapper)

    expect(syncVolcanoPlanModels).toHaveBeenLastCalledWith(101, { apply: true, expected_removals: [] })
    expect(showError).toHaveBeenCalledWith('admin.accounts.syncVolcanoPlanDrifted')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(showSuccess).not.toHaveBeenCalled()
  })

  it('registers volcano-synced models into the searchable dropdown', async () => {
    // kimi-k3 不在前端静态模型库（allModels）里，但同步确认后应并入下拉，方便误删后重选。
    const base = volcanoPreviewFull()
    syncVolcanoPlanModels
      .mockResolvedValueOnce({
        ...base,
        confirmed: ['kimi-k3'],
        will_add: ['kimi-k3'],
        full_confirm: true
      })
      .mockResolvedValueOnce({
        ...base,
        confirmed: ['kimi-k3'],
        will_add: ['kimi-k3'],
        applied: true,
        full_confirm: true
      })
    vi.spyOn(window, 'confirm').mockReturnValue(true)

    const wrapper = mountSelector({
      modelValue: [],
      platform: 'deepseek',
      accountId: 101,
      baseUrl: 'https://ark.cn-beijing.volces.com/api/coding'
    })
    await clickSync(wrapper)

    expect(wrapper.emitted('update:modelValue')).toEqual([[['kimi-k3']]])
    // 打开下拉后，kimi-k3 应出现在可选项里（动态同步模型进入运行时下拉）。
    await wrapper.get('div.cursor-pointer').trigger('click')
    expect(wrapper.text()).toContain('kimi-k3')
  })
})
