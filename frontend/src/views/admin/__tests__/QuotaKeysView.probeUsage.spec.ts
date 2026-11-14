import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import QuotaKeysView from '../QuotaKeysView.vue'
import { adminAPI } from '@/api/admin'

// KNCY-O：探测弹窗「已用 / 总额度」按 usage 投影展示美元额度。
//   当 probeResult.usage.remaining_unit === 'usd' 时：
//     - 「已用」行改为「今日剩余」= usage.today_remaining（toFixed(2) 美元）；
//     - 「总额度」行改为「总量剩余」= usage.total_remaining（toFixed(2) 美元）；
//     - 不再显示分值的 probeResult.used / probeResult.max（避免出现 29048 这种百倍偏差）。
//   其它情况（无 usage 或非 usd）：逐字保持 probeResult.used / probeResult.max 展示。

const keyRow = {
  id: 7,
  status: 'active',
  exhausted: false,
  code_masked: '****abcd',
  last_refresh_at: null,
  last_refresh_error: null,
  expires_at: '2026-12-31T00:00:00.000Z',
  account_count: 2,
  working_count: 1
}

// 非 USD 口径的探测结果：逐字展示分值的 used / max。
const keyQuotaCounts = {
  group: 'pro',
  status_code: 200,
  max: 5000,
  used: 3200,
  device_count: 2,
  device_max_count: 5,
  mode: 1,
  expired_at: '2026-12-31T00:00:00.000Z',
  accounts_written: 3
}

// USD 口径的探测结果：分值 max=29048 不应被当作份额渲染，
// 而应投影为 today_remaining / total_remaining（美元）。
const keyQuotaUSD = {
  ...keyQuotaCounts,
  max: 29048,
  used: 0,
  usage: {
    scope: 'quota_key',
    label: '所属激活码额度',
    total: 29048,
    used: 0,
    remaining: 29048,
    today_remaining: 0.48,
    total_remaining: 290.48,
    remaining_unit: 'usd',
    status_code: 200,
    checked_at: '2026-10-22T02:00:00Z',
    attempted_at: '2026-10-22T02:00:00Z',
    has_snapshot: true,
    query_failed: false
  }
}

// usage 存在但非 usd 口径：仍逐字展示分值 used / max。
const keyQuotaNonUSD = {
  ...keyQuotaCounts,
  usage: {
    scope: 'quota_key',
    label: '所属激活码额度',
    total: 5000,
    used: 3200,
    remaining: 1800,
    remaining_unit: 'percent',
    status_code: 200,
    checked_at: '2026-10-22T02:00:00Z',
    attempted_at: '2026-10-22T02:00:00Z',
    has_snapshot: true,
    query_failed: false
  }
}

vi.mock('@/api/admin', () => ({
  adminAPI: {
    quotaKeys: {
      list: vi.fn(),
      get: vi.fn(),
      probeQuota: vi.fn(),
      refresh: vi.fn(),
      clearExhausted: vi.fn(),
      remove: vi.fn(),
      sweepExpired: vi.fn(),
      update: vi.fn(),
      create: vi.fn()
    }
  }
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: vi.fn() }),
  useRoute: () => ({ path: '/admin/quota-keys', query: {} })
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => {
        if (params && typeof params.amount === 'string') return `${key}:${params.amount}`
        return key
      }
    })
  }
})

function mountView() {
  return mount(QuotaKeysView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template: '<div><slot name="filters" /><slot name="table" /></div>'
        },
        DataTable: true,
        ConfirmDialog: true,
        EmptyState: true,
        TextArea: true,
        Icon: true,
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }
      }
    }
  })
}

