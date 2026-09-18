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

    // Web 模式隐藏 API 专属字段
    expect(wrapper.find('input[type="password"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="web-edit-cookie-input"]').exists()).toBe(true)

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

    expect(wrapper.find('input[type="password"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="web-edit-cookie-input"]').exists()).toBe(true)

    await submit(wrapper)

    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted.access_mode).toBe('web')
    expect(submitted.cookie).toBe('sessionid=xyz')
    expect(submitted.api_key).toBeUndefined()
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
    expect(wrapper.find('[data-testid="web-edit-kimi-token-input"]').exists()).toBe(true)

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

  it('keeps the stored cookie when the edit input is left empty and saves a new one when provided', async () => {
    const account = buildWebAccount('zhipu', {
      access_mode: 'web',
      cookie: 'old=cookie'
    })
    // 不回显敏感值：Cookie 输入初始为空
    const wrapper = mountModal(account)
    const cookieInput = wrapper.get<HTMLTextAreaElement>('[data-testid="web-edit-cookie-input"]')
    expect(cookieInput.element.value).toBe('')

    // 留空提交：保留原凭证
    await submit(wrapper)
    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials.cookie).toBe('old=cookie')

    // 填写新 Cookie 提交：正确保存
    updateAccountMock.mockClear()
    await cookieInput.setValue('new=cookie')
    await submit(wrapper)
    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted.cookie).toBe('new=cookie')
    expect(submitted.access_mode).toBe('web')
    wrapper.unmount()
  })

  it('saves a new kimi access_token and rejects an invalid token json', async () => {
    const account = buildWebAccount('kimi', {
      access_mode: 'web',
      access_token: 'token-old'
    })
    const wrapper = mountModal(account)
    const tokenInput = wrapper.get<HTMLTextAreaElement>('[data-testid="web-edit-kimi-token-input"]')
    expect(tokenInput.element.value).toBe('')

    // 无效 JSON：报错且不提交
    await tokenInput.setValue('not-json')
    await submit(wrapper)
    expect(updateAccountMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalledWith('admin.accounts.webProviders.errors.webKimiJsonInvalid')

    // 有效 JSON：保存新 access_token，保留 access_mode
    updateAccountMock.mockClear()
    await tokenInput.setValue('{"access_token":"token-new"}')
    await submit(wrapper)
    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted.access_token).toBe('token-new')
    expect(submitted.access_mode).toBe('web')
    wrapper.unmount()
  })
})
