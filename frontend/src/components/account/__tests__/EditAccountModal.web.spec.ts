import { describe, expect, it, vi, beforeEach } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'

const { updateAccountMock, showErrorMock } = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  showErrorMock: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock,
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get isSimpleMode() {
      return true
    }
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      update: updateAccountMock,
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false })
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([])
    }
  }
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

import EditAccountModal from '../EditAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: { type: [String, Number, Boolean, null], default: '' },
    options: { type: Array, default: () => [] }
  },
  emits: ['update:modelValue'],
  template: `
    <select v-bind="$attrs" :value="modelValue" @change="$emit('update:modelValue', $event.target.value)">
      <option v-for="option in options" :key="option.value" :value="option.value">{{ option.label }}</option>
    </select>
  `
})

const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: { modelValue: { type: Array, default: () => [] } },
  emits: ['update:modelValue'],
  template: '<div />'
})

function buildWebAccount(platform: string, credentials: Record<string, unknown>) {
  return {
    id: 10,
    name: `Web ${platform}`,
    notes: '',
    platform,
    type: 'apikey',
    credentials,
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function buildApiAccount() {
  return {
    ...buildWebAccount('zhipu', {}),
    id: 20,
    name: 'API Zhipu',
    credentials: {
      base_url: 'https://open.bigmodel.cn/api/paas/v4',
      account_mode: 'payg',
      api_protocol: 'chat_completions'
    }
  } as any
}

function mountModal(account: ReturnType<typeof buildWebAccount>) {
  return mount(EditAccountModal, {
    props: {
      show: true,
      account,
      proxies: [],
      groups: []
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: SelectStub,
        Icon: true,
        ProxySelector: true,
        GroupSelector: true,
        ModelWhitelistSelector: ModelWhitelistSelectorStub
      }
    }
  })
}

async function submit(wrapper: ReturnType<typeof mountModal>) {
  await wrapper.get('form#edit-account-form').trigger('submit.prevent')
  await flushPromises()
}

describe('EditAccountModal web access mode', () => {
  beforeEach(() => {
    updateAccountMock.mockReset()
    updateAccountMock.mockImplementation(async () => ({}))
    showErrorMock.mockReset()
  })

  it('renders no API Key field and keeps credentials for a web zhipu account', async () => {
    const account = buildWebAccount('zhipu', {
      access_mode: 'web',
      cookie: 'session=abc; user=someone'
    })
    const wrapper = mountModal(account)

    // Web 模式隐藏 API 专属字段；手工 Cookie 输入旧链已归零（E5），不再渲染
    expect(wrapper.find('input[type="password"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="web-edit-cookie-input"]').exists()).toBe(false)

    await submit(wrapper)

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted.access_mode).toBe('web')
    expect(submitted.cookie).toBe('session=abc; user=someone')
    expect(submitted.api_key).toBeUndefined()
    wrapper.unmount()
  })

  it('renders no API Key field and keeps credentials for a web deepseek account', async () => {
    const account = buildWebAccount('deepseek', {
      access_mode: 'web',
      cookie: 'sessionid=xyz'
    })
    const wrapper = mountModal(account)

    // Web 模式隐藏 API 专属字段；手工 Cookie 输入旧链已归零（E5）。
    // deepseek web 账号仍额外展示自动续期邮箱/密码输入（续期专用，不替代 API Key 语义）。
    expect(wrapper.find('[data-testid="web-edit-cookie-input"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="web-edit-login-email-input"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="web-edit-login-password-input"]').exists()).toBe(true)

    await submit(wrapper)

    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted.access_mode).toBe('web')
    expect(submitted.cookie).toBe('sessionid=xyz')
    expect(submitted.api_key).toBeUndefined()
    // 未填写续期邮箱/密码：login_* 键不得出现（后端 merge 保留原值）
    expect(submitted.login_email).toBeUndefined()
    expect(submitted.login_password).toBeUndefined()
    wrapper.unmount()
  })

  it('renders no API Key field and keeps credentials for a web kimi account', async () => {
    const account = buildWebAccount('kimi', {
      access_mode: 'web',
      access_token: 'token-a',
      refresh_token: 'refresh-b'
    })
    const wrapper = mountModal(account)

    expect(wrapper.find('input[type="password"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="web-edit-cookie-input"]').exists()).toBe(false)
    // 手工 Token JSON 输入旧链已归零（E5），不再渲染
    expect(wrapper.find('[data-testid="web-edit-kimi-token-input"]').exists()).toBe(false)

    await submit(wrapper)

    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted.access_mode).toBe('web')
    expect(submitted.access_token).toBe('token-a')
    expect(submitted.refresh_token).toBe('refresh-b')
    expect(submitted.api_key).toBeUndefined()
    wrapper.unmount()
  })

  it('still requires an API key for an api-mode account', async () => {
    const wrapper = mountModal(buildApiAccount())

    // API 模式仍显示 API Key 输入
    expect(wrapper.find('input[type="password"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="web-edit-cookie-input"]').exists()).toBe(false)

    await submit(wrapper)

    expect(updateAccountMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalledWith('admin.accounts.apiKeyIsRequired')
    wrapper.unmount()
  })

  it('keeps access_mode=web after editing a web account', async () => {
    const account = buildWebAccount('zhipu', {
      access_mode: 'web',
      cookie: 'old=1'
    })
    const wrapper = mountModal(account)

    await submit(wrapper)

    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload.credentials.access_mode).toBe('web')
    // name 等表单字段正常提交
    expect(payload.name).toBe('Web zhipu')
    wrapper.unmount()
  })

  it('keeps the stored cookie when editing a web account (no manual cookie input)', async () => {
    const account = buildWebAccount('zhipu', {
      access_mode: 'web',
      cookie: 'old=cookie'
    })
    // 手工 Cookie 输入旧链已归零（E5）：编辑面板不再承担"手工填凭证"职责，
    // 原凭证始终由后端 merge 保留，无手动入口。
    const wrapper = mountModal(account)

    // 提交：保留原 Cookie 凭证，access_mode 强制 web
    await submit(wrapper)
    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted.cookie).toBe('old=cookie')
    expect(submitted.access_mode).toBe('web')
    wrapper.unmount()
  })

  it('keeps the stored kimi access_token when editing (no manual token input)', async () => {
    const account = buildWebAccount('kimi', {
      access_mode: 'web',
      access_token: 'token-old'
    })
    // 手工 Token JSON 输入旧链已归零（E5）：编辑面板不再承担"手工填凭证"职责，
    // 登录态凭证由登录流程重新取得后写入，原值由后端 merge 保留。
    const wrapper = mountModal(account)

    // 提交：保留原 access_token，access_mode 强制 web
    await submit(wrapper)
    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted.access_token).toBe('token-old')
    expect(submitted.access_mode).toBe('web')
    wrapper.unmount()
  })

  it('deepseek: echoes stored login_email and saves renewal email/password when provided', async () => {
    const account = buildWebAccount('deepseek', {
      access_mode: 'web',
      cookie: 'sessionid=xyz',
      login_email: 'u@deepseek.com',
      login_password: 'old-secret'
    })
    const wrapper = mountModal(account)

    // 明文回显已存续期邮箱；续期密码敏感，一律留空不回显
    const emailInput = wrapper.get<HTMLInputElement>('[data-testid="web-edit-login-email-input"]')
    const passwordInput = wrapper.get<HTMLInputElement>('[data-testid="web-edit-login-password-input"]')
    expect(emailInput.element.value).toBe('u@deepseek.com')
    expect(passwordInput.element.value).toBe('')

    // 填写新密码提交：login_password 上送新值，login_email 保留
    updateAccountMock.mockClear()
    await passwordInput.setValue('new-secret')
    await submit(wrapper)
    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted.login_email).toBe('u@deepseek.com')
    expect(submitted.login_password).toBe('new-secret')
    wrapper.unmount()
  })

  it('deepseek: leaves login_password absent when the renewal password field is empty', async () => {
    const account = buildWebAccount('deepseek', {
      access_mode: 'web',
      cookie: 'sessionid=xyz',
      login_email: 'u@deepseek.com',
      login_password: 'old-secret'
    })
    const wrapper = mountModal(account)

    // 密码留空提交：不带 login_password 键（后端 merge 保留原值，防误清空）
    await submit(wrapper)
    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted.login_email).toBe('u@deepseek.com')
    expect(submitted.login_password).toBeUndefined()
    wrapper.unmount()
  })
})
