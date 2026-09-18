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
    expect(emitted![0][0]).toEqual({ platform: 'deepseek', cookie: 'ck=123' })
  })

  it('zhipu: uses login_phone and reveals the SMS step on needs_sms', async () => {
    webLoginPasswordMock.mockResolvedValue({ success: true, needs_sms: true, session_token: 'tok' })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu' } })

    await wrapper.find('[data-testid="web-auto-login-identifier"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-password"]').setValue('pw')
    await wrapper.find('[data-testid="web-auto-login-submit"]').trigger('click')
    await flushPromises()

    expect(webLoginPasswordMock).toHaveBeenCalledWith(
      expect.objectContaining({ platform: 'zhipu', login_phone: '13800000000' })
    )
    expect(wrapper.find('[data-testid="web-auto-login-sms"]').exists()).toBe(true)
  })

  it('zhipu: SMS submit shows the honest not-connected error and never fakes success', async () => {
    webLoginPasswordMock.mockResolvedValue({ success: true, needs_sms: true, session_token: 'tok' })
    webLoginSmsMock.mockResolvedValue({ success: false, detail: '短信码登录尚未接入发码通道' })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'zhipu' } })

    await wrapper.find('[data-testid="web-auto-login-identifier"]').setValue('13800000000')
    await wrapper.find('[data-testid="web-auto-login-password"]').setValue('pw')
    await wrapper.find('[data-testid="web-auto-login-submit"]').trigger('click')
    await flushPromises()
    await wrapper.find('[data-testid="web-auto-login-sms-code"]').setValue('123456')
    await wrapper.find('[data-testid="web-auto-login-sms-submit"]').trigger('click')
    await flushPromises()

    expect(webLoginSmsMock).toHaveBeenCalledWith({ session_token: 'tok', sms_code: '123456' })
    expect(wrapper.find('[data-testid="web-auto-login-sms-error"]').text()).toContain(
      '短信码登录尚未接入发码通道'
    )
    // 未伪造成功
    expect(wrapper.emitted('recovered')).toBeFalsy()
  })

  it('surfaces the interceptor flat-object error message verbatim', async () => {
    webLoginPasswordMock.mockRejectedValue({ status: 400, message: 'boom' })
    const wrapper = mount(WebAutoLoginForm, { props: { platform: 'deepseek' } })

    await wrapper.find('[data-testid="web-auto-login-identifier"]').setValue('a@b.com')
    await wrapper.find('[data-testid="web-auto-login-password"]').setValue('pw')
    await wrapper.find('[data-testid="web-auto-login-submit"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="web-auto-login-error"]').text()).toBe('boom')
  })
})
