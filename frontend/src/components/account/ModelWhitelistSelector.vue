<template>
  <div>
    <!-- Multi-select Dropdown -->
    <div class="relative mb-3">
      <div
        @click="toggleDropdown"
        class="cursor-pointer rounded-lg border border-gray-300 bg-white px-3 py-2 dark:border-dark-500 dark:bg-dark-700"
      >
        <div class="grid grid-cols-2 gap-1.5">
          <span
            v-for="model in modelValue"
            :key="model"
            class="inline-flex items-center justify-between gap-1 rounded bg-gray-100 px-2 py-1 text-xs text-gray-700 dark:bg-dark-600 dark:text-gray-300"
          >
            <span class="flex items-center gap-1 truncate">
              <ModelIcon :model="model" size="14px" />
              <span class="truncate">{{ model }}</span>
            </span>
            <button
              type="button"
              @click.stop="removeModel(model)"
              class="shrink-0 rounded-full hover:bg-gray-200 dark:hover:bg-dark-500"
            >
              <Icon name="x" size="xs" class="h-3.5 w-3.5" :stroke-width="2" />
            </button>
          </span>
        </div>
        <div class="mt-2 flex items-center justify-between border-t border-gray-200 pt-2 dark:border-dark-600">
          <span class="text-xs text-gray-400">{{ t('admin.accounts.modelCount', { count: modelValue.length }) }}</span>
          <svg class="h-5 w-5 text-gray-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7" />
          </svg>
        </div>
      </div>
      <!-- Dropdown List -->
      <div
        v-if="showDropdown"
        class="absolute left-0 right-0 top-full z-50 mt-1 rounded-lg border border-gray-200 bg-white shadow-lg dark:border-dark-600 dark:bg-dark-700"
      >
        <div class="sticky top-0 border-b border-gray-200 bg-white p-2 dark:border-dark-600 dark:bg-dark-700">
          <input
            v-model="searchQuery"
            type="text"
            class="input w-full text-sm"
            :placeholder="t('admin.accounts.searchModels')"
            @click.stop
          />
        </div>
        <div class="max-h-52 overflow-auto">
          <div
            v-for="model in filteredModels"
            :key="model.value"
            data-testid="model-option"
            class="group flex items-center hover:bg-gray-100 dark:hover:bg-dark-600"
          >
            <button
              type="button"
              data-testid="select-model"
              class="flex min-w-0 flex-1 items-center gap-2 px-3 py-2 text-left text-sm"
              @click="toggleModel(model.value)"
            >
              <span
                :class="[
                  'flex h-4 w-4 shrink-0 items-center justify-center rounded border',
                  modelValue.includes(model.value)
                    ? 'border-primary-500 bg-primary-500 text-white'
                    : 'border-gray-300 dark:border-dark-500'
                ]"
              >
                <svg v-if="modelValue.includes(model.value)" class="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="3" d="M5 13l4 4L19 7" />
                </svg>
              </span>
              <ModelIcon :model="model.value" size="18px" />
              <span class="truncate text-gray-900 dark:text-white">{{ model.value }}</span>
            </button>
            <button
              type="button"
              data-testid="copy-model-id"
              class="mr-2 rounded p-1.5 text-gray-400 opacity-70 transition-colors hover:bg-gray-200 hover:text-primary-600 focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 group-hover:opacity-100 dark:text-gray-500 dark:hover:bg-dark-500 dark:hover:text-primary-400"
              :title="`${t('common.copy')} ${model.value}`"
              :aria-label="`${t('common.copy')} ${model.value}`"
              @click="copyModelId(model.value)"
            >
              <Icon name="copy" size="sm" />
            </button>
          </div>
          <div v-if="filteredModels.length === 0" class="px-3 py-4 text-center text-sm text-gray-500">
            {{ t('admin.accounts.noMatchingModels') }}
          </div>
        </div>
      </div>
    </div>

    <!-- Quick Actions -->
    <div class="mb-4 flex flex-wrap gap-2">
      <button
        type="button"
        @click="fillRelated"
        class="rounded-lg border border-blue-200 px-3 py-1.5 text-sm text-blue-600 hover:bg-blue-50 dark:border-blue-800 dark:text-blue-400 dark:hover:bg-blue-900/30"
      >
        {{ t('admin.accounts.fillRelatedModels') }}
      </button>
      <button
        v-if="canSyncUpstream"
        type="button"
        @click="syncUpstreamModels"
        :disabled="isSyncingUpstream"
        class="rounded-lg border border-emerald-200 px-3 py-1.5 text-sm text-emerald-600 hover:bg-emerald-50 disabled:cursor-not-allowed disabled:opacity-60 dark:border-emerald-800 dark:text-emerald-400 dark:hover:bg-emerald-900/30"
      >
        {{ isSyncingUpstream ? t('admin.accounts.syncUpstreamModelsLoading') : t('admin.accounts.syncUpstreamModels') }}
      </button>
      <button
        type="button"
        @click="clearAll"
        class="rounded-lg border border-red-200 px-3 py-1.5 text-sm text-red-600 hover:bg-red-50 dark:border-red-800 dark:text-red-400 dark:hover:bg-red-900/30"
      >
        {{ t('admin.accounts.clearAllModels') }}
      </button>
    </div>

    <!-- Custom Model Input -->
    <div class="mb-3">
      <label class="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.accounts.customModelName') }}</label>
      <div class="flex gap-2">
        <input
          v-model="customModel"
          type="text"
          class="input flex-1"
          :placeholder="t('admin.accounts.enterCustomModelName')"
          @keydown.enter.prevent="handleEnter"
          @compositionstart="isComposing = true"
          @compositionend="isComposing = false"
        />
        <button
          type="button"
          @click="addCustom"
          class="rounded-lg bg-primary-50 px-4 py-2 text-sm font-medium text-primary-600 hover:bg-primary-100 dark:bg-primary-900/30 dark:text-primary-400 dark:hover:bg-primary-900/50"
        >
          {{ t('admin.accounts.addModel') }}
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { accountsAPI } from '@/api/admin/accounts'
import type { SyncUpstreamPreviewParams } from '@/api/admin/accounts'
import { useClipboard } from '@/composables/useClipboard'
import ModelIcon from '@/components/common/ModelIcon.vue'
import Icon from '@/components/icons/Icon.vue'
import { allModels, getModelsByPlatform } from '@/composables/useModelWhitelist'

