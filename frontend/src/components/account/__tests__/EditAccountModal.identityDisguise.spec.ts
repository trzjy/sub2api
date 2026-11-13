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

const account = (platform = 'deepseek', extra: Record<string, unknown> = {}, credentials: Record<string, unknown> = {}, id = 12) => ({
  id, name: 'Account', notes: '', platform, type: 'apikey',
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
  auto_derive_forbidden_names: true,
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
      auto_derive_forbidden_names: payload.auto_derive_forbidden_names as boolean,
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

  // DSG-F1：读取失败的伪装配置不得被保存覆盖。
  it('does NOT persist disguise when the disguise read failed (DSG-F1)', async () => {
    // 读取伪装接口 reject（模拟网络/后端异常），UI 仅持有初值 none。
    getDisguiseMock.mockRejectedValueOnce(new Error('disguise read failed'))
    const wrapper = mountModal()
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalled())
    // 改个备注（其它字段）后保存
    await wrapper.get('[data-testid="account-notes"]').setValue('changed note')
    await wrapper.vm.$nextTick()
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    // 主更新仍正常发生
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalled())
    // 关键断言：专用伪装接口绝不被调用，不会把 none 写回后端清空已有伪装。
    expect(updateDisguiseMock).not.toHaveBeenCalled()
  })

  it('still persists disguise exactly once on a successful read (regression)', async () => {
    // 读取成功（默认 mock 即 resolve emptyState），保存后应按原逻辑调用一次专用接口。
    const wrapper = mountModal()
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalled())
    await wrapper.get('[data-testid="identity-disguise-profile"]').setValue('deepseek')
    await wrapper.vm.$nextTick()
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateDisguiseMock).toHaveBeenCalledTimes(1))
    const [id, payload] = updateDisguiseMock.mock.calls[0]
    expect(id).toBe(12)
    expect(payload.profile).toBe('deepseek')
    // 不得混进主账号更新载荷
    const mainPayload = updateAccountMock.mock.calls[0][1] as Record<string, unknown>
    expect(JSON.stringify(mainPayload)).not.toContain('identity_disguise')
  })

  it('recovers: a later successful reload allows the disguise to be saved (DSG-F1 state recovery)', async () => {
    // 第一次读取失败 → loaded 保持 false，保存不写伪装
    getDisguiseMock.mockRejectedValueOnce(new Error('disguise read failed'))
    const wrapper = mountModal()
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalled())
    await wrapper.get('[data-testid="account-notes"]').setValue('note before recovery')
    await wrapper.vm.$nextTick()
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalled())
    expect(updateDisguiseMock).not.toHaveBeenCalled()
    updateAccountMock.mockClear()

    // 读取恢复成功：改回 resolve，并切换账号 id 触发 watch 重新加载
    getDisguiseMock.mockResolvedValue({ ...emptyState, profile: 'kimi' })
    await wrapper.setProps({ account: account('deepseek', {}, {}, 99) })
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalledWith(99))
    await wrapper.setProps({ account: account('deepseek', {}, {}, 12) })
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalledWith(12))

    // 此时 loaded 已恢复为 true，再次保存应正常写入伪装
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateDisguiseMock).toHaveBeenCalledTimes(1))
    const [id, payload] = updateDisguiseMock.mock.calls[0]
    expect(id).toBe(12)
    expect(payload.profile).toBe('kimi')
  })

  // DSG-C4F：首次读取失败后，关闭再打开同一账号应重试读取，成功后保存恢复可用。
  it('retries the disguise read on reopen after a failed read, then allows save (DSG-C4F)', async () => {
    // 首次读取失败（仅第一次 reject，之后回落到默认 resolve）
    getDisguiseMock.mockRejectedValueOnce(new Error('disguise read failed'))
    const wrapper = mountModal()
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalledTimes(1))

    // 关闭再打开同一账号（id 不变，不会触发 account.id watch）
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })

    // 重新打开后应再次发起读取请求
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalledTimes(2))

    // 第二次读取恢复成功 → loaded 置 true，保存恢复可用
    await wrapper.get('[data-testid="identity-disguise-profile"]').setValue('deepseek')
    await wrapper.vm.$nextTick()
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateDisguiseMock).toHaveBeenCalledTimes(1))
    const [id, payload] = updateDisguiseMock.mock.calls[0]
    expect(id).toBe(12)
    expect(payload.profile).toBe('deepseek')
  })

  // DSG-C4F（回归）：读取成功后关闭再打开同一账号，不应重复发起读取请求。
  it('does not re-read the disguise when reopened after a successful read (regression DSG-C4F)', async () => {
    const wrapper = mountModal()
    // 首次读取成功，且 UI 已反映加载结果（确保 loaded = true）
    await vi.waitFor(() =>
      expect(wrapper.get('[data-testid="identity-disguise-profile"]').element.value).toBe('none')
    )
    expect(getDisguiseMock).toHaveBeenCalledTimes(1)

    // 关闭再打开同一账号
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })

    // 已加载成功时不再发起读取请求
    expect(getDisguiseMock).toHaveBeenCalledTimes(1)
  })

  // DSG-C4F（回归）：首次读取失败且重开仍失败，保存仍被守卫拦住。
  it('still blocks the disguise save when a reopened read keeps failing (regression DSG-C4F)', async () => {
    // 所有读取都失败
    getDisguiseMock.mockRejectedValue(new Error('disguise read failed'))
    const wrapper = mountModal()
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalledTimes(1))

    // 关闭再打开同一账号 → 会再次发起读取（仍失败）
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalledTimes(2))

    // 改个备注后保存：主更新照常，但伪装写回仍被守卫拦住
    await wrapper.get('[data-testid="account-notes"]').setValue('changed note')
    await wrapper.vm.$nextTick()
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalled())
    expect(updateDisguiseMock).not.toHaveBeenCalled()
    // 读取失败提示仍然出现（既有行为不变）
    expect(showErrorMock).toHaveBeenCalledWith('admin.accounts.identityDisguise.loadFailed')
  })

  // 回归：后端历史上返回过 forbidden_names: null，前端 join 会抛错并把整个读取
  // 判为失败 → 伪装既不显示也不保存。必须容忍 null 并照常加载。
  it('tolerates a null forbidden_names from the backend', async () => {
    getDisguiseMock.mockResolvedValue({ ...emptyState, forbidden_names: null } as never)
    const wrapper = mountModal()
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalled())
    // 读取没有被判为失败
    expect(showErrorMock).not.toHaveBeenCalledWith('admin.accounts.identityDisguise.loadFailed')
    // 且伪装配置仍可保存（读取已标记为成功）
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateDisguiseMock).toHaveBeenCalled())
  })

  // 「自动生成」开关由后端随配置落库并回显 —— 不再从「列表是否为空」反推。
  it('restores the auto-derive switch from the backend flag, not from the list length', async () => {
    // 自动推导出的禁词列表非空，但开关是「自动生成」
    getDisguiseMock.mockResolvedValue({
      ...emptyState,
      profile: 'kimi',
      forbidden_names: ['kimi-k2'],
      auto_derive_forbidden_names: true
    })
    const wrapper = mountModal()
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalled())
    await vi.waitFor(() =>
      expect(
        (wrapper.get('[data-testid="identity-disguise-auto-derive"]').element as HTMLInputElement).checked
      ).toBe(true)
    )
  })

  it('restores the auto-derive switch as unchecked for a manually filled list', async () => {
    getDisguiseMock.mockResolvedValue({
      ...emptyState,
      profile: 'kimi',
      forbidden_names: ['kimi-k2'],
      auto_derive_forbidden_names: false
    })
    const wrapper = mountModal()
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalled())
    await vi.waitFor(() =>
      expect(
        (wrapper.get('[data-testid="identity-disguise-auto-derive"]').element as HTMLInputElement).checked
      ).toBe(false)
    )
  })

  it('sends the auto-derive flag to the dedicated endpoint', async () => {
    getDisguiseMock.mockResolvedValue({
      ...emptyState,
      profile: 'kimi',
      forbidden_names: ['kimi-k2'],
      auto_derive_forbidden_names: false
    })
    const wrapper = mountModal()
    await vi.waitFor(() => expect(getDisguiseMock).toHaveBeenCalled())
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateDisguiseMock).toHaveBeenCalled())
    const [, payload] = updateDisguiseMock.mock.calls[0]
    expect(payload.auto_derive_forbidden_names).toBe(false)
    expect(payload.forbidden_names).toEqual(['kimi-k2'])
  })

  // 「不伪装」选项的显示名走 i18n，而不是后端目录里的内部标识 "none"。
  it('labels the none option with the localized label', async () => {
    const wrapper = mountModal()
    await vi.waitFor(() =>
      expect(wrapper.findAll('[data-testid="identity-disguise-profile"] option').length).toBe(3)
    )
    const options = wrapper.findAll('[data-testid="identity-disguise-profile"] option')
    expect(options[0].text()).toBe('admin.accounts.identityDisguise.profileNone')
    // 其余模板名仍用后端目录的 label
    expect(options[1].text()).toBe('Kimi')
    expect(options[2].text()).toBe('DeepSeek')
  })
})
