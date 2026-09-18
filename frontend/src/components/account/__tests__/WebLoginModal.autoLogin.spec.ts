import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, ref } from 'vue'

const createWebLoginProxySessionMock = vi.fn().mockRejectedValue(new Error('no proxy'))

vi.mock('@/api/admin/accounts', () => ({
  createWebLoginProxySession: (...args: unknown[]) => createWebLoginProxySessionMock(...args),
  deleteWebLoginProxySession: vi.fn().mockResolvedValue(undefined),
  validateWebCredentials: vi.fn().mockResolvedValue(true)
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ cachedPublicSettings: null })
}))
vi.mock('@/composables/useWebLoginCapture', () => ({
  useWebLoginCapture: () => ({
    capturing: ref(false),
    capturedCookie: ref(''),
    timedOut: ref(false),
    start: vi.fn(),
    stop: vi.fn()
  })
}))
vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))
vi.mock('@/components/common/BaseDialog.vue', () => ({
  default: { template: '<div><slot /></div>' }
}))
vi.mock('@/components/account/WebAutoLoginForm.vue', () => ({
  default: { template: '<div data-testid="web-auto-login-form"></div>' }
}))

import WebLoginModal from '../WebLoginModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /></div>'
})

function mountModal(platform: string) {
  return mount(WebLoginModal, {
    props: { show: true, platform },
    global: { stubs: { BaseDialog: BaseDialogStub, teleport: true } }
  })
}

async function flush() {
  await flushPromises()
  await flushPromises()
  await flushPromises()
}

describe('WebLoginModal auto-login tab', () => {
  it('opens on the Cookie Capture tab by default', async () => {
    const wrapper = mountModal('deepseek')
    await flush()
    expect(wrapper.find('[data-testid="web-login-tab-capture"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="web-login-open-new-tab"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="web-auto-login-form"]').exists()).toBe(false)
  })

  it('switches to the Auto Login tab and renders the auto-login form', async () => {
    const wrapper = mountModal('deepseek')
    await flush()
    await wrapper.find('[data-testid="web-login-tab-auto"]').trigger('click')
    await flush()
    expect(wrapper.find('[data-testid="web-auto-login-form"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="web-login-open-new-tab"]').exists()).toBe(false)
  })

  it('resets to the capture tab when the modal is reopened', async () => {
    const wrapper = mountModal('deepseek')
    await flush()
    await wrapper.find('[data-testid="web-login-tab-auto"]').trigger('click')
    await flush()
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flush()
    expect(wrapper.find('[data-testid="web-auto-login-form"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="web-login-open-new-tab"]').exists()).toBe(true)
  })
})
