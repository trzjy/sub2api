<template>
  <AppLayout>
    <div class="w-full min-w-0 space-y-6 pb-8">
      <header
        class="page-header mb-0 rounded-3xl bg-white p-5 shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700 sm:p-6"
      >
        <h1 class="page-title flex items-center gap-2 text-xl font-black text-gray-900 dark:text-white">
          <span class="inline-flex h-8 w-8 items-center justify-center rounded-xl bg-amber-50 text-amber-500 dark:bg-amber-900/30 dark:text-amber-400">
            <Icon name="bell" size="sm" />
          </span>
          {{ t('admin.promoIntel.title') }}
        </h1>
        <p class="page-description mt-1.5 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.promoIntel.description') }}
        </p>
        <div class="mt-4 border-t border-gray-100 pt-4 dark:border-dark-700">
          <div class="tabs inline-flex w-full max-w-2xl flex-wrap sm:w-auto" role="tablist">
            <button
              type="button"
              role="tab"
              class="tab flex-1 sm:flex-none"
              :class="activeTab === 'items' ? 'tab-active' : ''"
              :aria-selected="activeTab === 'items'"
              @click="activeTab = 'items'"
            >
              {{ t('admin.promoIntel.tabs.items') }}
            </button>
            <button
              type="button"
              role="tab"
              class="tab flex-1 sm:flex-none"
              :class="activeTab === 'sources' ? 'tab-active' : ''"
              :aria-selected="activeTab === 'sources'"
              @click="activeTab = 'sources'"
            >
              {{ t('admin.promoIntel.tabs.sources') }}
            </button>
            <button
              type="button"
              role="tab"
              class="tab flex-1 sm:flex-none"
              :class="activeTab === 'settings' ? 'tab-active' : ''"
              :aria-selected="activeTab === 'settings'"
              @click="activeTab = 'settings'"
            >
              {{ t('admin.promoIntel.tabs.settings') }}
            </button>
          </div>
        </div>
      </header>

      <!-- ============ 情报 + 每日简报 ============ -->
      <template v-if="activeTab === 'items'">
        <div class="rounded-3xl bg-white p-5 shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700">
          <div class="flex flex-wrap items-center justify-between gap-3">
            <div class="flex items-center gap-2">
              <h2 class="text-sm font-bold text-gray-900 dark:text-white">{{ t('admin.promoIntel.briefing.title') }}</h2>
              <input v-model="briefingDate" type="date" class="input h-8 w-40 text-xs" @change="reloadBriefing" />
            </div>
            <div class="flex flex-wrap items-center gap-2 text-xs">
              <span class="rounded-lg bg-gray-100 px-2.5 py-1 font-semibold text-gray-700 dark:bg-dark-700 dark:text-gray-200">
                {{ t('admin.promoIntel.briefing.total', { n: briefing?.total ?? 0 }) }}
              </span>
              <span class="rounded-lg bg-red-50 px-2.5 py-1 font-semibold text-red-600 dark:bg-red-900/30 dark:text-red-300">
                {{ t('admin.promoIntel.briefing.high', { n: briefing?.high_count ?? 0 }) }}
              </span>
              <span class="rounded-lg bg-blue-50 px-2.5 py-1 font-semibold text-blue-600 dark:bg-blue-900/30 dark:text-blue-300">
                {{ t('admin.promoIntel.briefing.pending', { n: briefing?.pending ?? 0 }) }}
              </span>
            </div>
          </div>
          <div v-if="briefing && briefing.total > 0" class="mt-3 flex flex-wrap gap-1.5">
            <span
              v-for="(count, vendor) in briefing.by_vendor"
              :key="vendor"
              class="rounded-md bg-amber-50 px-2 py-0.5 text-xs font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
            >
              {{ vendor }} × {{ count }}
            </span>
          </div>
        </div>

        <TablePageLayout>
          <template #filters>
            <div class="flex flex-wrap items-center gap-2">
              <input
                v-model="itemFilters.search"
                type="text"
                class="input h-9 w-56 text-sm"
                :placeholder="t('admin.promoIntel.items.searchPlaceholder')"
                @input="debouncedItemReload"
              />
              <select v-model="itemFilters.vendor" class="input h-9 w-36 text-sm" @change="onItemFilterChange">
                <option value="">{{ t('admin.promoIntel.items.allVendors') }}</option>
                <option v-for="v in vendorOptions" :key="v" :value="v">{{ v }}</option>
              </select>
              <select v-model="itemFilters.category" class="input h-9 w-36 text-sm" @change="onItemFilterChange">
                <option value="">{{ t('admin.promoIntel.items.allCategories') }}</option>
                <option v-for="c in itemCategories" :key="c" :value="c">{{ categoryLabel(c) }}</option>
              </select>
              <select v-model="itemFilters.status" class="input h-9 w-32 text-sm" @change="onItemFilterChange">
                <option value="">{{ t('admin.promoIntel.items.allStatuses') }}</option>
                <option value="pending">{{ t('admin.promoIntel.status.pending') }}</option>
                <option value="useful">{{ t('admin.promoIntel.status.useful') }}</option>
                <option value="ignored">{{ t('admin.promoIntel.status.ignored') }}</option>
              </select>
              <button type="button" class="btn btn-secondary h-9" :disabled="itemsLoading" @click="reloadItems">
                {{ t('common.refresh') }}
              </button>
            </div>
          </template>

          <template #table>
            <DataTable :columns="itemColumns" :data="intelItems" :loading="itemsLoading">
              <template #cell-title="{ row }">
                <div class="max-w-md">
                  <div class="flex items-center gap-1.5">
                    <span
                      v-if="row.relevance === 'high'"
                      class="inline-flex shrink-0 rounded bg-red-100 px-1.5 py-0.5 text-[10px] font-bold text-red-600 dark:bg-red-900/40 dark:text-red-300"
                    >
                      HOT
                    </span>
                    <span class="font-medium text-gray-900 dark:text-white">{{ row.title }}</span>
                  </div>
                  <p v-if="row.summary" class="mt-0.5 line-clamp-2 text-xs text-gray-500 dark:text-gray-400">{{ row.summary }}</p>
                  <p v-if="row.discount_info" class="mt-0.5 text-xs font-semibold text-emerald-600 dark:text-emerald-400">
                    {{ row.discount_info }}
                  </p>
                  <p v-if="row.extract_status === 'pending'" class="mt-0.5 text-xs text-gray-400">
                    {{ t('admin.promoIntel.items.pendingExtract') }}
                  </p>
                </div>
              </template>

              <template #cell-vendor="{ row }">
                <span class="inline-flex items-center rounded-md bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-700 dark:bg-dark-700 dark:text-gray-200">
                  {{ row.vendor }}
                </span>
              </template>

              <template #cell-category="{ row }">
                <span class="text-xs text-gray-600 dark:text-gray-300">{{ categoryLabel(row.category) }}</span>
              </template>

              <template #cell-status="{ row }">
                <span
                  class="inline-flex items-center rounded-md px-2 py-0.5 text-xs font-medium"
                  :class="statusBadgeClass(row.status)"
                >
                  {{ statusLabel(row.status) }}
                </span>
              </template>

              <template #cell-digest_date="{ row }">
                <span class="text-xs text-gray-500 dark:text-gray-400">{{ row.digest_date }}</span>
              </template>

              <template #cell-actions="{ row }">
                <div class="flex items-center gap-1.5">
                  <a
                    v-if="row.url"
                    :href="row.url"
                    target="_blank"
                    rel="noopener noreferrer"
                    class="rounded-lg p-1.5 text-gray-400 transition hover:bg-gray-100 hover:text-blue-500 dark:hover:bg-dark-700"
                    :title="t('admin.promoIntel.items.viewSource')"
                  >
                    <Icon name="externalLink" size="sm" />
                  </a>
                  <button
                    v-if="row.status !== 'useful'"
                    type="button"
                    class="rounded-lg px-2 py-1 text-xs font-medium text-emerald-600 transition hover:bg-emerald-50 dark:hover:bg-emerald-900/30"
                    @click="setItemStatus(row, 'useful')"
                  >
                    {{ t('admin.promoIntel.status.useful') }}
                  </button>
                  <button
                    v-if="row.status !== 'ignored'"
                    type="button"
                    class="rounded-lg px-2 py-1 text-xs font-medium text-gray-400 transition hover:bg-gray-100 dark:hover:bg-dark-700"
                    @click="setItemStatus(row, 'ignored')"
                  >
                    {{ t('admin.promoIntel.status.ignoreAction') }}
                  </button>
                </div>
              </template>

              <template #empty>
                <EmptyState
                  :title="t('admin.promoIntel.items.emptyTitle')"
                  :description="t('admin.promoIntel.items.emptyDescription')"
                />
              </template>
            </DataTable>
          </template>

          <template #pagination>
            <Pagination
              v-if="itemPagination.total > 0"
              :page="itemPagination.page"
              :total="itemPagination.total"
              :page-size="itemPagination.page_size"
              @update:page="onItemPageChange"
              @update:pageSize="onItemPageSizeChange"
            />
          </template>
        </TablePageLayout>
      </template>

      <!-- ============ 资讯源 ============ -->
      <TablePageLayout v-if="activeTab === 'sources'">
        <template #filters>
          <div class="flex flex-wrap items-center gap-2">
            <input
              v-model="sourceFilters.search"
              type="text"
              class="input h-9 w-56 text-sm"
              :placeholder="t('admin.promoIntel.sources.searchPlaceholder')"
              @input="debouncedSourceReload"
            />
            <select v-model="sourceFilters.enabled" class="input h-9 w-32 text-sm" @change="onSourceFilterChange">
              <option value="">{{ t('admin.promoIntel.sources.allEnabled') }}</option>
              <option value="true">{{ t('common.enabled') }}</option>
              <option value="false">{{ t('common.disabled') }}</option>
            </select>
            <button type="button" class="btn btn-primary h-9" @click="openSourceDialog(null)">
              {{ t('admin.promoIntel.sources.createButton') }}
            </button>
          </div>
        </template>

        <template #table>
          <DataTable :columns="sourceColumns" :data="sources" :loading="sourcesLoading">
            <template #cell-name="{ row }">
              <div class="max-w-xs">
                <span class="font-medium text-gray-900 dark:text-white">{{ row.name }}</span>
                <p v-if="row.notes" class="mt-0.5 line-clamp-1 text-xs text-gray-400">{{ row.notes }}</p>
              </div>
            </template>

            <template #cell-vendor="{ row }">
              <span class="inline-flex items-center rounded-md bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-700 dark:bg-dark-700 dark:text-gray-200">
                {{ row.vendor }}
              </span>
            </template>

            <template #cell-category="{ row }">
              <span class="text-xs text-gray-600 dark:text-gray-300">{{ sourceCategoryLabel(row.category) }}</span>
            </template>

            <template #cell-url="{ row }">
              <a
                :href="row.url"
                target="_blank"
                rel="noopener noreferrer"
                class="block max-w-xs truncate text-xs text-blue-500 hover:underline"
                :title="row.url"
              >
                {{ row.url }}
              </a>
            </template>

            <template #cell-last_fetched_at="{ row }">
              <div class="text-xs">
                <p class="text-gray-500 dark:text-gray-400">{{ formatTime(row.last_fetched_at) }}</p>
                <p
                  v-if="row.last_status === 'error'"
                  class="mt-0.5 line-clamp-1 max-w-xs text-red-500"
                  :title="row.last_error"
                >
                  {{ row.last_error }}
                </p>
              </div>
            </template>

            <template #cell-enabled="{ row }">
              <Toggle :modelValue="row.enabled" @update:modelValue="toggleSource(row)" />
            </template>

            <template #cell-actions="{ row }">
              <div class="flex items-center gap-1.5">
                <button
                  type="button"
                  class="rounded-lg px-2 py-1 text-xs font-medium text-blue-500 transition hover:bg-blue-50 disabled:opacity-50 dark:hover:bg-blue-900/30"
                  :disabled="fetchingId === row.id"
                  @click="fetchNow(row)"
                >
                  {{ fetchingId === row.id ? t('admin.promoIntel.sources.fetching') : t('admin.promoIntel.sources.fetchNow') }}
                </button>
                <button
                  type="button"
                  class="rounded-lg px-2 py-1 text-xs font-medium text-gray-500 transition hover:bg-gray-100 dark:hover:bg-dark-700"
                  @click="openSourceDialog(row)"
                >
                  {{ t('common.edit') }}
                </button>
                <button
                  type="button"
                  class="rounded-lg px-2 py-1 text-xs font-medium text-red-500 transition hover:bg-red-50 dark:hover:bg-red-900/30"
                  @click="confirmDeleteSource(row)"
                >
                  {{ t('common.delete') }}
                </button>
              </div>
            </template>

            <template #empty>
              <EmptyState
                :title="t('admin.promoIntel.sources.emptyTitle')"
                :description="t('admin.promoIntel.sources.emptyDescription')"
              />
            </template>
          </DataTable>
        </template>

        <template #pagination>
          <Pagination
            v-if="sourcePagination.total > 0"
            :page="sourcePagination.page"
            :total="sourcePagination.total"
            :page-size="sourcePagination.page_size"
            @update:page="onSourcePageChange"
            @update:pageSize="onSourcePageSizeChange"
          />
        </template>
      </TablePageLayout>

      <!-- ============ 设置 ============ -->
      <div
        v-if="activeTab === 'settings'"
        class="max-w-2xl rounded-3xl bg-white p-6 shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700"
      >
        <h2 class="text-sm font-bold text-gray-900 dark:text-white">{{ t('admin.promoIntel.settings.llmTitle') }}</h2>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promoIntel.settings.llmHint') }}</p>

        <div class="mt-5 space-y-4">
          <div class="flex items-center justify-between">
            <label class="input-label mb-0">{{ t('admin.promoIntel.settings.enabled') }}</label>
            <Toggle :modelValue="settingsForm.enabled" @update:modelValue="settingsForm.enabled = !settingsForm.enabled" />
          </div>

          <div>
            <label class="input-label">{{ t('admin.promoIntel.settings.baseUrl') }}</label>
            <input
              v-model.trim="settingsForm.llm_base_url"
              type="text"
              class="input font-mono text-xs"
              placeholder="https://api.deepseek.com/v1"
            />
          </div>

          <div>
            <label class="input-label">{{ t('admin.promoIntel.settings.apiKey') }}</label>
            <input
              v-model.trim="settingsForm.llm_api_key"
              type="password"
              autocomplete="new-password"
              class="input font-mono text-xs"
              :placeholder="settings?.llm_api_key_set ? settings.llm_api_key_mask : t('admin.promoIntel.settings.apiKeyPlaceholder')"
            />
            <p class="mt-1 text-xs text-gray-400">{{ t('admin.promoIntel.settings.apiKeyHint') }}</p>
          </div>

          <div>
            <label class="input-label">{{ t('admin.promoIntel.settings.model') }}</label>
            <input
              v-model.trim="settingsForm.llm_model"
              type="text"
              class="input font-mono text-xs"
              placeholder="deepseek-chat"
            />
          </div>
        </div>

        <div class="mt-6 flex items-center gap-3">
          <button type="button" class="btn btn-primary" :disabled="settingsSaving" @click="saveSettings">
            {{ t('common.save') }}
          </button>
          <button type="button" class="btn btn-secondary" :disabled="settingsTesting" @click="testLlm">
            {{ settingsTesting ? t('admin.promoIntel.settings.testing') : t('admin.promoIntel.settings.test') }}
          </button>
          <span v-if="settings?.llm_configured" class="text-xs text-emerald-600 dark:text-emerald-400">
            {{ t('admin.promoIntel.settings.configured') }}
          </span>
          <span v-else class="text-xs text-gray-400">{{ t('admin.promoIntel.settings.notConfigured') }}</span>
        </div>
      </div>
    </div>

    <!-- 资讯源新建/编辑 -->
    <BaseDialog
      :show="showSourceDialog"
      :title="editingSource ? t('admin.promoIntel.sources.editTitle') : t('admin.promoIntel.sources.createTitle')"
      width="wide"
      @close="showSourceDialog = false"
    >
      <div class="space-y-4">
        <div>
          <label class="input-label">{{ t('admin.promoIntel.sources.form.name') }}</label>
          <input v-model.trim="sourceForm.name" type="text" class="input" />
        </div>
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div>
            <label class="input-label">{{ t('admin.promoIntel.sources.form.vendor') }}</label>
            <input v-model.trim="sourceForm.vendor" type="text" class="input font-mono text-xs" list="promo-intel-vendors" />
            <datalist id="promo-intel-vendors">
              <option v-for="v in vendorOptions" :key="v" :value="v" />
            </datalist>
          </div>
          <div>
            <label class="input-label">{{ t('admin.promoIntel.sources.form.category') }}</label>
            <select v-model="sourceForm.category" class="input">
              <option v-for="c in sourceCategories" :key="c" :value="c">{{ sourceCategoryLabel(c) }}</option>
            </select>
          </div>
        </div>
        <div>
          <label class="input-label">{{ t('admin.promoIntel.sources.form.url') }}</label>
          <input v-model.trim="sourceForm.url" type="text" class="input font-mono text-xs" placeholder="https://" />
        </div>
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div>
            <label class="input-label">{{ t('admin.promoIntel.sources.form.interval') }}</label>
            <input v-model.number="sourceForm.fetch_interval_minutes" type="number" min="30" max="43200" class="input" />
            <p class="mt-1 text-xs text-gray-400">{{ t('admin.promoIntel.sources.form.intervalHint') }}</p>
          </div>
          <div class="flex items-end gap-6">
            <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-200">
              <Toggle :modelValue="sourceForm.enabled" @update:modelValue="sourceForm.enabled = !sourceForm.enabled" />
              {{ t('admin.promoIntel.sources.form.enabled') }}
            </label>
            <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-200">
              <Toggle :modelValue="sourceForm.llm_extract" @update:modelValue="sourceForm.llm_extract = !sourceForm.llm_extract" />
              {{ t('admin.promoIntel.sources.form.llmExtract') }}
            </label>
          </div>
        </div>
        <div>
          <label class="input-label">{{ t('admin.promoIntel.sources.form.notes') }}</label>
          <textarea v-model="sourceForm.notes" class="input min-h-[64px] text-xs"></textarea>
        </div>
      </div>
      <template #footer>
        <div class="flex justify-end gap-3">
          <button type="button" class="btn btn-secondary" @click="showSourceDialog = false">{{ t('common.cancel') }}</button>
          <button type="button" class="btn btn-primary" :disabled="sourceSaving || !sourceForm.name || !sourceForm.vendor || !sourceForm.url" @click="saveSource">
            {{ t('common.save') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <ConfirmDialog
      :show="showDeleteDialog"
      :title="t('common.delete')"
      :message="deleteConfirmMessage"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="doDeleteSource"
      @cancel="showDeleteDialog = false"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { adminAPI } from '@/api/admin'
import type {
  Briefing,
  ItemListParams,
  PromoIntelItem,
  PromoIntelItemCategory,
  PromoIntelItemStatus,
  PromoIntelSource,
  PromoIntelSourceCategory,
  SourceListParams,
} from '@/api/admin/promoIntel'
import type { Column } from '@/components/common/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import Toggle from '@/components/common/Toggle.vue'
import { getPersistedPageSize } from '@/composables/usePersistedPageSize'

const { t } = useI18n()
const appStore = useAppStore()

const activeTab = ref<'items' | 'sources' | 'settings'>('items')

// ---------- 情报列表 + 简报 ----------

const intelItems = ref<PromoIntelItem[]>([])
const itemsLoading = ref(false)
const briefing = ref<Briefing | null>(null)
const briefingDate = ref(todayStr())

const itemFilters = reactive({
  search: '',
  vendor: '',
  category: '' as PromoIntelItemCategory | '',
  status: '' as PromoIntelItemStatus | '',
})
const itemPagination = reactive({ page: 1, page_size: getPersistedPageSize(), total: 0 })

const itemCategories: PromoIntelItemCategory[] = [
  'free_quota', 'discount', 'subscription', 'price_change', 'new_model', 'new_product', 'event', 'policy', 'other',
]
const vendorOptions = [
  'deepseek', 'kimi', 'zhipu', 'minimax', 'volcano', 'alibaba', 'baidu', 'tencent', 'stepfun',
  'siliconflow', 'ppio', 'dmxapi', 'aihubmix',
  'openai', 'anthropic', 'google', 'xai', 'mistral', 'openrouter', 'together', 'fireworks',
  'groq', 'deepinfra', 'github', 'cursor', 'qoder', 'augment', 'zed',
]
const sourceCategories: PromoIntelSourceCategory[] = ['announcement', 'changelog', 'pricing', 'activity', 'blog']

const itemColumns = computed<Column[]>(() => [
  { key: 'title', label: t('admin.promoIntel.items.columns.title'), sortable: false },
  { key: 'vendor', label: t('admin.promoIntel.items.columns.vendor'), sortable: false },
  { key: 'category', label: t('admin.promoIntel.items.columns.category'), sortable: false },
  { key: 'status', label: t('admin.promoIntel.items.columns.status'), sortable: false },
  { key: 'digest_date', label: t('admin.promoIntel.items.columns.date'), sortable: false },
  { key: 'actions', label: t('admin.promoIntel.items.columns.actions'), sortable: false },
])

function categoryLabel(c: string): string {
  return t(`admin.promoIntel.category.${c}`)
}
function sourceCategoryLabel(c: string): string {
  return t(`admin.promoIntel.sourceCategory.${c}`)
}
function statusLabel(s: string): string {
  return t(`admin.promoIntel.status.${s}`)
}
function statusBadgeClass(s: string): string {
  switch (s) {
    case 'useful':
      return 'bg-emerald-50 text-emerald-600 dark:bg-emerald-900/30 dark:text-emerald-300'
    case 'ignored':
      return 'bg-gray-100 text-gray-400 dark:bg-dark-700 dark:text-gray-500'
    default:
      return 'bg-blue-50 text-blue-600 dark:bg-blue-900/30 dark:text-blue-300'
  }
}

async function reloadBriefing() {
  try {
    briefing.value = await adminAPI.promoIntel.getBriefing(briefingDate.value || undefined)
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.promoIntel.items.loadError')))
  }
}

let itemsAbort: AbortController | null = null
async function reloadItems() {
  if (itemsAbort) itemsAbort.abort()
  const ctrl = new AbortController()
  itemsAbort = ctrl
  itemsLoading.value = true
  try {
    const params: ItemListParams = {
      page: itemPagination.page,
      page_size: itemPagination.page_size,
    }
    if (itemFilters.search.trim()) params.search = itemFilters.search.trim()
    if (itemFilters.vendor) params.vendor = itemFilters.vendor
    if (itemFilters.category) params.category = itemFilters.category
    if (itemFilters.status) params.status = itemFilters.status
    const res = await adminAPI.promoIntel.listItems(params, { signal: ctrl.signal })
    if (ctrl.signal.aborted || itemsAbort !== ctrl) return
    intelItems.value = res.items || []
    itemPagination.total = res.total
  } catch (err: unknown) {
    const e = err as { name?: string; code?: string }
    if (e?.name === 'AbortError' || e?.code === 'ERR_CANCELED') return
    appStore.showError(extractApiErrorMessage(err, t('admin.promoIntel.items.loadError')))
  } finally {
    if (itemsAbort === ctrl) {
      itemsLoading.value = false
      itemsAbort = null
    }
  }
}

async function setItemStatus(row: PromoIntelItem, status: PromoIntelItemStatus) {
  try {
    const updated = await adminAPI.promoIntel.updateItemStatus(row.id, status)
    row.status = updated.status
    void reloadBriefing()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

function onItemFilterChange() {
  itemPagination.page = 1
  reloadItems()
}
function onItemPageChange(page: number) {
  itemPagination.page = page
  reloadItems()
}
function onItemPageSizeChange(size: number) {
  itemPagination.page_size = size
  itemPagination.page = 1
  reloadItems()
}

let itemSearchTimer: ReturnType<typeof setTimeout> | null = null
function debouncedItemReload() {
  if (itemSearchTimer) clearTimeout(itemSearchTimer)
  itemSearchTimer = setTimeout(() => {
    itemPagination.page = 1
    reloadItems()
  }, 300)
}

// ---------- 资讯源 ----------

const sources = ref<PromoIntelSource[]>([])
const sourcesLoading = ref(false)
const sourceFilters = reactive({ search: '', enabled: '' as '' | 'true' | 'false' })
const sourcePagination = reactive({ page: 1, page_size: getPersistedPageSize(), total: 0 })
const fetchingId = ref<number | null>(null)

const showSourceDialog = ref(false)
const editingSource = ref<PromoIntelSource | null>(null)
const sourceSaving = ref(false)
const sourceForm = reactive({
  name: '',
  vendor: '',
  category: 'announcement' as PromoIntelSourceCategory,
  url: '',
  fetch_interval_minutes: 1440,
  enabled: true,
  llm_extract: true,
  notes: '',
})

const showDeleteDialog = ref(false)
const deletingSource = ref<PromoIntelSource | null>(null)
const deleteConfirmMessage = computed(() =>
  t('admin.promoIntel.sources.deleteConfirm', { name: deletingSource.value?.name || '' })
)

const sourceColumns = computed<Column[]>(() => [
  { key: 'name', label: t('admin.promoIntel.sources.columns.name'), sortable: false },
  { key: 'vendor', label: t('admin.promoIntel.sources.columns.vendor'), sortable: false },
  { key: 'category', label: t('admin.promoIntel.sources.columns.category'), sortable: false },
  { key: 'url', label: t('admin.promoIntel.sources.columns.url'), sortable: false },
  { key: 'last_fetched_at', label: t('admin.promoIntel.sources.columns.lastFetched'), sortable: false },
  { key: 'enabled', label: t('admin.promoIntel.sources.columns.enabled'), sortable: false },
  { key: 'actions', label: t('admin.promoIntel.sources.columns.actions'), sortable: false },
])

let sourcesAbort: AbortController | null = null
async function reloadSources() {
  if (sourcesAbort) sourcesAbort.abort()
  const ctrl = new AbortController()
  sourcesAbort = ctrl
  sourcesLoading.value = true
  try {
    const params: SourceListParams = {
      page: sourcePagination.page,
      page_size: sourcePagination.page_size,
    }
    if (sourceFilters.search.trim()) params.search = sourceFilters.search.trim()
    if (sourceFilters.enabled === 'true') params.enabled = true
    if (sourceFilters.enabled === 'false') params.enabled = false
    const res = await adminAPI.promoIntel.listSources(params, { signal: ctrl.signal })
    if (ctrl.signal.aborted || sourcesAbort !== ctrl) return
    sources.value = res.items || []
    sourcePagination.total = res.total
  } catch (err: unknown) {
    const e = err as { name?: string; code?: string }
    if (e?.name === 'AbortError' || e?.code === 'ERR_CANCELED') return
    appStore.showError(extractApiErrorMessage(err, t('admin.promoIntel.sources.loadError')))
  } finally {
    if (sourcesAbort === ctrl) {
      sourcesLoading.value = false
      sourcesAbort = null
    }
  }
}

function openSourceDialog(row: PromoIntelSource | null) {
  editingSource.value = row
  sourceForm.name = row?.name ?? ''
  sourceForm.vendor = row?.vendor ?? ''
  sourceForm.category = row?.category ?? 'announcement'
  sourceForm.url = row?.url ?? ''
  sourceForm.fetch_interval_minutes = row?.fetch_interval_minutes ?? 1440
  sourceForm.enabled = row?.enabled ?? true
  sourceForm.llm_extract = row?.llm_extract ?? true
  sourceForm.notes = row?.notes ?? ''
  showSourceDialog.value = true
}

async function saveSource() {
  sourceSaving.value = true
  try {
    if (editingSource.value) {
      await adminAPI.promoIntel.updateSource(editingSource.value.id, { ...sourceForm })
      appStore.showSuccess(t('admin.promoIntel.sources.saveSuccess'))
    } else {
      await adminAPI.promoIntel.createSource({ ...sourceForm })
      appStore.showSuccess(t('admin.promoIntel.sources.createSuccess'))
    }
    showSourceDialog.value = false
    reloadSources()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  } finally {
    sourceSaving.value = false
  }
}

async function toggleSource(row: PromoIntelSource) {
  const next = !row.enabled
  try {
    await adminAPI.promoIntel.updateSource(row.id, { enabled: next })
    row.enabled = next
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

async function fetchNow(row: PromoIntelSource) {
  if (fetchingId.value != null) return
  fetchingId.value = row.id
  try {
    const res = await adminAPI.promoIntel.fetchSourceNow(row.id)
    if (!res.content_changed) {
      appStore.showSuccess(t('admin.promoIntel.sources.fetchUnchanged'))
    } else if (res.items_created > 0 || res.items_updated > 0) {
      appStore.showSuccess(t('admin.promoIntel.sources.fetchSuccess', { created: res.items_created, updated: res.items_updated }))
    } else if (res.skipped_reason) {
      appStore.showSuccess(t(`admin.promoIntel.sources.skip.${res.skipped_reason}`))
    } else {
      appStore.showSuccess(t('admin.promoIntel.sources.fetchNoItems'))
    }
    reloadSources()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.promoIntel.sources.fetchFailed')))
  } finally {
    fetchingId.value = null
  }
}

function confirmDeleteSource(row: PromoIntelSource) {
  deletingSource.value = row
  showDeleteDialog.value = true
}

async function doDeleteSource() {
  if (!deletingSource.value) return
  try {
    await adminAPI.promoIntel.deleteSource(deletingSource.value.id)
    appStore.showSuccess(t('admin.promoIntel.sources.deleteSuccess'))
    showDeleteDialog.value = false
    deletingSource.value = null
    reloadSources()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

function onSourceFilterChange() {
  sourcePagination.page = 1
  reloadSources()
}
function onSourcePageChange(page: number) {
  sourcePagination.page = page
  reloadSources()
}
function onSourcePageSizeChange(size: number) {
  sourcePagination.page_size = size
  sourcePagination.page = 1
  reloadSources()
}

let sourceSearchTimer: ReturnType<typeof setTimeout> | null = null
function debouncedSourceReload() {
  if (sourceSearchTimer) clearTimeout(sourceSearchTimer)
  sourceSearchTimer = setTimeout(() => {
    sourcePagination.page = 1
    reloadSources()
  }, 300)
}

// ---------- 设置 ----------

const settings = ref<Awaited<ReturnType<typeof adminAPI.promoIntel.getSettings>> | null>(null)
const settingsForm = reactive({
  enabled: true,
  llm_base_url: '',
  llm_api_key: '',
  llm_model: '',
})
const settingsSaving = ref(false)
const settingsTesting = ref(false)

async function reloadSettings() {
  try {
    const s = await adminAPI.promoIntel.getSettings()
    settings.value = s
    settingsForm.enabled = s.enabled
    settingsForm.llm_base_url = s.llm_base_url || ''
    settingsForm.llm_api_key = ''
    settingsForm.llm_model = s.llm_model || ''
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

async function saveSettings() {
  settingsSaving.value = true
  try {
    const params: Record<string, unknown> = { enabled: settingsForm.enabled }
    if (settingsForm.llm_base_url !== (settings.value?.llm_base_url || '')) {
      params.llm_base_url = settingsForm.llm_base_url
    }
    if (settingsForm.llm_api_key) params.llm_api_key = settingsForm.llm_api_key
    if (settingsForm.llm_model !== (settings.value?.llm_model || '')) {
      params.llm_model = settingsForm.llm_model
    }
    settings.value = await adminAPI.promoIntel.updateSettings(params)
    settingsForm.llm_api_key = ''
    appStore.showSuccess(t('admin.promoIntel.settings.saveSuccess'))
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  } finally {
    settingsSaving.value = false
  }
}

async function testLlm() {
  settingsTesting.value = true
  try {
    const res = await adminAPI.promoIntel.testSettings()
    appStore.showSuccess(res.message || t('admin.promoIntel.settings.testSuccess'))
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.promoIntel.settings.testFailed')))
  } finally {
    settingsTesting.value = false
  }
}

// ---------- 工具 ----------

function formatTime(value: string | null): string {
  if (!value) return t('admin.promoIntel.sources.neverFetched')
  try {
    return new Date(value).toLocaleString()
  } catch {
    return value
  }
}

function todayStr(): string {
  const d = new Date()
  const m = `${d.getMonth() + 1}`.padStart(2, '0')
  const day = `${d.getDate()}`.padStart(2, '0')
  return `${d.getFullYear()}-${m}-${day}`
}

onMounted(() => {
  void reloadBriefing()
  void reloadItems()
})

onUnmounted(() => {
  itemsAbort?.abort()
  sourcesAbort?.abort()
  if (itemSearchTimer) clearTimeout(itemSearchTimer)
  if (sourceSearchTimer) clearTimeout(sourceSearchTimer)
})

// Tab 切换时懒加载对应数据。
import { watch } from 'vue'
watch(activeTab, (tab) => {
  if (tab === 'sources' && sources.value.length === 0) void reloadSources()
  if (tab === 'settings' && settings.value === null) void reloadSettings()
})
</script>
