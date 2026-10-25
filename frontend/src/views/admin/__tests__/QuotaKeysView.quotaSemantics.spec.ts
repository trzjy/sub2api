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
    // 其他 key 仍回显 key 本身（保持既有用例断言不变）；
    // 仅 QKERR-C 接入的 errorText 两个 key 返回中文，便于回归断言可见文本。
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => {
        if (key === 'admin.quotaKeys.errorText.refreshLimitedHours') {
          return `刷新次数达上限（${params?.count}次），约 ${params?.hours} 小时后重置。`
        }
        if (key === 'admin.quotaKeys.errorText.refreshLimitedMinutes') {
          return `刷新次数达上限（${params?.count}次），约 ${params?.minutes} 分钟后重置。`
        }
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
    expect(out.length).toBeGreaterThan(0)
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
    expect(text).not.toContain('永不过期')
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

// ---------------------------------------------------------------------------
// HDR-1：详情账号折叠标题须用 account_count（真实总数），而非分页长度。
// 缺陷：原标题用 detail.accounts?.length（后端默认 page_size=20 的分页长度），
// 当账号数 > 20 时与字段区的 account_count 矛盾。本用例用「accounts 仅 2 条、
// account_count=35」模拟分页截断，锁死标题数字来自 account_count。
// ---------------------------------------------------------------------------

describe('QuotaKeysView 详情账号折叠标题用真实总数（HDR-1）', () => {
  const hdrDetail = {
    ...keyRow,
    account_count: 35,
    accounts: [
      { id: 11, name: 'cursor-acc-1', platform: 'cursor', status: 'active' },
      { id: 12, name: 'cursor-acc-2', platform: 'cursor', status: 'inactive' }
    ]
  }

  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.get).mockReset()
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [keyRow],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    } as never)
    vi.mocked(adminAPI.quotaKeys.get).mockResolvedValue(hdrDetail as never)
  })

  it('详情账号折叠标题用 account_count（真实总数），不用分页长度', async () => {
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
    // 标题渲染形如 admin.quotaKeys.accountsTitle（35）：须包含真实总数。
    expect(text).toContain('35')
    // 绝不包含分页长度（2）。
    expect(text).not.toContain('（2）')
    wrapper.unmount()
  })
})

// ---------------------------------------------------------------------------
// PG2：详情弹窗账号列表补分页控件后，翻页须按新 page 重新拉取详情，
// 且保持弹窗打开（不关弹窗）。
// ---------------------------------------------------------------------------

describe('QuotaKeysView 详情账号翻页（PG2）', () => {
  const page1Detail = {
    ...keyRow,
    account_count: 25,
    total: 25,
    page: 1,
    page_size: 20,
    accounts: [
      { id: 11, name: 'cursor-acc-1', platform: 'cursor', status: 'active' },
      { id: 12, name: 'cursor-acc-2', platform: 'cursor', status: 'inactive' }
    ]
  }
  const page2Detail = {
    ...keyRow,
    account_count: 25,
    total: 25,
    page: 2,
    page_size: 20,
    accounts: [{ id: 13, name: 'cursor-acc-3', platform: 'cursor', status: 'active' }]
  }

  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.get).mockReset()
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [keyRow],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    } as never)
    vi.mocked(adminAPI.quotaKeys.get).mockImplementation(
      (async (id: number, page: number) => {
        return (page === 2 ? page2Detail : page1Detail) as never
      }) as never
    )
  })

  it('详情账号翻页：第 2 页按新 page 拉取，弹窗保持打开', async () => {
    const wrapper = mountView()
    await flushPromises()

    const vm = wrapper.vm as unknown as {
      detail: { id: number; page: number } | null
      showDetailDialog: boolean
      openDetail: (row: typeof keyRow) => Promise<void>
      handleDetailPageChange: (nextPage: number) => Promise<void>
    }

    await vm.openDetail(keyRow)
    await flushPromises()
    await vm.handleDetailPageChange(2)
    await flushPromises()

    // 翻到第 2 页须以新 page 重新拉取详情。
    expect(adminAPI.quotaKeys.get).toHaveBeenLastCalledWith(keyRow.id, 2, 20)
    // 详情回带 page=2。
    expect(vm.detail?.page).toBe(2)
    // 翻页不关闭弹窗。
    expect(vm.showDetailDialog).toBe(true)
    wrapper.unmount()
  })
})

