<template>
  <div
    v-if="visible"
    data-test="cn-quota-usage"
    class="min-w-[200px] space-y-1"
  >
    <!-- ============ L5 显示互斥 ============
         TH 有 Pass（th_pass_snapshot.has_pass=true）→ 只渲染订阅用量窗口；
         TH 无 Pass → 只渲染免费用量窗口（7 天滚动计数 + th free-tier 状态）；
         Kira → 只渲染用量窗口（当日已用/上限 + reset_at 倒计时 + VND 余额）。
         三条路径 v-if/v-else 互斥，不混算（L1）。不做 $ 换算/剩余估算/重置卡（L6）。

         派发单 F4 口径：类别文案（免费/付费/订阅）是多余设计，已整体删除（含底部
         维度面板与其 kind 标签）。数据展示（进度条/金额/状态徽标）自己说明自己；
         quota_dimensions 仍作为状态判定的唯一事实源（三分支可见性继续消费维度
         status 与 observed_at），只是不再渲染类别标签行。 -->

    <!-- TH 订阅链：Pass 档位 + 周期重置倒计时 + 本期已用（today/7d/renew 切换）+ 津贴进度 -->
    <div v-if="showPassWindow" data-test="cn-quota-pass-window" class="space-y-1">
      <div class="flex flex-wrap items-center gap-1.5">
        <span
          data-test="cn-quota-pass-name"
          class="rounded bg-indigo-100 px-1 py-0.5 text-[10px] font-medium text-indigo-700 dark:bg-indigo-900/40 dark:text-indigo-300"
        >
          {{ passName }}
        </span>
        <span
          v-if="resetAt"
          data-test="cn-quota-pass-renews"
          class="text-[10px] text-gray-400 dark:text-gray-500"
          :title="resetAtFull"
        >
          {{ resetAtLabel }}
        </span>
        <!-- 恢复倒计时：订阅维度 status==='exhausted' 驱动（recovery_at 取值路径不变，
             仍读 cn_quota_lifecycle）。 -->
        <span
          v-if="exhausted && recoveryAt"
          data-test="cn-quota-recovery"
          class="text-[10px] text-red-600 dark:text-red-400"
          :title="recoveryAtFull"
        >
          {{ recoveryLabel }}
        </span>
      </div>

      <!-- 本期已用窗口切换（today/7d/renew），官方 CSV 聚合计数，无分母（L6）；
           renew 档为订阅续期倒计时（renews_at），不再显示本地 30 天聚合 -->
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

      <!-- 官方 Pass 津贴进度条（plan_used_pct，0-100 截断；plan_exhausted 沿用已耗尽样式）。
           单轨展示不变式（方案 §3.4 v24）：数值存在即渲染；status=unknown 且数值存在 →
           灰度条 + observed_at 陈旧标注（stale），不再隐藏进度条。 -->
      <UsageProgressBar
        v-if="passPlanBarVisible"
        data-test="cn-quota-pass-plan-bar"
        :label="t('admin.accounts.cnProviders.planLabel')"
        :utilization="planUsedPct ?? 0"
        :unknown-usage="planUsedPct == null"
        :stale="thStale"
        :stale-note="thStaleNote"
        color="indigo"
      />
      <!-- 同一 invariant 的第二分支：从未探测（订阅维度 status=unknown 且 observed_at
           缺失 且 无 plan_used_pct）→「未知」徽标，不假显 0%；维度缺失（旧 DTO）→
           既不渲染进度条也不渲染徽标（不造状态）。 -->
      <div
        v-else-if="passPlanUnknownVisible"
        data-test="cn-quota-pass-plan-unknown"
        class="flex flex-wrap items-center gap-1"
      >
        <span :class="unknownBadgeClass">
          {{ t('admin.accounts.cnProviders.dimensionStatusUnknown') }}
        </span>
      </div>
    </div>

    <!-- TH 免费链：7 天滚动计数 + th free-tier 状态（有值→进度条，无值→「未知」徽标） -->
    <div v-else-if="isTokenHarbor" data-test="cn-quota-free-window" class="space-y-1">
      <div
        data-test="cn-quota-free-7d"
        class="text-[10px] leading-4 text-gray-500 dark:text-gray-400"
      >
        {{ free7dLabel }}
      </div>
      <!-- th free-tier（th_pass_snapshot.used_pct，官方免费津贴已用百分比）：
           三分支判定源 = free 维度（source=th_pass_snapshot, kind=free）status/observed_at。 -->
      <UsageProgressBar
        v-if="thFreeBarVisible"
        data-test="cn-quota-th-free-bar"
        :label="t('admin.accounts.cnProviders.planLabel')"
        :utilization="thFreePct ?? 0"
        :unknown-usage="thFreePct == null"
        :stale="thFreeStale"
        :stale-note="thFreeStaleNote"
        color="indigo"
      />
      <div
        v-else-if="thFreeUnknownVisible"
        data-test="cn-quota-th-free-unknown"
        class="flex flex-wrap items-center gap-1"
      >
        <span :class="unknownBadgeClass">
          {{ t('admin.accounts.cnProviders.dimensionStatusUnknown') }}
        </span>
      </div>
    </div>

    <!-- Kira 用量链：当日已用/上限进度条 + reset_at 倒计时 + VND 余额 -->
    <div v-else data-test="cn-quota-kira-window" class="space-y-1">
      <!-- 当日进度条（used_percent）：数值存在即渲染；free 维度 status=unknown 且数值存在
           → 灰度条 + observed_at 陈旧标注；从未探测（status=unknown 且无 observed_at 且
           无数值）→「未知」徽标，不渲染旧快照百分比。 -->
      <UsageProgressBar
        v-if="kiraFreeVisible"
        data-test="cn-quota-kira-bar"
        :label="t('admin.accounts.cnProviders.kiraDailyLabel')"
        :utilization="kiraUtilization"
        :unknown-usage="kiraUsedPercent == null"
        :stale="kiraStale"
        :stale-note="kiraStaleNote"
        :resets-at="kiraResetAt || null"
        color="emerald"
      />
      <div
        v-else-if="kiraFreeUnknownVisible"
        data-test="cn-quota-kira-free-unknown"
        class="flex flex-wrap items-center gap-1"
      >
        <span :class="unknownBadgeClass">
          {{ t('admin.accounts.cnProviders.dimensionStatusUnknown') }}
        </span>
      </div>
      <div
        v-if="kiraFreeVisible && kiraUsedTokens != null && kiraLimitTokens != null"
        data-test="cn-quota-kira-used-limit"
        class="text-[10px] leading-4 text-gray-500 dark:text-gray-400"
      >
        {{ t('admin.accounts.cnProviders.kiraUsedLimit', { used: kiraUsedTokens, limit: kiraLimitTokens }) }}
      </div>
      <div
        v-if="vndBalance != null"
        data-test="cn-quota-kira-vnd"
        class="flex flex-wrap items-center gap-1 text-[10px] font-medium leading-4 text-emerald-600 dark:text-emerald-300"
      >
        <!-- vndBalance != null 即显示金额；paid 维度 status=unknown 且金额存在 →
             金额旁附陈旧标注（staleNote），不再渲染孤立状态徽标。 -->
        <span>{{ t('admin.accounts.cnProviders.kiraVndBalance', { balance: formatVnd(vndBalance) }) }}</span>
        <span
          v-if="kiraVndStaleNote"
          data-test="cn-quota-kira-vnd-stale"
          class="font-normal text-gray-400 dark:text-gray-500"
        >
          {{ kiraVndStaleNote }}
        </span>
      </div>
      <div
        v-else-if="kiraVndUnknownVisible"
        data-test="cn-quota-kira-vnd-unknown"
        class="flex flex-wrap items-center gap-1"
      >
        <span :class="unknownBadgeClass">
          {{ t('admin.accounts.cnProviders.dimensionStatusUnknown') }}
        </span>
      </div>
    </div>

    <!-- TH 钱包 paid 维度（spend_after_allowance=true 且 th_balance 有值）：钱包余额行。
         无值不渲染（不是状态出口，不造「未知」）。 -->
    <div
      v-if="thWalletVisible"
      data-test="cn-quota-th-wallet"
      class="flex flex-wrap items-center gap-1 text-[10px] font-medium leading-4 text-emerald-600 dark:text-emerald-300"
    >
      <span>{{ thWalletLabel }}</span>
      <span
        v-if="thWalletStaleNote"
        data-test="cn-quota-th-wallet-stale"
        class="font-normal text-gray-400 dark:text-gray-500"
      >
        {{ thWalletStaleNote }}
      </span>
    </div>

    <!-- TH 模型级 free-tier 耗尽逐模型展示：scope=model 且 status=exhausted。
         模型 ID 有信息量，故整块保留（派发单 F4 唯一保留的维度行渲染）。 -->
    <div v-if="exhaustedModelDimensions.length" data-test="cn-quota-model-exhausted" class="space-y-1">
      <div
        v-for="(dim, i) in exhaustedModelDimensions"
        :key="`${dim.target}:${i}`"
        data-test="cn-quota-model-row"
        class="flex flex-wrap items-center gap-1"
      >
        <span
          data-test="cn-quota-model-id"
          class="rounded bg-gray-100 px-1 py-0.5 text-[10px] font-medium text-gray-600 dark:bg-dark-800 dark:text-gray-300"
        >
          {{ dim.target }}
        </span>
        <span
          data-test="cn-quota-model-status"
          class="inline-flex items-center rounded bg-red-100 px-1 py-0.5 text-[10px] font-medium text-red-700 dark:bg-red-900/30 dark:text-red-300"
        >
          {{ t('admin.accounts.cnProviders.statusExhausted') }}
        </span>
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
import type { Account, QuotaDimensionItem } from '@/types'
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

