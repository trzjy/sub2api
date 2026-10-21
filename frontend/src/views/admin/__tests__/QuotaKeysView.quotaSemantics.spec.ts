import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import QuotaKeysView from '../QuotaKeysView.vue'
import { adminAPI } from '@/api/admin'

// P1-B3 两层额度语义：
//   第一层 key 级（激活码）：总额度 / 期限 / 设备 / 取号结果 —— 在激活码页的用量弹窗展示；
//   第二层账号级：关联账号只展示归属与状态，绝不复制 key 级余额数值。
// 本用例锁死「key 页显示 key 总额度、账号行不出现 key 余额数字」这条边界。

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

const keyDetail = {
  ...keyRow,
  notes: null,
  accounts: [
    { id: 11, name: 'cursor-acc-1', platform: 'cursor', status: 'active' },
    { id: 12, name: 'cursor-acc-2', platform: 'cursor', status: 'inactive' }
  ]
}

const keyQuota = {
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
    useI18n: () => ({ t: (key: string) => key })
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
        AutoEntryConfigModal: true,
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }
      }
    }
  })
}

describe('QuotaKeysView 两层额度语义（P1-B3）', () => {
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
    vi.mocked(adminAPI.quotaKeys.get).mockResolvedValue(keyDetail as never)
    vi.mocked(adminAPI.quotaKeys.probeQuota).mockResolvedValue(keyQuota as never)
  })

  it('激活码页展示 key 级额度快照：总额度 / 期限 / 设备 / 取号结果', async () => {
    const wrapper = mountView()
    await flushPromises()

    const vm = wrapper.vm as unknown as {
      handleProbe: (row: typeof keyRow) => Promise<void>
    }
    await vm.handleProbe(keyRow)
    await flushPromises()
    await nextTick()

    const text = wrapper.text()
    // key 级口径字段齐全：总额度 max / 期限 / 设备 / 取号结果（已写入账号数）。
    expect(text).toContain('admin.quotaKeys.probe.max')
    expect(text).toContain('5000')
    expect(text).toContain('admin.quotaKeys.probe.expiredAt')
    expect(text).toContain('admin.quotaKeys.probe.deviceCount')
    expect(text).toContain('admin.quotaKeys.probe.accountsWritten')
    // 明确标注为 key 级口径（第一层）。
    expect(text).toContain('admin.quotaKeys.probe.keyLevelHint')
    wrapper.unmount()
  })

  it('关联账号层不展示复制自 key 的余额数值', async () => {
    const wrapper = mountView()
    await flushPromises()

    const vm = wrapper.vm as unknown as {
      openDetail: (row: typeof keyRow) => Promise<void>
      toggleAccounts: (id: number) => void
    }
    await vm.openDetail(keyRow)
    await flushPromises()
    await nextTick()
    vm.toggleAccounts(7)
    await nextTick()

    const text = wrapper.text()
    // 账号层展示归属与状态。
    expect(text).toContain('cursor-acc-1')
    expect(text).toContain('cursor-acc-2')
    // 绝不出现 key 级额度数值（总额度 / 已用）。
    expect(text).not.toContain('5000')
    expect(text).not.toContain('3200')
    // 明确标注两层额度独立。
    expect(text).toContain('admin.quotaKeys.detail.accountsQuotaHint')
    wrapper.unmount()
  })
})

// ---------------------------------------------------------------------------
// C-U7-B 到期时间展示收敛：到期列 + 探测弹窗的语义锁死。
// 本用例使用 t 的 mock（返回 key 本身），故断言针对 i18n key 与格式化字符串分支。
// ---------------------------------------------------------------------------

describe('QuotaKeysView 到期时间展示收敛（C-U7-B）', () => {
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
    vi.mocked(adminAPI.quotaKeys.get).mockResolvedValue(keyDetail as never)
  })

  it('status=expired 且无到期时间 → "已过期"', async () => {
    const wrapper = mountView()
    await flushPromises()
    const vm = wrapper.vm as unknown as {
      formatExpiresAt: (row: { status: string; expires_at: string | null }) => string
    }
    const out = vm.formatExpiresAt({ status: 'expired', expires_at: null })
    expect(out).toBe('admin.quotaKeys.status.expired')
    wrapper.unmount()
  })

  it('status=active 且无到期时间 → "到期时间未知"', async () => {
    const wrapper = mountView()
    await flushPromises()
    const vm = wrapper.vm as unknown as {
      formatExpiresAt: (row: { status: string; expires_at: string | null }) => string
    }
    const out = vm.formatExpiresAt({ status: 'active', expires_at: null })
    expect(out).toBe('admin.quotaKeys.expiryUnknown')
    wrapper.unmount()
  })

  it('status=active 且有期限 → 返回具体日期字符串（非 unknown/never key）', async () => {
    const wrapper = mountView()
    await flushPromises()
    const vm = wrapper.vm as unknown as {
      formatExpiresAt: (row: { status: string; expires_at: string | null }) => string
    }
    const out = vm.formatExpiresAt({ status: 'active', expires_at: '2026-12-31T00:00:00Z' })
    expect(out).not.toBe('admin.quotaKeys.expiryUnknown')
    expect(out).not.toBe('admin.quotaKeys.never')
    expect(out).not.toBe('admin.quotaKeys.status.expired')
    expect(out.length).toBeGreaterThan(0)
    wrapper.unmount()
  })

  it('探测弹窗空值不再显示"永不过期"（改显示"到期时间未知"）', async () => {
    vi.mocked(adminAPI.quotaKeys.probeQuota).mockResolvedValue({
      ...keyQuota,
      expired_at: null
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
    // 空值必须映射到"到期时间未知"文案。
    expect(text).toContain('admin.quotaKeys.expiryUnknown')
    // 绝不再渲染"永不过期"。
    expect(text).not.toContain('admin.quotaKeys.never')
    wrapper.unmount()
  })
})
