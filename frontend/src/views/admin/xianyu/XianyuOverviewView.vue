<template>
  <AppLayout>
    <div class="p-6">
      <div class="mb-6 flex items-start justify-between">
        <div>
          <h1 class="text-2xl font-bold">{{ t('admin.xianyu.overview.title') }}</h1>
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.xianyu.overview.description') }}</p>
        </div>
        <div class="flex items-center gap-2">
          <span v-if="overview?.worker_last_checked_at" class="text-xs text-gray-400">
            {{ t('admin.xianyu.overview.lastChecked') }}: {{ formatDateTime(overview.worker_last_checked_at) }}
          </span>
          <button
            class="btn btn-secondary"
            :disabled="loading"
            @click="load"
          >
            <Icon name="refresh" size="sm" />
            {{ loading ? '...' : t('admin.xianyu.products.refresh') }}
          </button>
        </div>
      </div>

      <div v-if="loadError" class="mb-4 rounded border border-red-200 bg-red-50 px-4 py-2 text-sm text-red-700 dark:border-dark-700 dark:bg-red-900/20 dark:text-red-300">
        {{ loadError }}
      </div>

      <div class="grid grid-cols-2 gap-4 md:grid-cols-3 lg:grid-cols-4">
        <StatCard :title="t('admin.xianyu.overview.workerHealthy')" :value="workerHealthLabel" />
        <StatCard :title="t('admin.xianyu.overview.enabledAccounts')" :value="String(overview?.enabled_accounts ?? 0)" />
        <StatCard :title="t('admin.xianyu.overview.runningTasks')" :value="String(overview?.running_tasks ?? 0)" />
        <StatCard :title="t('admin.xianyu.overview.unmappedProducts')" :value="String(overview?.unmapped_products ?? 0)" />
        <StatCard :title="t('admin.xianyu.overview.todayDelivered')" :value="String(overview?.today_delivered ?? 0)" />
        <StatCard :title="t('admin.xianyu.overview.todayFailed')" :value="String(overview?.today_failed ?? 0)" />
        <StatCard :title="t('admin.xianyu.overview.pendingDeliveries')" :value="String(overview?.pending_deliveries ?? 0)" />
      </div>

      <div class="mt-6">
        <div class="mb-3 flex items-center justify-between">
          <h2 class="text-lg font-semibold">{{ t('admin.xianyu.overview.pools') }}</h2>
        </div>
        <div v-if="overview?.pools?.length" class="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
          <div
            v-for="item in overview.pools"
            :key="item.pool.id"
            class="rounded-lg border border-gray-200 p-4 dark:border-gray-700"
            :class="{ 'border-red-400 dark:border-red-600': item.low_stock }"
          >
            <div class="flex items-center justify-between">
              <span class="font-semibold">{{ item.pool.name }}</span>
              <span
                v-if="item.low_stock"
                class="rounded bg-red-100 px-2 py-0.5 text-xs text-red-600 dark:bg-red-900 dark:text-red-300"
              >
                {{ t('admin.xianyu.overview.lowStock') }}
              </span>
            </div>
            <div class="mt-2 text-sm text-gray-600 dark:text-gray-400">
              {{ t('admin.xianyu.overview.remaining') }}: {{ item.remaining }} ·
              {{ t('admin.xianyu.overview.used') }}: {{ item.used }} ·
              {{ t('admin.xianyu.overview.disabled') }}: {{ item.disabled }}
            </div>
          </div>
        </div>
        <EmptyState v-else :message="t('admin.xianyu.overview.none')" />
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import { formatDateTime } from '@/utils/format'
import type { XianyuOverview } from '@/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import StatCard from '@/components/common/StatCard.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import Icon from '@/components/icons/Icon.vue'

const { t } = useI18n()
const appStore = useAppStore()

const overview = ref<XianyuOverview | null>(null)
const loading = ref(false)
const loadError = ref('')

const workerHealthLabel = computed(() => {
  if (!overview.value) return t('admin.xianyu.overview.unknown')
  const status = overview.value.worker_health_status || (overview.value.worker_healthy ? 'healthy' : 'unhealthy')
  switch (status) {
    case 'healthy': return t('admin.xianyu.overview.healthy')
    case 'unhealthy': return t('admin.xianyu.overview.unhealthy')
    default: return t('admin.xianyu.overview.unknown')
  }
})

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    overview.value = await adminAPI.xianyu.getOverview()
  } catch (err) {
    loadError.value = String(err)
    appStore.showError(String(err))
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>
