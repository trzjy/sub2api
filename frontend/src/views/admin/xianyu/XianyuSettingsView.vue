<template>
  <AppLayout>
    <div class="p-6">
      <div class="mb-6">
        <h1 class="text-2xl font-bold">{{ t('admin.xianyu.settings.title') }}</h1>
        <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.xianyu.settings.description') }}</p>
      </div>

      <div class="grid grid-cols-1 gap-6 lg:grid-cols-2">
        <div class="rounded-lg border border-gray-200 dark:border-dark-700">
          <div class="border-b border-gray-200 px-5 py-3 dark:border-dark-700">
            <h2 class="font-semibold">{{ t('admin.xianyu.settings.workerConfig') }}</h2>
          </div>
          <div class="space-y-4 p-5">
            <div v-if="workerConfig" class="mb-2 flex items-center gap-2 text-sm">
              <span>{{ t('admin.xianyu.settings.workerStatus') }}:</span>
              <StatusBadge
                :status="workerConfig.health_status"
                :label="healthLabel(workerConfig.health_status)"
              />
            </div>
            <div>
              <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.settings.baseUrl') }}</label>
              <input v-model="form.base_url" class="input w-full" autocomplete="off" autocapitalize="off" spellcheck="false" :placeholder="t('admin.xianyu.settings.baseUrlHint')" />
              <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.xianyu.settings.baseUrlHint') }}</p>
            </div>
            <div>
              <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.settings.apiToken') }}</label>
              <input v-model="form.api_token" type="password" class="input w-full" autocomplete="new-password" :placeholder="t('admin.xianyu.settings.apiTokenHint')" />
              <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.xianyu.settings.apiTokenHint') }}</p>
              <p v-if="workerConfig" class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('admin.xianyu.settings.tokenLeaveBlank') }}</p>
            </div>
            <div class="flex items-center gap-2">
              <button class="btn btn-primary" @click="saveWorker">
                {{ t('admin.xianyu.settings.saveWorker') }}
              </button>
              <button class="btn btn-secondary" @click="checkHealth">
                {{ t('admin.xianyu.settings.healthCheck') }}
              </button>
            </div>
          </div>
        </div>

        <div class="rounded-lg border border-gray-200 dark:border-dark-700">
          <div class="border-b border-gray-200 px-5 py-3 dark:border-dark-700">
            <h2 class="font-semibold">{{ t('admin.xianyu.settings.title') }}</h2>
          </div>
          <div class="space-y-4 p-5">
            <div class="flex items-center justify-between">
              <div>
                <label class="text-sm font-medium">{{ t('admin.xianyu.settings.deliveryEnabled') }}</label>
                <p class="text-xs text-gray-400 dark:text-gray-500">{{ t('admin.xianyu.settings.deliveryEnabledHint') }}</p>
              </div>
              <Toggle v-model="settingsForm.delivery_enabled" />
            </div>
            <div class="flex items-center justify-between">
              <div>
                <label class="text-sm font-medium">{{ t('admin.xianyu.settings.accountAutoRefresh') }}</label>
                <p class="text-xs text-gray-400 dark:text-gray-500">{{ t('admin.xianyu.settings.accountAutoRefreshHint') }}</p>
              </div>
              <Toggle v-model="settingsForm.account_auto_refresh" />
            </div>
            <div class="flex items-center justify-between">
              <div>
                <label class="text-sm font-medium">{{ t('admin.xianyu.settings.productAutoBind') }}</label>
              </div>
              <Toggle v-model="settingsForm.product_auto_bind" />
            </div>
            <div>
              <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.settings.productSyncPeriod') }}</label>
              <input v-model.number="settingsForm.sync_interval_minutes" type="number" min="1" class="input w-full" />
              <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.xianyu.settings.productSyncPeriodHint') }}</p>
            </div>
            <div class="flex justify-end">
              <button class="btn btn-primary" @click="saveToggles">
                {{ t('admin.xianyu.settings.saveToggle') }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type { XianyuWorkerConfig } from '@/types'
import { extractApiErrorMessage } from '@/utils/apiError'
import AppLayout from '@/components/layout/AppLayout.vue'
import StatusBadge from '@/components/common/StatusBadge.vue'
import Toggle from '@/components/common/Toggle.vue'

const { t } = useI18n()
const appStore = useAppStore()

const workerConfig = ref<XianyuWorkerConfig | null>(null)
const form = reactive<{ base_url: string; api_token: string; status?: 'active' | 'disabled' }>({ base_url: '', api_token: '', status: 'active' })

async function load() {
  try {
    const configs = await adminAPI.xianyu.listWorkerConfigs()
    const active = configs.find((c) => c.status === 'active') || configs[0]
    workerConfig.value = active ?? null
    form.base_url = active?.base_url ?? ''
    form.api_token = ''
    form.status = active?.status ?? 'active'
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.xianyu.settings.loadWorkerFailed')))
  }
}

async function saveWorker() {
  if (!form.base_url.trim()) {
    appStore.showError(t('admin.xianyu.settings.baseUrlInvalid'))
    return
  }
  if (!form.api_token.trim() && !workerConfig.value) {
    appStore.showError(t('admin.xianyu.settings.tokenRequired'))
    return
  }
  try {
    const saved = await adminAPI.xianyu.saveWorkerConfig({
      id: workerConfig.value?.id,
      base_url: form.base_url.trim(),
      api_token: form.api_token || undefined,
      status: form.status
    })
    workerConfig.value = saved
    form.api_token = ''
    appStore.showSuccess(t('admin.xianyu.settings.workerConfigSaved'))
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.xianyu.settings.saveWorkerFailed')))
  }
}

async function checkHealth() {
  try {
    await adminAPI.xianyu.checkHealth()
    await load()
    appStore.showSuccess(t('admin.xianyu.settings.healthCheck'))
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.xianyu.settings.healthCheckFailed')))
  }
}

const settingsForm = reactive({
  delivery_enabled: false,
  account_auto_refresh: true,
  product_auto_bind: true,
  sync_interval_minutes: 5
})

async function loadToggles() {
  try {
    const s = await adminAPI.xianyu.getSettings()
    settingsForm.delivery_enabled = s.delivery_enabled
    settingsForm.account_auto_refresh = s.account_auto_refresh
    settingsForm.product_auto_bind = s.product_auto_bind
    settingsForm.sync_interval_minutes = s.sync_interval_minutes
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.xianyu.settings.loadSettingsFailed')))
  }
}

async function saveToggles() {
  try {
    await adminAPI.xianyu.saveSettings({
      delivery_enabled: settingsForm.delivery_enabled,
      account_auto_refresh: settingsForm.account_auto_refresh,
      product_auto_bind: settingsForm.product_auto_bind,
      sync_interval_minutes: settingsForm.sync_interval_minutes
    })
    appStore.showSuccess(t('admin.xianyu.settings.success'))
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.xianyu.settings.saveSettingsFailed')))
  }
}

function healthLabel(status: string): string {
  switch (status) {
    case 'healthy': return t('admin.xianyu.settings.workerHealthy')
    case 'unhealthy': return t('admin.xianyu.settings.workerUnhealthy')
    default: return t('admin.xianyu.settings.workerUnknown')
  }
}

onMounted(() => {
  load()
  loadToggles()
})
</script>
