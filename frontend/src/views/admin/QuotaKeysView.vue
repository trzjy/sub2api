<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div class="min-w-0">
            <h1 class="page-title flex items-center gap-2 text-xl font-black text-gray-900 dark:text-white">
              <span class="inline-flex h-8 w-8 items-center justify-center rounded-xl bg-blue-50 text-blue-500 dark:bg-blue-900/30 dark:text-blue-400">
                <Icon name="key" size="sm" />
              </span>
              {{ t('admin.quotaKeys.title') }}
            </h1>
            <p class="page-description mt-1.5 text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.description') }}
            </p>
          </div>
          <div class="flex flex-1 flex-wrap items-center justify-end gap-2">
            <button
              class="btn btn-secondary"
              :disabled="loading"
              :title="t('common.refresh')"
              @click="load"
            >
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button class="btn btn-secondary" :disabled="sweeping" @click="openSweepDialog">
              {{ t('admin.quotaKeys.sweepExpired') }}
            </button>
            <button class="btn btn-secondary" @click="showConfigModal = true">
              {{ t('admin.quotaKeys.autoEntry.title') }}
            </button>
            <button class="btn btn-primary" @click="openCreateDialog">
              <Icon name="plus" size="md" class="mr-1" />
              {{ t('admin.quotaKeys.create') }}
            </button>
          </div>
        </div>
      </template>

      <template #table>
        <DataTable :columns="columns" :data="keys" :loading="loading">
          <template #cell-code_masked="{ value }">
            <code class="font-mono text-sm text-gray-900 dark:text-gray-100">{{ value || t('admin.quotaKeys.na') }}</code>
          </template>

          <!-- §25.6⑦ / §25.7-B：来源额度快照（供应商总额/已用/算术剩余 + 观测时间）。
               无快照 → 不渲染任何数值；最近一次观测失败 → 保留上次成功值并标注查询失败。 -->
          <template #cell-usage="{ row }">
            <QuotaKeyUsageCell v-if="row.usage" :usage="row.usage" />
            <span v-else class="text-sm text-gray-400 dark:text-gray-500">{{ t('admin.quotaKeys.na') }}</span>
          </template>

          <template #cell-status="{ row }">
            <span
              class="inline-flex items-center rounded-md px-2 py-0.5 text-xs font-medium"
              :class="statusBadgeClass(row)"
            >
              {{ statusLabel(row) }}
            </span>
          </template>

          <template #cell-exhausted="{ row }">
            <span :class="row.exhausted ? 'text-red-600 dark:text-red-400' : 'text-gray-500 dark:text-gray-400'">
              {{ row.exhausted ? t('admin.quotaKeys.yes') : t('admin.quotaKeys.no') }}
            </span>
          </template>

          <template #cell-account_count="{ row }">
            <span class="text-sm text-gray-900 dark:text-gray-100">{{ row.account_count ?? 0 }}</span>
          </template>

          <template #cell-working_count="{ row }">
            <span class="text-sm text-gray-900 dark:text-gray-100">{{ row.working_count ?? 0 }}</span>
          </template>

          <template #cell-expires_at="{ row }">
            <span class="text-sm text-gray-500 dark:text-dark-400">{{ formatExpiresAt(row) }}</span>
          </template>

          <template #cell-last_refresh_error="{ row }">
            <span
              v-if="row.last_refresh_error"
              class="text-xs text-red-600 dark:text-red-400"
              :title="row.last_refresh_error"
            >
              {{ row.last_refresh_error }}
            </span>
            <span v-else class="text-sm text-gray-400 dark:text-gray-500">{{ t('admin.quotaKeys.na') }}</span>
          </template>

          <template #cell-actions="{ row }">
            <div class="flex items-center justify-end gap-1">
              <button
                class="rounded p-1 text-gray-400 transition-colors hover:text-gray-600 dark:hover:text-gray-200"
                :title="t('admin.quotaKeys.actions.detail')"
                @click="openDetail(row)"
              >
                <Icon name="eye" size="sm" />
              </button>
              <button
                class="rounded p-1 text-gray-400 transition-colors hover:text-gray-600 dark:hover:text-gray-200"
                :title="t('admin.quotaKeys.actions.edit')"
                @click="openEditDialog(row)"
              >
                <Icon name="edit" size="sm" />
              </button>
              <button
                class="rounded p-1 text-gray-400 transition-colors hover:text-gray-600 disabled:opacity-40 dark:hover:text-gray-200"
                :disabled="busyId === row.id"
                :title="t('admin.quotaKeys.actions.refresh')"
                @click="handleRefresh(row)"
              >
                <Icon name="sync" size="sm" />
              </button>
              <button
                v-if="row.exhausted"
                class="rounded p-1 text-gray-400 transition-colors hover:text-gray-600 disabled:opacity-40 dark:hover:text-gray-200"
                :disabled="busyId === row.id"
                :title="t('admin.quotaKeys.actions.clearExhausted')"
                @click="handleClearExhausted(row)"
              >
                <Icon name="checkCircle" size="sm" />
              </button>
              <button
                class="rounded p-1 text-gray-400 transition-colors hover:text-blue-600 disabled:opacity-40 dark:hover:text-blue-400"
                :disabled="busyId === row.id"
                :title="t('admin.quotaKeys.probe.button')"
                @click="handleProbe(row)"
              >
                <Icon name="chart" size="sm" />
              </button>
              <button
                class="rounded p-1 text-gray-400 transition-colors hover:text-red-600 dark:hover:text-red-400"
                :title="t('admin.quotaKeys.actions.delete')"
                @click="openDeleteDialog(row)"
              >
                <Icon name="trash" size="sm" />
              </button>
            </div>
          </template>

          <template #empty>
            <EmptyState
              :title="t('admin.quotaKeys.emptyTitle')"
              :description="t('admin.quotaKeys.emptyDescription')"
              :action-text="t('admin.quotaKeys.create')"
              @action="openCreateDialog"
            />
          </template>
        </DataTable>
      </template>
    </TablePageLayout>

    <!-- 创建 / 编辑 -->
    <BaseDialog
      :show="showFormDialog"
      :title="editing ? t('admin.quotaKeys.editTitle') : t('admin.quotaKeys.createTitle')"
      width="normal"
      @close="closeFormDialog"
    >
      <div class="space-y-4">
        <!-- F1：明文激活码只在创建时输入一次，不进入持久化 state（submit 后立即清空） -->
        <div v-if="!editing">
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.code') }}
          </label>
          <input
            v-model="form.code"
            type="text"
            autocomplete="off"
            class="input font-mono"
            :placeholder="t('admin.quotaKeys.codePlaceholder')"
            @input="formError = ''"
          />
          <p class="input-hint mt-1">{{ t('admin.quotaKeys.codeHint') }}</p>
        </div>

        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.expiresAt') }}
          </label>
          <input v-model="form.expires_at" type="datetime-local" class="input" />
          <p class="input-hint mt-1">{{ t('admin.quotaKeys.expiresAtPlaceholder') }}</p>
        </div>

        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.notes') }}
          </label>
          <TextArea
            v-model="form.notes"
            :rows="3"
            :placeholder="t('admin.quotaKeys.notesPlaceholder')"
          />
        </div>

        <p v-if="formError" class="text-sm text-red-600 dark:text-red-400">{{ formError }}</p>
      </div>

      <template #footer>
        <div class="flex items-center justify-end gap-2">
          <button class="btn btn-secondary" @click="closeFormDialog">{{ t('common.cancel') }}</button>
          <button class="btn btn-primary" :disabled="saving" @click="submitForm">
            {{ t('common.save') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <!-- 详情：账号列表默认折叠（§0.1 #7），折叠状态按激活码 id 记忆，不随刷新/翻页重置 -->
    <BaseDialog
      :show="showDetailDialog"
      :title="t('admin.quotaKeys.detailTitle')"
      width="wide"
      @close="showDetailDialog = false"
    >
      <div v-if="detail" class="space-y-4">
        <dl class="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2">
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.maskedCode') }}
            </dt>
            <dd class="mt-0.5 font-mono text-sm text-gray-900 dark:text-gray-100">{{ detail.code_masked }}</dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.status') }}
            </dt>
            <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ statusLabel(detail) }}</dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.exhausted') }}
            </dt>
            <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">
              {{ detail.exhausted ? t('admin.quotaKeys.yes') : t('admin.quotaKeys.no') }}
            </dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.accountCount') }}
            </dt>
            <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ detail.account_count ?? 0 }}</dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.workingCount') }}
            </dt>
            <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ detail.working_count ?? 0 }}</dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.expiresAt') }}
            </dt>
            <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ formatExpiresAt(detail) }}</dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.lastRefreshAt') }}
            </dt>
            <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">
              {{ detail.last_refresh_at ? formatDateTime(detail.last_refresh_at) : t('admin.quotaKeys.na') }}
            </dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.lastRefreshError') }}
            </dt>
            <dd class="mt-0.5 break-all text-sm text-red-600 dark:text-red-400">
              {{ detail.last_refresh_error || t('admin.quotaKeys.na') }}
            </dd>
          </div>
          <div class="sm:col-span-2">
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.notes') }}
            </dt>
            <dd class="mt-0.5 whitespace-pre-wrap text-sm text-gray-900 dark:text-gray-100">
              {{ detail.notes || t('admin.quotaKeys.na') }}
            </dd>
          </div>
        </dl>

        <!-- §25.6⑤⑦：详情展示与列表/probe/账号页同一份已提交快照投影（只读，不发网络）。
             无快照时只显示占位符，不补 0、不伪造「无额度」。 -->
        <div class="rounded-lg border border-gray-200 p-2 dark:border-dark-600">
          <QuotaKeyUsageCell v-if="detail.usage" :usage="detail.usage" />
          <span v-else class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.quotaKeys.na') }}</span>
        </div>

        <div class="border-t border-gray-200 pt-3 dark:border-dark-600">
          <button
            class="flex w-full items-center justify-between gap-2 text-left"
            :aria-expanded="isAccountsExpanded(detail.id)"
            @click="toggleAccounts(detail.id)"
          >
            <span class="text-sm font-medium text-gray-900 dark:text-white">
              {{ t('admin.quotaKeys.accountsTitle') }}（{{ detail.accounts?.length ?? 0 }}）
            </span>
            <span class="flex items-center gap-1 text-xs text-gray-500 dark:text-gray-400">
              {{
                isAccountsExpanded(detail.id)
                  ? t('admin.quotaKeys.collapseAccounts')
                  : t('admin.quotaKeys.expandAccounts')
              }}
              <Icon
                :name="isAccountsExpanded(detail.id) ? 'chevronDown' : 'chevronRight'"
                size="sm"
              />
            </span>
          </button>

          <div v-if="isAccountsExpanded(detail.id)" class="mt-3">
            <!-- P1-B3 两层额度语义：账号层只展示归属与状态，绝不复制 key 级余额数值。 -->
            <p class="mb-2 text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.accountsQuotaHint') }}
            </p>
            <p v-if="!detail.accounts || detail.accounts.length === 0" class="text-sm text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.noAccounts') }}
            </p>
            <ul v-else class="divide-y divide-gray-100 dark:divide-dark-700">
              <li
                v-for="account in detail.accounts"
                :key="account.id"
                class="flex items-center justify-between gap-3 py-2"
              >
                <div class="min-w-0">
                  <p class="truncate text-sm font-medium text-gray-900 dark:text-white">{{ account.name }}</p>
                  <p class="truncate text-xs text-gray-500 dark:text-gray-400">
                    #{{ account.id }} · {{ account.platform }}
                  </p>
                </div>
                <span class="shrink-0 text-xs text-gray-500 dark:text-gray-400">{{ account.status }}</span>
              </li>
            </ul>
          </div>
        </div>
      </div>

      <template #footer>
        <div class="flex items-center justify-end">
          <button class="btn btn-secondary" @click="showDetailDialog = false">{{ t('common.close') }}</button>
        </div>
      </template>
    </BaseDialog>

    <ConfirmDialog
      :show="showDeleteDialog"
      :title="t('admin.quotaKeys.confirmDeleteTitle')"
      :message="t('admin.quotaKeys.deleteConfirm')"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="confirmDelete"
      @cancel="showDeleteDialog = false"
    />

    <ConfirmDialog
      :show="showSweepDialog"
      :title="t('admin.quotaKeys.confirmSweepTitle')"
      :message="t('admin.quotaKeys.sweepConfirm')"
      :confirm-text="t('common.confirm')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="confirmSweep"
      @cancel="showSweepDialog = false"
    />

    <!-- 入库可配置（D2-a） -->
    <AutoEntryConfigModal :show="showConfigModal" @close="showConfigModal = false" />

    <!-- 用量查询（D2-a probe_quota）结果 -->
    <BaseDialog
      :show="showProbeDialog"
      :title="t('admin.quotaKeys.probe.title')"
      width="normal"
      @close="showProbeDialog = false"
    >
      <div v-if="probing" class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">
        {{ t('admin.quotaKeys.probe.probing') }}
      </div>
      <dl v-else-if="probeResult" class="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2">
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.group') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.group || t('admin.quotaKeys.na') }}</dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.status') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.status_code }}</dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.used') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.used }}</dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.max') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.max }}</dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.deviceCount') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.device_count }}</dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.deviceMaxCount') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.device_max_count }}</dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.expiredAt') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">
            {{ probeResult.expired_at ? formatDateTime(probeResult.expired_at) : t('admin.quotaKeys.expiryUnknown') }}
          </dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.accountsWritten') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.accounts_written }}</dd>
        </div>
      </dl>
      <!-- P1-B3 两层额度语义：本弹窗整体是 key 级额度快照（总额度/期限/设备/取号结果），
           与账号层余额分属两层，不在账号层复制展示。 -->
      <p v-if="probeResult" class="mt-3 text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.quotaKeys.probe.keyLevelHint') }}
      </p>
      <!-- §25.6⑤：probe 一并回传**已提交**快照投影（与列表/详情/账号页同源同值），
           不是把本次 lookup 结果当已落库事实；无快照/失败按同一渲染纪律处理。 -->
      <div v-if="probeResult?.usage" class="mt-2 border-t border-gray-200 pt-2 dark:border-dark-600">
        <QuotaKeyUsageCell :usage="probeResult.usage" />
      </div>
      <p v-if="!probeResult && probeError" class="text-sm text-red-600 dark:text-red-400">{{ probeError }}</p>

      <template #footer>
        <div class="flex items-center justify-end">
          <button class="btn btn-secondary" @click="showProbeDialog = false">{{ t('common.close') }}</button>
        </div>
      </template>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatDateTime, resolveExpiryDisplay } from '@/utils/format'