// ===== 规范化维度（quota_dimensions）：三分支可见性判定的唯一事实源 =====
// 派发单 F4：维度不再渲染为类别标签行，但 status / observed_at 仍是展示不变式的唯一
// 判定源；判定的 DOM 出口改为窗口内的数据展示（进度条 / 金额 /「未知」徽标）。
const quotaDimensions = computed<QuotaDimensionItem[]>(() => props.account.quota_dimensions ?? [])

// TH 模型级 free-tier 耗尽：逐模型展示（模型 ID + 耗尽徽标）。
const exhaustedModelDimensions = computed(() =>
  quotaDimensions.value.filter((d) => d.scope === 'model' && d.status === 'exhausted')
)

// TH 订阅维度（source=th_pass_snapshot, kind=subscription, scope=account）：驱动订阅
// 津贴进度条可见性（三分支）与恢复倒计时（exhausted）。
const thSubscriptionDimension = computed<QuotaDimensionItem | undefined>(() =>
  quotaDimensions.value.find(
    (d) => d.source === 'th_pass_snapshot' && d.kind === 'subscription' && d.scope === 'account'
  )
)

// TH free-tier 维度（source=th_pass_snapshot, kind=free, scope=account）：驱动 th
// 免费津贴进度条可见性（三分支）。
const thFreeDimension = computed<QuotaDimensionItem | undefined>(() =>
  quotaDimensions.value.find(
    (d) => d.source === 'th_pass_snapshot' && d.kind === 'free' && d.scope === 'account'
  )
)

