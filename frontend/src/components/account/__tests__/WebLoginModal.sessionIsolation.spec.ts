import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import WebLoginModal from '../WebLoginModal.vue'

// 登录会话隔离回归（docs/web-platform-account-pool-auto-login-plan.md §0）：
// 1. 新窗口兜底路径（window.open 直连官方域）无法自动隔离，界面必须明确提示
//    用户先退出官方账号 / 使用无痕窗口，不得宣称 window.open 能自动无痕；
// 2. 同平台既有账号已持有相同登录态（后端 reason=web_credential_duplicate）时
//    新建不得伪装成功，必须展示明确错误。

const mocks = vi.hoisted(() => ({
  validateWebCredentialsMock: vi.fn(),
  createWebLoginProxySessionMock: vi.fn(),
  getWebLoginProxyCaptureMock: vi.fn(),
  deleteWebLoginProxySessionMock: vi.fn()
}))

const appStoreState = vi.hoisted(() => ({
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

function mountModal(platform: 'deepseek' | 'zhipu' | 'kimi' = 'zhipu') {
  return mount(WebLoginModal, {
    props: { show: true, platform },
    global: {
      stubs: { BaseDialog: BaseDialogStub, teleport: true }
    }
  })
}

async function flush() {
  await flushPromises()
  await flushPromises()
}

describe('WebLoginModal 登录会话隔离', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    appStoreState.cachedPublicSettings = null
    mocks.createWebLoginProxySessionMock.mockRejectedValue(new Error('proxy unavailable'))
    mocks.getWebLoginProxyCaptureMock.mockResolvedValue({
      captured: false,
      cookie: '',
      expires_at: ''
    })
    mocks.validateWebCredentialsMock.mockResolvedValue(true)
    mocks.deleteWebLoginProxySessionMock.mockResolvedValue(undefined)
  })

  it('新窗口兜底按钮不静默 window.open 复用会话：含隔离提示', async () => {
    const openSpy = vi.spyOn(window, 'open').mockReturnValue(null)
    const modal = mountModal('zhipu')
    await flush()

    const hint = modal.find('[data-testid="web-login-open-new-tab"]')
    expect(hint.exists()).toBe(true)

    // 隔离提示：打开前先退出官方账号 / 使用无痕窗口 / 独立浏览器 Profile。
    const hintText = modal.element.textContent ?? ''
    expect(hintText).toContain('webLogin.openOfficialHint')

    // 点击仍走 window.open（兜底可用），但提示文案必须包含隔离约束（不宣称自动无痕）。
    await hint.trigger('click')
    expect(openSpy).toHaveBeenCalledTimes(1)
    openSpy.mockRestore()
    modal.unmount()
  })

  it('同平台既有账号持有相同登录态（409 web_credential_duplicate）时展示明确错误，不伪装成功', async () => {
    // 后端 409 → axios 拦截器扁平 reject {status, reason, message}。
    mocks.validateWebCredentialsMock.mockRejectedValue({
      status: 409,
      reason: 'web_credential_duplicate',
      message: '检测到已有账号持有相同登录会话的凭证'
    })

    const modal = mountModal('zhipu')
    await flush()

    const input = modal.find('[data-testid="web-login-cookie-input"]')
    expect(input.exists()).toBe(true)
    await input.setValue('chatglm_token=existing-session')
    await modal.find('[data-testid="web-login-submit"]').trigger('click')
    await flush()

    // 不得 emit applied（不得把复用会话伪装成新账号），且展示去重提示。
    const appliedEvents = modal.emitted('applied')
    expect(appliedEvents).toBeUndefined()
    const errorEl = modal.find('[data-testid="web-login-validation-error"]')
    expect(errorEl.exists()).toBe(true)
    expect(errorEl.text()).toBe('admin.accounts.webProviders.errors.webCredentialDuplicate')
    modal.unmount()
  })

  it('其余后端错误回落通用失败提示', async () => {
    mocks.validateWebCredentialsMock.mockRejectedValue({
      status: 500,
      message: 'internal error'
    })

    const modal = mountModal('zhipu')
    await flush()

    const input = modal.find('[data-testid="web-login-cookie-input"]')
    await input.setValue('chatglm_token=some-cookie')
    await modal.find('[data-testid="web-login-submit"]').trigger('click')
    await flush()

    const errorEl = modal.find('[data-testid="web-login-validation-error"]')
    expect(errorEl.exists()).toBe(true)
    expect(errorEl.text()).toBe('internal error')
    modal.unmount()
  })

  it('origin 缺失时立即删除已创建 session，不等 TTL，并降级手动粘贴', async () => {
    // createWebLoginProxySession 成功但 public settings 缺 web_login_proxy_origin。
    mocks.createWebLoginProxySessionMock.mockResolvedValue({
      token: 't-orphan',
      url: '/api/v1/web-login-proxy/t-orphan/',
      expires_at: '2099-01-01T00:00:00Z'
    })

    const modal = mountModal('zhipu')
    await flush()

    // 已创建 session 必须被立即删除（不得等 TTL 释放槽位）。
    expect(mocks.deleteWebLoginProxySessionMock).toHaveBeenCalledWith('t-orphan')
    // 降级路径：无 iframe、无轮询、展示不可用提示。
    expect(modal.find('[data-testid="web-login-proxy-iframe-wrap"]').exists()).toBe(false)
    expect(modal.find('[data-testid="web-login-proxy-unavailable"]').exists()).toBe(true)
    expect(mocks.getWebLoginProxyCaptureMock).not.toHaveBeenCalled()
    modal.unmount()
  })

  it('清理失败有兜底：删除 session 失败不阻断降级，日志由 TTL 兜底', async () => {
    mocks.createWebLoginProxySessionMock.mockResolvedValue({
      token: 't-orphan2',
      url: '/api/v1/web-login-proxy/t-orphan2/',
      expires_at: '2099-01-01T00:00:00Z'
    })
    mocks.deleteWebLoginProxySessionMock.mockRejectedValue(new Error('delete failed'))
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {})

    const modal = mountModal('zhipu')
    await flush()

    expect(mocks.deleteWebLoginProxySessionMock).toHaveBeenCalledWith('t-orphan2')
    // 清理失败不阻断降级路径（不抛错、仍展示不可用提示）。
    expect(modal.find('[data-testid="web-login-proxy-unavailable"]').exists()).toBe(true)
    expect(warnSpy).toHaveBeenCalled()
    warnSpy.mockRestore()
    modal.unmount()
  })

  it('弹窗卸载与异步创建并发时：旧会话删除，不启动捕获轮询', async () => {
    // 创建请求挂起，期间弹窗被卸载（关闭竞态）。
    let resolveCreate: (v: { token: string; url: string; expires_at: string }) => void = () => {}
    mocks.createWebLoginProxySessionMock.mockReturnValue(
      new Promise((r) => { resolveCreate = r })
    )

    const modal = mountModal('zhipu')
    modal.unmount() // 异步创建尚未返回即卸载

    resolveCreate({
      token: 't-race',
      url: '/api/v1/web-login-proxy/t-race/',
      expires_at: '2099-01-01T00:00:00Z'
    })
    await flush()

    // 竞态返回的旧会话必须被立即删除（不遗留到 TTL）。
    expect(mocks.deleteWebLoginProxySessionMock).toHaveBeenCalledWith('t-race')
    // 不得重新启动捕获轮询。
    expect(mocks.getWebLoginProxyCaptureMock).not.toHaveBeenCalled()
  })
})

