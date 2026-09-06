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
              <th class="px-4 py-2">{{ t('admin.xianyu.accounts.remark') }}</th>
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
              :class="account.status === 'logged_out' ? 'opacity-60' : ''"
            >
              <td class="px-4 py-2 font-medium">{{ account.nickname || '-' }}</td>
              <td class="px-4 py-2 font-mono text-xs">{{ account.account_id }}</td>
              <td class="px-4 py-2">
                <span>{{ account.remark || '-' }}</span>
                <button class="ml-1 text-xs text-gray-400 hover:text-gray-600" @click="openRemark(account)">
                  {{ t('common.edit') }}
                </button>
              </td>
              <td class="px-4 py-2">
                <StatusBadge
                  :status="statusTone(account.status)"
                  :label="statusLabel(account.status)"
                />
              </td>
              <td class="px-4 py-2" :title="account.cookie_detail || undefined">
                <StatusBadge
                  v-if="account.status === 'logged_out'"
                  status="cleared"
                  :label="t('admin.xianyu.accounts.cookieCleared')"
                />
                <StatusBadge
                  v-else
                  :status="cookieTone(account.cookie_status)"
                  :label="cookieLabel(account.cookie_status)"
                />
              </td>
              <td class="px-4 py-2">
                <StatusBadge
                  :status="taskTone(account)"
                  :label="taskLabel(account.task_status)"
                />
              </td>
              <td class="px-4 py-2 text-gray-500">{{ account.last_login_at ? formatDateTime(account.last_login_at) : '-' }}</td>
              <td class="px-4 py-2 text-gray-500">{{ account.last_seen_at ? formatDateTime(account.last_seen_at) : '-' }}</td>
              <td class="px-4 py-2">
                <div class="flex items-center justify-end gap-1.5">
                  <!-- 已退出的账号凭证已删除：仅可重新扫码登录，不再提供启用/刷新/退出 -->
                  <template v-if="account.status === 'logged_out'">
                    <button class="btn btn-primary btn-xs" @click="openScan(account)">
                      {{ t('admin.xianyu.accounts.relogin') }}
                    </button>
                  </template>
                  <template v-else>
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
                  </template>
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

      <BaseDialog :show="remarkVisible" :title="t('admin.xianyu.accounts.editRemark')" @close="remarkVisible = false">
        <div class="space-y-4">
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.accounts.accountId') }}</label>
            <p class="font-mono text-xs">{{ editingAccount?.account_id }}</p>
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.accounts.remark') }}</label>
            <input
              v-model="remarkForm"
              class="input w-full"
              maxlength="200"
              :placeholder="t('admin.xianyu.accounts.remarkPlaceholder')"
            />
          </div>
          <div class="flex justify-end gap-2">
            <button class="btn btn-secondary" @click="remarkVisible = false">{{ t('common.cancel') }}</button>
            <button class="btn btn-primary" @click="saveRemark">{{ t('common.save') }}</button>
          </div>
        </div>
      </BaseDialog>

      <BaseDialog :show="scanVisible" :title="t('admin.xianyu.accounts.scanTitle')" @close="stopPollingBehavior">
        <div class="flex flex-col items-center gap-3">
          <div v-if="scanStatus === 'success'" class="text-green-600">
            {{ t('admin.xianyu.accounts.scanSuccess') }}
            <span v-if="scanCountdown > 0" class="ml-1 text-sm text-gray-500">
              {{ t('admin.xianyu.accounts.scanAutoClose', { seconds: scanCountdown }) }}
            </span>
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
import { extractI18nErrorMessage, extractApiErrorCode } from '@/utils/apiError'
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
    appStore.showError(extractI18nErrorMessage(err, t, 'admin.xianyu.errors', t('common.error')))
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
    syncError.value = extractI18nErrorMessage(err, t, 'admin.xianyu.errors', t('common.error'))
    appStore.showError(extractI18nErrorMessage(err, t, 'admin.xianyu.errors', t('common.error')))
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
        appStore.showError(extractI18nErrorMessage(err, t, 'admin.xianyu.errors', t('common.error')))
        // 无法唤醒（Worker 侧账号已不存在/已退出登录）→ 直接弹出扫码登录引导重新登录。
        const code = extractApiErrorCode(err)
        if (code === 'XIANYU_WORKER_ACCOUNT_NOT_FOUND' || code === 'XIANYU_ACCOUNT_LOGGED_OUT') {
          await load()
          openScan(account)
        }
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
        appStore.showError(extractI18nErrorMessage(err, t, 'admin.xianyu.errors', t('common.error')))
      }
    }
  )
}

