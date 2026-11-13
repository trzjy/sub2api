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
        <!-- TablePageLayout 的表格槽位是 overflow:hidden 的固定高度容器，
             内容超出会被裁掉且无法滚动。这里再加一层滚动容器，让全部账号
             都能靠右侧滚动条下滑查看。 -->
        <div class="h-full overflow-y-auto pr-1">
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
            class="rounded-xl border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-800"
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
                  {{ rowsByFamily[family.platform].length }}
                </span>
              </span>
              <span class="text-xs text-gray-500 dark:text-gray-400">
                {{ isExpanded(family.platform) ? t('admin.largeRequestRouting.collapse') : t('admin.largeRequestRouting.expand') }}
              </span>
            </button>

            <div v-show="isExpanded(family.platform)" class="border-t border-gray-100 px-4 py-2 dark:border-dark-700">
              <!-- 账号全部完整渲染，不做内部限高/裁切：靠页面右侧滚动条往下看，
                   表头 sticky 在视口顶部，滚动时列名始终可见。 -->
              <table class="w-full">
                <thead class="sticky top-0 z-10 bg-white dark:bg-dark-800">
                  <tr class="border-b border-gray-200 text-left text-xs text-gray-500 dark:border-dark-700 dark:text-gray-400">
                    <th class="w-16 py-2 pr-3 font-medium">#</th>
                    <th class="py-1.5 pr-3 font-medium">{{ t('admin.largeRequestRouting.name') }}</th>
                    <th class="w-24 py-2 pr-3 font-medium">{{ t('admin.largeRequestRouting.platform') }}</th>
                    <th class="w-20 py-2 pr-3 font-medium">{{ t('admin.largeRequestRouting.priority') }}</th>
                    <th class="w-40 py-2 pr-3 font-medium">{{ t('admin.largeRequestRouting.status') }}</th>
                    <th class="py-1.5 pr-3 font-medium">{{ t('admin.largeRequestRouting.group') }}</th>
                    <th class="w-44 py-2 font-medium">{{ t('admin.largeRequestRouting.switch') }}</th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="row in rowsByFamily[family.platform]"
                    :key="row.account.id"
                    class="border-b border-gray-100 text-sm text-gray-700 dark:border-dark-800 dark:text-gray-300"
                    :data-testid="`row-${family.platform}-${row.account.id}`"
                  >
                    <td class="py-1.5 pr-3 tabular-nums text-gray-500 dark:text-gray-400">#{{ row.account.id }}</td>
                    <td class="py-1.5 pr-3">{{ row.account.name }}</td>
                    <td class="py-1.5 pr-3">
                      <span
                        class="inline-flex items-center rounded px-1.5 py-0.5 text-xs"
                        :class="row.account.platform === family.platform
                          ? 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
                          : 'bg-amber-50 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400'"
                      >
                        {{ row.account.platform }}
                      </span>
                    </td>
                    <td class="py-1.5 pr-3 tabular-nums">{{ row.account.priority }}</td>
                    <td class="py-1.5 pr-3">
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
                    <td class="py-1.5 pr-3">{{ row.groupNames.join('、') }}</td>
                    <td class="py-1.5">
                      <select
                        class="h-8 w-full rounded-md border border-gray-300 bg-white px-2 text-sm text-gray-800 disabled:cursor-not-allowed disabled:bg-gray-100 disabled:text-gray-400 dark:border-dark-600 dark:bg-dark-800 dark:text-gray-100 dark:disabled:bg-dark-700"
                        :data-testid="`lro-scope-${row.account.id}`"
                        :value="requestSizeScopeOf(row.account)"
                        :disabled="row.account.platform !== family.platform"
                        :title="row.account.platform !== family.platform ? t('admin.largeRequestRouting.crossPlatformHint') : ''"
                        @change="onScopeChange(row.account, $event)"
                      >
                        <option value="any">{{ t('admin.largeRequestRouting.scopeAny') }}</option>
                        <option
                          value="large_only"
                          :disabled="isScopeOptionBlocked(row.account, 'large_only')"
                          :title="isScopeOptionBlocked(row.account, 'large_only') ? t('admin.largeRequestRouting.lastPoolGuard') : ''"
                        >
                          {{ t('admin.largeRequestRouting.scopeLargeOnly') }}
                        </option>
                        <option
                          value="small_only"
                          :disabled="isScopeOptionBlocked(row.account, 'small_only')"
                          :title="isScopeOptionBlocked(row.account, 'small_only') ? t('admin.largeRequestRouting.lastPoolGuard') : ''"
                        >
                          {{ t('admin.largeRequestRouting.scopeSmallOnly') }}
                        </option>
                      </select>
                    </td>
                  </tr>
                  <tr v-if="rowsByFamily[family.platform].length === 0">
                    <td colspan="7" class="py-4 text-center text-xs text-gray-400 dark:text-gray-500">
                      {{ t('admin.largeRequestRouting.noAccounts') }}
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </section>
        </div>
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

