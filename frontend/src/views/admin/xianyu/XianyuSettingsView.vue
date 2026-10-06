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

      <div class="mt-6 rounded-lg border border-gray-200 dark:border-dark-700">
        <div class="border-b border-gray-200 px-5 py-3 dark:border-dark-700">
          <h2 class="font-semibold">{{ t('admin.xianyu.settings.deliveryTemplates') }}</h2>
        </div>
        <div class="space-y-4 p-5">
          <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.xianyu.settings.deliveryTemplatesHint') }}</p>
          <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-600">
            <textarea v-model="deliveryTemplate" rows="12" class="input w-full resize-y font-mono text-xs"></textarea>
            <div class="mt-2 rounded-lg bg-gray-50 p-3 text-xs text-gray-600 dark:bg-dark-700 dark:text-gray-300">
              <p class="mb-1 font-medium">{{ t('admin.xianyu.settings.deliveryTemplatePreview') }}</p>
              <p class="whitespace-pre-wrap">{{ templatePreview(deliveryTemplate) }}</p>
            </div>
            <div class="mt-2 flex justify-end">
              <button class="btn btn-primary btn-sm" :disabled="savingTemplate" @click="saveDeliveryTemplate">
                {{ savingTemplate ? t('admin.xianyu.settings.deliveryTemplateSaving') : t('admin.xianyu.settings.deliveryTemplateSave') }}
              </button>
            </div>
          </div>
        </div>
      </div>

      <!-- 曝光助手 -->
      <div class="mt-6 rounded-lg border border-gray-200 dark:border-dark-700">
        <div class="flex items-center justify-between border-b border-gray-200 px-5 py-3 dark:border-dark-700">
          <h2 class="font-semibold">{{ t('admin.xianyu.settings.exposureTitle') }}</h2>
          <Toggle v-model="settingsForm.exposure_enabled" />
        </div>
        <div class="space-y-4 p-5">
          <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.xianyu.settings.exposureHint') }}</p>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.settings.exposureCron') }}</label>
            <input v-model="settingsForm.exposure_cron" class="input w-full font-mono" placeholder="0 10,15,20 * * *" />
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.xianyu.settings.exposureCronHint') }}</p>
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.settings.exposureWecomWebhook') }}</label>
            <input v-model="settingsForm.exposure_wecom_webhook" type="password" class="input w-full" autocomplete="new-password" />
          </div>
          <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
            <div>
              <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.settings.exposureAIBaseURL') }}</label>
              <input v-model="settingsForm.exposure_ai_base_url" class="input w-full" placeholder="https://..." />
            </div>
            <div>
              <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.settings.exposureAIKey') }}</label>
              <input v-model="settingsForm.exposure_ai_api_key" type="password" class="input w-full" autocomplete="new-password" />
            </div>
            <div>
              <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.settings.exposureAIModel') }}</label>
              <input v-model="settingsForm.exposure_ai_model" class="input w-full" />
            </div>
          </div>
          <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
            <div>
              <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.settings.exposureStaleDays') }}</label>
              <input v-model.number="settingsForm.exposure_stale_days" type="number" min="0" class="input w-full" />
            </div>
            <div>
              <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.settings.exposureMaxOrders') }}</label>
              <input v-model.number="settingsForm.exposure_max_orders" type="number" min="0" class="input w-full" />
            </div>
            <div>
              <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.settings.exposureCacheMinutes') }}</label>
              <input v-model.number="settingsForm.exposure_market_cache_minutes" type="number" min="0" class="input w-full" />
            </div>
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.settings.exposureBlockedWords') }}</label>
            <input v-model="settingsForm.exposure_blocked_words" class="input w-full" :placeholder="t('admin.xianyu.settings.exposureBlockedWordsHint')" />
          </div>
          <div class="flex flex-wrap items-center gap-2">
            <button class="btn btn-primary" :disabled="savingExposure" @click="saveExposure">
              {{ t('admin.xianyu.settings.saveToggle') }}
            </button>
            <button class="btn btn-secondary" :disabled="testingPush" @click="testPush">
              {{ t('admin.xianyu.settings.exposureTestPush') }}
            </button>
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
import AppLayout from '@/components/layout/AppLayout.vue'
import StatusBadge from '@/components/common/StatusBadge.vue'
import Toggle from '@/components/common/Toggle.vue'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const appStore = useAppStore()

const workerConfig = ref<XianyuWorkerConfig | null>(null)
const deliveryTemplate = ref('')
const savingTemplate = ref(false)
const form = reactive<{ base_url: string; api_token: string; status?: 'active' | 'disabled' }>({ base_url: '', api_token: '', status: 'active' })

