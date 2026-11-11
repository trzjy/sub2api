<template>
  <div>
    <label class="input-label">
      {{ t('admin.users.groups') }}
      <span class="font-normal text-gray-400">{{ t('common.selectedCount', { count: modelValue.length }) }}</span>
    </label>
    <div
      v-if="isSearchable"
      class="flex items-center gap-2 rounded-t-lg border border-b-0 border-gray-200 bg-gray-50 px-3 py-2 dark:border-dark-600 dark:bg-dark-800"
    >
      <Icon name="search" size="sm" class="shrink-0 text-gray-400" />
      <input
        v-model="searchText"
        type="text"
        :disabled="props.disabled"
        :placeholder="t('common.searchPlaceholder')"
        class="flex-1 bg-transparent text-sm text-gray-900 placeholder:text-gray-400 focus:outline-none disabled:cursor-not-allowed dark:text-gray-100 dark:placeholder:text-dark-400"
      />
    </div>
    <div
      :class="[
        'grid max-h-32 grid-cols-2 gap-1 overflow-y-auto p-2',
        isSearchable
          ? 'rounded-b-lg border border-t-0 border-gray-200 bg-gray-50 dark:border-dark-600 dark:bg-dark-800'
          : 'rounded-lg border border-gray-200 bg-gray-50 dark:border-dark-600 dark:bg-dark-800'
      ]"
    >
      <label
        v-for="group in filteredGroups"
        :key="group.id"
        class="flex cursor-pointer items-center gap-2 rounded px-2 py-1.5 transition-colors hover:bg-white dark:hover:bg-dark-700 disabled:cursor-not-allowed disabled:opacity-60"
        :title="group.rate_multiplier == null ? group.name : t('admin.groups.rateAndAccounts', { rate: group.rate_multiplier, count: group.account_count || 0 })"
      >
        <input
          type="checkbox"
          :value="group.id"
          :checked="modelValue.includes(group.id)"
          :disabled="props.disabled"
          @change="handleChange(group.id, ($event.target as HTMLInputElement).checked)"
          class="h-3.5 w-3.5 shrink-0 rounded border-gray-300 text-primary-500 focus:ring-primary-500 dark:border-dark-500"
        />
        <GroupBadge
          :name="group.name"
          :platform="group.platform"
          :subscription-type="group.subscription_type || undefined"
          :rate-multiplier="group.rate_multiplier == null ? undefined : group.rate_multiplier"
          class="min-w-0 flex-1"
        />
        <span class="shrink-0 text-xs text-gray-400">{{ group.account_count || 0 }}</span>
      </label>
      <div
        v-if="filteredGroups.length === 0"
        class="col-span-2 py-2 text-center text-sm text-gray-500 dark:text-gray-400"
      >
        {{ t('common.noGroupsAvailable') }}
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import GroupBadge from './GroupBadge.vue'
import Icon from '@/components/icons/Icon.vue'
import type { Group, GroupPlatform, ModelAllowlist } from '@/types'
import { useAuthStore } from '@/stores'

const { t } = useI18n()
const authStore = useAuthStore()

interface Props {
  modelValue: number[]
  groups: (Group & { account_count?: number })[]
  platform?: GroupPlatform // Optional platform filter
  mixedScheduling?: boolean // For antigravity accounts: allow anthropic/gemini groups
  // 额外放行的原生平台集合（可选）。仅多模型供给平台需要（当前 cursor：账号是供给端，
  // 模型按所属原生平台进组，U7）——传入后可选中这些平台的分组；不传时保持
  // 「同 platform + composite」的既有规则，不全局放开跨平台绑定。
  nativePlatforms?: GroupPlatform[]
  // 可选：账号当前支持的模型名单（如 ['claude-fable-5-1', 'gpt-6.1-sol']）。
  // 传入（非 undefined）时，composite 聚合组仅当启用了白名单且白名单覆盖其中至少一个模型才显示；
  // 不传时保持既有行为（composite 组全显示），不影响其它页面/平台。
  accountModels?: string[]
  searchable?: boolean | 'auto'
  disabled?: boolean // 只读态：checkbox 与变更事件禁用（账号编辑弹窗 Cursor 绑定激活码时分组只读）
}

const props = withDefaults(defineProps<Props>(), {
  nativePlatforms: () => [],
  searchable: 'auto',
  disabled: false
})
const emit = defineEmits<{
  'update:modelValue': [value: number[]]
}>()