import { adminAPI } from '@/api/admin'
import type {
  QuotaKey,
  QuotaKeyDetail,
  CreateQuotaKeyRequest,
  UpdateQuotaKeyRequest,
  ProbeQuotaResponse
} from '@/api/admin/quotaKeys'
import type { Column } from '@/components/common/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import TextArea from '@/components/common/TextArea.vue'
import Icon from '@/components/icons/Icon.vue'
import AutoEntryConfigModal from '@/components/quotaKeys/AutoEntryConfigModal.vue'
import QuotaKeyUsageCell from '@/components/quotaKeys/QuotaKeyUsageCell.vue'

const { t } = useI18n()
const appStore = useAppStore()

const keys = ref<QuotaKey[]>([])
const loading = ref(false)
const busyId = ref<number | null>(null)
const saving = ref(false)
const sweeping = ref(false)

const showFormDialog = ref(false)
const showDetailDialog = ref(false)
const showDeleteDialog = ref(false)
const showSweepDialog = ref(false)
// D2-a：入库可配置弹窗 + 用量查询弹窗。
const showConfigModal = ref(false)
const showProbeDialog = ref(false)
const probing = ref(false)
const probeResult = ref<ProbeQuotaResponse | null>(null)
const probeError = ref('')
const editing = ref<QuotaKey | null>(null)
const deleting = ref<QuotaKey | null>(null)
const detail = ref<QuotaKeyDetail | null>(null)
const formError = ref('')