// TH 钱包维度（source=th_balance, kind=paid, scope=account）：钱包余额行陈旧标注的判定源
// （存在性/数值来自 spend_after_allowance 能力字段与 th_balance 键）。
const thPaidDimension = computed<QuotaDimensionItem | undefined>(() =>
  quotaDimensions.value.find(
    (d) => d.source === 'th_balance' && d.kind === 'paid' && d.scope === 'account'
  )
)

// Kira 付费余额维度：source=kira_vnd_balance 维度（paid），既有 VND 金额取数路径不变。
const kiraPaidDimension = computed(() =>
  quotaDimensions.value.find((d) => d.source === 'kira_vnd_balance' && d.kind === 'paid')
)

// Kira 免费维度（source=kira_usage_snapshot, kind=free）：驱动当日进度条 + 已用/上限数值
// 的可见性（三分支）。
const kiraFreeDimension = computed<QuotaDimensionItem | undefined>(() =>
  quotaDimensions.value.find((d) => d.source === 'kira_usage_snapshot' && d.kind === 'free')
)

// 「未知」徽标样式：单一出口，替代被删除的类别/kind 标签样式。
const unknownBadgeClass = [
  'inline-flex',
  'items-center',
  'rounded',
  'bg-gray-100',
  'px-1',
  'py-0.5',
  'text-[10px]',
  'font-medium',
  'text-gray-500',
  'dark:bg-dark-800',
  'dark:text-gray-400'
]

