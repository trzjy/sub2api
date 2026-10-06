<template>
  <!--
    渠道级档位 + 权威观测时间（后端经推导产出，与 per-model 明细行分列展示）。
    无数据（档位为空 / 无观测时间）按空态横线呈现，绝不显示为失败。
  -->
  <div class="flex flex-col items-start gap-0.5" data-testid="channel-freshness">
    <span
      v-if="hasStatus"
      class="inline-flex items-center rounded-full px-2 py-0.5 text-[11px] font-medium"
      :class="statusBadgeClass(statusValue)"
      data-testid="channel-freshness-status"
    >
      {{ statusLabel(statusValue) }}
    </span>
    <span
      v-else
      class="text-xs text-gray-400 dark:text-gray-500"
      data-testid="channel-freshness-empty"
    >-</span>
    <span
      v-if="hasStatus"
      class="text-[11px] text-gray-400 dark:text-gray-500"
      data-testid="channel-freshness-observed"
    >
      {{ observedText }}
    </span>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { MonitorStatus } from '@/api/admin/channelMonitor'
import { useChannelMonitorFormat } from '@/composables/useChannelMonitorFormat'

const props = defineProps<{
  /** 渠道级档位；空/缺省表示无数据观测。 */
  status?: string | null
  /** 渠道级权威观测时间（RFC3339）；空表示无数据。 */
  observedAt?: string | null
}>()

const { statusLabel, statusBadgeClass, formatRelativeTime } = useChannelMonitorFormat()

const hasStatus = computed(() => Boolean(props.status))
const statusValue = computed<MonitorStatus | ''>(() => (props.status ?? '') as MonitorStatus | '')
const observedText = computed(() => (props.observedAt ? formatRelativeTime(props.observedAt) : '-'))
</script>
