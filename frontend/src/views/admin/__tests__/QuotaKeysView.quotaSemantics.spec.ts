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

// ---------------------------------------------------------------------------
// O03：probe 成功后自动刷新列表与已开详情（expires_at 事实源唯一 = list/get）。
// 后端 ProbeKeyQuota 已把 expires_at 写库，前端须在 probe 成功后复用 load() 与
// get() 重新读取，禁止拿 probe 的未解析原始字符串另建展示数据。
// ---------------------------------------------------------------------------

type O03Vm = {
  keys: Array<{ id: number; expires_at: string | null }>
  detail: { id: number; expires_at: string | null } | null
  handleProbe: (row: { id: number }) => Promise<void>
  openDetail: (row: { id: number }) => Promise<void>
}

describe('QuotaKeysView probe 成功后刷新列表与详情（O03）', () => {
  const nullRow = { ...keyRow, expires_at: null as string | null }
  const updatedRow = { ...keyRow, expires_at: '2027-06-01T00:00:00.000Z' }

  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.get).mockReset()
    vi.mocked(adminAPI.quotaKeys.probeQuota).mockReset()
  })

  it('初始列表 expires_at=null，probe 成功后列表自动刷新为新日期（无需手动刷新）', async () => {
    let listCalls = 0
    vi.mocked(adminAPI.quotaKeys.list).mockImplementation(async () => {
      listCalls += 1
      const item = listCalls === 1 ? nullRow : updatedRow
      return { items: [item], total: 1, page: 1, page_size: 20, pages: 1 } as never
    })
    vi.mocked(adminAPI.quotaKeys.probeQuota).mockResolvedValue(keyQuota as never)

    const wrapper = mountView()
    await flushPromises()

    // 挂载即首次 load()：列表该行 expires_at 为 null。
    const vm0 = wrapper.vm as unknown as O03Vm
    expect(vm0.keys[0].expires_at).toBeNull()

    const vm = wrapper.vm as unknown as O03Vm
    await vm.handleProbe(nullRow)
    await flushPromises()
    await nextTick()

    // 成功路径下 list 接口被再次调用（共 2 次）。
    expect(adminAPI.quotaKeys.list).toHaveBeenCalledTimes(2)
    // 行数据已更新为新日期，无需手动刷新。
    expect(vm.keys[0].expires_at).toBe('2027-06-01T00:00:00.000Z')
    wrapper.unmount()
  })

  it('详情弹窗打开后 probe 成功 → get 再次拉取且 detail 数据更新', async () => {
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [keyRow],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    } as never)

    const oldDetail = { ...keyDetail, expires_at: null as string | null }
    const newDetail = { ...keyDetail, expires_at: '2027-06-01T00:00:00.000Z' }
    let getCalls = 0
    vi.mocked(adminAPI.quotaKeys.get).mockImplementation(async () => {
      getCalls += 1
      return (getCalls === 1 ? oldDetail : newDetail) as never
    })
    vi.mocked(adminAPI.quotaKeys.probeQuota).mockResolvedValue(keyQuota as never)

    const wrapper = mountView()
    await flushPromises()
    const vm = wrapper.vm as unknown as O03Vm

    // 打开详情弹窗（第一次 get，expires_at 为 null）。
    await vm.openDetail(keyRow)
    await flushPromises()
    expect(vm.detail?.expires_at).toBeNull()

    // probe 成功 → handleProbe 内对当前打开行再次拉取 get。
    await vm.handleProbe(keyRow)
    await flushPromises()
    await nextTick()

    // get 被再次调用（共 2 次）。
    expect(adminAPI.quotaKeys.get).toHaveBeenCalledTimes(2)
    // detail 数据已更新为新日期。
    expect(vm.detail?.expires_at).toBe('2027-06-01T00:00:00.000Z')
    wrapper.unmount()
  })
})
