<template>
  <div class="plaza-pricing-table overflow-x-auto">
    <table class="w-full min-w-[1080px] table-fixed border-collapse text-sm tabular-nums">
      <colgroup>
        <col class="w-[22%]" />
        <col class="w-[10%]" />
        <col class="w-[11%]" />
        <col class="w-[9%]" />
        <col class="w-[13%]" />
        <col class="w-[11%]" />
        <col class="w-[8%]" />
        <col class="w-[16%]" />
      </colgroup>
      <thead>
        <tr class="text-xs font-semibold uppercase tracking-wider text-gray-500 dark:text-dark-400">
          <th rowspan="2" class="border-r border-gray-100 py-2.5 pl-5 pr-4 text-left align-middle dark:border-dark-700/60">
            {{ t('modelPlaza.table.model') }}
          </th>
          <th rowspan="2" class="border-r border-gray-100 py-2.5 px-3 text-left align-middle dark:border-dark-700/60">
            {{ t('modelPlaza.table.group') }}
          </th>
          <th colspan="3" class="pz-bg pt-2 text-center">
            <div class="pz-title border-b pb-2 font-semibold">
              {{ t('modelPlaza.table.paidPrice') }}
              <span class="pz-unit ml-1 normal-case font-normal">{{ t('modelPlaza.table.unitPerMillion') }}</span>
            </div>
          </th>
          <th colspan="3" class="border-l border-gray-100 pt-2 text-center dark:border-dark-700/60">
            <div class="border-b border-gray-200 pb-2 text-gray-400 dark:border-dark-600 dark:text-dark-500">
              {{ t('modelPlaza.table.officialPrice') }}
              <span class="ml-1 normal-case font-normal text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.unitPerMillion') }}</span>
            </div>
          </th>
          <th rowspan="2" class="border-l border-gray-100 py-2.5 pl-3 pr-5 text-right align-middle dark:border-dark-700/60">
            {{ t('payment.planCard.peakRate') }}
          </th>
        </tr>
        <tr class="border-b border-gray-200 text-left text-[11px] font-medium uppercase leading-4 tracking-wide text-gray-400 dark:border-dark-700 dark:text-dark-500">
          <th class="pz-bg px-3 py-2 font-medium">{{ t('modelPlaza.table.input') }}</th>
          <th class="pz-bg px-3 py-2 font-medium">{{ t('modelPlaza.table.output') }}</th>
          <th class="pz-bg px-3 py-2 font-medium">{{ t('modelPlaza.table.cache') }}</th>
          <th class="border-l border-gray-100 px-3 py-2 font-medium dark:border-dark-700/60">{{ t('modelPlaza.table.input') }}</th>
          <th class="px-3 py-2 font-medium">{{ t('modelPlaza.table.output') }}</th>
          <th class="px-3 py-2 font-medium">{{ t('modelPlaza.table.cache') }}</th>
        </tr>
      </thead>
      <tbody>
        <template v-for="row in props.rows" :key="row.key">
          <tr
            v-for="(entry, eIdx) in row.entries"
            :key="row.key + '-' + entry.groupId"
            class="border-b border-gray-100 transition-colors last:border-b-0 hover:bg-gray-50/70 dark:border-dark-800 dark:hover:bg-dark-800/50"
          >
            <!-- 模型名仅在首行显示，同模型多分组相邻排列 -->
            <td class="border-r border-gray-100 py-2.5 pl-5 pr-4 align-middle dark:border-dark-700/60">
              <div v-if="eIdx === 0" class="flex flex-wrap items-center gap-1.5">
                <span class="font-medium text-gray-900 dark:text-white">{{ row.model.name }}</span>
                <span
                  v-if="props.platform && row.model.platform !== platform"
                  :class="['inline-flex items-center rounded-md px-1.5 py-0.5 text-[10px] font-medium', platformBadgeLightClass(row.model.platform)]"
                >
                  {{ platformLabel(row.model.platform) }}
                </span>
                <span
                  v-if="billingMode(row.model) !== BILLING_MODE_TOKEN"
                  class="rounded-md bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
                >
                  {{ billingModeLabel(row.model) }}
                </span>
                <span
                  v-if="row.model.long_context_basis === 'marginal'"
                  class="rounded-md bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
                  :title="t('modelPlaza.table.tierHintMarginal')"
                >
                  {{ t('modelPlaza.table.marginalBadge') }}
                </span>
              </div>
            </td>

            <!-- 分组（供应商）徽章 -->
            <td class="py-2.5 pr-4 align-middle">
              <GroupBadge
                :name="entry.groupName"
                :platform="(row.model.platform as any)"
                :subscription-type="(entry.subscriptionType as any)"
                hide-rate
                class="max-w-full"
              />
            </td>

            <template v-if="billingMode(row.model) === BILLING_MODE_TOKEN">
              <td class="pz-cell px-3 py-2.5 align-middle font-mono text-xs text-gray-900 dark:text-gray-50">
                <template v-if="tokenIntervals(entry.model).length">
                  <div v-for="(iv, idx) in tokenIntervals(entry.model)" :key="idx" class="whitespace-nowrap leading-5">
                    <span class="mr-1 font-sans font-normal text-gray-400 dark:text-dark-500" :title="tierHint(entry.model)">{{ tierLabel(iv) }}</span>
                    {{ paidPerMillion(iv.input_price, null, entry.rate) }}
                    <span
                      v-if="discountBadge(iv.input_price, entry.model.official_pricing?.input_price)"
                      class="ml-1 inline-flex items-center rounded bg-emerald-100 px-1 py-0.5 text-[10px] font-semibold text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300"
                      :title="t('modelPlaza.table.discountHint')"
                    >{{ discountBadge(iv.input_price, entry.model.official_pricing?.input_price) }}</span>
                  </div>
                </template>
                <template v-else>
                  {{ paidPerMillion(entry.model.pricing?.input_price, null, entry.rate) }}
                  <span
                    v-if="discountBadge(entry.model.pricing?.input_price, entry.model.official_pricing?.input_price)"
                    class="ml-1 inline-flex items-center rounded bg-emerald-100 px-1 py-0.5 text-[10px] font-semibold text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300"
                    :title="t('modelPlaza.table.discountHint')"
                  >{{ discountBadge(entry.model.pricing?.input_price, entry.model.official_pricing?.input_price) }}</span>
                </template>
              </td>
              <td class="pz-cell px-3 py-2.5 align-middle font-mono text-xs text-gray-900 dark:text-gray-50">
                <template v-if="tokenIntervals(entry.model).length">
                  <div v-for="(iv, idx) in tokenIntervals(entry.model)" :key="idx" class="whitespace-nowrap leading-5">
                    {{ paidPerMillion(iv.output_price, null, entry.rate) }}
                    <span
                      v-if="discountBadge(iv.output_price, entry.model.official_pricing?.output_price)"
                      class="ml-1 inline-flex items-center rounded bg-emerald-100 px-1 py-0.5 text-[10px] font-semibold text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300"
                      :title="t('modelPlaza.table.discountHint')"
                    >{{ discountBadge(iv.output_price, entry.model.official_pricing?.output_price) }}</span>
                  </div>
                </template>
                <template v-else>
                  {{ paidPerMillion(entry.model.pricing?.output_price, null, entry.rate) }}
                  <span
                    v-if="discountBadge(entry.model.pricing?.output_price, entry.model.official_pricing?.output_price)"
                    class="ml-1 inline-flex items-center rounded bg-emerald-100 px-1 py-0.5 text-[10px] font-semibold text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300"
                    :title="t('modelPlaza.table.discountHint')"
                  >{{ discountBadge(entry.model.pricing?.output_price, entry.model.official_pricing?.output_price) }}</span>
                </template>
              </td>
              <td class="pz-cell px-3 py-2.5 align-middle font-mono text-xs text-gray-800 dark:text-gray-200">
                <div v-if="hasCachePricing(entry.model)" class="space-y-0.5">
                  <div>
                    <span class="mr-1 font-sans font-normal text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.cacheWriteShort') }}</span>
                    {{ paidPerMillion(entry.model.pricing?.cache_write_price, null, entry.rate) }}
                  </div>
                  <div>
                    <span class="mr-1 font-sans font-normal text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.cacheReadShort') }}</span>
                    {{ paidPerMillion(entry.model.pricing?.cache_read_price, null, entry.rate) }}
                  </div>
                </div>
                <span v-else class="text-gray-400 dark:text-dark-500">-</span>
              </td>
            </template>

            <!-- 按次/按图片计费：实付区整体合并 -->
            <template v-else>
              <td colspan="3" class="pz-cell px-3 py-2.5 align-middle">
                <div v-if="requestIntervals(entry.model).length" class="flex flex-wrap items-center gap-1.5">
                  <span
                    v-for="(iv, idx) in requestIntervals(entry.model)"
                    :key="idx"
                    class="inline-flex items-center gap-1 rounded-md bg-gray-100 px-2 py-0.5 font-mono text-xs text-gray-800 dark:bg-dark-700/60 dark:text-gray-200"
                  >
                    <span class="font-sans text-gray-400 dark:text-dark-500">{{ tierLabel(iv) }}</span>
                    {{ paidRequestPrice(entry.model, iv.per_request_price, entry.rate) }}
                    <span class="font-sans text-gray-400 dark:text-dark-500">{{ perUnitSuffix(entry.model) }}</span>
                  </span>
                </div>
                <template v-else-if="entry.model.pricing?.per_request_price != null">
                  <span class="font-mono font-semibold text-gray-900 dark:text-gray-50">
                    {{ paidRequestPrice(entry.model, entry.model.pricing.per_request_price, entry.rate) }}
                  </span>
                  <span class="ml-1 text-xs text-gray-400 dark:text-dark-500">{{ perUnitSuffix(entry.model) }}</span>
                </template>
                <span v-else class="text-gray-400 dark:text-dark-500">-</span>
              </td>
            </template>

            <!-- 官方价格（同模型各分组同源，取首行非空） -->
            <td class="border-l border-gray-100 px-3 py-2.5 align-middle font-mono text-xs text-gray-500 dark:border-dark-700/60 dark:text-dark-400">
              <template v-if="officialIntervals(entry.model).length">
                <div v-for="(iv, idx) in officialIntervals(entry.model)" :key="idx" class="whitespace-nowrap leading-5" :title="t('modelPlaza.table.tierHint')">
                  <span class="mr-1 font-sans text-gray-400 dark:text-dark-500" :title="t('modelPlaza.table.tierHint')">{{ tierLabel(iv) }}</span>
                  {{ official(iv.input_price) }}
                </div>
              </template>
              <template v-else>{{ official(firstOfficial(row, 'input_price')) }}</template>
            </td>
            <td class="px-3 py-2.5 align-middle font-mono text-xs text-gray-500 dark:border-dark-700/60 dark:text-dark-400">
              <template v-if="officialIntervals(entry.model).length">
                <div v-for="(iv, idx) in officialIntervals(entry.model)" :key="idx" class="whitespace-nowrap leading-5" :title="t('modelPlaza.table.tierHint')">
                  {{ official(iv.output_price) }}
                </div>
              </template>
              <template v-else>{{ official(firstOfficial(row, 'output_price')) }}</template>
            </td>
            <td class="px-3 py-2.5 align-middle font-mono text-xs text-gray-500 dark:text-dark-400">
              <template v-if="officialIntervals(entry.model).length">
                <div v-for="(iv, idx) in officialIntervals(entry.model)" :key="idx" class="whitespace-nowrap leading-5" :title="t('modelPlaza.table.tierHint')">
                  <template v-if="iv.cache_write_price != null || iv.cache_read_price != null">
                    <span class="font-sans text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.cacheWriteShort') }}</span>
                    {{ official(iv.cache_write_price) }}
                    <span class="ml-1 font-sans text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.cacheReadShort') }}</span>
                    {{ official(iv.cache_read_price) }}
                  </template>
                  <span v-else class="text-gray-400 dark:text-dark-500">-</span>
                </div>
              </template>
              <div v-else-if="firstOfficialCache(row)" class="space-y-0.5">
                <div>
                  <span class="mr-1 font-sans text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.cacheWriteShort') }}</span>
                  {{ official(firstOfficialCache(row)!.cache_write_price) }}
                </div>
                <div>
                  <span class="mr-1 font-sans text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.cacheReadShort') }}</span>
                  {{ official(firstOfficialCache(row)!.cache_read_price) }}
                </div>
              </div>
              <span v-else class="text-gray-400 dark:text-dark-500">-</span>
            </td>

            <!-- 高峰倍率提示（仅实际启用的分组显示） -->
            <td class="border-l border-gray-100 px-3 py-2.5 text-right align-middle font-mono text-xs text-gray-500 dark:border-dark-700/60 dark:text-dark-400">
              <span v-if="entry.peakRateText" class="text-amber-700 dark:text-amber-300" :title="entry.peakRateTitle">{{ entry.peakRateText }}</span>
              <span v-else class="text-gray-400 dark:text-dark-500">-</span>
            </td>
          </tr>
        </template>
      </tbody>
    </table>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { PropType } from 'vue'
