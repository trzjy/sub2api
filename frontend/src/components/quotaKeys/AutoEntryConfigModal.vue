<template>
  <BaseDialog
    :show="show"
    :title="t('admin.quotaKeys.autoEntry.title')"
    width="wide"
    @close="$emit('close')"
  >
    <div v-if="loading" class="py-10 text-center text-sm text-gray-500 dark:text-gray-400">
      {{ t('common.loading') }}
    </div>

    <div v-else class="space-y-5">
      <p class="text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.quotaKeys.autoEntry.description') }}
      </p>

      <!-- 厂商勾选 -->
      <div>
        <p class="mb-2 text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.quotaKeys.autoEntry.vendors') }}
        </p>
        <div class="flex flex-wrap gap-x-4 gap-y-2">
          <label
            v-for="v in vendorList"
            :key="v.key"
            class="inline-flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300"
          >
            <input type="checkbox" v-model="localVendors[v.key]" />
            {{ t(v.labelKey) }}
          </label>
        </div>
      </div>

      <!-- 模型多选（按勾选厂商过滤） -->
      <div>
        <p class="mb-2 text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.quotaKeys.autoEntry.models') }}
        </p>
        <div v-if="enabledVendors.length === 0" class="text-xs text-gray-400">
          {{ t('admin.quotaKeys.na') }}
        </div>
        <div v-for="v in enabledVendors" :key="v.key" class="mb-3">
          <p class="mb-1 text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">
            {{ t(v.labelKey) }}
          </p>
          <div class="flex flex-wrap gap-x-4 gap-y-1">
            <label
              v-for="m in modelsForVendor(v.key)"
              :key="m.id"
              class="inline-flex items-center gap-1.5 text-xs text-gray-700 dark:text-gray-300"
            >
              <input
                type="checkbox"
                :checked="isModelSelected(m.id)"
                @change="toggleModel(m, ($event as Event).target as HTMLInputElement)"
              />
              {{ m.id }}
            </label>
          </div>
        </div>
      </div>

      <!-- 入库默认值 -->
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.autoEntry.priority') }}
          </label>
          <input
            v-model.number="localDefaults.priority"
            type="number"
            min="1"
            step="1"
            class="input"
          />
        </div>
        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.autoEntry.rateMultiplier') }}
          </label>
          <input
            v-model.number="localDefaults.rateMultiplier"
            type="number"
            min="0.1"
            step="0.1"
            class="input"
          />
        </div>
        <div class="sm:col-span-2">
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.autoEntry.concurrency') }}
          </label>
          <input
            type="text"
            class="input"
            :value="t('admin.quotaKeys.autoEntry.concurrencyPlaceholder')"
            disabled
          />
        </div>
      </div>

      <p v-if="error" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
    </div>

    <template #footer>
      <div class="flex items-center justify-end gap-2">
        <button class="btn btn-secondary" @click="$emit('close')">{{ t('common.cancel') }}</button>
        <button class="btn btn-primary" :disabled="saving" @click="save">
          {{ t('admin.quotaKeys.autoEntry.save') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { AutoEntryConfig, AutoEntryModelRef } from '@/api/admin/quotaKeys'
import BaseDialog from '@/components/common/BaseDialog.vue'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'saved'): void }>()

const { t } = useI18n()

// 厂商枚举（与后端 cursorKnownVendors 对齐）。labelKey 指向 admin.quotaKeys.autoEntry.vendor*。
const vendorList = [
  { key: 'openai', labelKey: 'admin.quotaKeys.autoEntry.vendorOpenAI' },
  { key: 'anthropic', labelKey: 'admin.quotaKeys.autoEntry.vendorAnthropic' },
  { key: 'google', labelKey: 'admin.quotaKeys.autoEntry.vendorGoogle' },
  { key: 'xai', labelKey: 'admin.quotaKeys.autoEntry.vendorXAI' },
  { key: 'deepseek', labelKey: 'admin.quotaKeys.autoEntry.vendorDeepSeek' },
  { key: 'mistral', labelKey: 'admin.quotaKeys.autoEntry.vendorMistral' }
] as const

const loading = ref(false)
const saving = ref(false)
const error = ref('')
const localVendors = ref<Record<string, boolean>>({})
// localModels 是 GET config 返回的模型全集（默认含全部静态清单），按勾选厂商过滤展示；
// 每模型的勾选态即其在 localModels 中的存在性（取消勾选即从 localModels 移除）。
const localModels = ref<AutoEntryModelRef[]>([])
const localDefaults = ref({ priority: 1, rateMultiplier: 1.0 })

const enabledVendors = computed(() => vendorList.filter((v) => localVendors.value[v.key]))

function modelsForVendor(vendor: string): AutoEntryModelRef[] {
  return localModels.value.filter((m) => m.vendor === vendor)
}

function isModelSelected(id: string): boolean {
  return localModels.value.some((m) => m.id === id)
}

function toggleModel(m: AutoEntryModelRef, target: HTMLInputElement): void {
  if (target.checked) {
    if (!isModelSelected(m.id)) {
      localModels.value.push({ id: m.id, vendor: m.vendor, probe_status: m.probe_status ?? '' })
    }
  } else {
    localModels.value = localModels.value.filter((x) => x.id !== m.id)
  }
}

// 打开弹窗即拉取当前配置（settings 缺省时后端返回缺省配置）。
watch(
  () => props.show,
  (visible) => {
    if (!visible) return
    void loadConfig()
  }
)

async function loadConfig(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const cfg: AutoEntryConfig = await adminAPI.quotaKeys.getAutoEntryConfig()
    localVendors.value = { ...cfg.vendors }
    localModels.value = cfg.models.map((m) => ({
      id: m.id,
      vendor: m.vendor,
      probe_status: m.probe_status ?? ''
    }))
    localDefaults.value = {
      priority: cfg.entry_defaults?.priority ?? 1,
      rateMultiplier: cfg.entry_defaults?.rate_multiplier ?? 1.0
    }
  } catch (err: unknown) {
    error.value = extractMessage(err)
  } finally {
    loading.value = false
  }
}

async function save(): Promise<void> {
  saving.value = true
  error.value = ''
  try {
    // 保存时仅保留"已勾选厂商"下的模型（后端校验：禁用厂商的模型不允许落库）。
    const models = localModels.value.filter((m) => localVendors.value[m.vendor])
    const payload: AutoEntryConfig = {
      vendors: { ...localVendors.value },
      models,
      entry_defaults: {
        concurrency: null,
        priority: localDefaults.value.priority,
        rate_multiplier: localDefaults.value.rateMultiplier
      }
    }
    await adminAPI.quotaKeys.updateAutoEntryConfig(payload)
    emit('saved')
    emit('close')
  } catch (err: unknown) {
    error.value = extractMessage(err)
  } finally {
    saving.value = false
  }
}

function extractMessage(err: unknown): string {
  if (err && typeof err === 'object' && 'message' in err) {
    return String((err as { message: unknown }).message)
  }
  return String(err)
}
</script>
