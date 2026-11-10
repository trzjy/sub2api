<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div class="text-sm text-gray-600 dark:text-gray-300">
            <p class="font-medium text-gray-800 dark:text-gray-100">
              {{ t('admin.largeRequestRouting.title') }}
            </p>
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.largeRequestRouting.description') }}
            </p>
          </div>
        </div>
      </template>

      <template #table>
        <div
          v-if="errorMessage"
          data-testid="lro-error-banner"
          class="mb-4 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700 dark:border-red-900/50 dark:bg-red-900/20 dark:text-red-300"
        >
          {{ errorMessage }}
        </div>

        <div v-if="loading" class="flex items-center justify-center py-16 text-sm text-gray-500 dark:text-gray-400">
          {{ t('common.loading') }}
        </div>

        <div v-else class="space-y-4">
          <section
            v-for="family in FAMILIES"
            :key="family.platform"
            class="overflow-hidden rounded-xl border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-800"
            :data-testid="`family-${family.platform}`"
          >
            <button
              type="button"
              class="flex w-full items-center justify-between px-4 py-3 text-left transition-colors hover:bg-gray-50 dark:hover:bg-dark-700"
              :data-testid="`family-toggle-${family.platform}`"
              :aria-expanded="isExpanded(family.platform)"
              @click="toggleFamily(family.platform)"
            >
              <span class="flex items-center gap-3">
                <h2 class="text-base font-semibold text-gray-800 dark:text-gray-100">{{ family.title }}</h2>
                <span class="rounded-full bg-gray-100 px-2 py-0.5 text-xs text-gray-500 dark:bg-dark-700 dark:text-gray-400">
                  {{ accountCount(family.platform) }}
                </span>
              </span>
              <span class="text-xs text-gray-500 dark:text-gray-400">
                {{ isExpanded(family.platform) ? t('admin.largeRequestRouting.collapse') : t('admin.largeRequestRouting.expand') }}
              </span>
            </button>

            <div v-show="isExpanded(family.platform)" class="border-t border-gray-100 px-4 py-3 dark:border-dark-700">
              <div
                v-for="segment in SEGMENTS"
                :key="`${family.platform}-${segment.key}`"
                class="mb-5 last:mb-0"
                :data-testid="`segment-${family.platform}-${segment.key}`"
              >
                <h3 class="mb-2 text-sm font-medium text-gray-600 dark:text-gray-300">
                  {{ segment.label }}
                </h3>

                <table class="w-full">
                  <thead>
                    <tr class="border-b border-gray-200 text-left text-xs text-gray-500 dark:border-dark-700 dark:text-gray-400">
                      <th class="w-16 py-2 pr-3 font-medium">#</th>
                      <th class="py-2 pr-3 font-medium">{{ t('admin.largeRequestRouting.name') }}</th>
                      <th class="w-24 py-2 pr-3 font-medium">{{ t('admin.largeRequestRouting.platform') }}</th>
                      <th class="w-20 py-2 pr-3 font-medium">{{ t('admin.largeRequestRouting.priority') }}</th>
                      <th class="w-40 py-2 pr-3 font-medium">{{ t('admin.largeRequestRouting.status') }}</th>
                      <th class="py-2 pr-3 font-medium">{{ t('admin.largeRequestRouting.group') }}</th>
                      <th class="w-44 py-2 font-medium">{{ t('admin.largeRequestRouting.switch') }}</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr
                      v-for="row in rowsByFamily[family.platform][segment.key]"
                      :key="`${row.account.id}-${row.account.platform}`"
                      class="border-b border-gray-100 text-sm text-gray-700 dark:border-dark-800 dark:text-gray-300"
                      :data-testid="`row-${family.platform}-${segment.key}-${row.account.id}`"
                    >
                      <td class="py-2 pr-3 tabular-nums text-gray-500 dark:text-gray-400">#{{ row.account.id }}</td>
                      <td class="py-2 pr-3">{{ row.account.name }}</td>
                      <td class="py-2 pr-3">
                        <span
                          class="inline-flex items-center rounded px-1.5 py-0.5 text-xs"
                          :class="row.account.platform === family.platform
                            ? 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
                            : 'bg-amber-50 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400'"
                        >
                          {{ row.account.platform }}
                        </span>
                      </td>
                      <td class="py-2 pr-3 tabular-nums">{{ row.account.priority }}</td>
                      <td class="py-2 pr-3">
                        <span
                          class="mr-1 inline-flex items-center rounded-full px-2 py-0.5 text-xs"
                          :class="statusClass(row.account.status)"
                        >
                          {{ statusLabel(row.account.status) }}
                        </span>
                        <span
                          class="inline-flex items-center rounded-full px-2 py-0.5 text-xs"
                          :class="row.account.schedulable ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400' : 'bg-gray-100 text-gray-500 dark:bg-dark-700 dark:text-gray-400'"
                        >
                          {{ row.account.schedulable ? t('admin.largeRequestRouting.schedulable') : t('admin.largeRequestRouting.unschedulable') }}
                        </span>
                      </td>
                      <td class="py-2 pr-3">{{ row.groupNames.join('、') }}</td>
                      <td class="py-2">
                        <select
                          class="h-8 w-full rounded-md border border-gray-300 bg-white px-2 text-sm text-gray-800 disabled:cursor-not-allowed disabled:bg-gray-100 disabled:text-gray-400 dark:border-dark-600 dark:bg-dark-800 dark:text-gray-100 dark:disabled:bg-dark-700"
                          :data-testid="`lro-scope-${row.account.id}`"
                          :value="requestSizeScopeOf(row.account)"
                          :disabled="isScopeDisabled(row.account, family.platform)"
                          :title="isScopeDisabled(row.account, family.platform) ? scopeDisabledReason(row.account, family.platform) : ''"
                          @change="onScopeChange(row.account, $event)"
                        >
                          <option value="any">{{ t('admin.largeRequestRouting.scopeAny') }}</option>
                          <option value="large_only">{{ t('admin.largeRequestRouting.scopeLargeOnly') }}</option>
                          <option value="small_only">{{ t('admin.largeRequestRouting.scopeSmallOnly') }}</option>
                        </select>
                      </td>
                    </tr>
                    <tr v-if="(rowsByFamily[family.platform][segment.key] || []).length === 0">
                      <td colspan="7" class="py-4 text-center text-xs text-gray-400 dark:text-gray-500">
                        {{ t('admin.largeRequestRouting.noAccounts') }}
                      </td>
                    </tr>
                  </tbody>
                </table>
              </div>
            </div>
          </section>
        </div>
      </template>
    </TablePageLayout>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import { adminAPI } from '@/api/admin'
