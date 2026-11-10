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
        <div v-if="errorMessage" data-testid="lro-error-banner" class="mb-4 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700 dark:border-red-900/50 dark:bg-red-900/20 dark:text-red-300">
          {{ errorMessage }}
        </div>

        <div v-if="loading" class="flex items-center justify-center py-16 text-sm text-gray-500 dark:text-gray-400">
          {{ t('common.loading') }}
        </div>

        <div v-else class="grid grid-cols-1 gap-6 xl:grid-cols-3">
          <section
            v-for="family in FAMILIES"
            :key="family.platform"
            class="rounded-xl border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800"
            :data-testid="`family-${family.platform}`"
          >
            <h2 class="mb-3 text-base font-semibold text-gray-800 dark:text-gray-100">
              {{ family.title }}
            </h2>

            <div
              v-for="segment in SEGMENTS"
              :key="`${family.platform}-${segment.key}`"
              class="mb-4 last:mb-0"
              :data-testid="`segment-${family.platform}-${segment.key}`"
            >
              <h3 class="mb-2 text-sm font-medium text-gray-600 dark:text-gray-300">
                {{ segment.label }}
              </h3>

              <table class="w-full">
                <thead>
                  <tr class="border-b border-gray-200 text-left text-xs text-gray-500 dark:border-dark-700 dark:text-gray-400">
                    <th class="py-2 pr-2 font-medium">#</th>
                    <th class="py-2 pr-2 font-medium">{{ t('admin.largeRequestRouting.name') }}</th>
                    <th class="py-2 pr-2 font-medium">{{ t('admin.largeRequestRouting.priority') }}</th>
                    <th class="py-2 pr-2 font-medium">{{ t('admin.largeRequestRouting.status') }}</th>
                    <th class="py-2 pr-2 font-medium">{{ t('admin.largeRequestRouting.group') }}</th>
                    <th class="py-2 font-medium">{{ t('admin.largeRequestRouting.switch') }}</th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="row in rowsByFamily[family.platform][segment.key]"
                    :key="row.account.id"
                    class="border-b border-gray-100 text-sm text-gray-700 dark:border-dark-800 dark:text-gray-300"
                    :data-testid="`row-${family.platform}-${segment.key}-${row.account.id}`"
                  >
                    <td class="py-2 pr-2 tabular-nums text-gray-500 dark:text-gray-400">#{{ row.account.id }}</td>
                    <td class="py-2 pr-2">{{ row.account.name }}</td>
                    <td class="py-2 pr-2 tabular-nums">{{ row.account.priority }}</td>
                    <td class="py-2 pr-2">
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
                    <td class="py-2 pr-2">{{ row.groupNames.join('、') }}</td>
                    <td class="py-2">
                      <span
                        class="inline-flex items-center"
                        :title="isSwitchDisabled(row.account) ? t('admin.largeRequestRouting.lastPoolGuard') : ''"
                      >
                        <input
                          type="checkbox"
                          class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
                          :checked="isLargeRequestOnly(row.account)"
                          :disabled="isSwitchDisabled(row.account)"
                          :data-testid="`lro-toggle-${row.account.id}`"
                          @change="onToggle(row.account, $event)"
                        />
                      </span>
                    </td>
                  </tr>
                  <tr v-if="(rowsByFamily[family.platform][segment.key] || []).length === 0">
                    <td colspan="6" class="py-4 text-center text-xs text-gray-400 dark:text-gray-500">
                      {{ t('admin.largeRequestRouting.noAccounts') }}
                    </td>
                  </tr>
                </tbody>
              </table>
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

const FAMILIES: Array<{ platform: 'deepseek' | 'zhipu' | 'kimi'; title: string }> = [
  { platform: 'deepseek', title: 'DeepSeek' },
  { platform: 'zhipu', title: 'GLM' },
  { platform: 'kimi', title: 'Kimi' }
]

const SEGMENTS: Array<{ key: 'standard' | 'subscription'; label: string }> = [
  { key: 'standard', label: t('admin.largeRequestRouting.meteredGroups') },
  { key: 'subscription', label: t('admin.largeRequestRouting.subscriptionGroups') }
]

const ACCOUNT_PAGE_SIZE = 200

interface SegmentRow {
  account: AccountListItem
  groupNames: string[]
}

