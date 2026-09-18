import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import WebLoginModal from '../WebLoginModal.vue'

const mocks = vi.hoisted(() => ({
  validateWebCredentialsMock: vi.fn(),
  createWebLoginProxySessionMock: vi.fn(),
  getWebLoginProxyCaptureMock: vi.fn(),
  deleteWebLoginProxySessionMock: vi.fn()
}))

const appStoreState = vi.hoisted(() => ({
  // 控制 WebLoginModal 读取的 public settings；默认缺 web_login_proxy_origin → 代理不可用降级。
  cachedPublicSettings: null as null | { web_login_proxy_origin?: string }
}))

vi.mock('@/api/admin/accounts', () => ({
  validateWebCredentials: mocks.validateWebCredentialsMock,
  createWebLoginProxySession: mocks.createWebLoginProxySessionMock,
  getWebLoginProxyCapture: mocks.getWebLoginProxyCaptureMock,
  deleteWebLoginProxySession: mocks.deleteWebLoginProxySessionMock
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => appStoreState
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

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /></div>'
})

const PROXY_URL = '/api/v1/web-login-proxy/t1/'

function mountModal(platform: 'deepseek' | 'zhipu' | 'kimi' = 'zhipu', show = true) {
  return mount(WebLoginModal, {
    props: { show, platform },
    global: {
      stubs: { BaseDialog: BaseDialogStub, teleport: true }
    }
  })
}

async function flush() {
  await flushPromises()
  // captured -> handleValidateAndApply is async; flush a couple of cycles.
  await flushPromises()
  await flushPromises()
}

describe('WebLoginModal', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    appStoreState.cachedPublicSettings = null
    // 默认：代理会话成功、轮询未捕获、校验通过。
    mocks.createWebLoginProxySessionMock.mockResolvedValue({
      token: 't1',
      url: PROXY_URL,
      expires_at: '2099-01-01T00:00:00Z'
    })
    mocks.getWebLoginProxyCaptureMock.mockResolvedValue({
      captured: false,
      cookie: '',
      expires_at: ''
    })
    mocks.validateWebCredentialsMock.mockResolvedValue(true)
    mocks.deleteWebLoginProxySessionMock.mockResolvedValue(undefined)
  })

  it('renders cookie textarea for cookie platforms and token JSON for kimi', async () => {
    const zhipu = mountModal('zhipu')
    await flush()
    expect(zhipu.find('[data-testid="web-login-cookie-input"]').exists()).toBe(true)
    expect(zhipu.find('[data-testid="web-login-token-json"]').exists()).toBe(false)
    zhipu.unmount()

    const kimi = mountModal('kimi')
    await flush()
    expect(kimi.find('[data-testid="web-login-token-json"]').exists()).toBe(true)
    expect(kimi.find('[data-testid="web-login-cookie-input"]').exists()).toBe(false)
  })

  it('shows new-tab button for all platforms', async () => {
    const zhipu = mountModal('zhipu')
    await flush()
    expect(zhipu.find('[data-testid="web-login-open-new-tab"]').exists()).toBe(true)
    zhipu.unmount()

    const kimi = mountModal('kimi')
    await flush()
    expect(kimi.find('[data-testid="web-login-open-new-tab"]').exists()).toBe(true)
  })

  it('opens official login page in a new tab on click', async () => {
    const openSpy = vi.spyOn(window, 'open').mockReturnValue(null)
    const wrapper = mountModal('deepseek')
    await flush()
    await wrapper.find('[data-testid="web-login-open-new-tab"]').trigger('click')
    expect(openSpy).toHaveBeenCalledWith('https://chat.deepseek.com/', '_blank', 'noopener')
    openSpy.mockRestore()
  })

  it('embeds proxy iframe when an isolated origin is configured and starts polling', async () => {
    appStoreState.cachedPublicSettings = { web_login_proxy_origin: 'https://wlp.example.com:8443' }
    const wrapper = mountModal('zhipu')
    await flush()
    const wrap = wrapper.find('[data-testid="web-login-proxy-iframe-wrap"]')
    expect(wrap.exists()).toBe(true)
    // iframe src 带隔离 origin 前缀（同源回退已移除）。
    expect(wrap.find('iframe').attributes('src')).toBe('https://wlp.example.com:8443' + PROXY_URL)
    expect(wrap.find('iframe').attributes('sandbox')).toBeUndefined()
    // 轮询已启动（首轮已调用 capture）。
    expect(mocks.getWebLoginProxyCaptureMock).toHaveBeenCalled()
  })

  it('shows proxy-unavailable hint and no iframe when web_login_proxy_origin is missing (no same-origin fallback)', async () => {
    // 同源回退已禁止：origin 缺失视为代理不可用，降级官方页登录 + 手动粘贴。
    appStoreState.cachedPublicSettings = null
    const wrapper = mountModal('zhipu')
    await flush()
    expect(wrapper.find('[data-testid="web-login-proxy-unavailable"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="web-login-proxy-iframe-wrap"]').exists()).toBe(false)
    // 不回退同源，故代理轮询不启动。
    expect(mocks.getWebLoginProxyCaptureMock).not.toHaveBeenCalled()
  })

  it('prepends web_login_proxy_origin to iframe src when settings provide an isolated origin', async () => {
    appStoreState.cachedPublicSettings = { web_login_proxy_origin: 'https://wlp.example.com:8443' }
    const wrapper = mountModal('zhipu')
    await flush()
    const wrap = wrapper.find('[data-testid="web-login-proxy-iframe-wrap"]')
    expect(wrap.find('iframe').attributes('src')).toBe('https://wlp.example.com:8443' + PROXY_URL)
  })

  it('auto-fills cookie, validates and emits applied + close on capture success', async () => {
    appStoreState.cachedPublicSettings = { web_login_proxy_origin: 'https://wlp.example.com:8443' }
    mocks.getWebLoginProxyCaptureMock.mockResolvedValue({
      captured: true,
      cookie: 'sessionid=abc',
      expires_at: '2099-01-01T00:00:00Z'
    })
    const wrapper = mountModal('zhipu')
    await flush()

    expect((wrapper.find('[data-testid="web-login-cookie-input"]').element as HTMLTextAreaElement).value).toBe('sessionid=abc')
    expect(wrapper.find('[data-testid="web-login-autofilled"]').exists()).toBe(true)
    expect(mocks.validateWebCredentialsMock).toHaveBeenCalledWith('zhipu', expect.objectContaining({ cookie: 'sessionid=abc' }))
    expect(wrapper.emitted('applied')).toBeTruthy()
    expect(wrapper.emitted('applied')![0][0]).toEqual({
      platform: 'zhipu',
      credentials: { access_mode: 'web', cookie: 'sessionid=abc' }
    })
    expect(wrapper.emitted('close')).toBeTruthy()
  })

  it('shows proxy-unavailable hint and does not crash when session creation fails', async () => {
    mocks.createWebLoginProxySessionMock.mockRejectedValue(new Error('proxy down'))
    const wrapper = mountModal('zhipu')
    await flush()

    expect(wrapper.find('[data-testid="web-login-proxy-unavailable"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="web-login-proxy-iframe-wrap"]').exists()).toBe(false)
    expect(mocks.getWebLoginProxyCaptureMock).not.toHaveBeenCalled()
  })

  it('does not poll for kimi even when proxy iframe is shown', async () => {
    appStoreState.cachedPublicSettings = { web_login_proxy_origin: 'https://wlp.example.com:8443' }
    const wrapper = mountModal('kimi')
    await flush()

    expect(wrapper.find('[data-testid="web-login-proxy-iframe-wrap"]').exists()).toBe(true)
    expect(mocks.getWebLoginProxyCaptureMock).not.toHaveBeenCalled()
  })

  it('deletes the proxy session on close', async () => {
    appStoreState.cachedPublicSettings = { web_login_proxy_origin: 'https://wlp.example.com:8443' }
    const wrapper = mountModal('zhipu')
    await flush()
    expect(mocks.deleteWebLoginProxySessionMock).not.toHaveBeenCalled()

    const cancel = wrapper.findAll('button').find((b) => b.text() === 'common.cancel')!
    await cancel.trigger('click')
    await flush()

    expect(mocks.deleteWebLoginProxySessionMock).toHaveBeenCalledWith('t1')
    expect(wrapper.emitted('close')).toBeTruthy()
  })

  it('shows empty-input error without calling backend', async () => {
    const wrapper = mountModal('zhipu')
    await flush()
    await wrapper.find('[data-testid="web-login-submit"]').trigger('click')
    await flush()
    expect(wrapper.find('[data-testid="web-login-validation-error"]').exists()).toBe(true)
    expect(mocks.validateWebCredentialsMock).not.toHaveBeenCalled()
  })

  it('validates cookie paste and emits applied + close on success', async () => {
    const wrapper = mountModal('zhipu')
    await flush()
    await wrapper.find('[data-testid="web-login-cookie-input"]').setValue('sessionid=abc; waf_cookie=def')
    await wrapper.find('[data-testid="web-login-submit"]').trigger('click')
    await flush()

    expect(mocks.validateWebCredentialsMock).toHaveBeenCalledWith('zhipu', expect.objectContaining({ cookie: 'sessionid=abc; waf_cookie=def' }))
    expect(wrapper.emitted('applied')).toBeTruthy()
    expect(wrapper.emitted('applied')![0][0]).toEqual({
      platform: 'zhipu',
      credentials: { access_mode: 'web', cookie: 'sessionid=abc; waf_cookie=def' }
    })
    expect(wrapper.emitted('close')).toBeTruthy()
  })

  it('validates kimi token JSON and emits built credentials', async () => {
    const wrapper = mountModal('kimi')
    await flush()
    await wrapper.find('[data-testid="web-login-token-json"]').setValue('{"access_token":"tok","refresh_token":"rt","user_id":"u1"}')
    await wrapper.find('[data-testid="web-login-submit"]').trigger('click')
    await flush()

    expect(mocks.validateWebCredentialsMock).toHaveBeenCalledWith('kimi', expect.objectContaining({ access_token: 'tok' }))
    expect(wrapper.emitted('applied')![0][0]).toEqual({
      platform: 'kimi',
      credentials: { access_mode: 'web', access_token: 'tok', refresh_token: 'rt', user_id: 'u1' }
    })
    expect(wrapper.emitted('close')).toBeTruthy()
  })

  it('shows validation error and does not emit applied when backend rejects', async () => {
    mocks.validateWebCredentialsMock.mockResolvedValue(false)
    const wrapper = mountModal('zhipu')
    await flush()
    await wrapper.find('[data-testid="web-login-cookie-input"]').setValue('sessionid=abc')
    await wrapper.find('[data-testid="web-login-submit"]').trigger('click')
    await flush()

    expect(wrapper.find('[data-testid="web-login-validation-error"]').exists()).toBe(true)
    expect(wrapper.emitted('applied')).toBeFalsy()
    expect(wrapper.emitted('close')).toBeFalsy()
  })

  it('does not emit close while validating', async () => {
    let resolveValidate: (v: boolean) => void = () => {}
    mocks.validateWebCredentialsMock.mockReturnValue(new Promise<boolean>((r) => { resolveValidate = r }))
    const wrapper = mountModal('zhipu')
    await flush()
    await wrapper.find('[data-testid="web-login-cookie-input"]').setValue('sessionid=abc')
    await wrapper.find('[data-testid="web-login-submit"]').trigger('click')
    await wrapper.vm.$nextTick()

    await wrapper.find('[data-testid="web-login-submit"]').trigger('click')
    expect(wrapper.emitted('close')).toBeFalsy()
    resolveValidate(true)
    await flush()
    expect(wrapper.emitted('applied')).toBeTruthy()
  })

  it('resets state when reopened', async () => {
    const wrapper = mountModal('zhipu', false)
    await wrapper.setProps({ show: true })
    await flush()
    await wrapper.find('[data-testid="web-login-cookie-input"]').setValue('sessionid=abc')
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flush()
    expect((wrapper.find('[data-testid="web-login-cookie-input"]').element as HTMLTextAreaElement).value).toBe('')
  })

  it('is not disturbed by the capture polling in manual-paste flows', async () => {
    // 默认未捕获，轮询不应自动填入、不应误触发校验/emit。
    const wrapper = mountModal('zhipu')
    await flush()
    expect((wrapper.find('[data-testid="web-login-cookie-input"]').element as HTMLTextAreaElement).value).toBe('')
    expect(wrapper.emitted('applied')).toBeFalsy()
    expect(wrapper.emitted('close')).toBeFalsy()
  })
})
