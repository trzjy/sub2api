import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const {
  createAccountMock,
  probeUpstreamBillingMock,
  syncUpstreamModelsMock,
  showErrorMock,
  showWarningMock,
  importCodexSessionMock,
  createOpenAICodexPATMock,
  authIsSimpleMode,
} = vi.hoisted(() => ({
  createAccountMock: vi.fn(),
  probeUpstreamBillingMock: vi.fn(),
  syncUpstreamModelsMock: vi.fn(),
  showErrorMock: vi.fn(),
  showWarningMock: vi.fn(),
  importCodexSessionMock: vi.fn(),
  createOpenAICodexPATMock: vi.fn(),
  authIsSimpleMode: { value: true },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock,
    showSuccess: vi.fn(),
    showWarning: showWarningMock,
  }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get isSimpleMode() {
      return authIsSimpleMode.value
    },
  }),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      create: createAccountMock,
      probeUpstreamBilling: probeUpstreamBillingMock,
      syncUpstreamModels: syncUpstreamModelsMock,
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
      importCodexSession: importCodexSessionMock,
      createOpenAICodexPAT: createOpenAICodexPATMock,
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({}),
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([]),
    },
  },
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn().mockResolvedValue([]),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

import CreateAccountModal from '../CreateAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})

const OAuthAuthorizationFlowStub = defineComponent({
  name: 'OAuthAuthorizationFlow',
  props: {
    showManualOption: Boolean,
    showCodexSessionImportOption: Boolean,
    showAgentIdentityOption: Boolean,
    showCodexPatOption: Boolean,
    initialInputMethod: String,
  },
  data: () => ({ inputMethod: 'manual' }),
  emits: ['import-codex-session', 'import-codex-pat'],
  template: `
    <div>
      <button data-testid="import-codex-session" @click="$emit('import-codex-session', 'session-json')">session</button>
      <button data-testid="import-codex-pat" @click="$emit('import-codex-pat', 'pat-token')">pat</button>
    </div>
  `,
})

const GroupSelectorStub = defineComponent({
  name: 'GroupSelector',
  props: {
    modelValue: {
      type: Array,
      default: () => [],
    },
  },
  emits: ['update:modelValue'],
  template: `
    <button
      type="button"
      data-testid="select-pricing-groups"
      @click="$emit('update:modelValue', [1, 2])"
    >
      groups
    </button>
  `,
})

const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: {
    modelValue: {
      type: Array,
      default: () => [],
    },
    platform: String,
    syncCredentials: Object,
  },
  emits: ['update:modelValue', 'upstream-synced'],
  template: `<button
    type="button"
    data-testid="model-whitelist-selector"
    @click="$emit('update:modelValue', ['public-glm']); $emit('upstream-synced')"
  >models</button>`,
})

function mountModal(groups: any[] = []) {
  return mount(CreateAccountModal, {
    props: { show: true, proxies: [], groups },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        OAuthAuthorizationFlow: OAuthAuthorizationFlowStub,
        ConfirmDialog: true,
        Select: true,
        Icon: true,
        PlatformIcon: true,
        ProxySelector: true,
        ProxyAdBanner: true,
        GroupSelector: GroupSelectorStub,
        ModelWhitelistSelector: ModelWhitelistSelectorStub,
        QuotaLimitCard: true,
      },
    },
  })
}

// 网页版二级入口：选 CN 基础平台卡片（卡片标签为英文名）→ 点账号类型"网页版"。
async function selectWebModeViaCnPlatform(wrapper: ReturnType<typeof mountModal>, cardLabel: 'Kimi' | 'Zhipu GLM' | 'DeepSeek') {
  await selectButtonByText(wrapper, cardLabel)
  await flushPromises()
  await wrapper.get('[data-testid="cn-web-mode"]').trigger('click')
  await flushPromises()
}

async function selectButtonByText(wrapper: ReturnType<typeof mountModal>, text: string) {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(text))
  expect(button).toBeDefined()
  await button?.trigger('click')
}

async function submitApiKeyAccount(
  platform: 'openai' | 'anthropic',
  enableLongContextBilling = false,
  disableUpstreamBillingProbe = false
) {
  const wrapper = mountModal()
  await selectButtonByText(wrapper, platform === 'openai' ? 'OpenAI' : 'admin.accounts.claudeConsole')
  if (platform === 'openai') {
    await selectButtonByText(wrapper, 'API Key')
  }
  await wrapper.get('form#create-account-form input[type="text"]').setValue(`${platform} account`)
  await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
  if (enableLongContextBilling) {
    await wrapper.get('[data-testid="openai-long-context-billing-toggle"]').trigger('click')
  }
  if (disableUpstreamBillingProbe) {
    await wrapper.get('[data-testid="upstream-billing-auto-probe"]').trigger('click')
  }
  await wrapper.get('form#create-account-form').trigger('submit.prevent')
  await flushPromises()
  return wrapper
}

