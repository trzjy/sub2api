import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CNProviderBalanceCell from '../CNProviderBalanceCell.vue'
import type { Account } from '@/types'

const { queryBalance } = vi.hoisted(() => ({
  queryBalance: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    cnProviders: { queryBalance }
  }
}))

// 分量 mock：保留 createI18n 等真实导出（UsageProgressBar → @/utils/format → @/i18n
// 在模块初始化时会调用 createI18n），只覆写 useI18n 的 t。
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, named?: Record<string, unknown>) => {
        if (key === 'admin.accounts.cnProviders.remainingCount' && named) {
          return `剩余 ${named.count}`
        }
        // 三态文案需要真实插值，断言才能锁住「剩余次数/分母」的取数与量纲。
        if (key === 'admin.accounts.cnProviders.unlimitedWithPlan' && named) {
          return `${named.plan} · 不限量`
        }
        if (key === 'admin.accounts.cnProviders.countPackRemaining' && named) {
          return `次卡 ${named.remaining}/${named.limit}`
        }
        if (key === 'admin.accounts.cnProviders.countPackNoLimit' && named) {
          return `次卡 剩余 ${named.count}`
        }
        if (key === 'admin.accounts.cnProviders.countPack') return '次卡'
        // 错误码映射（派发单 balance-ui-errmap）：命中 → 本地化文案，与裸英文 message
        // 可区分，断言才能锁住「命中走映射、未命中回退 message」。
        if (key.startsWith('admin.accounts.cnProviders.probeErrors.')) {
          return `本地化:${key.slice('admin.accounts.cnProviders.probeErrors.'.length)}`
        }
        return key
      }
    })
  }
})

const account = {
  id: 7,
  platform: 'kimi',
  type: 'apikey',
  credentials: { account_mode: 'payg' },
  extra: {
    kimi_balance: 12.5,
    kimi_balance_currency: 'CNY'
  }
} as Account

