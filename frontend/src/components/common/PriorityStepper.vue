<template>
  <div
    class="group/priority inline-flex h-7 items-center rounded-lg border transition-colors duration-150"
    :class="[
      editing || dirty
        ? 'border-primary-300 bg-primary-50/70 dark:border-primary-700 dark:bg-primary-900/20'
        : 'border-transparent hover:border-gray-200 hover:bg-gray-50 dark:hover:border-dark-600 dark:hover:bg-dark-700/60'
    ]"
    :data-testid="`${testId}-cell`"
  >
    <button
      type="button"
      class="flex h-full w-6 items-center justify-center rounded-l-lg text-gray-500 opacity-0 transition hover:bg-gray-200/70 hover:text-primary-600 focus:opacity-100 focus:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 disabled:cursor-not-allowed disabled:text-gray-300 disabled:hover:bg-transparent disabled:hover:text-gray-300 dark:disabled:text-dark-600 dark:disabled:hover:text-dark-600 group-hover/priority:opacity-100 group-focus-within/priority:opacity-100 dark:text-gray-400 dark:hover:bg-dark-600 dark:hover:text-primary-400 [@media(hover:none)]:opacity-100"
      :class="{ '!opacity-100': editing || dirty }"
      :disabled="saving || draft <= min"
      :title="raiseTitle || t('admin.accounts.priorityQuick.raise')"
      :aria-label="raiseTitle || t('admin.accounts.priorityQuick.raise')"
      :data-testid="`${testId}-decrement`"
      @click="step(-1)"
    >
      <svg class="h-3 w-3" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round">
        <path d="M2.5 6h7" />
      </svg>
    </button>

    <input
      v-if="editing"
      ref="inputRef"
      v-model="inputValue"
      type="text"
      inputmode="numeric"
      class="h-full w-10 border-0 bg-transparent p-0 text-center font-mono text-sm tabular-nums text-gray-900 focus:outline-none focus:ring-0 dark:text-white"
      :aria-label="editHint || t('admin.accounts.columns.priority')"
      :data-testid="`${testId}-input`"
      @keydown.enter.prevent="commitInput"
      @keydown.esc.prevent="cancelInput"
      @keydown.up.prevent="nudgeInput(1)"
      @keydown.down.prevent="nudgeInput(-1)"
      @blur="commitInput"
    />
    <button
      v-else
      type="button"
      class="relative flex h-full min-w-[2rem] items-center justify-center px-1 font-mono text-sm tabular-nums transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-primary-500"
      :class="dirty ? 'font-semibold text-primary-600 dark:text-primary-400' : 'text-gray-700 hover:text-gray-900 dark:text-gray-300 dark:hover:text-white'"
      :disabled="saving"
      :title="editHint || t('admin.accounts.priorityQuick.editHint')"
      :data-testid="`${testId}-value`"
      @click="startEditing"
    >
      {{ draft }}
      <span
        v-if="saving"
        class="absolute -right-0.5 top-1 h-1.5 w-1.5 animate-pulse rounded-full bg-primary-500"
        :data-testid="`${testId}-saving`"
      />
    </button>

    <button
      type="button"
      class="flex h-full w-6 items-center justify-center rounded-r-lg text-gray-500 opacity-0 transition hover:bg-gray-200/70 hover:text-primary-600 focus:opacity-100 focus:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 disabled:cursor-not-allowed disabled:text-gray-300 disabled:hover:bg-transparent disabled:hover:text-gray-300 dark:disabled:text-dark-600 dark:disabled:hover:text-dark-600 group-hover/priority:opacity-100 group-focus-within/priority:opacity-100 dark:text-gray-400 dark:hover:bg-dark-600 dark:hover:text-primary-400 [@media(hover:none)]:opacity-100"
      :class="{ '!opacity-100': editing || dirty }"
      :disabled="saving || draft >= max"
      :title="lowerTitle || t('admin.accounts.priorityQuick.lower')"
      :aria-label="lowerTitle || t('admin.accounts.priorityQuick.lower')"
      :data-testid="`${testId}-increment`"
      @click="step(1)"
    >
      <svg class="h-3 w-3" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round">
        <path d="M2.5 6h7M6 2.5v7" />
      </svg>
    </button>
  </div>
