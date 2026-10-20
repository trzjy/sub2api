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
        <!-- P1-B3：模型能力改自动来源展示——清单唯一来源是后端实供清单（available_models）
             的自动投影，不提供手工录入入口；管理员只能在自动来源清单内勾选/取消。 -->
        <p class="mb-2 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.quotaKeys.autoEntry.modelsSourceAuto') }}
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

      <!-- 入库默认值（P1-B3）：priority 固定 1 只读、并发只读（沿用列默认）；
           cursor 单独倍率编辑已收起——倍率不再在本弹窗暴露。 -->
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.autoEntry.priority') }}
          </label>
          <input
            :value="localDefaults.priority"
            type="number"
            class="input"
            readonly
            disabled
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
            readonly
            disabled
          />
        </div>
      </div>

      <p v-if="error" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
      <p v-if="successMsg" class="text-sm text-amber-600 dark:text-amber-400">{{ successMsg }}</p>
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
  { key: 'mistral', labelKey: 'admin.quotaKeys.autoEntry.vendorMistral' },
  { key: 'muse', labelKey: 'admin.quotaKeys.autoEntry.vendorMuse' },
  { key: 'composer', labelKey: 'admin.quotaKeys.autoEntry.vendorComposer' }
] as const

const loading = ref(false)
const saving = ref(false)
const error = ref('')
// successMsg 仅在"保存时静默清理了清单外失效勾选"时出现（R12-2）：提示本次丢弃了 N 个失效模型，
// 不阻断保存。其余正常保存流程不显示它。
const successMsg = ref('')
// staleCleanedCount 记录 loadConfig 时清理项总数（R13-1 扩展前仅含 R12-2 幽灵项剔除；
// 扩展后 = 幽灵项剔除数 + 厂商归属修正数）。loadConfig 只清 UI 态、不自动保存；该计数在管理员
// 点保存时通过 successMsg 提示，并清零。
const staleCleanedCount = ref(0)
// staleVendorFixedCount 仅记录 loadConfig 时就地修正的"自报 vendor ≠ 真实 vendor"归属项数
// （R13-1），用于保存提示条区分幽灵剔除与归属修正两类计数。
const staleVendorFixedCount = ref(0)
const localVendors = ref<Record<string, boolean>>({})
// localModels 是"已勾选"模型集合（保存契约的提交源）：勾选即入列、取消即从 localModels 移除。
// 渲染源不依赖它——模型多选区以 availableModels（可选全集）渲染，勾选态由 isModelSelected 判定，
// 因此取消勾选后该项仍渲染、复选框未选中，可再勾回（CUR-F2 裁定 1）。
const localModels = ref<AutoEntryModelRef[]>([])
// availableModels 是后端实供清单投影的"可选模型全集"（按厂商过滤展示的渲染源），只读。
const availableModels = ref<AutoEntryModelRef[]>([])
// rateMultiplier 自 P1-B3 起不再提供编辑入口（cursor 单独倍率编辑已收口）：
// 仍从后端读取并原样回写，仅作为存量值透传——不批改历史非 1 值（后端存量不动）。
const localDefaults = ref({ priority: 1, rateMultiplier: 1.0 })

const enabledVendors = computed(() => vendorList.filter((v) => localVendors.value[v.key]))

