<template>
  <div
    v-if="visible"
    data-test="cn-provider-quota"
    class="min-w-[220px] space-y-1"
  >
    <!-- Tier rows: 5h + weekly utilization bars (snapshot renders on mount).
         复用账号页 UsageProgressBar：同阈值配色、同倒计时格式。 -->
    <div v-if="data?.success && data.tiers?.length" class="space-y-1">
      <UsageProgressBar
        v-for="tier in data.tiers"
        :key="tier.window"
        data-test="cn-provider-quota-tier"
        :label="windowLabel(tier.window)"
        :color="tier.window === 'weekly' ? 'emerald' : 'indigo'"
        :utilization="tier.used_percent ?? 0"
        :unknown-usage="tier.used_percent == null"
        :resets-at="tier.reset_at"
      />
    </div>

    <!-- Explicit refresh action (aligned with the OpenAI "Query" / Grok "Probe"
         buttons): a verb label tells users this chip is clickable. The previous
         noun label ("5h/weekly") read as a passive caption and users could not
         discover the manual refresh. -->
    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        data-test="cn-provider-quota-probe"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="loading"
        :title="t('admin.accounts.cnProviders.probeTooltip')"
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

<script lang="ts">
// 模块级共享状态（非 setup 块）：列表页每行一个组件实例，翻页/筛选/刷新会
// 重复挂载各自副本；lastAutoProbeAt 放在模块作用域才能跨实例去重，避免对
// 上游形成探测风暴。若放在 <script setup> 内则每实例独立，去抖失效。
const lastAutoProbeAt = new Map<number, number>()
</script>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { CNProviderQuotaProbeResult } from '@/api/admin/cnProviders'
import type { Account } from '@/types'
import { cnQuotaCellVisible, cnQuotaProviderPrefix, resolveAccountBaseURL, isVolcanoBaseURL } from './credentialsBuilder'
import UsageProgressBar from './UsageProgressBar.vue'

const props = defineProps<{
  account: Account
}>()

const { t } = useI18n()

const readMode = (): string => {
  const mode = props.account.credentials?.account_mode
  return typeof mode === 'string' ? mode : ''
}

// 火山优先：base_url 命中 ark.cn-beijing.volces.com 时强制返回火山 base（而非
// 自适应账号的 api_base_urls，后者可能指向 kimi），与后端 resolveCNQuotaProvider
// 的火山优先识别保持一致——否则火山订阅号被误判为 Kimi，去读 kimi_* 快照而拿不到
// volcano_* 快照（5h/周/月档全空或显示旧值）。
const readBaseURL = (): string => {
  const raw = (props.account.credentials?.base_url as string) || ''
  if (isVolcanoBaseURL(raw)) return raw
  return resolveAccountBaseURL(props.account.credentials)
}

// 快照键前缀（kimi/zhipu 平台即供应商；火山 = base_url 命中 volces，账号仍存为 deepseek 平台）。
const providerPrefix = computed(() => cnQuotaProviderPrefix(props.account.platform, readBaseURL()))

const visible = computed(() => cnQuotaCellVisible(props.account.platform, readMode(), readBaseURL()))

const loading = ref(false)
const error = ref<string | null>(null)
const data = ref<CNProviderQuotaProbeResult | null>(null)

// 后端周期任务/手动探测写入的 extra 快照键（<provider>_ 前缀，与后端
// cnQuotaExtraUpdates 对齐）。页面加载即有数据，无需等待探测。
const SNAPSHOT_STALE_MS = 15 * 60 * 1000

// 自动探测去抖窗口与最近一次自动探测时间（Map 定义于上方模块级 <script>，
// 跨实例共享；此处仅保留常量）。
const AUTO_PROBE_DEBOUNCE_MS = 5 * 60 * 1000

const readExtraNumber = (key: string): number | null => {
  const v = (props.account.extra as Record<string, unknown> | undefined)?.[key]
  return typeof v === 'number' && Number.isFinite(v) ? v : null
}

