<template>
  <div
    v-if="visible"
    data-test="web-console-quota"
    class="min-w-[200px] space-y-1 text-[10px] leading-4"
  >
    <!-- 1. 头部行：套餐名 / 到期日 / 控制台账号标识 -->
    <div
      v-if="snapshot?.plan_name || expireText || snapshot?.account_label"
      class="flex flex-wrap items-center gap-1"
    >
      <span
        v-if="snapshot?.plan_name"
        data-test="wcq-plan-name"
        class="font-medium text-gray-700 dark:text-gray-200"
      >
        {{ snapshot.plan_name }}
      </span>
      <span v-if="expireText" data-test="wcq-expire" class="text-gray-500 dark:text-gray-400">
        到期 {{ expireText }}
      </span>
      <span
        v-if="snapshot?.account_label"
        data-test="wcq-account-label"
        class="text-gray-400 dark:text-gray-500"
      >
        {{ snapshot.account_label }}
      </span>
    </div>

    <!-- 2. 进度条区：compshare/sensenova 按 tiers；longcat 按资源包 -->
    <div v-if="rollingTiers.length" class="space-y-1">
      <div v-for="tier in rollingTiers" :key="tier.window" data-test="wcq-tier">
        <UsageProgressBar
          :label="windowLabel(tier.window)"
          :color="windowColor(tier.window)"
          :utilization="tierUtilization(tier)"
          :resets-at="tier.reset_at"
        />
        <div class="pl-8 text-gray-500 dark:text-gray-400">
          {{ tier.used }} / {{ tier.limit }}
        </div>
      </div>
    </div>

    <div v-if="currentLot" class="space-y-1" data-test="wcq-lot">
      <UsageProgressBar
        label="资源包"
        color="emerald"
        :utilization="lotUtilization(currentLot)"
        :resets-at="currentLot.expire_time"
      />
      <div class="pl-8 text-gray-500 dark:text-gray-400">
        {{ currentLot.remaining_token }} / {{ currentLot.total_token }}
      </div>
    </div>

    <div
      v-for="(lot, idx) in queuedLots"
      :key="`queued-${lot.lot_id ?? idx}`"
      data-test="wcq-queued-lot"
      class="text-gray-500 dark:text-gray-400"
    >
      排队包：+{{ lotRemaining(lot) }}
    </div>

    <!-- 3. 余额/赠金行 -->
    <div
      v-if="provider === 'sensenova' && grantBalanceText"
      data-test="wcq-grant-balance"
      class="flex flex-wrap items-center gap-1 text-gray-600 dark:text-gray-300"
    >
      <span>赠金余额 {{ grantBalanceText }}</span>
      <span v-if="nearestGrantExpiry" class="text-gray-400 dark:text-gray-500">
        最近过期 {{ formatDate(nearestGrantExpiry) }}
      </span>
    </div>

    <div
      v-if="provider === 'longcat' && paygoBalanceText"
      data-test="wcq-paygo-balance"
      class="text-gray-600 dark:text-gray-300"
    >
      按量余额 ¥{{ paygoBalanceText }}
    </div>

    <!-- 4. 状态与操作行 -->
    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        data-test="wcq-probe"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="loading"
        @click="handleProbe()"
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
        刷新
      </button>

      <template v-if="needsInteractiveAuth">
        <span
          data-test="wcq-auth-needed"
          class="inline-flex items-center rounded bg-amber-100 px-1 py-0.5 text-[10px] font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
        >
          需要验证登录
        </span>
        <button
          type="button"
          data-test="wcq-auth"
          class="rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-amber-700 transition-colors hover:bg-amber-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-amber-300 dark:hover:bg-amber-900/30"
          :disabled="authStarting || authPolling"
          @click="handleStartAuth"
        >
          验证登录
        </button>
      </template>

      <span
        v-if="snapshot?.exhausted === true"
        data-test="wcq-exhausted"
        class="inline-flex items-center rounded bg-red-100 px-1 py-0.5 text-[10px] font-medium text-red-700 dark:bg-red-900/30 dark:text-red-300"
        :title="snapshot.until_indefinite ? '等待上游恢复或补额' : undefined"
      >
        额度已用尽
      </span>

      <span
        v-if="snapshot?.plan_match_status === 'unmatched'"
        data-test="wcq-unmatched"
        class="inline-flex items-center rounded bg-red-100 px-1 py-0.5 text-[10px] font-medium text-red-700 dark:bg-red-900/30 dark:text-red-300"
        title="控制台存在多个套餐，当前 API Key 未命中任何套餐"
      >
        未匹配到对应套餐
      </span>

      <span
        v-if="snapshot?.plan_match_status === 'no_active_plan' || snapshot?.subscription_expired"
        data-test="wcq-no-plan"
        class="inline-flex items-center rounded bg-amber-100 px-1 py-0.5 text-[10px] font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
      >
        订阅已到期/无有效套餐
      </span>
    </div>

    <!-- 5. 错误行 -->
    <div
      v-if="errorText"
      data-test="wcq-error"
      class="truncate text-[10px] leading-4 text-red-600 dark:text-red-400"
      :title="errorText"
    >
      {{ truncatedErrorText }}
    </div>
  </div>