const { t } = useI18n()

const props = defineProps<{
  modelValue: string[]
  platform?: string
  platforms?: string[]
  accountId?: number
  baseUrl?: string
  syncCredentials?: {
    platform: string
    type: string
    base_url?: string
    api_key: string
  }
}>()

const emit = defineEmits<{
  'update:modelValue': [value: string[]]
  'upstream-synced': []
}>()

const appStore = useAppStore()
const { copyToClipboard } = useClipboard()

const showDropdown = ref(false)
const searchQuery = ref('')
const customModel = ref('')
const isComposing = ref(false)
const isSyncingUpstream = ref(false)
const normalizedPlatforms = computed(() => {
  const rawPlatforms =
    props.platforms && props.platforms.length > 0
      ? props.platforms
      : props.platform
        ? [props.platform]
        : []

  return Array.from(
    new Set(
      rawPlatforms
        .map(platform => platform?.trim())
        .filter((platform): platform is string => Boolean(platform))
    )
  )
})

const upstreamSyncPlatforms = new Set([
  'anthropic',
  'openai',
  'gemini',
  'antigravity',
  'grok',
  'kimi',
  'zhipu',
  'deepseek'
])
const canSyncUpstream = computed(() => {
  if (props.accountId) {
    if (normalizedPlatforms.value.length === 0) return true
    return normalizedPlatforms.value.some(platform => upstreamSyncPlatforms.has(platform.toLowerCase()))
  }
  if (props.syncCredentials) {
    return upstreamSyncPlatforms.has(props.syncCredentials.platform.toLowerCase())
  }
  return false
})

// 火山方舟 Agent/Coding 订阅号识别：platform=deepseek + base_url host 为
// ark.cn-beijing.volces.com 且 path 落在 /api/plan 或 /api/coding。判定与后端
// parseVolcanoPlanProfile 一致（不用纯字符串 Contains，避免相似子串主机误判）。
const isVolcanoPlanAccount = computed(() => {
  const raw = (props.baseUrl ?? props.syncCredentials?.base_url ?? '').trim()
  if (!raw) return false
  let parsed: URL
  try {
    parsed = new URL(raw)
  } catch {
    return false
  }
  // 后端 parseVolcanoPlanProfile 仅接受 https：明文 http 会把携带的 API Key 明文外发，
  // 前端对齐拒绝，避免把 http 误判为火山订阅号走专用流程。
  if (parsed.protocol !== 'https:') return false
  // 与后端 parseVolcanoPlanProfile 的 net/url 主机相等判定对齐：精确 equals，不做
  // 子串 Contains，避免 notark.cn-beijing.volces.com 等相似子串主机被误判为火山订阅号。
  if (parsed.hostname.toLowerCase() !== 'ark.cn-beijing.volces.com') return false
  const path = parsed.pathname.replace(/\/+$/, '')
  return path === '/api/plan' || path.startsWith('/api/plan/') ||
    path === '/api/coding' || path.startsWith('/api/coding/')
})

