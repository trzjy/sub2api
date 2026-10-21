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
