<template>
  <div v-if="visible" class="space-y-1">
    <!-- Balance value row: static display (snapshot or probe result) -->
    <div class="flex flex-wrap items-center gap-1.5">
      <span
        data-test="cn-provider-balance-value"
        :class="['text-[10px] font-medium leading-4', platformTextClass(account.platform)]"
        :title="t('admin.accounts.cnProviders.balanceProbeTooltip')"
      >
        <!-- 付费维度状态徽标：Kira = kira_vnd_balance；relay 族 = resolver relay paid 维度
             source={platform}_balance。既有余额金额展示不变。 -->
        <span
          v-if="paidDimension"
          data-test="cn-provider-balance-kira-status"
          :class="dimensionStatusClass(paidDimension.status)"
        >
          {{ dimensionStatusLabel(paidDimension.status) }}
        </span>
        <!-- 单轨口径（方案 §3.4 / 派发单 UNIFY-QUOTA-UI-20261010）：付费维度存在且
             status==='unknown' → 隐藏金额数字、仅保留状态徽标（防同屏旧数值）；
             无维度覆盖（维度列表无匹配项）→ 保持现状渲染（降级兼容，不造状态）。 -->
        <span v-if="balanceAmountVisible">{{ balanceLabel }}</span>
      </span>

      <!-- 不限量账号剩余次数：只读落库快照数字（one-api total_available 为次数，无货币前缀） -->
      <span
        v-if="remainingCount != null"
        data-test="cn-provider-balance-remaining"
        class="text-[10px] leading-4 text-gray-400 dark:text-gray-500"
      >
        {{ t('admin.accounts.cnProviders.remainingCount', { count: remainingCountLabel }) }}
      </span>

      <!-- 订阅有效期（同程序中转 /v1/usage 探测透传的上游 expires_at） -->
      <span
        v-if="expiresLabel"
        data-test="cn-provider-balance-expires"
        class="text-[10px] leading-4"
        :class="expiresSoon ? 'font-medium text-red-600 dark:text-red-400' : 'text-gray-400 dark:text-gray-500'"
        :title="expiresTitle"
      >
        {{ expiresLabel }}
      </span>

      <!-- 订阅用量（订阅制账号没有余额数字，月用量是唯一可展示的数值） -->
      <span
        v-if="monthlyUsageLabel"
        data-test="cn-provider-balance-monthly-usage"
        class="text-[10px] leading-4 text-gray-400 dark:text-gray-500"
      >
        {{ monthlyUsageLabel }}
      </span>

      <!-- Low balance badge (reactive 402/429 marker or probe-detected) -->
      <span
        v-if="balanceLow"
        class="inline-flex items-center rounded bg-red-100 px-1 py-0.5 text-[10px] font-medium text-red-700 dark:bg-red-900/30 dark:text-red-300"
      >
        {{ t('admin.accounts.cnProviders.balanceLow') }}
      </span>
    </div>

    <!-- 次卡进度条（kind==='count_pack' 且有分母）：已用/总量按百分比展示。
         耗尽态不另造红色分支——由上方既有 balanceLow 徽标承担。 -->
    <UsageProgressBar
      v-if="kind === 'count_pack' && quotaLimit > 0"
      data-test="cn-count-pack-bar"
      :label="t('admin.accounts.cnProviders.countPack')"
      :utilization="countPackUsedPercent"
      :color="countPackBarColor"
    />

    <!-- Explicit refresh action (aligned with the OpenAI "Query" / Grok "Probe"
         buttons): the old chip doubled as the data display and a hidden click
         target, which users could not discover. The verb label makes the
         affordance explicit. -->
    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        data-test="cn-provider-balance-probe"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="loading"
        :title="t('admin.accounts.cnProviders.balanceProbeTooltip')"
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

    <div v-if="error" class="truncate text-[10px] text-red-600 dark:text-red-400" :title="error">
      {{ truncatedError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { CNProviderBalanceEntry, CNProviderBalanceResult } from '@/api/admin/cnProviders'
import type { Account, QuotaDimensionItem } from '@/types'
import { platformTextClass } from '@/utils/platformColors'
import { cnBalanceCellVisible, resolveAccountBaseURL } from './credentialsBuilder'
import UsageProgressBar from './UsageProgressBar.vue'

const props = defineProps<{
  account: Account
}>()

const { t } = useI18n()

const readMode = (): string => {
  const mode = props.account.credentials?.account_mode
  return typeof mode === 'string' ? mode : ''
}

// 仅 kimi / deepseek payg 账号有公开余额端点；zhipu / minimax 官方无余额端点，但挂在
// 这两个平台下的中转型账号（credentials.balance_probe.enabled）有 /v1/usage，按配置放行。
// 火山订阅号按 base_url 识别并隐藏余额单元格（订阅套餐无公开余额端点，避免与配额单元格
// 双重渲染）。
// capable 由后端投影字段 account.balance_probe_capable 传入（方案乙 / BAL-GATE-D5）：
// 即后端选路事实源 RelayProbeTargetURL 的有效探测能力（enabled 且 URL 可解析/可推导），
// 非 credentials.balance_probe.enabled 配置开关。配错账号（enabled 但无可解析 URL
// 且非官方端点）该字段为 false，整格不出现。
const visible = computed(() => {
  const creds = props.account.credentials
  const capable = props.account.balance_probe_capable === true
  return cnBalanceCellVisible(
    props.account.platform,
    readMode(),
    resolveAccountBaseURL(creds),
    capable,
    props.account.balance_probe?.enabled === true
  )
})

const loading = ref(false)
const error = ref<string | null>(null)
const data = ref<CNProviderBalanceResult | null>(null)

const extraKey = (suffix: string) => `${props.account.platform}_${suffix}`

// 落库快照（后端周期探测/响应式写入 account.Extra）。
const snapshotBalance = computed(() => {
  const v = props.account.extra?.[extraKey('balance')]
  return typeof v === 'number' ? v : null
})
const snapshotCurrency = computed(() => {
  const v = props.account.extra?.[extraKey('balance_currency')]
  return typeof v === 'string' ? v : ''
})
// 多币种快照（后端写 <platform>_balances：[{currency, balance}]，deepseek CNY+USD）。
const snapshotBalances = computed<CNProviderBalanceEntry[]>(() => {
  const v = props.account.extra?.[extraKey('balances')]
  if (!Array.isArray(v)) return []
  return v.flatMap((item): CNProviderBalanceEntry[] => {
    if (!item || typeof item !== 'object') return []
    const { currency, balance } = item as Record<string, unknown>
    if (typeof currency !== 'string' || typeof balance !== 'number') return []
    return [{ currency, balance }]
  })
})
// 余额不足出口：单一事实源为后端 DTO 顶层字段 balance_low（方案 §3.4 / R5-F3 逐字锁定，
// 由 C5 后端卡 dto 层 accountBalanceLowFromExtra 同源产出）。
// 旧链归零：移除前端 extra 直读兜底（<platform>_balance_low），仅消费 DTO 字段。
const balanceLow = computed(() => props.account.balance_low === true)

// 付费余额维度：Kira = source=kira_vnd_balance 维度（paid）；relay 族 = resolver relay paid
// 维度 source={platform}_balance（执行时核对字面量）。既存 kira_vnd_balance 优先；否则匹配本
// 平台余额维度。既有余额金额取数（currentEntries / balanceLabel）路径不变。
const paidDimension = computed<QuotaDimensionItem | undefined>(() => {
  const dims = props.account.quota_dimensions ?? []
  const kira = dims.find((d) => d.source === 'kira_vnd_balance' && d.kind === 'paid')
  if (kira) return kira
  return dims.find((d) => d.kind === 'paid' && d.source === `${props.account.platform}_balance`)
})

// 余额金额可见性：付费维度存在且 status==='unknown' → 隐藏金额、仅保留状态徽标；
// 维度缺失（旧 DTO / 无维度覆盖的平台）→ 降级显示金额（不造状态）。
const balanceAmountVisible = computed(() => {
  const dim = paidDimension.value
  if (!dim) return true
  return dim.status !== 'unknown'
})

const dimensionStatusLabel = (status: QuotaDimensionItem['status']): string => {
  if (status === 'exhausted') return t('admin.accounts.cnProviders.statusExhausted')
  if (status === 'remaining') return t('admin.accounts.cnProviders.dimensionStatusRemaining')
  return t('admin.accounts.cnProviders.dimensionStatusUnknown')
}

const dimensionStatusClass = (status: QuotaDimensionItem['status']): string[] => {
  if (status === 'exhausted') {
    return ['inline-flex', 'items-center', 'rounded', 'bg-red-100', 'px-1', 'py-0.5', 'text-[10px]', 'font-medium', 'text-red-700', 'dark:bg-red-900/30', 'dark:text-red-300']
  }
  if (status === 'remaining') {
    return ['inline-flex', 'items-center', 'rounded', 'bg-emerald-100', 'px-1', 'py-0.5', 'text-[10px]', 'font-medium', 'text-emerald-700', 'dark:bg-emerald-900/30', 'dark:text-emerald-300']
  }
  return ['inline-flex', 'items-center', 'rounded', 'bg-gray-100', 'px-1', 'py-0.5', 'text-[10px]', 'font-medium', 'text-gray-500', 'dark:bg-dark-800', 'dark:text-gray-400']
}

// 同程序中转的订阅制不限量：后端探测到 remaining<0 时置 unlimited 快照，
// 这类账号没有可比的数字余额，展示上游分组名而非「¥ -1」。
const snapshotUnlimited = computed(() => props.account.extra?.[extraKey('balance_unlimited')] === true)
const snapshotPlanName = computed(() => {
  const v = props.account.extra?.[extraKey('balance_plan_name')]
  return typeof v === 'string' ? v.trim() : ''
})
const unlimited = computed(() => (data.value?.success ? data.value.unlimited === true : snapshotUnlimited.value))
const planName = computed(() => {
  if (data.value?.success && data.value.plan_name) return data.value.plan_name
  return snapshotPlanName.value
})

// 计费形态三态（后端契约逐字锁定）：探测结果优先，其次落库快照（<platform>_balance_kind），
// 两者皆缺（旧快照 / 官方原生路径未升级）按 'balance' 回落。
//   subscription = 订阅不限量（无数字余额，展示分组名）
//   count_pack   = 次卡（balance 字段装的是剩余「次数」，不是金额）
//   balance      = 金额余额
const BALANCE_KINDS = ['subscription', 'count_pack', 'balance'] as const
type BalanceKind = (typeof BALANCE_KINDS)[number]
const snapshotKind = computed<BalanceKind | null>(() => {
  const v = props.account.extra?.[extraKey('balance_kind')]
  if (typeof v !== 'string') return null
  return (BALANCE_KINDS as readonly string[]).includes(v) ? (v as BalanceKind) : null
})
const kind = computed<BalanceKind>(() => {
  if (data.value?.success && data.value.kind) return data.value.kind
  return snapshotKind.value ?? 'balance'
})

// 次卡分母 / 分子（单位：次）。后端仅次卡形态写 <platform>_balance_quota_limit /
// _quota_used，非次卡可能缺省——按 0 处理（进度条随之不渲染）。
const snapshotQuotaLimit = computed(() => {
  const v = props.account.extra?.[extraKey('balance_quota_limit')]
  return typeof v === 'number' ? v : null
})
const snapshotQuotaUsed = computed(() => {
  const v = props.account.extra?.[extraKey('balance_quota_used')]
  return typeof v === 'number' ? v : null
})
const quotaLimit = computed(() => {
  if (data.value?.success && typeof data.value.quota_limit === 'number') return data.value.quota_limit
  return snapshotQuotaLimit.value ?? 0
})
const quotaUsed = computed(() => {
  if (data.value?.success && typeof data.value.quota_used === 'number') return data.value.quota_used
  return snapshotQuotaUsed.value ?? 0
})

// 不限量账号的剩余次数徽标：仅在 unlimited 且落库快照 balance 为数字时展示。
// 只读快照、不读探测响应（探测在浏览器内发生在快照写入后，统一走快照避免双源口径）。
const remainingCount = computed(() =>
  unlimited.value && snapshotBalance.value != null ? snapshotBalance.value : null
)
const remainingCountLabel = computed(() => {
  if (remainingCount.value == null) return ''
  return remainingCount.value >= 100 ? remainingCount.value.toFixed(0) : remainingCount.value.toFixed(2)
})

// 订阅有效期：后端探测透传上游 subscription.expires_at（RFC3339 快照），
// 在余额同行以小字展示，临近到期（≤3 天）或已到期时标红。
const snapshotExpiresAt = computed(() => {
  const v = props.account.extra?.[extraKey('balance_expires_at')]
  return typeof v === 'string' ? v.trim() : ''
})
const expiresAt = computed(() => {
  const raw = data.value?.success && data.value.expires_at ? data.value.expires_at : snapshotExpiresAt.value
  if (!raw) return null
  const d = new Date(raw)
  return Number.isNaN(d.getTime()) ? null : d
})
const expiresLabel = computed(() => {
  if (!expiresAt.value) return ''
  const d = expiresAt.value
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  return t('admin.accounts.cnProviders.expiresAt', { date: `${mm}-${dd}` })
})
const expiresSoon = computed(() => {
  if (!expiresAt.value) return false
  return expiresAt.value.getTime() - Date.now() <= 3 * 24 * 60 * 60 * 1000
})
const expiresTitle = computed(() => expiresAt.value?.toLocaleString() ?? '')

// 订阅月用量：探测结果优先，其次落库快照（<platform>_balance_monthly_used）。
const snapshotMonthlyUsage = computed(() => {
  const v = props.account.extra?.[extraKey('balance_monthly_used')]
  return typeof v === 'number' ? v : null
})
const monthlyUsage = computed(() => {
  if (data.value?.success) {
    return typeof data.value.monthly_usage === 'number' ? data.value.monthly_usage : null
  }
  return snapshotMonthlyUsage.value
})
const monthlyUsageLabel = computed(() => {
  if (monthlyUsage.value == null) return ''
  const amount = monthlyUsage.value >= 100 ? monthlyUsage.value.toFixed(0) : monthlyUsage.value.toFixed(2)
  return t('admin.accounts.cnProviders.monthlyUsage', { amount })
})

// 余额数值：探测结果优先，其次落库快照（与 currentEntries 同源取数，只是不分币种）。
// 次卡形态下它是「剩余次数」，金额形态下是「金额」——量纲由 kind 决定。
const balanceValue = computed(() =>
  data.value && data.value.success ? data.value.balance : (snapshotBalance.value ?? 0)
)

// 次数格式化：次卡量纲为「次」，整数化展示（次数不应有小数）。刻意不用
// toLocaleString —— 千分位分隔符随浏览器 locale 漂移，同一账号在不同运营方
// 机器上显示不一致，且断言无法固定。缺省 0（非真实读数）同样显示 "0"。
const countText = (n: number): string => String(Math.round(n))

// 次卡已用百分比（0-100 截断）；分母缺省（非次卡 / 上游未给）时为 0，进度条随之隐藏。
const countPackUsedPercent = computed(() =>
  quotaLimit.value > 0 ? Math.min(100, Math.max(0, (quotaUsed.value / quotaLimit.value) * 100)) : 0
)
// 次卡条颜色：剩余 >20% 用 emerald，否则 amber。不新造红色分支——耗尽态由既有
// balanceLow 徽标承担（避免同屏两个红色出口）。
const countPackBarColor = computed<'emerald' | 'amber'>(() =>
  countPackUsedPercent.value < 80 ? 'emerald' : 'amber'
)

// 优先用探测结果，其次落库快照。多币种返回全部明细，否则主币种单条。
// 次卡形态直接返回空：上游 unit 在次卡下也误标 "USD"（不可信），落回金额路径会被
// formatEntry 加上币种前缀，把「剩余 705 次」渲染成 "USD 705.00"。
const currentEntries = computed<CNProviderBalanceEntry[]>(() => {
  if (kind.value === 'count_pack') return []
  if (data.value && data.value.success) {
    if (data.value.balances && data.value.balances.length > 0) return data.value.balances
    return [{ currency: data.value.currency || '', balance: data.value.balance }]
  }
  if (snapshotBalances.value.length > 0) return snapshotBalances.value
  if (snapshotBalance.value != null) {
    return [{ currency: snapshotCurrency.value, balance: snapshotBalance.value }]
  }
  return []
})

const formatEntry = (entry: CNProviderBalanceEntry): string => {
  const fixed = entry.balance >= 100 ? entry.balance.toFixed(0) : entry.balance.toFixed(2)
  return `${entry.currency || '¥'} ${fixed}`
}

const balanceLabel = computed(() => {
  // 顺序固定：subscription → count_pack → 金额。
  // subscription 兼容既有 unlimited 布尔（旧快照无 kind 时仍走这条路径）。
  if (kind.value === 'subscription' || unlimited.value) {
    return planName.value
      ? t('admin.accounts.cnProviders.unlimitedWithPlan', { plan: planName.value })
      : t('admin.accounts.cnProviders.unlimited')
  }
  if (kind.value === 'count_pack') {
    // 有分母展示「剩余/总量」，无分母只展示剩余次数（上游 quota 缺省的旧快照）。
    return quotaLimit.value > 0
      ? t('admin.accounts.cnProviders.countPackRemaining', {
          remaining: countText(balanceValue.value),
          limit: countText(quotaLimit.value)
        })
      : t('admin.accounts.cnProviders.countPackNoLimit', { count: countText(balanceValue.value) })
  }
  if (currentEntries.value.length === 0) {
    // 修复:此前误引 admin.accounts.grokBalance(实际嵌套在 usageWindow 下),
    // 未命中时渲染原始 key。CN 供应商使用自己的占位键。
    return t('admin.accounts.cnProviders.balance')
  }
  return currentEntries.value.map(formatEntry).join(' · ')
})

// 余额探测「配置/账号类」错误码（用户可行动）→ 本地化文案（派发单 balance-ui-errmap）。
// 瞬时/上游类码（CN_BALANCE_REQUEST_FAILED / REQUEST_BUILD_FAILED / PROBE_RESULT_INVALID /
// CODING_PLAN）刻意不在表内：沿用后端 message 的既有通用渲染。
// 白名单同时充当 i18n key 注入闸：只有表内常量才能拼进 t() 的 key。
const PROBE_ERROR_CODES = [
  'CN_BALANCE_INVALID_CONFIG',
  'CN_BALANCE_NOT_CONFIGURED',
  'CN_BALANCE_TH_NOT_CONFIGURED',
  'CN_BALANCE_NO_APIKEY',
  'CN_BALANCE_NO_ENDPOINT',
  'CN_BALANCE_URL_REJECTED',
  'CN_BALANCE_INVALID_PLATFORM',
  'CN_BALANCE_ACCOUNT_NOT_FOUND'
] as const

// 错误码取数：后端错误信封把稳定码放在 reason（response 包 Response.Reason），拦截器
// 再平铺到错误对象上；response.data.error 为派发单指定的首位读取位（兼容位）。
// 只认白名单内的码——非表内值一律当作无码，交回既有回退链。
const extractProbeErrorCode = (e: unknown): string => {
  const err = e as {
    reason?: unknown
    error?: unknown
    response?: { data?: { error?: unknown; reason?: unknown } }
  }
  const candidates = [err?.response?.data?.error, err?.response?.data?.reason, err?.reason, err?.error]
  for (const candidate of candidates) {
    if (typeof candidate !== 'string') continue
    const code = candidate.trim()
    if ((PROBE_ERROR_CODES as readonly string[]).includes(code)) return code
  }
  return ''
}

const extractErrorMessage = (e: unknown): string => {
  const code = extractProbeErrorCode(e)
  if (code) return t(`admin.accounts.cnProviders.probeErrors.${code}`)
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
    const result = await adminAPI.cnProviders.queryBalance(props.account.id)
    // 失败时保留快照展示（仅显示错误行），成功才覆盖。
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
