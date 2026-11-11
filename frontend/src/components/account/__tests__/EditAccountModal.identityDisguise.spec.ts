import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { mount } from '@vue/test-utils'

const {
  updateAccountMock,
  getProfilesMock,
  getDisguiseMock,
  updateDisguiseMock,
  showErrorMock,
  authIsSimpleMode
} = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  getProfilesMock: vi.fn(),
  getDisguiseMock: vi.fn(),
  updateDisguiseMock: vi.fn(),
  showErrorMock: vi.fn(),
  authIsSimpleMode: { value: true }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: showErrorMock, showSuccess: vi.fn(), showInfo: vi.fn(), showWarning: vi.fn() })
}))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ get isSimpleMode() { return authIsSimpleMode.value } }) }))
vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      update: updateAccountMock,
      getIdentityDisguiseProfiles: getProfilesMock,
      getAccountIdentityDisguise: getDisguiseMock,
      updateAccountIdentityDisguise: updateDisguiseMock,
      getGrokMediaEligibility: vi.fn().mockResolvedValue({ account_id: 12, mode: 'auto', eligible: false, reason: 'x' }),
      updateGrokMediaEligibility: vi.fn().mockResolvedValue({ account_id: 12, mode: 'auto', eligible: false, reason: 'x' }),
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false })
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) }
  }
}))
vi.mock('@/api/admin/accounts', () => ({ getAntigravityDefaultModelMapping: vi.fn() }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import EditAccountModal from '../EditAccountModal.vue'

const BaseDialogStub = defineComponent({
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const account = (platform = 'deepseek', extra: Record<string, unknown> = {}, credentials: Record<string, unknown> = {}) => ({
  id: 12, name: 'Account', notes: '', platform, type: 'apikey',
  credentials: { api_key: 'k', model_mapping: { 'deepseek-flash': 'x' }, ...credentials },
  credentials_status: { has_access_token: true, has_refresh_token: true }, extra,
  proxy_id: null, concurrency: 1, priority: 1, rate_multiplier: 1, status: 'active',
  group_ids: [], expires_at: null, auto_pause_on_expired: false
})

function mountModal(value = account()) {
  return mount(EditAccountModal, {
    props: { show: true, account: value, proxies: [], groups: [] },
    global: { stubs: {
      BaseDialog: BaseDialogStub, Select: true, Icon: true, ProxySelector: true,
      GroupSelector: true, ModelWhitelistSelector: true
    } }
  })
}

const emptyState = {
  account_id: 12,
  profile: 'none' as const,
  forbidden_names: [] as string[],
  rewrite_model: true,
  strip_reasoning: true,
  prompt: '',
  uses_legacy_keys: false
}

describe('EditAccountModal identity disguise', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    updateAccountMock.mockReset()
    getProfilesMock.mockReset()
    getDisguiseMock.mockReset()
    updateDisguiseMock.mockReset()
    showErrorMock.mockReset()
    updateAccountMock.mockResolvedValue(account())
    getProfilesMock.mockResolvedValue([
      { id: 'none', label: 'none', needs_forbidden_names: false },
      { id: 'kimi', label: 'Kimi', needs_forbidden_names: true },
      { id: 'deepseek', label: 'DeepSeek', needs_forbidden_names: true }
    ])
    getDisguiseMock.mockResolvedValue({ ...emptyState })
    updateDisguiseMock.mockImplementation(async (_id: number, payload: Record<string, unknown>) => ({
      ...emptyState,
      profile: payload.profile as 'none',
      forbidden_names: payload.forbidden_names as string[],
      rewrite_model: payload.rewrite_model as boolean,
      strip_reasoning: payload.strip_reasoning as boolean
    }))
  })

  it('loads the profile catalog and the current config on open', async () => {
    const wrapper = mountModal()
    await vi.waitFor(() => expect(getProfilesMock).toHaveBeenCalled())
    // 等 DOM 反映 catalog（mock 被调用 ≠ await 已 resolve → DOM 已更新）
    await vi.waitFor(() =>
      expect(wrapper.findAll('[data-testid="identity-disguise-profile"] option').length).toBe(3)
    )
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalledWith(12))
    expect(wrapper.find('[data-testid="identity-disguise-section"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="identity-disguise-profile"]').element.value).toBe('none')
  })

  it('renders the profile options from the backend catalog (no hardcoded list)', async () => {
    const wrapper = mountModal()
    await vi.waitFor(() =>
      expect(wrapper.findAll('[data-testid="identity-disguise-profile"] option').length).toBe(3)
    )
    const options = wrapper.findAll('[data-testid="identity-disguise-profile"] option')
    // none + kimi + deepseek（来自 mock 的后端目录）
    expect(options.length).toBe(3)
    expect(options.map((o) => o.element.value)).toEqual(['none', 'kimi', 'deepseek'])
  })

  it('hides the disguise switches when profile is none', async () => {
    const wrapper = mountModal()
    await vi.waitFor(() =>
      expect(wrapper.findAll('[data-testid="identity-disguise-profile"] option').length).toBe(3)
    )
    expect(wrapper.find('[data-testid="identity-disguise-strip-reasoning"]').exists()).toBe(false)
    await wrapper.get('[data-testid="identity-disguise-profile"]').setValue('deepseek')
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-testid="identity-disguise-strip-reasoning"]').exists()).toBe(true)
  })

  it('saves the disguise through the dedicated endpoint, not the main update payload', async () => {
    const wrapper = mountModal()
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalled())
    await wrapper.get('[data-testid="identity-disguise-profile"]').setValue('deepseek')
    await wrapper.vm.$nextTick()
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateDisguiseMock).toHaveBeenCalled())
    const [id, payload] = updateDisguiseMock.mock.calls[0]
    expect(id).toBe(12)
    expect(payload.profile).toBe('deepseek')
    expect(payload.rewrite_model).toBe(true)
    expect(payload.strip_reasoning).toBe(true)
    // 伪装配置不得混进主账号更新载荷（主更新的 extra 是整体替换语义）
    const mainPayload = updateAccountMock.mock.calls[0][1] as Record<string, unknown>
    expect(JSON.stringify(mainPayload)).not.toContain('identity_disguise')
  })

  it('warns when the account uses the Anthropic native protocol', async () => {
    const wrapper = mountModal(account('deepseek', {}, { api_protocol: 'anthropic' }))
    await vi.waitFor(() =>
      expect(wrapper.find('[data-testid="identity-disguise-protocol-warning"]').exists()).toBe(true)
    )
  })

  it('notices accounts still on the legacy config format', async () => {
    getDisguiseMock.mockResolvedValue({ ...emptyState, uses_legacy_keys: true })
    const wrapper = mountModal()
    await vi.waitFor(() =>
      expect(wrapper.find('[data-testid="identity-disguise-legacy-notice"]').exists()).toBe(true)
    )
  })

  it('reports a partial-save error when the dedicated endpoint fails', async () => {
    updateDisguiseMock.mockRejectedValueOnce(new Error('disguise failed'))
    const wrapper = mountModal()
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalled())
    await wrapper.get('[data-testid="identity-disguise-profile"]').setValue('kimi')
    await wrapper.vm.$nextTick()
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(showErrorMock).toHaveBeenCalledWith('admin.accounts.identityDisguise.saveFailed'))
  })
})