import type { AccountListItem, AdminGroup, GroupPlatform } from '@/types'

const { t } = useI18n()

type FamilyPlatform = 'deepseek' | 'zhipu' | 'kimi'
type Scope = 'any' | 'large_only' | 'small_only'

const FAMILIES: Array<{ platform: FamilyPlatform; title: string }> = [
  { platform: 'deepseek', title: 'DeepSeek' },
  { platform: 'zhipu', title: 'GLM' },
  { platform: 'kimi', title: 'Kimi' }
]

const SEGMENTS: Array<{ key: 'standard' | 'subscription'; label: string }> = [
  { key: 'standard', label: t('admin.largeRequestRouting.meteredGroups') },
  { key: 'subscription', label: t('admin.largeRequestRouting.subscriptionGroups') }
]

// 页面只按三个家族分段，但分组里可能绑定其他平台的账号（如 codebuddy），
// 必须一并加载并展示，否则会看不到池内真实成员。
const LOADED_PLATFORMS = ['deepseek', 'zhipu', 'kimi', 'codebuddy'] as const

const ACCOUNT_PAGE_SIZE = 200

interface SegmentRow {
  account: AccountListItem
  groupNames: string[]
}

const loading = ref(false)
const errorMessage = ref('')
const accounts = ref<AccountListItem[]>([])
const groups = ref<AdminGroup[]>([])
const expanded = ref<Record<FamilyPlatform, boolean>>({ deepseek: true, zhipu: false, kimi: false })

