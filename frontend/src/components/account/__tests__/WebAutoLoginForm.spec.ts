import { describe, expect, it, beforeEach, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { CreateAccountRequest } from '@/types'

const { webLoginPasswordMock, webLoginSmsMock, startChallengeMock, statusChallengeMock, consumeChallengeMock, webRegisterEmailCodeMock, webRegisterMock } = vi.hoisted(() => ({
  webLoginPasswordMock: vi.fn(),
  webLoginSmsMock: vi.fn(),
  startChallengeMock: vi.fn(),
  statusChallengeMock: vi.fn(),
  consumeChallengeMock: vi.fn(),
  webRegisterEmailCodeMock: vi.fn(),
  webRegisterMock: vi.fn()
}))

vi.mock('@/api/admin/webAutoLogin', () => ({
  webLoginPassword: webLoginPasswordMock,
  webLoginSms: webLoginSmsMock,
  startWebLoginChallenge: startChallengeMock,
  getWebLoginChallengeStatus: statusChallengeMock,
  consumeWebLoginChallenge: consumeChallengeMock,
  webRegisterEmailCode: webRegisterEmailCodeMock,
  webRegister: webRegisterMock
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

import WebAutoLoginForm from '../WebAutoLoginForm.vue'

const accountDraft: CreateAccountRequest = {
  name: 'draft web account',
  platform: 'deepseek',
  type: 'apikey',
  credentials: { access_mode: 'web' },
  group_ids: [3],
  concurrency: 4
}

beforeEach(() => {
  webLoginPasswordMock.mockReset()
  webLoginSmsMock.mockReset()
  startChallengeMock.mockReset()
  statusChallengeMock.mockReset()
  consumeChallengeMock.mockReset()
  webRegisterEmailCodeMock.mockReset()
  webRegisterMock.mockReset()
  startChallengeMock.mockResolvedValue({ success: true, session_id: 'opaque-session' })
  statusChallengeMock.mockResolvedValue({ success: true, session_id: 'opaque-session', status: 'succeeded' })
  consumeChallengeMock.mockResolvedValue({ success: true, session_id: 'opaque-session', status: 'consumed' })
})

describe('WebAutoLoginForm', () => {
  it('password new-account request carries account_draft and success emits only platform/account_id', async () => {
    webLoginPasswordMock.mockResolvedValue({ success: true, account_id: 12 })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-auto-login-identifier"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-auto-login-password"]').setValue('pw')
    await wrapper.find('[data-testid="web-auto-login-submit"]').trigger('click')
    await flushPromises()

    expect(webLoginPasswordMock).toHaveBeenCalledWith({
      platform: 'deepseek',
      login_email: 'user@deepseek.com',
      login_password: 'pw',
      account_draft: accountDraft
    })
    expect(webLoginPasswordMock.mock.calls[0][0]).not.toHaveProperty('account_id')
    expect(wrapper.emitted('recovered')).toEqual([[{ platform: 'deepseek', account_id: 12 }]])
  })

  it('existing password re-login carries account_id and omits account_draft', async () => {
    webLoginPasswordMock.mockResolvedValue({ success: true, account_id: 77 })
    const wrapper = mount(WebAutoLoginForm, {
      props: { platform: 'deepseek', accountId: 77, accountDraft }
    })

    await wrapper.find('[data-testid="web-auto-login-identifier"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-auto-login-password"]').setValue('pw')
    await wrapper.find('[data-testid="web-auto-login-submit"]').trigger('click')
    await flushPromises()

    expect(webLoginPasswordMock).toHaveBeenCalledWith({
      platform: 'deepseek',
      login_email: 'user@deepseek.com',
      login_password: 'pw',
      account_id: 77
    })
    expect(webLoginPasswordMock.mock.calls[0][0]).not.toHaveProperty('account_draft')
  })

  it('password with leading/trailing spaces is sent verbatim (only trimmed for the empty check)', async () => {
    webLoginPasswordMock.mockResolvedValue({ success: true, account_id: 31 })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek' } })

    await wrapper.find('[data-testid="web-auto-login-identifier"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-auto-login-password"]').setValue('  pw spaced  ')
    await wrapper.find('[data-testid="web-auto-login-submit"]').trigger('click')
    await flushPromises()

    expect(webLoginPasswordMock).toHaveBeenCalledWith({
      platform: 'deepseek',
      login_email: 'user@deepseek.com',
      login_password: '  pw spaced  '
    })
  })

  it('SMS new-account send/login requests carry account_draft and emit only account identity', async () => {
    webLoginSmsMock.mockImplementation(async (request: { action: string }) =>
      request.action === 'send_code' ? { success: true } : { success: true, account_id: 8 }
    )
    const wrapper = mount(WebAutoLoginForm, {
      props: { platform: 'kimi', accountDraft: { ...accountDraft, platform: 'kimi' } }
    })

    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000001')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-auto-login-sms-code"]').setValue('654321')
    await wrapper.find('[data-testid="web-auto-login-sms-submit"]').trigger('click')
    await flushPromises()

    expect(startChallengeMock).toHaveBeenCalledWith({
      platform: 'kimi',
      phone: '13800000001',
      account_draft: { ...accountDraft, platform: 'kimi' }
    })
    expect(statusChallengeMock).toHaveBeenCalledWith('opaque-session')
    expect(consumeChallengeMock).toHaveBeenCalledWith('opaque-session', {
      platform: 'kimi',
      phone: '13800000001',
      account_draft: { ...accountDraft, platform: 'kimi' }
    })
    expect(webLoginSmsMock).toHaveBeenNthCalledWith(1, {
      action: 'send_code',
      platform: 'kimi',
      phone: '13800000001',
      challenge_session_id: 'opaque-session'
    })
    expect(webLoginSmsMock).toHaveBeenNthCalledWith(2, {
      action: 'login',
      platform: 'kimi',
      phone: '13800000001',
      sms_code: '654321',
      challenge_session_id: 'opaque-session'
    })
    expect(wrapper.emitted('recovered')).toEqual([[{ platform: 'kimi', account_id: 8 }]])
  })

  it('requires an account name before sending the SMS code in new-account mode', async () => {
    const wrapper = mount(WebAutoLoginForm, {
      props: { platform: 'kimi', accountDraft: { ...accountDraft, platform: 'kimi', name: '' } }
    })

    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000001')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()

    expect(startChallengeMock).not.toHaveBeenCalled()
    expect(webLoginSmsMock).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="web-auto-login-error"]').text()).toContain('accountNameRequired')
    expect(wrapper.find('[data-testid="web-auto-login-challenge-status"]').text()).toContain('pending')
  })

  it('existing SMS re-login carries account_id and omits account_draft', async () => {
    webLoginSmsMock.mockResolvedValue({ success: true })
    const wrapper = mount(WebAutoLoginForm, {
      props: { platform: 'zhipu', accountId: 19, accountDraft }
    })

    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()

    expect(webLoginSmsMock).toHaveBeenCalledWith({
      action: 'send_code', platform: 'zhipu', phone: '13800000000', challenge_session_id: 'opaque-session'
    })
    expect(webLoginSmsMock.mock.calls[0][0]).not.toHaveProperty('account_id')
    expect(webLoginSmsMock.mock.calls[0][0]).not.toHaveProperty('account_draft')
  })

  it.each([
    ['success', { success: true, status: 'succeeded' }, 'succeeded'],
    ['failure', { success: false, status: 'failed', detail: 'failed' }, 'failed']
  ])('challenge status is %s without exposing credentials', async (_label, response, status) => {
    statusChallengeMock.mockResolvedValue(response)
    consumeChallengeMock.mockResolvedValue({ success: true, status: 'consumed' })
    webLoginSmsMock.mockResolvedValue({ success: true })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu' } })
    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="web-auto-login-challenge-status"]').text()).toContain(status)
  })

  it('stops without sending SMS when challenge context is unavailable', async () => {
    startChallengeMock.mockResolvedValue({ success: false, status: 'context_gap', detail: 'context gap' })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu' } })
    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()

    expect(webLoginSmsMock).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="web-auto-login-challenge-status"]').text()).toContain('context_gap')
  })

  it('maps an expired challenge response to expired status', async () => {
    statusChallengeMock.mockResolvedValue({ success: true, status: 'expired', detail: 'expired' })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu' } })
    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="web-auto-login-challenge-status"]').text()).toContain('expired')
  })


  it('does not update or emit after an in-flight request resolves after unmount', async () => {
    let resolveRequest!: (value: { success: boolean; account_id?: number }) => void
    webLoginPasswordMock.mockReturnValue(new Promise((resolve) => { resolveRequest = resolve }))
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })
    await wrapper.find('[data-testid="web-auto-login-identifier"]').setValue('a@b.com')
    await wrapper.find('[data-testid="web-auto-login-password"]').setValue('pw')
    await wrapper.find('[data-testid="web-auto-login-submit"]').trigger('click')
    wrapper.unmount()
    resolveRequest({ success: true, account_id: 55 })
    await flushPromises()
    expect(wrapper.emitted('recovered')).toBeUndefined()
  })

  it('does not enter the SMS step when an in-flight challenge request resolves after unmount', async () => {
    let resolveRequest!: (value: { success: boolean; session_id?: string }) => void
    startChallengeMock.mockReturnValue(new Promise((resolve) => { resolveRequest = resolve }))
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu', accountDraft } })
    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    wrapper.unmount()
    resolveRequest({ success: true, session_id: 'late-session' })
    await flushPromises()
    expect(wrapper.emitted('recovered')).toBeUndefined()
  })

  it('keeps challenge status pending before the challenge response', async () => {
    let resolveRequest!: (value: { success: boolean; session_id?: string }) => void
    startChallengeMock.mockReturnValue(new Promise((resolve) => { resolveRequest = resolve }))
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu' } })
    expect(wrapper.find('[data-testid="web-auto-login-challenge-status"]').text()).toContain('pending')
    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    expect(wrapper.find('[data-testid="web-auto-login-challenge-status"]').text()).toContain('pending')
    resolveRequest({ success: true })
    await flushPromises()
  })

  it('ignores repeated send-code clicks while a request is pending', async () => {
    let resolveRequest!: (value: { success: boolean }) => void
    webLoginSmsMock.mockReturnValue(new Promise((resolve) => { resolveRequest = resolve }))
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu' } })
    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000000')
    const button = wrapper.find('[data-testid="web-auto-login-send-code"]')
    await button.trigger('click')
    await button.trigger('click')
    expect(webLoginSmsMock).toHaveBeenCalledTimes(1)
    resolveRequest({ success: true })
    await flushPromises()
  })

  it('ignores repeated SMS login clicks while account creation is pending', async () => {
    let resolveLogin!: (value: { success: boolean; account_id?: number }) => void
    webLoginSmsMock
      .mockResolvedValueOnce({ success: true })
      .mockReturnValueOnce(new Promise((resolve) => { resolveLogin = resolve }))
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu', accountDraft } })
    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-auto-login-sms-code"]').setValue('123456')
    const button = wrapper.find('[data-testid="web-auto-login-sms-submit"]')
    await button.trigger('click')
    await button.trigger('click')
    expect(webLoginSmsMock).toHaveBeenCalledTimes(2)
    resolveLogin({ success: true, account_id: 21 })
    await flushPromises()
    expect(wrapper.emitted('recovered')).toEqual([[{ platform: 'zhipu', account_id: 21 }]])
  })

  it('register success carries account_draft and emits recovered', async () => {
    webRegisterEmailCodeMock.mockResolvedValue({ success: true, send_window_secs: 60 })
    webRegisterMock.mockResolvedValue({ success: true, account_id: 33 })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-register-password"]').setValue('passw0rd1')
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()

    expect(webRegisterMock).toHaveBeenCalledWith({
      platform: 'deepseek',
      email: 'user@deepseek.com',
      email_verification_code: '123456',
      password: 'passw0rd1',
      account_draft: accountDraft
    })
    expect(wrapper.emitted('recovered')).toEqual([[{ platform: 'deepseek', account_id: 33 }]])
  })

  it('register business failure (success:false, HTTP 200) falls back to password mode with email/password', async () => {
    webRegisterEmailCodeMock.mockResolvedValue({ success: true, send_window_secs: 60 })
    webRegisterMock.mockResolvedValue({ success: false })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-register-password"]').setValue('passw0rd1')
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()

    // 任务卡 §2.2-3：业务失败也切回「密码登录」并把已填邮箱/密码带过去。
    expect(wrapper.find('[data-testid="web-register-submit"]').exists()).toBe(false)
    expect((wrapper.find('[data-testid="web-auto-login-identifier"]').element as HTMLInputElement).value).toBe('user@deepseek.com')
    expect((wrapper.find('[data-testid="web-auto-login-password"]').element as HTMLInputElement).value).toBe('passw0rd1')
    // errorMsg 为共用通道，切回密码模式后展示于 web-auto-login-error。
    expect(wrapper.find('[data-testid="web-auto-login-error"]').text()).toBe('admin.accounts.webLogin.register.registerFailed')
    expect(wrapper.emitted('recovered')).toBeUndefined()
  })

  it('hides register toggle in relogin mode (accountId present) and ignores programmatic entry', async () => {
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountId: 7, accountDraft } })

    // 重登表单不出现注册入口（外审 2026-09-22 P1）。
    expect(wrapper.find('[data-testid="web-register-mode-toggle"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="web-register-submit"]').exists()).toBe(false)
    // 密码登录分支保持可用。
    expect(wrapper.find('[data-testid="web-auto-login-identifier"]').exists()).toBe(true)
  })

  it('after register failure fallback the password submit button is re-enabled and password login works', async () => {
    // 外审 2026-09-22 P1：fallback 使代次递增后，submitRegister 的 finally 不再
    // 清 submitting，必须由 fallback 同步清除，否则密码提交按钮持续禁用。
    webRegisterEmailCodeMock.mockResolvedValue({ success: true, send_window_secs: 60 })
    webRegisterMock.mockResolvedValue({ success: false })
    webLoginPasswordMock.mockResolvedValue({ success: true, account_id: 44 })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-register-password"]').setValue('passw0rd1')
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()

    const submit = wrapper.find('[data-testid="web-auto-login-submit"]')
    expect(submit.attributes('disabled')).toBeUndefined()
    await submit.trigger('click')
    await flushPromises()
    expect(webLoginPasswordMock).toHaveBeenCalledWith({
      platform: 'deepseek',
      login_email: 'user@deepseek.com',
      login_password: 'passw0rd1',
      account_draft: accountDraft
    })
    expect(wrapper.emitted('recovered')).toEqual([[{ platform: 'deepseek', account_id: 44 }]])
    expect(wrapper.find('[data-testid="web-auto-login-submit"]').attributes('disabled')).toBeUndefined()
  })

  it('clicking the password-mode button during a pending password login does not drop the response', async () => {
    // 外审 2026-09-22 P2：密码模式下点「密码登录」按钮不得使在途登录请求失效。
    let resolveLogin!: (value: { success: boolean; account_id?: number }) => void
    webLoginPasswordMock.mockReturnValue(new Promise((resolve) => { resolveLogin = resolve }))
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-auto-login-identifier"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-auto-login-password"]').setValue('pw')
    await wrapper.find('[data-testid="web-auto-login-submit"]').trigger('click')
    // 登录等待期间点击当前已激活的「密码登录」模式按钮。
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[0].trigger('click')

    resolveLogin({ success: true, account_id: 9 })
    await flushPromises()

    expect(wrapper.emitted('recovered')).toEqual([[{ platform: 'deepseek', account_id: 9 }]])
    expect(wrapper.find('[data-testid="web-auto-login-submit"]').attributes('disabled')).toBeUndefined()
  })

  it('maps EMAIL_DOMAIN error detail to the email-domain message and other errors to passthrough', async () => {
    webRegisterEmailCodeMock.mockResolvedValue({ success: true, send_window_secs: 60 })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    const enterRegister = async () => {
      await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
      await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
      await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
      await flushPromises()
      await wrapper.find('[data-testid="web-register-code"]').setValue('123456')
      await wrapper.find('[data-testid="web-register-password"]').setValue('passw0rd1')
    }

    // 外审 2026-09-22 P1：detail 含 EMAIL_DOMAIN token → 邮箱域名文案。
    webRegisterMock.mockRejectedValueOnce({
      status: 400,
      metadata: { detail: '该邮箱域名不被上游支持（EMAIL_DOMAIN_NOT_SUPPORTED），请更换邮箱后重试' }
    })
    await enterRegister()
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="web-auto-login-error"]').text()).toBe('admin.accounts.webLogin.register.errorEmailDomain')

    // 外审 2026-09-22 P1：无 EMAIL_DOMAIN token 的错误（如验证码失败）不得误归为
    // 邮箱域名错误，走统一透传（显示后端 detail 原文而非分类文案）。
    webRegisterMock.mockRejectedValueOnce({
      status: 400,
      metadata: { detail: '邮箱验证码错误（EMAIL_PASSCODE_FAILED），请核对后重试' }
    })
    await enterRegister()
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="web-auto-login-error"]').text()).toBe('邮箱验证码错误（EMAIL_PASSCODE_FAILED），请核对后重试')
  })

  it('stale send-code response after platform switch does not write register state', async () => {
    let resolveSend!: (value: { success: boolean; send_window_secs?: number }) => void
    webRegisterEmailCodeMock.mockReturnValueOnce(new Promise((resolve) => { resolveSend = resolve }))
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()

    // 等待发码响应期间切换平台：watcher 重置注册状态并递增请求代次。
    await wrapper.setProps({ platform: 'kimi' })
    await flushPromises()
    resolveSend({ success: true, send_window_secs: 60 })
    await flushPromises()

    // 过期响应不得回写（外审 2026-09-22 P2）：表单保持密码登录模式（kimi 无注册入口）。
    expect(wrapper.find('[data-testid="web-register-email"]').exists()).toBe(false)
    expect(webRegisterEmailCodeMock).toHaveBeenCalledTimes(1)
  })
})
