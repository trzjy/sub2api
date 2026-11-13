import { describe, expect, it, vi, beforeEach } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'

const { startLoginMock, finishMock, actionMock, frameMock } = vi.hoisted(() => ({
  startLoginMock: vi.fn(),
  finishMock: vi.fn(),
  actionMock: vi.fn(),
  frameMock: vi.fn()
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
      getWebConsoleLoginFrame: frameMock,
      postWebConsoleLoginAction: actionMock,
      finishWebConsoleLogin: finishMock,
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

  it('「手机验证码登录」必须出现在按钮区（这是切换登录方式的唯一入口）', async () => {
    // 线上投诉现场：用户看到的是二维码页，页面上明明写着「手机验证码登录」，
    // 面板里却没有能点它的地方，于是被永远关在扫码页里（2026-11-13）。
    startLoginMock.mockResolvedValue({
      session_id: 'sid-2',
      status: 'pending',
      phase: 'ready',
      screenshot: 'AAAA',
      fields: [],
      buttons: [
        { index: 0, text: '《用户协议》' },
        { index: 1, text: '手机验证码登录' }
      ],
      form_state: 'ok'
    })
    const wrapper = mountModal(buildConsoleAccount())
    await wrapper.get('[data-testid="web-console-login-start"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="web-console-login-button-1"]').text()).toBe('手机验证码登录')
    wrapper.unmount()
  })

  it('会话存下来了，但用量没读到：照实说，不冒充成功', async () => {
    // 2026-11-13 线上：探测把「读不到用量」当成「没登录」，会话被反复丢掉，
    // 用户看到的是「登录已失效，请重新登录」。现在会话照存，只把实话说清楚。
    actionMock.mockReset()
    actionMock.mockResolvedValue(null)
    startLoginMock.mockResolvedValue({
      session_id: 'sid-4',
      status: 'pending',
      phase: 'ready',
      screenshot: 'AAAA',
      fields: [{ index: 0, type: 'tel', label: '手机号', filled: false, masked: false }],
      buttons: [],
      form_state: 'ok'
    })
    finishMock.mockReset()
    finishMock.mockResolvedValue({ status: 'succeeded', message: 'probe_needs_interactive' })
    const wrapper = mountModal(buildConsoleAccount())
    await wrapper.get('[data-testid="web-console-login-start"]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-testid="web-console-login-finish"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.webConsoleQuota.loginSavedButProbe')
    // 注意：本文件的 i18n mock 只回 key、不做插值，所以断言不到具体 reason；
    // 真实文案会把 {reason} 换成后端给的标签。
    wrapper.unmount()
  })

  it('登录一生效就自动保存，不要求用户去找「我已完成登录」', async () => {
    // 2026-11-13 线上：用户登录成功了，但那个按钮埋在面板底部看不见，
    // 他只看见弹窗的绿色「更新」（那是保存账号配置的），于是会话一直存不进去。
    // 契约：看到 status=authenticated 就必须自动走一遍保存流程。
    frameMock.mockReset()
    frameMock.mockResolvedValue({
      session_id: 'sid-5',
      status: 'authenticated',
      phase: 'ready',
      screenshot: 'AAAA',
      fields: [],
      buttons: [],
      form_state: 'ok'
    })
    finishMock.mockReset()
    finishMock.mockResolvedValue({ status: 'succeeded' })
    startLoginMock.mockResolvedValue({
      session_id: 'sid-5',
      status: 'pending',
      phase: 'opening'
    })

    const wrapper = mountModal(buildConsoleAccount())
    await wrapper.get('[data-testid="web-console-login-start"]').trigger('click')
    await flushPromises()
    // 等一轮轮询（1.2s）把 authenticated 帧拿回来
    await new Promise((resolve) => setTimeout(resolve, 1400))
    await flushPromises()

    expect(finishMock).toHaveBeenCalled()
    expect(finishMock.mock.calls[0][1]).toBe('sid-5')
    wrapper.unmount()
  })

  it('「我已完成登录」必须在面板最上方就能看见（兜底）', async () => {
    startLoginMock.mockResolvedValue({
      session_id: 'sid-6',
      status: 'pending',
      phase: 'ready',
      screenshot: 'AAAA',
      fields: [],
      buttons: [],
      form_state: 'ok'
    })
    const wrapper = mountModal(buildConsoleAccount())
    await wrapper.get('[data-testid="web-console-login-start"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="web-console-login-done-row"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="web-console-login-finish"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('读不到表单时说「没读到」，不能替页面下结论说「没有输入框」', async () => {
    // 读失败（页面跳转中 / 超时）与「这页确实没有输入框」返回值都是空数组，
    // 旧界面把后者当事实显示，用户既不知道怎么回事也没有下一步。
    startLoginMock.mockResolvedValue({
      session_id: 'sid-3',
      status: 'pending',
      phase: 'ready',
      screenshot: 'AAAA',
      fields: [],
      buttons: [],
      form_state: 'unavailable'
    })
    const wrapper = mountModal(buildConsoleAccount())
    await wrapper.get('[data-testid="web-console-login-start"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="web-console-login-form-unavailable"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.webConsoleQuota.formUnavailable')
    expect(wrapper.text()).not.toContain('admin.accounts.webConsoleQuota.formEmpty')
    // 必须给一个能自己救回来的按钮
    expect(wrapper.find('[data-testid="web-console-login-form-retry"]').exists()).toBe(true)
    wrapper.unmount()
  })
})
