<template>
  <Teleport to="body">
    <div v-if="show && parent" class="fixed inset-0 z-[10000] flex items-center justify-center p-4">
      <div class="absolute inset-0 bg-black/40" @click="$emit('close')" />
      <div
        class="relative flex max-h-[88vh] w-full max-w-6xl flex-col overflow-hidden rounded-xl bg-white shadow-2xl ring-1 ring-black/5 dark:bg-dark-800"
      >
        <!-- Header -->
        <div class="flex items-center justify-between border-b border-gray-200 px-5 py-3 dark:border-dark-700">
          <div>
            <h3 class="text-base font-semibold text-gray-900 dark:text-white">
              {{ t('admin.accounts.codeBuddyWizardTitle') }}
              <!-- 方案 N3：母账号站点徽标（影子继承母账号站点） -->
              <span
                data-test="codebuddy-wizard-site-badge"
                class="ml-2 inline-block rounded border px-1.5 py-0.5 align-middle text-[10px] font-medium"
                :class="codeBuddySiteBadgeClass(site)"
                :title="t('admin.accounts.codeBuddySiteBadgeTitle', { site: siteLabel })"
              >{{ siteLabel }}</span>
            </h3>
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.codeBuddyWizardOpen', { name: parent.name }) }}</p>
            <!-- 方案 §0 N1 动作 B-1：显式展示母账号的代理绑定（影子继承同一代理）。
                 注意：**不**在此建议 intl 母账号绑 HK 出口代理 —— PR-V2a 生产实测
                 （直连 HK / 经 US 代理 / 本机 SG 三出口）证明 intl models 端点的 500
                 与出口 IP 无关（代理不可解），写"绑代理可解"属误导。 -->
            <div class="mt-1 flex flex-wrap items-center gap-1.5 text-xs">
              <span class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.codeBuddyProxyLabel') }}</span>
              <template v-if="parent.proxy">
                <span
                  data-test="codebuddy-parent-proxy"
                  class="inline-block rounded bg-cyan-100 px-1.5 py-0.5 text-[10px] font-medium text-cyan-700 dark:bg-cyan-900/30 dark:text-cyan-300"
                >{{ parent.proxy.name }}</span>
                <span class="text-gray-400">{{ proxySummary }}</span>
              </template>
              <span v-else data-test="codebuddy-parent-proxy-none" class="text-gray-400">
                {{ t('admin.accounts.codeBuddyProxyNone') }}
              </span>
            </div>
          </div>
            <button class="rounded p-1 text-gray-400 hover:bg-gray-100 dark:hover:bg-dark-700" @click="$emit('close')">
            <Icon name="x" size="md" />
          </button>
        </div>

        <!-- Body -->
        <div class="flex-1 overflow-y-auto px-5 py-4">
          <!-- 同步/创建失败提示：独立于状态分支，引导态同样可见。
               （此前该提示只渲染在表格分支内，同步失败时用户看不到任何原因。） -->
          <div
            v-if="syncError"
            class="mb-3 rounded-lg border border-red-300 bg-red-50 px-3 py-2 text-xs text-red-700 dark:border-red-700/50 dark:bg-red-900/20 dark:text-red-300"
          >
            {{ syncError }}
          </div>

          <!-- intl 不可用 -->
          <div
            v-if="isIntl"
            class="rounded-lg border border-amber-300 bg-amber-50 px-4 py-3 text-sm text-amber-800 dark:border-amber-700/50 dark:bg-amber-900/20 dark:text-amber-300"
          >
            {{ t('admin.accounts.codeBuddyIntlUnavailable') }}
          </div>

          <!-- 未同步态：引导 -->
          <div
            v-else-if="state === 'guide' && !syncing"
            class="flex flex-col items-center gap-4 py-12 text-center"
          >
            <Icon name="cube" size="xl" class="text-cyan-500" />
            <p class="max-w-md text-sm text-gray-600 dark:text-gray-300">
              {{ t('admin.accounts.codeBuddyUnsyncedGuide') }}
            </p>
            <button
              class="rounded-lg bg-cyan-600 px-4 py-2 text-sm font-medium text-white hover:bg-cyan-700 disabled:opacity-60"
              :disabled="syncing"
              @click="onSync"
            >
              {{ t('admin.accounts.codeBuddySyncModels') }}
            </button>
          </div>

          <!-- 同步中 -->
          <div v-else-if="syncing" class="flex flex-col items-center gap-3 py-12 text-center">
            <Icon name="refresh" size="xl" class="animate-spin text-cyan-500" />
            <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.accounts.codeBuddySyncing') }}</p>
          </div>

          <!-- 清单表格 -->
          <div v-else-if="state === 'table'">
            <div class="mb-3 flex flex-wrap items-center gap-2">
              <button
                class="rounded border border-gray-300 px-2 py-1 text-xs text-gray-600 hover:bg-gray-100 dark:border-dark-600 dark:text-gray-300 dark:hover:bg-dark-700"
                @click="selectAll"
              >
                {{ t('admin.accounts.codeBuddySelectAll') }}
              </button>
              <button
                class="rounded border border-gray-300 px-2 py-1 text-xs text-gray-600 hover:bg-gray-100 dark:border-dark-600 dark:text-gray-300 dark:hover:bg-dark-700"
                @click="deselectAll"
              >
                {{ t('admin.accounts.codeBuddyDeselectAll') }}
              </button>
              <span class="text-xs text-gray-400">
                {{ selectedCount }} / {{ rows.length }} {{ t('admin.accounts.codeBuddyColStatus') }}
              </span>
              <button
                data-test="codebuddy-create"
                class="ml-auto rounded-lg bg-cyan-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-cyan-700 disabled:opacity-60"
                :disabled="creating || validCombos.length === 0"
                @click="onCreate"
              >
                <Icon v-if="creating" name="refresh" size="sm" class="mr-1 inline animate-spin" />
                {{ t('admin.accounts.codeBuddyBatchCreate') }}
                <template v-if="validCombos.length > 0"> ({{ validCombos.length }})</template>
              </button>
            </div>

            <div class="overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-700">
              <table class="min-w-full text-left text-sm">
                <thead class="bg-gray-50 text-xs uppercase text-gray-500 dark:bg-dark-900/40 dark:text-gray-400">
                  <tr>
                    <th class="w-10 px-3 py-2"></th>
                    <th class="px-3 py-2">{{ t('admin.accounts.codeBuddyColModel') }}</th>
                    <th class="px-3 py-2">{{ t('admin.accounts.codeBuddyColContext') }}</th>
                    <th class="px-3 py-2">{{ t('admin.accounts.codeBuddyColMaxOutput') }}</th>
                    <th class="px-3 py-2">{{ t('admin.accounts.codeBuddyColPrice') }}</th>
                    <th class="px-3 py-2">{{ t('admin.accounts.codeBuddyColPlatform') }}</th>
                    <th class="px-3 py-2">{{ t('admin.accounts.codeBuddyColGroup') }}</th>
                    <th class="px-3 py-2 w-20">{{ t('admin.accounts.codeBuddyColPriority') }}</th>
                    <th class="px-3 py-2">{{ t('admin.accounts.codeBuddyColStatus') }}</th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                  <tr v-for="row in rows" :key="row.model">
                    <!-- v2 一模型可多影子：即便该模型已有影子，仍需可勾选为其它分组建号 -->
                    <td class="px-3 py-2 align-top">
                      <input
                        type="checkbox"
                        :data-test="`codebuddy-select-${row.model}`"
                        :checked="selected.has(row.model)"
                        @change="toggleSelect(row.model, ($event.target as HTMLInputElement).checked)"
                      />
                    </td>
                    <td class="px-3 py-2 align-top font-medium text-gray-900 dark:text-white">{{ row.model }}</td>
                    <td class="px-3 py-2 align-top text-gray-500 dark:text-gray-400">{{ formatTokens(row.contextWindow) }}</td>
                    <td class="px-3 py-2 align-top text-gray-500 dark:text-gray-400">{{ formatTokens(row.maxOutput) }}</td>
                    <td class="px-3 py-2 align-top">
                      <span v-if="officialPriceState(row.model).status === 'pending'" class="text-gray-400">
                        {{ t('admin.accounts.codeBuddyPriceLoading') }}
                      </span>
                      <span
                        v-else-if="officialPriceState(row.model).status === 'priced'"
                        class="text-gray-700 dark:text-gray-200"
                        :title="t('admin.accounts.codeBuddyPriceTooltip')"
                      >{{ officialPriceLabel(row.model) }}</span>
                      <span v-else class="text-gray-400">{{ t('admin.accounts.codeBuddyPriceUnpriced') }}
                        <button class="ml-1 text-cyan-600 underline" @click="$emit('configure-price', row.model)">{{ t('admin.accounts.codeBuddyConfigurePrice') }}</button>
                      </span>
                    </td>
                    <td class="px-3 py-2 align-top">
                      <select
                        v-model="row.platform"
                        class="rounded border border-gray-300 bg-white px-1.5 py-1 text-xs dark:border-dark-600 dark:bg-dark-800"
                      >
                        <option v-for="opt in targetPlatformOptions" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
                      </select>
                    </td>
                    <!-- N4：分组 chips 多选 + 「+」添加按钮（选择器复用 GroupSelector） -->
                    <td class="px-3 py-2 align-top">
                      <div class="flex flex-wrap items-center gap-1">
                        <span
                          v-for="gid in row.groupIds"
                          :key="gid"
                          class="inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-[10px] font-medium"
                          :class="platformBadgeLightClass(row.platform)"
                        >
                          {{ groupNameById(gid) }}
                          <button
                            type="button"
                            class="text-gray-500 hover:text-red-500"
                            :title="t('admin.accounts.codeBuddyRemoveGroup', { name: groupNameById(gid) })"
                            @click="removeGroup(row, gid)"
                          >×</button>
                        </span>
                        <span v-if="row.groupIds.length === 0" class="text-xs text-gray-400">
                          {{ t('admin.accounts.codeBuddyGroupUnselected') }}
                        </span>
                        <button
                          type="button"
                          :data-test="`codebuddy-group-picker-${row.model}`"
                          class="rounded border border-gray-300 px-1.5 py-0.5 text-[10px] leading-4 text-gray-600 hover:bg-gray-100 dark:border-dark-600 dark:text-gray-300 dark:hover:bg-dark-700"
                          :class="pickerModel === row.model ? 'border-red-400 text-red-500' : ''"
                          :title="pickerModel === row.model ? t('admin.accounts.codeBuddyHideGroupPicker') : t('admin.accounts.codeBuddyAddGroup')"
                          @click="togglePicker(row.model)"
                        >{{ pickerModel === row.model ? '−' : '+' }}</button>
                      </div>
                      <div v-if="pickerModel === row.model" class="mt-1 min-w-[280px]" :data-test="`codebuddy-group-selector-${row.model}`">
                        <GroupSelector
                          v-model="row.groupIds"
                          :groups="activeGroups"
                          :platform="row.platform as GroupPlatform"
                        />
                      </div>
                    </td>
                    <td class="px-3 py-2 align-top">
                      <input
                        v-model.number="row.priority"
                        type="number" step="1"
                        class="w-16 rounded border border-gray-300 bg-white px-1.5 py-1 text-xs dark:border-dark-600 dark:bg-dark-800"
                      />
                    </td>
                    <td class="px-3 py-2 align-top">
                      <div class="flex flex-col gap-1">
                        <span
                          v-if="rowCoveredCount(row) > 0 && pendingGroupIds(row).length === 0"
                          class="w-fit rounded bg-emerald-100 px-1.5 py-0.5 text-[10px] font-medium text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300"
                        >
                          {{ t('admin.accounts.codeBuddyStatusCreated') }}
                        </span>
                        <span
                          v-else-if="rowCoveredCount(row) > 0"
                          class="w-fit rounded bg-amber-100 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
                        >
                          {{ t('admin.accounts.codeBuddyStatusPartial', { done: rowCoveredCount(row), total: row.groupIds.length }) }}
                        </span>
                        <!-- 该模型已有影子（未选分组时也要如实显示「已创建」，不能报「未创建」） -->
                        <span
                          v-else-if="rowHasAnyShadow(row)"
                          class="w-fit rounded bg-emerald-100 px-1.5 py-0.5 text-[10px] font-medium text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300"
                        >
                          {{ t('admin.accounts.codeBuddyStatusCreated') }}
                        </span>
                        <span v-else class="text-gray-400">{{ t('admin.accounts.codeBuddyStatusNotCreated') }}</span>
                        <!-- N3：站点徽标 + 存量影子改名提示 -->
                        <span
                          v-if="row.site"
                          data-test="codebuddy-row-site-badge"
                          class="w-fit rounded border px-1.5 py-0.5 text-[10px] font-medium"
                          :class="codeBuddySiteBadgeClass(row.site)"
                        >{{ row.site === 'intl' ? t('admin.accounts.codeBuddySiteIntl') : t('admin.accounts.codeBuddySiteCn') }}</span>
                        <span
                          v-if="row.legacyName"
                          data-test="codebuddy-row-legacy-name"
                          class="w-fit cursor-help text-[10px] text-amber-600 dark:text-amber-400"
                          :title="t('admin.accounts.codeBuddyLegacyNameHint')"
                        >{{ t('admin.accounts.codeBuddyLegacyName') }}</span>
                        <button
                          v-if="row.shadowId != null"
                          class="w-fit text-cyan-600 underline"
                          @click="$emit('jump-parent')"
                        >{{ t('admin.accounts.codeBuddyJumpParent') }}</button>
                      </div>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>

            <!-- 创建结果：按 (模型, 分组) 二元组展示 -->
            <div v-if="createResults" class="mt-3 rounded-lg border px-3 py-2 text-sm"
              :class="createResults.failed.length === 0 ? 'border-emerald-300 bg-emerald-50 text-emerald-700 dark:border-emerald-700/50 dark:bg-emerald-900/20 dark:text-emerald-300' : 'border-amber-300 bg-amber-50 text-amber-800 dark:border-amber-700/50 dark:bg-amber-900/20 dark:text-amber-300'">
              <template v-if="createResults.failed.length === 0">{{ t('admin.accounts.codeBuddyBatchCreateSuccess', { count: createResults.success.length }) }}</template>
              <template v-else>{{ t('admin.accounts.codeBuddyBatchCreatePartial', { success: createResults.success.length, failed: createResults.failed.length }) }}</template>
              <ul v-if="createResults.failed.length > 0" class="mt-1 list-inside list-disc text-xs">
                <li v-for="f in createResults.failed" :key="`${f.model}::${f.groupId}`" :data-test="`codebuddy-create-failed-${f.model}-${f.groupId}`">
                  {{ t('admin.accounts.codeBuddyCreateFailedItem', { model: f.model, group: f.groupName, error: f.error }) }}
                </li>
              </ul>
            </div>
          </div>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Icon } from '@/components/icons'