// 运行时同步模型的登记表：同步（火山订阅号专用 / 通用上游）出来的、静态模型库里
// 没有的具名模型，登记后并入可搜索下拉，避免用户误删后无法从下拉重新选到。
const runtimeSyncedModels = ref<string[]>([])

const registerSyncedModels = (models: string[]) => {
  const staticValues = new Set(allModels.map(m => m.value))
  const next = [...runtimeSyncedModels.value]
  for (const raw of models) {
    const id = raw.trim()
    if (!id || id.includes('*') || staticValues.has(id) || next.includes(id)) continue
    next.push(id)
  }
  runtimeSyncedModels.value = next
}

const availableOptions = computed(() => {
  let base: { value: string; label: string }[]
  if (normalizedPlatforms.value.length === 0) {
    base = [...allModels] // 拷贝，避免下方 push 污染全局 allModels
  } else {
    const allowedModels = new Set<string>()
    for (const platform of normalizedPlatforms.value) {
      for (const model of getModelsByPlatform(platform)) {
        allowedModels.add(model)
      }
    }
    base = allModels.filter(model => allowedModels.has(model.value))
  }

  if (runtimeSyncedModels.value.length === 0) return base
  const seen = new Set(base.map(m => m.value))
  for (const id of runtimeSyncedModels.value) {
    if (!seen.has(id)) {
      base.push({ value: id, label: id })
      seen.add(id)
    }
  }
  return base
})

const filteredModels = computed(() => {
  const query = searchQuery.value.toLowerCase().trim()
  if (!query) return availableOptions.value
  return availableOptions.value.filter(
    m => m.value.toLowerCase().includes(query) || m.label.toLowerCase().includes(query)
  )
})

const toggleDropdown = () => {
  showDropdown.value = !showDropdown.value
  if (!showDropdown.value) searchQuery.value = ''
}

const removeModel = (model: string) => {
  emit('update:modelValue', props.modelValue.filter(m => m !== model))
}

const toggleModel = (model: string) => {
  if (props.modelValue.includes(model)) {
    removeModel(model)
  } else {
    emit('update:modelValue', [...props.modelValue, model])
  }
}

const copyModelId = async (model: string) => {
  await copyToClipboard(model)
}

const addCustom = () => {
  const model = customModel.value.trim()
  if (!model) return
  if (props.modelValue.includes(model)) {
    appStore.showInfo(t('admin.accounts.modelExists'))
    return
  }
  emit('update:modelValue', [...props.modelValue, model])
  customModel.value = ''
}

const handleEnter = () => {
  if (!isComposing.value) addCustom()
}

const fillRelated = () => {
  const newModels = [...props.modelValue]
  for (const platform of normalizedPlatforms.value) {
    for (const model of getModelsByPlatform(platform)) {
      if (!newModels.includes(model)) {
        newModels.push(model)
      }
    }
  }
  emit('update:modelValue', newModels)
}

const syncUpstreamModels = async () => {
  if (isSyncingUpstream.value) return
  if (!props.accountId && !props.syncCredentials) return

  // 火山订阅号走专用流程：preview 拿分类与差异 → 确认 → apply 落库（非 append-only）。
  if (isVolcanoPlanAccount.value) {
    if (!props.accountId) {
      appStore.showError(t('admin.accounts.syncVolcanoPlanRequiresAccount'))
      return
    }
    await syncVolcanoPlan()
    return
  }

  isSyncingUpstream.value = true
  try {
    let result
    if (props.accountId) {
      result = await accountsAPI.syncUpstreamModels(props.accountId)
    } else if (props.syncCredentials) {
      result = await accountsAPI.syncUpstreamModelsPreview(props.syncCredentials as SyncUpstreamPreviewParams)
    } else {
      return
    }

    const upstreamModels = result.models.map(model => model.trim()).filter(Boolean)
    if (upstreamModels.length === 0) {
      appStore.showInfo(t('admin.accounts.syncUpstreamModelsEmpty'))
      return
    }

    if (!props.accountId) {
      emit('upstream-synced')
    }

    registerSyncedModels(upstreamModels)
    const newModels = [...props.modelValue]
    let addedCount = 0
    for (const model of upstreamModels) {
      if (!newModels.includes(model)) {
        newModels.push(model)
        addedCount += 1
      }
    }

    emit('update:modelValue', newModels)
    if (result.warnings?.some(warning => warning.code === 'upstream_model_metadata_incomplete')) {
      appStore.showWarning(t('admin.accounts.syncUpstreamModelsMetadataIncomplete'))
      return
    }
    if (result.warnings?.some(warning => warning.code === 'volcano_model_sync_partial')) {
      appStore.showWarning(t('admin.accounts.syncUpstreamModelsVolcanoPartial'))
      return
    }
    if (addedCount > 0) {
      appStore.showSuccess(t('admin.accounts.syncUpstreamModelsSuccess', { count: addedCount, total: upstreamModels.length }))
    } else {
      appStore.showInfo(t('admin.accounts.syncUpstreamModelsNoChanges', { count: upstreamModels.length }))
    }
  } catch (error) {
    const message = error instanceof Error ? error.message : t('admin.accounts.syncUpstreamModelsFailed')
    appStore.showError(t('admin.accounts.syncUpstreamModelsError', { message }))
  } finally {
    isSyncingUpstream.value = false
  }
}

