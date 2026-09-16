import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import WebLoginModal from '../WebLoginModal.vue'

const { validateWebCredentialsMock } = vi.hoisted(() => ({
  validateWebCredentialsMock: vi.fn()
}))

vi.mock('@/api/admin/accounts', () => ({
  validateWebCredentials: validateWebCredentialsMock
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({})
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

function mountModal(platform: 'web-deepseek' | 'web-zhipu' | 'web-kimi' = 'web-zhipu', show = true) {
  return mount(WebLoginModal, {
    props: { show, platform },
    global: {
      stubs: { BaseDialog: BaseDialogStub, teleport: true }
    }
  })
}

describe('WebLoginModal', () => {
  beforeEach(() => {
    validateWebCredentialsMock.mockReset()
    validateWebCredentialsMock.mockResolvedValue(true)
  })

  it('renders cookie textarea for cookie platforms and token JSON for kimi', () => {
    const zhipu = mountModal('web-zhipu')
    expect(zhipu.find('[data-testid="web-login-cookie-input"]').exists()).toBe(true)
    expect(zhipu.find('[data-testid="web-login-token-json"]').exists()).toBe(false)
    zhipu.unmount()

    const kimi = mountModal('web-kimi')
    expect(kimi.find('[data-testid="web-login-token-json"]').exists()).toBe(true)
    expect(kimi.find('[data-testid="web-login-cookie-input"]').exists()).toBe(false)
  })

  it('shows iframe section for non-kimi platforms only', () => {
    const zhipu = mountModal('web-zhipu')
    expect(zhipu.find('[data-testid="web-login-iframe-wrap"]').exists()).toBe(true)
    zhipu.unmount()

    const kimi = mountModal('web-kimi')
    expect(kimi.find('[data-testid="web-login-iframe-wrap"]').exists()).toBe(false)
    expect(kimi.find('[data-testid="web-login-kimi-open"]').exists()).toBe(true)
  })

  it('shows empty-input error without calling backend', async () => {
    const wrapper = mountModal('web-zhipu')
    await wrapper.find('[data-testid="web-login-submit"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="web-login-validation-error"]').exists()).toBe(true)
    expect(validateWebCredentialsMock).not.toHaveBeenCalled()
  })

  it('validates cookie paste and emits applied + close on success', async () => {
    const wrapper = mountModal('web-zhipu')
    await wrapper.find('[data-testid="web-login-cookie-input"]').setValue('sessionid=abc; waf_cookie=def')
    await wrapper.find('[data-testid="web-login-submit"]').trigger('click')
    await flushPromises()

    expect(validateWebCredentialsMock).toHaveBeenCalledWith('web-zhipu', expect.objectContaining({ cookie: 'sessionid=abc; waf_cookie=def' }))
    expect(wrapper.emitted('applied')).toBeTruthy()
    expect(wrapper.emitted('applied')![0][0]).toEqual({
      platform: 'web-zhipu',
      credentials: { cookie: 'sessionid=abc; waf_cookie=def' }
    })
    expect(wrapper.emitted('close')).toBeTruthy()
  })

  it('validates kimi token JSON and emits built credentials', async () => {
    const wrapper = mountModal('web-kimi')
    await wrapper.find('[data-testid="web-login-token-json"]').setValue('{"access_token":"tok","refresh_token":"rt","user_id":"u1"}')
    await wrapper.find('[data-testid="web-login-submit"]').trigger('click')
    await flushPromises()

    expect(validateWebCredentialsMock).toHaveBeenCalledWith('web-kimi', expect.objectContaining({ access_token: 'tok' }))
    expect(wrapper.emitted('applied')![0][0]).toEqual({
      platform: 'web-kimi',
      credentials: { access_token: 'tok', refresh_token: 'rt', user_id: 'u1' }
    })
    expect(wrapper.emitted('close')).toBeTruthy()
  })

  it('shows validation error and does not emit applied when backend rejects', async () => {
    validateWebCredentialsMock.mockResolvedValue(false)
    const wrapper = mountModal('web-zhipu')
    await wrapper.find('[data-testid="web-login-cookie-input"]').setValue('sessionid=abc')
    await wrapper.find('[data-testid="web-login-submit"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="web-login-validation-error"]').exists()).toBe(true)
    expect(wrapper.emitted('applied')).toBeFalsy()
    expect(wrapper.emitted('close')).toBeFalsy()
  })

  it('does not emit close while validating', async () => {
    let resolveValidate: (v: boolean) => void = () => {}
    validateWebCredentialsMock.mockReturnValue(new Promise<boolean>((r) => { resolveValidate = r }))
    const wrapper = mountModal('web-zhipu')
    await wrapper.find('[data-testid="web-login-cookie-input"]').setValue('sessionid=abc')
    await wrapper.find('[data-testid="web-login-submit"]').trigger('click')
    await wrapper.vm.$nextTick()

    await wrapper.find('[data-testid="web-login-submit"]').trigger('click')
    expect(wrapper.emitted('close')).toBeFalsy()
    resolveValidate(true)
    await flushPromises()
    expect(wrapper.emitted('applied')).toBeTruthy()
  })

  it('resets state when reopened', async () => {
    const wrapper = mountModal('web-zhipu', false)
    await wrapper.setProps({ show: true })
    await wrapper.find('[data-testid="web-login-cookie-input"]').setValue('sessionid=abc')
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    expect((wrapper.find('[data-testid="web-login-cookie-input"]').element as HTMLTextAreaElement).value).toBe('')
  })
})
