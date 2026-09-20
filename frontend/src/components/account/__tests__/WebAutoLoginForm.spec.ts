import { describe, expect, it, beforeEach, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { CreateAccountRequest } from '@/types'

const { webLoginPasswordMock, webLoginSmsMock, startChallengeMock, statusChallengeMock, consumeChallengeMock } = vi.hoisted(() => ({
  webLoginPasswordMock: vi.fn(),
  webLoginSmsMock: vi.fn(),
  startChallengeMock: vi.fn(),
  statusChallengeMock: vi.fn(),
  consumeChallengeMock: vi.fn()
}))

vi.mock('@/api/admin/webAutoLogin', () => ({
  webLoginPassword: webLoginPasswordMock,
  webLoginSms: webLoginSmsMock,
  startWebLoginChallenge: startChallengeMock,
  getWebLoginChallengeStatus: statusChallengeMock,
  consumeWebLoginChallenge: consumeChallengeMock
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
})
