import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

const routeState = vi.hoisted(() => ({
  query: {} as Record<string, unknown>,
}))

vi.mock('vue-router', async () => {
  const actual = await vi.importActual<typeof import('vue-router')>('vue-router')
  return {
    ...actual,
    useRoute: () => routeState,
  }
})

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

import StripePopupView from '../StripePopupView.vue'
import { currencySymbol } from '@/utils/money'

describe('StripePopupView currency display', () => {
  beforeEach(() => {
    routeState.query = {}
  })

  it('renders the amount with the currency from the query parameter (regression for the ¥ bug)', async () => {
    routeState.query = {
      order_id: '42',
      method: 'alipay',
      amount: '100',
      currency: 'HKD',
    }

    const wrapper = mount(StripePopupView)
    await flushPromises()

    expect(wrapper.text()).toContain(currencySymbol('HKD'))
    expect(wrapper.text()).toContain('100')
    expect(wrapper.text()).not.toContain('¥100')
  })

  it('falls back to CNY when the currency query parameter is missing', async () => {
    routeState.query = {
      order_id: '42',
      method: 'alipay',
      amount: '88',
    }

    const wrapper = mount(StripePopupView)
    await flushPromises()

    expect(wrapper.text()).toContain(currencySymbol('CNY'))
    expect(wrapper.text()).toContain('88')
  })

  it('renders USD symbol for USD currency', async () => {
    routeState.query = {
      order_id: '42',
      method: 'alipay',
      amount: '10',
      currency: 'USD',
    }

    const wrapper = mount(StripePopupView)
    await flushPromises()

    expect(wrapper.text()).toContain(currencySymbol('USD'))
    expect(wrapper.text()).toContain('10')
  })
})