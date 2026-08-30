<template>
  <AppLayout>
    <div class="p-6">
      <div class="mb-6 flex items-start justify-between">
        <div>
          <h1 class="text-2xl font-bold">{{ t('admin.xianyu.accounts.title') }}</h1>
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.xianyu.accounts.description') }}</p>
          <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('admin.xianyu.accounts.syncHint') }}</p>
        </div>
        <div class="flex items-center gap-2">
          <button class="btn btn-secondary" :disabled="loading" @click="sync">
            <Icon name="sync" size="sm" /> {{ t('admin.xianyu.accounts.sync') }}
          </button>
          <button class="btn btn-secondary" :disabled="loading" @click="load">
            <Icon name="refresh" size="sm" />
          </button>
          <button class="btn btn-primary" :disabled="loading" @click="openScan(null)">
            {{ t('admin.xianyu.accounts.scanLogin') }}
          </button>
        </div>
      </div>

      <div v-if="syncError" class="mb-4 rounded border border-red-200 bg-red-50 px-4 py-2 text-sm text-red-700 dark:border-dark-700 dark:bg-red-900/20 dark:text-red-300">
        {{ syncError }}
      </div>

      <div v-if="accounts.length" class="overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-700">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-gray-200 bg-gray-50 text-left dark:border-dark-700 dark:bg-dark-800">
              <th class="px-4 py-2">{{ t('admin.xianyu.accounts.nickname') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.accounts.accountId') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.accounts.status') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.accounts.cookieStatus') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.accounts.taskStatus') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.accounts.lastLoginAt') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.accounts.lastSeenAt') }}</th>
              <th class="px-4 py-2 text-right">{{ t('common.actions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="account in accounts"
              :key="account.id"
              class="border-b border-gray-100 dark:border-dark-700"
            >
              <td class="px-4 py-2 font-medium">{{ account.nickname || '-' }}</td>
              <td class="px-4 py-2">{{ account.account_id }}</td>
              <td class="px-4 py-2">
                <StatusBadge
                  :status="account.status"
                  :label="statusLabel(account.status)"
                />
              </td>
              <td class="px-4 py-2">
                <StatusBadge
                  :status="account.cookie_status"
                  :label="cookieLabel(account.cookie_status)"
                />
              </td>
              <td class="px-4 py-2">
                <StatusBadge
                  :status="account.task_status"
                  :label="taskLabel(account.task_status)"
                />
              </td>
              <td class="px-4 py-2 text-gray-500">{{ account.last_login_at ? formatDateTime(account.last_login_at) : '-' }}</td>
              <td class="px-4 py-2 text-gray-500">{{ account.last_seen_at ? formatDateTime(account.last_seen_at) : '-' }}</td>
              <td class="px-4 py-2">
                <div class="flex items-center justify-end gap-1.5">
                  <button v-if="account.status !== 'enabled'" class="btn btn-primary btn-xs" @click="enable(account)">
                    {{ t('admin.xianyu.accounts.enable') }}
                  </button>
                  <button v-else class="btn btn-secondary btn-xs" @click="disable(account)">
                    {{ t('admin.xianyu.accounts.disable') }}
                  </button>
                  <button class="btn btn-secondary btn-xs" @click="doRefreshCookie(account)">
                    {{ t('admin.xianyu.accounts.refreshCookie') }}
                  </button>
                  <button class="btn btn-secondary btn-xs" @click="openScan(account)">
                    {{ t('admin.xianyu.accounts.scanLogin') }}
                  </button>
                  <button class="btn btn-danger btn-xs" @click="doClearCredentials(account)">
                    {{ t('admin.xianyu.accounts.clearCredentials') }}
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <EmptyState v-else :message="t('admin.xianyu.accounts.noAccounts')">
        <template #action>
          <button class="btn btn-primary btn-sm" :disabled="loading" @click="openScan(null)">
            {{ t('admin.xianyu.accounts.scanLogin') }}
          </button>
        </template>
      </EmptyState>

      <BaseDialog :show="scanVisible" :title="t('admin.xianyu.accounts.scanTitle')" @close="stopPollingBehavior">
        <div class="flex flex-col items-center gap-3">
          <div v-if="scanStatus === 'success'" class="text-green-600">
            {{ t('admin.xianyu.accounts.scanSuccess') }}
          </div>
          <div v-else-if="scanStatus === 'failed'" class="text-red-600">
            {{ scanMessage || t('admin.xianyu.accounts.scanFailed') }}
          </div>
          <div v-else-if="scanStatus === 'expired'" class="text-red-600">
            {{ t('admin.xianyu.accounts.scanExpired') }}
          </div>
          <div v-else-if="scanQRCode" class="flex flex-col items-center gap-2">
            <img :src="scanQRCode" class="h-56 w-56 rounded border border-gray-200 dark:border-dark-700" alt="QR" />
            <span class="text-sm text-gray-500">
              {{ scanStatus === 'scanned' ? t('admin.xianyu.accounts.scanScanned') : t('admin.xianyu.accounts.scanWaiting') }}
            </span>
          </div>
          <div v-else class="text-gray-500">...</div>
        </div>
      </BaseDialog>

      <ConfirmDialog
        :show="confirmVisible"
        :title="confirmTitle"
        :message="confirmMessage"
        @confirm="doConfirmAction"
        @cancel="confirmVisible = false"
      />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import { formatDateTime } from '@/utils/format'
import type { XianyuAccount } from '@/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import StatusBadge from '@/components/common/StatusBadge.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Icon from '@/components/icons/Icon.vue'

const { t } = useI18n()
const appStore = useAppStore()

const accounts = ref<XianyuAccount[]>([])
const loading = ref(false)
const syncError = ref('')

async function load() {
  loading.value = true
  try {
    accounts.value = await adminAPI.xianyu.listAccounts()
    syncError.value = ''
  } catch (err) {
    appStore.showError(String(err))
  } finally {
    loading.value = false
  }
}

async function sync() {
  syncError.value = ''
  try {
    await adminAPI.xianyu.syncAccounts()
    await load()
    appStore.showSuccess(t('admin.xianyu.accounts.success'))
  } catch (err) {
    syncError.value = String(err)
    appStore.showError(String(err))
  }
}

const confirmVisible = ref(false)
const confirmTitle = ref('')
const confirmMessage = ref('')
let pendingAction: (() => void) | null = null

function ask(title: string, message: string, action: () => void) {
  confirmTitle.value = title
  confirmMessage.value = message
  pendingAction = action
  confirmVisible.value = true
}

function doConfirmAction() {
  confirmVisible.value = false
  if (pendingAction) {
    const action = pendingAction
    pendingAction = null
    action()
  }
}

function enable(account: XianyuAccount) {
  ask(
    t('admin.xianyu.accounts.enable'),
    t('admin.xianyu.accounts.confirmEnable'),
    async () => {
      try {
        await adminAPI.xianyu.enableAccount(account.account_id)
        await load()
        appStore.showSuccess(t('admin.xianyu.accounts.success'))
      } catch (err) {
        appStore.showError(String(err))
      }
    }
  )
}

function disable(account: XianyuAccount) {
  ask(
    t('admin.xianyu.accounts.disable'),
    t('admin.xianyu.accounts.confirmDisable'),
    async () => {
      try {
        await adminAPI.xianyu.disableAccount(account.account_id)
        await load()
        appStore.showSuccess(t('admin.xianyu.accounts.success'))
      } catch (err) {
        appStore.showError(String(err))
      }
    }
  )
}

function doRefreshCookie(account: XianyuAccount) {
  ask(
    t('admin.xianyu.accounts.refreshCookie'),
    t('admin.xianyu.accounts.confirmRefresh'),
    async () => {
      try {
        await adminAPI.xianyu.refreshCookie(account.account_id)
        await load()
        appStore.showSuccess(t('admin.xianyu.accounts.success'))
      } catch (err) {
        appStore.showError(String(err))
      }
    }
  )
}

function doClearCredentials(account: XianyuAccount) {
  ask(
    t('admin.xianyu.accounts.clearCredentials'),
    t('admin.xianyu.accounts.confirmClearCredentials', { nickname: account.nickname || account.account_id }),
    async () => {
      try {
        await adminAPI.xianyu.clearCredentials(account.account_id)
        await load()
        appStore.showSuccess(t('admin.xianyu.accounts.success'))
      } catch (err) {
        appStore.showError(String(err))
      }
    }
  )
}

const scanVisible = ref(false)
const scanAccount = ref<XianyuAccount | null>(null)
const scanSessionID = ref('')
const scanStatus = ref('waiting')
const scanMessage = ref('')
const scanQRCode = ref('')
let pollTimer: number | null = null

async function openScan(account: XianyuAccount | null) {
  scanAccount.value = account
  scanStatus.value = 'waiting'
  scanMessage.value = ''
  scanQRCode.value = ''
  scanSessionID.value = ''
  scanVisible.value = true
  try {
    const session = await adminAPI.xianyu.createLoginSession(account?.account_id || '')
    scanStatus.value = session.status
    scanQRCode.value = session.qr_code || ''
    scanMessage.value = session.message || ''
    scanSessionID.value = session.session_id || ''
    if (!scanSessionID.value) {
      appStore.showError(t('admin.xianyu.accounts.scanNoSession'))
      scanVisible.value = false
      return
    }
  } catch (err) {
    appStore.showError(String(err))
    scanVisible.value = false
    return
  }
  startPolling(scanSessionID.value)
}

function startPolling(sessionID: string) {
  stopPolling()
  pollCount = 0
  pollTimer = window.setTimeout(() => pollOnce(sessionID), 2000)
}

const MAX_POLL_ATTEMPTS = 60 // 2s 间隔 × 60 ≈ 120s 上限，防止无限轮询

let pollCount = 0

async function pollOnce(sessionID: string) {
  pollCount++
  if (pollCount > MAX_POLL_ATTEMPTS) {
    stopPolling()
    scanStatus.value = 'failed'
    scanMessage.value = t('admin.xianyu.accounts.scanTimeout')
    return
  }
  try {
    const session = await adminAPI.xianyu.queryLoginSession(sessionID)
    scanStatus.value = session.status
    scanMessage.value = session.message || ''
    if (session.status === 'success' || session.status === 'failed' || session.status === 'expired') {
      stopPolling()
      if (session.status === 'success') {
        await load()
      }
      return
    }
  } catch (err) {
    stopPolling()
    appStore.showError(String(err))
    return
  }
  // 上一轮完成后再调度下一轮，避免 setInterval 并发请求堆积
  if (pollCount <= MAX_POLL_ATTEMPTS) {
    pollTimer = window.setTimeout(() => pollOnce(sessionID), 2000)
  }
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

function stopPollingBehavior() {
  scanVisible.value = false
  stopPolling()
}

function statusLabel(status: string): string {
  switch (status) {
    case 'enabled': return t('admin.xianyu.accounts.enabled')
    case 'disabled': return t('admin.xianyu.accounts.disabled')
    case 'expired': return t('admin.xianyu.accounts.expired')
    case 'syncing': return t('admin.xianyu.accounts.syncing')
    default: return status
  }
}

function cookieLabel(status: string): string {
  switch (status) {
    case 'valid': return t('admin.xianyu.accounts.valid')
    case 'invalid': return t('admin.xianyu.accounts.invalid')
    case 'expiring': return t('admin.xianyu.accounts.expiring')
    default: return t('admin.xianyu.accounts.unknown')
  }
}

function taskLabel(status: string): string {
  switch (status) {
    case 'running': return t('admin.xianyu.accounts.running')
    case 'stopped': return t('admin.xianyu.accounts.stopped')
    case 'starting': return t('admin.xianyu.accounts.starting')
    case 'stopping': return t('admin.xianyu.accounts.stopping')
    default: return t('admin.xianyu.accounts.unknown')
  }
}

onMounted(load)
onUnmounted(stopPolling)
</script>
