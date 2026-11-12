import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { CreateAccountRequest } from '@/types'

const {
  createMock,
  showErrorMock,
  showWarningMock,
  showSuccessMock,
  getIdentityDisguiseProfilesMock,
  updateIdentityDisguiseMock,
} = vi.hoisted(() => ({
  createMock: vi.fn(),
  showErrorMock: vi.fn(),
  showWarningMock: vi.fn(),
  showSuccessMock: vi.fn(),
  getIdentityDisguiseProfilesMock: vi.fn(),
  updateIdentityDisguiseMock: vi.fn(),
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock,
    showSuccess: showSuccessMock,
    showWarning: showWarningMock,
  }),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      create: createMock,
      getIdentityDisguiseProfiles: getIdentityDisguiseProfilesMock,
      updateAccountIdentityDisguise: updateIdentityDisguiseMock,
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

import CreateAccountModal from './CreateAccountModal.vue'

const BaseDialogStub = {
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
}

function mountModal() {
  return mount(CreateAccountModal, {
    props: { show: true, proxies: [], groups: [] },
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
      },
    },
  })
}

const PROFILE_OPTIONS = [
  { id: 'none', label: '不伪装' },
  { id: 'kimi', label: 'Kimi' },
  { id: 'deepseek', label: 'DeepSeek' },
]

function buildPayload(): CreateAccountRequest {
  return {
    name: 'acct',
    platform: 'anthropic',
    type: 'apikey',
    credentials: {},
    upstream_billing_probe_enabled: false,
  }
}

beforeEach(() => {
  createMock.mockResolvedValue({ id: 100 })
  getIdentityDisguiseProfilesMock.mockResolvedValue(PROFILE_OPTIONS)
  updateIdentityDisguiseMock.mockResolvedValue(undefined)
})

afterEach(() => {
  vi.clearAllMocks()
})

describe('CreateAccountModal 身份伪装状态重置（DSG-F2）', () => {
  // Done when 1：关闭后重开弹窗伪装状态已清空
  it('关闭弹窗再打开，伪装控件回到初值（profile=none、禁词文本空）', async () => {
    const wrapper = mountModal()
    await flushPromises()

    // 为账号 A 设置伪装配置
    wrapper.vm.identityDisguiseProfile = 'deepseek'
    wrapper.vm.identityDisguiseForbiddenNamesText = 'Alice, Bob'
    wrapper.vm.identityDisguiseAutoDerive = false
    wrapper.vm.identityDisguiseRewriteModel = false
    wrapper.vm.identityDisguiseStripReasoning = false
    await flushPromises()

    // 关闭弹窗（触发 resetForm）
    await wrapper.setProps({ show: false })
    await flushPromises()
    // 再次打开
    await wrapper.setProps({ show: true })
    await flushPromises()

    // 状态回到初值
    expect(wrapper.vm.identityDisguiseProfile).toBe('none')
    expect(wrapper.vm.identityDisguiseForbiddenNamesText).toBe('')
    expect(wrapper.vm.identityDisguiseAutoDerive).toBe(true)
    expect(wrapper.vm.identityDisguiseRewriteModel).toBe(true)
    expect(wrapper.vm.identityDisguiseStripReasoning).toBe(true)

    // DOM 控件也随之复位
    const profileSelect = wrapper.find('[data-testid="identity-disguise-profile"]')
    expect(profileSelect.exists()).toBe(true)
    expect((profileSelect.element as HTMLSelectElement).value).toBe('none')
  })

  // Done when 2：创建提交字段不受上一次影响（回归）
  it('重开后未改动伪装控件，再次创建不沿用上一次配置（profile 复位为 none）', async () => {
    const wrapper = mountModal()
    await flushPromises()

    // 账号 A：选择 deepseek 并填禁词
    wrapper.vm.identityDisguiseProfile = 'deepseek'
    wrapper.vm.identityDisguiseForbiddenNamesText = 'Alice'
    await wrapper.vm.submitCreateAccount(buildPayload())
    await flushPromises()

    expect(updateIdentityDisguiseMock).toHaveBeenCalledTimes(1)
    expect(updateIdentityDisguiseMock).toHaveBeenLastCalledWith(100, {
      profile: 'deepseek',
      forbidden_names: ['Alice'],
      auto_derive_forbidden_names: true,
      rewrite_model: true,
      strip_reasoning: true,
    })

    // 关闭弹窗（触发 resetForm）
    await wrapper.setProps({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true })
    await flushPromises()

    // 账号 B：不改动任何伪装控件，直接再次创建
    await wrapper.vm.submitCreateAccount(buildPayload())
    await flushPromises()

    // profile 已复位为 none —— 不应再次写出伪装，也不沿用 deepseek/Alice
    expect(updateIdentityDisguiseMock).toHaveBeenCalledTimes(1)
  })

  it('重开后改设新伪装值，保存使用当前 UI 值而非上一次', async () => {
    const wrapper = mountModal()
    await flushPromises()

    // 账号 A：deepseek / Alice
    wrapper.vm.identityDisguiseProfile = 'deepseek'
    wrapper.vm.identityDisguiseForbiddenNamesText = 'Alice'
    await wrapper.vm.submitCreateAccount(buildPayload())
    await flushPromises()

    // 关闭并重置
    await wrapper.setProps({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true })
    await flushPromises()

    // 账号 B：改设 kimi / Carol
    wrapper.vm.identityDisguiseProfile = 'kimi'
    wrapper.vm.identityDisguiseForbiddenNamesText = 'Carol'
    await wrapper.vm.submitCreateAccount(buildPayload())
    await flushPromises()

    expect(updateIdentityDisguiseMock).toHaveBeenCalledTimes(2)
    expect(updateIdentityDisguiseMock).toHaveBeenLastCalledWith(100, {
      profile: 'kimi',
      forbidden_names: ['Carol'],
      auto_derive_forbidden_names: true,
      rewrite_model: true,
      strip_reasoning: true,
    })
    // 关键回归断言：第二次保存未沿用第一次的 deepseek/Alice
    const lastArgs = updateIdentityDisguiseMock.mock.calls[1][1]
    expect(lastArgs.profile).toBe('kimi')
    expect(lastArgs.forbidden_names).toEqual(['Carol'])
  })
})
