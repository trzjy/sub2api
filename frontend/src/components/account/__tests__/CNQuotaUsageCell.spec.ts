import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CNQuotaUsageCell from '../CNQuotaUsageCell.vue'
import UsageProgressBar from '../UsageProgressBar.vue'
import type { Account } from '@/types'

const { queryQuota } = vi.hoisted(() => ({
  queryQuota: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    cnProviders: { queryQuota }
  }
}))

// 保留 vue-i18n 真实导出：UsageProgressBar / @/utils/format 依赖 @/i18n 的模块级
// createI18n。t 带 named params 时把参数序列化进返回值，便于断言聚合数值。
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key} ${JSON.stringify(params)}` : key
    })
  }
})

// TH 订阅账号（账号 207 同型）：platform 挂 kimi，base_url 指向 tokenharbor.ai，
// extra 含 th_pass_snapshot（has_pass=true，含 renews_at 续期 + reset_at 周期重置 +
// plan_used_pct / plan_exhausted 官方津贴）+ th_usage_snapshot（today/7d 两键，无 30d）。
const thPassAccount = {
  id: 207,
  platform: 'kimi',
  type: 'apikey',
  credentials: { base_url: 'https://tokenharbor.ai/v1' },
  extra: {
    th_pass_snapshot: {
      has_pass: true,
      pass_name: 'Agent Pass',
      renews_at: new Date(Date.now() + 29 * 24 * 60 * 60 * 1000).toISOString(),
      reset_at: new Date(Date.now() + 4 * 24 * 60 * 60 * 1000).toISOString(),
      plan_used_pct: 62,
      plan_exhausted: false,
      spend_after_allowance: false,
      fetched_at: new Date().toISOString()
    },
    th_usage_snapshot: {
      windows: {
        today: { requests: 10, tokens_in: 100, tokens_out: 200 },
        '7d': { requests: 120, tokens_in: 1000, tokens_out: 2000 }
      },
      fetched_at: new Date().toISOString()
    }
  }
} as unknown as Account

// TH 免费账号（无订阅）：只渲染 7 天滚动已用计数（窗口仅 today/7d）。
const thFreeAccount = {
  id: 187,
  platform: 'kimi',
  type: 'apikey',
  credentials: { base_url: 'https://tokenharbor.ai/v1' },
  extra: {
    th_usage_snapshot: {
      windows: {
        today: { requests: 1, tokens_in: 1, tokens_out: 1 },
        '7d': { requests: 14, tokens_in: 700, tokens_out: 700 }
      },
      fetched_at: new Date().toISOString()
    }
  }
} as unknown as Account

// Kira 免费账号：base_url 指向 kiraai.vn（platform 不反映真实上游）。
const kiraResetAtIso = new Date(Date.now() + 2 * 60 * 60 * 1000).toISOString()
const kiraAccount = {
  id: 208,
  platform: 'kimi',
  type: 'apikey',
  credentials: { base_url: 'https://kiraai.vn/api/v1' },
  extra: {
    kira_usage_snapshot: {
      window: 'daily',
      used_percent: 30,
      used_tokens: 1000,
      limit_tokens: 6000000,
      reset_at: kiraResetAtIso,
      fetched_at: new Date().toISOString()
    },
    kimi_balance: 38406
  }
} as unknown as Account

describe('CNQuotaUsageCell', () => {
  beforeEach(() => {
    queryQuota.mockReset()
  })

  // L5 互斥：TH 有 Pass → 只渲染订阅用量窗口（Pass 档位/到期倒计时/today 切换/状态），
  // 免费窗口与 Kira 窗口不得同时出现。
  it('renders only the subscription window for a TH account with has_pass=true', () => {
    const wrapper = mount(CNQuotaUsageCell, { props: { account: thPassAccount } })

    expect(wrapper.find('[data-test="cn-quota-pass-window"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="cn-quota-free-window"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-quota-kira-window"]').exists()).toBe(false)

    // Pass 档位名直接来自快照
    expect(wrapper.get('[data-test="cn-quota-pass-name"]').text()).toBe('Agent Pass')
    // 徽标数据源已从 renews_at 切到 reset_at（7 天周期重置）
    expect(wrapper.get('[data-test="cn-quota-pass-renews"]').text()).toContain(
      'admin.accounts.cnProviders.passResetsAt'
    )
    // 单轨收敛：Pass 窗口头部的重复状态徽标（旧 lifecycle/plan_exhausted 派生）已删除，
    // 订阅/免费状态的唯一出口 = 下方维度面板（cn-quota-dimensions）的对应维度行。
    expect(wrapper.find('[data-test="cn-quota-status"]').exists()).toBe(false)
    // 默认今日窗口：requests=10
    const stats = wrapper.get('[data-test="cn-quota-window-stats"]').text()
    expect(stats).toContain('"requests":"10"')
    // 官方 Pass 津贴进度条（plan_used_pct=62）渲染
    const planBar = wrapper.find('[data-test="cn-quota-pass-plan-bar"]')
    expect(planBar.exists()).toBe(true)
    expect(planBar.findComponent(UsageProgressBar).props('utilization')).toBe(62)
  })

  // today/7d/renew 切换：today/7d 渲染官方 CSV 聚合，renew 档显示续期倒计时。
  it('switches today/7d/renew window: aggregates + renewal countdown', async () => {
    const wrapper = mount(CNQuotaUsageCell, { props: { account: thPassAccount } })

    const toggles = wrapper.findAll('[data-test="cn-quota-window-toggle"]')
    expect(toggles).toHaveLength(3)
    expect(toggles.map((b) => b.text())).toEqual([
      'admin.accounts.cnProviders.windowToday',
      'admin.accounts.cnProviders.window7d',
      'admin.accounts.cnProviders.windowRenew'
    ])

    // 7 天档：聚合计数 + reset_at 周期重置倒计时（passResetsAt 文案）
    await toggles[1].trigger('click')
    const stats7d = wrapper.get('[data-test="cn-quota-window-stats"]').text()
    expect(stats7d).toContain('"requests":"120"')
    expect(stats7d).toContain('admin.accounts.cnProviders.passResetsAt')

    // renew 档：订阅续期倒计时（passRenewsAt，同源同值）
    await toggles[2].trigger('click')
    expect(wrapper.get('[data-test="cn-quota-window-stats"]').text()).toContain(
      'admin.accounts.cnProviders.passRenewsAt'
    )

    await toggles[0].trigger('click')
    expect(wrapper.get('[data-test="cn-quota-window-stats"]').text()).toContain('"requests":"10"')
  })

  // 单轨口径（方案 §3.4）：订阅维度 status=exhausted → 进度条仍渲染（status ≠ unknown），
  // 但"已耗尽"状态唯一出口 = 维度面板行（旧 cn-quota-status 徽标已删除）。
  it('renders the plan bar for an exhausted subscription dimension with status shown only in the panel', () => {
    const exhaustedPlan = {
      ...thPassAccount,
      extra: {
        ...thPassAccount.extra,
        th_pass_snapshot: {
          ...thPassAccount.extra.th_pass_snapshot,
          plan_exhausted: true
        }
      },
      quota_dimensions: [
        { kind: 'subscription', scope: 'account', target: '', status: 'exhausted', servable: 'no', source: 'th_pass_snapshot', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: exhaustedPlan } })

    // 旧窗口头部状态徽标已删除（单轨收敛）
    expect(wrapper.find('[data-test="cn-quota-status"]').exists()).toBe(false)
    // 进度条仍渲染：订阅维度 status=exhausted（≠ unknown）
    expect(wrapper.get('[data-test="cn-quota-pass-plan-bar"]').exists()).toBe(true)
    // "已耗尽"状态唯一出口 = 维度面板
    expect(wrapper.get('[data-test="cn-quota-dimensions"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.cnProviders.statusExhausted')
  })

  // 官方津贴 0%：plan_used_pct=0 是有效值（非缺失），渲染 0% 进度条而非 '—'。
  it('renders 0% (not an em dash) when plan_used_pct is 0', () => {
    const zeroPlan = {
      ...thPassAccount,
      extra: {
        ...thPassAccount.extra,
        th_pass_snapshot: {
          ...thPassAccount.extra.th_pass_snapshot,
          plan_used_pct: 0
        }
      }
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: zeroPlan } })

    const planBar = wrapper.get('[data-test="cn-quota-pass-plan-bar"]')
    const bar = planBar.findComponent(UsageProgressBar)
    expect(bar.props('utilization')).toBe(0)
    expect(bar.props('unknownUsage')).toBe(false)
    expect(planBar.text()).toContain('0%')
    expect(planBar.text()).not.toContain('—')
  })

  // 快照缺失 plan_used_pct：plan_exhausted=true 触发进度条渲染，走 unknown 路径显示 '—'，
  // 不得假显 0%（plan_used_pct 缺失时 unknownUsage=true 由组件推导）。
  it('renders an em dash for the plan bar when plan_used_pct is absent', () => {
    const base = thPassAccount.extra.th_pass_snapshot
    const missingPct = {
      ...thPassAccount,
      extra: {
        ...thPassAccount.extra,
        th_pass_snapshot: {
          has_pass: true,
          pass_name: base.pass_name,
          renews_at: base.renews_at,
          reset_at: base.reset_at,
          plan_exhausted: true,
          spend_after_allowance: base.spend_after_allowance,
          fetched_at: base.fetched_at
        }
      }
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: missingPct } })

    const planBar = wrapper.get('[data-test="cn-quota-pass-plan-bar"]')
    const bar = planBar.findComponent(UsageProgressBar)
    expect(bar.props('unknownUsage')).toBe(true)
    expect(planBar.text()).toContain('—')
    expect(planBar.text()).not.toContain('%')
  })

  // 单轨口径（方案 §3.4）：恢复倒计时由订阅维度 status==='exhausted' 驱动，recovery_at
  // 取值路径不变（仍读 cn_quota_lifecycle）；旧窗口头部 cn-quota-status 已删除。
  it('shows the recovery countdown driven by the subscription dimension exhausted status', () => {
    const exhausted = {
      ...thPassAccount,
      extra: {
        ...thPassAccount.extra,
        cn_quota_lifecycle: {
          state: 'exhausted',
          recovery_at: new Date(Date.now() + 90 * 60 * 1000).toISOString()
        }
      },
      quota_dimensions: [
        { kind: 'subscription', scope: 'account', target: '', status: 'exhausted', servable: 'no', source: 'th_pass_snapshot', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: exhausted } })

    // 旧窗口头部状态徽标已删除（单轨收敛）
    expect(wrapper.find('[data-test="cn-quota-status"]').exists()).toBe(false)
    // 恢复倒计时由订阅维度 exhausted 驱动，recovery_at 仍读 cn_quota_lifecycle
    expect(wrapper.get('[data-test="cn-quota-recovery"]').text()).toContain(
      'admin.accounts.cnProviders.recoverAt'
    )
  })

  // L5 互斥：TH 无 Pass → 只渲染免费窗口（7 天滚动、仅已用计数），无订阅窗口。
  it('renders only the free rolling-7d window for a TH account without pass', () => {
    const wrapper = mount(CNQuotaUsageCell, { props: { account: thFreeAccount } })

    expect(wrapper.find('[data-test="cn-quota-free-window"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="cn-quota-pass-window"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-quota-kira-window"]').exists()).toBe(false)

    const label = wrapper.get('[data-test="cn-quota-free-7d"]').text()
    expect(label).toContain('admin.accounts.cnProviders.freeRolling7d')
    expect(label).toContain('"requests":"14"')
    // 无订阅：不渲染窗口切换按钮
    expect(wrapper.find('[data-test="cn-quota-window-toggle"]').exists()).toBe(false)
  })

  // L5 互斥：Kira → 当日进度条 + 已用/上限 + VND 余额 + reset 倒计时，无 TH 内容。
  it('renders only the Kira daily window with VND balance', () => {
    const wrapper = mount(CNQuotaUsageCell, { props: { account: kiraAccount } })

    expect(wrapper.find('[data-test="cn-quota-kira-window"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="cn-quota-pass-window"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-quota-free-window"]').exists()).toBe(false)

    // 进度条由共享 UsageProgressBar 渲染：utilization 来自快照 used_percent
    const bar = wrapper.findComponent(UsageProgressBar)
    expect(bar.exists()).toBe(true)
    expect(bar.props('utilization')).toBe(30)
    expect(bar.props('resetsAt')).toBe(kiraResetAtIso)
    // reset_at 倒计时（2 小时后）
    expect(wrapper.text()).toContain("1h 59m")
    // 已用/上限
    expect(wrapper.get('[data-test="cn-quota-kira-used-limit"]').text()).toContain('"used":1000')
    expect(wrapper.get('[data-test="cn-quota-kira-used-limit"]').text()).toContain('"limit":6000000')
    // VND 余额（<platform>_balance）
    expect(wrapper.get('[data-test="cn-quota-kira-vnd"]').text()).toContain('"balance":"38406"')
  })

  // 手动查询：走既有 queryQuota 接口；请求中按钮禁用；成功后 daily 档立即生效。
  it('probes via the existing quota endpoint with a disabled button while loading', async () => {
    let resolveProbe!: (value: unknown) => void
    queryQuota.mockImplementation(
      () => new Promise((resolve) => { resolveProbe = resolve })
    )
    const wrapper = mount(CNQuotaUsageCell, { props: { account: kiraAccount } })

    const probeButton = wrapper.get('[data-test="cn-quota-probe"]')
    expect(probeButton.attributes('disabled')).toBeUndefined()

    await probeButton.trigger('click')
    expect(queryQuota).toHaveBeenCalledWith(kiraAccount.id)
    // 请求中：按钮禁用，重复点击不再触发
    expect(wrapper.get('[data-test="cn-quota-probe"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[data-test="cn-quota-probe"]').trigger('click')
    expect(queryQuota).toHaveBeenCalledTimes(1)

    resolveProbe({
      success: true,
      tiers: [{ window: 'daily', used_percent: 42, reset_at: '' }]
    })
    await flushPromises()
    // 请求结束：按钮恢复可用，探测返回的 daily 档立即渲染
    expect(wrapper.get('[data-test="cn-quota-probe"]').attributes('disabled')).toBeUndefined()
    expect(wrapper.findComponent(UsageProgressBar).props('utilization')).toBe(42)
  })

  // 探测失败：保留快照展示，仅追加错误行。
  it('keeps the snapshot display and shows an error line when the probe fails', async () => {
    queryQuota.mockResolvedValue({ success: false, error: 'UPSTREAM_TIMEOUT' })
    const wrapper = mount(CNQuotaUsageCell, { props: { account: kiraAccount } })

    await wrapper.get('[data-test="cn-quota-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('UPSTREAM_TIMEOUT')
    // 快照值未被失败探测覆盖
    expect(wrapper.findComponent(UsageProgressBar).props('utilization')).toBe(30)
  })

  // 非 TH/Kira 账号不渲染该组件（互斥只作用于 TH/Kira 上游）。
  it('stays hidden for a plain CN account', () => {
    const plain = {
      id: 1,
      platform: 'zhipu',
      type: 'apikey',
      credentials: { account_mode: 'coding', base_url: 'https://open.bigmodel.cn/api/paas/v4' },
      extra: {}
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: plain } })
    expect(wrapper.find('[data-test="cn-quota-usage"]').exists()).toBe(false)
    expect(queryQuota).not.toHaveBeenCalled()
  })

  // ===== 方案 §3.4 / R5-F3：quota_dimensions 驱动维度渲染 =====

  // 渲染账号实际拥有的全部 account 级维度（含既有窗口分支未覆盖的 paid/subscription），
  // 维度存在/状态以 quota_dimensions 为唯一事实源；exhausted/remaining/unknown 三态徽标。
  it('renders every account-scope dimension from quota_dimensions with status badges', () => {
    const dimAccount = {
      ...kiraAccount,
      quota_dimensions: [
        { kind: 'paid', scope: 'account', target: '', status: 'exhausted', servable: 'no', source: 'kira_vnd_balance', observed_at: '2026-10-09T02:00:00Z' },
        { kind: 'free', scope: 'account', target: '', status: 'remaining', servable: 'yes', source: 'kira_free', observed_at: '2026-10-09T02:00:00Z' },
        { kind: 'subscription', scope: 'account', target: '', status: 'unknown', servable: 'unknown', source: 'th_wallet', observed_at: null }
      ]
    } as unknown as Account

    const wrapper = mount(CNQuotaUsageCell, { props: { account: dimAccount } })

    expect(wrapper.find('[data-test="cn-quota-dimensions"]').exists()).toBe(true)
    const dims = wrapper.findAll('[data-test="cn-quota-dimension"]')
    expect(dims).toHaveLength(3)
    const text = wrapper.text()
    // 全部 kind 渲染
    expect(text).toContain('admin.accounts.cnProviders.dimensionKindPaid')
    expect(text).toContain('admin.accounts.cnProviders.dimensionKindFree')
    expect(text).toContain('admin.accounts.cnProviders.dimensionKindSubscription')
    // 三态徽标
    expect(text).toContain('admin.accounts.cnProviders.statusExhausted')
    expect(text).toContain('admin.accounts.cnProviders.dimensionStatusRemaining')
    expect(text).toContain('admin.accounts.cnProviders.dimensionStatusUnknown')
  })

  // 无 quota_dimensions 时不渲染维度面板（两态兼容，缺省为空）。
  it('does not render the dimension panel when quota_dimensions is absent', () => {
    const wrapper = mount(CNQuotaUsageCell, { props: { account: kiraAccount } })
    expect(wrapper.find('[data-test="cn-quota-dimensions"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-quota-model-exhausted"]').exists()).toBe(false)
  })

  // TH 模型级 free-tier 耗尽逐模型展示：仅 scope=model 且 status=exhausted 的条目
  // 渲染为逐模型行（模型 ID + 耗尽徽标）；remaining 模型不展示。
  it('shows per-model rows for exhausted model-scope free-tier dimensions', () => {
    const modelExhausted = {
      ...thPassAccount,
      quota_dimensions: [
        { kind: 'free', scope: 'model', target: 'gpt-4o', status: 'exhausted', servable: 'no', source: 'th_model_free', observed_at: '2026-10-09T02:00:00Z' },
        { kind: 'free', scope: 'model', target: 'claude-3-5-sonnet', status: 'exhausted', servable: 'no', source: 'th_model_free', observed_at: '2026-10-09T02:00:00Z' },
        { kind: 'free', scope: 'model', target: 'gpt-4o-mini', status: 'remaining', servable: 'yes', source: 'th_model_free', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as unknown as Account

    const wrapper = mount(CNQuotaUsageCell, { props: { account: modelExhausted } })

    expect(wrapper.find('[data-test="cn-quota-model-exhausted"]').exists()).toBe(true)
    // 仅 exhausted 模型逐行展示（remaining 模型不出现）
    const rows = wrapper.findAll('[data-test="cn-quota-model-row"]')
    expect(rows).toHaveLength(2)
    const ids = wrapper.findAll('[data-test="cn-quota-model-id"]').map((el) => el.text())
    expect(ids).toEqual(['gpt-4o', 'claude-3-5-sonnet'])
    expect(wrapper.text()).toContain('admin.accounts.cnProviders.statusExhausted')
    expect(wrapper.text()).not.toContain('gpt-4o-mini')
  })

  // Kira paid 行（方案 §3.4 v24）：窗口内孤立 VND 状态徽标已删除（单轨口径，付费行
  // 不是状态出口）；金额取数路径不变；paid 维度状态唯一出口 = 规范化维度面板。
  it('upgrades the Kira VND line without an inline status badge (status moves to the dimension panel)', () => {
    const kiraPaid = {
      ...kiraAccount,
      quota_dimensions: [
        { kind: 'paid', scope: 'account', target: '', status: 'remaining', servable: 'yes', source: 'kira_vnd_balance', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as unknown as Account

    const wrapper = mount(CNQuotaUsageCell, { props: { account: kiraPaid } })

    const vnd = wrapper.get('[data-test="cn-quota-kira-vnd"]')
    // 孤立 VND 状态徽标已删除（负例）
    expect(wrapper.find('[data-test="cn-quota-kira-vnd-status"]').exists()).toBe(false)
    // 既有 VND 金额展示保留（取数路径不变）
    expect(vnd.text()).toContain('"balance":"38406"')
    // paid 维度状态唯一出口 = 规范化维度面板（remaining）
    expect(wrapper.find('[data-test="cn-quota-dimensions"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.cnProviders.dimensionStatusRemaining')
  })

  // ===== 单轨展示不变式回归（派发单 UNIFY-QUOTA-UI-20261010 / §3.4）=====

  // ===== 派发单 F1 / 方案 §3.4 v24：单轨展示不变式（灰度进度条 + 陈旧标注）=====

  // a) 维度 unknown + 数值存在 → 渲染进度条（灰度）+ 陈旧标注，不渲染 unknown 徽标
  it('a) renders a gray stale plan bar (no unknown badge) when the subscription dimension is unknown but a value exists', () => {
    const unknownSub = {
      ...thPassAccount,
      quota_dimensions: [
        { kind: 'subscription', scope: 'account', target: '', status: 'unknown', servable: 'unknown', source: 'th_pass_snapshot', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: unknownSub } })
    // 数值存在（plan_used_pct=62）→ 进度条渲染（灰度）
    const planBar = wrapper.get('[data-test="cn-quota-pass-plan-bar"]')
    expect(planBar.exists()).toBe(true)
    const bar = planBar.findComponent(UsageProgressBar)
    expect(bar.props('stale')).toBe(true)
    expect(bar.props('unknownUsage')).toBe(false)
    expect(planBar.text()).toContain('62%')
    // 陈旧标注（observed_at 相对时间）
    const note = wrapper.find('[data-test="cn-quota-stale-note"]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toContain('admin.accounts.cnProviders.staleAgo')
    // 不渲染 unknown 徽标（已探测、数值存在 → 维度面板不再显示「未知」）
    expect(wrapper.find('[data-test="cn-quota-dimension-status"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('admin.accounts.cnProviders.dimensionStatusUnknown')
  })

  // a) Kira 免费维度 unknown + 数值存在 → 灰度进度条 + 陈旧标注 + 不渲染 unknown 徽标
  it('a) renders a gray stale Kira free bar (no unknown badge) when the free dimension is unknown but a value exists', () => {
    const unknownFree = {
      ...kiraAccount,
      quota_dimensions: [
        { kind: 'free', scope: 'account', target: '', status: 'unknown', servable: 'unknown', source: 'kira_usage_snapshot', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: unknownFree } })
    const bar = wrapper.findComponent(UsageProgressBar)
    expect(bar.exists()).toBe(true)
    expect(bar.props('stale')).toBe(true)
    expect(bar.props('unknownUsage')).toBe(false)
    expect(wrapper.text()).toContain('30%')
    // 陈旧标注
    const note = wrapper.find('[data-test="cn-quota-stale-note"]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toContain('admin.accounts.cnProviders.staleAgo')
    // 不渲染 unknown 徽标
    expect(wrapper.find('[data-test="cn-quota-dimension-status"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('admin.accounts.cnProviders.dimensionStatusUnknown')
  })

  // f) 第三分支（方案 §3.4 最终口径 / 派发单 F1-R2）：数值存在 + observed_at 缺失/零值/无效
  // → 灰度进度条 + 固定「采集时间未知」文案，不渲染「未知」徽标（区别于从未探测的 b) 分支）。
  it('f) renders a gray bar with a fixed "probe time unknown" note when value exists but observed_at is missing', () => {
    const subNoObserved = {
      ...thPassAccount,
      quota_dimensions: [
        { kind: 'subscription', scope: 'account', target: '', status: 'unknown', servable: 'unknown', source: 'th_pass_snapshot', observed_at: null }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: subNoObserved } })
    // 数值存在（plan_used_pct=62）→ 进度条渲染（灰度）
    const planBar = wrapper.get('[data-test="cn-quota-pass-plan-bar"]')
    expect(planBar.exists()).toBe(true)
    const bar = planBar.findComponent(UsageProgressBar)
    expect(bar.props('stale')).toBe(true)
    expect(bar.props('unknownUsage')).toBe(false)
    expect(planBar.text()).toContain('62%')
    // 第三分支：陈旧标注固定「采集时间未知」（非相对时间）
    const note = wrapper.find('[data-test="cn-quota-stale-note"]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toContain('admin.accounts.cnProviders.staleTimeUnknown')
    expect(note.text()).not.toContain('admin.accounts.cnProviders.staleAgo')
    // 关键区分：observed_at 缺失但数值存在 → 不渲染「未知」徽标（仅从未探测的 b) 才渲染）
    expect(wrapper.find('[data-test="cn-quota-dimension-status"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('admin.accounts.cnProviders.dimensionStatusUnknown')
  })

  // f) Kira 免费维度同样锁死第三分支：数值存在 + observed_at 缺失 → 灰度条 + 「采集时间未知」
  it('f) renders a gray Kira free bar with a fixed "probe time unknown" note when value exists but observed_at is missing', () => {
    const freeNoObserved = {
      ...kiraAccount,
      quota_dimensions: [
        { kind: 'free', scope: 'account', target: '', status: 'unknown', servable: 'unknown', source: 'kira_usage_snapshot', observed_at: null }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: freeNoObserved } })
    const bar = wrapper.findComponent(UsageProgressBar)
    expect(bar.exists()).toBe(true)
    expect(bar.props('stale')).toBe(true)
    expect(bar.props('unknownUsage')).toBe(false)
    expect(wrapper.text()).toContain('30%')
    const note = wrapper.find('[data-test="cn-quota-stale-note"]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toContain('admin.accounts.cnProviders.staleTimeUnknown')
    expect(wrapper.find('[data-test="cn-quota-dimension-status"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('admin.accounts.cnProviders.dimensionStatusUnknown')
  })

  // f) observed_at 为零值/无效串同样走第三分支固定文案（与缺失同口径）
  it('f) treats an invalid observed_at as "probe time unknown" when value exists', () => {
    const subBadObserved = {
      ...thPassAccount,
      quota_dimensions: [
        { kind: 'subscription', scope: 'account', target: '', status: 'unknown', servable: 'unknown', source: 'th_pass_snapshot', observed_at: 'not-a-date' }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: subBadObserved } })
    const bar = wrapper.get('[data-test="cn-quota-pass-plan-bar"]').findComponent(UsageProgressBar)
    expect(bar.props('stale')).toBe(true)
    expect(bar.props('unknownUsage')).toBe(false)
    const note = wrapper.find('[data-test="cn-quota-stale-note"]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toContain('admin.accounts.cnProviders.staleTimeUnknown')
    expect(wrapper.text()).not.toContain('admin.accounts.cnProviders.dimensionStatusUnknown')
  })

  // b) 维度 unknown + 数值不存在（从未探测）→ 渲染「未知」徽标、不渲染进度条
  it('b) shows the unknown badge and hides the bar when never probed (no value)', () => {
    const neverProbed = {
      ...thPassAccount,
      extra: {
        ...thPassAccount.extra,
        th_pass_snapshot: { ...thPassAccount.extra.th_pass_snapshot, plan_used_pct: undefined }
      },
      quota_dimensions: [
        { kind: 'subscription', scope: 'account', target: '', status: 'unknown', servable: 'unknown', source: 'th_pass_snapshot', observed_at: null }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: neverProbed } })
    // 无数值 → 进度条不渲染
    expect(wrapper.find('[data-test="cn-quota-pass-plan-bar"]').exists()).toBe(false)
    // 从未探测（observed_at=null）→ 维度面板渲染「未知」徽标
    expect(wrapper.find('[data-test="cn-quota-dimension-status"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.cnProviders.dimensionStatusUnknown')
  })

  // b) Kira 免费维度 unknown + 数值不存在（从未探测）→ 渲染「未知」徽标、不渲染进度条
  it('b) shows the unknown badge and hides the Kira free bar when never probed (no value)', () => {
    const neverProbedKira = {
      ...kiraAccount,
      extra: {
        ...kiraAccount.extra,
        kira_usage_snapshot: {
          ...kiraAccount.extra.kira_usage_snapshot,
          used_percent: undefined,
          used_tokens: undefined,
          limit_tokens: undefined
        }
      },
      quota_dimensions: [
        { kind: 'free', scope: 'account', target: '', status: 'unknown', servable: 'unknown', source: 'kira_usage_snapshot', observed_at: null }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: neverProbedKira } })
    expect(wrapper.find('[data-test="cn-quota-kira-bar"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-quota-dimension-status"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.cnProviders.dimensionStatusUnknown')
  })

  // c) Kira 窗口内孤立 VND 状态徽标（cn-quota-kira-vnd-status）已删除（负例断言）
  it('c) the isolated Kira VND status badge is removed (negative assertion)', () => {
    const kiraWithPaid = {
      ...kiraAccount,
      quota_dimensions: [
        { kind: 'paid', scope: 'account', target: '', status: 'remaining', servable: 'yes', source: 'kira_vnd_balance', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: kiraWithPaid } })
    expect(wrapper.find('[data-test="cn-quota-kira-vnd-status"]').exists()).toBe(false)
    // VND 金额仍展示（取数路径不变）
    expect(wrapper.get('[data-test="cn-quota-kira-vnd"]').text()).toContain('"balance":"38406"')
  })

  // c) 无 paid 维度的 Kira 账号同样不存在孤立 VND 状态徽标
  it('c) the isolated Kira VND status badge is absent even without a paid dimension', () => {
    const wrapper = mount(CNQuotaUsageCell, { props: { account: kiraAccount } })
    expect(wrapper.find('[data-test="cn-quota-kira-vnd-status"]').exists()).toBe(false)
  })

  // d) status=remaining/exhausted → 绿/红条 + 状态徽标 + 数值（既有行为回归）
  it('d) renders a green/red bar plus the status badge and value for remaining/exhausted', () => {
    // remaining（Kira free, 30% → 绿条）
    const remainingFree = {
      ...kiraAccount,
      quota_dimensions: [
        { kind: 'free', scope: 'account', target: '', status: 'remaining', servable: 'yes', source: 'kira_usage_snapshot', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as unknown as Account
    const rWrap = mount(CNQuotaUsageCell, { props: { account: remainingFree } })
    const rBar = rWrap.findComponent(UsageProgressBar)
    expect(rBar.props('stale')).toBe(false)
    expect(rBar.props('unknownUsage')).toBe(false)
    expect(rBar.props('utilization')).toBe(30)
    expect(rBar.text()).toContain('30%')
    expect(rWrap.find('[data-test="cn-quota-dimension-status"]').exists()).toBe(true)
    expect(rWrap.text()).toContain('admin.accounts.cnProviders.dimensionStatusRemaining')

    // exhausted（TH 订阅, 95% → 红条）
    const exhaustedSub = {
      ...thPassAccount,
      extra: {
        ...thPassAccount.extra,
        th_pass_snapshot: { ...thPassAccount.extra.th_pass_snapshot, plan_used_pct: 95 }
      },
      quota_dimensions: [
        { kind: 'subscription', scope: 'account', target: '', status: 'exhausted', servable: 'no', source: 'th_pass_snapshot', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as unknown as Account
    const eWrap = mount(CNQuotaUsageCell, { props: { account: exhaustedSub } })
    const eBar = eWrap.get('[data-test="cn-quota-pass-plan-bar"]').findComponent(UsageProgressBar)
    expect(eBar.props('stale')).toBe(false)
    expect(eBar.props('unknownUsage')).toBe(false)
    expect(eBar.props('utilization')).toBe(95)
    expect(eBar.text()).toContain('95%')
    expect(eWrap.find('[data-test="cn-quota-dimension-status"]').exists()).toBe(true)
    expect(eWrap.text()).toContain('admin.accounts.cnProviders.statusExhausted')
  })

  // e) TH 订阅 plan_used_pct=81 + unknown → 灰条显示 81% + 陈旧标注
  it('e) shows 81% on a gray stale bar for TH subscription plan_used_pct=81 with unknown status', () => {
    const sub81 = {
      ...thPassAccount,
      extra: {
        ...thPassAccount.extra,
        th_pass_snapshot: { ...thPassAccount.extra.th_pass_snapshot, plan_used_pct: 81 }
      },
      quota_dimensions: [
        { kind: 'subscription', scope: 'account', target: '', status: 'unknown', servable: 'unknown', source: 'th_pass_snapshot', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: sub81 } })
    const bar = wrapper.get('[data-test="cn-quota-pass-plan-bar"]').findComponent(UsageProgressBar)
    expect(bar.props('stale')).toBe(true)
    expect(bar.props('unknownUsage')).toBe(false)
    expect(bar.props('utilization')).toBe(81)
    expect(bar.text()).toContain('81%')
    const note = wrapper.find('[data-test="cn-quota-stale-note"]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toContain('admin.accounts.cnProviders.staleAgo')
    // 数值存在 → 不渲染 unknown 徽标
    expect(wrapper.text()).not.toContain('admin.accounts.cnProviders.dimensionStatusUnknown')
  })

  // ② 维度 remaining → 数值渲染。
  it('② renders the Kira free bar when the free dimension is remaining', () => {
    const remainingFree = {
      ...kiraAccount,
      quota_dimensions: [
        { kind: 'free', scope: 'account', target: '', status: 'remaining', servable: 'yes', source: 'kira_usage_snapshot', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: remainingFree } })
    const bar = wrapper.findComponent(UsageProgressBar)
    expect(bar.exists()).toBe(true)
    expect(bar.props('utilization')).toBe(30)
    expect(wrapper.get('[data-test="cn-quota-kira-used-limit"]').exists()).toBe(true)
  })

  // ③ 订阅维度 exhausted → 状态唯一出口在维度面板，无第二个重复徽标（cn-quota-status 已删）。
  it('③ exposes subscription exhausted status only via the dimension panel (no duplicate badge)', () => {
    const exhaustedSub = {
      ...thPassAccount,
      quota_dimensions: [
        { kind: 'subscription', scope: 'account', target: '', status: 'exhausted', servable: 'no', source: 'th_pass_snapshot', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: exhaustedSub } })
    // 旧窗口头部重复徽标已删除
    expect(wrapper.find('[data-test="cn-quota-status"]').exists()).toBe(false)
    // 唯一出口 = 维度面板（kind=subscription + status=exhausted）
    const panel = wrapper.get('[data-test="cn-quota-dimensions"]')
    expect(panel.text()).toContain('admin.accounts.cnProviders.dimensionKindSubscription')
    expect(panel.text()).toContain('admin.accounts.cnProviders.statusExhausted')
  })

  // ④ quota_dimensions 缺失（旧 DTO 兼容）→ 降级为现状渲染：进度条/金额照旧渲染，不报错、不造状态。
  it('④ degrades to legacy rendering when quota_dimensions is absent', () => {
    const thWrap = mount(CNQuotaUsageCell, { props: { account: thPassAccount } })
    expect(thWrap.get('[data-test="cn-quota-pass-plan-bar"]').exists()).toBe(true)
    expect(thWrap.find('[data-test="cn-quota-status"]').exists()).toBe(false)

    const kiraWrap = mount(CNQuotaUsageCell, { props: { account: kiraAccount } })
    expect(kiraWrap.findComponent(UsageProgressBar).exists()).toBe(true)
    expect(kiraWrap.get('[data-test="cn-quota-kira-vnd"]').text()).toContain('"balance":"38406"')
  })
})