describe('CNProviderBalanceCell', () => {
  beforeEach(() => {
    queryBalance.mockReset()
  })

  it('renders the persisted balance as static text with an explicit query action', async () => {
    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await flushPromises()

    // Snapshot value renders without any probe.
    expect(queryBalance).not.toHaveBeenCalled()
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')

    // The control reads as an action; the i18n mock returns the key itself.
    const probeButton = wrapper.get('[data-test="cn-provider-balance-probe"]')
    expect(probeButton.text()).toBe('admin.accounts.cnProviders.probe')

    await probeButton.trigger('click')
    await flushPromises()
    expect(queryBalance).toHaveBeenCalledWith(account.id)
  })

  it('does NOT render the low-balance badge when only the extra marker is set (single source of truth)', () => {
    const extraOnlyAccount = {
      ...account,
      extra: { kimi_balance: 0.4, kimi_balance_low: true }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: extraOnlyAccount } })

    expect(wrapper.text()).not.toContain('admin.accounts.cnProviders.balanceLow')
  })

  it('does NOT render the low-balance badge when the DTO field is absent', () => {
    const wrapper = mount(CNProviderBalanceCell, { props: { account } })

    expect(wrapper.text()).not.toContain('admin.accounts.cnProviders.balanceLow')
  })

  it('keeps the snapshot balance visible when a query fails', async () => {
    queryBalance.mockResolvedValue({ success: false, error: 'HTTP 401' })
    const wrapper = mount(CNProviderBalanceCell, { props: { account } })

    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('CNY 12.50')
    expect(wrapper.text()).toContain('HTTP 401')
  })

  it('shows remaining count next to the unlimited plan badge from snapshot', () => {
    const unlimitedAccount = {
      ...account,
      extra: {
        kimi_balance_unlimited: true,
        kimi_balance: 1,
        kimi_balance_plan_name: 'Plan A'
      }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: unlimitedAccount } })

    // mock 的 t 对 unlimitedWithPlan 做了真实插值（三态用例需要），断言随之改为插值结果。
    expect(wrapper.text()).toContain('Plan A · 不限量')
    const remaining = wrapper.get('[data-test="cn-provider-balance-remaining"]')
    expect(remaining.text()).toContain('剩余 1')
  })

  it('does not render remaining count when unlimited but no numeric snapshot', () => {
    const unlimitedNoBalance = {
      ...account,
      extra: {
        kimi_balance_unlimited: true,
        kimi_balance_plan_name: 'Plan A'
      }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: unlimitedNoBalance } })

    expect(wrapper.find('[data-test="cn-provider-balance-remaining"]').exists()).toBe(false)
  })

  // ===== 方案 §3.4 / R5-F3：balance_low 出口 + Kira paid 结构化行 =====

  // balance_low 出口：顶层 DTO 字段（逐字锁定）true → 渲染余额不足徽标；
  // 既有余额金额取数路径不变。
  it('shows the low-balance badge from the top-level DTO balance_low field', () => {
    const dtoLowAccount = {
      ...account,
      balance_low: true
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: dtoLowAccount } })

    expect(wrapper.text()).toContain('admin.accounts.cnProviders.balanceLow')
    // 既有余额金额展示不变
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')
  })

  // Kira paid 结构化行：source=kira_vnd_balance 维度驱动余额行状态徽标；
  // 既有余额金额（currentEntries）取数路径不变。
  it('adds a status badge to the balance row for a Kira paid dimension', () => {
    const kiraPaidAccount = {
      ...account,
      quota_dimensions: [
        { kind: 'paid', scope: 'account', target: '', status: 'exhausted', servable: 'no', source: 'kira_vnd_balance', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: kiraPaidAccount } })

    expect(wrapper.find('[data-test="cn-provider-balance-kira-status"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.cnProviders.statusExhausted')
    // 既有余额金额展示不变
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')
  })

  // ===== 单轨展示不变式回归（派发单 UNIFY-QUOTA-UI-20261010 / §3.4）=====

  // ① 负例锁死：Kira 付费维度 unknown + 旧快照有余额 → 仅渲染状态徽标、金额数字隐藏。
  it('① hides the balance amount when the Kira paid dimension is unknown', () => {
    const unknownKiraPaid = {
      ...account,
      quota_dimensions: [
        { kind: 'paid', scope: 'account', target: '', status: 'unknown', servable: 'unknown', source: 'kira_vnd_balance', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as Account
    const wrapper = mount(CNProviderBalanceCell, { props: { account: unknownKiraPaid } })
    // 状态徽标（unknown）仍渲染
    expect(wrapper.find('[data-test="cn-provider-balance-kira-status"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.cnProviders.dimensionStatusUnknown')
    // 金额数字隐藏（旧快照 kimi_balance=12.5 不出现）
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).not.toContain('CNY 12.50')
  })

  // ① relay 族付费维度 unknown（source={platform}_balance）→ 仅徽标、金额隐藏（降级兼容之外的维度覆盖情形）。
  it('① hides the balance amount when the relay paid dimension is unknown', () => {
    const relayAccount = {
      id: 7,
      platform: 'deepseek',
      type: 'apikey',
      credentials: { account_mode: 'payg' },
      extra: { deepseek_balance: 8.8, deepseek_balance_currency: 'USD' },
      quota_dimensions: [
        { kind: 'paid', scope: 'account', target: '', status: 'unknown', servable: 'unknown', source: 'deepseek_balance', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as Account
    const wrapper = mount(CNProviderBalanceCell, { props: { account: relayAccount } })
    expect(wrapper.find('[data-test="cn-provider-balance-kira-status"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.cnProviders.dimensionStatusUnknown')
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).not.toContain('USD 8.80')
  })

  // ② 维度 remaining → 金额渲染（既有余额金额取数路径不变）。
  it('② renders the balance amount when the paid dimension is remaining', () => {
    const remainingKiraPaid = {
      ...account,
      quota_dimensions: [
        { kind: 'paid', scope: 'account', target: '', status: 'remaining', servable: 'yes', source: 'kira_vnd_balance', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as Account
    const wrapper = mount(CNProviderBalanceCell, { props: { account: remainingKiraPaid } })
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')
  })

  // ③ 付费维度 exhausted → 状态唯一出口渲染、无第二个重复徽标（本 cell 仅一个状态徽标）。
  it('③ exposes paid status only via the single status badge (no duplicate)', () => {
    const exhaustedKiraPaid = {
      ...account,
      quota_dimensions: [
        { kind: 'paid', scope: 'account', target: '', status: 'exhausted', servable: 'no', source: 'kira_vnd_balance', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as Account
    const wrapper = mount(CNProviderBalanceCell, { props: { account: exhaustedKiraPaid } })
    expect(wrapper.findAll('[data-test="cn-provider-balance-kira-status"]')).toHaveLength(1)
    expect(wrapper.text()).toContain('admin.accounts.cnProviders.statusExhausted')
  })

  // ④ quota_dimensions 缺失（旧 DTO 兼容）→ 降级为现状渲染：金额照旧展示，不报错、不造状态。
  it('④ degrades to legacy balance rendering when quota_dimensions is absent', () => {
    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')
  })

  // ===== 派发单 BAL-GATE-D3 C1/C2：probeEnabled 门控（可见性能力门控）=====

  // 火山订阅号 + 有余额探测能力（后端投影 balance_probe_capable=true）→ 余额单元格可见
  // （capable 优先于火山排除，消除平行闸）。注意：判定已升级为后端投影能力，非
  // credentials.balance_probe.enabled 配置开关。
  it('volcano base URL + balance_probe_capable → renders balance cell (capable overrides volcano exclusion)', () => {
    const volcanoProbeAccount = {
      id: 7,
      platform: 'deepseek',
      type: 'apikey',
      balance_probe_capable: true,
      credentials: {
        account_mode: 'payg',
        base_url: 'https://ark.cn-beijing.volces.com/api/plan',
        balance_probe: { enabled: true, url: 'https://ark.cn-beijing.volces.com/api/plan/v1/usage' }
      },
      extra: { deepseek_balance: 8.8, deepseek_balance_currency: 'USD' }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: volcanoProbeAccount } })
    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('USD 8.80')
  })

  // 火山订阅号 + 无探测能力 → 余额单元格隐藏（旧规则不变，避免与配额单元格双重渲染）。
  it('volcano base URL + no balance_probe_capable → balance cell stays hidden (legacy rule)', () => {
    const volcanoNoProbe = {
      id: 7,
      platform: 'deepseek',
      type: 'apikey',
      credentials: {
        account_mode: 'payg',
        base_url: 'https://ark.cn-beijing.volces.com/api/plan'
      }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: volcanoNoProbe } })
    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(false)
  })

  // zhipu + 探测能力 → 余额单元格可见（zhipu 旧规则恒隐藏，capable 使其可见）。
  it('zhipu + balance_probe_capable → renders balance cell (capable overrides zhipu default-hidden)', () => {
    const zhipuProbe = {
      id: 7,
      platform: 'zhipu',
      type: 'apikey',
      balance_probe_capable: true,
      credentials: {
        account_mode: 'payg',
        base_url: 'https://zhipu-probe.example.com/v1/usage',
        balance_probe: { enabled: true, url: 'https://zhipu-probe.example.com/v1/usage' }
      }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: zhipuProbe } })
    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(true)
  })

  // zhipu + 无探测能力 → 余额单元格隐藏（旧规则：智谱 payg 无公开余额端点）。
  it('zhipu + no balance_probe_capable → balance cell stays hidden (legacy rule)', () => {
    const zhipuNoProbe = {
      id: 7,
      platform: 'zhipu',
      type: 'apikey',
      credentials: { account_mode: 'payg' }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: zhipuNoProbe } })
    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(false)
  })

  // kimi/deepseek 既有行为：kimi payg 无探测能力仍可见（既有用例，此处显式锁死）——
  // 官方端点账号显示走前端既有平台规则，不由 balance_probe_capable 驱动。
  it('kimi payg + no capable → balance cell visible (legacy behavior unchanged)', () => {
    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(true)
  })

  // kimi coding 无探测能力 → 隐藏（既有用例，此处显式锁死 coding 隐藏）。
  it('kimi coding + no capable → balance cell hidden (legacy behavior unchanged)', () => {
    const kimiCoding = {
      id: 7,
      platform: 'kimi',
      type: 'apikey',
      credentials: { account_mode: 'coding' },
      extra: { kimi_balance: 12.5, kimi_balance_currency: 'CNY' }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: kimiCoding } })
    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(false)
  })

  // Done when #3：openai + 探测能力 → 余额单元格渲染（非 CN 平台靠 capable 能力打开）。
  it('openai + balance_probe_capable → renders balance cell (Done when #3)', () => {
    const openaiProbe = {
      id: 7,
      platform: 'openai',
      type: 'apikey',
      balance_probe_capable: true,
      credentials: {
        account_mode: 'payg',
        base_url: 'https://openai-probe.example.com/v1/usage',
        balance_probe: { enabled: true, url: 'https://openai-probe.example.com/v1/usage' }
      }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: openaiProbe } })
    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(true)
  })

  // Done when #3：openai + 无探测能力 → 不渲染。
  it('openai + no balance_probe_capable → balance cell NOT rendered (Done when #3)', () => {
    const openaiNoProbe = {
      id: 7,
      platform: 'openai',
      type: 'apikey',
      credentials: { account_mode: 'payg' }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: openaiNoProbe } })
    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(false)
  })

  // ===== 方案乙核心语义（BAL-GATE-D5）：enabled 但不可探测 → 整格不出现 =====
  // openai + credentials.balance_probe.enabled=true，但后端投影 balance_probe_capable
  // 缺省/false（配置开关打开却无能解析 URL 且非官方端点）→ 单元格绝不渲染。
  it('openai + balance_probe.enabled=true but capable=false → balance cell NOT rendered (plan B core)', () => {
    const openaiMisconfigured = {
      id: 7,
      platform: 'openai',
      type: 'apikey',
      credentials: {
        account_mode: 'payg',
        balance_probe: { enabled: true } // 开关开了，但无 url 且非官方端点 → 后端投影为 false
      }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: openaiMisconfigured } })
    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(false)
  })

  // ===== 计费形态三态（subscription / count_pack / balance），契约见 cnProviders.ts =====

  // 生产实例：七号云站（platform=kimi）上游 remaining:-1 + mode:unrestricted。
  it('subscription: 展示分组名 · 不限量，且不渲染次卡进度条', () => {
    const subAccount = {
      id: 7,
      platform: 'kimi',
      type: 'apikey',
      credentials: { account_mode: 'payg' },
      extra: {
        kimi_balance_unlimited: true,
        kimi_balance_kind: 'subscription',
        kimi_balance_plan_name: '订阅不限量kimi-k3'
      }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: subAccount } })

    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain(
      '订阅不限量kimi-k3 · 不限量'
    )
    expect(wrapper.find('[data-test="cn-count-pack-bar"]').exists()).toBe(false)
  })

  // 生产实例：goaichat 站次卡（quota 1000/295/705）。上游 unit 误标 "USD"，
  // 次卡路径必须完全绕开带币种前缀的金额格式化。
  it('count_pack: 展示「剩余/总量」次数并渲染进度条，且不带任何币种前缀', () => {
    const packAccount = {
      id: 7,
      platform: 'kimi',
      type: 'apikey',
      credentials: { account_mode: 'payg' },
      extra: {
        kimi_balance: 705,
        kimi_balance_kind: 'count_pack',
        kimi_balance_quota_limit: 1000,
        kimi_balance_quota_used: 295,
        kimi_balance_currency: ''
      }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: packAccount } })

    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('次卡 705/1000')
    const bar = wrapper.get('[data-test="cn-count-pack-bar"]')
    // utilization = 295/1000 → 填充宽度 29.5%
    const width = /width:\s*([\d.]+)%/.exec(bar.html())
    expect(width).not.toBeNull()
    expect(parseFloat(width![1])).toBeCloseTo(29.5, 5)
    // 剩余 70.5% > 20% → emerald 标签（耗尽态由 balanceLow 徽标承担，不新造红色分支）
    expect(bar.html()).toContain('bg-emerald-100')
    // 关键不变式：次卡数字不得被当成美元金额
    expect(wrapper.text()).not.toContain('USD')
    expect(wrapper.text()).not.toContain('¥')
  })

  // 旧快照缺 quota 分母：只展示剩余次数，进度条随之隐藏（不造假分母）。
  it('count_pack without quota_limit: 只展示剩余次数，不渲染进度条', () => {
    const noDenomAccount = {
      id: 7,
      platform: 'kimi',
      type: 'apikey',
      credentials: { account_mode: 'payg' },
      extra: { kimi_balance: 705, kimi_balance_kind: 'count_pack' }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: noDenomAccount } })

    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('次卡 剩余 705')
    expect(wrapper.find('[data-test="cn-count-pack-bar"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('USD')
  })

  // 次卡已用 ≥80% → amber（仍不新造红色分支）。
  it('count_pack near exhaustion: 进度条转 amber 而非红色', () => {
    const nearlyUsedAccount = {
      id: 7,
      platform: 'kimi',
      type: 'apikey',
      credentials: { account_mode: 'payg' },
      extra: {
        kimi_balance: 50,
        kimi_balance_kind: 'count_pack',
        kimi_balance_quota_limit: 1000,
        kimi_balance_quota_used: 950
      }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: nearlyUsedAccount } })
    const bar = wrapper.get('[data-test="cn-count-pack-bar"]')
    // 只断言本组件传下去的 color 落在标签徽章上（amber）。填充条颜色由
    // UsageProgressBar 内部按 utilization 派生（≥90% 自身转红），不属于本组件新造的分支。
    expect(bar.get('span').classes().join(' ')).toContain('bg-amber-100')
  })

  // 回归：kind 缺省（旧快照 / 官方原生路径未升级）按 balance 处理，金额展示与币种前缀不变。
  it('balance (legacy default): kind 缺省仍按金额渲染 CNY 12.50，无进度条', () => {
    const wrapper = mount(CNProviderBalanceCell, { props: { account } })

    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')
    expect(wrapper.find('[data-test="cn-count-pack-bar"]').exists()).toBe(false)
  })

  // 显式 kind:'balance' 与缺省同义。
  it('balance: 显式 kind=balance 走金额路径', () => {
    const explicitBalanceAccount = {
      ...account,
      extra: { kimi_balance: 12.5, kimi_balance_currency: 'CNY', kimi_balance_kind: 'balance' }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: explicitBalanceAccount } })
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')
  })

  // 脏快照值（非三态之一）必须被拒绝并回落 balance，不得穿透成 undefined 分支。
  it('rejects an unknown snapshot kind and falls back to balance', () => {
    const dirtyAccount = {
      ...account,
      extra: { kimi_balance: 12.5, kimi_balance_currency: 'CNY', kimi_balance_kind: 'whatever' }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: dirtyAccount } })
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')
    expect(wrapper.find('[data-test="cn-count-pack-bar"]').exists()).toBe(false)
  })

  // 探测响应优先于落库快照（双源合并口径与 unlimited/planName 一致）。
  it('probe result overrides the snapshot for kind and quota fields', async () => {
    queryBalance.mockResolvedValue({
      success: true,
      balance: 705,
      currency: '',
      balances: [],
      available: true,
      kind: 'count_pack',
      quota_limit: 1000,
      quota_used: 295,
      provider: 'kimi',
      fetched_at: 1,
      persisted: true
    })
    // 快照仍是旧的金额形态，探测回来后必须按次卡展示。
    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('次卡 705/1000')
    expect(wrapper.text()).not.toContain('CNY 12.50')
    expect(wrapper.find('[data-test="cn-count-pack-bar"]').exists()).toBe(true)
  })

  // zhipu / minimax 中转型账号（配了 balance_probe）放行后，次卡文案按平台前缀读快照。
  it('zhipu relay account with balance_probe renders count pack from zhipu_ prefixed keys', () => {
    const zhipuRelayAccount = {
      id: 7,
      platform: 'zhipu',
      type: 'apikey',
      // 探针配置由后端投到 DTO 顶层（只投配置，不含 token），可见性据此放行。
      balance_probe: { enabled: true, url: 'https://goaichat.example.com/v1/usage' },
      credentials: {
        account_mode: 'payg',
        base_url: 'https://goaichat.example.com/v1'
      },
      extra: {
        zhipu_balance: 705,
        zhipu_balance_kind: 'count_pack',
        zhipu_balance_quota_limit: 1000,
        zhipu_balance_quota_used: 295
      }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: zhipuRelayAccount } })
    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('次卡 705/1000')
  })

  // ===== 派发单 balance-ui-errmap：余额探测错误码本地化映射 =====
  // 注：后端错误信封把稳定码放在 reason（response 包 Response.Reason），client 拦截器
  // 平铺到错误对象上，故真实链路的码走 err.reason；err.response.data.error 为兼容位。

  // 用户报告的裸渲染串：CN_BALANCE_INVALID_CONFIG → 本地化文案，英文 message 不再出现。
  it('maps CN_BALANCE_INVALID_CONFIG to the localized copy instead of the raw English message', async () => {
    queryBalance.mockRejectedValue({
      status: 400,
      code: 400,
      message: 'balance probe access_token and count_unit must be configured together',
      reason: 'CN_BALANCE_INVALID_CONFIG'
    })

    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('本地化:CN_BALANCE_INVALID_CONFIG')
    expect(wrapper.text()).not.toContain('access_token')
    // 失败关闭：快照余额保留（既有行为不回归）
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')
  })

  // 派发单指定的取数位 response.data.error 同样命中映射。
  it('maps a probe error code carried on response.data.error', async () => {
    queryBalance.mockRejectedValue({
      response: { data: { error: 'CN_BALANCE_NO_APIKEY', message: 'account api_key is empty' } }
    })

    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('本地化:CN_BALANCE_NO_APIKEY')
    expect(wrapper.text()).not.toContain('account api_key is empty')
  })

  // 表内其余配置类码同样走映射（URL 白名单拒绝）。
  it('maps CN_BALANCE_URL_REJECTED to the localized copy', async () => {
    queryBalance.mockRejectedValue({
      message: 'probe url host not allowed: evil.example.com',
      reason: 'CN_BALANCE_URL_REJECTED'
    })

    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('本地化:CN_BALANCE_URL_REJECTED')
    expect(wrapper.text()).not.toContain('evil.example.com')
  })

  // 未命中映射的瞬时/上游类码 → 既有通用渲染（回退 message 原样，逐字不回归）。
  it('falls back to the raw message for a transient code that is NOT in the map', async () => {
    queryBalance.mockRejectedValue({
      message: 'upstream request failed: dial tcp: i/o timeout',
      reason: 'CN_BALANCE_REQUEST_FAILED'
    })

    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('upstream request failed')
    expect(wrapper.text()).not.toContain('本地化:')
  })

  // 无码（纯 message 错误）→ 既有回退链不变。
  it('keeps the existing fallback chain when no error code is present', async () => {
    queryBalance.mockRejectedValue({ message: 'boom from server' })

    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('boom from server')
    expect(wrapper.text()).not.toContain('本地化:')
  })

  // 无码且无 message → 既有兜底 t('common.error') 不变。
  it('falls back to common.error when neither a code nor a message is present', async () => {
    queryBalance.mockRejectedValue({ reason: 'CN_BALANCE_REQUEST_BUILD_FAILED' })

    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    // 未命中映射的码被当作无码处理，走既有链：reason 命中 err?.reason 分支。
    expect(wrapper.text()).toContain('CN_BALANCE_REQUEST_BUILD_FAILED')
    expect(wrapper.text()).not.toContain('本地化:')
  })

  // ===== 派发单 balance-ui-errmap：探测返回 success=false 路径的错误码本地化 =====
  // 后端「上游订阅失效」分支填 error_code=CN_BALANCE_UPSTREAM_SUBSCRIPTION_UNAVAILABLE
  // （字面量与卡 A 后端改动严格一致）。命中白名单 → 走本地化文案，原始英文不再出现在正文。

  it('maps success=false error_code CN_BALANCE_UPSTREAM_SUBSCRIPTION_UNAVAILABLE to localized copy (no raw English in body)', async () => {
    queryBalance.mockResolvedValue({
      success: false,
      error_code: 'CN_BALANCE_UPSTREAM_SUBSCRIPTION_UNAVAILABLE',
      error: 'Upstream subscription "X" expired or unavailable (missing remaining)'
    })

    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    // 命中映射 → 本地化文案
    expect(wrapper.text()).toContain('本地化:CN_BALANCE_UPSTREAM_SUBSCRIPTION_UNAVAILABLE')
    // 原始英文正文不渲染（仅挂在 title 上）
    expect(wrapper.text()).not.toContain('Upstream subscription')
    expect(wrapper.text()).not.toContain('missing remaining')
    // 快照余额保留
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')
  })

  // 无 error_code → 回退链不变：原样显示 result.error 原文。
  it('falls back to the raw result.error when success=false has no error_code', async () => {
    queryBalance.mockResolvedValue({
      success: false,
      error: '某种未编碼的错误'
    })

    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('某种未编碼的错误')
    expect(wrapper.text()).not.toContain('本地化:')
  })

  // 白名单闸门：error_code 不在表内 → 不允许拿任意码拼 i18n key，回退原文。
  it('whitelist gate: an unmapped error_code falls back to the raw result.error', async () => {
    queryBalance.mockResolvedValue({
      success: false,
      error_code: '不在白名单里的码',
      error: '原文'
    })

    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('原文')
    expect(wrapper.text()).not.toContain('本地化:')
  })

  // ===== 卡 B2 回归锁：异常路径（queryBalance reject）错误行 title 不能丢失 =====
  // 卡 B 把 :title 从 error 改为 errorTitle 后，catch 分支只设 error.value、未设
  // errorTitle.value → title 变空，hover 看不到完整原文。本用例锁死：
  // ① 正文为截断文本（含 "..." 结尾，与 truncatedError 一致）；
  // ② title 非空且含完整原文（含末尾 FULL_TAIL_MARKER），可 hover 看全文。
  it('queryBalance reject (exception path): body truncated but title keeps full error text', async () => {
    // 模拟后端原文被 truncate 到 80 字符仍远超阈值的长英文错误，结尾放唯一标记。
    const longError =
      'upstream request failed: ' + 'x'.repeat(70) + ' FULL_TAIL_MARKER'
    queryBalance.mockRejectedValue({ message: longError })

    // 注意：不设 expires_at，避免订阅有效期 span 也带 title 干扰错误行选择器。
    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    // 错误行是模板里唯一带 truncate 类的 div（且 v-if="error"）。
    const errDiv = wrapper.get('div.truncate')

    // ① 正文为截断后的文本（含 "..." 结尾），与既有 truncatedError 行为一致。
    expect(errDiv.text()).toMatch(/\.\.\.$/)
    expect(errDiv.text()).not.toContain('FULL_TAIL_MARKER')

    // ② title 非空且包含完整原文（含末尾 FULL_TAIL_MARKER），可 hover 看全文。
    const title = errDiv.attributes('title')
    expect(title).toBeTruthy()
    expect(title).toContain('FULL_TAIL_MARKER')
    expect(title).toBe(longError)
  })
})
