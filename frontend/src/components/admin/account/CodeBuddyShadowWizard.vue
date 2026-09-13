<template>
  <Teleport to="body">
    <div v-if="show && parent" class="fixed inset-0 z-[10000] flex items-center justify-center p-4">
      <div class="absolute inset-0 bg-black/40" @click="$emit('close')" />
      <div
        class="relative flex max-h-[88vh] w-full max-w-5xl flex-col overflow-hidden rounded-xl bg-white shadow-2xl ring-1 ring-black/5 dark:bg-dark-800"
      >
        <!-- Header -->
        <div class="flex items-center justify-between border-b border-gray-200 px-5 py-3 dark:border-dark-700">
          <div>
            <h3 class="text-base font-semibold text-gray-900 dark:text-white">
              {{ t('admin.accounts.codeBuddyWizardTitle') }}
            </h3>
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.codeBuddyWizardOpen', { name: parent.name }) }}</p>
          </div>
            <button class="rounded p-1 text-gray-400 hover:bg-gray-100 dark:hover:bg-dark-700" @click="$emit('close')">
            <Icon name="x" size="md" />
          </button>
        </div>

        <!-- Body -->
        <div class="flex-1 overflow-y-auto px-5 py-4">
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
                class="ml-auto rounded-lg bg-cyan-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-cyan-700 disabled:opacity-60"
                :disabled="creating || validSelections.length === 0"
                @click="onCreate"
              >
                <Icon v-if="creating" name="refresh" size="sm" class="mr-1 inline animate-spin" />
                {{ t('admin.accounts.codeBuddyBatchCreate') }}
                <template v-if="validSelections.length > 0"> ({{ validSelections.length }})</template>
              </button>
            </div>

            <div v-if="syncError" class="mb-3 rounded-lg border border-red-300 bg-red-50 px-3 py-2 text-xs text-red-700 dark:border-red-700/50 dark:bg-red-900/20 dark:text-red-300">
              {{ syncError }}
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
                    <th class="px-3 py-2 w-20">{{ t('admin.accounts.codeBuddyColMultiplier') }}</th>
                    <th class="px-3 py-2 w-20">{{ t('admin.accounts.codeBuddyColPriority') }}</th>
                    <th class="px-3 py-2">{{ t('admin.accounts.codeBuddyColStatus') }}</th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                  <tr v-for="row in rows" :key="row.model" :class="row.created ? 'opacity-60' : ''">
                    <td class="px-3 py-2">
                      <input
                        v-if="!row.created"
                        type="checkbox"
                        :checked="selected.has(row.model)"
                        @change="toggleSelect(row.model, ($event.target as HTMLInputElement).checked)"
                      />
                      <Icon v-else name="check" size="sm" class="text-emerald-500" />
                    </td>
                    <td class="px-3 py-2 font-medium text-gray-900 dark:text-white">{{ row.model }}</td>
                    <td class="px-3 py-2 text-gray-500 dark:text-gray-400">{{ formatTokens(row.contextWindow) }}</td>
                    <td class="px-3 py-2 text-gray-500 dark:text-gray-400">{{ formatTokens(row.maxOutput) }}</td>
                    <td class="px-3 py-2">
                      <span v-if="officialPrice(row.model)" class="text-gray-700 dark:text-gray-200">{{ officialPrice(row.model) }}</span>
                      <span v-else class="text-gray-400">{{ t('admin.accounts.codeBuddyPriceUnpriced') }}
                        <button class="ml-1 text-cyan-600 underline" @click="$emit('configure-price', row.model)">{{ t('admin.accounts.codeBuddyConfigurePrice') }}</button>
                      </span>
                    </td>
                    <td class="px-3 py-2">
                      <select
                        v-model="row.platform"
                        class="rounded border border-gray-300 bg-white px-1.5 py-1 text-xs dark:border-dark-600 dark:bg-dark-800"
                      >
                        <option v-for="opt in targetPlatformOptions" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
                      </select>
                    </td>
                    <td class="px-3 py-2">
                      <select
                        v-model="row.groupId"
                        class="min-w-[120px] rounded border border-gray-300 bg-white px-1.5 py-1 text-xs dark:border-dark-600 dark:bg-dark-800"
                        :class="selected.has(row.model) && !row.groupId ? 'border-red-400' : ''"
                      >
                        <option value="">—</option>
                        <option v-for="g in groupsForPlatform(row.platform)" :key="g.id" :value="g.id">{{ g.name }}</option>
                      </select>
                    </td>
                    <td class="px-3 py-2">
                      <input
                        v-model.number="row.multiplier"
                        type="number" min="0.1" step="0.1"
                        class="w-16 rounded border border-gray-300 bg-white px-1.5 py-1 text-xs dark:border-dark-600 dark:bg-dark-800"
                      />
                    </td>
                    <td class="px-3 py-2">
                      <input
                        v-model.number="row.priority"
                        type="number" step="1"
                        class="w-16 rounded border border-gray-300 bg-white px-1.5 py-1 text-xs dark:border-dark-600 dark:bg-dark-800"
                      />
                    </td>
                    <td class="px-3 py-2">
                      <span v-if="row.created" class="rounded bg-emerald-100 px-1.5 py-0.5 text-[10px] font-medium text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300">
                        {{ t('admin.accounts.codeBuddyStatusCreated') }}
                      </span>
                      <button
                        v-else-if="row.shadowId != null"
                        class="text-cyan-600 underline"
                        @click="$emit('jump-parent')"
                      >{{ t('admin.accounts.codeBuddyJumpParent') }}</button>
                      <span v-else class="text-gray-400">{{ t('admin.accounts.codeBuddyStatusNotCreated') }}</span>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>

            <!-- 创建结果 -->
            <div v-if="createResults" class="mt-3 rounded-lg border px-3 py-2 text-sm"
              :class="createResults.failed.length === 0 ? 'border-emerald-300 bg-emerald-50 text-emerald-700 dark:border-emerald-700/50 dark:bg-emerald-900/20 dark:text-emerald-300' : 'border-amber-300 bg-amber-50 text-amber-800 dark:border-amber-700/50 dark:bg-amber-900/20 dark:text-amber-300'">
              <template v-if="createResults.failed.length === 0">{{ t('admin.accounts.codeBuddyBatchCreateSuccess', { count: createResults.success.length }) }}</template>
              <template v-else>{{ t('admin.accounts.codeBuddyBatchCreatePartial', { success: createResults.success.length, failed: createResults.failed.length }) }}</template>
              <ul v-if="createResults.failed.length > 0" class="mt-1 list-inside list-disc text-xs">
                <li v-for="f in createResults.failed" :key="f.model">{{ f.model }}: {{ f.error }}</li>
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
import type { Account, AdminGroup, AccountListItem } from '@/types'
import { syncUpstreamModels, createCodeBuddyShadow } from '@/api/admin/accounts'
import type { UpstreamModelMetadata } from '@/api/admin/accounts'
import { CONCRETE_PLATFORM_OPTIONS, inferCodeBuddyShadowPlatform } from '@/constants/platforms'