// 弹窗关闭遗留 session 回归：成功关闭、show true→false、重复清理均须删除
// 代理 session 并停止轮询（子组件常驻，父组件仅置 show=false）。
describe('WebLoginModal 弹窗关闭清理', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    appStoreState.cachedPublicSettings = { web_login_proxy_origin: 'https://wlp.example.com' }
    mocks.createWebLoginProxySessionMock.mockResolvedValue({
      token: 't-close',
      url: '/api/v1/web-login-proxy/t-close/',
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

  it('成功提交关闭后 session 被删除（emit close 前清理）', async () => {
    const modal = mountModal('zhipu')
    await flush()

    const input = modal.find('[data-testid="web-login-cookie-input"]')
    await input.setValue('chatglm_token=fresh-session')
    await modal.find('[data-testid="web-login-submit"]').trigger('click')
    await flush()

    expect(modal.emitted('applied')).toHaveLength(1)
    expect(modal.emitted('close')).toHaveLength(1)
    // 成功关闭路径直接 emit('close')，session 必须在此之前被删除。
    expect(mocks.deleteWebLoginProxySessionMock).toHaveBeenCalledWith('t-close')
    modal.unmount()
  })

  it('show true→false 触发清理；重复清理幂等（不重复删除同一 token）', async () => {
    const parent = mount({
      components: { WebLoginModal, BaseDialog: BaseDialogStub },
      data: () => ({ show: true }),
      template: '<WebLoginModal :show="show" platform="zhipu" @close="show = false" />'
    }, {
      global: { stubs: { BaseDialog: BaseDialogStub, teleport: true } }
    })
    await flush()

    // 打开时已建立会话（未删除）。
    expect(mocks.deleteWebLoginProxySessionMock).not.toHaveBeenCalled()

    // show true→false：watch 关闭分支触发 cleanupSession。
    ;(parent.vm as unknown as { show: boolean }).show = false
    await flush()
    expect(mocks.deleteWebLoginProxySessionMock).toHaveBeenCalledTimes(1)
    expect(mocks.deleteWebLoginProxySessionMock).toHaveBeenCalledWith('t-close')

    // 再次置 false（重复清理）：token 已清空，不重复删除同一 session。
    ;(parent.vm as unknown as { show: boolean }).show = false
    await flush()
    expect(mocks.deleteWebLoginProxySessionMock).toHaveBeenCalledTimes(1)
    parent.unmount()
  })
})