</template>

<script lang="ts">
// 模块级共享状态（非 setup 块）：账号列表每行一个实例，翻页/筛选/刷新会重复挂载
// 各自副本；lastProbeAt 必须跨实例共享，5 分钟内对同一账号只自动探测一次，
// 避免对上游形成探测风暴。放在 <script setup> 内会退化为每实例独立、去抖失效。
const lastProbeAt = new Map<number, number>()

export default {}
</script>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import {
  pollWebConsoleAuth,
  probeWebConsoleQuota,
  startWebConsoleAuth,
  type WebConsoleQuotaProbeResult
} from '@/api/admin/cnProviders'
import type { Account, WebConsoleQuotaLot, WebConsoleQuotaSnapshot, WebConsoleQuotaTier } from '@/types'
import UsageProgressBar from './UsageProgressBar.vue'

const props = defineProps<{
  account: Account
}>()

const emit = defineEmits<{ (e: 'updated'): void }>()

const SNAPSHOT_STALE_MS = 15 * 60 * 1000
const AUTO_PROBE_DEBOUNCE_MS = 5 * 60 * 1000
const AUTH_POLL_INTERVAL_MS = 3 * 1000
const AUTH_POLL_MAX_MS = 10 * 60 * 1000

const provider = computed(() => props.account.web_console_quota_provider ?? '')

const visible = computed(() => !!props.account.web_console_quota_provider)

// 刷新/验证登录返回的快照优先于 props，父级未及时回灌时也能立即反映最新状态。
const snapshotOverride = ref<WebConsoleQuotaSnapshot | null>(null)

const snapshot = computed<WebConsoleQuotaSnapshot | undefined>(
  () =>
    snapshotOverride.value ??
    (props.account.extra?.web_console_quota_snapshot as WebConsoleQuotaSnapshot | undefined)
)

const loading = ref(false)
const error = ref<string | null>(null)
const authStarting = ref(false)
const authPolling = ref(false)

let pollTimer: ReturnType<typeof setInterval> | null = null
let pollStartedAt = 0

const stopPolling = () => {
  if (pollTimer !== null) {
    clearInterval(pollTimer)
    pollTimer = null
  }
  authPolling.value = false
}

const extractErrorMessage = (e: unknown): string => {
  const err = e as {
    message?: string
    reason?: string
    response?: { data?: { message?: string; error?: string } }
  }
  return (
    err?.message || err?.reason || err?.response?.data?.message || err?.response?.data?.error || '操作失败'
  )
}

// ---- 数值与格式化 ----

