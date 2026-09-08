<template>
  <AppLayout>
    <div class="mb-6 flex flex-wrap items-start justify-between gap-4">
      <div>
        <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ t('admin.pricing.title') }}</h1>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.pricing.description') }}</p>
      </div>
    </div>

    <!-- Tabs -->
    <div class="mb-4 flex flex-wrap gap-2">
      <button
        v-for="tab in tabs"
        :key="tab.key"
        type="button"
        class="btn"
        :class="activeTab === tab.key ? 'btn-primary' : 'btn-secondary'"
        @click="switchTab(tab.key)"
      >
        {{ t(tab.label) }}
        <span
          v-if="tab.badge && tab.badge() > 0"
          class="ml-1.5 inline-flex h-5 min-w-5 items-center justify-center rounded-full bg-red-500 px-1.5 text-xs font-bold text-white"
        >
          {{ tab.badge() }}
        </span>
      </button>
    </div>

    <!-- ==================== 同步状态 ==================== -->
    <div v-if="activeTab === 'status'" class="space-y-4">
      <div class="card p-6">
        <div class="flex flex-wrap items-start justify-between gap-4">
          <div class="min-w-0 flex-1">
            <h2 class="text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.pricing.status.syncTitle') }}</h2>
            <p class="mt-1 break-all text-xs text-gray-500 dark:text-gray-400">{{ status?.sync.remote_url || '—' }}</p>
          </div>
          <button type="button" class="btn btn-primary" :disabled="syncing || status?.sync.syncing" @click="doSync">
            <Icon name="refresh" size="sm" class="mr-1.5" :class="{ 'animate-spin': syncing }" />
            {{ t('admin.pricing.status.syncNow') }}
          </button>
        </div>

        <div class="mt-5 grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <div class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
            <div class="text-xs font-bold uppercase tracking-wider text-gray-400">{{ t('admin.pricing.status.modelCount') }}</div>
            <div class="mt-1 text-xl font-bold text-gray-900 dark:text-white">{{ status?.sync.model_count ?? '—' }}</div>
          </div>
          <div class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
            <div class="text-xs font-bold uppercase tracking-wider text-gray-400">{{ t('admin.pricing.status.lastUpdated') }}</div>
            <div class="mt-1 text-sm font-medium text-gray-900 dark:text-white">{{ formatTime(status?.sync.last_updated) || '—' }}</div>
            <div class="mt-0.5 text-xs text-gray-400">{{ t('admin.pricing.status.scheduler') }}: {{ status?.sync.scheduler_enabled ? t('common.enabled') : t('common.disabled') }}</div>
          </div>
          <div class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
            <div class="text-xs font-bold uppercase tracking-wider text-gray-400">{{ t('admin.pricing.status.lastAttempt') }}</div>
            <div class="mt-1 text-sm font-medium text-gray-900 dark:text-white">{{ formatTime(status?.sync.last_attempt_at) || '—' }}</div>
            <div v-if="status?.sync.last_error" class="mt-0.5 break-all text-xs text-red-500" :title="status.sync.last_error">
              {{ status.sync.last_error }}
            </div>
            <div v-else class="mt-0.5 text-xs text-green-600 dark:text-green-400">{{ t('admin.pricing.status.noError') }}</div>
          </div>
          <div class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
            <div class="text-xs font-bold uppercase tracking-wider text-gray-400">{{ t('admin.pricing.status.customLayer') }}</div>
            <div class="mt-1 text-xl font-bold text-gray-900 dark:text-white">
              {{ status?.custom.enabled ?? 0 }} / {{ status?.custom.entries ?? 0 }}
            </div>
            <div class="mt-0.5 text-xs text-gray-400">{{ t('admin.pricing.status.customLayerHint') }}</div>
          </div>
        </div>

        <div v-if="status?.sync.local_hash" class="mt-4 break-all font-mono text-xs text-gray-400">
          {{ t('admin.pricing.status.localHash') }}: {{ status.sync.local_hash }}
        </div>
      </div>

      <!-- 线上计费缺口 -->
      <div class="card p-6">
        <h2 class="text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.pricing.status.gapsTitle') }}</h2>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.pricing.status.gapsHint') }}</p>
        <div v-if="!status?.live_gaps.length" class="mt-4 flex items-center gap-2 text-sm text-green-600 dark:text-green-400">
          <Icon name="check" size="sm" />
          {{ t('admin.pricing.status.noGaps') }}
        </div>
        <div v-else class="mt-4 overflow-x-auto">
          <table class="min-w-full text-sm">
            <thead>
              <tr class="border-b border-gray-200 text-left text-xs font-bold uppercase tracking-wider text-gray-400 dark:border-dark-600">
                <th class="py-2 pr-4">{{ t('admin.pricing.columns.model') }}</th>
                <th class="py-2 pr-4">{{ t('admin.pricing.gaps.count') }}</th>
                <th class="py-2 pr-4">{{ t('admin.pricing.gaps.firstSeen') }}</th>
                <th class="py-2 pr-4">{{ t('admin.pricing.gaps.lastSeen') }}</th>
                <th class="py-2">{{ t('admin.pricing.actions') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="gap in status.live_gaps" :key="gap.model" class="border-b border-gray-100 dark:border-dark-700">
                <td class="py-2 pr-4 font-mono font-medium text-gray-900 dark:text-white">{{ gap.model }}</td>
                <td class="py-2 pr-4 text-red-500">{{ gap.count }}</td>
                <td class="py-2 pr-4 text-gray-500">{{ formatTime(gap.first_seen) }}</td>
                <td class="py-2 pr-4 text-gray-500">{{ formatTime(gap.last_seen) }}</td>
                <td class="py-2">
                  <button type="button" class="font-medium text-primary-600 hover:text-primary-700 dark:text-primary-400" @click="openEditFromModel(gap.model)">
                    {{ t('admin.pricing.gaps.addPrice') }}
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>

    <!-- ==================== 价格目录 ==================== -->
    <div v-if="activeTab === 'catalog'" class="space-y-4">
      <div class="card p-4">
        <div class="flex flex-wrap items-end gap-4">
          <div class="min-w-[220px] flex-1">
            <label class="input-label">{{ t('admin.pricing.catalog.search') }}</label>
            <input v-model.trim="catalogSearch" type="text" class="input" :placeholder="t('admin.pricing.catalog.searchPlaceholder')" @keyup.enter="searchCatalog" />
          </div>
          <div class="w-40">
            <label class="input-label">{{ t('admin.pricing.catalog.source') }}</label>
            <Select v-model="catalogSource" :options="sourceOptions" @change="searchCatalog" />
          </div>
          <button type="button" class="btn btn-primary" :disabled="catalogLoading" @click="searchCatalog">
            {{ t('common.search') }}
          </button>
          <div class="ml-auto w-64">
            <label class="input-label">{{ t('admin.pricing.preview.title') }}</label>
            <div class="flex gap-2">
              <input v-model.trim="previewModel" type="text" class="input" :placeholder="t('admin.pricing.preview.modelPlaceholder')" />
              <button type="button" class="btn btn-secondary shrink-0" :disabled="!previewModel || previewLoading" @click="doPreview">
                {{ t('admin.pricing.preview.run') }}
              </button>
            </div>
          </div>
        </div>
        <p class="mt-3 text-xs text-gray-400">{{ t('admin.pricing.catalog.sourceHint') }}</p>
      </div>

      <!-- 试算结果 -->
      <div v-if="preview" class="card p-6">
        <div class="flex flex-wrap items-center gap-3">
          <span class="break-all font-mono text-base font-semibold text-gray-900 dark:text-white">{{ preview.model }}</span>
          <span :class="sourceBadgeClass(preview.source)">{{ sourceLabel(preview.source) }}</span>
          <span v-if="preview.group_name" class="rounded-full bg-gray-100 px-2.5 py-0.5 text-xs font-medium text-gray-600 dark:bg-dark-700 dark:text-gray-300">
            {{ preview.group_name }} × {{ preview.rate_multiplier }}
          </span>
        </div>
        <div class="mt-4 grid grid-cols-2 gap-4 lg:grid-cols-4">
          <div class="rounded-xl bg-gray-50 p-3 dark:bg-dark-900">
            <div class="text-xs font-bold uppercase tracking-wider text-gray-400">{{ t('admin.pricing.preview.input') }}</div>
            <div class="mt-1 font-mono text-sm font-semibold text-gray-900 dark:text-white">${{ fmtPrice(preview.input_per_mtok) }} / {{ t('admin.pricing.mtok') }}</div>
          </div>
          <div class="rounded-xl bg-gray-50 p-3 dark:bg-dark-900">
            <div class="text-xs font-bold uppercase tracking-wider text-gray-400">{{ t('admin.pricing.preview.output') }}</div>
            <div class="mt-1 font-mono text-sm font-semibold text-gray-900 dark:text-white">${{ fmtPrice(preview.output_per_mtok) }} / {{ t('admin.pricing.mtok') }}</div>
          </div>
          <div class="rounded-xl bg-gray-50 p-3 dark:bg-dark-900">
            <div class="text-xs font-bold uppercase tracking-wider text-gray-400">{{ t('admin.pricing.preview.cacheWrite') }}</div>
            <div class="mt-1 font-mono text-sm font-semibold text-gray-900 dark:text-white">${{ fmtPrice(preview.cache_write_per_mtok) }} / {{ t('admin.pricing.mtok') }}</div>
          </div>
          <div class="rounded-xl bg-gray-50 p-3 dark:bg-dark-900">
            <div class="text-xs font-bold uppercase tracking-wider text-gray-400">{{ t('admin.pricing.preview.cacheRead') }}</div>
            <div class="mt-1 font-mono text-sm font-semibold text-gray-900 dark:text-white">${{ fmtPrice(preview.cache_read_per_mtok) }} / {{ t('admin.pricing.mtok') }}</div>
          </div>
        </div>
      </div>

      <TablePageLayout>
        <template #table>
          <DataTable :columns="catalogColumns" :data="catalog" :loading="catalogLoading" row-key="model">
            <template #cell-model="{ row }">
              <span class="break-all font-mono text-sm font-medium text-gray-900 dark:text-white">{{ row.model }}</span>
              <span v-if="row.token_pricing_absent" class="ml-2 rounded bg-amber-100 px-1.5 py-0.5 text-[11px] font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-400">
                {{ t('admin.pricing.catalog.imageOnly') }}
              </span>
            </template>
            <template #cell-source="{ row }">
              <span :class="sourceBadgeClass(row.source)">{{ sourceLabel(row.source) }}</span>
            </template>
            <template #cell-input_per_mtok="{ value }">${{ fmtPrice(value) }}</template>
            <template #cell-output_per_mtok="{ value }">${{ fmtPrice(value) }}</template>
            <template #cell-cache_read_per_mtok="{ value }">${{ fmtPrice(value) }}</template>
            <template #cell-actions="{ row }">
              <button
                type="button"
                class="font-medium text-primary-600 transition-colors hover:text-primary-700 dark:text-primary-400"
                @click="openEditFromModel(row.model)"
              >
                {{ t('admin.pricing.catalog.override') }}
              </button>
            </template>
            <template #empty>
              <div class="flex flex-col items-center py-8">
                <p class="text-sm font-medium text-gray-500 dark:text-gray-400">{{ t('admin.pricing.catalog.empty') }}</p>
              </div>
            </template>
          </DataTable>
        </template>
        <template #pagination>
          <Pagination
            v-if="catalogTotal > 0"
            :total="catalogTotal"
            :page="catalogPage"
            :page-size="catalogPageSize"
            @update:page="(p: number) => { catalogPage = p; fetchCatalog() }"
          />
        </template>
      </TablePageLayout>
    </div>

    <!-- ==================== 未覆盖模型 ==================== -->
    <div v-if="activeTab === 'uncovered'" class="space-y-4">
      <div class="card flex flex-wrap items-end justify-between gap-4 p-4">
        <div class="flex flex-wrap items-end gap-4">
          <div class="w-44">
            <label class="input-label">{{ t('admin.pricing.uncovered.windowDays') }}</label>
            <Select v-model="uncoveredDays" :options="uncoveredDayOptions" @change="fetchUncovered" />
          </div>
          <p class="pb-2 text-xs text-gray-400">{{ t('admin.pricing.uncovered.hint') }}</p>
        </div>
        <button type="button" class="btn btn-secondary" :disabled="uncoveredLoading" @click="fetchUncovered">
          <Icon name="refresh" size="sm" class="mr-1.5" />
          {{ t('common.refresh') }}
        </button>
      </div>

      <div class="card p-6">
        <div v-if="uncovered && !uncovered.items.length && !uncoveredLoading" class="flex items-center gap-2 text-sm text-green-600 dark:text-green-400">
          <Icon name="check" size="sm" />
          {{ t('admin.pricing.uncovered.allCovered', { scanned: uncovered.scanned }) }}
        </div>
        <div v-else class="overflow-x-auto">
          <table class="min-w-full text-sm">
            <thead>
              <tr class="border-b border-gray-200 text-left text-xs font-bold uppercase tracking-wider text-gray-400 dark:border-dark-600">
                <th class="py-2 pr-4">{{ t('admin.pricing.columns.model') }}</th>
                <th class="py-2 pr-4">{{ t('admin.pricing.uncovered.references') }}</th>
                <th class="py-2 pr-4">{{ t('admin.pricing.uncovered.usage') }}</th>
                <th class="py-2 pr-4">{{ t('admin.pricing.uncovered.risk') }}</th>
                <th class="py-2">{{ t('admin.pricing.actions') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="item in uncovered?.items || []" :key="item.model" class="border-b border-gray-100 dark:border-dark-700">
                <td class="py-2 pr-4 font-mono font-medium text-gray-900 dark:text-white">{{ item.model }}</td>
                <td class="max-w-xs py-2 pr-4">
                  <span class="break-words text-xs text-gray-500 dark:text-gray-400">{{ (item.references || []).join('、') || '—' }}</span>
                </td>
                <td class="whitespace-nowrap py-2 pr-4 text-xs text-gray-500 dark:text-gray-400">
                  <template v-if="item.usage">
                    {{ item.usage.requests }} {{ t('admin.pricing.uncovered.requests') }} · {{ formatTokens(item.usage.total_tokens) }} {{ t('admin.pricing.uncovered.tokens') }}
                  </template>
                  <template v-else>—</template>
                </td>
                <td class="py-2 pr-4">
                  <span v-if="item.zero_cost_only" class="rounded-full bg-red-100 px-2 py-0.5 text-xs font-semibold text-red-600 dark:bg-red-900/30 dark:text-red-400">
                    {{ t('admin.pricing.uncovered.zeroCost') }}
                  </span>
                  <span v-else class="text-xs text-gray-400">—</span>
                </td>
                <td class="py-2">
                  <button type="button" class="font-medium text-primary-600 hover:text-primary-700 dark:text-primary-400" @click="openEditFromModel(item.model)">
                    {{ t('admin.pricing.gaps.addPrice') }}
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>

    <!-- ==================== 自定义价格 ==================== -->
    <div v-if="activeTab === 'custom'" class="space-y-4">
      <div class="card flex flex-wrap items-center justify-between gap-4 p-4">
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.pricing.custom.hint') }}</p>
        <button type="button" class="btn btn-primary" @click="openEdit(null)">
          <Icon name="plus" size="sm" class="mr-1.5" />
          {{ t('admin.pricing.custom.add') }}
        </button>
      </div>

      <div class="card p-6">
        <div v-if="!customList.length && !customLoading" class="flex flex-col items-center py-8">
          <p class="text-sm font-medium text-gray-500 dark:text-gray-400">{{ t('admin.pricing.custom.empty') }}</p>
        </div>
        <div v-else class="overflow-x-auto">
          <table class="min-w-full text-sm">
            <thead>
              <tr class="border-b border-gray-200 text-left text-xs font-bold uppercase tracking-wider text-gray-400 dark:border-dark-600">
                <th class="py-2 pr-4">{{ t('admin.pricing.custom.models') }}</th>
                <th class="py-2 pr-4">{{ t('admin.pricing.custom.mode') }}</th>
                <th class="py-2 pr-4">{{ t('admin.pricing.preview.input') }}</th>
                <th class="py-2 pr-4">{{ t('admin.pricing.preview.output') }}</th>
                <th class="py-2 pr-4">{{ t('admin.pricing.custom.remark') }}</th>
                <th class="py-2 pr-4">{{ t('admin.pricing.custom.enabled') }}</th>
                <th class="py-2">{{ t('admin.pricing.actions') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="entry in customList" :key="entry.id" class="border-b border-gray-100 dark:border-dark-700">
                <td class="max-w-xs py-2 pr-4">
                  <span class="break-all font-mono text-xs font-medium text-gray-900 dark:text-white">{{ entry.models.join(', ') }}</span>
                </td>
                <td class="py-2 pr-4 text-xs text-gray-500">{{ entry.billing_mode || 'token' }}</td>
                <td class="whitespace-nowrap py-2 pr-4 font-mono text-xs">${{ fmtPrice((entry.input_price ?? 0) * 1e6) }}</td>
                <td class="whitespace-nowrap py-2 pr-4 font-mono text-xs">${{ fmtPrice((entry.output_price ?? 0) * 1e6) }}</td>
                <td class="max-w-[200px] truncate py-2 pr-4 text-xs text-gray-500" :title="entry.remark">{{ entry.remark || '—' }}</td>
                <td class="py-2 pr-4">
                  <span :class="entry.enabled ? 'rounded-full bg-green-100 px-2 py-0.5 text-xs font-medium text-green-700 dark:bg-green-900/30 dark:text-green-400' : 'rounded-full bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-500 dark:bg-dark-700 dark:text-gray-400'">
                    {{ entry.enabled ? t('common.enabled') : t('common.disabled') }}
                  </span>
                </td>
                <td class="whitespace-nowrap py-2">
                  <button type="button" class="mr-3 font-medium text-primary-600 hover:text-primary-700 dark:text-primary-400" @click="openEdit(entry)">
                    {{ t('common.edit') }}
                  </button>
                  <button type="button" class="font-medium text-red-600 hover:text-red-700 dark:text-red-400" @click="askDelete(entry)">
                    {{ t('common.delete') }}
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>

    <!-- 编辑对话框 -->
    <BaseDialog :show="editVisible" :title="editTitle" width="wide" @close="editVisible = false">
      <div class="space-y-4 py-2">
        <div>
          <label class="input-label">{{ t('admin.pricing.custom.models') }}</label>
          <input v-model.trim="editForm.modelsText" type="text" class="input" :placeholder="t('admin.pricing.custom.modelsPlaceholder')" />
          <p class="mt-1 text-xs text-gray-400">{{ t('admin.pricing.custom.modelsHint') }}</p>
        </div>
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div>
            <label class="input-label">{{ t('admin.pricing.custom.mode') }}</label>
            <Select v-model="editForm.billing_mode" :options="billingModeOptions" />
          </div>
          <div>
            <label class="input-label">{{ t('admin.pricing.custom.remark') }}</label>
            <input v-model.trim="editForm.remark" type="text" class="input" />
          </div>
        </div>
        <p class="text-xs text-gray-400">{{ t('admin.pricing.custom.priceUnit') }}</p>
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
          <div>
            <label class="input-label">{{ t('admin.pricing.preview.input') }} ($/MTok)</label>
            <Input v-model="editForm.input_per_mtok" type="number" />
          </div>
          <div>
            <label class="input-label">{{ t('admin.pricing.preview.output') }} ($/MTok)</label>
            <Input v-model="editForm.output_per_mtok" type="number" />
          </div>
          <div>
            <label class="input-label">{{ t('admin.pricing.preview.cacheWrite') }} ($/MTok)</label>
            <Input v-model="editForm.cache_write_per_mtok" type="number" />
          </div>
          <div>
            <label class="input-label">{{ t('admin.pricing.preview.cacheRead') }} ($/MTok)</label>
            <Input v-model="editForm.cache_read_per_mtok" type="number" />
          </div>
          <div v-if="editForm.billing_mode !== 'token'">
            <label class="input-label">{{ t('admin.pricing.custom.perRequest') }} ($/次)</label>
            <Input v-model="editForm.per_request_price" type="number" />
          </div>
          <div class="flex items-end pb-1">
            <label class="inline-flex cursor-pointer items-center gap-2">
              <input v-model="editForm.enabled" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600" />
              <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.pricing.custom.enabled') }}</span>
            </label>
          </div>
        </div>
      </div>
      <template #footer>
        <div class="flex justify-end gap-3">
          <button type="button" class="btn btn-secondary" @click="editVisible = false">{{ t('common.cancel') }}</button>
          <button type="button" class="btn btn-primary" :disabled="editSaving || !editForm.modelsText" @click="saveEdit">
            {{ t('common.save') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <!-- 删除确认 -->
    <ConfirmDialog
      :show="deleteVisible"
      :title="t('admin.pricing.custom.deleteTitle')"
      :message="t('admin.pricing.custom.deleteMessage', { models: deleteTarget?.models.join(', ') || '' })"
      danger
      @confirm="confirmDelete"
      @cancel="deleteVisible = false"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type {
  CatalogEntry,
  CustomModelPricing,
  PricingPreview,
  PricingStatusResponse,
  UncoveredResponse
} from '@/api/admin'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import type { Column } from '@/components/common/types'
import Pagination from '@/components/common/Pagination.vue'
import Select from '@/components/common/Select.vue'
import Input from '@/components/common/Input.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore } from '@/stores'

const { t } = useI18n()
const appStore = useAppStore()

type TabKey = 'status' | 'catalog' | 'uncovered' | 'custom'
const activeTab = ref<TabKey>('status')

const status = ref<PricingStatusResponse | null>(null)
const uncovered = ref<UncoveredResponse | null>(null)

const tabs = computed(() => [
  { key: 'status' as TabKey, label: 'admin.pricing.tabs.status', badge: () => status.value?.live_gaps.length ?? 0 },
  { key: 'catalog' as TabKey, label: 'admin.pricing.tabs.catalog', badge: () => 0 },
  { key: 'uncovered' as TabKey, label: 'admin.pricing.tabs.uncovered', badge: () => uncovered.value?.items.length ?? 0 },
  { key: 'custom' as TabKey, label: 'admin.pricing.tabs.custom', badge: () => 0 }
])

function switchTab(tab: TabKey) {
  activeTab.value = tab
  if (tab === 'status' && !status.value) fetchStatus()
  if (tab === 'catalog' && !catalog.value.length && !catalogLoading.value) fetchCatalog()
  if (tab === 'uncovered' && !uncovered.value) fetchUncovered()
  if (tab === 'custom' && !customList.value.length && !customLoading.value) fetchCustom()
}

// --- 同步状态 ---

const syncing = ref(false)

async function fetchStatus() {
  try {
    status.value = await adminAPI.pricing.getStatus()
    if (status.value.live_gaps == null) status.value.live_gaps = []
  } catch (err: any) {
    appStore.showError(err?.message || t('common.error'))
  }
}

async function doSync() {
  syncing.value = true
  try {
    status.value = await adminAPI.pricing.syncNow()
    appStore.showSuccess(t('admin.pricing.status.syncDone'))
  } catch (err: any) {
    appStore.showError(err?.message || t('common.error'))
  } finally {
    syncing.value = false
  }
}

// --- 价格目录 ---

const catalog = ref<CatalogEntry[]>([])
const catalogTotal = ref(0)
const catalogPage = ref(1)
const catalogPageSize = ref(50)
const catalogLoading = ref(false)
const catalogSearch = ref('')
const catalogSource = ref('')

const sourceOptions = computed(() => [
  { value: '', label: t('admin.pricing.catalog.allSources') },
  { value: 'custom', label: sourceLabel('custom') },
  { value: 'litellm', label: sourceLabel('litellm') },
  { value: 'fallback', label: sourceLabel('fallback') }
])

const catalogColumns = computed<Column[]>(() => [
  { key: 'model', label: t('admin.pricing.columns.model') },
  { key: 'source', label: t('admin.pricing.columns.source') },
  { key: 'input_per_mtok', label: t('admin.pricing.preview.input') },
  { key: 'output_per_mtok', label: t('admin.pricing.preview.output') },
  { key: 'cache_read_per_mtok', label: t('admin.pricing.preview.cacheRead') },
  { key: 'actions', label: t('admin.pricing.actions') }
])

async function fetchCatalog() {
  catalogLoading.value = true
  try {
    const res = await adminAPI.pricing.getCatalog({
      search: catalogSearch.value,
      source: catalogSource.value,
      page: catalogPage.value,
      page_size: catalogPageSize.value
    })
    catalog.value = res.items || []
    catalogTotal.value = res.total || 0
  } catch (err: any) {
    appStore.showError(err?.message || t('common.error'))
  } finally {
    catalogLoading.value = false
  }
}

function searchCatalog() {
  catalogPage.value = 1
  fetchCatalog()
}

// --- 试算 ---

const previewModel = ref('')
const preview = ref<PricingPreview | null>(null)
const previewLoading = ref(false)

async function doPreview() {
  if (!previewModel.value) return
  previewLoading.value = true
  try {
    preview.value = await adminAPI.pricing.getPreview(previewModel.value)
  } catch (err: any) {
    preview.value = null
    appStore.showError(err?.message || t('common.error'))
  } finally {
    previewLoading.value = false
  }
}

// --- 未覆盖扫描 ---

const uncoveredLoading = ref(false)
const uncoveredDays = ref('30')

const uncoveredDayOptions = computed(() => [
  { value: '7', label: t('admin.pricing.uncovered.days7') },
  { value: '30', label: t('admin.pricing.uncovered.days30') },
  { value: '90', label: t('admin.pricing.uncovered.days90') }
])

async function fetchUncovered() {
  uncoveredLoading.value = true
  try {
    uncovered.value = await adminAPI.pricing.getUncovered(Number(uncoveredDays.value) || 30)
    if (!uncovered.value.items) uncovered.value.items = []
  } catch (err: any) {
    appStore.showError(err?.message || t('common.error'))
  } finally {
    uncoveredLoading.value = false
  }
}

// --- 自定义价格 ---

const customList = ref<CustomModelPricing[]>([])
const customLoading = ref(false)
const editVisible = ref(false)
const editSaving = ref(false)
const editingId = ref<number | null>(null)

const editForm = reactive({
  modelsText: '',
  billing_mode: 'token',
  input_per_mtok: '',
  output_per_mtok: '',
  cache_write_per_mtok: '',
  cache_read_per_mtok: '',
  per_request_price: '',
  enabled: true,
  remark: ''
})

const billingModeOptions = computed(() => [
  { value: 'token', label: 'token' },
  { value: 'per_request', label: 'per_request' },
  { value: 'image', label: 'image' },
  { value: 'video', label: 'video' }
])

const editTitle = computed(() =>
  editingId.value ? t('admin.pricing.custom.editTitle') : t('admin.pricing.custom.addTitle')
)

async function fetchCustom() {
  customLoading.value = true
  try {
    customList.value = (await adminAPI.pricing.listCustom()) || []
  } catch (err: any) {
    appStore.showError(err?.message || t('common.error'))
  } finally {
    customLoading.value = false
  }
}

function openEdit(entry: CustomModelPricing | null) {
  editingId.value = entry?.id ?? null
  editForm.modelsText = entry?.models.join(', ') ?? ''
  editForm.billing_mode = entry?.billing_mode || 'token'
  editForm.input_per_mtok = priceToMtokInput(entry?.input_price)
  editForm.output_per_mtok = priceToMtokInput(entry?.output_price)
  editForm.cache_write_per_mtok = priceToMtokInput(entry?.cache_write_price)
  editForm.cache_read_per_mtok = priceToMtokInput(entry?.cache_read_price)
  editForm.per_request_price = priceToMtokInput(entry?.per_request_price)
  editForm.enabled = entry?.enabled ?? true
  editForm.remark = entry?.remark ?? ''
  editVisible.value = true
}

function openEditFromModel(model: string) {
  activeTab.value = 'custom'
  if (!customList.value.length) fetchCustom()
  openEdit(null)
  editForm.modelsText = model
}

function priceToMtokInput(perToken?: number | null): string {
  if (perToken == null) return ''
  return String(perToken * 1e6)
}

function mtokInputToPrice(value: string): number | null {
  const trimmed = value.trim()
  if (trimmed === '') return null
  const num = Number(trimmed)
  if (!Number.isFinite(num)) return null
  return num / 1e6
}

async function saveEdit() {
  const models = editForm.modelsText
    .split(',')
    .map((m) => m.trim())
    .filter(Boolean)
  if (!models.length) return
  editSaving.value = true
  const payload = {
    models,
    billing_mode: editForm.billing_mode,
    input_price: mtokInputToPrice(editForm.input_per_mtok),
    output_price: mtokInputToPrice(editForm.output_per_mtok),
    cache_write_price: mtokInputToPrice(editForm.cache_write_per_mtok),
    cache_read_price: mtokInputToPrice(editForm.cache_read_per_mtok),
    per_request_price: mtokInputToPrice(editForm.per_request_price),
    intervals: [],
    enabled: editForm.enabled,
    remark: editForm.remark
  }
  try {
    if (editingId.value) {
      await adminAPI.pricing.updateCustom(editingId.value, payload)
    } else {
      await adminAPI.pricing.createCustom(payload)
    }
    appStore.showSuccess(t('common.saved'))
    editVisible.value = false
    await fetchCustom()
    fetchStatus()
  } catch (err: any) {
    appStore.showError(err?.message || t('common.error'))
  } finally {
    editSaving.value = false
  }
}

const deleteVisible = ref(false)
const deleteTarget = ref<CustomModelPricing | null>(null)

function askDelete(entry: CustomModelPricing) {
  deleteTarget.value = entry
  deleteVisible.value = true
}

async function confirmDelete() {
  if (!deleteTarget.value) return
  try {
    await adminAPI.pricing.deleteCustom(deleteTarget.value.id)
    appStore.showSuccess(t('common.deleted'))
    deleteVisible.value = false
    await fetchCustom()
    fetchStatus()
  } catch (err: any) {
    appStore.showError(err?.message || t('common.error'))
  }
}

// --- 工具 ---

function sourceLabel(source: string): string {
  const key = String(source || '')
  if (key === 'custom') return t('admin.pricing.source.custom')
  if (key === 'litellm' || key === 'remote') return t('admin.pricing.source.remote')
  if (key === 'fallback') return t('admin.pricing.source.builtin')
  if (key === 'channel') return t('admin.pricing.source.channel')
  if (key === 'group') return t('admin.pricing.source.group')
  return key
}

function sourceBadgeClass(source: string): string {
  const cls = 'inline-flex rounded-full px-2 py-0.5 text-xs font-medium '
  if (source === 'custom') return cls + 'bg-purple-100 text-purple-700 dark:bg-purple-900/30 dark:text-purple-300'
  if (source === 'litellm' || source === 'remote') return cls + 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
  if (source === 'fallback') return cls + 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400'
  return cls + 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
}

function fmtPrice(value?: number): string {
  if (value == null || !Number.isFinite(value)) return '0.000000'
  // 保留 6 位有效小数，尾零裁剪
  return value.toFixed(6).replace(/\.?0+$/, '') || '0'
}

function formatTokens(value?: number): string {
  if (value == null) return '0'
  if (value >= 1e9) return (value / 1e9).toFixed(2) + 'B'
  if (value >= 1e6) return (value / 1e6).toFixed(2) + 'M'
  if (value >= 1e3) return (value / 1e3).toFixed(1) + 'K'
  return String(value)
}

function formatTime(value?: string): string {
  if (!value) return ''
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

onMounted(() => {
  fetchStatus()
})
</script>