</template>

<script setup lang="ts">
/**
 * 优先级步进单元格：号池（账号管理）与激活码管理共用的**唯一**优先级编辑控件。
 *
 * 交互与号池既有设计完全一致（用户裁定「复用现有的号池优先级设计」）：
 *   - 悬停出现 [−] / [+]，连点走 debounce 合并为一次保存；
 *   - 点数字可就地输入，Enter 提交、Esc 取消、上下方向键微调；
 *   - 保存失败回滚显示值并抛 error，由调用方决定提示文案；
 *   - 保存中显示脉冲小圆点，卸载前把未发出的改动落库。
 *
 * 数值口径由 props 决定（默认 1..9999，越小越优先），落库动作由调用方注入 `save`，
 * 本组件不感知具体资源类型，因此账号与激活码共用同一份实现，不会各自漂移。
 */
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

const props = withDefaults(
  defineProps<{
    /** 当前值（以调用方数据为准，保存成功后由调用方回写并驱动这里同步） */
    value: number
    /** 落库动作：抛异常即视为失败，本组件负责回滚显示值 */
    save: (next: number) => Promise<void>
    min?: number
    max?: number
    /** data-testid 前缀，便于既有用例按资源区分 */
    testId?: string
    raiseTitle?: string
    lowerTitle?: string
    editHint?: string
  }>(),
  { min: 1, max: 9999, testId: 'priority-stepper' }
)

const emit = defineEmits<{
  (e: 'error', error: unknown): void
}>()

// 连续点击时合并为一次保存，避免每次 +/- 都打一次接口
const SAVE_DEBOUNCE_MS = 450

const { t } = useI18n()

const draft = ref(props.value)
const saving = ref(false)
const editing = ref(false)
const inputValue = ref('')
const inputRef = ref<HTMLInputElement | null>(null)
let saveTimer: ReturnType<typeof setTimeout> | null = null

const dirty = computed(() => draft.value !== props.value)

watch(
  () => props.value,
  (next) => {
    if (!saving.value && !saveTimer && !editing.value) draft.value = next
  }
)

const clamp = (value: number) => Math.min(props.max, Math.max(props.min, Math.round(value)))

const clearTimer = () => {
  if (saveTimer) {
    clearTimeout(saveTimer)
    saveTimer = null
  }
}

const save = async () => {
  clearTimer()
  const target = draft.value
  if (target === props.value) return
  saving.value = true
  try {
    await props.save(target)
  } catch (error) {
    draft.value = props.value
    emit('error', error)
  } finally {
    saving.value = false
  }
}

const scheduleSave = () => {
  clearTimer()
  saveTimer = setTimeout(() => void save(), SAVE_DEBOUNCE_MS)
}

const step = (delta: number) => {
  if (saving.value) return
  const next = clamp(draft.value + delta)
  if (next === draft.value) return
  draft.value = next
  scheduleSave()
}

const startEditing = async () => {
  if (saving.value) return
  clearTimer()
  inputValue.value = String(draft.value)
  editing.value = true
  await nextTick()
  inputRef.value?.focus()
  inputRef.value?.select()
}

const nudgeInput = (delta: number) => {
  const current = Number.parseInt(inputValue.value, 10)
  inputValue.value = String(clamp((Number.isFinite(current) ? current : draft.value) + delta))
}

const commitInput = () => {
  if (!editing.value) return
  editing.value = false
  const parsed = Number.parseInt(inputValue.value.trim(), 10)
  if (Number.isFinite(parsed)) draft.value = clamp(parsed)
  void save()
}

const cancelInput = () => {
  editing.value = false
  if (!saving.value) draft.value = props.value
}

onBeforeUnmount(() => {
  // 卸载前把还没发出的改动落库，避免翻页/刷新时丢失
  if (saveTimer) void save()
})
</script>
