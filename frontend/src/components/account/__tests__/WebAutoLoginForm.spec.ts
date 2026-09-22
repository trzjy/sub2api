import { describe, expect, it, beforeEach, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { CreateAccountRequest } from '@/types'

const { webLoginPasswordMock, webLoginSmsMock, startChallengeMock, statusChallengeMock, consumeChallengeMock, webRegisterEmailCodeMock, webRegisterMock, newWebRegisterIdempotencyKeyMock } = vi.hoisted(() => ({
  webLoginPasswordMock: vi.fn(),
  webLoginSmsMock: vi.fn(),
  startChallengeMock: vi.fn(),
  statusChallengeMock: vi.fn(),
  consumeChallengeMock: vi.fn(),
  webRegisterEmailCodeMock: vi.fn(),
  webRegisterMock: vi.fn(),
  // 外审 R2-P2：幂等键生成也走 mock，测试可断言键在结果不明重试间的复用语义。
  newWebRegisterIdempotencyKeyMock: vi.fn(() => 'web-register-register-test-key')
}))

vi.mock('@/api/admin/webAutoLogin', () => ({
  webLoginPassword: webLoginPasswordMock,
  webLoginSms: webLoginSmsMock,
  startWebLoginChallenge: startChallengeMock,
  getWebLoginChallengeStatus: statusChallengeMock,
  consumeWebLoginChallenge: consumeChallengeMock,
  webRegisterEmailCode: webRegisterEmailCodeMock,
  webRegister: webRegisterMock,
  newWebRegisterIdempotencyKey: newWebRegisterIdempotencyKeyMock
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
  newWebRegisterIdempotencyKeyMock.mockReset()
  let keySeq = 0
  newWebRegisterIdempotencyKeyMock.mockImplementation(() => `web-register-register-test-key-${++keySeq}`)
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
    }, { idempotencyKey: expect.stringMatching(/^web-register-register-test-key/) })
    expect(wrapper.emitted('recovered')).toEqual([[{ platform: 'deepseek', account_id: 33 }]])
  })

  // 外审 2026-09-22 R2-P2：结果不明（网络错误/5xx）后重试必须复用同一幂等键
  //（协调器重放首次结果），收到明确结果后新提交才换新键。
  it('reuses the idempotency key and original payload when retrying after an unknown-outcome failure; re-sending code starts a new operation', async () => {
    webRegisterEmailCodeMock.mockResolvedValue({ success: true, send_window_secs: 60 })
    // 第一次提交：网络错误（无 status，结果不明）。
    webRegisterMock.mockRejectedValueOnce(new Error('network down'))
    // 第二次提交（同键重试）：明确成功。
    webRegisterMock.mockResolvedValueOnce({ success: true, account_id: 44 })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-register-password"]').setValue('passw0rd1')

    // 第一次提交：网络错误（结果不明）。
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(webRegisterMock).toHaveBeenCalledTimes(1)
    const firstKey = webRegisterMock.mock.calls[0][1].idempotencyKey
    expect(firstKey).toBeTruthy()
    const firstPayload = webRegisterMock.mock.calls[0][0]
    expect(firstPayload.email_verification_code).toBe('123456')

    // 错误后 fallback 到密码登录模式；重进注册模式（外审 R3-P3：回填原提交字段，
    // 验证码/密码/邮箱原样保留，codeSent 视为已发，无需重发码）。
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await flushPromises()
    expect((wrapper.find('[data-testid="web-register-email"]').element as HTMLInputElement).value).toBe('user@deepseek.com')
    expect((wrapper.find('[data-testid="web-register-code"]').element as HTMLInputElement).value).toBe('123456')
    expect((wrapper.find('[data-testid="web-register-password"]').element as HTMLInputElement).value).toBe('passw0rd1')

    // 直接提交：同键 + 原 payload（含原验证码）——不触发 fingerprint 冲突。
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(webRegisterMock).toHaveBeenCalledTimes(2)
    expect(webRegisterMock.mock.calls[1][1].idempotencyKey).toBe(firstKey)
    expect(webRegisterMock.mock.calls[1][0]).toEqual(firstPayload)
    expect(wrapper.emitted('recovered')).toEqual([[{ platform: 'deepseek', account_id: 44 }]])

    // 键生成器只在注册提交时调用（发码键在 API 层生成，已被 mock）：首次提交 1 次。
    expect(newWebRegisterIdempotencyKeyMock).toHaveBeenCalledTimes(1)

    // 外审 R3-P3：显式重发码 = 明确开启新操作——重发码自身成功且轮换语义生效
    //（下一次提交会生成新键，而非复用已废弃的 firstKey）。
    webRegisterMock.mockRejectedValueOnce(Object.assign(new Error('gone'), { status: 502 }))
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    expect(webRegisterEmailCodeMock).toHaveBeenCalledTimes(2)
    await wrapper.find('[data-testid="web-register-code"]').setValue('654321')
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(webRegisterMock).toHaveBeenCalledTimes(3)
    // 新验证码 + 旧键已废弃 → 新键，且键生成器被再次调用。
    expect(webRegisterMock.mock.calls[2][1].idempotencyKey).not.toBe(firstKey)
    expect(newWebRegisterIdempotencyKeyMock).toHaveBeenCalledTimes(2)
  })

  it('an expired in-flight response never overwrites the retry pair left by a newer request (R4-P3 interleave)', async () => {
    webRegisterEmailCodeMock.mockResolvedValue({ success: true, send_window_secs: 60 })
    let rejectA!: (e: unknown) => void
    const promiseA = new Promise((_resolve, reject) => { rejectA = reject })
    // 请求 A：挂起在途。
    webRegisterMock.mockImplementationOnce(() => promiseA)
    // 请求 B：网络错误（结果不明，留下配对）。
    webRegisterMock.mockRejectedValueOnce(new Error('network down'))
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-register-password"]').setValue('passw0rd1')

    // 提交 A（挂起在途，键 keyA）。
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    const keyA = webRegisterMock.mock.calls[0][1].idempotencyKey
    expect(keyA).toBeTruthy()

    // A 在途时退出注册（代次递增使 A 过期、清除 submitting），重进。
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[0].trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await flushPromises()

    // 重发码（显式新操作：轮换注册键），填新验证码后提交 B（网络错误留配对）。
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-code"]').setValue('654321')
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(webRegisterMock).toHaveBeenCalledTimes(2)
    const keyB = webRegisterMock.mock.calls[1][1].idempotencyKey
    expect(keyB).not.toBe(keyA)
    const payloadB = webRegisterMock.mock.calls[1][0]

    // A 此刻才失败返回（已过期、键已轮换）：不得覆盖 B 留下的配对。
    rejectA(new Error('late network error'))
    await flushPromises()

    // 再次重试：仍复用 keyB + B 的原 payload（未被 A 的过期快照污染）。
    webRegisterMock.mockResolvedValueOnce({ success: true, account_id: 55 })
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[0].trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(webRegisterMock).toHaveBeenCalledTimes(3)
    expect(webRegisterMock.mock.calls[2][1].idempotencyKey).toBe(keyB)
    expect(webRegisterMock.mock.calls[2][0]).toEqual(payloadB)
    expect(wrapper.emitted('recovered')).toEqual([[{ platform: 'deepseek', account_id: 55 }]])
  })

  it('keeps the idempotency key when the coordinator returns a processing-conflict 409 (R5-P2)', async () => {
    webRegisterEmailCodeMock.mockResolvedValue({ success: true, send_window_secs: 60 })
    // 首次提交：协调器 processing 冲突 409（首次请求仍在处理，非终态）。
    webRegisterMock.mockRejectedValueOnce({
      status: 409, reason: 'IDEMPOTENCY_IN_PROGRESS', message: 'idempotent request is still processing'
    })
    // 同键重试：明确成功。
    webRegisterMock.mockResolvedValueOnce({ success: true, account_id: 77 })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-register-password"]').setValue('passw0rd1')

    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    const firstKey = webRegisterMock.mock.calls[0][1].idempotencyKey
    expect(firstKey).toBeTruthy()

    // fallback 到密码模式再回来（同键重试路径），直接提交。
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[0].trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    // processing 409 不得弃键：重试复用同键且走协调器重放成功。
    expect(webRegisterMock).toHaveBeenCalledTimes(2)
    expect(webRegisterMock.mock.calls[1][1].idempotencyKey).toBe(firstKey)
    expect(wrapper.emitted('recovered')).toEqual([[{ platform: 'deepseek', account_id: 77 }]])
  })

  it('discards the key only on a terminal web_credential_duplicate 409 (R5-P2)', async () => {
    webRegisterEmailCodeMock.mockResolvedValue({ success: true, send_window_secs: 60 })
    webRegisterMock.mockRejectedValueOnce({
      status: 409, reason: 'web_credential_duplicate', message: 'duplicate'
    })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-register-password"]').setValue('passw0rd1')

    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    // 终态 409：弃键——下次提交是新键。
    expect(webRegisterMock).toHaveBeenCalledTimes(1)
  })

  it('freezes the account_draft snapshot before outbound so in-flight draft mutation cannot corrupt the retry pair (R5-P3)', async () => {
    webRegisterEmailCodeMock.mockResolvedValue({ success: true, send_window_secs: 60 })
    // 首次提交：网络错误（结果不明，留配对）。
    webRegisterMock.mockRejectedValueOnce(new Error('network down'))
    // 同键重试：成功。
    webRegisterMock.mockResolvedValueOnce({ success: true, account_id: 88 })
    const draft = { ...accountDraft, name: 'original-name' }
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft: draft } })

    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-register-password"]').setValue('passw0rd1')

    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    const firstPayload = webRegisterMock.mock.calls[0][0]
    expect(firstPayload.account_draft.name).toBe('original-name')

    // 在途失败后父组件原地修改草稿对象（模拟 props 对象被变异）。
    draft.name = 'mutated-name'

    // 同键重试：发送的必须是冻结快照（original-name），不是变异后的草稿。
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[0].trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(webRegisterMock).toHaveBeenCalledTimes(2)
    expect(webRegisterMock.mock.calls[1][1].idempotencyKey).toBe(webRegisterMock.mock.calls[0][1].idempotencyKey)
    expect(webRegisterMock.mock.calls[1][0].account_draft.name).toBe('original-name')
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

  // 外审 R6-P1：配对在出站前即登记——首次请求在途期间退出注册模式再进、改邮箱后
  // 提交 B，B 能先比较配对快照判漂移（= 新操作），轮换新键，而不是复用 A 的旧键发
  // 新载荷触发协调器 fingerprint 冲突（IDEMPOTENCY_KEY_CONFLICT）导致弃键。
  it('rotates to a new key when the form drifts while the first register request is still in flight (R6-P1)', async () => {
    webRegisterEmailCodeMock.mockResolvedValue({ success: true, send_window_secs: 60 })
    // 请求 A：手工控制的 pending promise，永不自动 resolve（在途）。
    let resolveA!: (value: { success: boolean; account_id?: number }) => void
    webRegisterMock.mockImplementationOnce(() => new Promise((resolve) => { resolveA = resolve }))
    // 请求 B：明确成功（终态）。
    webRegisterMock.mockResolvedValueOnce({ success: true, account_id: 66 })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-register-password"]').setValue('passw0rd1')

    // 提交 A（在途，键 keyA，配对此刻已登记）。
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(webRegisterMock).toHaveBeenCalledTimes(1)
    const keyA = webRegisterMock.mock.calls[0][1].idempotencyKey
    expect(keyA).toBeTruthy()

    // A 在途时退出注册模式再进（代次递增使 A 过期），改邮箱。
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[0].trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-email"]').setValue('other@deepseek.com')

    // 提交 B：表单字段漂移 = 新操作 → 新键，不与 A 的键冲突。
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(webRegisterMock).toHaveBeenCalledTimes(2)
    expect(webRegisterMock.mock.calls[1][1].idempotencyKey).not.toBe(keyA)
    expect(wrapper.emitted('recovered')).toEqual([[{ platform: 'deepseek', account_id: 66 }]])

    // A 迟到返回：代次已过期，不影响界面状态（配对写点已消失，无从覆盖）。
    resolveA({ success: true, account_id: 1 })
    await flushPromises()
  })

  // 外审 R6-P1 补充面：在途期间退出重进但不改表单 → 漂移比较命中同操作 → 复用同键，
  // 且 account_draft 与首次出站字节一致（出站前登记的正是同操作分支替换后的冻结快照）。
  it('reuses the same key and frozen payload when re-submitting the same operation while the first request is in flight (R6-P1)', async () => {
    webRegisterEmailCodeMock.mockResolvedValue({ success: true, send_window_secs: 60 })
    let resolveA!: (value: { success: boolean; account_id?: number }) => void
    webRegisterMock.mockImplementationOnce(() => new Promise((resolve) => { resolveA = resolve }))
    webRegisterMock.mockResolvedValueOnce({ success: true, account_id: 67 })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-register-password"]').setValue('passw0rd1')

    // 提交 A（在途）。
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(webRegisterMock).toHaveBeenCalledTimes(1)
    const keyA = webRegisterMock.mock.calls[0][1].idempotencyKey
    const payloadA = webRegisterMock.mock.calls[0][0]

    // A 在途时退出再进、不改任何表单字段：重进时回填配对快照（codeSent 视为已发）。
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[0].trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await flushPromises()
    expect((wrapper.find('[data-testid="web-register-email"]').element as HTMLInputElement).value).toBe('user@deepseek.com')

    // 提交 B：同操作 → 同键 + 与首次出站字节一致（jsdom 无 structuredClone，快照
    // 本就以 JSON 深拷贝冻结，比较用同口径）。
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(webRegisterMock).toHaveBeenCalledTimes(2)
    expect(webRegisterMock.mock.calls[1][1].idempotencyKey).toBe(keyA)
    expect(JSON.parse(JSON.stringify(webRegisterMock.mock.calls[1][0]))).toEqual(JSON.parse(JSON.stringify(payloadA)))
    expect(wrapper.emitted('recovered')).toEqual([[{ platform: 'deepseek', account_id: 67 }]])

    resolveA({ success: true, account_id: 1 })
    await flushPromises()
  })

  // 外审 R6-P2：切平台 = 新上下文。结果不明失败留下的键+配对必须在切平台时清空，
  // 否则切走再切回会 enterRegisterMode 回填旧邮箱/验证码并复用旧键重放前一次注册。
  it('clears the register idempotency key and retry pair on platform switch (R6-P2)', async () => {
    webRegisterEmailCodeMock.mockResolvedValue({ success: true, send_window_secs: 60 })
    // 结果不明失败（无 status 网络错误）：键+配对保留。
    webRegisterMock.mockRejectedValueOnce(new Error('network'))
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek', accountDraft } })

    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await wrapper.find('[data-testid="web-register-email"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-register-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-register-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-register-password"]').setValue('passw0rd1')
    await wrapper.find('[data-testid="web-register-submit"]').trigger('click')
    await flushPromises()
    expect(webRegisterMock).toHaveBeenCalledTimes(1)
    expect(newWebRegisterIdempotencyKeyMock).toHaveBeenCalledTimes(1)

    // 切平台（watcher 清注册键+配对），再切回 deepseek。
    await wrapper.setProps({ platform: 'kimi' })
    await flushPromises()
    await wrapper.setProps({ platform: 'deepseek' })
    await flushPromises()

    // 进入注册模式：不得回填旧邮箱/验证码（配对已被清），codeSent=false。
    await wrapper.find('[data-testid="web-register-mode-toggle"]').findAll('button')[1].trigger('click')
    await flushPromises()
    expect((wrapper.find('[data-testid="web-register-email"]').element as HTMLInputElement).value).toBe('')
    expect((wrapper.find('[data-testid="web-register-code"]').element as HTMLInputElement).value).toBe('')
    expect(wrapper.find('[data-testid="web-register-code-sent-hint"]').exists()).toBe(false)
  })
})