// 影子允许落入的目标分组平台（排除 openai/codebuddy 等，与后端 CreateShadow 守卫一致）。
const TARGET_PLATFORMS = ['deepseek', 'zhipu', 'kimi', 'minimax', 'other']
const targetPlatformOptions = CONCRETE_PLATFORM_OPTIONS.filter((p) => TARGET_PLATFORMS.includes(p.value))

export interface CodeBuddyShadowWizardRow {
  model: string
  contextWindow: number
  maxOutput: number
  platform: string
  groupId: number | ''
  multiplier: number
  priority: number
  created: boolean
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
const createResults = ref<{ success: Account[]; failed: { model: string; error: string }[] } | null>(null)

const isIntl = computed(() => props.parent?.credentials?.site === 'intl')
const selectedCount = computed(() => selected.value.size)

const groupsForPlatform = (platform: string) =>
  props.groups.filter((g) => g.platform === platform && g.status === 'active')

function officialPrice(_model: string): string | null {
  // 官方价接入点（模型广场/计费配置）暂未对接；未配置时返回 null，UI 显示「未定价 → 去配置」。
  return null
}

function formatTokens(n: number): string {
  if (!n || n <= 0) return '-'
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(n % 1_000_000 === 0 ? 0 : 1) + 'M'
  if (n >= 1000) return (n / 1000).toFixed(n % 1000 === 0 ? 0 : 1) + 'K'
  return String(n)
}

function buildRows(metadata: Record<string, UpstreamModelMetadata> | undefined, modelIds: string[]) {
  const result: CodeBuddyShadowWizardRow[] = []
  const seen = new Set<string>()
  const ids = metadata && Object.keys(metadata).length > 0 ? Object.keys(metadata) : modelIds
  for (const id of ids) {
    const key = id.trim()
    if (!key || seen.has(key)) continue
    seen.add(key)
    const meta: UpstreamModelMetadata | undefined = metadata?.[key]
    const existingShadow = props.shadows.find((s) => s.extra?.shadow_model === key)
    result.push({
      model: key,
      contextWindow: meta?.context_window ?? 0,
      maxOutput: meta?.max_output_tokens ?? 0,
      platform: inferCodeBuddyShadowPlatform(key),
      groupId: '',
      multiplier: 1,
      priority: props.parent?.priority ?? 0,
      created: Boolean(existingShadow),
      shadowId: existingShadow?.id ?? null,
    })
  }
  result.sort((a, b) => a.model.localeCompare(b.model))
  return result
}

async function onSync() {
  if (!props.parent) return
  syncing.value = true
  syncError.value = ''
  createResults.value = null
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
  } catch (err) {
    syncError.value = t('admin.accounts.codeBuddySyncFailed', { error: err instanceof Error ? err.message : String(err) })
    state.value = 'guide'
  } finally {
    syncing.value = false
  }
}

