import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import PlazaMergedTable from '../PlazaMergedTable.vue'
import type { PlazaModel } from '@/api/modelPlaza'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
  createI18n: () => ({}),
}))

function model(overrides: Partial<PlazaModel> = {}): PlazaModel {
  return {
    name: 'claude-fable-5',
    platform: 'anthropic',
    pricing: {
      billing_mode: 'token',
      input_price: 1e-5,
      output_price: 5e-5,
      cache_write_price: 1.25e-5,
      cache_read_price: 1e-6,
      image_input_price: null,
      image_output_price: null,
      per_request_price: null,
      intervals: []
    },
    official_pricing: {
      input_price: 1e-5,
      output_price: 5e-5,
      cache_write_price: 1.25e-5,
      cache_read_price: 1e-6
    },
    ...overrides
  } as PlazaModel
}

function mountTable(rows: Parameters<typeof PlazaMergedTable.setup>[0] extends never ? never : any[]) {
  return mount(PlazaMergedTable, {
    props: { rows, platform: '' },
    global: {
      stubs: {
        GroupBadge: { props: ['name'], template: '<span class="group-badge">{{ name }}</span>' },
        PlatformIcon: true
      }
    }
  })
}

describe('PlazaMergedTable 折扣徽章', () => {
  it('徽章口径 = 折后实付 ÷ 官方价(乘分组倍率):0.3 倍率显示 3折,而非渠道价对比出的 10折', () => {
    const wrapper = mountTable([
      {
        key: 'claude-fable-5',
        model: model(),
        entries: [
          { groupId: 1, groupName: 'claude kiro', rate: 0.3, model: model() },
          { groupId: 2, groupName: 'claude-max(满血)', rate: 0.95, model: model() }
        ]
      }
    ])
    const text = wrapper.text()
    // claude kiro: $3/$10 = 3折
    expect(text).toContain('3折')
    // claude-max: $9.5/$10 = 9.5折
    expect(text).toContain('9.5折')
    // 不应出现未乘倍率的错误口径 10折
    expect(text).not.toContain('10折')
  })

  it('原价(倍率 1)不渲染徽章', () => {
    const wrapper = mountTable([
      {
        key: 'claude-fable-5',
        model: model(),
        entries: [{ groupId: 3, groupName: 'claude 不限量', rate: 1, model: model() }]
      }
    ])
    expect(wrapper.text()).not.toContain('折')
  })
})