// 维度是否已探测到可展示数值（决定「未知」徽标 vs 灰度陈旧条）：
// 订阅维度→plan_used_pct；TH free-tier→used_pct；TH wallet→th_balance；
// Kira 免费→used_percent；Kira paid→vndBalance。
const dimensionHasValue = (dim: QuotaDimensionItem): boolean => {
  if (dim.source === 'th_pass_snapshot' && dim.kind === 'subscription') return planUsedPct.value != null
  if (dim.source === 'th_pass_snapshot' && dim.kind === 'free') return thFreePct.value != null
  if (dim.source === 'th_balance' && dim.kind === 'paid') return thWalletBalance.value != null
  if (dim.source === 'kira_usage_snapshot' && dim.kind === 'free') return kiraUsedPercent.value != null
  if (dim.source === 'kira_vnd_balance' && dim.kind === 'paid') return vndBalance.value != null
  return false
}

// 单轨展示不变式（方案 §3.4 三分支最终口径 / 派发单 F1-R2 / F4）：
// 「未知」徽标仅当维度从未探测（observed_at 缺失/空 且 对应数值也不存在）才渲染；
// 已探测且数值存在但 observed_at 缺失/零值/无效（status=unknown）→ 不渲染「未知」，
// 改由灰度进度条 + 固定「采集时间未知」陈旧标注表达（第三分支）。
const dimensionStatusVisible = (dim: QuotaDimensionItem): boolean =>
  dim.status !== 'unknown' || (!dim.observed_at && !dimensionHasValue(dim))

// 从未探测（→ 渲染「未知」徽标）：维度存在、status=unknown、无 observed_at、无数值。
// 维度缺失（旧 DTO）→ false（降级渲染，不造状态）。
const neverProbed = (dim: QuotaDimensionItem | undefined): boolean =>
  !!dim && dim.status === 'unknown' && dimensionStatusVisible(dim)

// 陈旧标注：相对时间格式「约 N 分钟前 / 约 N 小时前 / 约 N 天前」（>24h 显示天）。
// observed_at 缺失/零值/无效 → parseTime 返回 null，formatStaleRelative 返回空串，
// 由 staleNoteFor 的第三分支兜底为固定「采集时间未知」文案。
const formatStaleRelative = (observedAt: string | undefined): string => {
  const d = parseTime(observedAt)
  if (!d) return ''
  const diffMs = Date.now() - d.getTime()
  const minutes = Math.floor(diffMs / 60000)
  if (minutes < 1) return t('admin.accounts.cnProviders.staleMinutes', { minutes: 0 })
  if (minutes < 60) return t('admin.accounts.cnProviders.staleMinutes', { minutes })
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return t('admin.accounts.cnProviders.staleHours', { hours })
  const days = Math.floor(hours / 24)
  return t('admin.accounts.cnProviders.staleDays', { days })
}

// 封装（方案 §3.4 三分支最终口径 / 派发单 F1-R2）：
// 数值存在 + observed_at 有效 → 灰度 + 「约 {time}前」；
// 数值存在 + observed_at 缺失/零值/无效 → 灰度 + 固定「采集时间未知」；
// 数值不存在分支不调用本函数（由「未知」徽标路径表达）。
const staleNoteFor = (observedAt: string | undefined): string => {
  const rel = formatStaleRelative(observedAt)
  return rel
    ? t('admin.accounts.cnProviders.staleAgo', { time: rel })
    : t('admin.accounts.cnProviders.staleTimeUnknown')
}

// ===== extra 快照解析（照 TokenHarborPassSnapshotFromExtra 的 JSON 兼容口径）=====

const readExtraObject = (key: string): Record<string, unknown> | null => {
  const raw = (props.account.extra as Record<string, unknown> | undefined)?.[key]
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null
  return raw as Record<string, unknown>
}

const extraNumber = (v: unknown): number | null => (typeof v === 'number' && Number.isFinite(v) ? v : null)

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

// 订阅续期倒计时（renews_at）：第三档胶囊（renew）显示，与 Pass 卡到期徽标同源同值。
const renewsAt = computed(() => parseTime(passSnapshot.value?.renews_at))
const renewsLabel = computed(() => {
  if (!renewsAt.value) return ''
  const countdown = formatCountdown(renewsAt.value)
  const time = countdown || renewsAt.value.toLocaleString()
  return t('admin.accounts.cnProviders.passRenewsAt', { time })
})