import GroupSelector from '@/components/common/GroupSelector.vue'
import type { Account, AdminGroup, AccountListItem, GroupPlatform } from '@/types'
import { syncUpstreamModels, createCodeBuddyShadow } from '@/api/admin/accounts'
import type { UpstreamModelMetadata } from '@/api/admin/accounts'
import {
  loadOfficialPrices,
  officialPriceLabel,
  officialPriceState,
} from './codeBuddyOfficialPrice'
import {
  CONCRETE_PLATFORM_OPTIONS,
  codeBuddyShadowName,
  inferCodeBuddyShadowPlatform,
  normalizeCodeBuddySite,
  parseCodeBuddyShadowSite,
} from '@/constants/platforms'
import { codeBuddySiteBadgeClass, platformBadgeLightClass } from '@/utils/platformColors'

// 影子允许落入的目标分组平台（排除 openai/codebuddy 等，与后端 CreateShadow 守卫一致）。
const TARGET_PLATFORMS = ['deepseek', 'zhipu', 'kimi', 'minimax', 'other']
const targetPlatformOptions = CONCRETE_PLATFORM_OPTIONS.filter((p) => TARGET_PLATFORMS.includes(p.value))

export interface CodeBuddyShadowWizardRow {
  model: string
  contextWindow: number
  maxOutput: number
  platform: string
  /** 用户在本行勾选的目标分组（提交时按 (模型 × 分组) 展开） */
  groupIds: number[]
  priority: number
  /** 既有影子已覆盖的分组（来自 props.shadows[].group_ids），用于 (模型, 分组) 去重 */
  existingGroupIds: number[]
  /** 本次会话已成功创建的分组 */
  createdGroupIds: number[]
  /** 该模型已存在影子但列表未返回 group_ids —— 回落模型级去重（保守跳过） */
  modelLevelCovered: boolean
  /** 该模型既有影子名不含 site 段（存量数据，N3 建议改名） */
  legacyName: boolean
  /** 该模型既有影子代表的站点；无既有影子或无法判定时为 null（不展示徽标） */
  site: 'cn' | 'intl' | null
  shadowId: number | null
}