const searchText = ref('')

// 判断 composite 聚合组是否覆盖账号模型名单中的至少一个模型。
// 规则（按 U5 裁定：模型名单中有哪些家族，就显示模型家族相关的分组）：
// - 未启用白名单或白名单模型为空 → false（未启用白名单的聚合组不等于匹配了模型家族）；
// - 账号模型名单为空 → false；
// - pattern 与 model 均做 trim().toLowerCase()：精确相等，或 pattern 以 '*' 结尾时 model 前缀命中，任一命中返回 true。
const compositeGroupMatchesAccountModels = (
  g: Group & { account_count?: number; model_allowlist?: ModelAllowlist },
  models: string[]
): boolean => {
  const allowlist = g.model_allowlist
  if (!allowlist?.enabled || !allowlist.models?.length) return false
  if (models.length === 0) return false
  const normalizedModels = models.map((m) => m.trim().toLowerCase())
  return allowlist.models.some((pattern: string) => {
    const p = pattern.trim().toLowerCase()
    return normalizedModels.some((model) =>
      p.endsWith('*') ? model.startsWith(p.slice(0, -1)) : model === p
    )
  })
}

const isSearchable = computed(() => {
  if (props.searchable === 'auto') return props.groups.length > 5
  return props.searchable
})

// Filter groups by platform if specified
const filteredGroups = computed(() => {
  let result = authStore.isSimpleMode
    ? props.groups.filter((g) => g.platform !== 'composite')
    : props.groups
  if (props.platform) {
    // antigravity 账户启用混合调度后，可选择 anthropic/gemini 分组
    if (props.platform === 'antigravity' && props.mixedScheduling) {
      result = result.filter(
        (g) => g.platform === 'antigravity' || g.platform === 'anthropic' || g.platform === 'gemini' || g.platform === 'composite'
      )
    } else {
      // 默认：只能选择同 platform 的分组；composite 分组可接收任意具体平台账号；
      // nativePlatforms 按调用方声明放行（多模型供给平台按模型所属原生平台选组，U7）。
      // 传入 accountModels 时，composite 聚合组仅当白名单覆盖账号模型名单中至少一个模型才显示（U5）；
      // 不传时保持既有行为（composite 组全显示），不影响其它页面/平台。
      result = result.filter((g) => {
        if (g.platform === 'composite') {
          if (props.accountModels !== undefined) {
            return compositeGroupMatchesAccountModels(g, props.accountModels)
          }
          return true
        }
        return g.platform === props.platform || props.nativePlatforms.includes(g.platform)
      })
    }
  }
  // 已绑定组恒可见：无论平台过滤结果如何，当前已选中且存在于 groups 的分组必须显示，
  // 否则用户看不到既有绑定、无法保留或显式取消（不能因列表过滤隐式丢组）。
  // simple mode 下 composite 的隐藏/清理是既有规则，保持不变（不补回）。
  const visible = new Set(result.map((g) => g.id))
  const boundButHidden = props.groups.filter(
    (g) =>
      props.modelValue.includes(g.id) &&
      !visible.has(g.id) &&
      !(authStore.isSimpleMode && g.platform === 'composite')
  )
  if (boundButHidden.length > 0) {
    result = [...result, ...boundButHidden]
  }
  if (isSearchable.value && searchText.value) {
    const q = searchText.value.toLowerCase()
    result = result.filter(
      (g) => g.name.toLowerCase().includes(q) || g.description?.toLowerCase().includes(q)
    )
  }
  return result
})

watch(
  () => [authStore.isSimpleMode, props.groups, props.modelValue] as const,
  () => {
    if (!authStore.isSimpleMode || props.groups.length === 0) return
    const visibleIDs = new Set(props.groups.filter((group) => group.platform !== 'composite').map((group) => group.id))
    const cleaned = props.modelValue.filter((id) => visibleIDs.has(id))
    if (cleaned.length !== props.modelValue.length) emit('update:modelValue', cleaned)
  },
  { immediate: true, deep: true }
)

const handleChange = (groupId: number, checked: boolean) => {
  if (props.disabled) return // 只读兜底：脚本/事件无法触发变更
  const newValue = checked
    ? [...props.modelValue, groupId]
    : props.modelValue.filter((id) => id !== groupId)
  emit('update:modelValue', newValue)
}
</script>