const readExtraString = (key: string): string => {
  const v = (props.account.extra as Record<string, unknown> | undefined)?.[key]
  return typeof v === 'string' ? v : ''
}

// 从持久化快照构造展示数据。5h 档在 used 或 reset_at 存在即渲染
// （用量上游不可得 → used 为 null 显示“—”，但倒计时仍显示）；周/月档在 reset_at 存在即渲染
// （用量上游不可得 → used 为 null 显示“未知”），不再写假 0 诱出渲染以免误导“未用”。
const snapshotData = computed<CNProviderQuotaProbeResult | null>(() => {
  const prefix = providerPrefix.value
  const used5h = readExtraNumber(`${prefix}_5h_used_percent`)
  const usedWeekly = readExtraNumber(`${prefix}_weekly_used_percent`)
  const usedMonthly = readExtraNumber(`${prefix}_monthly_used_percent`)
  const reset5h = readExtraString(`${prefix}_5h_reset_at`)
  const resetWeekly = readExtraString(`${prefix}_weekly_reset_at`)
  const resetMonthly = readExtraString(`${prefix}_monthly_reset_at`)
  if (used5h == null && reset5h == '' && resetWeekly == '' && resetMonthly == '') return null
  const tiers: CNProviderQuotaProbeResult['tiers'] = []
  if (used5h != null || reset5h != '') {
    tiers.push({ window: '5h', used_percent: used5h, reset_at: reset5h || undefined })
  }
  if (resetWeekly != '') {
    tiers.push({ window: 'weekly', used_percent: usedWeekly, reset_at: resetWeekly })
  }
  if (resetMonthly != '') {
    tiers.push({ window: 'monthly', used_percent: usedMonthly, reset_at: resetMonthly })
  }
  return { success: true, tiers } as CNProviderQuotaProbeResult
})

// 快照是否过期（无更新时间或超过 staleness 窗口）→ 挂载时需要自动探测。
const snapshotIsStale = computed(() => {
  const updatedAt = readExtraString(`${providerPrefix.value}_usage_updated_at`)
  if (!updatedAt) return true
  const ts = new Date(updatedAt).getTime()
  return Number.isNaN(ts) || Date.now() - ts > SNAPSHOT_STALE_MS
})

// 挂载时：先用持久化快照渲染；快照缺失或过期再自动探测一次（失败显示错误，
// 避免静默失败导致单元格空白无提示）。
onMounted(() => {
  if (!visible.value) return
  data.value = snapshotData.value
  if (!snapshotIsStale.value) return
  // 模块级去抖：列表页每行一个实例，翻页/筛选/刷新会重复挂载；同一账号
  // 短时间内已自动探测过则跳过，避免对上游形成探测风暴。
  const last = lastAutoProbeAt.get(props.account.id) ?? 0
  if (Date.now() - last < AUTO_PROBE_DEBOUNCE_MS) return
  lastAutoProbeAt.set(props.account.id, Date.now())
  handleProbe()
})

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

const windowLabel = (window: string) => {
  if (window === 'weekly') return t('admin.accounts.cnProviders.windowWeekly')
  if (window === 'monthly') return t('admin.accounts.cnProviders.windowMonthly')
  return t('admin.accounts.cnProviders.window5h')
}

const handleProbe = async () => {
  if (loading.value) return
  loading.value = true
  error.value = null
  try {
    const result = await adminAPI.cnProviders.queryQuota(props.account.id)
    // 失败时保留已渲染的快照条形图（仅显示错误行），成功才覆盖。
    if (result.success) {
      data.value = result
    } else {
      error.value = result.error || t('common.error')
    }
  } catch (e) {
    error.value = extractErrorMessage(e)
  } finally {
    loading.value = false
  }
}

watch(
  () => props.account.id,
  () => {
    data.value = null
    error.value = null
    loading.value = false
  }
)
</script>