const form = reactive({
  code: '',
  expires_at: '',
  notes: ''
})

/**
 * §0.1 #7 折叠裁定：账号列表默认折叠。
 * 展开状态按激活码 id 记忆在组件作用域内，不随列表刷新/重挂载重置；
 * 未显式展开过的 id 即视为折叠，因此默认值天然是「折叠」。
 */
const expandedAccounts = reactive(new Set<number>())

const columns = computed<Column[]>(() => [
  { key: 'code_masked', label: t('admin.quotaKeys.columns.maskedCode'), sortable: false },
  // 来源额度快照列（§25.6⑦）：列名沿用「激活码用量」口径，数值为一层（key/来源）快照。
  { key: 'usage', label: t('admin.quotaKeys.probe.title'), sortable: false },
  { key: 'status', label: t('admin.quotaKeys.columns.status'), sortable: false },
  { key: 'exhausted', label: t('admin.quotaKeys.columns.exhausted'), sortable: false },
  { key: 'account_count', label: t('admin.quotaKeys.columns.accountCount'), sortable: false },
  { key: 'working_count', label: t('admin.quotaKeys.columns.workingCount'), sortable: false },
  { key: 'expires_at', label: t('admin.quotaKeys.columns.expiresAt'), sortable: false },
  { key: 'last_refresh_error', label: t('admin.quotaKeys.columns.lastRefreshError'), sortable: false },
  { key: 'actions', label: t('admin.quotaKeys.columns.actions'), sortable: false }
])

