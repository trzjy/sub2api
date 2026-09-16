import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

// 发起方断言：StripePaymentInline 构造 /payment/stripe-popup 弹窗 URL 时必须透传
// currency（StripePopupView 按 route.query.currency 渲染符号，缺省会回落 CNY）。
// StripePopupView.spec.ts 只测弹窗页自身（mock query），盖不住这条端到端链路。

const resolveSpy = vi.hoisted(() => vi.fn())
const windowOpen = vi.hoisted(() => vi.fn())
const loadStripe = vi.hoisted(() => vi.fn())
const stripeElements = vi.hoisted(() => ({ create: vi.fn() }))
const stripePaymentElement = vi.hoisted(() => ({ mount: vi.fn(), on: vi.fn() }))
const stripeInstance = vi.hoisted(() => ({ elements: vi.fn(), confirmPayment: vi.fn() }))

vi.mock('vue-router', async () => {
  const actual = await vi.importActual<typeof import('vue-router')>('vue-router')
  return {
    ...actual,
    useRouter: () => ({ resolve: resolveSpy }),
  }
})

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

vi.mock('@/stores', () => ({
  useAppStore: () => ({ showError: vi.fn() }),
}))

vi.mock('@/api/payment', () => ({
  paymentAPI: { cancelOrder: vi.fn() },
}))

vi.mock('@stripe/stripe-js/pure', () => ({ loadStripe }))

import StripePaymentInline from '../StripePaymentInline.vue'

function mountInline(extraProps: Record<string, unknown> = {}) {
  return mount(StripePaymentInline, {
    props: {
      orderId: 42,
      amount: 13.99,
      clientSecret: 'pi_secret_42',
      publishableKey: 'pk_test',
      payAmount: 103,
      ...extraProps,
    },
    global: { stubs: { Icon: true } },
  })
}

async function openPopupViaAlipay(wrapper: ReturnType<typeof mountInline>) {
  await flushPromises()
  const changeCallback = stripePaymentElement.on.mock.calls.find(([event]) => event === 'change')?.[1] as
    | ((event: { value: { type: string } }) => void)
    | undefined
  expect(changeCallback).toBeDefined()
  changeCallback!({ value: { type: 'alipay' } })
  await wrapper.find('button.btn-stripe').trigger('click')
  await flushPromises()
}

describe('StripePaymentInline popup URL currency passthrough', () => {
  beforeEach(() => {
    resolveSpy.mockReset().mockImplementation((route: { query: Record<string, string> }) => ({
      href: '/payment/stripe-popup?' + new URLSearchParams(route.query).toString(),
    }))
    windowOpen.mockReset().mockReturnValue({} as Window)
    vi.spyOn(window, 'open').mockImplementation(windowOpen)
    loadStripe.mockReset().mockResolvedValue(stripeInstance)
    stripeInstance.elements.mockReset().mockReturnValue(stripeElements)
    stripeElements.create.mockReset().mockReturnValue(stripePaymentElement)
    stripePaymentElement.mount.mockReset()
    stripePaymentElement.on.mockReset().mockImplementation((event: string, callback: () => void) => {
      if (event === 'ready') callback()
    })
  })

  it('passes the order currency into the popup URL query (regression for the ¥ bug)', async () => {
    const wrapper = mountInline({ currency: 'HKD' })
    await openPopupViaAlipay(wrapper)

    expect(resolveSpy).toHaveBeenCalledTimes(1)
    expect(resolveSpy.mock.calls[0][0].query).toMatchObject({
      order_id: '42',
      method: 'alipay',
      amount: '103',
      currency: 'HKD',
    })
    expect(windowOpen).toHaveBeenCalledWith(
      expect.stringContaining('currency=HKD'),
      'paymentPopup',
      expect.any(String),
    )
    expect(wrapper.emitted('redirect')?.[0]).toEqual([42, expect.stringContaining('currency=HKD')])
  })

  it('falls back to CNY in the popup URL when the currency prop is absent', async () => {
    const wrapper = mountInline()
    await openPopupViaAlipay(wrapper)

    expect(resolveSpy.mock.calls[0][0].query.currency).toBe('CNY')
    expect(windowOpen).toHaveBeenCalledWith(expect.stringContaining('currency=CNY'), 'paymentPopup', expect.any(String))
  })
})
