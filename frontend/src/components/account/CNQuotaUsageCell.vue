<template>
  <div
    v-if="visible"
    data-test="cn-quota-usage"
    class="min-w-[200px] space-y-1"
  >
    <!-- ============ L5 显示互斥 ============
         TH 有 Pass（th_pass_snapshot.has_pass=true）→ 只渲染订阅用量窗口；
         TH 无 Pass → 只渲染免费用量窗口（7 天滚动，仅官方已用计数）；
         Kira → 只渲染免费用量窗口（当日已用/上限 + reset_at 倒计时 + VND 余额）。
         三条路径 v-if/v-else 互斥，不混算（L1）。不做 $ 换算/剩余估算/重置卡（L6）。 -->

    <!-- TH 订阅链：Pass 档位 + 到期倒计时 + 本期已用（today/7d/30d 切换）+ 状态 -->
    <div v-if="showPassWindow" data-test="cn-quota-pass-window" class="space-y-1">
      <div class="flex flex-wrap items-center gap-1.5">
        <span
          data-test="cn-quota-pass-name"
          class="rounded bg-indigo-100 px-1 py-0.5 text-[10px] font-medium text-indigo-700 dark:bg-indigo-900/40 dark:text-indigo-300"
        >
          {{ passName }}
        </span>
        <span
          v-if="renewsAt"
          data-test="cn-quota-pass-renews"
          class="text-[10px] text-gray-400 dark:text-gray-500"
          :title="renewsAtFull"
        >
          {{ renewsLabel }}
        </span>
        <span
          data-test="cn-quota-status"
          :class="[
            'inline-flex items-center rounded px-1 py-0.5 text-[10px] font-medium',
            exhausted
              ? 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
              : 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
          ]"
        >
          {{ exhausted ? t('admin.accounts.cnProviders.statusExhausted') : t('admin.accounts.cnProviders.statusNormal') }}
        </span>
        <span
          v-if="exhausted && recoveryAt"
          data-test="cn-quota-recovery"
          class="text-[10px] text-red-600 dark:text-red-400"
          :title="recoveryAtFull"
        >
          {{ recoveryLabel }}
        </span>
      </div>

      <!-- 本期已用窗口切换（today/7d/30d），官方 CSV 聚合计数，无分母（L6） -->
      <div class="flex flex-wrap items-center gap-1">
        <button
          v-for="w in WINDOWS"
          :key="w"
          type="button"
          data-test="cn-quota-window-toggle"
          :class="[
            'rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 transition-colors',
            activeWindow === w
              ? 'bg-indigo-600 text-white dark:bg-indigo-500'
              : 'bg-gray-100 text-gray-600 hover:bg-gray-200 dark:bg-dark-800 dark:text-gray-300 dark:hover:bg-dark-700'
          ]"
          @click="activeWindow = w"
        >
          {{ windowToggleLabel(w) }}
        </button>
        <span
          data-test="cn-quota-window-stats"
          class="text-[10px] leading-4 text-gray-500 dark:text-gray-400"
        >
          {{ activeWindowStatsLabel }}
        </span>
      </div>
    </div>

    <!-- TH 免费链：7 天滚动计数，只显示已用（无官方分母，L6/L7） -->
    <div v-else-if="isTokenHarbor" data-test="cn-quota-free-window" class="space-y-1">
      <div
        data-test="cn-quota-free-7d"
        class="text-[10px] leading-4 text-gray-500 dark:text-gray-400"
      >
        {{ free7dLabel }}
      </div>
    </div>

    <!-- Kira 免费链：当日已用/上限进度条 + reset_at 倒计时 + VND 余额 -->
    <div v-else data-test="cn-quota-kira-window" class="space-y-1">
      <UsageProgressBar
        data-test="cn-quota-kira-bar"
        :label="t('admin.accounts.cnProviders.kiraDailyLabel')"
        :utilization="kiraUtilization"
        :unknown-usage="kiraUsedPercent == null"
        :resets-at="kiraResetAt || null"
        color="emerald"
      />
      <div
        v-if="kiraUsedTokens != null && kiraLimitTokens != null"
        data-test="cn-quota-kira-used-limit"
        class="text-[10px] leading-4 text-gray-500 dark:text-gray-400"
      >
        {{ t('admin.accounts.cnProviders.kiraUsedLimit', { used: kiraUsedTokens, limit: kiraLimitTokens }) }}
      </div>
      <div
        v-if="vndBalance != null"
        data-test="cn-quota-kira-vnd"
        class="text-[10px] font-medium leading-4 text-emerald-600 dark:text-emerald-300"
      >
        {{ t('admin.accounts.cnProviders.kiraVndBalance', { balance: formatVnd(vndBalance) }) }}
      </div>
    </div>

    <!-- 手动查询按钮：走既有管理端 quota 探测接口，请求中禁用 -->
    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        data-test="cn-quota-probe"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="loading"
        :title="t('admin.accounts.cnProviders.probeTooltip')"
        @click="handleProbe"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': loading }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.cnProviders.probe') }}
      </button>
    </div>

    <div
      v-if="error"
      class="truncate text-[10px] leading-4 text-red-600 dark:text-red-400"
      :title="error"
    >
      {{ truncatedError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type {
  CNQuotaTier,
  KiraUsageSnapshot,
  THPassSnapshot,
  THUsageSnapshot,
  THUsageWindowStats
} from '@/api/admin/cnProviders'
import type { Account } from '@/types'
import { formatCompactNumber, formatCountdown } from '@/utils/format'
import { resolveAccountBaseURL } from './credentialsBuilder'
import UsageProgressBar from './UsageProgressBar.vue'

const props = defineProps<{
  account: Account
}>()

const { t } = useI18n()

// ===== 上游识别（base_url 是唯一事实源，与后端判定对齐）=====
// TH：credentials.base_url 含 tokenharbor.ai（与 isTokenHarborAccount 同口径）；
// Kira：大小写不敏感包含 kiraai.vn（与后端 isKiraBaseURL 同口径）。

const baseHasTokenHarbor = (account: Account): boolean =>
  resolveAccountBaseURL(account.credentials).toLowerCase().includes('tokenharbor.ai')

const baseHasKira = (account: Account): boolean =>
  resolveAccountBaseURL(account.credentials).toLowerCase().includes('kiraai.vn')

const isTokenHarbor = computed(() => baseHasTokenHarbor(props.account))
const isKira = computed(() => baseHasKira(props.account))
const visible = computed(() => isTokenHarbor.value || isKira.value)

// ===== extra 快照解析（照 TokenHarborPassSnapshotFromExtra 的 JSON 兼容口径）=====

const readExtraObject = (key: string): Record<string, unknown> | null => {
  const raw = (props.account.extra as Record<string, unknown> | undefined)?.[key]
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null
  return raw as Record<string, unknown>
}

const passSnapshot = computed<THPassSnapshot | null>(() => {
  const raw = readExtraObject('th_pass_snapshot')
  if (!raw || typeof raw.has_pass !== 'boolean') return null
  return raw as unknown as THPassSnapshot
})

const usageSnapshot = computed<THUsageSnapshot | null>(() => {
  const raw = readExtraObject('th_usage_snapshot')
  if (!raw || !raw.windows || typeof raw.windows !== 'object') return null
  return raw as unknown as THUsageSnapshot
})

const kiraSnapshot = computed<KiraUsageSnapshot | null>(() => {
  const raw = readExtraObject('kira_usage_snapshot')
  if (!raw) return null
  return raw as unknown as KiraUsageSnapshot
})

// 状态机状态（extra 键 cn_quota_lifecycle：state/recovery_at/...，§4.1）。
const lifecycle = computed<{ state?: string; recovery_at?: string } | null>(() => {
  const raw = readExtraObject('cn_quota_lifecycle')
  if (!raw) return null
  return raw as { state?: string; recovery_at?: string }
})

// ===== TH 订阅链（has_pass=true）=====

// L5：有订阅快照（has_pass=true）才渲染订阅窗口；否则只渲染免费窗口。
const showPassWindow = computed(() => isTokenHarbor.value && passSnapshot.value?.has_pass === true)

const passName = computed(() => passSnapshot.value?.pass_name?.trim() || t('admin.accounts.cnProviders.passWindow'))

const parseTime = (raw: string | undefined): Date | null => {
  if (!raw) return null
  const d = new Date(raw)
  return Number.isNaN(d.getTime()) ? null : d
}

const renewsAt = computed(() => parseTime(passSnapshot.value?.renews_at))
const renewsLabel = computed(() => {
  if (!renewsAt.value) return ''
  const countdown = formatCountdown(renewsAt.value)
  const time = countdown || renewsAt.value.toLocaleString()
  return t('admin.accounts.cnProviders.passRenewsAt', { time })
})
const renewsAtFull = computed(() => renewsAt.value?.toLocaleString() ?? '')

const exhausted = computed(() => lifecycle.value?.state === 'exhausted')
const recoveryAt = computed(() => parseTime(lifecycle.value?.recovery_at))
const recoveryLabel = computed(() => {
  if (!recoveryAt.value) return ''
  const countdown = formatCountdown(recoveryAt.value)
  const time = countdown || recoveryAt.value.toLocaleString()
  return t('admin.accounts.cnProviders.recoverAt', { time })
})
const recoveryAtFull = computed(() => recoveryAt.value?.toLocaleString() ?? '')

// 本期已用窗口切换（today/7d/30d）。
const WINDOWS = ['today', '7d', '30d'] as const
type UsageWindow = (typeof WINDOWS)[number]
const activeWindow = ref<UsageWindow>('today')

const windowToggleLabel = (w: UsageWindow): string => {
  if (w === 'today') return t('admin.accounts.cnProviders.windowToday')
  if (w === '7d') return t('admin.accounts.cnProviders.window7d')
  return t('admin.accounts.cnProviders.window30d')
}

const activeWindowStats = computed<THUsageWindowStats | null>(() => {
  const windows = usageSnapshot.value?.windows as Record<string, THUsageWindowStats> | undefined
  const stats = windows?.[activeWindow.value]
  if (!stats || typeof stats !== 'object') return null
  return stats
})

const activeWindowStatsLabel = computed(() => {
  const stats = activeWindowStats.value
  const requests = typeof stats?.requests === 'number' ? stats.requests : 0
  const tokensIn = typeof stats?.tokens_in === 'number' ? stats.tokens_in : 0
  const tokensOut = typeof stats?.tokens_out === 'number' ? stats.tokens_out : 0
  const tokens = tokensIn + tokensOut
  return t('admin.accounts.cnProviders.windowStats', {
    requests: formatCompactNumber(requests, { allowBillions: false }),
    tokens: formatCompactNumber(tokens)
  })
})

// ===== TH 免费链（无 Pass）：7 天滚动计数，只显示已用 =====

const free7dLabel = computed(() => {
  const windows = usageSnapshot.value?.windows as Record<string, THUsageWindowStats> | undefined
  const stats = windows?.['7d']
  const requests = typeof stats?.requests === 'number' ? stats.requests : 0
  const tokensIn = typeof stats?.tokens_in === 'number' ? stats.tokens_in : 0
  const tokensOut = typeof stats?.tokens_out === 'number' ? stats.tokens_out : 0
  return t('admin.accounts.cnProviders.freeRolling7d', {
    requests: formatCompactNumber(requests, { allowBillions: false }),
    tokens: formatCompactNumber(tokensIn + tokensOut)
  })
})

// ===== Kira 免费链 =====

// 手动探测成功后优先用探测返回的 daily 档（后端探测会同时刷新快照，父级账号
// 列表刷新前也能立即反映最新值）；否则用落库快照。
const probedDailyTier = ref<CNQuotaTier | null>(null)

const kiraUsedPercent = computed<number | null>(() => {
  if (probedDailyTier.value && probedDailyTier.value.used_percent != null) {
    return probedDailyTier.value.used_percent
  }
  const v = kiraSnapshot.value?.used_percent
  return typeof v === 'number' ? v : null
})

const kiraUtilization = computed(() => kiraUsedPercent.value ?? 0)

const extraNumber = (v: unknown): number | null => (typeof v === 'number' && Number.isFinite(v) ? v : null)

const kiraUsedTokens = computed(() => {
  if (probedDailyTier.value) return null
  return extraNumber(kiraSnapshot.value?.used_tokens)
})
const kiraLimitTokens = computed(() => {
  if (probedDailyTier.value) return null
  return extraNumber(kiraSnapshot.value?.limit_tokens)
})

const kiraResetAt = computed(() => {
  const raw = probedDailyTier.value?.reset_at || kiraSnapshot.value?.reset_at
  return typeof raw === 'string' ? raw.trim() : ''
})

// VND 余额：Kira 余额探测写 <platform>_balance（cn_provider_kira.go cnExtraKey）。
const vndBalance = computed<number | null>(() => {
  const v = (props.account.extra as Record<string, unknown> | undefined)?.[
    `${props.account.platform}_balance`
  ]
  return extraNumber(v)
})

const formatVnd = (value: number): string => (value >= 100 ? value.toFixed(0) : value.toFixed(2))

// ===== 手动查询（既有管理端 quota 探测接口）=====

const loading = ref(false)
const error = ref<string | null>(null)

const extractErrorMessage = (e: unknown): string => {
  const err = e as {
    message?: string
    reason?: string
    response?: { data?: { message?: string; error?: string } }
  }
  return (
    err?.message ||
    err?.reason ||
    err?.response?.data?.message ||
    err?.response?.data?.error ||
    t('common.error')
  )
}

const truncatedError = computed(() => {
  if (!error.value) return ''
  return error.value.length > 80 ? `${error.value.slice(0, 80)}...` : error.value
})

const handleProbe = async () => {
  if (loading.value) return
  loading.value = true
  error.value = null
  try {
    const result = await adminAPI.cnProviders.queryQuota(props.account.id)
    if (result.success) {
      // Kira：探测结果含 daily 档，立即反映；TH：快照由后端探测落库，
      // 父级列表刷新后经 props 更新。失败保留快照展示，仅显示错误行。
      const daily = result.tiers?.find((tier) => tier.window === 'daily')
      probedDailyTier.value = daily ?? null
    } else {
      error.value = result.error || t('common.error')
    }
  } catch (e) {
    error.value = extractErrorMessage(e)
  } finally {
    loading.value = false
  }
}
</script>
