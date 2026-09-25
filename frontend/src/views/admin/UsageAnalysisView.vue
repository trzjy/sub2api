<template>
  <AppLayout>
    <TablePageLayout>
      <!-- Filters -->
      <template #filters>
        <div class="card p-4 sm:p-6">
          <div class="flex flex-wrap items-end justify-between gap-4">
            <div class="flex flex-1 flex-wrap items-end gap-4">
              <div class="w-full sm:w-auto sm:min-w-[170px]">
                <label class="input-label">{{ t('admin.usageAnalysis.filters.date') }}</label>
                <input v-model="filters.date" type="date" class="input" @change="search" />
              </div>

              <div class="w-full sm:w-auto sm:min-w-[170px]">
                <label class="input-label">{{ t('admin.usageAnalysis.filters.level') }}</label>
                <Select v-model="filters.level" :options="levelOptions" @change="search" />
              </div>

              <div class="w-full sm:w-auto sm:min-w-[150px]">
                <label class="input-label">{{ t('admin.usageAnalysis.filters.user') }}</label>
                <input
                  v-model.trim="filters.user_id"
                  type="number"
                  min="1"
                  class="input"
                  :placeholder="t('admin.usageAnalysis.filters.user')"
                  @keyup.enter="search"
                />
              </div>

              <div class="w-full sm:w-auto sm:min-w-[150px]">
                <label class="input-label">{{ t('admin.usageAnalysis.filters.group') }}</label>
                <input
                  v-model.trim="filters.group_id"
                  type="number"
                  min="1"
                  class="input"
                  :placeholder="t('admin.usageAnalysis.filters.group')"
                  @keyup.enter="search"
                />
              </div>

              <div class="w-full sm:w-auto sm:min-w-[200px]">
                <label class="input-label">{{ t('admin.usageAnalysis.filters.rule') }}</label>
                <input
                  v-model.trim="filters.rule"
                  type="text"
                  class="input"
                  :placeholder="t('admin.usageAnalysis.filters.rule')"
                  @keyup.enter="search"
                />
              </div>

              <div class="flex items-end">
                <label class="flex cursor-pointer items-center gap-2 pb-2 text-sm text-gray-700 dark:text-gray-300">
                  <input v-model="filters.include_low" type="checkbox" class="h-4 w-4" @change="search" />
                  {{ t('admin.usageAnalysis.filters.includeLow') }}
                </label>
              </div>
            </div>

            <div class="flex w-full flex-wrap items-center justify-end gap-3 sm:w-auto">
              <button type="button" class="btn btn-primary" :disabled="loading" @click="search">
                {{ t('admin.usageAnalysis.filters.search') }}
              </button>
              <button type="button" class="btn btn-secondary" :disabled="loading" @click="resetFilters">
                {{ t('admin.usageAnalysis.filters.reset') }}
              </button>
            </div>
          </div>
        </div>
      </template>

      <!-- Freshness bar + entry threshold -->
      <template #table>
        <div class="mb-4 space-y-3">
          <!-- Freshness bar -->
          <div class="card flex flex-wrap items-center gap-x-6 gap-y-2 p-4">
            <div class="flex items-center gap-2">
              <Icon name="shield" size="sm" class="text-primary-600 dark:text-primary-400" />
              <span class="text-sm font-semibold text-gray-700 dark:text-gray-200">
                {{ t('admin.usageAnalysis.freshness.title') }}
              </span>
            </div>

            <div v-if="runStatus" class="flex flex-wrap items-center gap-x-5 gap-y-1.5 text-xs">
              <span class="inline-flex items-center gap-1.5">
                {{ t('admin.usageAnalysis.freshness.status') }}:
                <span :class="runStatusBadgeClass(runStatus.status)">
                  <span class="h-1.5 w-1.5 rounded-full" :class="runStatusDotClass(runStatus.status)"></span>
                  {{ runStatusLabels[runStatus.status] }}
                </span>
              </span>
              <span v-if="runStatus.window_end" class="text-gray-500 dark:text-gray-400">
                {{ t('admin.usageAnalysis.freshness.windowEnd') }}:
                <span class="font-medium text-gray-700 dark:text-gray-200">{{ formatTime(runStatus.window_end) }}</span>
              </span>
              <span class="text-gray-500 dark:text-gray-400">
                {{ t('admin.usageAnalysis.freshness.consecutivePartials') }}:
                <span class="font-medium text-gray-700 dark:text-gray-200">{{ runStatus.consecutive_partials }}</span>
              </span>
              <span class="text-gray-500 dark:text-gray-400">
                {{ t('admin.usageAnalysis.freshness.failedBatches') }}:
                <span class="font-medium text-gray-700 dark:text-gray-200">{{ runStatus.failed_batches }}</span>
              </span>
              <span
                v-if="!runStatus.history_covered"
                class="inline-flex items-center gap-1.5 rounded-full bg-amber-100 px-2.5 py-0.5 font-semibold text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
              >
                {{ t('admin.usageAnalysis.freshness.historyCovering') }} · {{ runStatus.recon_progress }}
              </span>
              <span
                v-else-if="runStatus.r1_reeval_pending"
                class="inline-flex items-center gap-1.5 rounded-full bg-amber-100 px-2.5 py-0.5 font-semibold text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
              >
                {{ t('admin.usageAnalysis.freshness.historyReevalPending') }} · {{ runStatus.recon_progress }}
              </span>
              <span
                v-else
                class="inline-flex items-center gap-1.5 rounded-full bg-green-100 px-2.5 py-0.5 font-semibold text-green-700 dark:bg-green-900/30 dark:text-green-300"
              >
                {{ t('admin.usageAnalysis.freshness.historyCovered') }}
              </span>
              <span
                v-if="runStatus.error"
                class="inline-flex items-center gap-1.5 rounded-full bg-red-100 px-2.5 py-0.5 font-semibold text-red-700 dark:bg-red-900/30 dark:text-red-300"
              >
                {{ t('admin.usageAnalysis.freshness.error') }}: {{ runStatus.error }}
              </span>
            </div>
            <div v-else-if="runStatusLoading" class="text-xs text-gray-400">
              {{ t('admin.usageAnalysis.freshness.status') }}…
            </div>

            <button
              type="button"
              class="btn btn-secondary ml-auto py-1.5"
              :disabled="runStatusLoading"
              @click="fetchRunStatus"
            >
              <Icon name="refresh" size="xs" />
              {{ t('admin.usageAnalysis.freshness.refresh') }}
            </button>
          </div>

          <!-- Entry threshold (consumed from backend, never hard-coded) -->
          <div
            v-if="minScore !== null"
            class="flex items-center gap-2 rounded-lg bg-gray-50 px-3 py-2 text-xs text-gray-600 ring-1 ring-gray-200 dark:bg-dark-800 dark:text-gray-300 dark:ring-dark-600"
          >
            <Icon name="infoCircle" size="xs" class="text-gray-400" />
            {{ t('admin.usageAnalysis.includeLine', { score: minScore }) }}
            <span class="text-gray-400">·</span>
            {{ t('admin.usageAnalysis.includeLowHint') }}
          </div>
        </div>

        <!-- Table -->
        <DataTable
          :columns="columns"
          :data="displayReports"
          :loading="loading"
          row-key="report_id"
          clickable-rows
          @row-click="(row: Report) => openDetail(row.report_id)"
        >
          <template #cell-username="{ row }">
            <span class="font-medium text-gray-900 dark:text-white">{{ row.username || '—' }}</span>
          </template>

          <template #cell-group_name="{ value }">
            <span class="text-gray-600 dark:text-gray-300">{{ value || '—' }}</span>
          </template>

          <template #cell-report_date="{ value }">
            <span class="whitespace-nowrap text-gray-600 dark:text-gray-300">{{ formatDate(value) }}</span>
          </template>

          <template #cell-score="{ value }">
            <span class="font-semibold text-gray-900 dark:text-white">{{ value }}</span>
          </template>

          <template #cell-level="{ row }">
            <span :class="levelBadgeClass(row.level)">
              <span class="h-1.5 w-1.5 rounded-full" :class="levelDotClass(row.level)"></span>
              {{ levelLabels[row.level] }}
            </span>
          </template>

          <template #cell-rulesSummary="{ row }">
            <span class="text-xs text-gray-600 dark:text-gray-300">{{ ruleSummary(row) }}</span>
          </template>

          <template #cell-status="{ row }">
            <span :class="statusBadgeClass(row.status)">
              {{ statusLabels[row.status] }}
            </span>
          </template>

          <template #cell-actions="{ row }">
            <button
              type="button"
              class="inline-flex items-center gap-1 font-medium text-primary-600 transition-colors hover:text-primary-700 dark:text-primary-400 dark:hover:text-primary-300"
              @click.stop="openDetail(row.report_id)"
            >
              <Icon name="eye" size="sm" />
              {{ t('admin.usageAnalysis.viewDetail') }}
            </button>
          </template>

          <template #empty>
            <div class="flex flex-col items-center py-8">
              <Icon name="shield" size="xl" class="mb-4 h-12 w-12 text-gray-300 dark:text-dark-600" />
              <p class="text-sm font-medium text-gray-500 dark:text-gray-400">
                {{ t('admin.usageAnalysis.listEmpty') }}
              </p>
            </div>
          </template>
        </DataTable>
      </template>

      <!-- Pagination -->
      <template #pagination>
        <Pagination
          v-if="total > 0"
          :total="total"
          :page="page"
          :page-size="pageSize"
          @update:page="onPageChange"
          @update:pageSize="onPageSizeChange"
        />
      </template>
    </TablePageLayout>

    <!-- Detail dialog with evidence drill-down -->
    <BaseDialog
      :show="detailVisible"
      :title="t('admin.usageAnalysis.detail.title')"
      width="wide"
      :close-on-click-outside="true"
      @close="detailVisible = false"
    >
      <div v-if="detailLoading" class="flex items-center justify-center py-16">
        <div class="flex flex-col items-center gap-3">
          <div class="h-8 w-8 animate-spin rounded-full border-b-2 border-primary-600"></div>
          <div class="text-sm font-medium text-gray-500 dark:text-gray-400">{{ t('common.loading') }}</div>
        </div>
      </div>

      <div v-else-if="detail" class="space-y-5 py-2">
        <!-- Hero meta -->
        <div class="rounded-2xl border border-gray-200 bg-gray-50/60 p-5 dark:border-dark-700 dark:bg-dark-900/60">
          <div class="flex flex-wrap items-center gap-3">
            <span :class="levelBadgeClass(detail.level)">
              <span class="h-1.5 w-1.5 rounded-full" :class="levelDotClass(detail.level)"></span>
              {{ levelLabels[detail.level] }}
            </span>
            <span class="text-2xl font-bold text-gray-900 dark:text-white">{{ detail.score }}</span>
            <span :class="statusBadgeClass(detail.status)">{{ statusLabels[detail.status] }}</span>
          </div>
          <div class="mt-3 grid grid-cols-2 gap-3 sm:grid-cols-4">
            <div>
              <div class="text-xs font-bold uppercase tracking-wider text-gray-400">
                {{ t('admin.usageAnalysis.detail.user') }}
              </div>
              <div class="mt-1 text-sm font-medium text-gray-900 dark:text-white">
                {{ detail.username || '—' }}
              </div>
            </div>
            <div>
              <div class="text-xs font-bold uppercase tracking-wider text-gray-400">
                {{ t('admin.usageAnalysis.detail.group') }}
              </div>
              <div class="mt-1 text-sm font-medium text-gray-900 dark:text-white">
                {{ detail.group_name || '—' }}
              </div>
            </div>
            <div>
              <div class="text-xs font-bold uppercase tracking-wider text-gray-400">
                {{ t('admin.usageAnalysis.detail.date') }}
              </div>
              <div class="mt-1 text-sm font-medium text-gray-900 dark:text-white">
                {{ formatDate(detail.report_date) }}
              </div>
            </div>
            <div>
              <div class="text-xs font-bold uppercase tracking-wider text-gray-400">
                {{ t('admin.usageAnalysis.detail.reportId') }}
              </div>
              <div class="mt-1 text-sm font-medium text-gray-900 dark:text-white">
                {{ detail.report_id }}
              </div>
            </div>
          </div>
          <div
            v-if="detail.invalidated_at"
            class="mt-2 text-xs text-gray-400"
          >
            {{ t('admin.usageAnalysis.detail.invalidatedAt') }}: {{ formatTime(detail.invalidated_at) }}
          </div>
          <div class="mt-1 text-xs text-gray-400">
            {{ t('admin.usageAnalysis.detail.policyVersion') }}: {{ detail.policy_version }}
          </div>
        </div>

        <!-- Status actions -->
        <div
          v-if="availableActions(detail.status).length"
          class="flex flex-wrap items-center gap-3 rounded-xl bg-gray-50 p-4 dark:bg-dark-900"
        >
          <span class="text-xs font-bold uppercase tracking-wider text-gray-400">
            {{ t('admin.usageAnalysis.detail.status') }}
          </span>
          <button
            v-for="action in availableActions(detail.status)"
            :key="action"
            type="button"
            class="btn btn-primary py-1.5"
            :disabled="updatingId !== null"
            @click="changeStatus(detail.report_id, action)"
          >
            <Icon v-if="action === 'acknowledged'" name="check" size="xs" />
            <Icon v-else-if="action === 'dismissed'" name="x" size="xs" />
            <Icon v-else name="checkCircle" size="xs" />
            {{ updatingId === detail.report_id ? t('admin.usageAnalysis.actions.updating') : statusLabels[action] }}
          </button>
        </div>

        <!-- Rule hits -->
        <section>
          <h4 class="mb-2 text-xs font-bold uppercase tracking-wider text-gray-400">
            {{ t('admin.usageAnalysis.detail.rules') }}
          </h4>
          <div v-if="detail.rule_hits && detail.rule_hits.length" class="overflow-hidden rounded-xl ring-1 ring-gray-200 dark:ring-dark-600">
            <table class="w-full text-left text-sm">
              <thead class="bg-gray-50 text-xs uppercase text-gray-400 dark:bg-dark-900">
                <tr>
                  <th class="px-3 py-2">{{ t('admin.usageAnalysis.detail.rule') }}</th>
                  <th class="px-3 py-2">{{ t('admin.usageAnalysis.detail.scope') }}</th>
                  <th class="px-3 py-2">{{ t('admin.usageAnalysis.detail.ruleDetail') }}</th>
                  <th class="px-3 py-2 text-right">{{ t('admin.usageAnalysis.detail.points') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                <tr v-for="(hit, idx) in detail.rule_hits" :key="idx">
                  <td class="px-3 py-2 font-mono text-gray-800 dark:text-gray-200">{{ hit.rule }}</td>
                  <td class="px-3 py-2 text-gray-500 dark:text-gray-400">{{ hit.scope }}</td>
                  <td class="px-3 py-2 text-gray-600 dark:text-gray-300">{{ hit.detail }}</td>
                  <td class="px-3 py-2 text-right font-semibold text-gray-900 dark:text-white">{{ hit.points }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="text-sm text-gray-400">{{ t('admin.usageAnalysis.detail.noRules') }}</p>
        </section>

        <!-- Evidence -->
        <section>
          <div class="mb-2 flex items-center gap-2">
            <h4 class="text-xs font-bold uppercase tracking-wider text-gray-400">
              {{ t('admin.usageAnalysis.evidence.title') }}
            </h4>
            <span
              v-if="evidence?.truncated"
              class="inline-flex items-center gap-1 rounded-full bg-amber-100 px-2 py-0.5 text-[11px] font-semibold text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
            >
              {{ t('admin.usageAnalysis.evidence.truncated') }}
            </span>
          </div>

          <div v-if="hasEvidence" class="space-y-4">
            <!-- Active hours -->
            <div v-if="evidence?.active_hours !== undefined" class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
              <div class="text-xs font-bold uppercase tracking-wider text-gray-400">
                {{ t('admin.usageAnalysis.evidence.activeHours') }}
              </div>
              <div class="mt-1 text-lg font-bold text-gray-900 dark:text-white">{{ evidence.active_hours }}</div>
            </div>

            <!-- 24h heatmap: render only the hours actually present -->
            <div v-if="heatmap.length" class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
              <div class="mb-2 text-xs font-bold uppercase tracking-wider text-gray-400">
                {{ t('admin.usageAnalysis.evidence.heatmap') }}
              </div>
              <div class="flex flex-wrap gap-1.5">
                <div
                  v-for="h in heatmap"
                  :key="h.hour"
                  class="flex w-12 flex-col items-center rounded-md bg-white p-1 ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-600"
                  :title="`${h.hour}:00 — ${h.requests}`"
                >
                  <span class="text-[10px] text-gray-400">{{ h.hour }}:00</span>
                  <div
                    class="mt-0.5 w-full rounded bg-primary-500/20"
                    :style="{ height: heatmapBarHeight(h.requests) + 'px' }"
                  ></div>
                  <span class="mt-0.5 text-[11px] font-semibold text-gray-700 dark:text-gray-200">{{ h.requests }}</span>
                </div>
              </div>
            </div>

            <!-- IP top -->
            <div v-if="evidence?.ip_top && evidence.ip_top.length" class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
              <div class="mb-2 text-xs font-bold uppercase tracking-wider text-gray-400">
                {{ t('admin.usageAnalysis.evidence.ipTop') }}
              </div>
              <table class="w-full text-left text-sm">
                <thead class="text-xs uppercase text-gray-400">
                  <tr>
                    <th class="py-1 pr-3">{{ t('admin.usageAnalysis.evidence.ip') }}</th>
                    <th class="py-1 pr-3 text-right">{{ t('admin.usageAnalysis.evidence.ipRequests') }}</th>
                    <th class="py-1 text-right">{{ t('admin.usageAnalysis.evidence.distinctUsers') }}</th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                  <tr v-for="(ip, idx) in evidence.ip_top" :key="idx">
                    <td class="py-1 pr-3 font-mono text-gray-800 dark:text-gray-200">{{ ip.ip }}</td>
                    <td class="py-1 pr-3 text-right text-gray-600 dark:text-gray-300">{{ ip.requests }}</td>
                    <td class="py-1 text-right text-gray-600 dark:text-gray-300">{{ ip.distinct_users }}</td>
                  </tr>
                </tbody>
              </table>
            </div>

            <!-- UA top -->
            <div v-if="evidence?.ua_top && evidence.ua_top.length" class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
              <div class="mb-2 text-xs font-bold uppercase tracking-wider text-gray-400">
                {{ t('admin.usageAnalysis.evidence.uaTop') }}
              </div>
              <table class="w-full text-left text-sm">
                <thead class="text-xs uppercase text-gray-400">
                  <tr>
                    <th class="py-1 pr-3">{{ t('admin.usageAnalysis.evidence.ua') }}</th>
                    <th class="py-1 text-right">{{ t('admin.usageAnalysis.evidence.uaCount') }}</th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                  <tr v-for="(ua, idx) in evidence.ua_top" :key="idx">
                    <td class="py-1 pr-3 break-all font-mono text-xs text-gray-800 dark:text-gray-200">{{ ua.ua }}</td>
                    <td class="py-1 text-right text-gray-600 dark:text-gray-300">{{ ua.count }}</td>
                  </tr>
                </tbody>
              </table>
            </div>

            <!-- Key distribution -->
            <div v-if="evidence?.key_distribution && evidence.key_distribution.length" class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
              <div class="mb-2 text-xs font-bold uppercase tracking-wider text-gray-400">
                {{ t('admin.usageAnalysis.evidence.keyDistribution') }}
              </div>
              <table class="w-full text-left text-sm">
                <thead class="text-xs uppercase text-gray-400">
                  <tr>
                    <th class="py-1 pr-3">{{ t('admin.usageAnalysis.evidence.keyName') }}</th>
                    <th class="py-1 text-right">{{ t('admin.usageAnalysis.evidence.keyRequests') }}</th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                  <tr v-for="(k, idx) in evidence.key_distribution" :key="idx">
                    <td class="py-1 pr-3 text-gray-800 dark:text-gray-200">{{ k.key }}</td>
                    <td class="py-1 text-right text-gray-600 dark:text-gray-300">{{ k.count }}</td>
                  </tr>
                </tbody>
              </table>
            </div>

            <!-- R2 context -->
            <div v-if="evidence?.r2_context" class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
              <div class="mb-2 text-xs font-bold uppercase tracking-wider text-gray-400">
                {{ t('admin.usageAnalysis.evidence.r2Context') }}
              </div>
              <div class="grid grid-cols-2 gap-3 sm:grid-cols-3">
                <div v-if="evidence.r2_context.peer_count !== undefined">
                  <div class="text-xs uppercase text-gray-400">{{ t('admin.usageAnalysis.evidence.peerCount') }}</div>
                  <div class="text-sm font-medium text-gray-900 dark:text-white">{{ evidence.r2_context.peer_count }}</div>
                </div>
                <div v-if="evidence.r2_context.filter">
                  <div class="text-xs uppercase text-gray-400">{{ t('admin.usageAnalysis.evidence.filter') }}</div>
                  <div class="text-sm font-medium text-gray-900 dark:text-white">{{ evidence.r2_context.filter }}</div>
                </div>
                <div v-if="evidence.r2_context.algorithm">
                  <div class="text-xs uppercase text-gray-400">{{ t('admin.usageAnalysis.evidence.algorithm') }}</div>
                  <div class="text-sm font-medium text-gray-900 dark:text-white">{{ evidence.r2_context.algorithm }}</div>
                </div>
                <div v-if="evidence.r2_context.p95 !== undefined">
                  <div class="text-xs uppercase text-gray-400">{{ t('admin.usageAnalysis.evidence.p95') }}</div>
                  <div class="text-sm font-medium text-gray-900 dark:text-white">{{ evidence.r2_context.p95 }}</div>
                </div>
              </div>
            </div>
          </div>
          <p v-else class="text-sm text-gray-400">{{ t('admin.usageAnalysis.evidence.noData') }}</p>
        </section>
      </div>

      <template #footer>
        <button type="button" class="btn btn-secondary" @click="gotoUserManagement">
          <Icon name="externalLink" size="xs" />
          {{ t('admin.usageAnalysis.actions.gotoUserManagement') }}
        </button>
        <button type="button" class="btn btn-primary" @click="detailVisible = false">
          {{ t('common.close') }}
        </button>
      </template>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { adminAPI } from '@/api/admin'
import type {
  Report,
  ReportDetail,
  ReportStatus,
  RiskLevel,
  RunStatusResponse,
  UpdateReportStatusPayload,
  ListReportsParams
} from '@/api/admin'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import type { Column } from '@/components/common/types'
import Pagination from '@/components/common/Pagination.vue'
import Select from '@/components/common/Select.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore } from '@/stores'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const appStore = useAppStore()
const router = useRouter()

// ==================== State ====================
const loading = ref(false)
const reports = ref<Report[]>([])
const total = ref(0)
const minScore = ref<number | null>(null)
const page = ref(1)
const pageSize = ref(20)

const filters = reactive({
  date: '',
  level: '' as '' | RiskLevel,
  user_id: '',
  group_id: '',
  rule: '',
  include_low: false
})

const runStatus = ref<RunStatusResponse | null>(null)
const runStatusLoading = ref(false)

// ==================== Labels / badges (static keys only) ====================
const levelLabels: Record<string, string> = {
  low: t('admin.usageAnalysis.level.low'),
  medium: t('admin.usageAnalysis.level.medium'),
  high: t('admin.usageAnalysis.level.high'),
  critical: t('admin.usageAnalysis.level.critical')
}

const statusLabels: Record<string, string> = {
  open: t('admin.usageAnalysis.reportStatus.open'),
  acknowledged: t('admin.usageAnalysis.reportStatus.acknowledged'),
  dismissed: t('admin.usageAnalysis.reportStatus.dismissed'),
  resolved: t('admin.usageAnalysis.reportStatus.resolved')
}

const runStatusLabels: Record<RunStatusResponse['status'], string> = {
  idle: t('admin.usageAnalysis.status.idle'),
  running: t('admin.usageAnalysis.status.running'),
  completed: t('admin.usageAnalysis.status.completed'),
  partial: t('admin.usageAnalysis.status.partial'),
  error: t('admin.usageAnalysis.status.error')
}

const levelBadgeClass = (level: RiskLevel): string => {
  const base = 'inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-semibold '
  switch (level) {
    case 'critical':
      return base + 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
    case 'high':
      return base + 'bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-300'
    case 'medium':
      return base + 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
    default:
      return base + 'bg-gray-100 text-gray-700 dark:bg-gray-700/40 dark:text-gray-300'
  }
}

const levelDotClass = (level: RiskLevel): string => {
  switch (level) {
    case 'critical':
      return 'bg-red-500'
    case 'high':
      return 'bg-orange-500'
    case 'medium':
      return 'bg-amber-500'
    default:
      return 'bg-gray-400'
  }
}

const statusBadgeClass = (status: ReportStatus): string => {
  const base = 'inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-semibold '
  switch (status) {
    case 'open':
      return base + 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
    case 'acknowledged':
      return base + 'bg-indigo-100 text-indigo-700 dark:bg-indigo-900/30 dark:text-indigo-300'
    case 'resolved':
      return base + 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
    default:
      return base + 'bg-gray-100 text-gray-600 dark:bg-gray-700/40 dark:text-gray-300'
  }
}

const runStatusBadgeClass = (status: RunStatusResponse['status']): string => {
  const base = 'inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-semibold '
  switch (status) {
    case 'idle':
      return base + 'bg-gray-100 text-gray-700 dark:bg-gray-700/40 dark:text-gray-300'
    case 'running':
      return base + 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
    case 'completed':
      return base + 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
    case 'partial':
      return base + 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
    default:
      return base + 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
  }
}

const runStatusDotClass = (status: RunStatusResponse['status']): string => {
  switch (status) {
    case 'idle':
      return 'bg-gray-400'
    case 'running':
      return 'bg-blue-500'
    case 'completed':
      return 'bg-green-500'
    case 'partial':
      return 'bg-amber-500'
    default:
      return 'bg-red-500'
  }
}

// ==================== Columns ====================
const levelOptions = computed(() => [
  { value: '', label: t('admin.usageAnalysis.filters.allLevels') },
  { value: 'low', label: t('admin.usageAnalysis.level.low') },
  { value: 'medium', label: t('admin.usageAnalysis.level.medium') },
  { value: 'high', label: t('admin.usageAnalysis.level.high') },
  { value: 'critical', label: t('admin.usageAnalysis.level.critical') }
])

const columns = computed<Column[]>(() => [
  { key: 'username', label: t('admin.usageAnalysis.columns.user') },
  { key: 'group_name', label: t('admin.usageAnalysis.columns.group') },
  { key: 'report_date', label: t('admin.usageAnalysis.columns.date') },
  { key: 'score', label: t('admin.usageAnalysis.columns.score') },
  { key: 'level', label: t('admin.usageAnalysis.columns.level') },
  { key: 'rulesSummary', label: t('admin.usageAnalysis.columns.rules') },
  { key: 'status', label: t('admin.usageAnalysis.columns.status') },
  { key: 'actions', label: t('admin.usageAnalysis.columns.actions') }
])

// ==================== Derived ====================
const displayReports = computed<Report[]>(() => reports.value)

const evidence = computed(() => detail.value?.evidence)

const hasEvidence = computed(() => {
  const e = evidence.value
  if (!e) return false
  return Boolean(
    e.active_hours !== undefined ||
      (e.heatmap && Object.keys(e.heatmap).length > 0) ||
      (e.ip_top && e.ip_top.length) ||
      (e.ua_top && e.ua_top.length) ||
      (e.key_distribution && e.key_distribution.length) ||
      e.r2_context
  )
})

const heatmap = computed(() => {
  const hm = evidence.value?.heatmap
  if (!hm) return []
  // Backend returns a { "<hour>": number } map. Sort numerically by hour key
  // (NOT lexicographically) before rendering.
  return Object.keys(hm)
    .map((k) => ({ hour: Number(k), requests: hm[k] }))
    .sort((a, b) => a.hour - b.hour)
})

const heatmapMax = computed(() => {
  const max = heatmap.value.reduce((m, h) => Math.max(m, h.requests), 0)
  return max > 0 ? max : 1
})

function heatmapBarHeight(requests: number): number {
  // 4px..40px proportional bar
  return Math.max(4, Math.round((requests / heatmapMax.value) * 40))
}

// ==================== Helpers ====================
function ruleSummary(row: Report): string {
  const hits = row.rule_hits || []
  if (!hits.length) return '—'
  const names = hits.slice(0, 3).map((h) => h.rule)
  const extra = hits.length > 3 ? ` +${hits.length - 3}` : ''
  return names.join(', ') + extra
}

function formatDate(iso: string): string {
  if (!iso) return '—'
  // Pure date string (YYYY-MM-DD): return as-is. Parsing with `new Date()`
  // treats it as UTC midnight, which toLocaleDateString in negative-offset
  // timezones would shift to the previous day.
  if (/^\d{4}-\d{2}-\d{2}$/.test(iso)) return iso
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleDateString()
}

function formatTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString()
}

function availableActions(status: ReportStatus): UpdateReportStatusPayload[] {
  if (status === 'open') return ['acknowledged', 'dismissed']
  if (status === 'acknowledged') return ['resolved', 'dismissed']
  return []
}

// ==================== Data fetching ====================
function buildQuery(): ListReportsParams {
  const q: ListReportsParams = {
    page: page.value,
    page_size: pageSize.value,
    include_low: filters.include_low || undefined
  }
  if (filters.date) q.date = filters.date
  if (filters.level) q.level = filters.level
  if (filters.user_id) q.user_id = Number(filters.user_id)
  if (filters.group_id) q.group_id = Number(filters.group_id)
  if (filters.rule) q.rule = filters.rule
  return q
}

async function fetchReports() {
  loading.value = true
  try {
    const res = await adminAPI.usageRisk.listReports(buildQuery())
    reports.value = res.items || []
    total.value = res.total || 0
    minScore.value = typeof res.min_score === 'number' ? res.min_score : null
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.usageAnalysis.loadFailed')))
  } finally {
    loading.value = false
  }
}

async function fetchRunStatus() {
  runStatusLoading.value = true
  try {
    runStatus.value = await adminAPI.usageRisk.getRunStatus()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.usageAnalysis.runStatusLoadFailed')))
  } finally {
    runStatusLoading.value = false
  }
}

function search() {
  page.value = 1
  fetchReports()
}

function resetFilters() {
  filters.date = ''
  filters.level = ''
  filters.user_id = ''
  filters.group_id = ''
  filters.rule = ''
  filters.include_low = false
  search()
}

function onPageChange(p: number) {
  page.value = p
  fetchReports()
}

function onPageSizeChange(ps: number) {
  pageSize.value = ps
  page.value = 1
  fetchReports()
}

// ==================== Detail / status ====================
const detailVisible = ref(false)
const detailLoading = ref(false)
const detail = ref<ReportDetail | null>(null)
const updatingId = ref<number | null>(null)

async function openDetail(id: number) {
  detailVisible.value = true
  detailLoading.value = true
  detail.value = null
  try {
    detail.value = await adminAPI.usageRisk.getReport(id)
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.usageAnalysis.loadFailed')))
    detailVisible.value = false
  } finally {
    detailLoading.value = false
  }
}

async function changeStatus(id: number, status: UpdateReportStatusPayload) {
  updatingId.value = id
  try {
    const updated = await adminAPI.usageRisk.updateReportStatus(id, status)
    const idx = reports.value.findIndex((r) => r.report_id === id)
    if (idx >= 0) reports.value[idx] = { ...reports.value[idx], ...updated }
    if (detail.value && detail.value.report_id === id) {
      detail.value = { ...detail.value, ...updated }
    }
    appStore.showSuccess(statusLabels[status])
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.usageAnalysis.statusUpdateFailed')))
  } finally {
    updatingId.value = null
  }
}

function gotoUserManagement() {
  void router.push('/admin/users')
}

onMounted(() => {
  fetchRunStatus()
  fetchReports()
})
</script>
