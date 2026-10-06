<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.freshness.title')"
    width="wide"
    @close="emit('close')"
  >
    <div class="space-y-4">
      <div v-if="loading" class="flex items-center justify-center py-8" data-testid="freshness-loading">
        <svg class="h-6 w-6 animate-spin text-gray-400" fill="none" viewBox="0 0 24 24">
          <circle
            class="opacity-25"
            cx="12"
            cy="12"
            r="10"
            stroke="currentColor"
            stroke-width="4"
          ></circle>
          <path
            class="opacity-75"
            fill="currentColor"
            d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
          ></path>
        </svg>
      </div>

      <div
        v-else-if="!observations.length"
        class="rounded-lg border border-gray-200 p-4 text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400"
        data-testid="freshness-empty"
      >
        {{ t('admin.accounts.freshness.noObservations') }}
      </div>

      <div v-else class="overflow-x-auto">
        <table class="w-full text-left text-sm" data-testid="freshness-table">
          <thead class="text-xs uppercase text-gray-500 dark:text-gray-400">
            <tr>
              <th class="py-2 pr-3 font-medium">{{ t('admin.accounts.freshness.dimension') }}</th>
              <th class="py-2 pr-3 font-medium">{{ t('admin.accounts.freshness.scope') }}</th>
              <th class="py-2 pr-3 font-medium">{{ t('admin.accounts.freshness.observedAt') }}</th>
              <th class="py-2 pr-3 font-medium">{{ t('admin.accounts.freshness.attemptedAt') }}</th>
              <th class="py-2 pr-3 font-medium">{{ t('admin.accounts.freshness.threshold') }}</th>
              <th class="py-2 pr-3 font-medium">{{ t('admin.accounts.freshness.state') }}</th>
              <th class="py-2 font-medium">{{ t('admin.accounts.freshness.alert') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="obs in observations"
              :key="obs.scope"
              class="border-t border-gray-100 dark:border-dark-700"
              :data-testid="`freshness-row-${obs.display_state}`"
              :data-scope="obs.scope"
            >
              <td class="py-2 pr-3 text-gray-700 dark:text-gray-200">{{ dimensionLabel(obs.dimension) }}</td>
              <td class="py-2 pr-3 text-gray-900 dark:text-gray-100">{{ obs.scope }}</td>
              <td class="py-2 pr-3 text-gray-700 dark:text-gray-200">{{ formatTime(obs.observed_at) }}</td>
              <td class="py-2 pr-3 text-gray-700 dark:text-gray-200">{{ formatTime(obs.attempted_at) }}</td>
              <td class="py-2 pr-3 text-gray-700 dark:text-gray-200">{{ thresholdLabel(obs) }}</td>
              <td class="py-2 pr-3">
                <!-- 展示状态由后端 display_state 唯一决定：waiting_probe 是「等待主动复探」
                     空态，不得渲染恢复倒计时。 -->
                <span
                  class="inline-flex items-center rounded-full px-2 py-0.5 text-[11px] font-medium"
                  :class="stateBadgeClass(obs.display_state)"
                >{{ stateLabel(obs.display_state) }}</span>
              </td>
              <td class="py-2 text-gray-700 dark:text-gray-200">
                {{ obs.active_alert ? t('admin.accounts.freshness.alertActive') : '-' }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" @click="emit('close')">
          {{ t('common.close') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type {
  AccountFreshnessObservation,
  AccountFreshnessResponse,
} from '@/api/admin/accounts'
import type { Account } from '@/types'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { formatDateTime } from '@/utils/format'

const props = defineProps<{
  show: boolean
  account: Account | null
}>()

const emit = defineEmits<{
  close: []
}>()

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const freshness = ref<AccountFreshnessResponse | null>(null)

const observations = computed<AccountFreshnessObservation[]>(
  () => freshness.value?.observations ?? []
)

function dimensionLabel(dimension: string): string {
  if (dimension === 'account_level') return t('admin.accounts.freshness.dimensionAccountLevel')
  return t('admin.accounts.freshness.dimensionAccountModel')
}

function stateLabel(state: string): string {
  if (state === 'waiting_probe') return t('admin.accounts.freshness.waitingProbe')
  if (state === 'stale') return t('admin.accounts.freshness.stale')
  return t('admin.accounts.freshness.observed')
}

function stateBadgeClass(state: string): string {
  switch (state) {
    case 'stale':
      return 'bg-red-100 text-red-700 dark:bg-red-500/15 dark:text-red-300'
    case 'waiting_probe':
      // 无信号占位：中性空态，不是失败。
      return 'bg-gray-100 text-gray-700 dark:bg-dark-700 dark:text-gray-300'
    default:
      return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300'
  }
}

function formatTime(iso?: string): string {
  if (!iso) return '-'
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return '-'
  return formatDateTime(date)
}

function thresholdLabel(obs: AccountFreshnessObservation): string {
  const minutes = Math.round((obs.effective_threshold_seconds ?? 0) / 60)
  return t('admin.accounts.freshness.thresholdMinutes', { minutes })
}

const loadFreshness = async () => {
  if (!props.account) return
  loading.value = true
  try {
    freshness.value = await adminAPI.accounts.getFreshness(props.account.id)
  } catch (error: any) {
    appStore.showError(error?.message || t('admin.accounts.freshness.loadFailed'))
    freshness.value = null
  } finally {
    loading.value = false
  }
}

watch(
  () => [props.show, props.account?.id],
  ([visible]) => {
    if (visible && props.account) {
      void loadFreshness()
      return
    }
    freshness.value = null
  }
)
</script>