function statusLabel(row: QuotaKey | QuotaKeyDetail): string {
  if (row.exhausted) return t('admin.quotaKeys.status.exhausted')
  switch (row.status) {
    case 'active':
      return t('admin.quotaKeys.status.active')
    case 'expired':
      return t('admin.quotaKeys.status.expired')
    case 'inactive':
      return t('admin.quotaKeys.status.inactive')
    default:
      return row.status || t('admin.quotaKeys.status.unknown')
  }
}

function statusBadgeClass(row: QuotaKey | QuotaKeyDetail): string {
  if (row.exhausted) return 'bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300'
  switch (row.status) {
    case 'active':
      return 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-300'
    case 'expired':
      return 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300'
    case 'inactive':
      return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
    default:
      return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
  }
}

// 到期列渲染：按整行 status + 到期值收敛语义（C-U7-B 展示收敛）。
// - status=expired 且无时间 → "已过期"
// - active + 有时间 → 具体日期
// - active + 无时间（含空串） → "到期时间未知"
//   注：激活码不存在永不过期形态，上游给的码必然带期限；后端对 NULL 也只是
//   "无期限约束"，不是业务承诺。原 never 分支（空串 ⇒ 永不过期）已删除。
function formatExpiresAt(row: QuotaKey | QuotaKeyDetail): string {
  if (row.status === 'expired' && (row.expires_at === null || row.expires_at === undefined)) {
    return t('admin.quotaKeys.status.expired')
  }
  const r = resolveExpiryDisplay(row.expires_at)
  if (r.status === 'unknown') return t('admin.quotaKeys.expiryUnknown')
  return r.text
}