async function loadDeliveryTemplate() {
  try {
    deliveryTemplate.value = await adminAPI.xianyu.getDeliveryTemplate()
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

async function saveDeliveryTemplate() {
  savingTemplate.value = true
  try {
    await adminAPI.xianyu.saveDeliveryTemplate(deliveryTemplate.value)
    appStore.showSuccess(t('admin.xianyu.settings.deliveryTemplateSaved'))
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  } finally {
    savingTemplate.value = false
  }
}

const TEMPLATE_PREVIEW_SAMPLES: Record<string, string> = {
  DELIVERY_CONTENT: '7391e6babc573a773431baa694a6120e',
  item_title: 'GLM-5.3 24h天卡 不限次数 自动发货',
  order_id: '5127403682497093139',
  buyer_name: '买家昵称',
  buyer_id: '838831211',
  seller_name: '卖家昵称',
  item_id: '1080213108214'
}

function templatePreview(description: string): string {
  if (!description.trim()) {
    return t('admin.xianyu.settings.deliveryTemplatePreviewEmpty')
  }
  let out = description
  for (const [key, value] of Object.entries(TEMPLATE_PREVIEW_SAMPLES)) {
    out = out.split(`{${key}}`).join(value)
  }
  return out
}

async function load() {
  try {
    const configs = await adminAPI.xianyu.listWorkerConfigs()
    const active = configs.find((c) => c.status === 'active') || configs[0]
    workerConfig.value = active ?? null
    form.base_url = active?.base_url ?? ''
    form.api_token = ''
    form.status = active?.status ?? 'active'
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
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
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

async function checkHealth() {
  try {
    await adminAPI.xianyu.checkHealth()
    await load()
    appStore.showSuccess(t('admin.xianyu.settings.healthCheck'))
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

const settingsForm = reactive({
  delivery_enabled: false,
  account_auto_refresh: true,
  product_auto_bind: true,
  sync_interval_minutes: 5,
  // 曝光助手
  exposure_enabled: false,
  exposure_cron: '0 10,15,20 * * *',
  exposure_wecom_webhook: '',
  exposure_ai_base_url: '',
  exposure_ai_api_key: '',
  exposure_ai_model: '',
  exposure_stale_days: 7,
  exposure_max_orders: 1,
  exposure_market_cache_minutes: 240,
  exposure_blocked_words: '官方,拼车,共享,换绑,老号,claude,cursor'
})

const savingExposure = ref(false)
const testingPush = ref(false)

async function loadToggles() {
  try {
    const s = await adminAPI.xianyu.getSettings()
    settingsForm.delivery_enabled = s.delivery_enabled
    settingsForm.account_auto_refresh = s.account_auto_refresh
    settingsForm.product_auto_bind = s.product_auto_bind
    settingsForm.sync_interval_minutes = s.sync_interval_minutes
    if (s.exposure_enabled !== undefined) settingsForm.exposure_enabled = s.exposure_enabled
    if (s.exposure_cron) settingsForm.exposure_cron = s.exposure_cron
    if (s.exposure_wecom_webhook) settingsForm.exposure_wecom_webhook = s.exposure_wecom_webhook
    if (s.exposure_ai_base_url) settingsForm.exposure_ai_base_url = s.exposure_ai_base_url
    if (s.exposure_ai_api_key) settingsForm.exposure_ai_api_key = s.exposure_ai_api_key
    if (s.exposure_ai_model) settingsForm.exposure_ai_model = s.exposure_ai_model
    if (s.exposure_stale_days !== undefined) settingsForm.exposure_stale_days = s.exposure_stale_days
    if (s.exposure_max_orders !== undefined) settingsForm.exposure_max_orders = s.exposure_max_orders
    if (s.exposure_market_cache_minutes !== undefined) settingsForm.exposure_market_cache_minutes = s.exposure_market_cache_minutes
    if (s.exposure_blocked_words !== undefined) settingsForm.exposure_blocked_words = s.exposure_blocked_words
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
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
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

async function saveExposure() {
  savingExposure.value = true
  try {
    await adminAPI.xianyu.saveSettings({
      exposure_enabled: settingsForm.exposure_enabled,
      exposure_cron: settingsForm.exposure_cron,
      exposure_wecom_webhook: settingsForm.exposure_wecom_webhook,
      exposure_ai_base_url: settingsForm.exposure_ai_base_url,
      exposure_ai_api_key: settingsForm.exposure_ai_api_key,
      exposure_ai_model: settingsForm.exposure_ai_model,
      exposure_stale_days: settingsForm.exposure_stale_days,
      exposure_max_orders: settingsForm.exposure_max_orders,
      exposure_market_cache_minutes: settingsForm.exposure_market_cache_minutes,
      exposure_blocked_words: settingsForm.exposure_blocked_words
    })
    appStore.showSuccess(t('admin.xianyu.settings.success'))
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  } finally {
    savingExposure.value = false
  }
}

async function testPush() {
  testingPush.value = true
  try {
    await adminAPI.xianyu.testExposurePush()
    appStore.showSuccess(t('admin.xianyu.settings.exposureTestPushSent'))
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  } finally {
    testingPush.value = false
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
  loadDeliveryTemplate()
})
</script>
