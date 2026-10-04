import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import type { UserMonitorView } from '@/api/channelMonitor'
import MonitorCard from '../monitor/MonitorCard.vue'

// 用户渠道状态视图（MonitorCard）展示后端推导的渠道级档位与观测时间
// （派发单 D4b 项 2）；无数据为空态横线，不显示为失败。

const { isQuotaVisible } = vi.hoisted(() => ({
  isQuotaVisible: vi.fn(() => false),
}))

vi.mock('@/utils/featureFlags', () => ({
  isChannelMonitorQuotaVisible: () => isQuotaVisible(),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key, te: () => true }),
  }
})

function makeItem(overrides: Partial<UserMonitorView> = {}): UserMonitorView {
  return {
    id: 1,
    name: 'claude-main',
    provider: 'kimi',
    group_name: '',
    primary_model: 'quota',
    primary_status: 'operational',
    primary_latency_ms: null,
    primary_ping_latency_ms: null,
    availability_7d: 100,
    extra_models: [],
    timeline: [],
    ...overrides,
  }
}

function mountCard(item: UserMonitorView) {
  return mount(MonitorCard, {
    props: { item, window: '7d', availabilityValue: 100, countdownSeconds: 0 },
    global: {
      stubs: {
        MonitorMetricPair: true,
        MonitorAvailabilityRow: true,
        MonitorTimeline: true,
      },
    },
  })
}

describe('MonitorCard channel-level status', () => {
  it('renders the derived channel status next to the per-model status', () => {
    const wrapper = mountCard(
      makeItem({
        channel_status: 'operational',
        channel_observed_at: new Date(Date.now() - 30 * 1000).toISOString(),
      })
    )

    const badge = wrapper.get('[data-testid="channel-freshness-status"]')
    expect(badge.text()).toBe('monitorCommon.status.operational')
    expect(wrapper.get('[data-testid="channel-freshness-observed"]').text()).toBe(
      'monitorCommon.relativeSecondsAgo'
    )
  })

  it('renders the no-data empty state instead of a failure when there is no channel observation', () => {
    const wrapper = mountCard(makeItem({ channel_status: '', channel_observed_at: '' }))

    const empty = wrapper.get('[data-testid="channel-freshness-empty"]')
    expect(empty.text()).toBe('-')
    expect(empty.attributes('class')).not.toContain('bg-red-100')
    expect(wrapper.find('[data-testid="channel-freshness-status"]').exists()).toBe(false)
  })
})
