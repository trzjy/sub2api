import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import CodeBuddyShadowWizard from '../CodeBuddyShadowWizard.vue'
import GroupSelector from '@/components/common/GroupSelector.vue'
import {
  OFFICIAL_PRICE_CONCURRENCY,
  resetCodeBuddyOfficialPriceCache,
} from '../codeBuddyOfficialPrice'
import {
  codeBuddyShadowName,
  normalizeCodeBuddySite,
  parseCodeBuddyShadowSite,
} from '@/constants/platforms'
import type { Account, AccountListItem, AdminGroup } from '@/types'

// 向导与 GroupSelector 均走真实组件（验证「复用而非新造」），只 mock 网络与全局 store。
const { syncUpstreamModels, createCodeBuddyShadow, getCatalog } = vi.hoisted(() => ({
  syncUpstreamModels: vi.fn(),
  createCodeBuddyShadow: vi.fn(),
  getCatalog: vi.fn(),
}))

vi.mock('@/api/admin/accounts', () => ({
  syncUpstreamModels,
  createCodeBuddyShadow,
}))

vi.mock('@/api/admin/pricing', () => ({
  getCatalog,
}))

vi.mock('@/stores', () => ({
  useAuthStore: () => ({ isSimpleMode: false }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    // 带参插值一并回显，便于断言 (模型, 分组, 错误) 是否正确传到了文案里
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key} ${JSON.stringify(params)}` : key,
    }),
  }
})

// ---- fixtures ----

const makeAccount = (overrides: Partial<Account>): Account =>
  ({
    id: 1,
    name: 'account',
    platform: 'codebuddy',
    type: 'oauth',
    proxy_id: null,
    concurrency: 3,
    priority: 50,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  }) as Account

const makeShadow = (overrides: Partial<AccountListItem>): AccountListItem =>
  makeAccount(overrides as Partial<Account>) as unknown as AccountListItem

const makeGroup = (id: number, name: string, platform: string): AdminGroup =>
  ({
    id,
    name,
    platform,
    status: 'active',
    description: null,
    rate_multiplier: 1,
    is_exclusive: false,
    subscription_type: 'standard',
  }) as unknown as AdminGroup

const GROUPS: AdminGroup[] = [
  makeGroup(1, 'ds-primary', 'deepseek'),
  makeGroup(2, 'ds-backup', 'deepseek'),
  makeGroup(3, 'zhipu-main', 'zhipu'),
  makeGroup(4, 'ds-retired', 'deepseek'),
]
// 仅 id=4 停用，用于验证「启用」过滤仍在（平台过滤由 GroupSelector 负责）
GROUPS[3]!.status = 'inactive'

const SYNC_RESULT = {
  models: ['deepseek-v3', 'glm-4.5'],
  metadata: {
    'deepseek-v3': { id: 'deepseek-v3', context_window: 128000, max_output_tokens: 8192 },
    'glm-4.5': { id: 'glm-4.5', context_window: 200000, max_output_tokens: 16384 },
  },
}

// ---- mount helpers（组件用 Teleport，DOM 断言一律走 document.body）----

const wrappers: { unmount: () => void }[] = []

const mountWizard = (opts: { parent?: Account; shadows?: AccountListItem[]; groups?: AdminGroup[] } = {}) => {
  const wrapper = mount(CodeBuddyShadowWizard, {
    props: {
      show: true,
      parent: opts.parent ?? makeAccount({ id: 10, name: 'jossin', credentials: { site: 'cn' } }),
      shadows: opts.shadows ?? [],
      groups: opts.groups ?? GROUPS,
    },
    attachTo: document.body,
    global: {
      stubs: { GroupBadge: true },
    },
  })
  wrappers.push(wrapper)
  return wrapper
}

const q = <T extends Element>(selector: string): T | null => document.body.querySelector<T>(selector)

const clickEl = async (el: Element | null) => {
  expect(el, 'target element must exist').toBeTruthy()
  el!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await nextTick()
  // 批量创建是逐个 await 的串行循环，只 flush 一次 tick 会停在中间态。
  await flushPromises()
}

const clickButtonByText = async (text: string) => {
  const btn = Array.from(document.body.querySelectorAll('button')).find((b) =>
    (b.textContent ?? '').includes(text),
  )
  await clickEl(btn ?? null)
}

const checkRow = async (model: string, checked = true) => {
  const box = q<HTMLInputElement>(`[data-test="codebuddy-select-${model}"]`)
  expect(box, `row checkbox for ${model}`).toBeTruthy()
  box!.checked = checked
  box!.dispatchEvent(new Event('change'))
  await nextTick()
}

/** 打开某行的分组选择器并写入所选分组（选择器是真实 GroupSelector）。 */
const setRowGroups = async (wrapper: ReturnType<typeof mountWizard>, model: string, groupIds: number[]) => {
  await clickEl(q(`[data-test="codebuddy-group-picker-${model}"]`))
  const selectors = wrapper.findAllComponents(GroupSelector)
  expect(selectors.length).toBe(1)
  selectors[0]!.vm.$emit('update:modelValue', groupIds)
  await nextTick()
  return selectors[0]!
}

const syncTable = async () => {
  await clickButtonByText('admin.accounts.codeBuddySyncModels')
  await nextTick()
  await nextTick()
}

beforeEach(() => {
  for (const fn of [syncUpstreamModels, createCodeBuddyShadow, getCatalog]) fn.mockReset()
  // 官方价缓存是模块级的（会话内只查一次），用例之间必须清空才能断言调用次数。
  resetCodeBuddyOfficialPriceCache()
  syncUpstreamModels.mockResolvedValue(SYNC_RESULT)
  createCodeBuddyShadow.mockImplementation(async (_parentId: number, payload: { model: string }) => ({
    id: 900,
    name: payload.model,
    extra: { shadow_model: payload.model },
  }))
  getCatalog.mockResolvedValue({ items: [], total: 0 })
})

afterEach(() => {
  for (const w of wrappers.splice(0)) w.unmount()
  document.body.innerHTML = ''
  vi.restoreAllMocks()
})

// ================= N2：倍率列移除 =================

describe('CodeBuddyShadowWizard — N2 删倍率列', () => {
  it('表格不再渲染「倍率」列，计费单轨走分组定价', async () => {
    mountWizard()
    await syncTable()

    const headers = Array.from(document.body.querySelectorAll('th')).map((th) => th.textContent?.trim())
    expect(headers).toContain('admin.accounts.codeBuddyColGroup')
    expect(headers).toContain('admin.accounts.codeBuddyColPriority')
    expect(headers).not.toContain('admin.accounts.codeBuddyColMultiplier')

    const html = document.body.innerHTML
    expect(html).not.toContain('codeBuddyColMultiplier')
  })

  it('创建载荷不含 multiplier 字段', async () => {
    const wrapper = mountWizard()
    await syncTable()
    await checkRow('deepseek-v3')
    await setRowGroups(wrapper, 'deepseek-v3', [1])
    await clickEl(q('[data-test="codebuddy-create"]'))

    expect(createCodeBuddyShadow).toHaveBeenCalledTimes(1)
    const payload = createCodeBuddyShadow.mock.calls[0]![1] as Record<string, unknown>
    expect(payload).not.toHaveProperty('multiplier')
    expect(payload).toMatchObject({ model: 'deepseek-v3', platform: 'deepseek', group_ids: [1] })
  })
})

// ================= N2：官方价接线 =================

describe('CodeBuddyShadowWizard — N2 官方价列', () => {
  it('已覆盖模型显示目录价，未覆盖模型显示「未定价 → 去配置」', async () => {
    getCatalog.mockImplementation(async ({ search }: { search: string }) =>
      search === 'deepseek-v3'
        ? {
            items: [
              {
                model: 'deepseek-v3',
                source: 'litellm',
                input_per_mtok: 0.28,
                output_per_mtok: 1.1,
              },
            ],
            total: 1,
          }
        : { items: [], total: 0 },
    )

    mountWizard()
    await syncTable()
    await nextTick()

    expect(document.body.textContent).toContain('$0.28 / $1.1')
    expect(document.body.textContent).toContain('admin.accounts.codeBuddyPriceUnpriced')
    expect(document.body.textContent).toContain('admin.accounts.codeBuddyConfigurePrice')

    // 按模型逐个精确查询，且只查一次
    expect(getCatalog.mock.calls.map((c) => (c[0] as { search: string }).search).sort()).toEqual([
      'deepseek-v3',
      'glm-4.5',
    ])
  })

  it('目录里 source=none 的条目不算已定价', async () => {
    getCatalog.mockResolvedValue({
      items: [{ model: 'deepseek-v3', source: 'none', input_per_mtok: 0, output_per_mtok: 0 }],
      total: 1,
    })

    mountWizard()
    await syncTable()
    await nextTick()

    const row = q('[data-test="codebuddy-select-deepseek-v3"]')?.closest('tr')
    expect(row?.textContent).toContain('admin.accounts.codeBuddyPriceUnpriced')
  })

  it('引导态/组件挂载时不查价，只有进入表格态才按模型查', async () => {
    mountWizard()
    await flushPromises()
    expect(getCatalog).not.toHaveBeenCalled()

    await syncTable()
    await flushPromises()
    expect(getCatalog).toHaveBeenCalledTimes(2)
  })

  it('单条查询失败只让该模型显示「未定价」，不冒泡错误态也不阻塞表格', async () => {
    getCatalog.mockRejectedValue(new Error('network down'))

    mountWizard()
    await syncTable()
    await nextTick()
    await flushPromises()

    const row = q('[data-test="codebuddy-select-deepseek-v3"]')?.closest('tr')
    expect(row?.textContent).toContain('admin.accounts.codeBuddyPriceUnpriced')
    expect(row?.textContent).toContain('admin.accounts.codeBuddyConfigurePrice')
    // 表格整体仍完整渲染（两行都在），且没有任何错误态文案冒泡出来
    expect(document.body.querySelectorAll('tbody tr')).toHaveLength(2)
    expect(document.body.textContent).not.toContain('codeBuddyPriceUnavailable')
  })

  it('目录同名多来源命中时优先自定义/覆盖层（非 litellm）', async () => {
    getCatalog.mockImplementation(async ({ search }: { search: string }) =>
      search === 'deepseek-v3'
        ? {
            items: [
              { model: 'deepseek-v3', source: 'litellm', input_per_mtok: 1, output_per_mtok: 2 },
              { model: 'DEEPSEEK-V3', source: 'custom', input_per_mtok: 0.5, output_per_mtok: 1.5 },
            ],
            total: 2,
          }
        : { items: [], total: 0 },
    )

    mountWizard()
    await syncTable()
    await nextTick()
    await flushPromises()

    const row = q('[data-test="codebuddy-select-deepseek-v3"]')?.closest('tr')
    expect(row?.textContent).toContain('$0.5 / $1.5')
    expect(row?.textContent).not.toContain('$1 / $2')
  })

  it('官方价缓存为模块级：重新打开向导不重复查询目录', async () => {
    const wrapper = mountWizard()
    await syncTable()
    await flushPromises()
    expect(getCatalog).toHaveBeenCalledTimes(2)

    await wrapper.setProps({ show: false })
    await nextTick()
    await wrapper.setProps({ show: true })
    await nextTick()
    await syncTable()
    await flushPromises()

    // 同一会话内同一模型只查一次
    expect(getCatalog).toHaveBeenCalledTimes(2)
    expect(document.body.textContent).toContain('admin.accounts.codeBuddyPriceUnpriced')
  })

  it('目录查询并发不超过 4', async () => {
    const models = Array.from({ length: 10 }, (_, i) => `model-${i}`)
    syncUpstreamModels.mockResolvedValue({
      models,
      metadata: Object.fromEntries(
        models.map((m) => [m, { id: m, context_window: 1000, max_output_tokens: 100 }]),
      ),
    })
    let inflight = 0
    let peak = 0
    getCatalog.mockImplementation(async () => {
      inflight += 1
      peak = Math.max(peak, inflight)
      await Promise.resolve()
      inflight -= 1
      return { items: [], total: 0 }
    })

    mountWizard()
    await syncTable()
    await flushPromises()

    expect(getCatalog).toHaveBeenCalledTimes(10)
    expect(peak).toBeLessThanOrEqual(OFFICIAL_PRICE_CONCURRENCY)
  })
})

// ================= N3：站点命名与徽标 =================

describe('CodeBuddyShadowWizard — N3 站点命名与徽标', () => {
  it('向导标题展示母账号站点徽标', async () => {
    mountWizard({ parent: makeAccount({ id: 10, name: 'jossin', credentials: { site: 'intl' } }) })
    const badge = q('[data-test="codebuddy-wizard-site-badge"]')
    expect(badge?.textContent?.trim()).toBe('admin.accounts.codeBuddySiteIntl')
  })

  it('创建默认名为 <母账号名>:<site>:<model>', async () => {
    const wrapper = mountWizard()
    await syncTable()
    await checkRow('deepseek-v3')
    await setRowGroups(wrapper, 'deepseek-v3', [1])
    await clickEl(q('[data-test="codebuddy-create"]'))

    expect(createCodeBuddyShadow).toHaveBeenCalledWith(
      10,
      expect.objectContaining({ name: 'jossin:cn:deepseek-v3' }),
    )
  })

  it('一模型多分组时名字追加分组短名做消歧', async () => {
    const wrapper = mountWizard()
    await syncTable()
    await checkRow('deepseek-v3')
    await setRowGroups(wrapper, 'deepseek-v3', [1, 2])
    await clickEl(q('[data-test="codebuddy-create"]'))

    const names = createCodeBuddyShadow.mock.calls.map((c) => (c[1] as { name: string }).name).sort()
    expect(names).toEqual(['jossin:cn:deepseek-v3:ds-backup', 'jossin:cn:deepseek-v3:ds-primary'])
  })

  it('影子行站点徽标来自既有影子名；存量命名（无 site 段）不显示徽标并给出提示', async () => {
    mountWizard({
      shadows: [
        makeShadow({
          id: 901,
          name: 'jossin:cn:deepseek-v3',
          extra: { shadow_model: 'deepseek-v3' },
          group_ids: [1],
        }),
        makeShadow({
          id: 902,
          name: 'jossin:glm-4.5',
          extra: { shadow_model: 'glm-4.5' },
          group_ids: [3],
        }),
      ],
    })
    await syncTable()

    const badges = Array.from(
      document.body.querySelectorAll('[data-test="codebuddy-row-site-badge"]'),
    ).map((el) => el.textContent?.trim())
    expect(badges).toEqual(['admin.accounts.codeBuddySiteCn'])

    const legacy = Array.from(
      document.body.querySelectorAll('[data-test="codebuddy-row-legacy-name"]'),
    )
    expect(legacy.length).toBe(1)
    expect(legacy[0]!.getAttribute('title')).toBe('admin.accounts.codeBuddyLegacyNameHint')
  })

  it('命名/解析纯函数覆盖 intl 与缺省 cn', () => {
    expect(codeBuddyShadowName('jossin', 'intl', 'claude-fable-5')).toBe(
      'jossin:intl:claude-fable-5',
    )
    expect(codeBuddyShadowName('jossin', undefined, 'deepseek-v3')).toBe('jossin:cn:deepseek-v3')
    expect(codeBuddyShadowName('jossin', 'cn', 'deepseek-v3', 'ds-primary')).toBe(
      'jossin:cn:deepseek-v3:ds-primary',
    )
    // 后端 ent 列 MaxLen(100)：超长必须截断，否则创建时裸 500
    const long = codeBuddyShadowName('p'.repeat(80), 'cn', 'm'.repeat(80))
    expect(Array.from(long).length).toBe(100)

    expect(normalizeCodeBuddySite('INTL')).toBe('intl')
    expect(normalizeCodeBuddySite('cn')).toBe('cn')
    expect(normalizeCodeBuddySite(undefined)).toBe('cn')
    expect(parseCodeBuddyShadowSite('jossin:cn:deepseek-v3')).toBe('cn')
    expect(parseCodeBuddyShadowSite('jossin:intl:claude-fable-5:x')).toBe('intl')
    expect(parseCodeBuddyShadowSite('jossin:deepseek-v3')).toBeNull()
    expect(parseCodeBuddyShadowSite('claude-fable-5')).toBeNull()
  })
})

// ================= N4：一模型多分组 =================

describe('CodeBuddyShadowWizard — N4 一模型多分组', () => {
  it('分组选择复用 GroupSelector 并按目标平台过滤', async () => {
    const wrapper = mountWizard()
    await syncTable()
    const selector = await setRowGroups(wrapper, 'deepseek-v3', [1])

    expect(selector.props('platform')).toBe('deepseek')
    // 停用分组不进选择器候选
    const ids = (selector.props('groups') as AdminGroup[]).map((g) => g.id).sort()
    expect(ids).toEqual([1, 2, 3])
  })

  it('一个模型绑 3 个分组 → 3 个创建请求，各自独立优先级', async () => {
    const wrapper = mountWizard()
    await syncTable()
    await checkRow('deepseek-v3')
    const selector = await setRowGroups(wrapper, 'deepseek-v3', [1, 2])
    expect(selector.props('platform')).toBe('deepseek')
    // 不同模型的行各自维护选择；glm 行绑一个 zhipu 分组
    await checkRow('glm-4.5')
    await setRowGroups(wrapper, 'glm-4.5', [3])
    await clickEl(q('[data-test="codebuddy-create"]'))

    expect(createCodeBuddyShadow).toHaveBeenCalledTimes(3)
    const payloads = createCodeBuddyShadow.mock.calls.map(
      (c) => c[1] as { model: string; group_ids: number[]; priority: number; platform: string },
    )
    expect(payloads.map((p) => `${p.model}:${p.group_ids[0]}`).sort()).toEqual([
      'deepseek-v3:1',
      'deepseek-v3:2',
      'glm-4.5:3',
    ])
    expect(payloads.every((p) => p.priority === 50)).toBe(true)
  })

  it('已存在的 (模型, 分组) 组合被跳过', async () => {
    const wrapper = mountWizard({
      shadows: [
        makeShadow({
          id: 903,
          name: 'jossin:cn:deepseek-v3',
          extra: { shadow_model: 'deepseek-v3' },
          group_ids: [1],
        }),
      ],
    })
    await syncTable()
    await checkRow('deepseek-v3')
    await setRowGroups(wrapper, 'deepseek-v3', [1, 2])
    await clickEl(q('[data-test="codebuddy-create"]'))

    expect(createCodeBuddyShadow).toHaveBeenCalledTimes(1)
    expect(createCodeBuddyShadow.mock.calls[0]![1]).toMatchObject({
      name: 'jossin:cn:deepseek-v3:ds-backup',
      group_ids: [2],
    })
  })

  it('已有影子的模型在未选分组时状态显示「已创建」而非「未创建」', async () => {
    mountWizard({
      shadows: [
        makeShadow({
          id: 906,
          name: 'jossin:cn:deepseek-v3',
          extra: { shadow_model: 'deepseek-v3' },
          group_ids: [1],
        }),
      ],
    })
    await syncTable()

    const createdRow = q('[data-test="codebuddy-select-deepseek-v3"]')?.closest('tr')
    expect(createdRow?.textContent).toContain('admin.accounts.codeBuddyStatusCreated')
    const emptyRow = q('[data-test="codebuddy-select-glm-4.5"]')?.closest('tr')
    expect(emptyRow?.textContent).toContain('admin.accounts.codeBuddyStatusNotCreated')
  })

  it('全部 (模型, 分组) 已存在时创建按钮禁用', async () => {
    const wrapper = mountWizard({
      shadows: [
        makeShadow({
          id: 904,
          name: 'jossin:cn:deepseek-v3',
          extra: { shadow_model: 'deepseek-v3' },
          group_ids: [1, 2],
        }),
      ],
    })
    await syncTable()
    await checkRow('deepseek-v3')
    await setRowGroups(wrapper, 'deepseek-v3', [1, 2])

    expect(q<HTMLButtonElement>('[data-test="codebuddy-create"]')?.disabled).toBe(true)
  })

  it('部分失败按 (模型, 分组) 二元组展示', async () => {
    createCodeBuddyShadow.mockImplementation(async (_parentId: number, payload: { model: string; group_ids: number[] }) => {
      if (payload.group_ids[0] === 2) throw new Error('CODEBUDDY_SHADOW_MODEL_EXISTS')
      return { id: 905, name: payload.model, extra: { shadow_model: payload.model } }
    })

    const wrapper = mountWizard()
    await syncTable()
    await checkRow('deepseek-v3')
    await setRowGroups(wrapper, 'deepseek-v3', [1, 2])
    await clickEl(q('[data-test="codebuddy-create"]'))

    expect(document.body.textContent).toContain(
      'admin.accounts.codeBuddyBatchCreatePartial',
    )
    const failed = q('[data-test="codebuddy-create-failed-deepseek-v3-2"]')
    expect(failed?.textContent).toContain('deepseek-v3')
    expect(failed?.textContent).toContain('ds-backup')
    expect(failed?.textContent).toContain('CODEBUDDY_SHADOW_MODEL_EXISTS')
  })
})

// ================= 空态 =================

describe('CodeBuddyShadowWizard — 空态与选择态', () => {
  it('未同步时显示引导态', async () => {
    mountWizard()
    expect(document.body.textContent).toContain('admin.accounts.codeBuddyUnsyncedGuide')
  })

  it('同步失败回到引导态并展示错误', async () => {
    syncUpstreamModels.mockRejectedValue(new Error('boom'))
    mountWizard()
    await syncTable()

    expect(document.body.textContent).toContain('admin.accounts.codeBuddySyncFailed')
    expect(document.body.textContent).toContain('admin.accounts.codeBuddyUnsyncedGuide')
  })

  it('同步返回空模型列表时不进入表格', async () => {
    syncUpstreamModels.mockResolvedValue({ models: [] })
    mountWizard()
    await syncTable()

    expect(document.body.textContent).toContain('admin.accounts.codeBuddySyncFailed')
    expect(document.body.querySelector('table')).toBeNull()
  })

  it('勾选模型但未选分组时创建按钮禁用', async () => {
    mountWizard()
    await syncTable()
    await checkRow('deepseek-v3')

    expect(q<HTMLButtonElement>('[data-test="codebuddy-create"]')?.disabled).toBe(true)
  })
})