function toggleSelect(model: string, checked: boolean) {
  const next = new Set(selected.value)
  if (checked) next.add(model)
  else next.delete(model)
  selected.value = next
}

function selectAll() {
  selected.value = new Set(rows.value.filter((r) => !r.created).map((r) => r.model))
}

function deselectAll() {
  selected.value = new Set()
}

const validSelections = computed(() =>
  rows.value.filter((r) => selected.value.has(r.model) && !r.created && r.groupId !== '')
)

async function onCreate() {
  if (!props.parent) return
  const targets = validSelections.value
  if (targets.length === 0) {
    syncError.value = t('admin.accounts.codeBuddyNoSelection')
    return
  }
  creating.value = true
  const success: Account[] = []
  const failed: { model: string; error: string }[] = []
  for (const row of targets) {
    try {
      const created = await createCodeBuddyShadow(props.parent.id, {
        model: row.model,
        platform: row.platform,
        group_ids: [Number(row.groupId)],
        priority: row.priority,
        // multiplier 仅作展示占位，后端当前以分组倍率为准；不传倍率。
      })
      success.push(created)
    } catch (err) {
      failed.push({ model: row.model, error: err instanceof Error ? err.message : String(err) })
    }
  }
  createResults.value = { success, failed }
  // 刷新影子状态：把新建的影子并入 props.shadows 视角（由父页面刷新列表更准确，这里先本地标记）。
  for (const acc of success) {
    const m = acc.extra?.shadow_model as string | undefined
    const row = rows.value.find((r) => r.model === m)
    if (row) {
      row.created = true
      row.shadowId = acc.id
    }
  }
  selected.value = new Set()
  creating.value = false
  if (success.length > 0) emit('created')
}

// 每次打开重置状态（保留 parent 选择）。
watch(
  () => props.show,
  (visible) => {
    if (visible) {
      state.value = 'guide'
      rows.value = []
      selected.value = new Set()
      syncError.value = ''
      createResults.value = null
    }
  },
  { immediate: true }
)
</script>