import { formatScaled } from '@/utils/pricing'
import { platformBadgeLightClass, platformLabel } from '@/utils/platformColors'
import GroupBadge from '@/components/common/GroupBadge.vue'
import {
  BILLING_MODE_TOKEN,
  BILLING_MODE_IMAGE,
  type BillingMode
} from '@/constants/channel'
import type { PlazaModel, PlazaTimePricingPeriod } from '@/api/modelPlaza'
import type { UserPricingInterval } from '@/api/channels'

export interface MergedEntry {
  model: PlazaModel
  groupId: number
  groupName: string
  rate: number
  subscriptionType?: string
  peakRateText?: string
  peakRateTitle?: string
}

export interface MergedRow {
  key: string
  model: PlazaModel
  entries: MergedEntry[]
}

const props = defineProps({
  rows: { type: Array as PropType<MergedRow[]>, required: true },
  platform: { type: String as PropType<string>, default: '' }
})

const { t } = useI18n()
const PER_MILLION = 1e6
const MIN_DECIMALS = 2

function billingMode(m: PlazaModel): BillingMode {
  return (m.pricing?.billing_mode as BillingMode) || BILLING_MODE_TOKEN
}
function billingModeLabel(m: PlazaModel): string {
  return billingMode(m) === BILLING_MODE_IMAGE ? t('modelPlaza.table.perImage') : t('modelPlaza.table.perRequest')
}

