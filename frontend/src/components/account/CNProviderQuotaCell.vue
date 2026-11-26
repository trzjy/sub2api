<template>
  <div
    v-if="visible"
    data-test="cn-provider-quota"
    class="min-w-[220px] space-y-1"
  >
    <!-- 规范化维度状态对齐（方案 §3.4 / R5-F3）：account 级 exhausted 维度 → 耗尽徽标；
         既有 used_percent 取数路径不变。 -->
    <span
      v-if="exhaustedAccountDimension"
      data-test="cn-provider-quota-exhausted"
      class="inline-flex items-center rounded bg-red-100 px-1 py-0.5 text-[10px] font-medium text-red-700 dark:bg-red-900/30 dark:text-red-300"
    >
      {{ t('admin.accounts.cnProviders.statusExhausted') }}
    </span>

    <!-- 阿里 Token Plan 专属展示：档位 + Credits 数字（档位月额度 / 已用 / 剩余）。
         数字全部来自**官方接口**：团队席位版读官方 GetSubscriptionStats（剩余额度直接给）；
         个人版读官方 quota-config（档位与总额度）+ 官方 usage 接口（已用百分比与重置时间，
         该接口只认浏览器登录态）。本地不做任何 Credits 折算。 -->
    <div
      v-if="aliyunSnapshot"
      data-test="cn-provider-quota-aliyun"
      class="space-y-1"
    >
      <div
        class="flex items-center gap-1 text-[10px] leading-4 text-gray-600 dark:text-gray-300"
        data-test="cn-provider-quota-aliyun-detail"
      >
        <span v-if="aliyunSnapshot.plan_name" class="font-medium">
          {{ aliyunPlanLabel(aliyunSnapshot.plan_name) }}
        </span>
        <!-- 官方给的月额度总量（quota-config 的 monthly / 席位版的 SeatCredits）。 -->
        <span v-if="aliyunSnapshot.monthly_credit > 0">
          {{ t('admin.accounts.cnProviders.aliyunOfficialQuota') }}
          {{ formatCredits(aliyunSnapshot.monthly_credit) }} Credits
        </span>
        <!--
          已用百分比：官方 usage 接口的 per{N}MonthPercentage 原值（个人版）
          或官方席位额度两个数相除（团队版）。这是**官方口径**，不含本地折算。
          Credits 数只作参考 —— 个人版那个数是「官方百分比 × 官方档位上限」折算出来的
          （官方不直接给 Credits 数），官方百分比才是原值。
        -->
        <span v-if="aliyunSnapshot.used_percent > 0">
          {{ t('admin.accounts.cnProviders.aliyunUsedPercent') }}
          {{ aliyunSnapshot.used_percent.toFixed(1) }}%
        </span>
        <span v-if="aliyunSnapshot.used_credit > 0 && aliyunSnapshot.used_percent > 0" class="text-gray-400 dark:text-gray-500">
          (≈{{ formatCredits(aliyunSnapshot.used_credit) }} / {{ formatCredits(aliyunSnapshot.monthly_credit) }})
        </span>
      </div>
      <UsageProgressBar
        data-test="cn-provider-quota-aliyun-bar"
        :label="t('admin.accounts.cnProviders.windowMonthly')"
        color="indigo"
        :utilization="aliyunSnapshot.used_percent"
        :unknown-usage="false"
        :resets-at="aliyunSnapshot.reset_at"
      />
      <!-- 同一份订阅被拆成多个账号时，用量按整个额度池合计（避免每个账号各显示 1/3）。 -->
      <div
        v-if="(aliyunSnapshot.pool_account_count ?? 0) > 1"
        class="text-[10px] leading-4 text-gray-500 dark:text-gray-400"
        data-test="cn-provider-quota-aliyun-pool"
      >
        {{ t('admin.accounts.cnProviders.aliyunSharedPool', { count: aliyunSnapshot.pool_account_count }) }}
      </div>
    </div>

    <!-- Tier rows: 5h + weekly utilization bars (snapshot renders on mount).
         复用账号页 UsageProgressBar：同阈值配色、同倒计时格式。
         单轨口径（方案 §3.4 / 派发单 UNIFY-QUOTA-UI-20261010）：每档进度条可见性 =
         对应 coding plan 免费维度（quota_dimensions source={provider}_usage_updated_at,
         kind=free, scope=account）status !== 'unknown'。后端未填 target，按 resolver 稳定
         顺序 [5h, weekly, monthly] 做位置映射（单一事实源仍为 quota_dimensions.status，
         组件内不自行比对 fetched_at）。status=unknown 档不渲染旧百分比；维度缺失（旧 DTO）
         → 降级按既有快照渲染。挂载时 snapshotIsStale 自动探测，刷新后维度转 confirmed 恢复。 -->
    <div v-if="data?.success && data.tiers?.length && providerPrefix !== 'aliyun'" class="space-y-1">
      <template v-for="tier in data.tiers" :key="tier.window">
        <UsageProgressBar
          v-if="tierVisible(tier.window)"
          data-test="cn-provider-quota-tier"
          :label="windowLabel(tier.window)"
          :color="tier.window === 'weekly' ? 'emerald' : 'indigo'"
          :utilization="tier.used_percent ?? 0"
          :unknown-usage="tier.used_percent == null"
          :resets-at="tier.reset_at"
        />
      </template>
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
      {{ errorMessage }}
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
import type { AliyunTokenPlanUsage, CNProviderQuotaProbeResult } from '@/api/admin/cnProviders'
import type { Account, QuotaDimensionItem } from '@/types'
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