type FamilyPlatform = 'deepseek' | 'zhipu' | 'kimi' | 'cursor'
type Scope = 'any' | 'large_only' | 'small_only'

const FAMILIES: Array<{ platform: FamilyPlatform; title: string }> = [
  { platform: 'deepseek', title: 'DeepSeek' },
  { platform: 'zhipu', title: 'GLM' },
  { platform: 'kimi', title: 'Kimi' },
  { platform: 'cursor', title: 'Cursor' }
]

// 页面只按四个家族分块，但分组里可能绑定其他平台的账号（如 codebuddy），
// 必须一并加载并展示，否则会看不到池内真实成员。
// Cursor 账号不通过分组归属，直接按 platform 展示。
const LOADED_PLATFORMS = ['deepseek', 'zhipu', 'kimi', 'codebuddy', 'cursor'] as const

const ACCOUNT_PAGE_SIZE = 200

interface AccountRow {
  account: AccountListItem
  groupNames: string[]
}

const loading = ref(false)
const errorMessage = ref('')
const accounts = ref<AccountListItem[]>([])
const groups = ref<AdminGroup[]>([])
// 四个平台默认全部展开：折叠隐藏内容会让用户以为平台缺失。
const expanded = ref<Record<FamilyPlatform, boolean>>({ deepseek: true, zhipu: true, kimi: true, cursor: true })

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
 * 每个账号在每个家族只出现一行，分组列合并展示它在该家族下绑定的全部分组名
 * （与站点既有账号列表的展示方式一致，不再按计量/订阅拆行）。
 * 跨平台账号（如 codebuddy 绑在 deepseek 订阅组）也一并展示，按账号自身平台标注。
 */
const rowsByFamily = computed(() => {
  const result: Record<FamilyPlatform, AccountRow[]> = {
    deepseek: [],
    zhipu: [],
    kimi: [],
    cursor: []
  }
  for (const account of accounts.value) {
    // Cursor 账号不通过分组归属，直接按 platform 收录。
    if (account.platform === 'cursor') {
      result.cursor.push({ account, groupNames: [] })
      continue
    }
    const namesByFamily: Record<FamilyPlatform, string[]> = { deepseek: [], zhipu: [], kimi: [], cursor: [] }
    for (const groupID of account.group_ids ?? []) {
      const group = groupsByID.value.get(groupID)
      if (!group) continue
      // 分组自身的 platform 决定归属；聚合分组（platform=composite，如「聚合月卡订阅」）
      // 不属于任何单一家族，按账号自身平台归属，否则该组名会在页面上彻底消失，
      // 让人误以为账号没绑这个组。
      const family: FamilyPlatform | null = isSupportedPlatform(group.platform)
        ? group.platform
        : isSupportedPlatform(account.platform as GroupPlatform)
          ? (account.platform as FamilyPlatform)
          : null
      if (!family) continue
      if (!namesByFamily[family].includes(group.name)) {
        namesByFamily[family].push(group.name)
      }
    }
    for (const family of FAMILIES) {
      if (family.platform === 'cursor') continue
      const names = namesByFamily[family.platform]
      if (names.length > 0) {
        result[family.platform].push({ account, groupNames: names })
      }
    }
  }
  for (const family of FAMILIES) {
    result[family.platform].sort((a, b) => a.account.id - b.account.id)
  }
  return result
})

function requestSizeScopeOf(account: AccountListItem): Scope {
  const extra = account.extra ?? {}
  if (extra.large_request_only === true) return 'large_only'
  if (extra.small_request_only === true) return 'small_only'
  return 'any'
}

/**
 * 判断把账号设为 target 后，是否会让某个所属分组在该规模上失去承接方。
 *
 * 口径与后端池覆盖校验逐条一致：排除本账号后，统计同分组、同平台、active 且
 * schedulable 的账号里，是否存在「不属于 target 这一侧」的账号（即能承接对侧规模：
 * 设 large_only 时看非 large_only，设 small_only 时看非 small_only）。
 * 对端只要不是同一侧就算可用——例如目标设 large_only 时，small_only 的对端仍能接
 * 小请求，必须算作可用，否则前端会禁用掉后端允许的合法配置。
 */
function isScopeOptionBlocked(account: AccountListItem, target: Scope): boolean {
  if (target === 'any') {
    return false
  }
  for (const groupID of account.group_ids ?? []) {
    const others = accounts.value.filter(
      (a) =>
        a.id !== account.id &&
        a.platform === account.platform &&
        a.schedulable &&
        a.status === 'active' &&
        (a.group_ids ?? []).includes(groupID)
    )
    if (!others.some((a) => requestSizeScopeOf(a) !== target)) {
      return true
    }
  }
  return false
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