const props = defineProps<{
  show: boolean
  parent: Account | null
  shadows: AccountListItem[]
  groups: AdminGroup[]
}>()

const emit = defineEmits<{
  close: []
  created: []
  'configure-price': [model: string]
  'jump-parent': []
}>()

const { t } = useI18n()

const state = ref<'guide' | 'table'>('guide')
const syncing = ref(false)
const creating = ref(false)
const syncError = ref('')
const rows = ref<CodeBuddyShadowWizardRow[]>([])
const selected = ref<Set<string>>(new Set())
/** 当前展开分组选择器的行（模型名）；null 表示无展开行 */
const pickerModel = ref<string | null>(null)
const createResults = ref<{
  success: { model: string; groupId: number; groupName: string }[]
  failed: { model: string; groupId: number; groupName: string; error: string }[]
} | null>(null)

const isIntl = computed(() => props.parent?.credentials?.site === 'intl')
const selectedCount = computed(() => selected.value.size)
/** 母账号站点（缺省 cn）——影子继承母账号站点，故向导级徽标取此值。 */
const site = computed(() => normalizeCodeBuddySite(props.parent?.credentials?.site))
const siteLabel = computed(() =>
  site.value === 'intl' ? t('admin.accounts.codeBuddySiteIntl') : t('admin.accounts.codeBuddySiteCn')
)

