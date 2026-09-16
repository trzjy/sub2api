import { defineComponent } from 'vue'
import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import PlanEditDialog from '../PlanEditDialog.vue'
import type { AdminGroup } from '@/types'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => {
      if (key === 'payment.admin.storedUsdPreview') return `stored ${params?.amount}`
      return key
    },
  }),
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorSpy,
    showSuccess: vi.fn(),
  }),
}))

const { createPlan, showErrorSpy } = vi.hoisted(() => ({
  createPlan: vi.fn(),
  showErrorSpy: vi.fn(),
}))

vi.mock('@/api/admin/payment', () => ({
  adminPaymentAPI: {
    createPlan,
    updatePlan: vi.fn(),
  },
}))

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: {
    show: Boolean,
    title: String,
    width: String,
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: [String, Number],
    options: {
      type: Array,
      default: () => [],
    },
    placeholder: String,
  },
  emits: ['update:modelValue'],
  setup(_props, { emit }) {
    const onChange = (event: Event) => {
      const value = (event.target as HTMLSelectElement).value
      emit('update:modelValue', value === '' ? null : Number(value))
    }
    return { onChange }
  },
  template: `
    <select
      :value="modelValue ?? ''"
      @change="onChange"
    >
      <option value="">{{ placeholder }}</option>
      <option
        v-for="option in options"
        :key="option.value"
        :value="option.value"
        :data-platform="option.platform"
      >
        {{ option.label }}
      </option>
    </select>
  `,
})

const groupFixture = (overrides: Partial<AdminGroup>): AdminGroup => ({
  id: 1,
  name: 'OpenAI',
  description: null,
  platform: 'openai',
  rate_multiplier: 1,
  rpm_limit: 0,
  is_exclusive: false,
  status: 'active',
  subscription_type: 'subscription',
  daily_limit_usd: null,
  weekly_limit_usd: null,
  monthly_limit_usd: null,
  allow_image_generation: false,
  image_rate_independent: false,
  image_rate_multiplier: 1,
  image_price_1k: null,
  image_price_2k: null,
  image_price_4k: null,
  peak_rate_enabled: false,
  peak_start: '',
  peak_end: '',
  peak_rate_multiplier: 1,
  claude_code_only: false,
  fallback_group_id: null,
  fallback_group_id_on_invalid_request: null,
  allow_messages_dispatch: false,
  require_oauth_only: false,
  require_privacy_set: false,
  created_at: '2026-07-01T00:00:00Z',
  updated_at: '2026-07-01T00:00:00Z',
  model_routing: null,
  model_routing_enabled: false,
  mcp_xml_inject: false,
  sort_order: 0,
  ...overrides,
})

function mountDialog({
  groups = [],
  paymentConfig = null,
}: {
  groups?: AdminGroup[]
  paymentConfig?: Record<string, unknown> | null
} = {}) {
  return mount(PlanEditDialog, {
    props: {
      show: true,
      plan: null,
      groups,
      paymentConfig,
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: SelectStub,
        Icon: true,
        GroupBadge: true,
      },
    },
  })
}

describe('PlanEditDialog', () => {
  it('treats the admin price as CNY and stores USD in the request payload', async () => {
    showErrorSpy.mockClear()
    createPlan.mockClear()
    const wrapper = mountDialog({
      groups: [groupFixture({ id: 1, name: 'G', platform: 'openai' })],
      paymentConfig: { fx_rates: { CNY: 7.15 }, recharge_fee_rate: 2.5 },
    })

    // Fill all required fields
    await wrapper.find('input[type="text"]').setValue('Test Plan')
    await wrapper.get('select').setValue('1')
    await wrapper.find('textarea').setValue('Test description')
    await wrapper.find('input[type="number"]').setValue('9.99')
    await wrapper.find('#plan-form').trigger('submit.prevent')
    await flushPromises()

    expect(wrapper.text()).toContain('stored $1.40')
    expect(showErrorSpy).not.toHaveBeenCalled()
    expect(createPlan).toHaveBeenCalledWith(expect.objectContaining({
      price: 1.4,
      original_price: 0,
    }))
  })

  it('hides the stored USD preview when the CNY FX rate is not configured', async () => {
    const wrapper = mountDialog({ paymentConfig: { fx_rates: {}, recharge_fee_rate: 2.5 } })

    await wrapper.find('input[type="number"]').setValue('9.99')

    expect(wrapper.text()).not.toContain('stored')
  })

  it('converts stored USD price to CNY when editing an existing plan', async () => {
    const existingPlan = {
      id: 42,
      name: 'Test Plan',
      group_id: 1,
      description: 'desc',
      price: 1.4,        // stored as USD
      original_price: 2.0, // stored as USD
      currency: 'USD',
      validity_days: 1,
      validity_unit: 'days',
      sort_order: 0,
      for_sale: true,
      features: [],
      status: 'active',
      group: { id: 1, name: 'G', platform: 'openai', rate_multiplier: 1 },
    } as any
    const wrapper = mount(PlanEditDialog, {
      props: { show: true, plan: existingPlan, groups: [groupFixture({ id: 1, name: 'G', platform: 'openai' })], paymentConfig: { fx_rates: { CNY: 7.15 } } },
      global: { stubs: { BaseDialog: BaseDialogStub, Select: SelectStub, Icon: true, GroupBadge: true } },
    })

    // price input (first number input) should show CNY value = 1.4 * 7.15 ≈ 10.01
    const priceInput = wrapper.find('input[type="number"]')
    expect(priceInput.element.value).toBe('10.01')
  })

  it('allows composite subscription groups for payment plans', () => {
    const wrapper = mountDialog({
      groups: [
        groupFixture({
          id: 10,
          name: 'OpenAI + Claude + Gemini + Grok',
          platform: 'composite',
          rate_multiplier: 1.2,
          subscription_type: 'subscription',
        }),
        groupFixture({
          id: 11,
          name: 'Standard OpenAI',
          platform: 'openai',
          subscription_type: 'standard',
        }),
      ],
    })

    const options = wrapper.findAll('option').map(option => option.text())

    expect(options).toContain('OpenAI + Claude + Gemini + Grok — composite (1.2x)')
    expect(options).not.toContain('Standard OpenAI — openai (1x)')
  })
})
