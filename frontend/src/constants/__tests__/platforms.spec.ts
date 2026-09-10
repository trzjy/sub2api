import { describe, expect, it } from 'vitest'
import {
  CONCRETE_PLATFORM_OPTIONS,
  GROUP_PLATFORM_OPTIONS,
  filterUpstreamModelsByPlatform,
  modelMatchesPlatform,
} from '@/constants/platforms'

const concretePlatforms = [
  'anthropic',
  'openai',
  'gemini',
  'antigravity',
  'grok',
  'kimi',
  'zhipu',
  'deepseek',
  'other',
  'minimax'
]

describe('platform option catalogs', () => {
  it('exposes every concrete account platform', () => {
    expect(CONCRETE_PLATFORM_OPTIONS.map((option) => option.value)).toEqual(concretePlatforms)
  })

  it('adds composite for group-backed filters', () => {
    expect(GROUP_PLATFORM_OPTIONS.map((option) => option.value)).toEqual([
      ...concretePlatforms,
      'composite'
    ])
  })
})

describe('modelMatchesPlatform', () => {
  it('matches vendor-prefixed and bare ids within the same family', () => {
    expect(modelMatchesPlatform('deepseek/deepseek-v4-flash', 'deepseek')).toBe(true)
    expect(modelMatchesPlatform('deepseek-chat', 'deepseek')).toBe(true)
    expect(modelMatchesPlatform('deepseek/deepseek-r1-distill-qwen-32b', 'deepseek')).toBe(true)
    expect(modelMatchesPlatform('anthropic/claude-opus-4.6', 'anthropic')).toBe(true)
    expect(modelMatchesPlatform('kimi/kimi-k2', 'kimi')).toBe(true)
    expect(modelMatchesPlatform('moonshot-v1-8k', 'kimi')).toBe(true)
    expect(modelMatchesPlatform('zhipu/glm-4.6', 'zhipu')).toBe(true)
    expect(modelMatchesPlatform('glm-4', 'zhipu')).toBe(true)
    expect(modelMatchesPlatform('minimax/minimax-m2', 'minimax')).toBe(true)
    expect(modelMatchesPlatform('grok-4', 'grok')).toBe(true)
    expect(modelMatchesPlatform('gemini/gemini-2.5-pro', 'gemini')).toBe(true)
    expect(modelMatchesPlatform('gemini-2.5-pro', 'antigravity')).toBe(true)
  })

  it('rejects cross-family models', () => {
    expect(modelMatchesPlatform('qwen/qwen3-32b', 'deepseek')).toBe(false)
    expect(modelMatchesPlatform('anthropic/claude-opus-4.6', 'deepseek')).toBe(false)
    expect(modelMatchesPlatform('bytedance/seed/seed-1.6', 'zhipu')).toBe(false)
    expect(modelMatchesPlatform('deepseek/deepseek-chat', 'kimi')).toBe(false)
  })

  it('matches openai family including boundary-guarded o-series', () => {
    expect(modelMatchesPlatform('gpt-4o', 'openai')).toBe(true)
    expect(modelMatchesPlatform('openai/gpt-5', 'openai')).toBe(true)
    expect(modelMatchesPlatform('o3-mini', 'openai')).toBe(true)
    expect(modelMatchesPlatform('chatgpt-4o-latest', 'openai')).toBe(true)
    expect(modelMatchesPlatform('text-embedding-3-large', 'openai')).toBe(true)
    expect(modelMatchesPlatform('codex-mini', 'openai')).toBe(true)
    expect(modelMatchesPlatform('qwen-o', 'openai')).toBe(false)
    expect(modelMatchesPlatform('deepseek/deepseek-chat', 'openai')).toBe(false)
  })

  it('treats platforms without a family pattern as always matching', () => {
    expect(modelMatchesPlatform('anything/any-model', 'other')).toBe(true)
    expect(modelMatchesPlatform('anything/any-model', 'unknown-platform')).toBe(true)
  })
})

describe('filterUpstreamModelsByPlatform', () => {
  const upstream = [
    'deepseek/deepseek-v4-flash',
    'deepseek-chat',
    'qwen/qwen3-32b',
    'anthropic/claude-opus-4.6',
    'qwen/qwen3-coder'
  ]

  it('keeps only platform-family models for a deepseek account', () => {
    const result = filterUpstreamModelsByPlatform(upstream, ['deepseek'])
    expect(result.filterApplied).toBe(true)
    expect(result.kept).toEqual(['deepseek/deepseek-v4-flash', 'deepseek-chat'])
    expect(result.skipped).toEqual(['qwen/qwen3-32b', 'anthropic/claude-opus-4.6', 'qwen/qwen3-coder'])
  })

  it('keeps everything when no platform carries a family pattern', () => {
    const result = filterUpstreamModelsByPlatform(upstream, ['other'])
    expect(result.filterApplied).toBe(false)
    expect(result.kept).toEqual(upstream)
    expect(result.skipped).toEqual([])
  })

  it('matches any sub-platform for composite accounts', () => {
    const result = filterUpstreamModelsByPlatform(['glm-4.6', 'deepseek-chat', 'gpt-4o'], ['zhipu', 'deepseek'])
    expect(result.filterApplied).toBe(true)
    expect(result.kept).toEqual(['glm-4.6', 'deepseek-chat'])
    expect(result.skipped).toEqual(['gpt-4o'])
  })

  it('normalizes platform ids case-insensitively', () => {
    expect(filterUpstreamModelsByPlatform(['deepseek-chat'], ['DeepSeek']).kept).toEqual(['deepseek-chat'])
  })

  it('returns everything untouched when platform list is empty', () => {
    const result = filterUpstreamModelsByPlatform(upstream, [])
    expect(result.filterApplied).toBe(false)
    expect(result.kept).toEqual(upstream)
  })
})
