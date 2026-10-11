/**
 * w45 conflict-resolution notes (second wave, B10 — frontend spec / semantic_conflict + same_contract)
 *
 * [现状取证] 本文件与 b0-ours 逐字节相同（git hash-object = 77a1baec33515f2dfa62e229487280119627c5de
 *   = git rev-parse b0-ours:frontend/src/components/account/__tests__/EditAccountModal.spec.ts），
 *   即自动合并没有把任何上游改动落进来。上游相对 base 的 5 处改动全是纯新增（diff base->upstream
 *   的删除行数 = 0），我方相对 base 也只多了第 3 行的 flushPromises 与 8 个用例块。三方 git merge-file
 *   只有 1 个文本冲突（就是第 3 行那个 import），机械合并后 2080 行。
 *
 * [总裁决] keep OURS 全文；上游 5 处新增一律 take UPSTREAM（暂缓，跨卡依赖）—— 逐条证据见下。
 *   缺席哨兵：同目录 EditAccountModalFusion.spec.ts（依赖落地即转红 = 设计中的报警）。
 *
 * [机械合并实验证据] 2026-10-11 本卡实测，两个探针文件跑完即删，未留下任何越界改动：
 *   变体 A = ours + 上游 5 处全量：vitest 在 collect 阶段直接断
 *     (Failed to resolve import "@/constants/platformCatalog") -> Tests: no tests，
 *     我方原有 65 例全部陪葬。所以上游块不是"加进去跑不过几条"，而是"加进去整文件报废"。
 *   变体 B = 变体 A 再剥掉 platformCatalog 的 import 及其唯一消费者 describe（generic form）：
 *     69 例 -> 3 failed / 66 passed，失败的恰好是下面 ③ ④b ⑥ 三块。
 *   基线（本卡交付态 = ours 原文）：65 passed / 0 failed，零回归。
 *
 * [逐处裁决]
 * (1) 未纳入：上游的 platformCatalog import（位置 = 第 3 行之后）。
 *   证据：工作树 frontend/src/constants/ 只有 account.ts / channel.ts / channelMonitor.ts /
 *   platforms.ts（+__tests__），没有 platformCatalog.ts 与 platformCatalog.builtin.json —— 两者
 *   只存在于 b0-upstream ref（blob 16bc44e2 / 1d7ec501）。全仓 grep "platformCatalog" 在 src/ 下
 *   只命中注释（w42/w43/w62 的同类"该模块缺席"登记）。权威表第 5 条"upstream 新符号已随自动合并
 *   入树"在本机整体不成立，故直接引用 = 整文件 collect 断（变体 A 实测）。
 * (2) 未纳入：ModelWhitelistSelectorStub 的 props 增列 modelMappings: { type: Array, default: () => [] }
 *   （位置 = 第 70 行那个 defineComponent 的 props 首位）。该改动本身惰性，只服务 (3) 的 props 断言，
 *   与 (3) 捆绑暂缓，避免落地半个契约。
 *   半边已收敛：真组件 ModelWhitelistSelector.vue 的 modelMappings?: { from, to }[] prop 已由 w43
 *   入树（工作树 line 164），缺的只是父组件传参，见 (3)。
 * (3) 未纳入：用例 'passes existing non-identity mappings to the whitelist selector and preserves
 *   them on save'（位置 = 现第 333 行 it('sets expiry presets ...') 之前）。
 *   变体 B 实测失败：expected [] to deeply equal [ { from: 'gpt-latest', to: 'deepseek-chat' } ]。
 *   根因在生产 EditAccountModal.vue 不给 selector 传 :model-mappings（工作树 grep -c
 *   "model-mappings" = 0，b0-upstream = 4）-> 属 w42 范围，本卡不越界改生产文件。
 * (4) 未纳入：上游 OpenCode 两块（位置 = 现第 448 行 it('preserves adaptive Kimi Responses
 *   endpoint on submit') 之前）。
 *   (4a) 'preserves OpenCode Zen account type and endpoints on submit'：变体 B 实测【通过】，
 *     但仍暂缓 —— 它把 account.platform 写成 'opencode_go'，而我方把该供应商归并为 'muse'
 *     （同仓先例：components/user/monitor/__tests__/ProviderIcon.spec.ts:30-39、
 *     utils/__tests__/platformColors.spec.ts:15/31/57、components/keys/UseKeyModal.vue:1263）。
 *     工作树生产无 opencode_go 分支，它跑的是"未知平台默认路径"，绿色偶然、断的不是我方产品面；
 *     且与 (4b) 同源成对，拆开落地只会让后来者困惑。
 *   (4b) 'treats a legacy OpenCode account without account_mode as GO'：变体 B 实测失败
 *     (expected { api_key: 'sk-opencode', ...(3) } to match object { account_mode: 'go', ...(2) })
 *     —— 缺上游"legacy 无 account_mode 归一化为 go"的生产实现（w42）。
 * (5) 未纳入：describe 'providers using the generic form'（2 例：'preserves stored endpoints and
 *   rules on submit' / 'offers the provider modes and keeps customised endpoints when switching
 *   mode'；位置 = (4) 之后、'preserves adaptive Kimi ...' 之前）。双重依赖：(1) 的 setPlatformCatalog
 *   家族 + 生产 data-testid="edit-generic-account-mode"（工作树命中 0，b0-upstream 命中 1）。
 *   五块里唯一拿不到运行证据的一块（变体 A 断在 import，变体 B 已按依赖剥除），收敛时需重跑确认。
 *   其 fixture 的 acme_router 还额外要求 AccountPlatform 是开放联合，见下表。
 * (6) 未纳入：用例 'preserves Seedance when exactly two endpoint capabilities are selected'
 *   （位置 = 现第 1483 行 it('keeps at least one OpenAI APIKey endpoint capability selected') 之前）。
 *   变体 B 实测失败：Unable to get [data-testid="openai-endpoint-capability-seedance"]。
 *   双重依赖：types 的 OpenAIEndpointCapability 加 'seedance'（w62）+ 生产
 *   openAIEndpointCapabilityOptions / normalizeOpenAIEndpointCapabilities 的 allowed 表加 seedance
 *   （工作树 line 3819-3831 只有 chat_completions / embeddings；b0-upstream grep "seedance" 命中 4）
 *   （w42）。
 *
 * [same_contract 字段级融合表] 本 spec 消费的契约，双方相对 base 的逐字段结果。契约文件
 *   frontend/src/types/index.ts 归 w62，本卡只消费不改动。
 *   - AccountPlatform（我方 types line 933）
 *       base : 'anthropic'|'openai'|'gemini'|'antigravity'|'grok'|'kimi'|'zhipu'|'deepseek'|'minimax'
 *       我方  : base + 'other' + 'codebuddy' + 'muse'（闭联合 12 项）
 *       上游  : 拆成 KnownAccountPlatform（base + 'opencode_go'|'typesafe'|'command_code'|'cline'，
 *               13 项）| (string & {})（开放联合，任意字符串）
 *       双方都改 -> 真冲突，按批规则默认我方（w62 卡片注记同为"双方重定义平台类型 AccountPlatform，
 *       取值集互斥"）。
 *       对本 spec 的影响：(4)(5) 的 platform = 'opencode_go' / 'acme_router' 在我方类型下无归属；
 *       之所以没编译错，只因为 buildAccount() 以 as any 收尾（本文件 line 166）—— 能跑 != 有语义。
 *   - GroupPlatform（我方 types line 545）：我方 = 闭联合（含 other/codebuddy/muse），上游 =
 *       AccountPlatform | 'composite'。双方都改 -> 默认我方。本 spec 不消费该类型，仅登记给 w62
 *       交叉参考。
 *   - OpenAIEndpointCapability（我方 types line 1555）
 *       base = 我方 = 'chat_completions' | 'embeddings'（我方逐字节未动）
 *       上游 = 追加 'seedance'
 *       -> 非双方冲突，是上游单方新增，按批规则"对应上游新功能的键取上游"-> take UPSTREAM
 *          （owner w62）。本卡不在 types 落地；(6) 的 spec 暂缓是它的下游连带。
 *   - credentials.model_mapping / account_mode / api_protocol / base_url / api_base_urls /
 *     protocol_rules：三方都落在 Account.credentials?: Record<string, unknown> 之下，types 从未
 *     声明这些子键 -> 无字段级契约冲突；(3)(4)(5) 对这些键的断言差全在生产实现（w42），不在 types。
 *   - 上游单方新增且本 spec 零消费：Account.opencode_go_usage、extra.codex_credits_snapshot、
 *     extra.codex_referral_snapshot、SiteConfig.subscription_enabled / payment_balance_disabled、
 *     AnnouncementItem.hide_open_button —— 仅登记给 w62，本卡无动作。
 *
 * [收敛单元 / owner]
 *   (a) frontend/src/constants/platformCatalog.ts + platformCatalog.builtin.json 入树 ——
 *       wave2 无卡认领（grep 卡片目录零命中），需 B11 或新开卡；解锁 (1)(2)（间接）(5)。
 *   (b) frontend/src/components/account/EditAccountModal.vue —— w42；解锁 (3)(4b)(5)(6)。
 *       差异移交：w42 的 manifest 登记 blob 4a52bcb8226b16d8e2531132592c42c81201f2e9，而工作树
 *       实测 blob 435d3d846ec5d5bf5b9224d02ae579133886f86f（== b0-ours），且文件内无任何 w42
 *       注记 —— w42 的交付未在工作树落地（或被还原）。本卡对 (b) 的依赖判定一律以工作树现状为准。
 *   (c) frontend/src/types/index.ts —— w62；解锁 (6) 的类型侧与 (4)(5) 的平台取值侧。工作树实测
 *       blob c98b534edacd1754d0a6cca50fcdd0afda7e9776 == b0-ours，同样尚未落地。
 *   (d) frontend/src/components/account/ModelWhitelistSelector.vue —— w43，已收敛（prop 在树）。
 *   (a)(b)(c) 齐备后，把 (1)-(6) 按上述位置原样补回（上游原文：git --no-optional-locks show
 *   b0-upstream:frontend/src/components/account/__tests__/EditAccountModal.spec.ts），并同步删除
 *   EditAccountModalFusion.spec.ts 里对应的哨兵 —— 它在依赖落地时必然失败，就是设计好的报警。
 *
 * [验证锚点]
 *   npx vitest run src/components/account/__tests__/EditAccountModal.spec.ts -> 65 passed（零回归）
 *   npx vitest run src/components/account/__tests__/EditAccountModalFusion.spec.ts -> 全绿（当前缺席态）
 *   npx vue-tsc --noEmit -> 本卡两文件零错误（全项目基线 5 处均在 views/user/KeysView.vue）
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
// w45 (1)：上游此处新增 platformCatalog 的 import（BUILTIN_PLATFORM_CATALOG /
//   resetPlatformCatalog / setPlatformCatalog）。该模块只存在于 b0-upstream ref，工作树没有 ->
//   引入即整文件 collect 断（机械合并变体 A 实测）。暂缓，证据见顶部 notes 块 (1)。

const { updateAccountMock, checkMixedChannelRiskMock, authIsSimpleMode } = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  checkMixedChannelRiskMock: vi.fn(),
  authIsSimpleMode: { value: true }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get isSimpleMode() {
      return authIsSimpleMode.value
    }
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      update: updateAccountMock,
      checkMixedChannelRisk: checkMixedChannelRiskMock
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([])
    }
  }
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

import EditAccountModal from '../EditAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: {
    show: {
      type: Boolean,
      default: false
    }
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: {
    // w45 (2)：上游此处新增 modelMappings: { type: Array, default: () => [] }。惰性改动，
    //   只服务 (3) 的 props 断言，与 (3) 捆绑暂缓（见顶部 notes）。
    modelValue: {
      type: Array,
      default: () => []
    }
  },
  emits: ['update:modelValue'],
  template: `
    <div>
      <button
        type="button"
        data-testid="rewrite-to-snapshot"
        @click="$emit('update:modelValue', ['gpt-5.2-2025-12-11'])"
      >
        rewrite
      </button>
      <span data-testid="model-whitelist-value">
        {{ Array.isArray(modelValue) ? modelValue.join(',') : '' }}
      </span>
    </div>
  `
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: {
      type: [String, Number, Boolean, null],
      default: ''
    },
    options: {
      type: Array,
      default: () => []
    }
  },
  emits: ['update:modelValue'],
  template: `
    <select
      v-bind="$attrs"
      :value="modelValue"
      @change="$emit('update:modelValue', $event.target.value)"
    >
      <option v-for="option in options" :key="option.value" :value="option.value">
        {{ option.label }}
      </option>
    </select>
  `
})

const GroupSelectorStub = defineComponent({
  name: 'GroupSelector',
  props: {
    modelValue: {
      type: Array,
      default: () => []
    }
  },
  emits: ['update:modelValue'],
  template: `
    <div data-testid="group-selector">
      <button
        type="button"
        data-testid="set-shadow-group"
        @click="$emit('update:modelValue', [7])"
      >
        group
      </button>
    </div>
  `
})

function buildAccount() {
  return {
    id: 1,
    name: 'OpenAI Key',
    notes: '',
    platform: 'openai',
    type: 'apikey',
    credentials: {
      api_key: 'sk-test',
      base_url: 'https://api.openai.com',
      model_mapping: {
        'gpt-5.2': 'gpt-5.2'
      }
    },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function buildOpenAISparkShadowAccount() {
  const account = buildAccount()
  return {
    ...account,
    id: 4,
    name: 'OpenAI Spark Shadow',
    type: 'oauth',
    parent_account_id: 1,
    credentials: {
      access_token: 'parent-access-token',
      refresh_token: 'parent-refresh-token',
      api_key: 'sk-parent',
      base_url: 'https://api.openai.com',
      model_mapping: {
        'gpt-5.3-codex-spark': 'gpt-5.3-codex-spark'
      },
      compact_model_mapping: {
        'gpt-5.3-codex-spark': 'gpt-5.3-codex-spark-compact'
      }
    },
    group_ids: []
  } as any
}

function buildVertexAccount() {
  return {
    id: 2,
    name: 'Vertex SA',
    notes: '',
    platform: 'gemini',
    type: 'service_account',
    credentials: {
      service_account_json: '{"type":"service_account","client_email":"sa@example.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\\nMIIE\\n-----END PRIVATE KEY-----\\n"}',
      project_id: 'demo-project',
      client_email: 'sa@example.iam.gserviceaccount.com',
      location: 'us-central1',
      tier_id: 'vertex'
    },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function buildAntigravityAccount(projectId = 'configured-project') {
  return {
    id: 3,
    name: 'Antigravity OAuth',
    notes: '',
    platform: 'antigravity',
    type: 'oauth',
    credentials: {
      antigravity_project_id: projectId,
      model_mapping: {
        'gemini-2.5-flash': 'gemini-2.5-flash'
      }
    },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function buildGrokOAuthAccount() {
  return {
    id: 5,
    name: 'Grok OAuth',
    notes: '',
    platform: 'grok',
    type: 'oauth',
    credentials: {
      refresh_token: 'grok-rt',
      base_url: 'https://api.x.ai/v1',
      model_mapping: {
        'grok-latest': 'grok-4.3'
      }
    },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function buildGrokAPIKeyAccount() {
  return {
    ...buildAccount(),
    id: 6,
    name: 'Grok API Key',
    platform: 'grok',
    credentials: {},
    credentials_status: { has_api_key: true },
    concurrency: 2
  } as any
}

function buildOpenAISetupTokenAccount() {
  return {
    ...buildAccount(),
    type: 'setup-token',
    extra: {
      openai_oauth_responses_websockets_v2_mode: 'ctx_pool',
      openai_oauth_responses_websockets_v2_enabled: true
    }
  } as any
}

function buildOpenAIOAuthParentAccount() {
  return {
    ...buildAccount(),
    id: 7,
    name: 'OpenAI OAuth Parent',
    type: 'oauth',
    parent_account_id: null,
    credentials: { access_token: 'oauth-token' },
    extra: {}
  } as any
}

function mountModal(account = buildAccount(), renderGroupSelector = false) {
  return mount(EditAccountModal, {
    props: {
      show: true,
      account,
      proxies: [],
      groups: []
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: SelectStub,
        Icon: true,
        ProxySelector: true,
        GroupSelector: renderGroupSelector ? false : GroupSelectorStub,
        ModelWhitelistSelector: ModelWhitelistSelectorStub
      }
    }
  })
}

describe('EditAccountModal', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
  })

  afterEach(() => vi.useRealTimers())

  // w45 (3)：上游此处新增用例 'passes existing non-identity mappings to the whitelist selector
  //   and preserves them on save'。依赖 w42 生产向 selector 传 :model-mappings，机械合并变体 B
  //   实测失败（expected [] to deeply equal [{ from: 'gpt-latest', to: 'deepseek-chat' }]）-> 暂缓。
  it('sets expiry presets from now instead of extending the saved expiry', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date('2028-02-29T12:34:00'))
    const account = buildAccount()
    account.expires_at = new Date('2030-06-15T09:00:00').getTime() / 1000
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })
    const wrapper = mountModal(account)
    const input = wrapper.get<HTMLInputElement>('input[type="datetime-local"]')

    for (const [label, expected] of [
      ['payment.oneMonth', '2028-03-29T12:34'],
      ['payment.oneYear', '2029-02-28T12:34'],
    ]) {
      const button = wrapper.findAll('button').find((candidate) => candidate.text() === label)!
      expect(button.attributes('type')).toBe('button')
      await button.trigger('click')
      expect(input.element.value).toBe(expected)
      expect(updateAccountMock).not.toHaveBeenCalled()
    }

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    expect(updateAccountMock.mock.calls[0]?.[1]?.expires_at).toBe(new Date('2029-02-28T12:34:00').getTime() / 1000)
    wrapper.unmount()
  })

  it('can clear a selected expiry preset before saving the account', async () => {
    const account = buildAccount()
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })
    const wrapper = mountModal(account)
    const button = wrapper.findAll('button').find((candidate) => candidate.text() === 'payment.oneYear')!
    await button.trigger('click')
    const input = wrapper.get<HTMLInputElement>('input[type="datetime-local"]')
    expect(input.element.value).not.toBe('')
    await input.setValue('')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    expect(updateAccountMock.mock.calls[0]?.[1]?.expires_at).toBe(0)
    wrapper.unmount()
  })

  it('allows removing assigned inactive groups and undoing the selection before saving', async () => {
    authIsSimpleMode.value = false
    const account = buildAccount()
    const activeGroup = {
      id: 1,
      name: 'Active group',
      platform: 'openai',
      status: 'active',
      subscription_type: 'standard',
      rate_multiplier: 1
    }
    const inactiveGroup = { ...activeGroup, id: 2, name: 'Paused group', status: 'inactive' }
    account.group_ids = [1, 2]
    account.groups = [
      { ...activeGroup, name: 'Outdated name' },
      inactiveGroup,
      inactiveGroup,
      { ...inactiveGroup, id: 3, name: 'Unassigned paused group' }
    ]
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account, true)
    await wrapper.setProps({ groups: [activeGroup] as any })
    const selector = wrapper.get('[data-tour="account-form-groups"]')
    expect(selector.findAll('input[type="checkbox"]').map(input => input.attributes('value')))
      .toEqual(['1', '2'])
    expect(selector.text()).toContain('Active group')
    expect(selector.text()).not.toContain('Outdated name')
    const pausedCheckbox = selector.get<HTMLInputElement>('input[value="2"]')
    expect(pausedCheckbox.element.checked).toBe(true)

    await pausedCheckbox.setValue(false)
    expect(selector.get<HTMLInputElement>('input[value="2"]').element.checked).toBe(false)
    await pausedCheckbox.setValue(true)
    expect(pausedCheckbox.element.checked).toBe(true)
    await pausedCheckbox.setValue(false)
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.group_ids).toEqual([1])
    expect(account.group_ids).toEqual([1, 2])
  })

  it('reopening the same account rehydrates the OpenAI whitelist from props', async () => {
    const account = buildAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect(wrapper.get('[data-testid="model-whitelist-value"]').text()).toBe('gpt-5.2')

    await wrapper.get('[data-testid="rewrite-to-snapshot"]').trigger('click')
    expect(wrapper.get('[data-testid="model-whitelist-value"]').text()).toBe('gpt-5.2-2025-12-11')

    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })

    expect(wrapper.get('[data-testid="model-whitelist-value"]').text()).toBe('gpt-5.2')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.model_mapping).toEqual({
      'gpt-5.2': 'gpt-5.2'
    })
  })

  // w45 (4)(5)：上游此处新增 OpenCode Zen / legacy-GO 两块，以及紧随其后的
  //   'providers using the generic form' describe（2 例）。依赖顶部 notes (1) 的 platformCatalog
  //   与 w42 的生产实现（opencode_go 分支、account_mode 归一化、edit-generic-account-mode 控件）
  //   -> 暂缓。(4a) 在变体 B 里偶然跑绿，仍按 (4a) 的理由与 (4b) 捆绑暂缓。
  it('preserves adaptive Kimi Responses endpoint on submit', async () => {
    const account = buildAccount()
    account.platform = 'kimi'
    account.credentials = {
      api_key: 'sk-kimi',
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://api.moonshot.cn/v1',
      api_base_urls: {
        chat_completions: 'https://api.moonshot.cn/v1',
        anthropic: 'https://api.moonshot.cn/anthropic',
        responses: 'https://api.moonshot.cn/v1'
      }
    }
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).toMatchObject({
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://api.moonshot.cn/v1',
      api_base_urls: {
        chat_completions: 'https://api.moonshot.cn/v1',
        anthropic: 'https://api.moonshot.cn/anthropic',
        responses: 'https://api.moonshot.cn/v1'
      }
    })
  })

  it('preserves adaptive GLM endpoints on submit', async () => {
    const account = buildAccount()
    account.platform = 'zhipu'
    account.credentials = {
      api_key: 'sk-glm',
      account_mode: 'coding',
      api_protocol: 'adaptive',
      base_url: 'https://open.bigmodel.cn/api/coding/paas/v4',
      api_base_urls: {
        chat_completions: 'https://open.bigmodel.cn/api/coding/paas/v4',
        anthropic: 'https://open.bigmodel.cn/api/anthropic'
      }
    }
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).toMatchObject({
      account_mode: 'coding',
      api_protocol: 'adaptive',
      base_url: 'https://open.bigmodel.cn/api/coding/paas/v4',
      api_base_urls: {
        chat_completions: 'https://open.bigmodel.cn/api/coding/paas/v4',
        anthropic: 'https://open.bigmodel.cn/api/anthropic'
      }
    })
  })

  it.each([
    ['explicit Chat Completions', 'chat_completions'],
    ['legacy missing protocol', undefined]
  ])('preserves a custom CN relay for %s accounts', async (_name, storedProtocol) => {
    const account = buildAccount()
    account.platform = 'zhipu'
    account.credentials = {
      api_key: 'sk-glm',
      account_mode: 'payg',
      base_url: 'https://relay.example.com/v1'
    }
    if (storedProtocol) {
      account.credentials.api_protocol = storedProtocol
    }
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    const submittedCredentials = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submittedCredentials).toMatchObject({
      account_mode: 'payg',
      api_protocol: 'chat_completions',
      base_url: 'https://relay.example.com/v1'
    })
    expect(submittedCredentials).not.toHaveProperty('api_base_urls')
  })

  it('edits and persists api_protocol for other accounts', async () => {
    const account = {
      ...buildAccount(),
      platform: 'other',
      credentials: {
        api_key: 'sk-or',
        base_url: 'https://openrouter.ai/api/v1'
      }
    } as any
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)

    // other 双协议选择器可见，默认 chat_completions。
    const anthropicBtn = wrapper
      .findAll('button')
      .find(b => b.text() === 'admin.accounts.cnProviders.apiProtocol.anthropic')
    expect(anthropicBtn).toBeDefined()
    await anthropicBtn!.trigger('click')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).toMatchObject({
      base_url: 'https://openrouter.ai/api/v1',
      api_protocol: 'anthropic'
    })
  })

  it('rehydrates stored anthropic protocol for other accounts', async () => {
    const account = {
      ...buildAccount(),
      platform: 'other',
      credentials: {
        api_key: 'sk-lkeap',
        base_url: 'https://api.lkeap.cloud.tencent.com/plan/anthropic',
        api_protocol: 'anthropic'
      }
    } as any
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).toMatchObject({
      base_url: 'https://api.lkeap.cloud.tencent.com/plan/anthropic',
      api_protocol: 'anthropic'
    })
  })

  it('uses the legacy base_url when adaptive endpoints are missing', async () => {
    const account = buildAccount()
    account.platform = 'zhipu'
    account.credentials = {
      api_key: 'sk-glm',
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://relay.example.com/v1',
      api_base_urls: {
        chat_completions: '   '
      }
    }
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).toMatchObject({
      api_protocol: 'adaptive',
      base_url: 'https://relay.example.com/v1',
      api_base_urls: {
        chat_completions: 'https://relay.example.com/v1',
        anthropic: 'https://open.bigmodel.cn/api/anthropic'
      }
    })
  })

  it('edits a Volcano subscription account using only the ark api_key (no AK/SK)', async () => {
    const account = buildAccount()
    account.platform = 'deepseek'
    account.credentials = {
      api_key: 'sk-deepseek',
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://ark.cn-beijing.volces.com/api/plan',
      api_base_urls: {
        chat_completions: 'https://ark.cn-beijing.volces.com/api/plan',
        anthropic: '',
        responses: ''
      }
    }
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    // 火山订阅号用量探测无需 AK/SK 签名，编辑时不应写入 access_key/secret_key。
    expect(submitted?.access_key).toBeUndefined()
    expect(submitted?.secret_key).toBeUndefined()
  })

  it('does not write AK/SK when switching to another Volcano account', async () => {
    const accountA = buildAccount()
    accountA.id = 11
    accountA.platform = 'deepseek'
    accountA.credentials = {
      api_key: 'sk-deepseek-a',
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://ark.cn-beijing.volces.com/api/plan',
      api_base_urls: {
        chat_completions: 'https://ark.cn-beijing.volces.com/api/plan',
        anthropic: '',
        responses: ''
      }
    }
    const accountB = buildAccount()
    accountB.id = 12
    accountB.platform = 'deepseek'
    accountB.credentials = {
      api_key: 'sk-deepseek-b',
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://ark.cn-beijing.volces.com/api/coding',
      api_base_urls: {
        chat_completions: 'https://ark.cn-beijing.volces.com/api/coding',
        anthropic: '',
        responses: ''
      }
    }
    updateAccountMock.mockReset().mockResolvedValue(accountB)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(accountA)
    // 同一编辑弹窗切换到另一个火山账号，不应带入任何 AK/SK。
    await wrapper.setProps({ account: accountB })

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted?.access_key).toBeUndefined()
    expect(submitted?.secret_key).toBeUndefined()
  })

  it('recognizes Volcano subscription when adaptive chat_completions is whitespace but base_url is volcano', async () => {
    const account = buildAccount()
    account.platform = 'deepseek'
    account.credentials = {
      api_key: 'sk-deepseek',
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://ark.cn-beijing.volces.com/api/plan',
      api_base_urls: {
        chat_completions: '   ',
        anthropic: '',
        responses: ''
      }
    }
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)
    // 空白 chat_completions 不应遮蔽真实火山 base_url（LOW 修复）；
    // 火山订阅号识别成功：api_key 输入框展示，且 AK/SK 录入区同步出现（用量探测签名需要）。
    const pw = wrapper.find('form#edit-account-form input[type="password"]')
    expect(pw.exists()).toBe(true)
    expect(wrapper.find('input[placeholder="admin.accounts.cnProviders.accessKeyPlaceholder"]').exists()).toBe(true)
    expect(wrapper.find('input[placeholder="admin.accounts.cnProviders.secretKeyPlaceholder"]').exists()).toBe(true)
  })

  it('keeps Volcano base_url in payload when adaptive chat_completions is whitespace', async () => {
    const account = buildAccount()
    account.platform = 'deepseek'
    account.credentials = {
      api_key: 'sk-deepseek',
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://ark.cn-beijing.volces.com/api/plan',
      api_base_urls: {
        chat_completions: '   ',
        anthropic: '',
        responses: ''
      }
    }
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    // 空白 chat_completions 提交应回退到火山 base_url，而非写成空值（MEDIUM 修复）。
    expect(submitted?.api_base_urls?.chat_completions).toBe('https://ark.cn-beijing.volces.com/api/plan')
    expect(submitted?.base_url).toBe('https://ark.cn-beijing.volces.com/api/plan')
  })

  it('keeps Volcano endpoint after switching chat_completions -> adaptive (no silent fallback)', async () => {
    const account = buildAccount()
    account.platform = 'deepseek'
    account.credentials = {
      api_key: 'sk-deepseek',
      account_mode: 'payg',
      api_protocol: 'chat_completions',
      base_url: 'https://ark.cn-beijing.volces.com/api/plan'
    }
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)
    // 切到 adaptive：火山 base_url 应同步进 chat_completions 槽位（HIGH 修复）
    const adaptiveButton = wrapper
      .findAll('button')
      .find(button => button.text().includes('admin.accounts.cnProviders.apiProtocol.adaptive'))
    expect(adaptiveButton).toBeDefined()
    await adaptiveButton!.trigger('click')
    await flushPromises()
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted?.api_base_urls?.chat_completions).toBe('https://ark.cn-beijing.volces.com/api/plan')
    expect(submitted?.base_url).toBe('https://ark.cn-beijing.volces.com/api/plan')
  })

  it('keeps Volcano endpoint after adaptive -> chat_completions when adaptive chat slot is blank (whitespace falls back to base_url)', async () => {
    const account = buildAccount()
    account.platform = 'deepseek'
    account.credentials = {
      api_key: 'sk-deepseek',
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://ark.cn-beijing.volces.com/api/plan',
      api_base_urls: {
        chat_completions: 'https://ark.cn-beijing.volces.com/api/plan',
        anthropic: '',
        responses: ''
      }
    }
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)
    // 把 adaptive 的 chat_completions 槽位（当前值为火山地址）清空为空白串
    const ccInput = wrapper.findAll('input').find(input => input.element.value === 'https://ark.cn-beijing.volces.com/api/plan')
    expect(ccInput).toBeDefined()
    await ccInput!.setValue('   ')
    await flushPromises()
    // 切回 chat_completions：空白槽位应回退到火山 base_url，而非写成 DeepSeek 默认（MEDIUM 修复）
    const chatButton = wrapper
      .findAll('button')
      .find(button => button.text().includes('admin.accounts.cnProviders.apiProtocol.chatCompletions'))
    expect(chatButton).toBeDefined()
    await chatButton!.trigger('click')
    await flushPromises()
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    const submitted = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(submitted?.api_protocol).toBe('chat_completions')
    expect(submitted?.base_url).toBe('https://ark.cn-beijing.volces.com/api/plan')
  })

  it('carries a fixed Chat relay into Adaptive when the user switches protocols', async () => {
    const account = buildAccount()
    account.platform = 'zhipu'
    account.credentials = {
      api_key: 'sk-glm',
      account_mode: 'payg',
      api_protocol: 'chat_completions',
      base_url: 'https://relay.example.com/v1'
    }
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)
    const adaptiveButton = wrapper
      .findAll('button')
      .find(button => button.text().includes('admin.accounts.cnProviders.apiProtocol.adaptive'))
    expect(adaptiveButton).toBeDefined()
    await adaptiveButton!.trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).toMatchObject({
      api_protocol: 'adaptive',
      base_url: 'https://relay.example.com/v1',
      api_base_urls: {
        chat_completions: 'https://relay.example.com/v1'
      }
    })
  })

  it.each([
    {
      name: 'Anthropic',
      platform: 'zhipu',
      protocol: 'anthropic',
      baseUrl: 'https://relay.example.com/anthropic',
      expectedBaseUrl: 'https://open.bigmodel.cn/api/paas/v4',
      expectedProtocolUrls: {
        chat_completions: 'https://open.bigmodel.cn/api/paas/v4',
        anthropic: 'https://relay.example.com/anthropic'
      }
    },
    {
      name: 'Responses',
      platform: 'deepseek',
      protocol: 'responses',
      baseUrl: 'https://relay.example.com/responses',
      expectedBaseUrl: 'https://api.deepseek.com',
      expectedProtocolUrls: {
        chat_completions: 'https://api.deepseek.com',
        anthropic: 'https://api.deepseek.com/anthropic',
        responses: 'https://relay.example.com/responses'
      }
    }
  ])('keeps a fixed $name relay in its protocol slot when switching to Adaptive', async (testCase) => {
    const account = buildAccount()
    account.platform = testCase.platform
    account.credentials = {
      api_key: 'sk-cn',
      account_mode: 'payg',
      api_protocol: testCase.protocol,
      base_url: testCase.baseUrl
    }
    updateAccountMock.mockReset().mockResolvedValue(account)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)
    const adaptiveButton = wrapper
      .findAll('button')
      .find(button => button.text().includes('admin.accounts.cnProviders.apiProtocol.adaptive'))
    expect(adaptiveButton).toBeDefined()
    await adaptiveButton!.trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).toMatchObject({
      api_protocol: 'adaptive',
      base_url: testCase.expectedBaseUrl,
      api_base_urls: testCase.expectedProtocolUrls
    })
  })

  it('preserves model mappings when editing the whitelist', async () => {
    const account = buildAccount()
    account.credentials.model_mapping = {
      'gpt-5.2': 'gpt-5.2',
      'gpt-latest': 'gpt-5.2'
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect(wrapper.get('[data-testid="model-whitelist-value"]').text()).toBe('gpt-5.2')

    await wrapper.get('[data-testid="rewrite-to-snapshot"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.model_mapping).toEqual({
      'gpt-5.2-2025-12-11': 'gpt-5.2-2025-12-11',
      'gpt-latest': 'gpt-5.2'
    })
  })

  it('submits OpenAI compact mode and compact-only model mapping', async () => {
    const account = buildAccount()
    account.extra = {
      openai_compact_mode: 'force_on'
    }
    account.credentials = {
      ...account.credentials,
      compact_model_mapping: {
        'gpt-5.4': 'gpt-5.4-openai-compact'
      }
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_compact_mode).toBe('force_on')
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.compact_model_mapping).toEqual({
      'gpt-5.4': 'gpt-5.4-openai-compact'
    })
  })

  it('loads and submits the per-account OpenAI long-context billing toggle', async () => {
    const account = buildAccount()
    account.extra = {
      openai_long_context_billing_enabled: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const toggle = wrapper.get('[data-testid="openai-long-context-billing-toggle"]')
    expect(toggle.attributes('aria-checked')).toBe('true')

    await toggle.trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })

  it('loads and clears the OAuth-only Codex namespace flatten toggle', async () => {
    const account = buildAccount()
    account.type = 'oauth'
    account.extra = {
      openai_responses_flatten_namespaces: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const toggle = wrapper.get('[data-testid="edit-openai-flatten-namespaces-toggle"]')

    // 关闭后应从 extra 中删除该键，而不是写入 false
    await toggle.trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty(
      'openai_responses_flatten_namespaces'
    )
  })

  it('submits the Codex namespace flatten toggle when switched on', async () => {
    const account = buildAccount()
    account.type = 'oauth'
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    await wrapper.get('[data-testid="edit-openai-flatten-namespaces-toggle"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_responses_flatten_namespaces).toBe(
      true
    )
  })

  it('writes the upstream request id header into extra only when it changes', async () => {
    const account = buildAccount()
    account.extra = { openai_compact_mode: 'force_on' }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const untouched = mountModal(account)
    await untouched.get('form#edit-account-form').trigger('submit.prevent')
    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.upstream_request_id_header).toBeUndefined()

    updateAccountMock.mockClear()
    const wrapper = mountModal(account)
    await wrapper.get('[data-testid="upstream-request-id-header"]').setValue(' X-Oneapi-Request-Id ')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).toMatchObject({
      openai_compact_mode: 'force_on',
      upstream_request_id_header: 'X-Oneapi-Request-Id'
    })
  })

  it('removes the upstream request id header from extra when cleared', async () => {
    const account = buildAccount()
    account.extra = { upstream_request_id_header: 'X-Request-ID' }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    expect((wrapper.get('[data-testid="upstream-request-id-header"]').element as HTMLInputElement).value).toBe('X-Request-ID')
    await wrapper.get('[data-testid="upstream-request-id-header"]').setValue('')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).toBeDefined()
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('upstream_request_id_header')
  })

  it('writes images_url_to_b64_json into extra when toggled on', async () => {
    const account = buildAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const toggle = wrapper.get('[data-testid="openai-images-url-to-b64-json-toggle"]')
    expect(toggle.attributes('aria-checked')).toBe('false')
    await toggle.trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.images_url_to_b64_json).toBe(true)
  })

  it('removes images_url_to_b64_json from extra when toggled off', async () => {
    const account = buildAccount()
    account.extra = { images_url_to_b64_json: true }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const toggle = wrapper.get('[data-testid="openai-images-url-to-b64-json-toggle"]')
    expect(toggle.attributes('aria-checked')).toBe('true')
    await toggle.trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).toBeDefined()
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('images_url_to_b64_json')
  })

  it('hides the Codex namespace flatten toggle for non-OAuth OpenAI accounts', async () => {
    const account = buildAccount()
    const wrapper = mountModal(account)

    expect(wrapper.find('[data-testid="edit-openai-flatten-namespaces-toggle"]').exists()).toBe(
      false
    )
  })

  it('defaults legacy OpenAI accounts to long-context billing disabled', async () => {
    const account = buildAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const toggle = wrapper.get('[data-testid="openai-long-context-billing-toggle"]')
    expect(toggle.attributes('aria-checked')).toBe('false')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })

  it('does not render or submit the long-context billing toggle for Spark shadow accounts', async () => {
    const account = buildOpenAISparkShadowAccount()
    account.extra = {
      openai_long_context_billing_enabled: false
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    expect(wrapper.find('[data-testid="openai-long-context-billing-toggle"]').exists()).toBe(false)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty(
      'openai_long_context_billing_enabled'
    )
  })

  it('preserves an explicit OpenAI long-context billing opt-out', async () => {
    const account = buildAccount()
    account.extra = {
      openai_long_context_billing_enabled: false
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const toggle = wrapper.get('[data-testid="openai-long-context-billing-toggle"]')
    expect(toggle.attributes('aria-checked')).toBe('false')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })

  it('fails closed for malformed OpenAI long-context billing values', async () => {
    const account = buildAccount()
    account.extra = {
      openai_long_context_billing_enabled: 'false'
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect(wrapper.get('[data-testid="openai-long-context-billing-toggle"]').attributes('aria-checked')).toBe('false')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })

  it('loads and submits Grok OAuth model mapping edits', async () => {
    const account = buildGrokOAuthAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    expect(wrapper.text()).toContain('Imagine Image')
    expect(wrapper.text()).toContain('Imagine Video')

    const inputWithValue = (value: string) => {
      const input = wrapper
        .findAll('input')
        .find((input) => (input.element as HTMLInputElement).value === value)
      expect(input).toBeTruthy()
      return input!
    }

    await inputWithValue('grok-latest').setValue('grok')
    await inputWithValue('grok-4.3').setValue('grok-build-0.1')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.model_mapping).toEqual({
      grok: 'grok-build-0.1'
    })
  })

  it('uses the official xAI base URL when a Grok API-key account omits base_url', async () => {
    const account = buildGrokAPIKeyAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect((wrapper.get('input[placeholder="https://api.x.ai/v1"]').element as HTMLInputElement).value)
      .toBe('https://api.x.ai/v1')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.base_url).toBe('https://api.x.ai/v1')
  })

  it('only submits model mapping credentials when saving an OpenAI spark shadow account', async () => {
    authIsSimpleMode.value = false
    const account = buildOpenAISparkShadowAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('[data-testid="set-shadow-group"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload?.group_ids).toEqual([7])
    expect(payload?.credentials).toEqual({
      model_mapping: {
        'gpt-5.3-codex-spark': 'gpt-5.3-codex-spark'
      },
      compact_model_mapping: {
        'gpt-5.3-codex-spark': 'gpt-5.3-codex-spark-compact'
      }
    })
  })

  it('submits OpenAI APIKey Responses support override mode', async () => {
    const account = buildAccount()
    account.extra = {
      openai_responses_mode: 'force_chat_completions',
      openai_responses_supported: false
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('[data-testid="openai-responses-mode-select"]').setValue('force_responses')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_responses_mode).toBe('force_responses')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_responses_supported).toBe(false)
  })

  it('submits the account upstream billing auto-probe setting', async () => {
    const account = buildAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const toggle = wrapper.get('[data-testid="upstream-billing-auto-probe"]')
    expect(toggle.attributes('aria-checked')).toBe('false')

    await toggle.trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.upstream_billing_probe_enabled).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty(
      'upstream_billing_probe_enabled'
    )
  })

  it('exposes the upstream billing auto-probe toggle for non-OpenAI API-key accounts', async () => {
    // 探测已放宽到全部 API-key 平台：grok 账号同样能开启并保存。
    const account = buildAccount()
    account.platform = 'grok'
    account.name = 'grok-relay'
    account.credentials = { api_key: 'sk-grok', base_url: 'https://relay.example/v1' }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const toggle = wrapper.get('[data-testid="upstream-billing-auto-probe"]')
    expect(toggle.attributes('aria-checked')).toBe('false')

    await toggle.trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.upstream_billing_probe_enabled).toBe(true)
  })

  it('enabling rate sync also enables probing and stops submitting a manual rate', async () => {
    const account = buildAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const syncToggle = wrapper.get('[data-testid="upstream-billing-rate-sync"]')
    const probeToggle = wrapper.get('[data-testid="upstream-billing-auto-probe"]')
    const rateInput = wrapper.get<HTMLInputElement>('[data-testid="account-rate-multiplier"]')
    expect(syncToggle.attributes('aria-checked')).toBe('false')
    expect(probeToggle.attributes('aria-checked')).toBe('false')
    expect(rateInput.element.disabled).toBe(false)
    expect(wrapper.text()).toContain('admin.accounts.billingRateMultiplierHint')
    expect(wrapper.text()).not.toContain('admin.accounts.upstreamBilling.syncRateManagedHint')

    await syncToggle.trigger('click')
    expect(syncToggle.attributes('aria-checked')).toBe('true')
    expect(probeToggle.attributes('aria-checked')).toBe('true')
    expect(rateInput.element.disabled).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.upstreamBilling.syncRateManagedHint')
    expect(wrapper.text()).not.toContain('admin.accounts.billingRateMultiplierHint')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload?.upstream_billing_probe_enabled).toBe(true)
    expect(payload?.upstream_billing_rate_sync_enabled).toBe(true)
    expect(payload).not.toHaveProperty('rate_multiplier')
  })

  it('disabling probing also disables rate sync and restores manual rate editing', async () => {
    const account = buildAccount()
    account.extra = {
      upstream_billing_probe_enabled: true,
      upstream_billing_rate_sync_enabled: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const syncToggle = wrapper.get('[data-testid="upstream-billing-rate-sync"]')
    const probeToggle = wrapper.get('[data-testid="upstream-billing-auto-probe"]')
    const rateInput = wrapper.get<HTMLInputElement>('[data-testid="account-rate-multiplier"]')
    expect(syncToggle.attributes('aria-checked')).toBe('true')
    expect(rateInput.element.disabled).toBe(true)

    await probeToggle.trigger('click')
    expect(probeToggle.attributes('aria-checked')).toBe('false')
    expect(syncToggle.attributes('aria-checked')).toBe('false')
    expect(rateInput.element.disabled).toBe(false)
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload?.upstream_billing_probe_enabled).toBe(false)
    expect(payload?.upstream_billing_rate_sync_enabled).toBe(false)
    expect(payload?.rate_multiplier).toBe(1)
  })

  it('disabling only rate sync keeps automatic probing enabled', async () => {
    const account = buildAccount()
    account.extra = {
      upstream_billing_probe_enabled: true,
      upstream_billing_rate_sync_enabled: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    await wrapper.get('[data-testid="upstream-billing-rate-sync"]').trigger('click')
    expect(wrapper.get('[data-testid="upstream-billing-auto-probe"]').attributes('aria-checked')).toBe(
      'true'
    )
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload?.upstream_billing_probe_enabled).toBe(true)
    expect(payload?.upstream_billing_rate_sync_enabled).toBe(false)
    expect(payload?.rate_multiplier).toBe(1)
  })

  it('clears OpenAI APIKey Responses override when set back to auto', async () => {
    const account = buildAccount()
    account.extra = {
      openai_responses_mode: 'force_chat_completions',
      openai_responses_supported: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('[data-testid="openai-responses-mode-select"]').setValue('auto')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('openai_responses_mode')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_responses_supported).toBe(true)
  })

  it('submits OpenAI APIKey endpoint capabilities from credentials', async () => {
    const account = buildAccount()
    account.credentials.openai_capabilities = ['chat_completions']
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect(wrapper.findAll('input[type="checkbox"]').some((input) => (input.element as HTMLInputElement).checked)).toBe(true)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.openai_capabilities).toEqual([
      'chat_completions'
    ])
  })

	it('submits OpenAI quota auto-pause thresholds in extra', async () => {
	  const account = buildAccount()
	  account.extra = {
		auto_pause_5h_threshold: 0.9,
		auto_pause_7d_threshold: 0.8
	  }
	  updateAccountMock.mockReset()
	  checkMixedChannelRiskMock.mockReset()
	  checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
	  updateAccountMock.mockResolvedValue(account)

	  const wrapper = mountModal(account)

	  await wrapper.get('[data-testid="auto-pause-5h-threshold"]').setValue('95')
	  await wrapper.get('[data-testid="auto-pause-7d-threshold"]').setValue('96')
	  await wrapper.get('form#edit-account-form').trigger('submit.prevent')

	  expect(updateAccountMock).toHaveBeenCalledTimes(1)
	  expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.auto_pause_5h_threshold).toBe(0.95)
	  expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.auto_pause_7d_threshold).toBe(0.96)
	})

	it('submits OpenAI quota auto-pause disable flag in extra', async () => {
	  // Toggling the per-account disable flag must persist as auto_pause_5h_disabled
	  // so an admin can exempt one account from auto-pause even when a global default
	  // threshold is configured (otherwise leaving the threshold blank would silently
	  // fall back to the global default).
	  const account = buildAccount()
	  updateAccountMock.mockReset()
	  checkMixedChannelRiskMock.mockReset()
	  checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
	  updateAccountMock.mockResolvedValue(account)

	  const wrapper = mountModal(account)

	  await wrapper.get('[data-testid="auto-pause-5h-disabled"]').trigger('click')
	  await wrapper.get('form#edit-account-form').trigger('submit.prevent')

	  expect(updateAccountMock).toHaveBeenCalledTimes(1)
	  expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.auto_pause_5h_disabled).toBe(true)
	  expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.auto_pause_7d_disabled).toBeUndefined()
	})

  // w45 (6)：上游此处新增用例 'preserves Seedance when exactly two endpoint capabilities are
  //   selected'。依赖 w62 types 的 OpenAIEndpointCapability 加 'seedance' 与 w42 生产选项表，
  //   机械合并变体 B 实测失败（Unable to get [data-testid="openai-endpoint-capability-seedance"]）
  //   -> 暂缓。
  it('keeps at least one OpenAI APIKey endpoint capability selected', async () => {
    const account = buildAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    const chatCheckbox = wrapper.get<HTMLInputElement>(
      '[data-testid="openai-endpoint-capability-chat_completions"]'
    )
    const embeddingsCheckbox = wrapper.get<HTMLInputElement>(
      '[data-testid="openai-endpoint-capability-embeddings"]'
    )

    expect(chatCheckbox.element.checked).toBe(true)
    expect(embeddingsCheckbox.element.checked).toBe(true)

    await embeddingsCheckbox.setValue(false)

    expect(chatCheckbox.element.checked).toBe(true)
    expect(embeddingsCheckbox.element.checked).toBe(false)

    await chatCheckbox.setValue(false)

    expect(chatCheckbox.element.checked).toBe(true)
    expect(embeddingsCheckbox.element.checked).toBe(false)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.openai_capabilities).toEqual([
      'chat_completions'
    ])
  })

  it('disables text generation protocol when only embeddings requests are accepted', async () => {
    const account = buildAccount()
    account.credentials.openai_capabilities = ['embeddings']
    account.extra = {
      openai_responses_mode: 'force_responses',
      openai_responses_supported: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    const responsesModeSelect = wrapper.get<HTMLSelectElement>(
      '[data-testid="openai-responses-mode-select"]'
    )

    expect(responsesModeSelect.element.disabled).toBe(true)
    expect(wrapper.find('[data-testid="openai-responses-mode-not-applicable"]').exists()).toBe(true)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.openai_capabilities).toEqual([
      'embeddings'
    ])
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('openai_responses_mode')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_responses_supported).toBe(true)
  })

  it('submits Codex image tool force-inject mode as bridge override', async () => {
    const account = buildAccount()
    account.extra = {
      codex_image_generation_bridge: false,
      codex_image_generation_bridge_enabled: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect(wrapper.text()).toContain('admin.accounts.openai.codexImageTool')
    expect(wrapper.text()).toContain('admin.accounts.openai.codexImageToolDesc')
    expect(wrapper.text()).toContain('admin.accounts.openai.codexImageToolEnabledDesc')

    await wrapper.get('button[data-testid="codex-image-tool-enabled"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.codex_image_generation_bridge).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('codex_image_generation_bridge_enabled')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('codex_image_generation_explicit_tool_policy')
  })

  it('submits Codex image tool no-injection mode without strip policy', async () => {
    const account = buildAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('button[data-testid="codex-image-tool-disabled"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.codex_image_generation_bridge).toBe(false)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('codex_image_generation_explicit_tool_policy')
  })

  it('submits Codex image tool block mode as strip policy and clears bridge override', async () => {
    const account = buildAccount()
    account.extra = {
      codex_image_generation_bridge: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect(wrapper.text()).toContain('admin.accounts.openai.codexImageToolBlock')
    expect(wrapper.text()).toContain('admin.accounts.openai.codexImageToolBlockDesc')

    await wrapper.get('button[data-testid="codex-image-tool-block"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.codex_image_generation_explicit_tool_policy).toBe('strip')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('codex_image_generation_bridge')
  })

  it('loads strip policy as block mode and clears both keys when reset to inherit', async () => {
    const account = buildAccount()
    account.extra = {
      codex_image_generation_explicit_tool_policy: 'strip'
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('button[data-testid="codex-image-tool-inherit"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('codex_image_generation_explicit_tool_policy')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('codex_image_generation_bridge')
  })

  it('setup-token account can select and submit OAuth WS mode', async () => {
    const account = buildOpenAISetupTokenAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('[data-testid="edit-openai-ws-mode-select"]').setValue('http_bridge')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_oauth_responses_websockets_v2_mode).toBe('http_bridge')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_oauth_responses_websockets_v2_enabled).toBe(true)
  })

  it('allows saving apikey account when backend redacted api_key but credentials_status reports it exists', async () => {
    // 新前端 + 新后端：响应已脱敏，credentials 里没有 api_key，credentials_status.has_api_key=true
    const account = buildAccount()
    account.credentials = {
      base_url: 'https://api.openai.com',
      model_mapping: { 'gpt-5.2': 'gpt-5.2' }
    }
    account.credentials_status = { has_api_key: true }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    // 用户未输入新 key 时，payload 不应带 api_key，由后端合并保留旧值
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).not.toHaveProperty('api_key')
  })

  it('allows saving apikey account against legacy backend without credentials_status', async () => {
    // 新前端 + 旧后端：credentials_status 缺失，但 credentials.api_key 仍是明文，应允许保存
    const account = buildAccount()
    // 显式确保没有 credentials_status
    expect(account.credentials_status).toBeUndefined()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    // 旧后端响应未脱敏，原 api_key 会随 currentCredentials 一起传回去（旧行为，等价于无操作）
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.api_key).toBe('sk-test')
  })

  it('blocks apikey save when neither credentials_status nor legacy api_key indicates existence', async () => {
    const account = buildAccount()
    account.credentials = {
      base_url: 'https://api.openai.com'
    }
    // 既没有 credentials_status 也没有旧的 api_key
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).not.toHaveBeenCalled()
  })

  it('allows saving Vertex SA account when backend redacted service_account_json but credentials_status reports it exists', async () => {
    // 新前端 + 新后端：响应已脱敏，credentials 里没有 service_account_json，credentials_status.has_service_account_json=true
    const account = buildVertexAccount()
    account.credentials = {
      project_id: 'demo-project',
      client_email: 'sa@example.iam.gserviceaccount.com',
      location: 'us-central1',
      tier_id: 'vertex'
    }
    account.credentials_status = { has_service_account_json: true }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.project_id).toBe('demo-project')
  })

  it('allows saving Vertex SA account against legacy backend without credentials_status', async () => {
    // 新前端 + 旧后端：credentials_status 缺失，但 credentials.service_account_json 仍是明文，应允许保存
    const account = buildVertexAccount()
    expect(account.credentials_status).toBeUndefined()
    expect(account.credentials.service_account_json).toBeTruthy()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
  })

  it('blocks Vertex SA save when neither credentials_status nor legacy json indicates existence', async () => {
    const account = buildVertexAccount()
    account.credentials = {
      project_id: 'demo-project',
      client_email: 'sa@example.iam.gserviceaccount.com',
      location: 'us-central1',
      tier_id: 'vertex'
    }
    // 既没有 credentials_status 也没有旧的 service_account_json
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).not.toHaveBeenCalled()
  })

  it('loads and submits Antigravity configured project fallback', async () => {
    const account = buildAntigravityAccount('configured-project')
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const input = wrapper.get<HTMLInputElement>('[data-testid="antigravity-project-id-input"]')
    expect(input.element.value).toBe('configured-project')

    await input.setValue('  updated-project  ')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.antigravity_project_id).toBe(
      'updated-project'
    )
  })

  it('clears Antigravity configured project fallback when input is empty', async () => {
    const account = buildAntigravityAccount('configured-project')
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const input = wrapper.get<HTMLInputElement>('[data-testid="antigravity-project-id-input"]')

    await input.setValue('')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).not.toHaveProperty(
      'antigravity_project_id'
    )
  })
})

describe('EditAccountModal OpenAI 自动使用重置卡', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })
  })

  it('仅对 OpenAI OAuth 母账号显示，默认关闭且阈值为 100/100', () => {
    const parent = mountModal(buildOpenAIOAuthParentAccount())
    expect(parent.find('[data-testid="auto-reset-credit-settings"]').exists()).toBe(true)
    expect((parent.get('[data-testid="auto-reset-credit-5h-threshold"]').element as HTMLInputElement).value).toBe('100')
    expect((parent.get('[data-testid="auto-reset-credit-7d-threshold"]').element as HTMLInputElement).value).toBe('100')
    expect(parent.get('[data-testid="auto-reset-credit-5h-threshold"]').attributes('disabled')).toBeDefined()
    parent.unmount()

    for (const account of [buildAccount(), buildOpenAISetupTokenAccount(), buildOpenAISparkShadowAccount()]) {
      const wrapper = mountModal(account)
      expect(wrapper.find('[data-testid="auto-reset-credit-settings"]').exists()).toBe(false)
      wrapper.unmount()
    }
  })

  it('独立保存两个阈值，并禁止把运行态回写到管理请求', async () => {
    const account = buildOpenAIOAuthParentAccount()
    account.extra = {
      codex_auto_reset_credit_state: {
        status: 'success',
        trigger_window: '5h',
        available_count: 1
      }
    }
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    await wrapper.get('[data-testid="auto-reset-credit-enabled"]').trigger('click')
    await wrapper.get('[data-testid="auto-reset-credit-5h-threshold"]').setValue('75.5')
    await wrapper.get('[data-testid="auto-reset-credit-7d-threshold"]').setValue('92')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    const extra = updateAccountMock.mock.calls[0]?.[1]?.extra
    expect(extra).toMatchObject({
      auto_reset_credit_enabled: true,
      auto_reset_credit_5h_threshold: 0.755,
      auto_reset_credit_7d_threshold: 0.92
    })
    expect(extra).not.toHaveProperty('codex_auto_reset_credit_state')
    wrapper.unmount()
  })

  it('开启后拒绝超出 0.1–100 范围的任一阈值', async () => {
    const wrapper = mountModal(buildOpenAIOAuthParentAccount())
    await wrapper.get('[data-testid="auto-reset-credit-enabled"]').trigger('click')
    await wrapper.get('[data-testid="auto-reset-credit-5h-threshold"]').setValue('0')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    expect(updateAccountMock).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