// 规范化维度状态对齐（方案 §3.4 / R5-F3）：account 级 exhausted 维度 → 耗尽徽标；
// 本 cell 唯一 exhausted 出口，既有 used_percent 取数路径不变。
const exhaustedAccountDimension = computed<QuotaDimensionItem | undefined>(() =>
  (props.account.quota_dimensions ?? []).find((d) => d.scope === 'account' && d.status === 'exhausted')
)

// 单轨口径（派发单 UNIFY-QUOTA-UI-20261010）：coding plan 滚动窗口维度（source =
// {provider}_usage_updated_at, kind=free, scope=account）。后端为 5h/weekly/monthly 三档
// 共用同一 source 且未填 target，故按 resolver 稳定追加顺序 [5h, weekly, monthly] 做位置映射。
// 单一事实源 = quota_dimensions.status，组件内不得再比对 fetched_at 自行判新鲜。
const codingPlanFreeDimensions = computed<QuotaDimensionItem[]>(() =>
  (props.account.quota_dimensions ?? []).filter(
    (d) => d.kind === 'free' && d.scope === 'account' && d.source === `${providerPrefix.value}_usage_updated_at`
  )
)

const TIER_WINDOW_ORDER = ['5h', 'weekly', 'monthly'] as const
type TierWindow = (typeof TIER_WINDOW_ORDER)[number]

// 每档进度条可见性：对应窗口维度存在且 status !== 'unknown' → 渲染；维度缺失（旧 DTO）→
// 降级按既有快照渲染（不造状态、不报错）。
const tierVisible = (window: string): boolean => {
  const idx = TIER_WINDOW_ORDER.indexOf(window as TierWindow)
  if (idx < 0) return true
  const dim = codingPlanFreeDimensions.value[idx]
  if (!dim) return true
  return dim.status !== 'unknown'
}

const loading = ref(false)
const error = ref<string | null>(null)
// errorCode 是后端给的稳定错误码（阿里分支用），前端按码显示中文提示。
// 文案（error）是给日志/排障看的，界面按码翻译，两者不混用。
const errorCode = ref<string | null>(null)
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

// 阿里 Token Plan 的快照：独立键 aliyun_token_plan_usage_snapshot（后端 service.aliyunQuotaSnapshot 同构）。
// 与其余 provider 的 <prefix>_<window>_used_percent 扁平键不同 —— 阿里要展示的是
// 「档位 / 已用 Credits / 总额度 / 剩余 / 重置时间」，扁平百分比键装不下这些字段。
type AliyunTokenPlanUsageSnapshot = AliyunTokenPlanUsage

const ALIYUN_SNAPSHOT_KEY = 'aliyun_token_plan_usage_snapshot'

// 官方档位名（quota-config 的 key，standard / lite / essential / pro）→ 展示名。
//
// 只在展示层做美化：后端存的是**官方原值**（standard），这里转成「Standard」给人看。
// 未知档位原样透传（官方将来加新档位时前端不会显示成空白）。
const ALIYUN_PLAN_LABELS: Record<string, string> = {
  standard: 'Standard',
  lite: 'Lite',
  essential: 'Essential',
  pro: 'Pro',
  max: 'Max'
}

const aliyunPlanLabel = (planName: string): string => {
  if (!planName) return ''
  return ALIYUN_PLAN_LABELS[planName.toLowerCase()] ?? planName
}

// 探测结果里带回的阿里快照（点「查询」当场生效，不必等列表刷新拿新 extra）。
const probedAliyun = ref<AliyunTokenPlanUsageSnapshot | null>(null)

