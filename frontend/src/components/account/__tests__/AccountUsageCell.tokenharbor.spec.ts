import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account } from '@/types'

const { getUsage } = vi.hoisted(() => ({
  getUsage: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getUsage
    }
  }
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

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'kimi',
    type: 'apikey',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: '2026-03-15T00:00:00Z',
    updated_at: '2026-03-15T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  }
}

// TokenHarbor 分支只渲染用量窗口限流行，CN 配额/余额子单元格不适用，全部 stub。
const tokenHarborStubs = {
  OllamaCloudUsageCell: {
    props: ['account'],
    template: '<div data-test="embedded-ollama">ollama</div>'
  },
  CNProviderQuotaCell: {
    template: '<div data-test="cn-quota-cell" />'
  },
  CNProviderBalanceCell: {
    template: '<div data-test="cn-balance-cell" />'
  },
  // 真实 UsageProgressBar 依赖 vueuse 计时器，这里 stub 成仅回显 label 的轻量组件，
  // 用 data-test + data-label 断言渲染了哪些限流行。
  UsageProgressBar: {
    props: ['label', 'utilization', 'unknownUsage', 'resetsAt', 'resetsAtPrefix', 'color'],
    template:
      '<div data-test="usage-progress-bar" :data-label="label" :data-prefix="resetsAtPrefix">{{ label }}</div>'
  }
}

const FUTURE = '2099-07-28T00:00:00Z'
const PAST = '2020-07-28T00:00:00Z'

describe('AccountUsageCell TokenHarbor rate-limit rows', () => {
  it('TokenHarbor 账号：仅渲染未过期的模型限流为 7d 用量窗口行，且不含 CN 子单元格', () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          credentials: { base_url: 'https://api.tokenharbor.ai/v1' },
          extra: {
            model_rate_limits: {
              'deepseek-v4.1-flash:free': {
                rate_limited_at: PAST,
                rate_limit_reset_at: FUTURE
              },
              'another-model': {
                rate_limited_at: PAST,
                rate_limit_reset_at: PAST // 已过期，应被排除
              }
            }
          }
        })
      },
      global: {
        stubs: tokenHarborStubs
      }
    })

    const bars = wrapper.findAll('[data-test="usage-progress-bar"]')
    expect(bars).toHaveLength(1)
    expect(bars[0].attributes('data-label')).toBe('7d')
    // 倒计时带 "限流至" 前缀（i18n 插值，mock 下只回显 key），可见行不含模型名
    expect(bars[0].attributes('data-prefix')).toBe('admin.accounts.tokenHarbor.rateLimitedPrefix')
    expect(bars[0].text()).not.toContain('deepseek-v4.1-flash:free')

    // CN 配额/余额子单元格不应渲染
    expect(wrapper.find('[data-test="cn-quota-cell"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-balance-cell"]').exists()).toBe(false)

    // title 绑定使用 "限流至 {time}" 文案 key（不含模型名，i18n 插值 mock 下只回显 key）
    const titleEl = wrapper.find('[title]')
    expect(titleEl.exists()).toBe(true)
    expect(titleEl.attributes('title')).toBe('admin.accounts.tokenHarbor.rateLimitedUntil')
    // 限流行数量与 label 正确
    expect(bars[0].text()).toContain('7d')
  })

  it('TokenHarbor 账号无任何限流时回落到占位符 "-"', () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          credentials: { base_url: 'https://api.tokenharbor.ai/v1' },
          extra: {}
        })
      },
      global: {
        stubs: tokenHarborStubs
      }
    })

    expect(wrapper.findAll('[data-test="usage-progress-bar"]')).toHaveLength(0)
    expect(wrapper.text()).toContain('-')
  })

  it('非 TokenHarbor 的 CN 账号仍走 CN 子单元格，不渲染 TokenHarbor 行', () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          credentials: { base_url: 'https://api.moonshot.cn/v1' },
          extra: {
            model_rate_limits: {
              'some-model': {
                rate_limited_at: PAST,
                rate_limit_reset_at: FUTURE
              }
            }
          }
        })
      },
      global: {
        stubs: tokenHarborStubs
      }
    })

    // 非 TokenHarbor：渲染 CN 配额/余额子单元格
    expect(wrapper.find('[data-test="cn-quota-cell"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="cn-balance-cell"]').exists()).toBe(true)
    // 不应渲染 TokenHarbor 限流行
    expect(wrapper.findAll('[data-test="usage-progress-bar"]')).toHaveLength(0)
  })
})