const groupsByID = computed(() => new Map(groups.value.map((group) => [group.id, group])))

const SUPPORTED_FAMILIES = new Set(['deepseek', 'zhipu', 'kimi'])

function isSupportedPlatform(platform: GroupPlatform): platform is FamilyPlatform {
  return SUPPORTED_FAMILIES.has(platform)
}

function isExpanded(platform: FamilyPlatform): boolean {
  return expanded.value[platform]
}

function toggleFamily(platform: FamilyPlatform): void {
  expanded.value = { ...expanded.value, [platform]: !expanded.value[platform] }
}

/**
 * 账号按 group_ids 落入对应家族的对应段。同一账号挂多段（如同时计量组与订阅组）
 * 时每段各显示一行；同一账号在同一段挂多个分组时合并为一行并列出全部组名。
 * 跨平台账号（如 codebuddy 绑在 deepseek 订阅组）也一并展示，按账号自身平台标注。
 */
const rowsByFamily = computed(() => {
  const result: Record<
    FamilyPlatform,
    Record<'standard' | 'subscription', SegmentRow[]>
  > = {
    deepseek: { standard: [], subscription: [] },
    zhipu: { standard: [], subscription: [] },
    kimi: { standard: [], subscription: [] }
  }
  const seen = new Set<string>()
  for (const account of accounts.value) {
    for (const groupID of account.group_ids ?? []) {
      const group = groupsByID.value.get(groupID)
      if (!group) continue
      if (!isSupportedPlatform(group.platform)) continue
      const family = group.platform
      const segment = group.subscription_type === 'subscription' ? 'subscription' : 'standard'
      const key = `${account.id}:${family}:${segment}`
      const bucket = result[family][segment]
      if (seen.has(key)) {
        const row = bucket.find((r) => r.account.id === account.id)
        if (row && !row.groupNames.includes(group.name)) {
          row.groupNames.push(group.name)
        }
        continue
      }
      seen.add(key)
      bucket.push({ account, groupNames: [group.name] })
    }
  }
  for (const family of Object.keys(result) as FamilyPlatform[]) {
    for (const segment of Object.keys(result[family]) as Array<'standard' | 'subscription'>) {
      result[family][segment].sort((a, b) => a.account.id - b.account.id)
    }
  }
  return result
})

function accountCount(platform: FamilyPlatform): number {
  const seen = new Set<number>()
  for (const segment of ['standard', 'subscription'] as const) {
    for (const row of rowsByFamily.value[platform][segment]) {
      seen.add(row.account.id)
    }
  }
  return seen.size
}

function requestSizeScopeOf(account: AccountListItem): Scope {
  const extra = account.extra ?? {}
  if (extra.large_request_only === true) return 'large_only'
  if (extra.small_request_only === true) return 'small_only'
  return 'any'
}

/**
 * 池内最后一个禁用：某账号所属任一分组的同 platform 且 schedulable && status==='active'
 * 的账号中，除本账号外没有任何「未设限制」的账号 → 不可设置（下拉禁用）。
 * 每次只允许一侧受限时其余账号仍需承接对侧，因此判断口径与后端池覆盖校验一致。
 */
function isPoolLast(account: AccountListItem): boolean {
  const groupIDs = account.group_ids ?? []
  for (const groupID of groupIDs) {
    const others = accounts.value.filter(
      (a) =>
        a.id !== account.id &&
        a.platform === account.platform &&
        a.schedulable &&
        a.status === 'active' &&
        (a.group_ids ?? []).includes(groupID)
    )
    const hasUnrestricted = others.some((a) => requestSizeScopeOf(a) === 'any')
    if (!hasUnrestricted) {
      return true
    }
  }
  return false
}