async function openCodexImportStep(toggleClicks = 0) {
  const wrapper = mountModal()
  await selectButtonByText(wrapper, 'OpenAI')
  for (let click = 0; click < toggleClicks; click += 1) {
    await wrapper.get('[data-testid="openai-long-context-billing-toggle"]').trigger('click')
  }
  await wrapper.get('form#create-account-form input[type="text"]').setValue('Codex import')
  await wrapper.get('form#create-account-form').trigger('submit.prevent')
  return wrapper
}

describe('CreateAccountModal OpenAI long-context billing', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    createAccountMock.mockReset().mockResolvedValue({ id: 42, platform: 'openai', type: 'apikey' })
    probeUpstreamBillingMock.mockReset().mockResolvedValue({})
    syncUpstreamModelsMock.mockReset().mockResolvedValue({ models: [], metadata: {} })
    showWarningMock.mockReset()
    importCodexSessionMock.mockReset().mockResolvedValue({
      created: 1,
      updated: 0,
      skipped: 0,
      failed: 0,
      errors: [],
      warnings: [],
    })
    createOpenAICodexPATMock.mockReset().mockResolvedValue({})
  })

  afterEach(() => vi.useRealTimers())

  it('sets month and year expiry presets without submitting the account form', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date('2026-01-31T12:34:00'))
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('expiry account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    const input = wrapper.get<HTMLInputElement>('input[type="datetime-local"]')

    for (const [label, expected] of [
      ['payment.oneMonth', '2026-02-28T12:34'],
      ['payment.oneYear', '2027-01-31T12:34'],
    ]) {
      const button = wrapper.findAll('button').find((candidate) => candidate.text() === label)!
      expect(button.attributes('type')).toBe('button')
      await button.trigger('click')
      expect(input.element.value).toBe(expected)
      expect(createAccountMock).not.toHaveBeenCalled()
    }

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    expect(createAccountMock.mock.calls[0]?.[0]?.expires_at).toBe(new Date('2027-01-31T12:34:00').getTime() / 1000)
    wrapper.unmount()
  })

  it('allows a manually entered expiry to override a preset before account creation', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('custom expiry account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await selectButtonByText(wrapper, 'payment.oneMonth')
    await wrapper.get('input[type="datetime-local"]').setValue('2030-04-15T09:20')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    expect(createAccountMock.mock.calls[0]?.[0]?.expires_at).toBe(new Date('2030-04-15T09:20:00').getTime() / 1000)
    wrapper.unmount()
  })

  it('hides only the redundant account toggle when every selected group enables tier pricing', async () => {
    authIsSimpleMode.value = false
    const wrapper = mountModal([
      { id: 1, long_context_pricing_enabled: true },
      { id: 2, long_context_pricing_enabled: true },
    ])

    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('[data-testid="select-pricing-groups"]').trigger('click')

    expect(wrapper.find('[data-testid="openai-long-context-billing-toggle"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="create-openai-ws-mode"]').exists()).toBe(true)
  })

  it('keeps the account toggle when any selected group disables tier pricing', async () => {
    authIsSimpleMode.value = false
    const wrapper = mountModal([
      { id: 1, long_context_pricing_enabled: true },
      { id: 2, long_context_pricing_enabled: false },
    ])

    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('[data-testid="select-pricing-groups"]').trigger('click')

    expect(wrapper.find('[data-testid="openai-long-context-billing-toggle"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="create-openai-ws-mode"]').exists()).toBe(true)
  })

  it('sends false explicitly for normal OpenAI account creation by default', async () => {
    await submitApiKeyAccount('openai')

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })

  it('omits the upstream request id header from extra when left empty', async () => {
    await submitApiKeyAccount('openai')

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra).not.toHaveProperty('upstream_request_id_header')
  })

  it('sends the trimmed upstream request id header in extra when filled', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('openai account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="upstream-request-id-header"]').setValue('  X-Oneapi-Request-Id  ')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.upstream_request_id_header).toBe('X-Oneapi-Request-Id')
  })

  it('omits images_url_to_b64_json from extra by default', async () => {
    await submitApiKeyAccount('openai')

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra).not.toHaveProperty('images_url_to_b64_json')
  })

  it('sends images_url_to_b64_json in extra when the toggle is enabled', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('openai account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="openai-images-url-to-b64-json-toggle"]').trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.images_url_to_b64_json).toBe(true)
  })

  it('persists upstream model metadata after creating an account from preview', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('OpenCode account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="model-whitelist-selector"]').trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledOnce()
    expect(syncUpstreamModelsMock).toHaveBeenCalledWith(42)
  })

  it('includes the current concrete model mapping in preview credentials', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="model-whitelist-selector"]').trigger('click')
    await flushPromises()

    expect(wrapper.getComponent(ModelWhitelistSelectorStub).props('syncCredentials')).toMatchObject({
      model_mapping: { 'public-glm': 'public-glm' }
    })
  })

  it('runs formal capability sync after creating an account with explicit mappings', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Mapped account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await selectButtonByText(wrapper, 'admin.accounts.modelMapping')
    await selectButtonByText(wrapper, 'admin.accounts.addMapping')
    await wrapper.get('input[placeholder="admin.accounts.requestModel"]').setValue('public-glm')
    await wrapper.get('input[placeholder="admin.accounts.actualModel"]').setValue('glm-5.3')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock.mock.calls[0]?.[0]?.credentials?.model_mapping).toEqual({
      'public-glm': 'glm-5.3'
    })
    expect(syncUpstreamModelsMock).toHaveBeenCalledWith(42)
  })

  it('warns when post-create capability metadata remains incomplete', async () => {
    syncUpstreamModelsMock.mockResolvedValue({
      models: ['x-preview-f-free'],
      warnings: [{ code: 'upstream_model_metadata_incomplete', message: 'metadata incomplete' }],
    })
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('OpenCode account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="model-whitelist-selector"]').trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(showWarningMock).toHaveBeenCalledWith(
      'admin.accounts.syncUpstreamModelsMetadataIncomplete'
    )
  })

  // namespace 摊平是仅 OAuth 的兼容开关：API Key 走 chat completions 回退桥时由桥自行摊平
  it('shows the Codex namespace flatten toggle only for OpenAI OAuth accounts', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')

    expect(wrapper.find('[data-testid="create-openai-flatten-namespaces-toggle"]').exists()).toBe(
      true
    )

    await selectButtonByText(wrapper, 'API Key')
    expect(wrapper.find('[data-testid="create-openai-flatten-namespaces-toggle"]').exists()).toBe(
      false
    )
  })

  it('enables upstream billing probes by default for new OpenAI API key accounts', async () => {
    await submitApiKeyAccount('openai')

    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBe(true)
  })

  it('waits for the initial upstream billing probe before refreshing the account list', async () => {
    let resolveProbe: (() => void) | undefined
    probeUpstreamBillingMock.mockImplementationOnce(
      () => new Promise<void>((resolve) => {
        resolveProbe = resolve
      })
    )

    const wrapper = await submitApiKeyAccount('openai')

    expect(probeUpstreamBillingMock).toHaveBeenCalledWith(42)
    expect(wrapper.emitted('created')).toBeUndefined()

    resolveProbe?.()
    await flushPromises()

    expect(wrapper.emitted('created')).toHaveLength(1)
  })

  it('sends an explicit disabled state when the create toggle is turned off', async () => {
    await submitApiKeyAccount('openai', false, true)

    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBe(false)
    expect(probeUpstreamBillingMock).not.toHaveBeenCalled()
  })

  it('submits adaptive Kimi protocol endpoints', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Kimi')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Kimi adaptive')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-kimi')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://api.moonshot.cn/v1',
      api_base_urls: {
        chat_completions: 'https://api.moonshot.cn/v1',
        anthropic: 'https://api.moonshot.cn/anthropic',
        responses: 'https://api.moonshot.cn/v1'
      }
    })
  })

  it('submits adaptive Kimi Coding Plan Responses endpoint', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Kimi')
    await selectButtonByText(wrapper, 'admin.accounts.cnProviders.accountMode.coding')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Kimi coding')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-kimi-coding')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      account_mode: 'coding',
      api_protocol: 'adaptive',
      base_url: 'https://api.kimi.com/coding/v1',
      api_base_urls: {
        chat_completions: 'https://api.kimi.com/coding/v1',
        anthropic: 'https://api.kimi.com/coding',
        responses: 'https://api.kimi.com/coding/v1'
      }
    })
  })

  it('submits adaptive MiniMax protocol endpoints', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'MiniMax')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('MiniMax adaptive')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-minimax')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://api.minimaxi.com/v1',
      api_base_urls: {
        chat_completions: 'https://api.minimaxi.com/v1',
        anthropic: 'https://api.minimaxi.com/anthropic',
        responses: 'https://api.minimaxi.com/v1'
      }
    })
  })

  it('uses the edited adaptive Chat endpoint when previewing upstream models', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Kimi')
    await wrapper
      .get('[data-testid="cn-adaptive-base-url-chat_completions"]')
      .setValue('https://relay.example.com/v1')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-relay')

    expect(wrapper.getComponent(ModelWhitelistSelectorStub).props('syncCredentials')).toMatchObject({
      platform: 'kimi',
      type: 'apikey',
      base_url: 'https://relay.example.com/v1',
      api_key: 'sk-relay'
    })
  })

  it('shows a protocol picker and submits anthropic protocol for other accounts', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Other')
    const textInputs = wrapper.findAll('form#create-account-form input[type="text"]')
    expect(textInputs.length).toBeGreaterThanOrEqual(2)
    await textInputs[0]!.setValue('Other anthropic upstream')
    await textInputs[1]!.setValue('https://api.lkeap.cloud.tencent.com/plan/anthropic')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-lkeap')

    // other 双协议选择器可见（chat_completions 默认选中）。按钮含标题+描述两个 span，
    // 用 includes 匹配 i18n key 前缀。
    const chatBtn = wrapper.findAll('button').find(b => b.text().includes('admin.accounts.cnProviders.apiProtocol.chatCompletions'))
    const anthropicBtn = wrapper.findAll('button').find(b => b.text().includes('admin.accounts.cnProviders.apiProtocol.anthropic'))
    expect(chatBtn).toBeDefined()
    expect(anthropicBtn).toBeDefined()

    await anthropicBtn!.trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      base_url: 'https://api.lkeap.cloud.tencent.com/plan/anthropic',
      api_protocol: 'anthropic'
    })
  })

  it('sends api_protocol in syncCredentials for other accounts', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Other')
    const textInputs = wrapper.findAll('form#create-account-form input[type="text"]')
    await textInputs[1]!.setValue('https://openrouter.ai/api/v1')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-or')

    expect(wrapper.getComponent(ModelWhitelistSelectorStub).props('syncCredentials')).toMatchObject({
      platform: 'other',
      type: 'apikey',
      base_url: 'https://openrouter.ai/api/v1',
      api_key: 'sk-or',
      api_protocol: 'chat_completions'
    })
  })

  it('exposes Agent Identity in the OpenAI authorization methods', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('OpenAI account')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')

    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    expect(flow.props('showManualOption')).toBe(true)
    expect(flow.props('showCodexSessionImportOption')).toBe(true)
    expect(flow.props('showAgentIdentityOption')).toBe(true)
    expect(flow.props('showCodexPatOption')).toBe(true)
    expect(flow.props('initialInputMethod')).toBe('manual')
  })

  it.each([
    ['camelCase', { authMode: 'agentIdentity', agentIdentity: { agentRuntimeId: 'runtime' } }],
    ['nested identity without auth_mode', { agent_identity: { agent_runtime_id: 'runtime' } }],
  ])('accepts backend-compatible %s Agent Identity imports', async (_name, content) => {
    const wrapper = await openCodexImportStep()
    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    flow.vm.inputMethod = 'agent_identity'

    flow.vm.$emit('import-codex-session', JSON.stringify(content))
    await flushPromises()

    expect(importCodexSessionMock).toHaveBeenCalledTimes(1)
  })

  it('sends true explicitly when OpenAI long-context billing is enabled', async () => {
    await submitApiKeyAccount('openai', true)

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(true)
  })

  it('omits the OpenAI setting for non-OpenAI account creation', async () => {
    await submitApiKeyAccount('anthropic')

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
    // 上游倍率探测已放宽到全部 API-key 平台：非 OpenAI 平台与 OpenAI 一致，默认开启。
    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBe(true)
  })

  it('sends an explicit disabled state when the non-OpenAI create toggle is turned off', async () => {
    await submitApiKeyAccount('anthropic', false, true)

    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBe(false)
  })

  it('antigravity upstream 创建默认携带上游倍率探测开关', async () => {
    // antigravity upstream 走独立创建 helper，
    // 也必须与其余 API-key 平台一样默认开启探测并传递开关。
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Antigravity')
    await selectButtonByText(wrapper, 'admin.accounts.types.antigravityApikey')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('antigravity relay')
    const baseInput = wrapper
      .findAll('input')
      .find((candidate) => candidate.attributes('placeholder') === 'https://cloudcode-pa.googleapis.com')
    expect(baseInput).toBeDefined()
    await baseInput?.setValue('https://relay.example')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-upstream')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    const payload = createAccountMock.mock.calls[0]?.[0]
    expect(payload?.platform).toBe('antigravity')
    expect(payload?.type).toBe('apikey')
    expect(payload?.upstream_billing_probe_enabled).toBe(true)
    // 创建成功后前端立即发起一次首探（与其他 apikey 平台一致）。
    expect(probeUpstreamBillingMock).toHaveBeenCalledWith(42)
  })

  it('leaves Codex session import billing ownership to the backend', async () => {
    const wrapper = await openCodexImportStep()
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock).toHaveBeenCalledTimes(1)
    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
  })

  it('leaves Codex PAT import billing ownership to the backend', async () => {
    const wrapper = await openCodexImportStep()
    await wrapper.get('[data-testid="import-codex-pat"]').trigger('click')
    await flushPromises()

    expect(createOpenAICodexPATMock).toHaveBeenCalledTimes(1)
    expect(createOpenAICodexPATMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
  })

  it('sends explicit true for Codex session import after the toggle is enabled', async () => {
    const wrapper = await openCodexImportStep(1)
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(true)
  })

  it('sends explicit false for Codex session import after the toggle is changed back', async () => {
    const wrapper = await openCodexImportStep(2)
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })

  it('sends explicit true for Codex PAT import after the toggle is enabled', async () => {
    const wrapper = await openCodexImportStep(1)
    await wrapper.get('[data-testid="import-codex-pat"]').trigger('click')
    await flushPromises()

    expect(createOpenAICodexPATMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(true)
  })

  it('sends explicit false for Codex PAT import after the toggle is changed back', async () => {
    const wrapper = await openCodexImportStep(2)
    await wrapper.get('[data-testid="import-codex-pat"]').trigger('click')
    await flushPromises()

    expect(createOpenAICodexPATMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })
})

describe('CreateAccountModal volcano subscription', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    createAccountMock.mockReset().mockResolvedValue({ id: 1, platform: 'deepseek', type: 'apikey' })
    probeUpstreamBillingMock.mockReset().mockResolvedValue({})
    syncUpstreamModelsMock.mockReset().mockResolvedValue({ models: [], metadata: {} })
  })

  async function openVolcano(wrapper: ReturnType<typeof mountModal>, baseUrl: string) {
    await selectButtonByText(wrapper, 'DeepSeek')
    await wrapper.get('[data-testid="cn-adaptive-base-url-chat_completions"]').setValue(baseUrl)
  }

  it('creates a Volcano subscription account using only the ark api_key (no AK/SK)', async () => {
    const wrapper = mountModal()
    await openVolcano(wrapper, 'https://ark.cn-beijing.volces.com/api/plan')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('volcano account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('ark-test-key')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalled()
    const creds = createAccountMock.mock.calls[0]?.[0]?.credentials
    expect(creds?.base_url).toBe('https://ark.cn-beijing.volces.com/api/plan')
    // 火山订阅号用量探测走 ark API Key Bearer + 真实请求，无需 AK/SK 签名，
    // 创建时不应写入 access_key/secret_key。
    expect(creds?.access_key).toBeUndefined()
    expect(creds?.secret_key).toBeUndefined()
  })

  it('does not write AK/SK for consecutive Volcano account creations', async () => {
    const wrapper = mountModal()
    await openVolcano(wrapper, 'https://ark.cn-beijing.volces.com/api/plan')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('volcano A')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('ark-key-a')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    // 关闭再打开 → 重新创建第二个火山账号
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()

    createAccountMock.mockResolvedValue({ id: 2, platform: 'deepseek', type: 'apikey' })
    await openVolcano(wrapper, 'https://ark.cn-beijing.volces.com/api/coding')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('volcano B')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('ark-key-b')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalled()
    const last = createAccountMock.mock.calls[createAccountMock.mock.calls.length - 1]?.[0]?.credentials
    expect(last?.access_key).toBeUndefined()
    expect(last?.secret_key).toBeUndefined()
  })

  it('recognizes Volcano subscription and preserves endpoint (api_key 与 AK/SK 输入均展示)', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'DeepSeek')
    await flushPromises()
    // 切到 chat_completions 协议，露出单 base_url 输入框
    await selectButtonByText(wrapper, 'chatCompletions')
    await flushPromises()
    const baseInput = wrapper
      .findAll('input')
      .find((candidate) => candidate.attributes('placeholder') === 'https://api.deepseek.com')
    expect(baseInput).toBeDefined()
    await baseInput?.setValue('https://ark.cn-beijing.volces.com/api/plan')
    await flushPromises()
    // 切回 adaptive：isVolcanoSubscription 为 true 时 watcher 不覆盖 base_url
    await selectButtonByText(wrapper, 'adaptive')
    await flushPromises()
    // adaptive 下把 chat_completions 设为空白串（旧逻辑会误判为非火山）
    await wrapper.get('[data-testid="cn-adaptive-base-url-chat_completions"]').setValue('   ')
    await flushPromises()
    // 火山订阅号识别成功：api_key 输入框展示，AK/SK 录入区同步出现（用量探测签名需要）。
    const pw = wrapper.find('form#create-account-form input[type="password"]')
    expect(pw.exists()).toBe(true)
    expect(wrapper.find('input[placeholder="admin.accounts.cnProviders.accessKeyPlaceholder"]').exists()).toBe(true)
    expect(wrapper.find('input[placeholder="admin.accounts.cnProviders.secretKeyPlaceholder"]').exists()).toBe(true)
  })

  it('keeps Volcano endpoint after switching chat_completions <-> adaptive (no silent fallback)', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'DeepSeek')
    await flushPromises()
    await selectButtonByText(wrapper, 'chatCompletions')
    await flushPromises()
    const baseInput = wrapper
      .findAll('input')
      .find((candidate) => candidate.attributes('placeholder') === 'https://api.deepseek.com')
    await baseInput?.setValue('https://ark.cn-beijing.volces.com/api/plan')
    await flushPromises()
    // 切到 adaptive：火山 base_url 应同步进 chat_completions 槽位
    await selectButtonByText(wrapper, 'adaptive')
    await flushPromises()
    await wrapper.get('form#create-account-form input[type="text"]').setValue('volcano acct')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-deepseek')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    const creds = createAccountMock.mock.calls[0]?.[0]?.credentials
    expect(creds?.api_base_urls?.chat_completions).toBe('https://ark.cn-beijing.volces.com/api/plan')
    expect(creds?.base_url).toBe('https://ark.cn-beijing.volces.com/api/plan')
  })

  it('keeps Volcano base_url in payload when adaptive chat_completions is whitespace', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'DeepSeek')
    await flushPromises()
    await selectButtonByText(wrapper, 'chatCompletions')
    await flushPromises()
    const baseInput = wrapper
      .findAll('input')
      .find((candidate) => candidate.attributes('placeholder') === 'https://api.deepseek.com')
    await baseInput?.setValue('https://ark.cn-beijing.volces.com/api/plan')
    await flushPromises()
    await selectButtonByText(wrapper, 'adaptive')
    await flushPromises()
    // adaptive chat_completions 留空白串：提交应回退到火山 base_url，而非写成空值
    await wrapper.get('[data-testid="cn-adaptive-base-url-chat_completions"]').setValue('   ')
    await flushPromises()
    await wrapper.get('form#create-account-form input[type="text"]').setValue('volcano acct')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-deepseek')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    const creds = createAccountMock.mock.calls[0]?.[0]?.credentials
    expect(creds?.api_base_urls?.chat_completions).toBe('https://ark.cn-beijing.volces.com/api/plan')
    expect(creds?.base_url).toBe('https://ark.cn-beijing.volces.com/api/plan')
  })

  it('keeps Volcano endpoint after switching adaptive -> chat_completions (no silent fallback)', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'DeepSeek')
    await flushPromises()
    // 进入 adaptive 并在 chat_completions 槽位填写火山端点
    await selectButtonByText(wrapper, 'adaptive')
    await flushPromises()
    await wrapper.get('[data-testid="cn-adaptive-base-url-chat_completions"]').setValue('https://ark.cn-beijing.volces.com/api/plan')
    await flushPromises()
    // 切回 chat_completions：应从 adaptive 槽位回填，而非丢失为 DeepSeek 默认值（HIGH 修复）
    await selectButtonByText(wrapper, 'chatCompletions')
    await flushPromises()
    await wrapper.get('form#create-account-form input[type="text"]').setValue('volcano acct')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-deepseek')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    const creds = createAccountMock.mock.calls[0]?.[0]?.credentials
    expect(creds?.api_protocol).toBe('chat_completions')
    expect(creds?.base_url).toBe('https://ark.cn-beijing.volces.com/api/plan')
  })

  it('keeps Volcano endpoint after adaptive -> chat_completions when adaptive chat slot is blank (whitespace falls back to base_url)', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'DeepSeek')
    await flushPromises()
    // 先在 chat_completions 填入火山地址
    await selectButtonByText(wrapper, 'chatCompletions')
    await flushPromises()
    const baseInput = wrapper
      .findAll('input')
      .find((candidate) => candidate.attributes('placeholder') === 'https://api.deepseek.com')
    await baseInput?.setValue('https://ark.cn-beijing.volces.com/api/plan')
    await flushPromises()
    // 切到 adaptive：火山地址同步进 chat 槽位
    await selectButtonByText(wrapper, 'adaptive')
    await flushPromises()
    // 把 adaptive 的 chat_completions 槽位清空为空白串
    await wrapper.get('[data-testid="cn-adaptive-base-url-chat_completions"]').setValue('   ')
    await flushPromises()
    // 切回 chat_completions：空白槽位应回退到火山 base_url，而非写成 DeepSeek 默认（MEDIUM 修复）
    await selectButtonByText(wrapper, 'chatCompletions')
    await flushPromises()
    await wrapper.get('form#create-account-form input[type="text"]').setValue('volcano acct')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-deepseek')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    const creds = createAccountMock.mock.calls[0]?.[0]?.credentials
    expect(creds?.api_protocol).toBe('chat_completions')
    expect(creds?.base_url).toBe('https://ark.cn-beijing.volces.com/api/plan')
  })
})

