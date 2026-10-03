import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account } from '@/types'

// muse 分支纯前端消费：usage 数据由父级批量用量注入（batchedUsage），
// 不依赖 getUsage。本文件只覆盖 AccountUsageCell 渲染分支与语义。
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
      // 简单插值：把参数序列化进返回值，便于断言恢复时间确实被传入
      t: (key: string, params?: Record<string, unknown>) =>
        params && Object.keys(params).length ? `${key}::${JSON.stringify(params)}` : key
    })
  }
})

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'muse',
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

const usageBarStub = {
  props: ['label', 'utilization', 'resetsAt', 'color'],
  template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ resetsAt }}|{{ color }}</div>'
}

// deepseek 用例需要把 CN 分支子单元格 stub 掉，才能断言「是否进入 CN 分支」
const cnQuotaStub = { template: '<div data-test="cn-quota" />' }
const cnBalanceStub = { template: '<div data-test="cn-balance" />' }

// 统一 stub 集：三窗口进度条 + muse 专用 + CN 分支占位
const deepseekStubs = {
  UsageProgressBar: usageBarStub,
  AccountQuotaInfo: true,
  CNProviderQuotaCell: cnQuotaStub,
  CNProviderBalanceCell: cnBalanceStub
}

// 构造一个携带 opencode_zen usage_probe 的 deepseek apikey 账号
function makeDeepseekAccount(overrides: Partial<Account>): Account {
  return makeAccount({
    platform: 'deepseek',
    type: 'apikey',
    credentials: { usage_probe: 'opencode_zen' },
    ...overrides
  } as Partial<Account>)
}

describe('AccountUsageCell muse 分支', () => {
  beforeEach(() => {
    getUsage.mockReset()
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockImplementation(() => ({
        matches: true,
        media: '(min-width: 768px)',
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn(),
      }))
    })
  })

  it('三窗口（rolling/weekly/monthly）均渲染，percent 作为 utilization 透传', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 7001, platform: 'muse', type: 'apikey' }),
        requestBatchedUsage: vi.fn(),
        batchedUsage: {
          muse_usage: {
            rolling: { status: 'ok', percent: 12, resets_at: '2026-04-01T00:00:00Z' },
            weekly: { status: 'ok', percent: 34, resets_at: '2026-04-07T00:00:00Z' },
            monthly: { status: 'ok', percent: 56, resets_at: '2026-04-30T00:00:00Z' },
            paused: false,
            unschedulable_until: null
          }
        }
      },
      global: { stubs: { UsageProgressBar: usageBarStub, AccountQuotaInfo: true } }
    })

    await flushPromises()

    const bars = wrapper.findAll('.usage-bar').map((b) => b.text())
    expect(bars).toContain('R|12|2026-04-01T00:00:00Z|indigo')
    expect(bars).toContain('7d|34|2026-04-07T00:00:00Z|emerald')
    expect(bars).toContain('30d|56|2026-04-30T00:00:00Z|purple')
    // 无暂停态：不出现暂停徽章
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.musePaused')
  })

  it('percent 语义：已用比例直接作为进度条 utilization（非剩余比例）', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 7002, platform: 'muse', type: 'apikey' }),
        requestBatchedUsage: vi.fn(),
        batchedUsage: {
          muse_usage: {
            rolling: { status: 'ok', percent: 80, resets_at: null },
            weekly: { status: 'ok', percent: 0, resets_at: null },
            monthly: null,
            paused: false,
            unschedulable_until: null
          }
        }
      },
      global: { stubs: { UsageProgressBar: usageBarStub, AccountQuotaInfo: true } }
    })

    await flushPromises()

    const bars = wrapper.findAll('.usage-bar').map((b) => b.text())
    // 高已用比例 → 80；零已用 → 0；resets_at 为 null 时进度条不显示重置时间（空串）；monthly 缺省 → 不渲染
    expect(bars).toContain('R|80||indigo')
    expect(bars).toContain('7d|0||emerald')
    expect(bars.some((t) => t.startsWith('30d'))).toBe(false)
  })

  it('unschedulable_until 有值时展示暂停态徽章与恢复时间', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 7003, platform: 'muse', type: 'apikey' }),
        requestBatchedUsage: vi.fn(),
        batchedUsage: {
          muse_usage: {
            rolling: { status: 'exhausted', percent: 100, resets_at: '2026-04-01T00:00:00Z' },
            weekly: { status: 'ok', percent: 10, resets_at: null },
            monthly: { status: 'ok', percent: 5, resets_at: null },
            paused: true,
            unschedulable_until: '2026-04-01T00:00:00Z'
          }
        }
      },
      global: { stubs: { UsageProgressBar: usageBarStub, AccountQuotaInfo: true } }
    })

    await flushPromises()

    // 暂停徽章存在
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.musePaused')
    // 恢复时间被传入（unschedulable_until 经 formatDateOnly 后作为 time 参数，非空）
    expect(wrapper.text()).toMatch(
      /admin\.accounts\.usageWindow\.museResumeAt::\{"time":"[^"]+"\}/
    )
  })

  it('无 muse_usage 字段时不渲染 muse 分支（回落占位符，不渲染进度条/暂停态）', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 7004, platform: 'muse', type: 'apikey' }),
        requestBatchedUsage: vi.fn(),
        // 后端未下发 muse_usage（如账号尚未拉取到配额）
        batchedUsage: {}
      },
      global: { stubs: { UsageProgressBar: usageBarStub, AccountQuotaInfo: true } }
    })

    await flushPromises()

    expect(wrapper.findAll('.usage-bar').length).toBe(0)
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.musePaused')
    // 占位符 '-' 仍渲染（非 OAuth 段 fallback）
    expect(wrapper.text()).toContain('-')
  })

  it('非 muse 平台不会进入 muse 分支（回归护栏：其他平台展示不受影响）', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 7005, platform: 'gemini', type: 'apikey' }),
        requestBatchedUsage: vi.fn(),
        batchedUsage: {
          // 即便携带 muse_usage 数据，gemini 账号也不该渲染 muse 分支
          muse_usage: {
            rolling: { status: 'ok', percent: 99, resets_at: null },
            weekly: null,
            monthly: null,
            paused: false,
            unschedulable_until: null
          }
        }
      },
      global: { stubs: { UsageProgressBar: usageBarStub, AccountQuotaInfo: true } }
    })

    await flushPromises()

    expect(wrapper.findAll('.usage-bar').length).toBe(0)
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.musePaused')
  })
})

