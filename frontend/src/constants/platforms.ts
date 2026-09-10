import type { AccountPlatform, GroupPlatform } from '@/types'

export interface PlatformOption<T extends string = string> {
  value: T
  label: string
}

/**
 * Concrete upstream platforms supported by accounts and request routing.
 * Keep platform selectors derived from this catalog so newly added providers
 * do not silently disappear from list filters.
 */
export const CONCRETE_PLATFORM_OPTIONS = [
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'openai', label: 'OpenAI' },
  { value: 'gemini', label: 'Gemini' },
  { value: 'antigravity', label: 'Antigravity' },
  { value: 'grok', label: 'Grok' },
  { value: 'kimi', label: 'Kimi' },
  { value: 'zhipu', label: 'Zhipu GLM' },
  { value: 'deepseek', label: 'DeepSeek' },
  { value: 'other', label: 'Other' },
  { value: 'minimax', label: 'MiniMax' }
] as const satisfies readonly PlatformOption<AccountPlatform>[]

/** Platforms that can own a group. */
export const GROUP_PLATFORM_OPTIONS = [
  ...CONCRETE_PLATFORM_OPTIONS,
  { value: 'composite', label: 'Composite' }
] as const satisfies readonly PlatformOption<GroupPlatform>[]

/**
 * Composite 分组可作为转发目标的具体平台。other（通用 OpenAI 兼容自定义上游）
 * 刻意不承担 composite 目标：后端 target_platform 校验与 DB CHECK 均不含 other。
 */
export const COMPOSITE_TARGET_PLATFORM_OPTIONS = CONCRETE_PLATFORM_OPTIONS.filter(
  (p) => p.value !== 'other',
)

/**
 * 模型广场展示排序：claude → gpt → kimi → glm → deepseek，其余平台排在其后。
 */
export const PLAZA_PLATFORM_ORDER = [
  'anthropic',
  'openai',
  'kimi',
  'zhipu',
  'deepseek',
  'gemini',
  'antigravity',
  'grok',
  'other',
  'composite',
] as const

export function plazaPlatformOrder(platform: string): number {
  const idx = (PLAZA_PLATFORM_ORDER as readonly string[]).indexOf(platform)
  return idx === -1 ? PLAZA_PLATFORM_ORDER.length : idx
}

/**
 * 平台 → 上游模型 ID 词根匹配表。「同步上游支持的模型」在自动加入白名单前用它过滤
 * 聚合上游混回的跨平台无关模型（如 deepseek 账号的上游返回 qwen/claude 模型）。
 * 模型 ID 常带厂商前缀（qwen/qwen3-32b、deepseek/deepseek-chat），词根命中前缀或
 * 主体都算平台相关，因此按子串匹配；openai 的 o1/o3/o4 需词边界避免误伤同类命名。
 */
const PLATFORM_MODEL_PATTERNS: Record<string, RegExp> = {
  anthropic: /claude/i,
  openai: /gpt|chatgpt|codex|text-embedding|whisper|dall-e|davinci|(?:^|[^a-z0-9])o[134](?:[^0-9]|$)/i,
  gemini: /gemini/i,
  // antigravity 走 Gemini 协议，上游模型即 gemini 系列
  antigravity: /gemini/i,
  grok: /grok/i,
  kimi: /kimi|moonshot/i,
  zhipu: /glm|zhipu|chatglm|bigmodel/i,
  deepseek: /deepseek/i,
  minimax: /minimax|abab/i,
}

/** 判断上游模型 ID 是否属于给定平台的模型族。平台无词根定义（other/未知）时视为匹配。 */
export function modelMatchesPlatform(modelId: string, platform: string): boolean {
  const pattern = PLATFORM_MODEL_PATTERNS[platform.trim().toLowerCase()]
  if (!pattern) return true
  return pattern.test(modelId)
}

export interface UpstreamModelSyncFilter {
  kept: string[]
  skipped: string[]
  /** false 表示账号平台无词根定义（other/未知/未填），未做过滤 */
  filterApplied: boolean
}

/**
 * 按账号平台过滤上游同步模型列表：保留命中任一平台词根的模型（composite 多平台
 * 场景下任一子平台命中即保留），其余进 skipped 供提示文案展示。词根未覆盖的模型
 * 仍应由调用方保留在下拉中供手动添加，因此这里只做分组、不做丢弃。
 */
export function filterUpstreamModelsByPlatform(models: string[], platforms: string[]): UpstreamModelSyncFilter {
  const patterned = Array.from(
    new Set(platforms.map((platform) => platform.trim().toLowerCase()).filter(Boolean))
  ).filter((platform) => platform in PLATFORM_MODEL_PATTERNS)
  if (patterned.length === 0) {
    return { kept: models, skipped: [], filterApplied: false }
  }
  const kept: string[] = []
  const skipped: string[] = []
  for (const model of models) {
    const bucket = patterned.some((platform) => modelMatchesPlatform(model, platform)) ? kept : skipped
    bucket.push(model)
  }
  return { kept, skipped, filterApplied: true }
}