/** 母账号代理绑定摘要（影子继承母账号代理，故向导级展示即代表影子出站口径）。 */
const proxySummary = computed(() => {
  const p = props.parent?.proxy
  if (!p) return ''
  const country = p.country_code ? ` (${p.country_code})` : ''
  return `${p.host}:${p.port}${country}`
})

/** 传给 GroupSelector 的分组全集：仅做「启用」过滤，平台过滤语义一律交给 GroupSelector 自身。 */
const activeGroups = computed(() => props.groups.filter((g) => g.status === 'active'))

const groupNameById = (id: number): string =>
  props.groups.find((g) => g.id === id)?.name ?? String(id)

function formatTokens(n: number): string {
  if (!n || n <= 0) return '-'
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(n % 1_000_000 === 0 ? 0 : 1) + 'M'
  if (n >= 1000) return (n / 1000).toFixed(n % 1000 === 0 ? 0 : 1) + 'K'
  return String(n)
}

// ===== 官方价（N2）：见 ./codeBuddyOfficialPrice —— 模块级缓存 + 并发 4 + 与计费目录同源 =====
// 只在表格态（同步成功后）触发；引导态/组件挂载时不打接口。

// ===== 行构建与 (模型, 分组) 去重 =====
function buildRows(metadata: Record<string, UpstreamModelMetadata> | undefined, modelIds: string[]) {
  const result: CodeBuddyShadowWizardRow[] = []
  const seen = new Set<string>()
  const ids = metadata && Object.keys(metadata).length > 0 ? Object.keys(metadata) : modelIds
  for (const id of ids) {
    const key = id.trim()
    if (!key || seen.has(key)) continue
    seen.add(key)
    const meta: UpstreamModelMetadata | undefined = metadata?.[key]
    const modelShadows = props.shadows.filter((s) => s.extra?.shadow_model === key)
    // group_ids 用 omitempty 序列化：后端返回该键即代表真实分组（codebuddy 影子必有分组），
    // 键整体缺失说明列表未带分组信息，此时回落到模型级去重避免重复建号。
    const hasGroupInfo = modelShadows.some((s) => Array.isArray(s.group_ids))
    const existingGroupIds = hasGroupInfo
      ? Array.from(new Set(modelShadows.flatMap((s) => s.group_ids ?? [])))
      : []
    const shadowSites = modelShadows
      .map((s) => parseCodeBuddyShadowSite(s.name))
      .filter((v): v is 'cn' | 'intl' => v !== null)
    result.push({
      model: key,
      contextWindow: meta?.context_window ?? 0,
      maxOutput: meta?.max_output_tokens ?? 0,
      platform: inferCodeBuddyShadowPlatform(key),
      groupIds: [],
      priority: props.parent?.priority ?? 0,
      existingGroupIds,
      createdGroupIds: [],
      modelLevelCovered: modelShadows.length > 0 && !hasGroupInfo,
      legacyName: modelShadows.length > 0 && modelShadows.some((s) => parseCodeBuddyShadowSite(s.name) === null),
      site: shadowSites[0] ?? null,
      shadowId: modelShadows[0]?.id ?? null,
    })
  }
  result.sort((a, b) => a.model.localeCompare(b.model))
  return result
}