function paidPerMillion(value: number | null | undefined, _period: PlazaTimePricingPeriod | null = null, rate = 1): string {
  if (value == null) return '-'
  return formatScaled(value * rate, PER_MILLION, MIN_DECIMALS)
}
function paidRequestPrice(m: PlazaModel, value: number | null | undefined, rate = 1): string {
  if (value == null) return '-'
  void m
  return formatScaled(value * rate, 1, MIN_DECIMALS)
}
function official(value: number | null | undefined): string {
  if (value == null) return '-'
  return formatScaled(value, PER_MILLION, MIN_DECIMALS)
}

function tokenIntervals(m: PlazaModel): UserPricingInterval[] {
  return sortByContext(m.pricing?.intervals ?? [])
}
function requestIntervals(m: PlazaModel): UserPricingInterval[] {
  return sortByContext((m.pricing?.intervals ?? []).filter((iv) => iv.per_request_price != null))
}
function officialIntervals(m: PlazaModel): UserPricingInterval[] {
  return sortByContext(m.official_pricing?.intervals ?? [])
}
function sortByContext(list: UserPricingInterval[]): UserPricingInterval[] {
  return [...list].sort((a, b) => (a.min_tokens ?? 0) - (b.min_tokens ?? 0))
}
function tierLabel(iv: UserPricingInterval): string {
  if (iv.tier_label) return iv.tier_label
  const max = iv.max_tokens ?? null
  return max == null ? `>${(iv.min_tokens ?? 0) / 1000}K` : `${((iv.min_tokens ?? 0) + 1) / 1000}K~${max / 1000}K`
}
function tierHint(m: PlazaModel): string {
  return m.long_context_basis === 'marginal'
    ? t('modelPlaza.table.tierHintMarginal')
    : t('modelPlaza.table.tierHint')
}
function hasCachePricing(m: PlazaModel): boolean {
  return m.pricing?.cache_write_price != null || m.pricing?.cache_read_price != null
}
function firstOfficial(row: MergedRow, field: 'input_price' | 'output_price'): number | null | undefined {
  for (const e of row.entries) {
    const v = e.model.official_pricing?.[field]
    if (v != null) return v
  }
  return null
}
function firstOfficialCache(row: MergedRow) {
  for (const e of row.entries) {
    const op = e.model.official_pricing
    if (op && (op.cache_write_price != null || op.cache_read_price != null)) return op
  }
  return null
}
function perUnitSuffix(m: PlazaModel): string {
  return billingMode(m) === BILLING_MODE_IMAGE ? t('modelPlaza.table.perUnitImage') : t('modelPlaza.table.perUnitRequest')
}
function discountBadge(
  paid: number | null | undefined,
  official: number | null | undefined
): string {
  const off = official == null ? 0 : official * 1e6
  if (off <= 0) return ''
  const paidNum = paid == null ? 0 : paid * 1e6
  if (paidNum <= 0) return ''
  const zhe = (paidNum / off) * 10
  return `${zhe.toFixed(zhe >= 1 ? 1 : 2).replace(/\.?0+$/, '')}折`
}
</script>