describe('QuotaKeysView 探测弹窗按 usage 投影展示美元额度（KNCY-O）', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.get).mockReset()
    vi.mocked(adminAPI.quotaKeys.probeQuota).mockReset()
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [keyRow],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    } as never)
    vi.mocked(adminAPI.quotaKeys.get).mockResolvedValue({
      ...keyRow,
      notes: null,
      accounts: []
    } as never)
  })

  it('USD 口径：已用→今日剩余、总额度→总量剩余，且分值 29048 不出现', async () => {
    vi.mocked(adminAPI.quotaKeys.probeQuota).mockResolvedValue(keyQuotaUSD as never)

    const wrapper = mountView()
    await flushPromises()

    const vm = wrapper.vm as unknown as {
      handleProbe: (row: typeof keyRow) => Promise<void>
    }
    await vm.handleProbe(keyRow)
    await flushPromises()
    await nextTick()

    const text = wrapper.text()
    // 文案改用「今日剩余 / 总量剩余」。
    expect(text).toContain('admin.quotaKeys.probe.todayRemaining')
    expect(text).toContain('admin.quotaKeys.probe.totalRemaining')
    // 美元投影渲染：总量剩余含 290.48。
    expect(text).toContain('admin.quotaKeys.probe.usdAmount:290.48')
    // 关键回归锁：分值 max=29048 绝不当作份额渲染出来。
    expect(text).not.toContain('29048')
    // 原始 used/max 标签不再出现（被取代）。
    expect(text).not.toContain('admin.quotaKeys.probe.used')
    expect(text).not.toContain('admin.quotaKeys.probe.max')
    wrapper.unmount()
  })

  it('非 USD 口径（无 usage）：逐字保持 used / max 分值展示', async () => {
    vi.mocked(adminAPI.quotaKeys.probeQuota).mockResolvedValue(keyQuotaCounts as never)

    const wrapper = mountView()
    await flushPromises()

    const vm = wrapper.vm as unknown as {
      handleProbe: (row: typeof keyRow) => Promise<void>
    }
    await vm.handleProbe(keyRow)
    await flushPromises()
    await nextTick()

    const text = wrapper.text()
    // 标签保持 used / max。
    expect(text).toContain('admin.quotaKeys.probe.used')
    expect(text).toContain('admin.quotaKeys.probe.max')
    // 分值逐字展示。
    expect(text).toContain('5000')
    expect(text).toContain('3200')
    // 不出现美元投影文案。
    expect(text).not.toContain('admin.quotaKeys.probe.todayRemaining')
    expect(text).not.toContain('admin.quotaKeys.probe.totalRemaining')
    wrapper.unmount()
  })

  it('usage 存在但 remaining_unit 非 usd：仍逐字保持 used / max 分值', async () => {
    vi.mocked(adminAPI.quotaKeys.probeQuota).mockResolvedValue(keyQuotaNonUSD as never)

    const wrapper = mountView()
    await flushPromises()

    const vm = wrapper.vm as unknown as {
      handleProbe: (row: typeof keyRow) => Promise<void>
    }
    await vm.handleProbe(keyRow)
    await flushPromises()
    await nextTick()

    const text = wrapper.text()
    expect(text).toContain('admin.quotaKeys.probe.used')
    expect(text).toContain('admin.quotaKeys.probe.max')
    expect(text).toContain('5000')
    expect(text).toContain('3200')
    expect(text).not.toContain('admin.quotaKeys.probe.todayRemaining')
    expect(text).not.toContain('admin.quotaKeys.probe.totalRemaining')
    wrapper.unmount()
  })

  it('USD 口径且 today_remaining 缺失：该位显示占位符「—」，不补 0', async () => {
    vi.mocked(adminAPI.quotaKeys.probeQuota).mockResolvedValue({
      ...keyQuotaUSD,
      usage: { ...(keyQuotaUSD.usage as object), today_remaining: undefined }
    } as never)

    const wrapper = mountView()
    await flushPromises()

    const vm = wrapper.vm as unknown as {
      handleProbe: (row: typeof keyRow) => Promise<void>
    }
    await vm.handleProbe(keyRow)
    await flushPromises()
    await nextTick()

    const text = wrapper.text()
    // 总量剩余仍渲染，今日剩余缺失位给占位符「—」。
    expect(text).toContain('admin.quotaKeys.probe.usdAmount:290.48')
    expect(text).toContain('todayRemaining')
    // 占位符「—」出现在弹窗文本中（usdAmount:290.48 之外）。
    expect(text).toContain('—')
    wrapper.unmount()
  })
})