// 渲染源 = 该厂商下的可选全集（来自 availableModels）；不再用 localModels 既当渲染源又当勾选态。
function modelsForVendor(vendor: string): AutoEntryModelRef[] {
  return availableModels.value.filter((m) => m.vendor === vendor)
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
  successMsg.value = ''
  try {
    const cfg: AutoEntryConfig = await adminAPI.quotaKeys.getAutoEntryConfig()
    localVendors.value = { ...cfg.vendors }
    // R12-2：加载时把配置里"不在后端实供清单（available_models）内"的项从 localModels 剔除，
    // 使 UI 态即不含幽灵项（管理员无法取消的失效勾选）。注意：仅清 UI 态、不自动保存——只有管理员
    // 点保存，清理结果才落库（save() 也会再做一次同样过滤作兜底）。
    const availList = cfg.available_models ?? []
    // R13-1：实供清单的 id→vendor 映射，作为"真实归属"口径（与后端 CursorRealSupplyVendorByID
    // 同源）。自报 vendor 与真实 vendor 不符的已勾选项就地修正，避免 F8 校验把错误归属当
    // 配置错误拒绝、卡死无法自愈。
    const availVendorById = new Map(availList.map((m) => [m.id, m.vendor]))
    const availIds = new Set(availVendorById.keys())
    // R12-2 幽灵项剔除数：配置里不在实供清单内的项。
    const ghostCount = cfg.models.filter((m) => !availIds.has(m.id)).length
    // R13-1 归属修正数：在实供清单内、但自报 vendor 与真实 vendor 不符的项。
    let vendorFixedCount = 0
    localModels.value = cfg.models
      .filter((m) => availIds.has(m.id))
      .map((m) => {
        const realVendor = availVendorById.get(m.id)
        if (realVendor && m.vendor !== realVendor) {
          vendorFixedCount++
          // 就地修正为真实 vendor（数据源 = 后端 available_models 的 id→vendor 映射）。
          return { id: m.id, vendor: realVendor, probe_status: m.probe_status ?? '' }
        }
        return { id: m.id, vendor: m.vendor, probe_status: m.probe_status ?? '' }
      })
    // staleCleanedCount 语义扩展：幽灵项剔除数 + 归属修正数（R13-1）。
    staleCleanedCount.value = ghostCount + vendorFixedCount
    staleVendorFixedCount.value = vendorFixedCount
    availableModels.value = (cfg.available_models ?? []).map((m) => ({
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
  successMsg.value = ''
  try {
    // R12-2：保存时仅保留"已勾选厂商"下、且属于后端实供清单（available_models）的模型。
    // 不在实供清单内的失效勾选（如旧配置残留的官方模型名 gpt-4.1）静默丢弃（loadConfig 已将其从
    // UI 态剔除，此处过滤作兜底）——原样提交会让补池持续报配置错误；本轮清理数 > 0 时弹出提示条
    // （不阻断保存）。
    // R13-1：提交前再按实供清单 id→vendor 修正归属（兜底）：即便管理员在 loadConfig 后又手动
    // 勾选/改动过项，提交 payload 也带"真实 vendor"，避免后端 R8 归属校验拒绝（卡死自愈）。
    const availList = availableModels.value
    const availIds = new Set(availList.map((m) => m.id))
    const availVendorById = new Map(availList.map((m) => [m.id, m.vendor]))
    const models = localModels.value
      .map((m) => ({
        id: m.id,
        vendor: availVendorById.get(m.id) ?? m.vendor,
        probe_status: m.probe_status ?? ''
      }))
      .filter((m) => localVendors.value[m.vendor] && availIds.has(m.id))
    const ghost = staleCleanedCount.value - staleVendorFixedCount.value
    const vendor = staleVendorFixedCount.value
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
    // R15-2：仅在保存请求成功后显示清理/修正提示并清零计数——失败时保留计数，
    // 错误与"已清理"成功提示不同时出现。
    if (staleCleanedCount.value > 0) {
      if (ghost > 0 && vendor > 0) {
        successMsg.value = t('admin.quotaKeys.autoEntry.staleCleanedAndVendorsFixed', {
          ghost,
          vendor
        })
      } else if (vendor > 0) {
        successMsg.value = t('admin.quotaKeys.autoEntry.vendorAttributionFixed', { count: vendor })
      } else {
        successMsg.value = t('admin.quotaKeys.autoEntry.staleModelsCleaned', { count: ghost })
      }
      staleCleanedCount.value = 0
      staleVendorFixedCount.value = 0
    }
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