/** 该行是否还有未创建的分组（已覆盖的不再重复提交）。 */
function pendingGroupIds(row: CodeBuddyShadowWizardRow): number[] {
  if (row.modelLevelCovered) return []
  const covered = new Set([...row.existingGroupIds, ...row.createdGroupIds])
  return row.groupIds.filter((id) => !covered.has(id))
}

/** 该行已覆盖的分组数（模型级兜底时按已选数计，避免误报「未创建」）。 */
function rowCoveredCount(row: CodeBuddyShadowWizardRow): number {
  if (row.modelLevelCovered) return row.groupIds.length
  const covered = new Set([...row.existingGroupIds, ...row.createdGroupIds])
  return row.groupIds.filter((id) => covered.has(id)).length
}

/** 该模型是否已存在任一影子（既有或本次新建），用于未选分组时的状态展示。 */
function rowHasAnyShadow(row: CodeBuddyShadowWizardRow): boolean {
  return (
    row.modelLevelCovered ||
    row.shadowId != null ||
    row.existingGroupIds.length > 0 ||
    row.createdGroupIds.length > 0
  )
}

/** 行内是否已无可建组合（用于「全选」跳过与行状态图标）。 */
const rowFullyCovered = (row: CodeBuddyShadowWizardRow): boolean =>
  row.groupIds.length > 0 && pendingGroupIds(row).length === 0