// 官方 7 天周期重置（reset_at）：Pass 徽标 + 7 天窗口倒计时数据源（D-QLM-008）。
const resetAt = computed(() => parseTime(passSnapshot.value?.reset_at))
const resetAtLabel = computed(() => {
  if (!resetAt.value) return ''
  const countdown = formatCountdown(resetAt.value)
  const time = countdown || resetAt.value.toLocaleString()
  return t('admin.accounts.cnProviders.passResetsAt', { time })
})
const resetAtFull = computed(() => resetAt.value?.toLocaleString() ?? '')

// 官方 Pass 津贴进度条：plan_used_pct 仍作为 bar 数值来源（注释标注维度 source）。
// 单轨展示不变式（方案 §3.4 v24）：可见性 = 数值存在（plan_used_pct != null）即渲染，
// 不再受 status==='unknown' 阻断（探针间隔内不得隐藏进度条）；维度缺失（旧 DTO）→
// 降级按既有 plan_used_pct / plan_exhausted 渲染。status=unknown 且无数值 →「未知」徽标。
const planUsedPct = computed<number | null>(() => {
  const v = passSnapshot.value?.plan_used_pct
  return typeof v === 'number' ? v : null
})
const passPlanBarVisible = computed(() => {
  if (!showPassWindow.value) return false
  const dim = thSubscriptionDimension.value
  if (!dim) return planUsedPct.value != null || passSnapshot.value?.plan_exhausted === true
  return planUsedPct.value != null
})
const passPlanUnknownVisible = computed(
  () => showPassWindow.value && neverProbed(thSubscriptionDimension.value)
)

// 订阅维度 status=unknown 且数值存在 → 灰度进度条 + 陈旧标注（不渲染「未知」徽标）。
const thStale = computed(() => {
  const dim = thSubscriptionDimension.value
  return !!dim && dim.status === 'unknown' && planUsedPct.value != null
})
const thStaleNote = computed(() =>
  thStale.value ? staleNoteFor(thSubscriptionDimension.value?.observed_at) : null
)

// 已耗尽状态（驱动恢复倒计时）：订阅维度（quota_dimensions source=th_pass_snapshot
// kind=subscription）status==='exhausted'。维度缺失（旧 DTO）→ 降级按既有 lifecycle
// state / plan_exhausted 渲染。recovery_at 取值路径不变（仍读 cn_quota_lifecycle）。
const exhausted = computed(() => {
  const dim = thSubscriptionDimension.value
  if (dim) return dim.status === 'exhausted'
  return lifecycle.value?.state === 'exhausted' || passSnapshot.value?.plan_exhausted === true
})
const recoveryAt = computed(() => parseTime(lifecycle.value?.recovery_at))
const recoveryLabel = computed(() => {
  if (!recoveryAt.value) return ''
  const countdown = formatCountdown(recoveryAt.value)
  const time = countdown || recoveryAt.value.toLocaleString()
  return t('admin.accounts.cnProviders.recoverAt', { time })
})
const recoveryAtFull = computed(() => recoveryAt.value?.toLocaleString() ?? '')

// 本期已用窗口切换（today/7d/renew）。renew 档显示订阅续期倒计时，不再有本地聚合。
const WINDOWS = ['today', '7d', 'renew'] as const
type UsageWindow = (typeof WINDOWS)[number]
const activeWindow = ref<UsageWindow>('today')

const windowToggleLabel = (w: UsageWindow): string => {
  if (w === 'today') return t('admin.accounts.cnProviders.windowToday')
  if (w === '7d') return t('admin.accounts.cnProviders.window7d')
  return t('admin.accounts.cnProviders.windowRenew')
}

const activeWindowStats = computed<THUsageWindowStats | null>(() => {
  const windows = usageSnapshot.value?.windows as Record<string, THUsageWindowStats> | undefined
  const stats = windows?.[activeWindow.value]
  if (!stats || typeof stats !== 'object') return null
  return stats
})

