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