// 把后端返回的快照规整成前端要用的形状（字段缺失/类型不对就当作没有这份快照）。
const normalizeAliyun = (raw: unknown): AliyunTokenPlanUsageSnapshot | null => {
  if (!raw || typeof raw !== 'object') return null
  const s = raw as Partial<AliyunTokenPlanUsageSnapshot>
  if (typeof s.monthly_credit !== 'number' || typeof s.used_credit !== 'number') return null
  return {
    plan_name: typeof s.plan_name === 'string' ? s.plan_name : '',
    monthly_credit: s.monthly_credit,
    used_credit: s.used_credit,
    remaining_credit: typeof s.remaining_credit === 'number' ? s.remaining_credit : 0,
    used_percent: typeof s.used_percent === 'number' ? s.used_percent : 0,
    five_hour_credit: typeof s.five_hour_credit === 'number' ? s.five_hour_credit : undefined,
    data_source: typeof s.data_source === 'string' ? s.data_source : undefined,
    reset_at: typeof s.reset_at === 'string' ? s.reset_at : undefined,
    exhausted: s.exhausted === true,
    uncovered_models: Array.isArray(s.uncovered_models) ? s.uncovered_models : undefined,
    pool_account_count: typeof s.pool_account_count === 'number' ? s.pool_account_count : undefined,
    fetched_at: typeof s.fetched_at === 'string' ? s.fetched_at : ''
  }
}

const aliyunSnapshot = computed<AliyunTokenPlanUsageSnapshot | null>(() => {
  // 探测结果优先（最新），其次是账号 extra 里持久化的快照。
  if (probedAliyun.value) return probedAliyun.value
  const raw = (props.account.extra as Record<string, unknown> | undefined)?.[ALIYUN_SNAPSHOT_KEY]
  return normalizeAliyun(raw)
})

// 阿里快照是否过期（无抓取时间或超 staleness 窗口）。
const aliyunSnapshotIsStale = computed(() => {
  const snap = aliyunSnapshot.value
  if (!snap || !snap.fetched_at) return true
  const ts = new Date(snap.fetched_at).getTime()
  return Number.isNaN(ts) || Date.now() - ts > SNAPSHOT_STALE_MS
})

// 快照是否过期（无更新时间或超过 staleness 窗口）→ 挂载时需要自动探测。
const snapshotIsStale = computed(() => {
  if (providerPrefix.value === 'aliyun') return aliyunSnapshotIsStale.value
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

// 阿里错误码 → 中文提示（后端给码，这里只做翻译；认不出的码回落原始文案）。
const errorMessage = computed(() => {
  const code = errorCode.value
  if (code) {
    const key = `admin.accounts.cnProviders.aliyunErrors.${code}`
    const translated = t(key)
    if (translated !== key) return translated
  }
  return truncatedError.value
})

const windowLabel = (window: string) => {
  if (window === 'weekly') return t('admin.accounts.cnProviders.windowWeekly')
  if (window === 'monthly') return t('admin.accounts.cnProviders.windowMonthly')
  return t('admin.accounts.cnProviders.window5h')
}

// Credits 数字格式化：整数直接显示，小数保留 1 位（千分位）。
const formatCredits = (value: number): string => {
  const n = Number.isFinite(value) ? value : 0
  const rounded = Math.round(n * 10) / 10
  const text = Number.isInteger(rounded) ? String(rounded) : rounded.toFixed(1)
  return text.replace(/\B(?=(\d{3})+(?!\d))/g, ',')
}

const handleProbe = async () => {
  if (loading.value) return
  loading.value = true
  error.value = null
  errorCode.value = null
  try {
    const result = await adminAPI.cnProviders.queryQuota(props.account.id)
    // 失败时保留已渲染的快照条形图（仅显示错误行），成功才覆盖。
    if (result.success) {
      data.value = result
      // 阿里：探测结果带回快照 → 当场显示真实数字（否则得等列表刷新拿新 extra）。
      probedAliyun.value = normalizeAliyun(result.aliyun_usage)
    } else {
      error.value = result.error || t('common.error')
      errorCode.value = result.error_code ?? null
    }
  } catch (e) {
    error.value = extractErrorMessage(e)
    errorCode.value = null
  } finally {
    loading.value = false
  }
}

watch(
  () => props.account.id,
  () => {
    data.value = null
    error.value = null
    errorCode.value = null
    loading.value = false
    // 换账号了，上一个账号的探测快照不能留在这儿（会显示成新账号的用量）。
    probedAliyun.value = null
  }
)
</script>
