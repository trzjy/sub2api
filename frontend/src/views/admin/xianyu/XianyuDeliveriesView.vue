<template>
  <AppLayout>
    <div class="p-6">
      <div class="mb-6">
        <h1 class="text-2xl font-bold">{{ t('admin.xianyu.deliveries.title') }}</h1>
        <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.xianyu.deliveries.description') }}</p>
        <div class="mt-3 flex gap-2">
          <button
            :class="activeTab === 'inventory' ? 'btn btn-primary btn-sm' : 'btn btn-secondary btn-sm'"
            @click="switchTab('inventory')"
          >
            {{ t('admin.xianyu.deliveries.inventoryTab') }}
          </button>
          <button
            :class="activeTab === 'worker' ? 'btn btn-primary btn-sm' : 'btn btn-secondary btn-sm'"
            @click="switchTab('worker')"
          >
            {{ t('admin.xianyu.deliveries.workerTab') }}
          </button>
        </div>
      </div>

      <div class="mb-4 flex flex-wrap items-center gap-3">
        <input
          v-model="search"
          class="input max-w-xs"
          :placeholder="t('admin.xianyu.deliveries.searchPlaceholder')"
          @keyup.enter="load"
        />
        <Select v-model="statusFilter" :options="statusOptions" class="w-40" @change="load" />
        <button class="btn btn-secondary" @click="load">
          <Icon name="refresh" size="sm" />
        </button>
      </div>

      <!-- 主程序库存发货记录 -->
      <div v-if="activeTab === 'inventory' && claims.length" class="overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-700">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-gray-200 bg-gray-50 text-left dark:border-dark-700 dark:bg-dark-800">
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.orderNo') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.accountId') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.itemId') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.code') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.status') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.attempts') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.lastAttemptAt') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.error') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.createdAt') }}</th>
              <th class="px-4 py-2 text-right">{{ t('common.actions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="claim in claims" :key="claim.order_no" class="border-b border-gray-100 dark:border-dark-700">
              <td class="px-4 py-2 font-mono text-xs">{{ claim.order_no }}</td>
              <td class="px-4 py-2">{{ claim.account_id }}</td>
              <td class="px-4 py-2">{{ claim.item_id }}</td>
              <td class="px-4 py-2 font-mono text-xs">
                <span v-if="revealedCodes.has(claim.order_no)">{{ claim.code }}</span>
                <span v-else>{{ maskCode(claim.code) }}</span>
                <button class="ml-1 text-xs text-gray-400 hover:text-gray-600" @click="toggleReveal(claim.order_no)">
                  {{ revealedCodes.has(claim.order_no) ? t('admin.xianyu.deliveries.hideCode') : t('admin.xianyu.deliveries.showCode') }}
                </button>
              </td>
              <td class="px-4 py-2">
                <StatusBadge :status="claim.delivery_status" :label="deliveryStatusLabel(claim.delivery_status)" />
              </td>
              <td class="px-4 py-2">{{ claim.attempt_count }}</td>
              <td class="px-4 py-2 text-gray-500">{{ claim.last_attempt_at ? formatDateTime(claim.last_attempt_at) : '-' }}</td>
              <td class="max-w-xs truncate px-4 py-2 text-gray-500" :title="claim.delivery_error || ''">
                {{ claim.delivery_error || '-' }}
              </td>
              <td class="px-4 py-2 text-gray-500">{{ formatDateTime(claim.created_at) }}</td>
              <td class="px-4 py-2">
                <div class="flex items-center justify-end gap-1.5">
                  <button v-if="claim.delivery_status === 'failed'" class="btn btn-primary btn-xs" @click="resend(claim)">
                    {{ t('admin.xianyu.deliveries.resend') }}
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        <div class="flex items-center justify-end border-t border-gray-100 p-3 dark:border-dark-700">
          <Pagination
            :page="page"
            :total="total"
            :page-size="pageSize"
            @update:page="onPage"
            @update:pageSize="onPageSize"
          />
        </div>
      </div>
      <EmptyState v-else-if="activeTab === 'inventory'" :message="t('admin.xianyu.deliveries.noDeliveries')" />

      <!-- Worker 自动发货记录（订单级汇总，不复制卡券内容） -->
      <div v-if="activeTab === 'worker' && workerClaims.length" class="overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-700">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-gray-200 bg-gray-50 text-left dark:border-dark-700 dark:bg-dark-800">
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.orderNo') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.deliveryKind') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.quantity') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.quantitySent') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.status') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.error') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.deliveries.createdAt') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in workerClaims" :key="item.order_no" class="border-b border-gray-100 dark:border-dark-700">
              <td class="px-4 py-2 font-mono text-xs">{{ item.order_no }}</td>
              <td class="px-4 py-2">{{ item.delivery_kind }}</td>
              <td class="px-4 py-2">{{ item.quantity }}</td>
              <td class="px-4 py-2">{{ item.quantity_sent }}</td>
              <td class="px-4 py-2">
                <StatusBadge :status="item.delivery_status" :label="deliveryStatusLabel(item.delivery_status)" />
              </td>
              <td class="max-w-xs truncate px-4 py-2 text-gray-500" :title="item.delivery_error || ''">
                {{ item.delivery_error || '-' }}
              </td>
              <td class="px-4 py-2 text-gray-500">{{ formatDateTime(item.created_at) }}</td>
            </tr>
          </tbody>
        </table>
        <div class="flex items-center justify-end border-t border-gray-100 p-3 dark:border-dark-700">
          <Pagination
            :page="page"
            :total="total"
            :page-size="pageSize"
            @update:page="onPage"
            @update:pageSize="onPageSize"
          />
        </div>
      </div>
      <EmptyState v-else-if="activeTab === 'worker'" :message="t('admin.xianyu.deliveries.noWorkerDeliveries')" />

      <ConfirmDialog
        :show="confirmVisible"
        :title="t('admin.xianyu.deliveries.resend')"
        :message="t('admin.xianyu.deliveries.confirmResend')"
        @confirm="doResend"
        @cancel="confirmVisible = false"
      />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import { formatDateTime } from '@/utils/format'
