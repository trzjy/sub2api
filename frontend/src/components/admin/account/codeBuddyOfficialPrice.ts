/**
 * CodeBuddy 影子向导「官方价」列的数据源（方案 N2）。
 *
 * 走既有定价中心目录接口 `GET /admin/pricing/catalog?search=<模型名>`，与计费口径同源：
 * 目录本身就按 custom（定价中心自定义/覆盖层）> litellm（远程官方价表）> fallback（内置兜底）
 * 分层，同一模型名只会命中其中一层，因此「定价中心配过价」与「官方价表有价」都算已定价。
 *
 * 约束（含评审裁决）：
 *   - 缓存按模型名放模块级，会话内只查一次：向导反复开关不重复打接口；
 *   - 并发上限 4，单条失败只让该模型显示「未定价」，不冒泡错误态、不阻塞表格渲染；
 *   - 失败结论不落终局缓存，下次打开向导重试（避免一次网络抖动被固化成「没有官方价」）。
 * 目录接口 page_size 被后端钳到 ≤200，没有一次性拿全量的批量端点，故按模型逐个精确查询。
 */
import { reactive } from 'vue'
import { getCatalog } from '@/api/admin/pricing'
import type { CatalogEntry } from '@/api/admin/pricing'

export type OfficialPriceState =
  | { status: 'pending' }
  | { status: 'priced'; label: string }
  | { status: 'unpriced' }

/** 目录分层优先级：同名多来源命中时取层号最小者（自定义/覆盖层优先）。 */
const SOURCE_RANK: Record<string, number> = {
  custom: 0,
  litellm: 1,
  fallback: 2,
  fuzzy: 3,
  none: 4,
}

export const OFFICIAL_PRICE_CONCURRENCY = 4

const priceCache = reactive<Record<string, OfficialPriceState>>({})
/** 上次查询失败的模型：不作为终局结论，下次打开向导重试。 */
const retryable = new Set<string>()

export function officialPriceState(model: string): OfficialPriceState {
  return priceCache[model] ?? { status: 'pending' }
}

export function officialPriceLabel(model: string): string {
  const st = priceCache[model]
  return st && st.status === 'priced' ? st.label : ''
}

function formatPerMtok(v: number): string {
  if (!Number.isFinite(v) || v <= 0) return '0'
  return (v >= 1 ? v.toFixed(2) : v.toFixed(3)).replace(/\.?0+$/, '') || '0'
}

/**
 * 从目录搜索结果中取该模型**精确**条目的价。
 * 无精确命中 / 命中层为 none / 该条目没有 token 价 → null（未定价 → 去配置）。
 */
export function pickOfficialPrice(items: CatalogEntry[], model: string): string | null {
  const target = model.trim().toLowerCase()
  const exact = items.filter((it) => String(it.model).trim().toLowerCase() === target)
  if (exact.length === 0) return null
  // 同名可能同时出现在多层（custom 层键名小写、远程表保留原大小写），取层号最小者。
  const entry = exact.reduce((best, it) =>
    (SOURCE_RANK[it.source] ?? 99) < (SOURCE_RANK[best.source] ?? 99) ? it : best,
  )
  if (entry.source === 'none' || entry.token_pricing_absent) return null
  const input = Number(entry.input_per_mtok)
  const output = Number(entry.output_per_mtok)
  if (!Number.isFinite(input) && !Number.isFinite(output)) return null
  return `$${formatPerMtok(input)} / $${formatPerMtok(output)}`
}

/** 并发受限的 map（几十个模型逐个查目录时避免一次性打满）。 */
async function mapLimit<T>(items: T[], limit: number, worker: (item: T) => Promise<void>): Promise<void> {
  let cursor = 0
  const runners = Array.from({ length: Math.min(limit, items.length) }, async () => {
    while (cursor < items.length) {
      const index = cursor++
      await worker(items[index]!)
    }
  })
  await Promise.all(runners)
}

/** 按模型名查目录价；已查过且非失败的模型直接跳过（会话内只查一次）。 */
export async function loadOfficialPrices(models: string[]): Promise<void> {
  const targets = Array.from(new Set(models.map((m) => m.trim()))).filter(
    (m) => m !== '' && (priceCache[m] === undefined || retryable.has(m)),
  )
  if (targets.length === 0) return
  for (const m of targets) priceCache[m] = { status: 'pending' }
  await mapLimit(targets, OFFICIAL_PRICE_CONCURRENCY, async (model) => {
    try {
      const res = await getCatalog({ search: model, page_size: 200 })
      const label = pickOfficialPrice(res?.items ?? [], model)
      retryable.delete(model)
      priceCache[model] = label ? { status: 'priced', label } : { status: 'unpriced' }
    } catch {
      // 单条失败：只把该模型退回「未定价」展示（仍可点「去配置」），不写终局缓存。
      retryable.add(model)
      priceCache[model] = { status: 'unpriced' }
    }
  })
}

/** 仅供测试：清空模块级缓存，保证用例之间的隔离。 */
export function resetCodeBuddyOfficialPriceCache(): void {
  for (const key of Object.keys(priceCache)) delete priceCache[key]
  retryable.clear()
}