describe('CreateAccountModal web access mode (kimi / zhipu / deepseek + access_mode=web)', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    createAccountMock.mockReset().mockResolvedValue({ id: 42, platform: 'deepseek', type: 'apikey' })
    probeUpstreamBillingMock.mockReset().mockResolvedValue({})
    syncUpstreamModelsMock.mockReset().mockResolvedValue({ models: [], metadata: {} })
    showWarningMock.mockReset()
    importCodexSessionMock.mockReset().mockResolvedValue({
      created: 1,
      updated: 0,
      skipped: 0,
      failed: 0,
      errors: [],
      warnings: [],
    })
    createOpenAICodexPATMock.mockReset().mockResolvedValue({})
  })

  afterEach(() => vi.useRealTimers())


  it('submits pasted cookie credentials for deepseek web access mode with apikey type', async () => {
    const wrapper = mountModal()
    await selectWebModeViaCnPlatform(wrapper, 'DeepSeek')
    await flushPromises()
    // 风险提示必须可见（方案 §2.2）
    expect(wrapper.find('[data-testid="web-risk-warning"]').exists()).toBe(true)

    await wrapper.get('form#create-account-form input[type="text"]').setValue('web ds account')
    await wrapper.get('[data-testid="web-cookie-input"]').setValue('sessionid=abc; HWWAFSESID=xyz')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    const payload = createAccountMock.mock.calls[0]?.[0]
    expect(payload?.platform).toBe('deepseek')
    expect(payload?.type).toBe('apikey')
    expect(payload?.credentials?.cookie).toBe('sessionid=abc; HWWAFSESID=xyz')
    // 平台归并 PR-3：网页接入下沉为 credentials["access_mode"]="web"。
    expect(payload?.credentials?.access_mode).toBe('web')
    expect(payload?.credentials).not.toHaveProperty('api_key')
  })

  it('submits parsed token JSON for kimi web access mode and keeps only non-empty optional fields', async () => {
    const wrapper = mountModal()
    await selectWebModeViaCnPlatform(wrapper, 'Kimi')
    await flushPromises()

    await wrapper.get('form#create-account-form input[type="text"]').setValue('web kimi account')
    await wrapper.get('[data-testid="web-kimi-token-json"]').setValue(
      JSON.stringify({ access_token: 'at', refresh_token: 'rt', user_id: 'u-9' })
    )
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    const payload = createAccountMock.mock.calls[0]?.[0]
    expect(payload?.platform).toBe('kimi')
    expect(payload?.type).toBe('apikey')
    expect(payload?.credentials).toEqual({
      access_mode: 'web',
      access_token: 'at',
      refresh_token: 'rt',
      user_id: 'u-9',
    })
  })

  it('rejects blank cookie without calling create API', async () => {
    const wrapper = mountModal()
    await selectWebModeViaCnPlatform(wrapper, 'DeepSeek')
    await flushPromises()

    await wrapper.get('form#create-account-form input[type="text"]').setValue('web ds account')
    await wrapper.get('[data-testid="web-cookie-input"]').setValue('   ')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).not.toHaveBeenCalled()
  })

  it('rejects unparseable kimi token JSON without calling create API', async () => {
    const wrapper = mountModal()
    await selectWebModeViaCnPlatform(wrapper, 'Kimi')
    await flushPromises()

    await wrapper.get('form#create-account-form input[type="text"]').setValue('web kimi account')
    await wrapper.get('[data-testid="web-kimi-token-json"]').setValue('not-json')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).not.toHaveBeenCalled()
  })

  it('hides the generic api key block for web platforms', async () => {
    const wrapper = mountModal()
    await selectWebModeViaCnPlatform(wrapper, 'Zhipu GLM')
    await flushPromises()

    expect(wrapper.find('[data-testid="web-cookie-input"]').exists()).toBe(true)
    expect(wrapper.find('form#create-account-form input[type="password"]').exists()).toBe(false)
  })

  it('keeps the CN base card highlighted while web mode selected and resets platform on payg click', async () => {
    const wrapper = mountModal()
    await selectWebModeViaCnPlatform(wrapper, 'DeepSeek')
    await flushPromises()

    // DeepSeek 卡片保持高亮（平台归并后 platform 即基础平台）。
    const deepseekCard = wrapper.findAll('button').find((b) => b.text().includes('DeepSeek'))
    expect(deepseekCard).toBeDefined()
    expect(deepseekCard?.classes().join(' ')).toContain('bg-white')

    // 切回按量付费：platform 复位为基础平台，web 凭证区消失。
    await selectButtonByText(wrapper, 'admin.accounts.cnProviders.accountMode.payg')
    await flushPromises()
    expect(wrapper.find('[data-testid="web-risk-warning"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="cn-web-mode"]').exists()).toBe(true)
  })

  it('does not offer web mode for minimax', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'MiniMax')
    await flushPromises()

    expect(wrapper.find('[data-testid="cn-web-mode"]').exists()).toBe(false)
  })
})

