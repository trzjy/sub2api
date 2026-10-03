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

      <!-- D4d: 卡密推仓区块（追加，不改既有凭证配置区块） -->
      <div class="mt-6 max-w-2xl rounded-lg border border-gray-200 dark:border-dark-700">
        <div class="flex items-center justify-between border-b border-gray-200 px-5 py-3 dark:border-dark-700">
          <h2 class="font-semibold">{{ t('admin.xianguanjia.pool.title') }}</h2>
          <span class="text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.xianguanjia.pool.currentKind', { id: currentKindId || t('admin.xianguanjia.pool.kindUnset') }) }}
          </span>
        </div>
        <div class="space-y-4 p-5">
          <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div>
              <label class="mb-1 block text-sm font-medium">{{ t('admin.xianguanjia.pool.kindName') }}</label>
              <input v-model="kindForm.name" class="input w-full" autocomplete="off" />
            </div>
            <div>
              <label class="mb-1 block text-sm font-medium">{{ t('admin.xianguanjia.pool.categoryId') }}</label>
              <input v-model="kindForm.category_id" class="input w-full" inputmode="numeric" :placeholder="t('admin.xianguanjia.pool.categoryIdPlaceholder')" />
            </div>
          </div>
          <div>
            <button class="btn btn-primary" :disabled="creatingKind" @click="createKind">
              {{ creatingKind ? t('admin.xianguanjia.pool.creatingKind') : t('admin.xianguanjia.pool.createKind') }}
            </button>
          </div>
          <div class="border-t border-gray-200 pt-4 dark:border-dark-700">
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianguanjia.pool.cardsLabel') }}</label>
            <textarea
              v-model="pushText"
              rows="8"
              class="input w-full font-mono text-sm"
              :placeholder="t('admin.xianguanjia.pool.cardsPlaceholder')"
            ></textarea>
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.xianguanjia.pool.cardsHint') }}</p>
          </div>
          <div class="flex items-center gap-2">
            <button class="btn btn-primary" :disabled="pushing || !currentKindId" @click="pushCards">
              {{ pushing ? t('admin.xianguanjia.pool.pushing') : t('admin.xianguanjia.pool.push') }}
            </button>
          </div>
          <div v-if="pushResult" class="rounded-md bg-gray-50 p-3 text-sm dark:bg-dark-800">
            <p>{{ t('admin.xianguanjia.pool.pushResult', { total: pushResult.total, succeeded: pushResult.succeeded, failed: pushResult.failed }) }}</p>
            <ul v-if="pushResult.failures?.length" class="mt-1 list-inside list-disc text-red-500">
              <li v-for="(f, i) in pushResult.failures" :key="i">{{ f }}</li>
            </ul>
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
import type { XianguanjiaConfig, XianguanjiaCardPair, XianguanjiaPushResult } from '@/api/admin/xianguanjia'
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

onMounted(() => {
  load()
  loadKind()
})

// D4d: 卡密推仓区块逻辑。
// 批量推仓输入格式：textarea 每行一张卡，形如 `card_no,card_pwd`（首个英文逗号分隔，空行忽略）。

const currentKindId = ref(0)
const creatingKind = ref(false)
const pushing = ref(false)
const kindForm = reactive({ name: '', category_id: '' })
const pushText = ref('')
const pushResult = ref<XianguanjiaPushResult | null>(null)

async function loadKind() {
  try {
    const res = await adminAPI.xianguanjia.getPoolKind()
    currentKindId.value = res.kind_id ?? 0
  } catch (err) {
    // 卡种服务未接线（503）等场景不阻断页面其余功能，仅提示。
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

async function createKind() {
  const name = kindForm.name.trim()
  if (!name) {
    appStore.showError(t('admin.xianguanjia.pool.kindNameRequired'))
    return
  }
  const catRaw = kindForm.category_id.trim()
  let categoryId = 0
  if (catRaw) {
    categoryId = Number(catRaw)
    if (!Number.isInteger(categoryId) || categoryId < 0) {
      appStore.showError(t('admin.xianguanjia.pool.categoryIdInvalid'))
      return
    }
  }
  creatingKind.value = true
  try {
    const res = await adminAPI.xianguanjia.createPoolKind({
      name,
      category_id: categoryId || undefined
    })
    currentKindId.value = res.kind_id
    kindForm.name = ''
    kindForm.category_id = ''
    appStore.showSuccess(t('admin.xianguanjia.pool.kindCreated', { id: res.kind_id }))
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  } finally {
    creatingKind.value = false
  }
}

function parseCards(text: string): XianguanjiaCardPair[] | null {
  const cards: XianguanjiaCardPair[] = []
  const lines = text.split('\n')
  for (const line of lines) {
    const trimmed = line.trim()
    if (!trimmed) continue
    const idx = trimmed.indexOf(',')
    if (idx <= 0) return null
    const cardNo = trimmed.slice(0, idx).trim()
    const cardPwd = trimmed.slice(idx + 1).trim()
    if (!cardNo) return null
    cards.push({ card_no: cardNo, card_pwd: cardPwd })
  }
  return cards
}

async function pushCards() {
  if (!currentKindId.value) {
    appStore.showError(t('admin.xianguanjia.pool.kindRequired'))
    return
  }
  const cards = parseCards(pushText.value)
  if (!cards || cards.length === 0) {
    appStore.showError(t('admin.xianguanjia.pool.cardsInvalid'))
    return
  }
  pushing.value = true
  pushResult.value = null
  try {
    const res = await adminAPI.xianguanjia.pushPoolCards(currentKindId.value, cards)
    pushResult.value = res
    if (res.failed === 0) {
      appStore.showSuccess(t('admin.xianguanjia.pool.pushSuccess', { count: res.succeeded }))
      pushText.value = ''
    } else {
      appStore.showError(t('admin.xianguanjia.pool.pushPartial', { failed: res.failed }))
    }
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  } finally {
    pushing.value = false
  }
}
</script>