const formatDate = (value?: string | number | null): string => {
  if (value === null || value === undefined || value === '') return ''
  let ms: number
  if (typeof value === 'number') {
    ms = value > 1e12 ? value : value * 1000
  } else {
    const parsed = Date.parse(value)
    if (Number.isNaN(parsed)) return ''
    ms = parsed
  }
  const d = new Date(ms)
  if (Number.isNaN(d.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

const expireText = computed(() => formatDate(snapshot.value?.expire_at ?? null))

const rollingTiers = computed<WebConsoleQuotaTier[]>(() => {
  if (provider.value !== 'compshare' && provider.value !== 'sensenova') return []
  return snapshot.value?.tiers ?? []
})

const currentLot = computed<WebConsoleQuotaLot | undefined>(() => {
  if (provider.value !== 'longcat') return undefined
  return (snapshot.value?.lots ?? []).find((lot) => lot.is_current === true)
})

const queuedLots = computed<WebConsoleQuotaLot[]>(() => {
  if (provider.value !== 'longcat') return []
  return (snapshot.value?.lots ?? []).filter((lot) => lot.is_current !== true)
})

const grantBalanceText = computed(() => {
  const v = snapshot.value?.grant_balance
  if (v === null || v === undefined) return ''
  return String(v)
})

// 快照顶层无 nearest_grant_expiry 字段，取自有该字段的池（sensenova pool-usage）。
const nearestGrantExpiry = computed(() => {
  const pools = snapshot.value?.pools ?? []
  const pool = pools.find((p) => !!p.nearest_grant_expiry)
  return pool?.nearest_grant_expiry ?? ''
})

const paygoBalanceText = computed(() => {
  const cent = snapshot.value?.paygo_balance_cent
  if (cent === null || cent === undefined) return ''
  return (cent / 100).toFixed(2)
})

const windowLabel = (window: string): string => {
  const normalized = window.toLowerCase()
  if (normalized === '5h') return '5h'
  if (normalized === 'week' || normalized === 'weekly') return '周'
  if (normalized === 'month' || normalized === 'monthly') return '月'
  if (normalized === '7d') return '7d'
  return window
}

const windowColor = (window: string): 'indigo' | 'emerald' | 'purple' | 'amber' => {
  const normalized = window.toLowerCase()
  if (normalized === 'week' || normalized === 'weekly' || normalized === '7d') return 'emerald'
  if (normalized === 'month' || normalized === 'monthly') return 'purple'
  return 'indigo'
}

const tierUtilization = (tier: WebConsoleQuotaTier): number => {
  if (!(tier.limit > 0)) return 0
  return Math.min(100, (tier.used / tier.limit) * 100)
}

const lotUtilization = (lot: WebConsoleQuotaLot): number => {
  if (!(lot.total_token > 0)) return 0
  return ((lot.total_token - lot.remaining_token) / lot.total_token) * 100
}

const lotRemaining = (lot: WebConsoleQuotaLot): number => Math.max(0, lot.remaining_token)

// ---- 状态 ----

const needsInteractiveAuth = computed(
  () =>
    snapshot.value?.auth_state === 'needs_interactive_auth' ||
    snapshot.value?.auth_state === 'auth_failed'
)

const errorText = computed(
  () => snapshot.value?.auth_error || error.value || ''
)

const truncatedErrorText = computed(() =>
  errorText.value.length > 80 ? `${errorText.value.slice(0, 80)}...` : errorText.value
)

// ---- 探测 ----

const applyProbeResult = (result?: WebConsoleQuotaProbeResult | null) => {
  if (!result) return
  if (result.snapshot) snapshotOverride.value = result.snapshot
  if (!result.success) {
    error.value = result.error || result.auth_error || '探测失败'
  } else if (!result.error) {
    error.value = null
  }
}

const handleProbe = async () => {
  if (loading.value) return
  loading.value = true
  error.value = null
  try {
    const result = await probeWebConsoleQuota(props.account.id)
    applyProbeResult(result)
    lastProbeAt.set(props.account.id, Date.now())
    emit('updated')
  } catch (e) {
    error.value = extractErrorMessage(e)
  } finally {
    loading.value = false
  }
}

// ---- 交互登录轮询 ----

const startAuthPolling = (sessionId: string) => {
  stopPolling()
  authPolling.value = true
  pollStartedAt = Date.now()
  pollTimer = setInterval(async () => {
    if (Date.now() - pollStartedAt > AUTH_POLL_MAX_MS) {
      stopPolling()
      error.value = '验证登录超时，请重新发起'
      return
    }
    try {
      const result = await pollWebConsoleAuth(props.account.id, sessionId)
      if (result.status === 'succeeded') {
        stopPolling()
        authStarting.value = false
        applyProbeResult(result.probe)
        error.value = null
        emit('updated')
      } else if (result.status === 'failed') {
        stopPolling()
        authStarting.value = false
        error.value = result.message || '验证登录失败'
      }
    } catch (e) {
      // 单次轮询失败不终止：继续等待直到上限，避免瞬时网络抖动中断登录流程。
      error.value = extractErrorMessage(e)
    }
  }, AUTH_POLL_INTERVAL_MS)
}

const handleStartAuth = async () => {
  if (authStarting.value || authPolling.value) return
  authStarting.value = true
  error.value = null
  try {
    const { session_id } = await startWebConsoleAuth(props.account.id)
    startAuthPolling(session_id)
  } catch (e) {
    error.value = extractErrorMessage(e)
    authStarting.value = false
  }
}

// ---- 生命周期 ----

const snapshotIsStale = computed(() => {
  if (!snapshot.value) return true
  const fetchedAt = snapshot.value.fetched_at
  if (!fetchedAt) return true
  const ts = new Date(fetchedAt).getTime()
  return Number.isNaN(ts) || Date.now() - ts > SNAPSHOT_STALE_MS
})

onMounted(() => {
  if (!visible.value) return
  if (!snapshotIsStale.value) return
  const last = lastProbeAt.get(props.account.id) ?? 0
  if (Date.now() - last < AUTO_PROBE_DEBOUNCE_MS) return
  lastProbeAt.set(props.account.id, Date.now())
  handleProbe()
})

onUnmounted(() => {
  stopPolling()
})

// 账号 id 变化：清理轮询与本地态，避免旧会话结果污染新账号。
watch(
  () => props.account.id,
  () => {
    stopPolling()
    authStarting.value = false
    snapshotOverride.value = null
    error.value = null
    loading.value = false
  }
)
</script>
