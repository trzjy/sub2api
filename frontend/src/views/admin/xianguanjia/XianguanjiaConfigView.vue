<template>
  <AppLayout>
    <div class="p-6">
      <div class="mb-6">
        <h1 class="text-2xl font-bold">{{ t('admin.xianguanjia.title') }}</h1>
        <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.xianguanjia.description') }}</p>
      </div>

      <div class="max-w-2xl rounded-lg border border-gray-200 dark:border-dark-700">
        <div class="flex items-center justify-between border-b border-gray-200 px-5 py-3 dark:border-dark-700">
          <h2 class="font-semibold">{{ t('admin.xianguanjia.formTitle') }}</h2>
          <div v-if="config" class="flex items-center gap-4">
            <StatusBadge
              :status="config.status"
              :label="statusLabel(config.status)"
            />
            <StatusBadge
              :status="config.health_status"
              :label="healthLabel(config.health_status)"
            />
          </div>
        </div>
        <div class="space-y-4 p-5">
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianguanjia.appId') }}</label>
            <input v-model="form.app_id" class="input w-full" autocomplete="off" autocapitalize="off" spellcheck="false" />
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianguanjia.appSecret') }}</label>
            <input
              v-model="form.app_secret"
              type="password"
              class="input w-full"
              autocomplete="new-password"
              :placeholder="appSecretPlaceholder"
            />
            <p v-if="config?.app_secret_set" class="mt-1 text-xs text-gray-400 dark:text-gray-500">
              {{ t('admin.xianguanjia.appSecretTail', { tail: config.app_secret_tail }) }}
            </p>
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.xianguanjia.appSecretHint') }}</p>
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianguanjia.pushUrl') }}</label>
            <input v-model="form.push_url" class="input w-full" placeholder="https://corealgos.com/api/v1/webhook/xianguanjia" />
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.xianguanjia.pushUrlHint') }}</p>
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianguanjia.baseUrl') }}</label>
            <input v-model="form.base_url" class="input w-full" :placeholder="t('admin.xianguanjia.baseUrlPlaceholder')" />
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.xianguanjia.baseUrlHint') }}</p>
          </div>
          <div class="flex items-center gap-2">
            <button class="btn btn-primary" :disabled="saving" @click="save">
              {{ saving ? t('admin.xianguanjia.saving') : t('admin.xianguanjia.save') }}
            </button>
            <button class="btn btn-secondary" :disabled="probing || !config?.configured" @click="probe">
              {{ probing ? t('admin.xianguanjia.probing') : t('admin.xianguanjia.probe') }}
            </button>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type { XianguanjiaConfig } from '@/api/admin/xianguanjia'
import AppLayout from '@/components/layout/AppLayout.vue'
import StatusBadge from '@/components/common/StatusBadge.vue'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const appStore = useAppStore()

const config = ref<XianguanjiaConfig | null>(null)
const saving = ref(false)
const probing = ref(false)
const form = reactive({ app_id: '', app_secret: '', push_url: '', base_url: '' })

const appSecretPlaceholder = computed(() =>
  config.value?.app_secret_set ? t('admin.xianguanjia.appSecretKeepBlank') : t('admin.xianguanjia.appSecretRequired')
)

async function load() {
  try {
    const cfg = await adminAPI.xianguanjia.getConfig()
    config.value = cfg
    form.app_id = cfg.app_id ?? ''
    form.app_secret = ''
    form.push_url = cfg.push_url ?? ''
    form.base_url = cfg.base_url ?? ''
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

async function save() {
  if (!form.app_id.trim()) {
    appStore.showError(t('admin.xianguanjia.appIdRequired'))
    return
  }
  saving.value = true
  try {
    await adminAPI.xianguanjia.putConfig({
      app_id: form.app_id.trim(),
      app_secret: form.app_secret || undefined,
      push_url: form.push_url.trim(),
      base_url: form.base_url.trim()
    })
    appStore.showSuccess(t('admin.xianguanjia.saved'))
    await load()
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  } finally {
    saving.value = false
  }
}

async function probe() {
  probing.value = true
  try {
    const res = await adminAPI.xianguanjia.healthCheck()
    if (res.health_status === 'healthy') {
      appStore.showSuccess(t('admin.xianguanjia.probeHealthy'))
    } else {
      appStore.showError(t('admin.xianguanjia.probeUnhealthy'))
    }
    await load()
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  } finally {
    probing.value = false
  }
}

function statusLabel(status: string): string {
  if (status === 'active') return t('admin.xianguanjia.statusActive')
  return t('admin.xianguanjia.statusDisabled')
}

function healthLabel(status: string): string {
  switch (status) {
    case 'healthy':
      return t('admin.xianguanjia.healthHealthy')
    case 'unhealthy':
      return t('admin.xianguanjia.healthUnhealthy')
    default:
      return t('admin.xianguanjia.healthUnknown')
  }
}

onMounted(load)
</script>