function isScopeDisabled(account: AccountListItem, family: FamilyPlatform): boolean {
  // 跨平台账号不参与本家族调度，开关无意义，禁用。
  if (account.platform !== family) {
    return true
  }
  // 已受限时始终允许改回不限制。
  if (requestSizeScopeOf(account) !== 'any') {
    return false
  }
  return isPoolLast(account)
}

function scopeDisabledReason(account: AccountListItem, family: FamilyPlatform): string {
  if (account.platform !== family) {
    return t('admin.largeRequestRouting.crossPlatformHint')
  }
  return t('admin.largeRequestRouting.lastPoolGuard')
}

function statusClass(status: string): string {
  switch (status) {
    case 'active':
      return 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
    case 'inactive':
      return 'bg-gray-100 text-gray-500 dark:bg-dark-700 dark:text-gray-400'
    default:
      return 'bg-red-50 text-red-700 dark:bg-red-900/20 dark:text-red-400'
  }
}

function statusLabel(status: string): string {
  switch (status) {
    case 'active':
      return t('admin.largeRequestRouting.statusActive')
    case 'inactive':
      return t('admin.largeRequestRouting.statusInactive')
    default:
      return t('admin.largeRequestRouting.statusError')
  }
}

function extractError(err: unknown): string {
  const e = (err ?? {}) as { code?: string; message?: string; reason?: string; error?: string }
  if (e.code === 'REQUEST_SIZE_SCOPE_POOL_EXHAUSTED') {
    return e.message || e.reason || t('admin.largeRequestRouting.poolExhaustedFallback')
  }
  return e.message || e.reason || e.error || String(err || '')
}

function setLocalScope(accountID: number, scope: Scope): void {
  const target = accounts.value.find((a) => a.id === accountID)
  if (!target) return
  const next: Record<string, unknown> = { ...(target.extra ?? {}) }
  next.large_request_only = scope === 'large_only'
  next.small_request_only = scope === 'small_only'
  target.extra = next
}

async function onScopeChange(account: AccountListItem, event: Event): Promise<void> {
  const next = (event.target as HTMLSelectElement).value as Scope
  const previous = requestSizeScopeOf(account)
  if (next === previous) return
  errorMessage.value = ''
  // 乐观更新，成功后与后端返回值保持一致；失败回滚
  setLocalScope(account.id, next)
  try {
    const updated = await adminAPI.accounts.setAccountRequestSizeScope(account.id, next)
    if (updated?.extra) {
      const confirmed: Scope = updated.extra.large_request_only === true
        ? 'large_only'
        : updated.extra.small_request_only === true
          ? 'small_only'
          : 'any'
      setLocalScope(account.id, confirmed)
    }
  } catch (err) {
    setLocalScope(account.id, previous)
    errorMessage.value = extractError(err)
  }
}

async function loadAccountsForPlatform(platform: (typeof LOADED_PLATFORMS)[number]): Promise<AccountListItem[]> {
  const all: AccountListItem[] = []
  let page = 1
  for (;;) {
    const response = await adminAPI.accounts.list(page, ACCOUNT_PAGE_SIZE, { platform })
    all.push(...response.items)
    if (all.length >= response.total || response.items.length === 0) {
      break
    }
    page += 1
  }
  return all
}

onMounted(async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    const [groupList, ...platformAccounts] = await Promise.all([
      adminAPI.groups.getAllIncludingInactive(),
      ...LOADED_PLATFORMS.map((platform) => loadAccountsForPlatform(platform))
    ])
    groups.value = groupList
    accounts.value = platformAccounts.flat()
  } catch (err) {
    errorMessage.value = extractError(err)
  } finally {
    loading.value = false
  }
})
</script>
