import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  BUILTIN_PLATFORM_CATALOG,
  resetPlatformCatalog,
  setPlatformCatalog,
} from '@/constants/platformCatalog'
import type { Group, ModelAllowlist } from '@/types'

const {
  createAccountMock,
  showErrorMock,
  showWarningMock,
  showSuccessMock,
  authIsSimpleMode,
} = vi.hoisted(() => ({
  createAccountMock: vi.fn(),
  showErrorMock: vi.fn(),
  showWarningMock: vi.fn(),
  showSuccessMock: vi.fn(),
  // 复合分组要在分组选择器中可见，必须关闭 simple mode（simple mode 下 composite 被整体隐藏）。
  authIsSimpleMode: { value: false },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock,
    showSuccess: showSuccessMock,
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
      probeUpstreamBilling: vi.fn(),
      syncUpstreamModels: vi.fn(),
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
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

function mountModal(groups: Group[] = []) {
  return mount(CreateAccountModal, {
    props: { show: true, proxies: [], groups },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        OAuthAuthorizationFlow: true,
        ConfirmDialog: true,
        Select: true,
        Icon: true,
        PlatformIcon: true,
        ProxySelector: true,
        ProxyAdBanner: true,
        ModelWhitelistSelector: true,
        QuotaLimitCard: true,
        // 注意：GroupSelector 不 stub，使用真实组件以验证实际渲染出的可见分组。
      },
    },
  })
}

// 一组覆盖 cursor 原生平台与若干 composite 聚合组的固定夹具。
function buildGroups(): Group[] {
  const allowlist = (enabled: boolean, models: string[]): ModelAllowlist => ({ enabled, models })
  return [
    { id: 1, name: 'Native Anthropic', platform: 'anthropic' },
    { id: 2, name: 'Native OpenAI', platform: 'openai' },
    { id: 3, name: 'Native Gemini', platform: 'gemini' },
    { id: 4, name: 'Native Grok', platform: 'grok' },
    { id: 5, name: 'Native Muse', platform: 'muse' },
    { id: 6, name: 'Composite All', platform: 'composite', model_allowlist: allowlist(false, []) },
    { id: 7, name: 'Composite GPT', platform: 'composite', model_allowlist: allowlist(true, ['gpt-6.1-sol']) },
    { id: 8, name: 'Composite Claude', platform: 'composite', model_allowlist: allowlist(true, ['claude-fable-5-1']) },
    { id: 9, name: 'Composite Gemini', platform: 'composite', model_allowlist: allowlist(true, ['gemini-4-argon']) },
  ]
}

async function selectCursorWithModels(wrapper: ReturnType<typeof mountModal>, models: string[]) {
  // 设为 cursor 平台（触发 watcher 重置并按平台回填模型），再覆盖白名单模型。
  wrapper.vm.form.platform = 'cursor'
  await flushPromises()
  wrapper.vm.allowedModels = [...models]
  await flushPromises()
}

function visibleText(wrapper: ReturnType<typeof mountModal>): string {
  // 仅取分组选择器区域：找到 data-tour 容器下的文本。
  const el = wrapper.find('[data-tour="account-form-groups"]')
  if (el.exists()) return el.text()
  return wrapper.text()
}

describe('CreateAccountModal cursor 模型家族分组过滤', () => {
  beforeEach(() => {
    setPlatformCatalog(BUILTIN_PLATFORM_CATALOG)
    authIsSimpleMode.value = false
  })

  afterEach(() => {
    resetPlatformCatalog()
    vi.clearAllMocks()
  })

  it('只选 gpt-6.1-sol：仅显示 openai 原生组与匹配 composite 组', async () => {
    const wrapper = mountModal(buildGroups())
    await flushPromises()

    await selectCursorWithModels(wrapper, ['gpt-6.1-sol'])

    // 计算属性自检
    expect(wrapper.vm.cursorSelectedModels).toEqual(['gpt-6.1-sol'])
    expect(wrapper.vm.cursorNativePlatforms).toEqual(['openai'])

    const text = visibleText(wrapper)
    expect(text).toContain('Native OpenAI')
    expect(text).toContain('Composite GPT')
    expect(text).not.toContain('Native Anthropic')
    expect(text).not.toContain('Native Gemini')
    expect(text).not.toContain('Native Grok')
    expect(text).not.toContain('Native Muse')
    expect(text).not.toContain('Composite All')
    expect(text).not.toContain('Composite Claude')
    expect(text).not.toContain('Composite Gemini')
  })

  it('改选 claude-fable-5-1 + gemini-4-argon：显示 anthropic/gemini 组，不显示 openai', async () => {
    const wrapper = mountModal(buildGroups())
    await flushPromises()

    await selectCursorWithModels(wrapper, ['claude-fable-5-1', 'gemini-4-argon'])

    expect(wrapper.vm.cursorSelectedModels).toEqual(['claude-fable-5-1', 'gemini-4-argon'])
    expect(wrapper.vm.cursorNativePlatforms).toEqual(['anthropic', 'gemini'])

    const text = visibleText(wrapper)
    expect(text).toContain('Native Anthropic')
    expect(text).toContain('Native Gemini')
    expect(text).toContain('Composite Claude')
    expect(text).toContain('Composite Gemini')
    expect(text).not.toContain('Native OpenAI')
    expect(text).not.toContain('Native Grok')
    expect(text).not.toContain('Native Muse')
    expect(text).not.toContain('Composite All')
    expect(text).not.toContain('Composite GPT')
  })

  it('未选任何模型：回落全套 cursor 原生组，且传 undefined 使 composite 全显示', async () => {
    const wrapper = mountModal(buildGroups())
    await flushPromises()

    await selectCursorWithModels(wrapper, [])

    expect(wrapper.vm.cursorSelectedModels).toEqual([])
    // 未选模型 → 回落全套 cursor 原生平台
    expect(wrapper.vm.cursorNativePlatforms).toEqual(['anthropic', 'openai', 'gemini', 'grok', 'muse'])

    const text = visibleText(wrapper)
    expect(text).toContain('Native Anthropic')
    expect(text).toContain('Native OpenAI')
    expect(text).toContain('Native Gemini')
    expect(text).toContain('Native Grok')
    expect(text).toContain('Native Muse')
    // 未声明覆盖（accountModels=undefined）→ 回落现有行为，composite 全部显示
    expect(text).toContain('Composite All')
    expect(text).toContain('Composite GPT')
    expect(text).toContain('Composite Claude')
    expect(text).toContain('Composite Gemini')
  })
})
