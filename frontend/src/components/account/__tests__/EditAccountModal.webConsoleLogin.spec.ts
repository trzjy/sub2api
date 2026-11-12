import { describe, expect, it, vi, beforeEach } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'

const { startLoginMock } = vi.hoisted(() => ({
  startLoginMock: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
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
      update: vi.fn().mockResolvedValue({}),
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false })
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([])
    },
    cnProviders: {
      startWebConsoleLogin: startLoginMock,
      getWebConsoleLoginFrame: vi.fn(),
      postWebConsoleLoginAction: vi.fn(),
      finishWebConsoleLogin: vi.fn(),
      cancelWebConsoleLogin: vi.fn().mockResolvedValue(undefined)
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

// 这是一个 Web 控制台额度上游账号（longcat）：登录区必须渲染，且登录方式来自后端投影。
function buildConsoleAccount() {
  return {
    id: 442,
    name: 'LongCat-8877',
    notes: '',
    platform: 'deepseek',
    type: 'apikey',
    credentials: { base_url: 'https://api.longcat.chat/openai', api_protocol: 'chat_completions' },
    credentials_status: { has_api_key: true },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false,
    web_console_quota_provider: 'longcat',
    web_console_login_modes: ['qrcode', 'sms']
  } as any
}

function mountModal(account: ReturnType<typeof buildConsoleAccount>) {
  return mount(EditAccountModal, {
    props: { show: true, account, proxies: [], groups: [] },
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

describe('EditAccountModal 控制台登录面板', () => {
  beforeEach(() => {
    startLoginMock.mockReset()
  })

  it('点「登录控制台」后立刻展开面板，不等打开登录页的请求返回', async () => {
    // 真实场景：打开登录页要 10~30 秒。请求没回来之前面板就必须已经在了，
    // 否则用户看到的是「登录功能没了」（2026-11-13 线上故障）。
    startLoginMock.mockReturnValue(new Promise(() => {}))
    const wrapper = mountModal(buildConsoleAccount())

    expect(wrapper.find('[data-testid="web-console-quota-section"]').exists()).toBe(true)
    const btn = wrapper.get('[data-testid="web-console-login-start"]')
    await btn.trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="web-console-login-panel"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.webConsoleQuota.frameLoading')
    // 页面还没打开时不要显示「表单为空」，那会让人以为页面没有输入框
    expect(wrapper.find('[data-testid="web-console-login-form"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('会话建立后按 phase 渲染：ready 才给表单，failed 给可读的错误', async () => {
    startLoginMock.mockResolvedValue({
      session_id: 'sid-1',
      status: 'pending',
      phase: 'ready',
      viewport_width: 1280,
      viewport_height: 800,
      expires_in: 600,
      screenshot: 'AAAA',
      fields: [{ index: 0, type: 'tel', label: '手机号', filled: false, masked: false }],
      buttons: []
    })
    const wrapper = mountModal(buildConsoleAccount())
    await wrapper.get('[data-testid="web-console-login-start"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="web-console-login-panel"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="web-console-login-form"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('手机号')
    wrapper.unmount()
  })
})
