import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

const {
  webLoginPasswordMock,
  webLoginSmsMock
} = vi.hoisted(() => ({
  webLoginPasswordMock: vi.fn(),
  webLoginSmsMock: vi.fn()
}))

vi.mock('@/api/admin/webAutoLogin', () => ({
  webLoginPassword: webLoginPasswordMock,
  webLoginSms: webLoginSmsMock
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

import WebAutoLoginForm from '../WebAutoLoginForm.vue'

describe('WebAutoLoginForm', () => {
  it('deepseek: submits with login_email and emits recovered with the cookie', async () => {
    webLoginPasswordMock.mockResolvedValue({ success: true, cookie: 'ck=123' })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek' } })

    await wrapper.find('[data-testid="web-auto-login-identifier"]').setValue('user@deepseek.com')
    await wrapper.find('[data-testid="web-auto-login-password"]').setValue('pw')
    await wrapper.find('[data-testid="web-auto-login-submit"]').trigger('click')
    await flushPromises()

    expect(webLoginPasswordMock).toHaveBeenCalledWith(
      expect.objectContaining({
        platform: 'deepseek',
        login_email: 'user@deepseek.com',
        login_password: 'pw'
      })
    )
    const emitted = wrapper.emitted('recovered')
    expect(emitted).toBeTruthy()
    expect(emitted![0][0]).toEqual({
      platform: 'deepseek',
      cookie: 'ck=123',
      login_email: 'user@deepseek.com',
      login_password: 'pw'
    })
  })

  it('zhipu: send_code reveals the SMS code step and never uses a session_token', async () => {
    webLoginSmsMock.mockResolvedValue({ success: true })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu' } })

    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()

    expect(webLoginSmsMock).toHaveBeenCalledWith(
      expect.objectContaining({ action: 'send_code', platform: 'zhipu', phone: '13800000000' })
    )
    // 无 session_token 概念（E0 取证）：发码请求不应携带会话令牌字段。
    expect(webLoginSmsMock).not.toHaveBeenCalledWith(expect.objectContaining({ session_token: expect.anything() }))
    expect(wrapper.find('[data-testid="web-auto-login-sms-code"]').exists()).toBe(true)
  })

  it('zhipu: send_code needs_challenge shows the challenge banner and does not fake success', async () => {
    webLoginSmsMock.mockRejectedValue({
      status: 400,
      code: 400,
      message: '人机验证',
      metadata: { hint: '请在浏览器完成滑块验证后回填上方挑战字段', needs_challenge: 'true' }
    })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu' } })

    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="web-auto-login-challenge"]').exists()).toBe(true)
    // 新提示：引导用户在自有浏览器完成验证后把回传值回填上方挑战字段（不再使用旧 helper 文案）。
    expect(wrapper.find('[data-testid="web-auto-login-challenge"]').text()).toContain('挑战')
    expect(wrapper.find('[data-testid="web-auto-login-challenge"]').text()).toContain('回填')
    // 挑战失败不伪造成功，且保留重试入口（send-code 按钮仍在）。
    expect(wrapper.emitted('recovered')).toBeFalsy()
    expect(wrapper.find('[data-testid="web-auto-login-send-code"]').exists()).toBe(true)
  })

  it('zhipu: login success emits recovered with account_id and cookie', async () => {
    webLoginSmsMock.mockImplementation(async (req: { action: string }) => {
      if (req.action === 'send_code') return { success: true }
      return { success: true, account_id: 7, cookie: 'zck=1' }
    })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu' } })

    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-auto-login-sms-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-auto-login-sms-submit"]').trigger('click')
    await flushPromises()

    expect(webLoginSmsMock).toHaveBeenLastCalledWith(
      expect.objectContaining({ action: 'login', platform: 'zhipu', phone: '13800000000', sms_code: '123456' })
    )
    const emitted = wrapper.emitted('recovered')
    expect(emitted).toBeTruthy()
    expect(emitted![0][0]).toEqual({ platform: 'zhipu', account_id: 7, cookie: 'zck=1' })
  })

  it('kimi: login success emits recovered with account_id and access_token', async () => {
    webLoginSmsMock.mockImplementation(async (req: { action: string }) => {
      if (req.action === 'send_code') return { success: true }
      return { success: true, account_id: 8, access_token: 'atk' }
    })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'kimi' } })

    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000001')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-auto-login-sms-code"]').setValue('654321')
    await wrapper.find('[data-testid="web-auto-login-sms-submit"]').trigger('click')
    await flushPromises()

    const emitted = wrapper.emitted('recovered')
    expect(emitted).toBeTruthy()
    expect(emitted![0][0]).toEqual({ platform: 'kimi', account_id: 8, access_token: 'atk' })
  })

  it('zhipu: send_code non-challenge error surfaces the flat interceptor message verbatim', async () => {
    webLoginSmsMock.mockRejectedValue({ status: 400, message: '手机号格式不正确' })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu' } })

    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('abc')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="web-auto-login-error"]').text()).toBe('手机号格式不正确')
    expect(wrapper.emitted('recovered')).toBeFalsy()
  })

  it('surfaces the interceptor flat-object error message verbatim for deepseek password', async () => {
    webLoginPasswordMock.mockRejectedValue({ status: 400, message: 'boom' })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek' } })

    await wrapper.find('[data-testid="web-auto-login-identifier"]').setValue('a@b.com')
    await wrapper.find('[data-testid="web-auto-login-password"]').setValue('pw')
    await wrapper.find('[data-testid="web-auto-login-submit"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="web-auto-login-error"]').text()).toBe('boom')
  })

  it('zhipu: send_code echoes captcha challenge values (rid/md5/phone_code) back in the request', async () => {
    webLoginSmsMock.mockResolvedValue({ success: true })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu' } })

    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-zhipu-rid"]').setValue('rid-abc')
    await wrapper.find('[data-testid="web-auto-login-zhipu-md5"]').setValue('md5-xyz')
    await wrapper.find('[data-testid="web-auto-login-zhipu-phone-code"]').setValue('86')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()

    expect(webLoginSmsMock).toHaveBeenCalledWith(
      expect.objectContaining({
        action: 'send_code',
        platform: 'zhipu',
        phone: '13800000000',
        zhipu_captcha_rid: 'rid-abc',
        zhipu_captcha_md5: 'md5-xyz',
        zhipu_phone_code: '86'
      })
    )
  })

  it('kimi: send_code echoes captcha validate value back in the request', async () => {
    webLoginSmsMock.mockResolvedValue({ success: true })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'kimi' } })

    await wrapper.find('[data-testid="web-auto-login-phone"]').setValue('13800000001')
    await wrapper.find('[data-testid="web-auto-login-kimi-validate"]').setValue('yidun-validate-1')
    await wrapper.find('[data-testid="web-auto-login-send-code"]').trigger('click')
    await flushPromises()

    expect(webLoginSmsMock).toHaveBeenCalledWith(
      expect.objectContaining({
        action: 'send_code',
        platform: 'kimi',
        phone: '13800000001',
        kimi_captcha_validate: 'yidun-validate-1'
      })
    )
  })
})