// 上游倍率自动探测的"资格门控 + 错误展示"：web 平台建号不得携带探测开关；
// 后端按契约 fail-closed 返回 400 UPSTREAM_BILLING_PROBE_ACCOUNT_INVALID 时，
// 前端应把拦截器摊平的平面错误 message 透传给 showError，而非套用通用失败文案。
describe('CreateAccountModal upstream billing probe eligibility', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    showErrorMock.mockReset()
    createAccountMock.mockReset().mockResolvedValue({ id: 42, platform: 'zhipu', type: 'apikey' })
    probeUpstreamBillingMock.mockReset().mockResolvedValue({})
    syncUpstreamModelsMock.mockReset().mockResolvedValue({ models: [], metadata: {} })
  })

  // (a) 载荷门控：网页接入模式走网页凭证路径，payload 不得携带 upstream_billing_probe_enabled。
  it('omits upstream_billing_probe_enabled from the create payload for zhipu web access mode credentials', async () => {
    const wrapper = mountModal()
    await selectWebModeViaCnPlatform(wrapper, 'Zhipu GLM')
    await flushPromises()

    await wrapper.get('form#create-account-form input[type="text"]').setValue('web zhipu account')
    await wrapper.get('[data-testid="web-cookie-input"]').setValue('sessionid=test-cookie')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    const payload = createAccountMock.mock.calls[0]?.[0]
    expect(payload?.platform).toBe('web-zhipu')
    expect(payload?.type).toBe('apikey')
    expect(payload?.credentials?.cookie).toBe('sessionid=test-cookie')
    // isUpstreamBillingProbeEligible('web-zhipu','apikey') 为 false → 该字段为 undefined（或省略），
    // 后端契约要求 web 平台建号请求不得携带 upstream_billing_probe_enabled=true。
    expect(payload?.upstream_billing_probe_enabled).toBeUndefined()
    expect(showErrorMock).not.toHaveBeenCalled()
  })

  // (b) 错误展示：create reject 为拦截器摊平的平面对象（无 response 属性），
  // showError 应收到 message 原文，而非通用 failedToCreate 文案。
  it('surfaces the flat interceptor error message instead of the generic failedToCreate copy', async () => {
    createAccountMock.mockRejectedValue({
      status: 400,
      code: 400,
      reason: 'UPSTREAM_BILLING_PROBE_ACCOUNT_INVALID',
      message: 'account is not an API key account',
    })

    const wrapper = mountModal()
    await selectWebModeViaCnPlatform(wrapper, 'Zhipu GLM')
    await flushPromises()

    await wrapper.get('form#create-account-form input[type="text"]').setValue('web zhipu account')
    await wrapper.get('[data-testid="web-cookie-input"]').setValue('sessionid=test-cookie')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    // submitCreateAccount 的 catch 走 error?.message || t('failedToCreate')；
    // 平面错误含 message，应原样透传。useI18n 在 spec 内为 key->key，故通用文案即字面 i18n key。
    expect(showErrorMock).toHaveBeenCalledWith('account is not an API key account')
    expect(showErrorMock).not.toHaveBeenCalledWith('admin.accounts.failedToCreate')
    // 创建失败，不应派发 created 事件。
    expect(wrapper.emitted('created')).toBeUndefined()
  })
})
