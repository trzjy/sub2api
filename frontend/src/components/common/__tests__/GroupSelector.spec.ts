import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import GroupSelector from '../GroupSelector.vue'

const authState = { isSimpleMode: false }

vi.mock('@/stores', () => ({ useAuthStore: () => authState }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const groups = [
  { id: 1, name: 'Basic', platform: 'anthropic', status: 'active' },
  { id: 2, name: 'Composite', platform: 'composite', status: 'active' }
] as any

const mountSelector = (modelValue: number[] = []) => mount(GroupSelector, {
  props: { modelValue, groups },
  global: { stubs: { GroupBadge: { props: ['name'], template: '<span>{{ name }}</span>' }, Icon: true } }
})

describe('GroupSelector simple-mode binding policy', () => {
  beforeEach(() => { authState.isSimpleMode = false })

  it('hides composite groups in simple mode and preserves basic groups', () => {
    authState.isSimpleMode = true
    const wrapper = mountSelector()
    expect(wrapper.text()).toContain('Basic')
    expect(wrapper.text()).not.toContain('Composite')
  })

  it('keeps composite groups available in advanced mode', () => {
    const wrapper = mountSelector()
    expect(wrapper.text()).toContain('Composite')
  })

  it('cleans hidden historical composite IDs while preserving visible selections', () => {
    authState.isSimpleMode = true
    const wrapper = mountSelector([1, 2])
    expect(wrapper.emitted('update:modelValue')).toEqual([[[1]]])
  })
})

describe('GroupSelector native platforms + bound-group visibility (U7)', () => {
  beforeEach(() => { authState.isSimpleMode = false })

  const cursorGroups = [
    { id: 10, name: 'claude-max', platform: 'anthropic', status: 'active' },
    { id: 11, name: 'grok-max', platform: 'grok', status: 'active' },
    { id: 12, name: 'muse-spark', platform: 'muse', status: 'active' },
    { id: 13, name: 'cursor-own', platform: 'cursor', status: 'active' },
    { id: 14, name: 'Composite', platform: 'composite', status: 'active' },
    { id: 15, name: 'welfare', platform: 'composite', status: 'active' }
  ] as any

  const mountCursorSelector = (modelValue: number[], nativePlatforms?: string[]) =>
    mount(GroupSelector, {
      props: {
        modelValue,
        groups: cursorGroups,
        platform: 'cursor',
        nativePlatforms
      },
      global: { stubs: { GroupBadge: { props: ['name'], template: '<span>{{ name }}</span>' }, Icon: true } }
    })

  it('keeps same-platform + composite only without nativePlatforms (existing rule)', () => {
    const wrapper = mountCursorSelector([])
    const text = wrapper.text()
    expect(text).toContain('cursor-own')
    expect(text).toContain('Composite')
    expect(text).not.toContain('claude-max')
    expect(text).not.toContain('grok-max')
    expect(text).not.toContain('muse-spark')
  })

  it('reveals model-native platform groups when declared (U7)', () => {
    const wrapper = mountCursorSelector([], ['anthropic', 'openai', 'gemini', 'grok', 'muse'])
    const text = wrapper.text()
    expect(text).toContain('claude-max')
    expect(text).toContain('grok-max')
    expect(text).toContain('muse-spark')
    expect(text).toContain('cursor-own')
    expect(text).toContain('Composite')
  })

  it('always shows already-bound groups even when platform filter hides them (no implicit drop)', () => {
    // 已绑定 muse 组（id 12）但不放行 muse 平台：既有绑定必须可见，供用户保留或显式取消。
    const wrapper = mountCursorSelector([12])
    expect(wrapper.text()).toContain('muse-spark')
  })

  it('does not reveal bound groups of other accounts (only modelValue members)', () => {
    // 未绑定的原生组在未放行时仍隐藏（不无差别放开跨平台绑定）。
    const wrapper = mountCursorSelector([13])
    const text = wrapper.text()
    expect(text).toContain('cursor-own')
    expect(text).not.toContain('claude-max')
    expect(text).not.toContain('grok-max')
  })

  it('keeps simple-mode composite cleanup untouched even for bound groups', () => {
    authState.isSimpleMode = true
    // simple mode：composite 既有清理不变——已绑定 composite 组不补回显示。
    const wrapper = mountCursorSelector([14])
    expect(wrapper.text()).not.toContain('Composite')
  })
})

describe('GroupSelector composite group filter by accountModels (U5)', () => {
  beforeEach(() => { authState.isSimpleMode = false })

  const modelFamilyGroups = [
    { id: 1, name: 'claude-standard', platform: 'anthropic', subscription_type: 'standard', status: 'active' },
    { id: 2, name: 'claude-subscription', platform: 'anthropic', subscription_type: 'subscription', status: 'active' },
    { id: 3, name: 'gpt-group', platform: 'openai', status: 'active' },
    { id: 4, name: 'gemini-group', platform: 'gemini', status: 'active' },
    { id: 5, name: 'cn-aggregate', platform: 'composite', status: 'active', model_allowlist: { enabled: true, models: ['glm-5.3', 'kimi-k3'] } },
    { id: 6, name: 'disabled-aggregate', platform: 'composite', status: 'active', model_allowlist: { enabled: false, models: ['claude-fable-5-1'] } },
    { id: 7, name: 'claude-wildcard-aggregate', platform: 'composite', status: 'active', model_allowlist: { enabled: true, models: ['claude-*'] } },
    { id: 8, name: 'claude-exact-aggregate', platform: 'composite', status: 'active', model_allowlist: { enabled: true, models: ['claude-fable-5-1'] } },
    { id: 9, name: 'empty-aggregate', platform: 'composite', status: 'active', model_allowlist: { enabled: true, models: [] } }
  ] as any

  const mountModelFamilySelector = (modelValue: number[], accountModels?: string[]) =>
    mount(GroupSelector, {
      props: {
        modelValue,
        groups: modelFamilyGroups,
        platform: 'cursor',
        nativePlatforms: ['anthropic'],
        accountModels
      },
      global: { stubs: { GroupBadge: { props: ['name'], template: '<span>{{ name }}</span>' }, Icon: true } }
    })

  it('shows anthropic groups, hides openai/gemini, filters composite by accountModels', () => {
    const wrapper = mountModelFamilySelector([], ['claude-fable-5-1'])
    const text = wrapper.text()
    // anthropic 组（含 standard 与 subscription）均显示
    expect(text).toContain('claude-standard')
    expect(text).toContain('claude-subscription')
    // openai / gemini 组不显示（未放行）
    expect(text).not.toContain('gpt-group')
    expect(text).not.toContain('gemini-group')
    // composite 组白名单全是 glm/kimi 或 enabled=false 时不显示
    expect(text).not.toContain('cn-aggregate')
    expect(text).not.toContain('disabled-aggregate')
    expect(text).not.toContain('empty-aggregate')
    // composite 组白名单含 claude-* 或 claude-fable-5-1 时显示
    expect(text).toContain('claude-wildcard-aggregate')
    expect(text).toContain('claude-exact-aggregate')
  })

  it('keeps already-bound non-matching composite groups visible (bound-but-hidden rule)', () => {
    // 已绑定 cn-aggregate（id 5，白名单 glm/kimi，不匹配）仍受恒可见规则保护显示。
    const wrapper = mountModelFamilySelector([5], ['claude-fable-5-1'])
    expect(wrapper.text()).toContain('cn-aggregate')
  })

  it('shows all composite groups when accountModels is undefined (existing behavior)', () => {
    // 不传 accountModels：行为与现状完全一致，composite 组全显示。
    const wrapper = mountModelFamilySelector([])
    const text = wrapper.text()
    expect(text).toContain('cn-aggregate')
    expect(text).toContain('disabled-aggregate')
    expect(text).toContain('claude-wildcard-aggregate')
    expect(text).toContain('claude-exact-aggregate')
    expect(text).toContain('empty-aggregate')
    expect(text).toContain('claude-standard')
    expect(text).toContain('claude-subscription')
    expect(text).not.toContain('gpt-group')
    expect(text).not.toContain('gemini-group')
  })
})

describe('GroupSelector disabled readonly mode (D3)', () => {
  beforeEach(() => { authState.isSimpleMode = false })

  const disabledGroups = [
    { id: 1, name: 'Basic', platform: 'anthropic', status: 'active' },
    { id: 2, name: 'Composite', platform: 'composite', status: 'active' }
  ] as any

  const mountDisabledSelector = (modelValue: number[] = [], disabled = true) =>
    mount(GroupSelector, {
      props: { modelValue, groups: disabledGroups, disabled },
      global: { stubs: { GroupBadge: { props: ['name'], template: '<span>{{ name }}</span>' }, Icon: true } }
    })

  it('disabled=true: all checkboxes disabled and change does not emit update:modelValue', async () => {
    const wrapper = mountDisabledSelector([1])
    const checkboxes = wrapper.findAll('input[type=checkbox]')
    expect(checkboxes.length).toBeGreaterThan(0)
    checkboxes.forEach((cb) => expect((cb.element as HTMLInputElement).disabled).toBe(true))
    // 只读兜底：触发 change 仍不应产生 update:modelValue
    await checkboxes[0].setValue(true)
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })

  it('without disabled prop: regression - checkboxes enabled and change emits update:modelValue', async () => {
    const wrapper = mountDisabledSelector([1], false)
    const checkboxes = wrapper.findAll('input[type=checkbox]')
    checkboxes.forEach((cb) => expect((cb.element as HTMLInputElement).disabled).toBe(false))
    // 未传 disabled：既有行为不变，change 正常触发 update:modelValue
    const unchecked = checkboxes.find((cb) => !(cb.element as HTMLInputElement).checked)!
    await unchecked.setValue(true)
    expect(wrapper.emitted('update:modelValue')).toBeDefined()
    expect(wrapper.text()).toContain('Basic')
    expect(wrapper.text()).toContain('Composite')
  })
})