const loading = ref(false)
const errorMessage = ref('')
const accounts = ref<AccountListItem[]>([])
const groups = ref<AdminGroup[]>([])

const groupsByID = computed(() => new Map(groups.value.map((group) => [group.id, group])))

const SUPPORTED_FAMILIES = new Set(['deepseek', 'zhipu', 'kimi'])

function isSupportedPlatform(platform: GroupPlatform): platform is 'deepseek' | 'zhipu' | 'kimi' {
  return SUPPORTED_FAMILIES.has(platform)
}

/**
 * 账号按 group_ids 落入对应家族的对应段。同一账号挂多段（如同时计量组与订阅组）
 * 时每段各显示一行；同一账号在同一段挂多个分组时合并为一行并列出全部组名。
 */
const rowsByFamily = computed(() => {
  const result: Record<
    'deepseek' | 'zhipu' | 'kimi',
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
  for (const family of Object.keys(result) as Array<'deepseek' | 'zhipu' | 'kimi'>) {
    for (const segment of Object.keys(result[family]) as Array<'standard' | 'subscription'>) {
      result[family][segment].sort((a, b) => a.account.id - b.account.id)
    }
  }
  return result
})

function isLargeRequestOnly(account: AccountListItem): boolean {
  return account.extra?.large_request_only === true
}

/**
 * 池内最后一个禁用：某账号所属任一分组的同 platform 且 schedulable && status==='active'
 * 的账号中，除本账号外没有任何 non-large-request-only 账号 → 不可开启（勾选框禁用）。
 * 账号已启用时始终允许取消。
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
    const hasNonLargeRequestOnly = others.some((a) => a.extra?.large_request_only !== true)
    if (!hasNonLargeRequestOnly) {
      return true
    }
  }
  return false
}

function isSwitchDisabled(account: AccountListItem): boolean {
  // 已启用时始终允许取消
  if (isLargeRequestOnly(account)) {
    return false
  }
  return isPoolLast(account)
}

function statusClass(status: string): string {
  switch (status) {
    case 'active':
      return 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
    case 'inactive':
      return 'bg-gray-100 text-gray-500 dark:bg-dark-700 dark:text-gray-400'
    default:
      return 'bg-red-50 text-red-700 dark:bg-red-900/30 dark:text-red-400'
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
  if (e.code === 'LARGE_REQUEST_ONLY_POOL_EXHAUSTED') {
    return e.message || e.reason || t('admin.largeRequestRouting.poolExhaustedFallback')
  }
  return e.message || e.reason || e.error || String(err || '')
}

function setLocalLargeRequestOnly(accountID: number, value: boolean): void {
  const target = accounts.value.find((a) => a.id === accountID)
  if (target) {
    target.extra = { ...(target.extra ?? {}), large_request_only: value }
  }
}

async function onToggle(account: AccountListItem, event: Event): Promise<void> {
  const checked = (event.target as HTMLInputElement).checked
  const previous = isLargeRequestOnly(account)
  if (checked === previous) {
    return
  }
  errorMessage.value = ''
  // 乐观更新，成功后与后端返回值保持一致；失败回滚
  setLocalLargeRequestOnly(account.id, checked)
  try {
    const updated = await adminAPI.accounts.setAccountLargeRequestOnly(account.id, checked)
    if (updated?.extra && typeof updated.extra.large_request_only === 'boolean') {
      setLocalLargeRequestOnly(account.id, updated.extra.large_request_only)
    }
  } catch (err) {
    setLocalLargeRequestOnly(account.id, previous)
    errorMessage.value = extractError(err)
  }
}

async function loadAccountsForPlatform(platform: 'deepseek' | 'zhipu' | 'kimi'): Promise<AccountListItem[]> {
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
    const [groupList, deepseekAccounts, zhipuAccounts, kimiAccounts] = await Promise.all([
      adminAPI.groups.getAllIncludingInactive(),
      loadAccountsForPlatform('deepseek'),
      loadAccountsForPlatform('zhipu'),
      loadAccountsForPlatform('kimi')
    ])
    groups.value = groupList
    accounts.value = [...deepseekAccounts, ...zhipuAccounts, ...kimiAccounts]
  } catch (err) {
    errorMessage.value = extractError(err)
  } finally {
    loading.value = false
  }
})
</script>