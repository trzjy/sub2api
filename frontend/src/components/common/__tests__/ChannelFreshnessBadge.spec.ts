import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import ChannelFreshnessBadge from '../ChannelFreshnessBadge.vue'

// 渠道级档位展示（派发单 D4b 项 2）：渠道级档位用后端推导的 channel_status /
// channel_observed_at 新字段；无数据按空态横线呈现，绝不显示为失败。

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key, te: () => true }),
  }
})

function mountBadge(props: { status?: string | null; observedAt?: string | null }) {
  return mount(ChannelFreshnessBadge, { props })
}

describe('ChannelFreshnessBadge', () => {
  it('renders the derived channel status with its observed time', () => {
    const wrapper = mountBadge({
      status: 'degraded',
      observedAt: new Date(Date.now() - 5 * 60 * 1000).toISOString(),
    })

    const badge = wrapper.get('[data-testid="channel-freshness-status"]')
    expect(badge.text()).toBe('monitorCommon.status.degraded')
    // degraded = 琥珀色，不是失败红/中性灰
    expect(badge.attributes('class')).toContain('bg-amber-100')
    expect(badge.attributes('class')).not.toContain('bg-red-100')
    expect(wrapper.get('[data-testid="channel-freshness-observed"]').text()).toBe(
      'monitorCommon.relativeMinutesAgo'
    )
    expect(wrapper.find('[data-testid="channel-freshness-empty"]').exists()).toBe(false)
  })

  it('renders a dash for the observed time when there is no observation', () => {
    const wrapper = mountBadge({ status: 'operational', observedAt: '' })

    expect(wrapper.get('[data-testid="channel-freshness-observed"]').text()).toBe('-')
    expect(wrapper.get('[data-testid="channel-freshness-status"]').text()).toBe(
      'monitorCommon.status.operational'
    )
  })

  it('renders the empty-state dash (not a failure) when the channel has no data', () => {
    const wrapper = mountBadge({ status: '', observedAt: '' })

    const empty = wrapper.get('[data-testid="channel-freshness-empty"]')
    expect(empty.text()).toBe('-')
    // 空态不得是失败样式
    expect(empty.attributes('class')).not.toContain('bg-red-100')
    expect(empty.attributes('class')).not.toContain('bg-amber-100')
    expect(wrapper.find('[data-testid="channel-freshness-status"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="channel-freshness-observed"]').exists()).toBe(false)
  })

  it('treats missing fields as no data', () => {
    const wrapper = mountBadge({})

    expect(wrapper.find('[data-testid="channel-freshness-empty"]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('monitorCommon.status.failed')
  })
})
