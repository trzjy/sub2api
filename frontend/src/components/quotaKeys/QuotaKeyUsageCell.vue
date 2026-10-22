<template>
  <!--
    两层额度的展示面（方案 §25.6 消费闭包⑤⑥⑦ / §25.7-B 已接受口径）。

    第一层「所属激活码额度」= 供应商按激活码合并的额度快照（quota_keys.usage_snapshot
    的已提交投影）：只显示供应商给的总额/已用/算术剩余 + 观测时间，**不宣称**这是本站
    账单余额，也不拆赠额消耗顺序。

    纪律（渲染条件）：
      1. usage 缺失（无已提交快照）→ 整个组件不渲染，**不渲染任何数值**（缺失不是 0）；
      2. usage.query_failed（最近一次观测失败）→ 保留上次成功值 + checked_at，
         另起一行标注「查询失败」+ error_code + attempted_at，不写 0、不抹掉旧值；
      3. vendorAccountQuota.provided === false → **只渲染 note 文本**，
         本组件刻意不提供任何供应商余额数字/百分比/进度条的渲染位（§25.7-B）。
  -->
  <div
    v-if="visible"
    class="flex flex-col gap-0.5"
    data-test="quota-key-usage-cell"
  >
    <div
      v-if="showUsage"
      class="flex flex-col gap-0.5 text-[11px] leading-4"
      data-test="quota-key-usage"
    >
      <div class="flex flex-wrap items-center gap-1">
        <span
          class="inline-flex items-center rounded bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium text-blue-700 dark:bg-blue-900/30 dark:text-blue-300"
          data-test="quota-key-usage-label"
        >{{ label }}</span>
        <template v-if="showNumbers">
          <span
            class="font-mono text-gray-700 dark:text-gray-300"
            :title="t('admin.quotaKeys.probe.max')"
            data-test="quota-key-usage-total"
          >{{ totalText ?? '—' }}</span>
          <span class="text-gray-300 dark:text-dark-600">/</span>
          <span
            class="font-mono text-gray-700 dark:text-gray-300"
            :title="t('admin.quotaKeys.probe.used')"
            data-test="quota-key-usage-used"
          >{{ usedText ?? '—' }}</span>
          <template v-if="remainingText !== null">
            <span class="text-gray-300 dark:text-dark-600">/</span>
            <span
              class="font-mono text-emerald-700 dark:text-emerald-300"
              :title="t('admin.accounts.cnProviders.dimensionStatusRemaining')"
              data-test="quota-key-usage-remaining"
            >{{ remainingText }}</span>
          </template>
        </template>
        <span
          v-if="statusText !== null"
          class="text-[10px] text-gray-500 dark:text-gray-400"
          :title="t('admin.quotaKeys.probe.status')"
          data-test="quota-key-usage-status"
        >{{ statusText }}</span>
      </div>

      <!-- 观测时间：成功值（checked_at）与失败尝试（attempted_at）分开表达 -->
      <div
        v-if="checkedAtText"
        class="text-[10px] text-gray-400 dark:text-gray-500"
        data-test="quota-key-usage-checked-at"
      >{{ checkedAtText }}</div>

      <!-- 最近一次观测失败：保留上次成功值，不写 0、不抹旧值 -->
      <div
        v-if="queryFailed"
        class="text-[10px] text-amber-600 dark:text-amber-400"
        data-test="quota-key-usage-query-failed"
      >
        <span>{{ t('admin.accounts.opencodeGo.failed') }}</span>
        <span v-if="errorCode" data-test="quota-key-usage-error-code">({{ errorCode }})</span>
        <span v-if="attemptedAtText" data-test="quota-key-usage-attempted-at"> · {{ attemptedAtText }}</span>
      </div>
    </div>

    <!--
      §25.7-B：供应商账号级真实余额「未提供」——只给注明，不给任何数字/百分比。
      结构上这里没有任何余额渲染位，避免以后被误接成伪造值。
    -->
    <div
      v-if="vendorNote"
      class="text-[10px] leading-4 text-gray-500 dark:text-gray-400"
      data-test="vendor-account-quota-note"
    >{{ vendorNote }}</div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { QuotaKeyUsageProjection, VendorAccountQuotaView } from '@/types'
import { formatDateTime } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    /** 来源额度快照投影；缺失/undefined 表示无已提交快照（不是 0）。 */
    usage?: QuotaKeyUsageProjection | null
    /** 账号行的「供应商账号额度未提供」注明；provided=false 时只显示 note。 */
    vendorAccountQuota?: VendorAccountQuotaView | null
  }>(),
  {
    usage: null,
    vendorAccountQuota: null
  }
)

const { t } = useI18n()

const showUsage = computed(() => props.usage != null)

/** 供应商账号额度注明：只在明确 provided === false 且有 note 时渲染。 */
const vendorNote = computed(() => {
  const view = props.vendorAccountQuota
  if (!view || view.provided !== false) return ''
  return typeof view.note === 'string' ? view.note.trim() : ''
})

const visible = computed(() => showUsage.value || vendorNote.value !== '')

/** 展示标签：后端投影自带「所属激活码额度」，缺失时不补造。 */
const label = computed(() => {
  const value = props.usage?.label
  return typeof value === 'string' ? value : ''
})

function toCountText(value: number | undefined | null): string | null {
  if (value === undefined || value === null) return null
  if (typeof value !== 'number' || !Number.isFinite(value)) return null
  return String(value)
}

const totalText = computed(() => toCountText(props.usage?.total))
const usedText = computed(() => toCountText(props.usage?.used))
const remainingText = computed(() => toCountText(props.usage?.remaining))

/** 三数任一存在即渲染数值行；缺失位显示占位符，绝不补 0、绝不编造。 */
const showNumbers = computed(
  () => totalText.value !== null || usedText.value !== null || remainingText.value !== null
)

const statusText = computed(() => {
  const code = props.usage?.status_code
  if (code === undefined || code === null || !Number.isFinite(code)) return null
  return `HTTP ${code}`
})

const checkedAtText = computed(() => {
  const value = props.usage?.checked_at
  if (!value) return ''
  return formatDateTime(value)
})

const attemptedAtText = computed(() => {
  const value = props.usage?.attempted_at
  if (!value) return ''
  return formatDateTime(value)
})

const queryFailed = computed(() => props.usage?.query_failed === true)
const errorCode = computed(() => {
  const value = props.usage?.error_code
  return typeof value === 'string' ? value : ''
})
</script>