// ZB-T3b：deepseek apikey opt-in（usage_probe=opencode_zen）完全镜像 muse 用量通路。
// 验证三项：showUsageWindows=false（路由进 v-else 三窗口）、shouldFetchUsage=true
// （触发批量抓取）、usage 返回 muse_usage 后渲染三窗口。
describe('AccountUsageCell deepseek opt-in 镜像 muse', () => {
  beforeEach(() => {
    getUsage.mockReset()
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockImplementation(() => ({
        matches: true,
        media: '(min-width: 768px)',
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn()
      }))
    })
  })

  it('① opt-in deepseek：showUsageWindows=false、shouldFetchUsage=true、muse_usage 渲染三窗口', async () => {
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeDeepseekAccount({ id: 7101 }),
        requestBatchedUsage,
        batchedUsage: {
          muse_usage: {
            rolling: { status: 'ok', percent: 12, resets_at: '2026-04-01T00:00:00Z' },
            weekly: { status: 'ok', percent: 34, resets_at: '2026-04-07T00:00:00Z' },
            monthly: { status: 'ok', percent: 56, resets_at: '2026-04-30T00:00:00Z' },
            paused: false,
            unschedulable_until: null
          }
        }
      },
      global: { stubs: deepseekStubs }
    })

    await flushPromises()

    // shouldFetchUsage=true → 触发批量抓取（请求前判定，依赖 credentials.usage_probe）
    expect(requestBatchedUsage).toHaveBeenCalled()
    // showUsageWindows=false → 不进入 CN 分支（与 muse 一样走 v-else 三窗口）
    expect(wrapper.find('[data-test="cn-quota"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-balance"]').exists()).toBe(false)
    // muse_usage 透传 → 三窗口进度条渲染
    const bars = wrapper.findAll('.usage-bar').map((b) => b.text())
    expect(bars).toContain('R|12|2026-04-01T00:00:00Z|indigo')
    expect(bars).toContain('7d|34|2026-04-07T00:00:00Z|emerald')
    expect(bars).toContain('30d|56|2026-04-30T00:00:00Z|purple')
  })

  it('①（续）opt-in deepseek 抓取前 museUsage 为 null：显示占位符 "-"，不渲染进度条', async () => {
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeDeepseekAccount({ id: 7102 }),
        requestBatchedUsage,
        // 批量抓取尚未返回（与 muse 抓取前一致）
        batchedUsage: {}
      },
      global: { stubs: deepseekStubs }
    })

    await flushPromises()

    expect(requestBatchedUsage).toHaveBeenCalled()
    expect(wrapper.findAll('.usage-bar').length).toBe(0)
    // 内层 v-if="museUsage" 不动：museUsage 为 null 时回落占位符
    expect(wrapper.text()).toContain('-')
  })

  it('② 未 opt-in deepseek apikey：showUsageWindows=true（走 CN 分支）、shouldFetchUsage=false、无三窗口（零变化锚定）', async () => {
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      // 不携带 usage_probe → 保持基线行为
      props: {
        account: makeAccount({
          id: 7201,
          platform: 'deepseek',
          type: 'apikey'
        }),
        requestBatchedUsage
      },
      global: { stubs: deepseekStubs }
    })

    await flushPromises()

    // shouldFetchUsage=false → 不触发批量抓取
    expect(requestBatchedUsage).not.toHaveBeenCalled()
    // showUsageWindows=true → 进入 CN 分支（CNProviderQuotaCell 渲染，零变化锚定）
    expect(wrapper.find('[data-test="cn-quota"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="cn-balance"]').exists()).toBe(true)
    // 不渲染三窗口
    expect(wrapper.findAll('.usage-bar').length).toBe(0)
  })

  it('③ muse apikey 行为不变：即便 credentials.usage_probe 存在，仍走 muse 三窗口（回归护栏）', async () => {
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 7301,
          platform: 'muse',
          type: 'apikey',
          // 即便带 usage_probe，muse 不应被新分支影响
          credentials: { usage_probe: 'opencode_zen' }
        }),
        requestBatchedUsage,
        batchedUsage: {
          muse_usage: {
            rolling: { status: 'ok', percent: 7, resets_at: null },
            weekly: null,
            monthly: null,
            paused: false,
            unschedulable_until: null
          }
        }
      },
      global: { stubs: deepseekStubs }
    })

    await flushPromises()

    // muse apikey shouldFetchUsage 保持 true
    expect(requestBatchedUsage).toHaveBeenCalled()
    // muse 仍路由到三窗口，而非 CN 分支
    expect(wrapper.find('[data-test="cn-quota"]').exists()).toBe(false)
    const bars = wrapper.findAll('.usage-bar').map((b) => b.text())
    expect(bars).toContain('R|7||indigo')
  })
})