function isAccountsExpanded(id: number): boolean {
  return expandedAccounts.has(id)
}

function toggleAccounts(id: number): void {
  if (expandedAccounts.has(id)) {
    expandedAccounts.delete(id)
  } else {
    expandedAccounts.add(id)
  }
}

async function load() {
  loading.value = true
  try {
    const data = await adminAPI.quotaKeys.list()
    keys.value = data.items
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.loadError')))
  } finally {
    loading.value = false
  }
}

function openCreateDialog() {
  editing.value = null
  form.code = ''
  form.expires_at = ''
  form.notes = ''
  formError.value = ''
  showFormDialog.value = true
}

function openEditDialog(row: QuotaKey) {
  editing.value = row
  form.code = ''
  form.expires_at = row.expires_at ? toLocalInputValue(row.expires_at) : ''
  form.notes = row.notes ?? ''
  formError.value = ''
  showFormDialog.value = true
}

function closeFormDialog() {
  showFormDialog.value = false
  editing.value = null
  // F1：明文激活码用完即弃，绝不驻留前端状态
  form.code = ''
}

function toLocalInputValue(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

function buildPayload(): CreateQuotaKeyRequest | UpdateQuotaKeyRequest {
  const expiresAt = form.expires_at ? new Date(form.expires_at).toISOString() : null
  return {
    expires_at: expiresAt,
    notes: form.notes.trim() ? form.notes.trim() : null
  }
}

async function submitForm() {
  if (!editing.value && !form.code.trim()) {
    formError.value = t('admin.quotaKeys.codePlaceholder')
    return
  }
  saving.value = true
  try {
    if (editing.value) {
      await adminAPI.quotaKeys.update(editing.value.id, buildPayload() as UpdateQuotaKeyRequest)
      appStore.showSuccess(t('admin.quotaKeys.updated'))
    } else {
      const payload = { ...(buildPayload() as CreateQuotaKeyRequest), code: form.code.trim() }
      await adminAPI.quotaKeys.create(payload)
      appStore.showSuccess(t('admin.quotaKeys.created'))
    }
    closeFormDialog()
    await load()
  } catch (err: unknown) {
    const message = extractApiErrorMessage(
      err,
      editing.value ? t('admin.quotaKeys.updateError') : t('admin.quotaKeys.createError')
    )
    formError.value = message
    appStore.showError(message)
  } finally {
    saving.value = false
  }
}

async function openDetail(row: QuotaKey) {
  showDetailDialog.value = true
  detail.value = null
  try {
    detail.value = await adminAPI.quotaKeys.get(row.id)
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.detailLoadError')))
    showDetailDialog.value = false
  }
}

async function handleRefresh(row: QuotaKey) {
  if (busyId.value !== null) return
  busyId.value = row.id
  try {
    await adminAPI.quotaKeys.refresh(row.id)
    appStore.showSuccess(t('admin.quotaKeys.refreshed'))
    await load()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.refreshError')))
  } finally {
    busyId.value = null
  }
}

async function handleClearExhausted(row: QuotaKey) {
  if (busyId.value !== null) return
  busyId.value = row.id
  try {
    await adminAPI.quotaKeys.clearExhausted(row.id)
    appStore.showSuccess(t('admin.quotaKeys.cleared'))
    await load()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.clearError')))
  } finally {
    busyId.value = null
  }
}

// 用量查询（D2-a probe_quota）：调后端 lookup 并弹窗展示 key_info 摘要。
async function handleProbe(row: QuotaKey) {
  if (busyId.value !== null) return
  busyId.value = row.id
  showProbeDialog.value = true
  probing.value = true
  probeResult.value = null
  probeError.value = ''
  try {
    const info = await adminAPI.quotaKeys.probeQuota(row.id)
    probeResult.value = info
    // O03：probe 已把 expires_at 写库，刷新列表与已开详情以反映最新事实源。
    await load()
    if (detail.value && detail.value.id === row.id) {
      detail.value = await adminAPI.quotaKeys.get(row.id)
    }
  } catch (err: unknown) {
    probeError.value = extractApiErrorMessage(err, t('admin.quotaKeys.probe.error'))
  } finally {
    probing.value = false
    busyId.value = null
  }
}

function openDeleteDialog(row: QuotaKey) {
  deleting.value = row
  showDeleteDialog.value = true
}

async function confirmDelete() {
  if (!deleting.value) return
  const target = deleting.value
  try {
    await adminAPI.quotaKeys.remove(target.id)
    appStore.showSuccess(t('admin.quotaKeys.deleted'))
    showDeleteDialog.value = false
    deleting.value = null
    await load()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.deleteError')))
  }
}

function openSweepDialog() {
  showSweepDialog.value = true
}

async function confirmSweep() {
  sweeping.value = true
  try {
    const res = await adminAPI.quotaKeys.sweepExpired()
    appStore.showSuccess(t('admin.quotaKeys.swept', { count: res.swept ?? 0 }))
    showSweepDialog.value = false
    await load()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.sweepError')))
  } finally {
    sweeping.value = false
  }
}

onMounted(() => {
  void load()
})
</script>
