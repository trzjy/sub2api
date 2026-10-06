import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import type { ChannelMonitor } from '@/api/admin/channelMonitor'
import ChannelMonitorView from '@/views/admin/ChannelMonitorView.vue'

// 渠道监控页展示后端推导的渠道级档位与观测时间（派发单 D4b 项 2）：
// 渠道级档位用新字段 channel_status / channel_observed_at，per-model 明细行维持既有展示。

const { listMonitors } = vi.hoisted(() => ({ listMonitors: vi.fn() }))

vi.mock('@/utils/featureFlags', () => ({
  isChannelMonitorV1Mode: () => true,
  isChannelMonitorV2Mode: () => false,
  getChannelMonitorMode: () => 'v1' as const,
}))

vi.mock('@/features/channel-monitor-v2/MonitorSettingsPanel.vue', () => ({
  default: { name: 'MonitorSettingsPanel', template: '<div data-testid="v2-settings" />' },
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    channelMonitor: {
      list: listMonitors,
      duplicate: vi.fn(),
      update: vi.fn(),
      runNow: vi.fn(),
      del: vi.fn(),
    },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

const AppLayoutStub = defineComponent({ template: '<main><slot /></main>' })
const TablePageLayoutStub = defineComponent({
  template: '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>',
})

// 渲染 cell-channel_status slot：渠道级档位断言基于它。
const DataTableStub = defineComponent({
  props: {
    data: { type: Array, default: () => [] },
    columns: { type: Array, default: () => [] },
    loading: { type: Boolean, default: false },
  },
  template:
    '<div><div v-for="row in data" :key="row.id" class="channel-status-cell"><slot name="cell-channel_status" :row="row" /></div></div>',
})

function makeMonitor(overrides: Partial<ChannelMonitor> = {}): ChannelMonitor {
  return {
    id: 42,
    name: 'primary',
    provider: 'openai',
    api_mode: 'chat_completions',
    endpoint: 'https://api.example.com',
    api_key_masked: 'sk-t***',
    primary_model: 'gpt-4o-mini',
    extra_models: [],
    group_name: '',
    enabled: true,
    interval_seconds: 60,
    jitter_seconds: 0,
    last_checked_at: null,
    created_by: 1,
    created_at: '2026-07-16T00:00:00Z',
    updated_at: '2026-07-16T00:00:00Z',
    primary_status: '',
    primary_latency_ms: null,
    availability_7d: 0,
    extra_models_status: [],
    template_id: null,
    extra_headers: {},
    body_override_mode: 'off',
    body_override: null,
    check_mode: 'probe',
    account_id: null,
    ...overrides,
  }
}

async function mountView(monitor: ChannelMonitor) {
  listMonitors.mockResolvedValue({
    items: [monitor],
    total: 1,
    page: 1,
    page_size: 20,
    pages: 1,
  })
  const wrapper = mount(ChannelMonitorView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub,
        TablePageLayout: TablePageLayoutStub,
        DataTable: DataTableStub,
        MonitorFiltersBar: true,
        Pagination: true,
        ConfirmDialog: true,
        EmptyState: true,
        HelpTooltip: true,
        Toggle: true,
        MonitorFormDialog: true,
        MonitorTemplateManagerDialog: true,
        MonitorRunResultDialog: true,
        MonitorPrimaryModelCell: true,
      },
    },
  })
  await flushPromises()
  return wrapper
}

describe('ChannelMonitorView channel-level status', () => {
  beforeEach(() => {
    localStorage.clear()
    listMonitors.mockReset()
  })

  it('renders the derived channel status with its observed time', async () => {
    const wrapper = await mountView(
      makeMonitor({
        channel_status: 'degraded',
        channel_observed_at: new Date(Date.now() - 3 * 60 * 1000).toISOString(),
      })
    )

    const cell = wrapper.get('.channel-status-cell')
    const badge = cell.get('[data-testid="channel-freshness-status"]')
    expect(badge.text()).toBe('monitorCommon.status.degraded')
    expect(cell.get('[data-testid="channel-freshness-observed"]').text()).toBe(
      'monitorCommon.relativeMinutesAgo'
    )
    wrapper.unmount()
  })

  it('renders the no-data empty state (dash, not failure) when the channel has no observation', async () => {
    const wrapper = await mountView(makeMonitor({ channel_status: '', channel_observed_at: '' }))

    const cell = wrapper.get('.channel-status-cell')
    const empty = cell.get('[data-testid="channel-freshness-empty"]')
    expect(empty.text()).toBe('-')
    expect(empty.attributes('class')).not.toContain('bg-red-100')
    expect(cell.find('[data-testid="channel-freshness-status"]').exists()).toBe(false)
    wrapper.unmount()
  })
})