const remarkVisible = ref(false)
const editingAccount = ref<XianyuAccount | null>(null)
const remarkForm = ref('')

function openRemark(account: XianyuAccount) {
  editingAccount.value = account
  remarkForm.value = account.remark || ''
  remarkVisible.value = true
}

async function saveRemark() {
  if (!editingAccount.value) return
  try {
    await adminAPI.xianyu.saveAccountRemark(editingAccount.value.id, remarkForm.value.trim())
    remarkVisible.value = false
    await load()
    appStore.showSuccess(t('admin.xianyu.accounts.remarkSaved'))
  } catch (err) {
    appStore.showError(extractI18nErrorMessage(err, t, 'admin.xianyu.errors', t('common.error')))
  }
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
        appStore.showError(extractI18nErrorMessage(err, t, 'admin.xianyu.errors', t('common.error')))
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
        appStore.showError(extractI18nErrorMessage(err, t, 'admin.xianyu.errors', t('common.error')))
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
// 登录成功后倒计时自动关闭扫码弹窗。
let scanCloseTimer: number | null = null
const scanCountdown = ref(0)
const SCAN_AUTO_CLOSE_SECONDS = 2

function cancelScanAutoClose() {
  if (scanCloseTimer !== null) {
    window.clearInterval(scanCloseTimer)
    scanCloseTimer = null
  }
  scanCountdown.value = 0
}

function scheduleScanAutoClose() {
  cancelScanAutoClose()
  scanCountdown.value = SCAN_AUTO_CLOSE_SECONDS
  scanCloseTimer = window.setInterval(() => {
    scanCountdown.value -= 1
    if (scanCountdown.value <= 0) {
      cancelScanAutoClose()
      stopPollingBehavior()
    }
  }, 1000)
}

async function openScan(account: XianyuAccount | null) {
  cancelScanAutoClose()
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
    appStore.showError(extractI18nErrorMessage(err, t, 'admin.xianyu.errors', t('common.error')))
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
        // 登录成功即开始 2 秒倒计时自动关闭弹窗；同步刷新在后台并行完成。
        scheduleScanAutoClose()
        // 重新登录成功后立即同步一次投影，让该行马上回到可启用/已启用状态，而不是等下一轮自动巡检。
        try {
          await adminAPI.xianyu.syncAccounts()
        } catch {
          // 同步失败不阻塞提示：60 秒健康巡检也会收敛状态。
        }
        await load()
      }
      return
    }
  } catch (err) {
    stopPolling()
    appStore.showError(extractI18nErrorMessage(err, t, 'admin.xianyu.errors', t('common.error')))
    return
  }
  // 上一轮完成后再调度下一轮，避免 setInterval 并发请求堆积
  if (pollCount <= MAX_POLL_ATTEMPTS) {
    pollTimer = window.setTimeout(() => pollOnce(sessionID), 2000)
  }
}

function stopPolling() {
  if (pollTimer) {
    window.clearTimeout(pollTimer)
    pollTimer = null
  }
}

function stopPollingBehavior() {
  cancelScanAutoClose()
  scanVisible.value = false
  stopPolling()
}

function statusLabel(status: string): string {
  switch (status) {
    case 'enabled': return t('admin.xianyu.accounts.enabled')
    case 'disabled': return t('admin.xianyu.accounts.disabled')
    case 'expired': return t('admin.xianyu.accounts.expired')
    case 'syncing': return t('admin.xianyu.accounts.syncing')
    case 'logged_out': return t('admin.xianyu.accounts.loggedOut')
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

// 健康状态绿、异常红、中性灰（与 Cookie 列同规则）。
// 账号状态：已启用绿、已过期红、同步中黄；已停用/已退出登录走 StatusBadge 既有色（黄/灰）。
function statusTone(status: string): string {
  switch (status) {
    case 'enabled': return 'success'
    case 'expired': return 'error'
    case 'syncing': return 'warning'
    default: return status
  }
}

// 任务状态：运行中绿、启停中黄；账号仍处于启用态但任务已停止属异常，标红。
function taskTone(account: XianyuAccount): string {
  switch (account.task_status) {
    case 'running': return 'success'
    case 'starting':
    case 'stopping': return 'warning'
    case 'stopped': return account.status === 'enabled' ? 'error' : 'unknown'
    default: return account.task_status
  }
}

// 映射为 StatusBadge 的语义色：有效绿、即将过期黄、失效红、未知灰。
function cookieTone(status: string): string {
  switch (status) {
    case 'valid': return 'success'
    case 'expiring': return 'warning'
    case 'invalid': return 'error'
    default: return status
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
onUnmounted(() => {
  cancelScanAutoClose()
  stopPolling()
})
</script>
