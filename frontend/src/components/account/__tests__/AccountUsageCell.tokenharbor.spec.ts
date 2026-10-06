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
      '<div data-test="usage-progress-bar" :data-label="label" :data-prefix="resetsAtPrefix" :data-resets-at="resetsAt">{{ label }}</div>'
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
                rate_limit_reset_at: FUTURE,
                precise_reset: true // 权威恢复标记：未过期，显示倒计时
              },
              'another-model': {
                rate_limited_at: PAST,
                rate_limit_reset_at: PAST, // 已过期 + precise_reset=true → 按标准到期剔除
                precise_reset: true
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

  // E44：precise_reset 缺失（D4c 前存量旧行）与 false 同为无信号语义——即使 reset_at
  // 在将来，也不得显示倒计时（后端把缺失按 false 处理，前端一致）。
  it('precise_reset 缺失（未写字段）即无信号：不显示倒计时、显示"等待主动复探"', () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          credentials: { base_url: 'https://api.tokenharbor.ai/v1' },
          extra: {
            model_rate_limits: {
              'deepseek-v4.1-flash:free': {
                rate_limited_at: PAST,
                // 缺失字段，即使 reset_at 在将来也属无信号（与 false 同义）
                rate_limit_reset_at: FUTURE
              }
            }
          }
        })
      },
      global: { stubs: tokenHarborStubs }
    })

    const bars = wrapper.findAll('[data-test="usage-progress-bar"]')
    expect(bars).toHaveLength(1)
    // 不显示倒计时：resets-at 未传入
    expect(bars[0].attributes('data-resets-at')).toBeUndefined()
    // 不加"限流至"前缀
    expect(bars[0].attributes('data-prefix')).toBe('')
    // 显示"等待主动复探"
    const awaiting = wrapper.find('[data-test="tokenharbor-awaiting-probe"]')
    expect(awaiting.exists()).toBe(true)
    expect(awaiting.text()).toBe('admin.accounts.tokenHarbor.awaitingProbe')
  })

  it('precise_reset === true 时倒计时行为不变', () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          credentials: { base_url: 'https://api.tokenharbor.ai/v1' },
          extra: {
            model_rate_limits: {
              'deepseek-v4.1-flash:free': {
                rate_limited_at: PAST,
                rate_limit_reset_at: FUTURE,
                precise_reset: true
              }
            }
          }
        })
      },
      global: { stubs: tokenHarborStubs }
    })

    const bars = wrapper.findAll('[data-test="usage-progress-bar"]')
    expect(bars).toHaveLength(1)
    expect(bars[0].attributes('data-resets-at')).toBe(FUTURE)
    expect(bars[0].attributes('data-prefix')).toBe('admin.accounts.tokenHarbor.rateLimitedPrefix')
    expect(wrapper.find('[data-test="tokenharbor-awaiting-probe"]').exists()).toBe(false)
  })

  it('precise_reset === false：不渲染倒计时、无"限流至"前缀，渲染"等待主动复探"', () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          credentials: { base_url: 'https://api.tokenharbor.ai/v1' },
          extra: {
            model_rate_limits: {
              'deepseek-v4.1-flash:free': {
                rate_limited_at: PAST,
                // 无信号哨兵占位值（远未来），不得被当作恢复时刻展示
                rate_limit_reset_at: FUTURE,
                precise_reset: false
              }
            }
          }
        })
      },
      global: { stubs: tokenHarborStubs }
    })

    const bars = wrapper.findAll('[data-test="usage-progress-bar"]')
    expect(bars).toHaveLength(1)
    // 不显示倒计时：resets-at 未传入
    expect(bars[0].attributes('data-resets-at')).toBeUndefined()
    // 不加"限流至"前缀
    expect(bars[0].attributes('data-prefix')).toBe('')
    // 显示"等待主动复探"
    const awaiting = wrapper.find('[data-test="tokenharbor-awaiting-probe"]')
    expect(awaiting.exists()).toBe(true)
    expect(awaiting.text()).toBe('admin.accounts.tokenHarbor.awaitingProbe')
    // title 亦不得声称"限流至 <哨兵时间>"
    expect(wrapper.find('[title]').attributes('title')).toBe('admin.accounts.tokenHarbor.awaitingProbe')
  })

  // E42 无信号哨兵不吃到期剔除（D5 锁定）：precise_reset === false 的过期条目仍渲染
  // "等待主动复探"，不得因 reset_at 已过而被剔除。
  it('precise_reset === false 且已过期：仍渲染"等待主动复探"（不吃到期剔除）', () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          credentials: { base_url: 'https://api.tokenharbor.ai/v1' },
          extra: {
            model_rate_limits: {
              'deepseek-v4.1-flash:free': {
                rate_limited_at: PAST,
                // 已过期哨兵占位值，须仍渲染等待复探
                rate_limit_reset_at: PAST,
                precise_reset: false
              }
            }
          }
        })
      },
      global: { stubs: tokenHarborStubs }
    })

    const bars = wrapper.findAll('[data-test="usage-progress-bar"]')
    expect(bars).toHaveLength(1)
    // 不显示倒计时：resets-at 未传入
    expect(bars[0].attributes('data-resets-at')).toBeUndefined()
    // 不加"限流至"前缀
    expect(bars[0].attributes('data-prefix')).toBe('')
    // 显示"等待主动复探"
    const awaiting = wrapper.find('[data-test="tokenharbor-awaiting-probe"]')
    expect(awaiting.exists()).toBe(true)
    expect(awaiting.text()).toBe('admin.accounts.tokenHarbor.awaitingProbe')
  })

  // E44：缺失 precise_reset（D4c 前存量旧行）与 false 同为无信号语义，已过期仍渲染
  // "等待主动复探"，不显示倒计时——与后端把缺失按 false 处理逐位对齐。
  it('precise_reset 缺失且已过期：仍渲染"等待主动复探"（不显示倒计时，缺失=无信号）', () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          credentials: { base_url: 'https://api.tokenharbor.ai/v1' },
          extra: {
            model_rate_limits: {
              'deepseek-v4.1-flash:free': {
                rate_limited_at: PAST,
                // 已过期、且未写 precise_reset 字段（存量旧行），须仍渲染等待复探
                rate_limit_reset_at: PAST
              }
            }
          }
        })
      },
      global: { stubs: tokenHarborStubs }
    })

    const bars = wrapper.findAll('[data-test="usage-progress-bar"]')
    expect(bars).toHaveLength(1)
    // 不显示倒计时：resets-at 未传入
    expect(bars[0].attributes('data-resets-at')).toBeUndefined()
    // 不加"限流至"前缀
    expect(bars[0].attributes('data-prefix')).toBe('')
    // 显示"等待主动复探"
    const awaiting = wrapper.find('[data-test="tokenharbor-awaiting-probe"]')
    expect(awaiting.exists()).toBe(true)
    expect(awaiting.text()).toBe('admin.accounts.tokenHarbor.awaitingProbe')
    // title 亦不得声称"限流至 <占位时间>"
    expect(wrapper.find('[title]').attributes('title')).toBe('admin.accounts.tokenHarbor.awaitingProbe')
  })

  // E42 平行路径一致：precise_reset === true 的过期条目仍按标准行为剔除（不渲染）。
  it('precise_reset === true 且已过期：不渲染（标准到期剔除不变）', () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          credentials: { base_url: 'https://api.tokenharbor.ai/v1' },
          extra: {
            model_rate_limits: {
              'deepseek-v4.1-flash:free': {
                rate_limited_at: PAST,
                rate_limit_reset_at: PAST,
                precise_reset: true
              }
            }
          }
        })
      },
      global: { stubs: tokenHarborStubs }
    })

    // 过期权威标记被剔除，无用量行
    expect(wrapper.findAll('[data-test="usage-progress-bar"]')).toHaveLength(0)
    expect(wrapper.find('[data-test="tokenharbor-awaiting-probe"]').exists()).toBe(false)
    // 回落占位符
    expect(wrapper.text()).toContain('-')
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
