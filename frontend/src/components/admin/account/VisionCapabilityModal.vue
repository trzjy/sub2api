<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.vision.visionTitle')"
    width="normal"
    @close="handleClose"
  >
    <div class="space-y-4">
      <!-- Account Info Card -->
      <div
        v-if="account"
        class="flex items-center justify-between rounded-xl border border-gray-200 bg-gradient-to-r from-gray-50 to-gray-100 p-3 dark:border-dark-500 dark:from-dark-700 dark:to-dark-600"
      >
        <div class="flex items-center gap-3">
          <div
            class="flex h-10 w-10 items-center justify-center rounded-lg bg-gradient-to-br from-primary-500 to-primary-600"
          >
            <Icon name="eye" size="md" class="text-white" :stroke-width="2" />
          </div>
          <div>
            <div class="font-semibold text-gray-900 dark:text-gray-100">{{ account.name }}</div>
            <div class="flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400">
              <span
                class="rounded bg-gray-200 px-1.5 py-0.5 text-[10px] font-medium uppercase dark:bg-dark-500"
              >
                {{ account.type }}
              </span>
              <span>{{ t('admin.accounts.account') }}</span>
            </div>
          </div>
        </div>
        <span
          :class="[
            'rounded-full px-2.5 py-1 text-xs font-semibold',
            account.status === 'active'
              ? 'bg-green-100 text-green-700 dark:bg-green-500/20 dark:text-green-400'
              : 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-400'
          ]"
        >
          {{ account.status }}
        </span>
      </div>

      <p class="text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.vision.hint') }}
      </p>

      <!-- Model + Protocol selection -->
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div class="space-y-1.5">
          <label class="text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.accounts.vision.model') }}
          </label>
          <input
            v-model="selectedModel"
            type="text"
            class="input text-sm"
            :placeholder="t('admin.accounts.vision.modelPlaceholder')"
            :disabled="detecting || overriding"
          />
        </div>
        <div class="space-y-1.5">
          <label class="text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.accounts.vision.protocol') }}
          </label>
          <Select
            v-model="selectedProtocol"
            :options="protocolOptions"
            :disabled="detecting || overriding"
          />
        </div>
      </div>

      <!-- Action -->
      <div class="flex items-center gap-3">
        <button
          type="button"
          class="btn btn-primary"
          :disabled="!canDetect || detecting || overriding"
          @click="runDetect"
        >
          <span v-if="detecting" class="mr-2 inline-block h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white"></span>
          {{ detecting ? t('admin.accounts.vision.detecting') : t('admin.accounts.vision.runDetect') }}
        </button>
      </div>

      <!-- Error -->
      <div
        v-if="errorMessage"
        class="rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-800 dark:bg-red-900/20 dark:text-red-300"
      >
        {{ errorMessage }}
      </div>

      <!-- Result -->
      <div
        v-if="result"
        class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-600"
      >
        <div class="flex items-center justify-between">
          <span class="text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.accounts.vision.resultTitle') }}
          </span>
          <span
            :class="statusBadgeClass"
            class="rounded-full px-2.5 py-1 text-xs font-semibold"
          >
            {{ statusLabel }}
          </span>
        </div>

        <div class="text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.vision.supportsVision') }}：
          <span class="font-medium text-gray-700 dark:text-gray-200">
            {{ result.supports_vision === null ? '—' : (result.supports_vision ? t('common.yes') : t('common.no')) }}
          </span>
        </div>

        <!-- manual_review 手动覆盖 -->
        <div
          v-if="result.status === 'manual_review'"
          class="space-y-2 rounded-lg bg-amber-50 p-3 dark:bg-amber-900/20"
        >
          <p class="text-xs text-amber-700 dark:text-amber-300">
            {{ t('admin.accounts.vision.manualReviewHint') }}
          </p>
          <div class="flex items-center gap-3">
            <button
              type="button"
              class="btn btn-sm btn-secondary"
              :disabled="overriding"
              @click="markSupported"
            >
              {{ overriding && pendingValue === true ? t('admin.accounts.vision.overriding') : t('admin.accounts.vision.markSupported') }}
            </button>
            <button
              type="button"
              class="btn btn-sm btn-secondary"
              :disabled="overriding"
              @click="markUnsupported"
            >
              {{ overriding && pendingValue === false ? t('admin.accounts.vision.overriding') : t('admin.accounts.vision.markUnsupported') }}
            </button>
          </div>
        </div>

        <!-- 覆盖成功提示 -->
        <div
          v-if="overrideMessage"
          class="text-xs text-green-600 dark:text-green-400"
        >
          {{ overrideMessage }}
        </div>
      </div>

      <div
        v-if="!result && !errorMessage && !detecting"
        class="text-xs text-gray-400"
      >
        {{ t('admin.accounts.vision.noResult') }}
      </div>
    </div>

    <template #footer>
      <div class="flex justify-end gap-3 pt-4">
        <button type="button" class="btn btn-secondary" @click="handleClose">
          {{ t('admin.accounts.vision.close') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import { Icon } from '@/components/icons'
import { adminAPI } from '@/api/admin'
import type { Account } from '@/types'
import type {
  VisionCapabilityDetectResult,
  VisionCapabilityStatus,
} from '@/api/admin/accounts'

const props = defineProps<{
  show: boolean
  account: Account | null
}>()

const emit = defineEmits<{
  (e: 'close'): void
}>()

const { t } = useI18n()

const protocolOptions = [
  { value: 'chat_completions', label: t('admin.accounts.vision.protocolChatCompletions') },
]

const selectedModel = ref('')
const selectedProtocol = ref<'chat_completions'>('chat_completions')
const detecting = ref(false)
const overriding = ref(false)
const pendingValue = ref<boolean | null>(null)
const errorMessage = ref('')
const result = ref<VisionCapabilityDetectResult | null>(null)
const overrideMessage = ref('')

const canDetect = computed(() => !!props.account && selectedModel.value.trim().length > 0)

const statusLabel = computed(() => {
  if (!result.value) return ''
  switch (result.value.status) {
    case 'supported':
      return t('admin.accounts.vision.resultSupported')
    case 'unsupported':
      return t('admin.accounts.vision.resultUnsupported')
    case 'detect_failed':
      return t('admin.accounts.vision.resultDetectFailed')
    case 'manual_review':
      return t('admin.accounts.vision.resultManualReview')
    default:
      return result.value.status
  }
})

const statusBadgeClass = computed(() => {
  if (!result.value) return ''
  switch (result.value.status as VisionCapabilityStatus) {
    case 'supported':
      return 'bg-green-100 text-green-700 dark:bg-green-500/20 dark:text-green-400'
    case 'unsupported':
      return 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-400'
    case 'detect_failed':
      return 'bg-red-100 text-red-700 dark:bg-red-500/20 dark:text-red-400'
    case 'manual_review':
      return 'bg-amber-100 text-amber-700 dark:bg-amber-500/20 dark:text-amber-400'
    default:
      return 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-400'
  }
})

const resetState = () => {
  selectedModel.value = ''
  selectedProtocol.value = 'chat_completions'
  detecting.value = false
  overriding.value = false
  pendingValue.value = null
  errorMessage.value = ''
  result.value = null
  overrideMessage.value = ''
}

watch(
  () => props.show,
  (visible) => {
    if (visible) resetState()
  },
)

const runDetect = async () => {
  if (!props.account || !canDetect.value) return
  detecting.value = true
  errorMessage.value = ''
  result.value = null
  overrideMessage.value = ''
  try {
    const data = await adminAPI.accounts.detectVisionCapability(
      props.account.id,
      selectedModel.value.trim(),
      selectedProtocol.value,
    )
    result.value = data
  } catch (err: unknown) {
    const msg =
      (err as { message?: string })?.message || t('admin.accounts.vision.detectError')
    errorMessage.value = msg
  } finally {
    detecting.value = false
  }
}

const applyOverride = async (supportsVision: boolean) => {
  if (!props.account || !result.value) return
  overriding.value = true
  pendingValue.value = supportsVision
  overrideMessage.value = ''
  try {
    const data = await adminAPI.accounts.setVisionCapabilityOverride(
      props.account.id,
      selectedModel.value.trim(),
      selectedProtocol.value,
      supportsVision,
    )
    result.value = data
    overrideMessage.value = t('admin.accounts.vision.overrideSuccess')
  } catch (err: unknown) {
    const msg =
      (err as { message?: string })?.message || t('admin.accounts.vision.detectError')
    errorMessage.value = msg
  } finally {
    overriding.value = false
    pendingValue.value = null
  }
}

const markSupported = () => applyOverride(true)
const markUnsupported = () => applyOverride(false)

const handleClose = () => emit('close')
</script>