const activeWindowStatsLabel = computed(() => {
  // 第三档：订阅续期倒计时（renews_at），与 Pass 卡到期徽标同源同值。
  if (activeWindow.value === 'renew') {
    return renewsLabel.value
  }
  const stats = activeWindowStats.value
  const requests = typeof stats?.requests === 'number' ? stats.requests : 0
  const tokensIn = typeof stats?.tokens_in === 'number' ? stats.tokens_in : 0
  const tokensOut = typeof stats?.tokens_out === 'number' ? stats.tokens_out : 0
  const tokens = tokensIn + tokensOut
  const base = t('admin.accounts.cnProviders.windowStats', {
    requests: formatCompactNumber(requests, { allowBillions: false }),
    tokens: formatCompactNumber(tokens)
  })
  // 7 天档：在计数旁追加 reset_at 周期重置倒计时。
  if (activeWindow.value === '7d' && resetAtLabel.value) {
    return `${base} · ${resetAtLabel.value}`
  }
  return base
})

// ===== TH 免费链（无 Pass）：7 天滚动计数 + th free-tier 状态 =====

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

// th free-tier 官方免费津贴已用百分比（th_pass_snapshot.used_pct，D-QLM-006 官方口径键）。
const thFreePct = computed<number | null>(() =>
  extraNumber(readExtraObject('th_pass_snapshot')?.['used_pct'])
)
const thFreeBarVisible = computed(() => thFreePct.value != null)
const thFreeUnknownVisible = computed(() => neverProbed(thFreeDimension.value))
const thFreeStale = computed(() => {
  const dim = thFreeDimension.value
  return !!dim && dim.status === 'unknown' && thFreePct.value != null
})
const thFreeStaleNote = computed(() =>
  thFreeStale.value ? staleNoteFor(thFreeDimension.value?.observed_at) : null
)

// ===== TH 钱包 paid 维度：spend_after_allowance=true 且 th_balance 有值 → 钱包余额行 =====

const spendAfterAllowance = computed(() => passSnapshot.value?.spend_after_allowance === true)
const thWalletBalance = computed<number | null>(() =>
  extraNumber((props.account.extra as Record<string, unknown> | undefined)?.['th_balance'])
)
const thWalletVisible = computed(
  () => isTokenHarbor.value && spendAfterAllowance.value && thWalletBalance.value != null
)
const thWalletLabel = computed(() =>
  thWalletBalance.value == null
    ? ''
    : `${t('admin.usage.billingTypeBalance')} ${formatVnd(thWalletBalance.value)}`
)
// wallet 维度 status=unknown 且金额存在 → 金额旁附陈旧标注（与 Kira VND 行同口径）。
const thWalletStale = computed(() => {
  const dim = thPaidDimension.value
  return !!dim && dim.status === 'unknown' && thWalletBalance.value != null
})
const thWalletStaleNote = computed(() =>
  thWalletStale.value ? staleNoteFor(thPaidDimension.value?.observed_at) : null
)

// ===== Kira 用量链 =====

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

// 单轨可见性门：数值存在（kiraUsedPercent != null）即渲染；从未探测（free 维度
// status=unknown 且无 observed_at 且无数值）→ 不渲染旧快照百分比，改由「未知」徽标表达。
const kiraFreeVisible = computed(() => kiraUsedPercent.value != null)
const kiraFreeUnknownVisible = computed(() => neverProbed(kiraFreeDimension.value))

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

// Kira 免费维度 status=unknown 且数值存在 → 灰度进度条 + 陈旧标注。
const kiraStale = computed(() => {
  const dim = kiraFreeDimension.value
  return !!dim && dim.status === 'unknown' && kiraUsedPercent.value != null
})
const kiraStaleNote = computed(() =>
  kiraStale.value ? staleNoteFor(kiraFreeDimension.value?.observed_at) : null
)

// VND 余额：Kira 余额探测写 <platform>_balance（cn_provider_kira.go cnExtraKey）。
const vndBalance = computed<number | null>(() => {
  const v = (props.account.extra as Record<string, unknown> | undefined)?.[
    `${props.account.platform}_balance`
  ]
  return extraNumber(v)
})

// paid 维度 status=unknown 且金额存在 → 金额旁附陈旧标注；从未探测（status=unknown
// 且无 observed_at 且无金额）→「未知」徽标。
const kiraVndStale = computed(() => {
  const dim = kiraPaidDimension.value
  return !!dim && dim.status === 'unknown' && vndBalance.value != null
})
const kiraVndStaleNote = computed(() =>
  kiraVndStale.value ? staleNoteFor(kiraPaidDimension.value?.observed_at) : null
)
const kiraVndUnknownVisible = computed(() => neverProbed(kiraPaidDimension.value))

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