/**
 * 是否需要在影子名后追加分组短名做消歧（方案 N4）。
 * 需要的情况：本行一次绑多个分组，或该模型已有影子（否则先建 `<母>:<site>:<模型>`、
 * 之后为同模型另一分组再建时会生成同名影子）。
 */
function needsGroupSuffix(row: CodeBuddyShadowWizardRow): boolean {
  return row.groupIds.length > 1 || row.existingGroupIds.length > 0 || row.createdGroupIds.length > 0
}

// ===== 交互 =====
function togglePicker(model: string) {
  pickerModel.value = pickerModel.value === model ? null : model
}

function removeGroup(row: CodeBuddyShadowWizardRow, groupId: number) {
  row.groupIds = row.groupIds.filter((id) => id !== groupId)
}

function toggleSelect(model: string, checked: boolean) {
  const next = new Set(selected.value)
  if (checked) next.add(model)
  else next.delete(model)
  selected.value = next
}

function selectAll() {
  selected.value = new Set(rows.value.filter((r) => !rowFullyCovered(r)).map((r) => r.model))
}

function deselectAll() {
  selected.value = new Set()
}

/** 提交展开：(模型 × 分组) 二元组，各自独立优先级（N4 不做「一账号多组」新轨道）。 */
const validCombos = computed(() =>
  rows.value
    .filter((r) => selected.value.has(r.model))
    .flatMap((r) =>
      pendingGroupIds(r).map((groupId) => ({
        model: r.model,
        groupId,
        groupName: groupNameById(groupId),
        platform: r.platform,
        priority: r.priority,
        suffix: needsGroupSuffix(r),
      }))
    )
)