import type { XianyuOrderClaim, XianyuWorkerDelivery } from '@/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import StatusBadge from '@/components/common/StatusBadge.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import Select from '@/components/common/Select.vue'
import Pagination from '@/components/common/Pagination.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Icon from '@/components/icons/Icon.vue'

const { t } = useI18n()
const appStore = useAppStore()

const activeTab = ref<'inventory' | 'worker'>('inventory')
const claims = ref<XianyuOrderClaim[]>([])
const workerClaims = ref<XianyuWorkerDelivery[]>([])
const search = ref('')
const statusFilter = ref('')
const page = ref(1)
const pageSize = ref(20)
const total = ref(0)
const loading = ref(false)

const statusOptions = [
  { value: '', label: t('admin.xianyu.deliveries.status') },
  { value: 'pending', label: t('admin.xianyu.deliveries.pending') },
  { value: 'sent', label: t('admin.xianyu.deliveries.sent') },
  { value: 'failed', label: t('admin.xianyu.deliveries.failed') }
]

async function load() {
  loading.value = true
  try {
    const filter = {
      status: statusFilter.value || undefined,
      search: search.value || undefined,
      page: page.value,
      page_size: pageSize.value
    }
    if (activeTab.value === 'worker') {
      const resp = await adminAPI.xianyu.listWorkerDeliveries(filter)
      workerClaims.value = resp.items ?? []
      total.value = resp.total ?? 0
    } else {
      const resp = await adminAPI.xianyu.listDeliveries(filter)
      claims.value = resp.items ?? []
      total.value = resp.total ?? 0
    }
  } catch (err) {
    appStore.showError(String(err))
  } finally {
    loading.value = false
  }
}

function switchTab(tab: 'inventory' | 'worker') {
  if (activeTab.value === tab) return
  activeTab.value = tab
  page.value = 1
  load()
}

function onPage(value: number) {
  page.value = value
  load()
}
function onPageSize(value: number) {
  pageSize.value = value
  page.value = 1
  load()
}

function deliveryStatusLabel(status: string): string {
  switch (status) {
    case 'pending': return t('admin.xianyu.deliveries.pending')
    case 'sent': return t('admin.xianyu.deliveries.sent')
    case 'failed': return t('admin.xianyu.deliveries.failed')
    default: return t('admin.xianyu.deliveries.legacyUnverified')
  }
}

const revealedCodes = ref<Set<string>>(new Set())

function toggleReveal(orderNo: string) {
  const next = new Set(revealedCodes.value)
  if (next.has(orderNo)) {
    next.delete(orderNo)
  } else {
    next.add(orderNo)
  }
  revealedCodes.value = next
}

function maskCode(code: string): string {
  if (!code) return '-'
  if (code.length <= 4) return '*'.repeat(code.length)
  return `${code.slice(0, 2)}${'*'.repeat(Math.max(4, code.length - 4))}${code.slice(-2)}`
}

const confirmVisible = ref(false)
const resendTarget = ref<XianyuOrderClaim | null>(null)

function resend(claim: XianyuOrderClaim) {
  resendTarget.value = claim
  confirmVisible.value = true
}

async function doResend() {
  confirmVisible.value = false
  if (!resendTarget.value) return
  try {
    await adminAPI.xianyu.resendDelivery(resendTarget.value.order_no)
    await load()
    appStore.showSuccess(t('admin.xianyu.deliveries.resendConfirmed'))
  } catch (err) {
    appStore.showError(String(err))
  }
}

onMounted(load)
</script>