// syncVolcanoPlan 执行火山订阅号专用同步：先 apply=false 预览分类与差异，用户确认后
// apply=true 落库。unavailable/unverified 只作提示不并入；只有当 full_confirm（完整确认）
// 才允许“替换下架”，否则只新增。
const syncVolcanoPlan = async () => {
  if (isSyncingUpstream.value) return
  if (!props.accountId) return

  isSyncingUpstream.value = true
  try {
    const preview = await accountsAPI.syncVolcanoPlanModels(props.accountId, { apply: false })
    // 后端全量探活：成功响应的 confirmed 必非空（全部失败已 fail-closed 返回错误），
    // 空分类数组也稳定输出 [] 而非 null。此处仅作防御性提前退出：无任何确认、无新增、
    // 且非完全确认时不弹确认框。
    if (preview.confirmed.length === 0 && preview.will_add.length === 0 && !preview.full_confirm) {
      appStore.showInfo(t('admin.accounts.syncUpstreamModelsEmpty'))
      return
    }

    const newModels = [...props.modelValue]
    for (const model of preview.will_add) {
      if (!newModels.includes(model)) {
        newModels.push(model)
      }
    }

    // 差异摘要：新增 /（完全确认时）下架替换 / 不可用与未确认提示。
    const lines: string[] = []
    if (preview.will_add.length > 0) {
      lines.push(t('admin.accounts.syncVolcanoPlanWillAdd', { count: preview.will_add.length }))
    }
    if (preview.will_remove.length > 0) {
      lines.push(t('admin.accounts.syncVolcanoPlanWillRemove', { count: preview.will_remove.length }))
    }
    if (preview.full_confirm) {
      lines.push(t('admin.accounts.syncVolcanoPlanFullConfirm'))
    } else {
      lines.push(t('admin.accounts.syncVolcanoPlanPartialConfirm'))
    }
    if (preview.unavailable.length > 0) {
      lines.push(t('admin.accounts.syncVolcanoPlanUnavailable', { count: preview.unavailable.length }))
    }
    if (preview.unverified.length > 0) {
      lines.push(t('admin.accounts.syncVolcanoPlanUnverified', { count: preview.unverified.length }))
    }

    const confirmed = window.confirm(lines.join('\n'))
    if (!confirmed) return

    // apply 下架受 preview 确认绑定：把 preview 展示并经用户确认可移除的集合一并传给后端。
    // 后端若发现重扫产生 preview 未提示的下架会 fail-closed 拒绝并返回 config 错误（不提交），
    // 杜绝未获确认的破坏性收敛（R3-1）。此处保留客户端二次防御：即便后端返回了 drifted 结果
    // （正常不会），也拒绝生效并提示重新预览（reject-and-reconfirm）。
    const applied = await accountsAPI.syncVolcanoPlanModels(props.accountId, {
      apply: true,
      expected_removals: preview.will_remove
    })
    const previewRemoves = new Set(preview.will_remove)
    const unexpectedRemove = applied.will_remove.filter(model => !previewRemoves.has(model))
    if (unexpectedRemove.length > 0) {
      appStore.showError(t('admin.accounts.syncVolcanoPlanDrifted', { models: unexpectedRemove.join(', ') }))
      return
    }
    // 应用后以后端为准：完全确认→保留既有白名单中未下架的（含人工 identity，后端
    // 绝不删除人工映射，前端不得反向丢掉）+ 新确认集合；部分确认→旧值 ∪ 新确认。
    registerSyncedModels(applied.confirmed)
    const merged = props.modelValue.filter(model => !applied.will_remove.includes(model))
    for (const model of applied.confirmed) {
      if (!merged.includes(model)) {
        merged.push(model)
      }
    }
    emit('update:modelValue', merged)
    appStore.showSuccess(t('admin.accounts.syncVolcanoPlanSuccess', { count: applied.confirmed.length }))
  } catch (error) {
    const message = error instanceof Error ? error.message : t('admin.accounts.syncUpstreamModelsFailed')
    appStore.showError(t('admin.accounts.syncUpstreamModelsError', { message }))
  } finally {
    isSyncingUpstream.value = false
  }
}

const clearAll = () => {
  emit('update:modelValue', [])
}

</script>