async function onSync() {
  if (!props.parent) return
  syncing.value = true
  syncError.value = ''
  createResults.value = null
  pickerModel.value = null
  try {
    const res = await syncUpstreamModels(props.parent.id)
    if (!res || !res.models || res.models.length === 0) {
      syncError.value = t('admin.accounts.codeBuddySyncFailed', { error: 'no models returned' })
      state.value = 'guide'
      return
    }
    rows.value = buildRows(res.metadata, res.models)
    state.value = 'table'
    syncError.value = res.warnings?.length ? res.warnings.map((w) => w.message).join('; ') : ''
    void loadOfficialPrices(rows.value.map((r) => r.model))
  } catch (err) {
    syncError.value = t('admin.accounts.codeBuddySyncFailed', { error: err instanceof Error ? err.message : String(err) })
    state.value = 'guide'
  } finally {
    syncing.value = false
  }
}

async function onCreate() {
  const parent = props.parent
  if (!parent) return
  const combos = validCombos.value
  if (combos.length === 0) {
    syncError.value = t('admin.accounts.codeBuddyNoSelection')
    return
  }
  creating.value = true
  syncError.value = ''
  const success: { model: string; groupId: number; groupName: string }[] = []
  const failed: { model: string; groupId: number; groupName: string; error: string }[] = []
  const parentSite = parent.credentials?.site
  for (const combo of combos) {
    try {
      const created = await createCodeBuddyShadow(parent.id, {
        // 后端 CreateShadow 的缺省名不含站点段，故由前端显式传 name（N3）。
        name: codeBuddyShadowName(parent.name, parentSite, combo.model, combo.suffix ? combo.groupName : null),
        model: combo.model,
        platform: combo.platform,
        group_ids: [combo.groupId],
        priority: combo.priority,
      })
      success.push({ model: combo.model, groupId: combo.groupId, groupName: combo.groupName })
      const row = rows.value.find((r) => r.model === combo.model)
      if (row && !row.createdGroupIds.includes(combo.groupId)) {
        row.createdGroupIds = [...row.createdGroupIds, combo.groupId]
        if (row.shadowId == null) row.shadowId = created?.id ?? null
      }
    } catch (err) {
      failed.push({
        model: combo.model,
        groupId: combo.groupId,
        groupName: combo.groupName,
        error: err instanceof Error ? err.message : String(err),
      })
    }
  }
  createResults.value = { success, failed }
  selected.value = new Set()
  creating.value = false
  if (success.length > 0) emit('created')
}

// 每次打开重置状态（保留 parent 选择）。
// 注意：官方价缓存是模块级的，刻意不在此清空 —— 会话内同一模型只查一次目录。
watch(
  () => props.show,
  (visible) => {
    if (visible) {
      state.value = 'guide'
      rows.value = []
      selected.value = new Set()
      pickerModel.value = null
      syncError.value = ''
      createResults.value = null
    }
  },
  { immediate: true }
)
</script>