// ---------------------------------------------------------------------------
// QKERR-C：最近错误展示格式化接入。
// 列表单元格与详情弹窗共用 formatQuotaKeyError（经组件方法 formatLastRefreshError），
// 剥离内部 shareai 英文前缀，刷新上限消息压缩为「刷新次数达上限…约 N 小时/分钟后重置」；
// 真实 unknown-result 形态文本不得再在可见文本/tooltip 出现 shareai 前缀。
// ---------------------------------------------------------------------------

describe('QuotaKeysView 最近错误展示格式化（QKERR-C）', () => {
  // 真实形态：上游刷新上限消息被 shareai 错误链前缀包裹。
  const unknownResultRaw =
    'shareai: upstream code=1 unknown result: 当日刷新次数达到上限(5次)，将在每日8:00重置，还需7小时0分钟'

  // DataTable 默认被整桩（true），列表单元格模板不渲染；此处用最小桩渲染
  // cell-last_refresh_error 槽，以验证「列表显示」真实文本。
  function mountViewWithListErrorCell() {
    return mount(QuotaKeysView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          TablePageLayout: {
            template: '<div><slot name="filters" /><slot name="table" /></div>'
          },
          DataTable: {
            props: ['columns', 'data', 'loading'],
            template: `<div>
              <template v-for="(row, i) in data" :key="i">
                <slot name="cell-last_refresh_error" :row="row" />
              </template>
              <slot name="empty" />
            </div>`
          },
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

  const errorRow = { ...keyRow, last_refresh_error: unknownResultRaw }

  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.get).mockReset()
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [errorRow],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    } as never)
    vi.mocked(adminAPI.quotaKeys.get).mockResolvedValue({
      ...keyDetail,
      last_refresh_error: unknownResultRaw
    } as never)
  })

  it('列表单元格：真实 unknown-result 文本剥离 shareai 前缀并压缩为刷新上限短句', async () => {
    const wrapper = mountViewWithListErrorCell()
    await flushPromises()

    const vm = wrapper.vm as unknown as {
      formatLastRefreshError: (raw: string | null) => string
    }
    // 经组件方法（列表单元格使用同一函数）格式化。
    const out = vm.formatLastRefreshError(unknownResultRaw)
    expect(out).not.toContain('shareai: upstream code=1 unknown result:')
    expect(out).toContain('刷新次数达上限')
    expect(out).toContain('约')
    expect(out).toContain('小时后')

    // 列表单元格实际渲染文本同样不含前缀、含中文短句。
    const listText = wrapper.text()
    expect(listText).not.toContain('shareai: upstream code=1 unknown result:')
    expect(listText).toContain('刷新次数达上限')
    expect(listText).toContain('约')
    expect(listText).toContain('小时后')
    wrapper.unmount()
  })

  it('详情弹窗：最近错误同样剥离前缀并压缩', async () => {
    const wrapper = mountViewWithListErrorCell()
    await flushPromises()
    const vm = wrapper.vm as unknown as {
      openDetail: (row: typeof errorRow) => Promise<void>
    }
    await vm.openDetail(errorRow)
    await flushPromises()
    await nextTick()

    const detailText = wrapper.text()
    expect(detailText).not.toContain('shareai: upstream code=1 unknown result:')
    expect(detailText).toContain('刷新次数达上限')
    expect(detailText).toContain('约')
    expect(detailText).toContain('小时后')
    wrapper.unmount()
  })

  it('空值：列表方法返回空串，详情回退 na 文案', async () => {
    const wrapper = mountViewWithListErrorCell()
    await flushPromises()
    const vm = wrapper.vm as unknown as {
      formatLastRefreshError: (raw: string | null) => string
      openDetail: (row: typeof keyRow) => Promise<void>
    }
    // 列表方法对空值 / 空白返回空串（由模板回退 na）。
    expect(vm.formatLastRefreshError(null)).toBe('')
    expect(vm.formatLastRefreshError('   ')).toBe('')

    // 详情空值回退 na。
    vi.mocked(adminAPI.quotaKeys.get).mockResolvedValue({
      ...keyDetail,
      last_refresh_error: null
    } as never)
    await vm.openDetail(keyRow)
    await flushPromises()
    await nextTick()
    expect(wrapper.text()).toContain('admin.quotaKeys.na')
    wrapper.unmount()
  })
})
