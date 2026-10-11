<template>
  <div class="space-y-4">
    <!-- Quick Amount Buttons -->
    <div>
      <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">
        {{ t('payment.quickAmounts') }}
      </label>
      <div class="grid grid-cols-3 gap-2">
        <button
          v-for="amt in filteredAmounts"
          :key="amt"
          type="button"
          :class="[
            'rounded-lg border-2 px-4 py-3 text-center font-medium transition-colors',
            modelValue === amt
              ? 'border-primary-500 bg-primary-50 text-primary-700 dark:border-primary-400 dark:bg-primary-900/40 dark:text-primary-300'
              : 'border-gray-200 bg-white text-gray-700 hover:border-gray-300 dark:border-dark-600 dark:bg-dark-800 dark:text-gray-200 dark:hover:border-dark-500',
          ]"
          :data-testid="`quick-amount-${amt}`"
          @click="selectAmount(amt)"
        >
          {{ currencySymbol }}{{ amt }}
        </button>
      </div>
    </div>

    <!-- Custom Amount Input -->
    <div>
      <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">
        {{ t('payment.customAmount') }}
      </label>
      <div class="relative">
        <span class="absolute left-3 top-1/2 -translate-y-1/2 text-gray-400 dark:text-dark-500">
          {{ currencySymbol }}
        </span>
        <input
          type="text"
          inputmode="decimal"
          :value="customText"
          :placeholder="placeholderText"
          class="input w-full py-3 pl-8 pr-4"
          @input="handleInput"
        />
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { currencySymbol as getCurrencySymbol } from '@/utils/money'

// w51 conflict-resolution fusion tests (second wave, B10 — funds_or_state / semantic_conflict)
// 四方裁决（base / ours / upstream / result），依据 merge-plan §5 rule 1（我方重写了该块 → 我方语义
// 为准，上游同块内的正交修复显式吸收，禁止整段保留我方版本导致上游修复静默丢弃）+ 权威表第 7 条
// （funds 数值保留我方值，双方值在此登记）。
// 冲突取证：git merge-file -p --diff3（ours/base/upstream）得 4 个冲突块，分别落在按钮内容、
// import 段、props 声明、withDefaults 默认值；另有 3 处上游改动位于非冲突区，被机械合并直接带入
// （其中 2 处正当吸收见第 3、4 条，1 处必须回退见第 5 条）。
// 1) KEEP OURS — currency prop 语义取我方：声明 currency?: string，默认值 CNY，符号渲染走
//    @/utils/money 的 currencySymbol()（我方 SSOT），按钮文案为「符号 + 金额」，自定义金额输入
//    前缀同为该符号。双方值登记：我方默认 CNY（消费方 views/user/PaymentView.vue 传
//    :currency="selectedCurrency"，值域 CNY/USD/EUR...）；上游默认 undefined，且只被
//    currencyDigits() 的兜底分支与折扣第二行的金额格式化消费 —— 两者均属本轮暂缓的充值优惠阶梯，
//    无生产者时该 prop 在上游侧是死参数，故连名字带默认值一起按我方。
//    注：两个默认值在「符号渲染」这条现存路径上行为等价（我方 currencySymbol() 内部
//    normalizePaymentCurrency 对 undefined 也兜底回 CNY），所以该差异只能由声明层锚点守，
//    Fusion spec 的现状锁正是为此；不要拿行为用例当默认值裁决的证据。
// 2) 上游充值优惠阶梯整组（bonusTiers / bonusMode / multiplier 三个 props、五个渲染函数、
//    促销价签与第二行文案、i18n 文案键）—— 按用户 2026-10-11 裁定「未采纳」，已整组剔除：
//    frontend/src/utils/rechargeBonus.ts、components/admin/settings/RechargeBonusTierEditor.vue、
//    types/payment.ts 的 RechargeBonusTier 与 payment.rechargeBonus.* 文案键一并删除；AmountInput
//    侧从未落地该组 props，本文件无需改动代码。后续若再遇到同一上游组，请勿按 UPSTREAM 回补。
// 3) ABSORB UPSTREAM — 非法金额输入的复位：handleInput 的 invalid 分支由「直接 return」改为
//    「先把 DOM value 复位成已接受值再 return」。上游对同一函数的正交 bug 修复，与优惠阶梯无关、
//    零新依赖，机械合并把它放在非冲突区，整段吸收。缺该复位时，用户敲入非法字符后输入框显示脏值
//    而 customText/modelValue 仍是旧值，二者失同步。上游 AmountInput.spec.ts 的第一个 describe
//    （4 例 it.each + 2 例，共 6 例）正是该修复的覆盖，已在 Fusion spec 里等价复刻 —— 上游 spec
//    文件本身按第 2 条暂缓（且它与我方文件同名路径会撞，故本卡交付用 Fusion 后缀另立）。
// 4) ABSORB UPSTREAM — 快额按钮的 data-testid 钩子（值为 quick-amount- 加金额）：机械合并的
//    非冲突区，纯测试钩子、无运行时依赖，吸收后 B11 汇入上游 spec 时该处不再产生 diff。
// 5) KEEP OURS — 按钮 :class 绑定仍是我方（= base）形态：机械合并把上游的 relative/px-3/py-3
//    前缀类、grid 的 gap-x-4 gap-y-4 pt-2 间距类以及 quoteFor(amt).percent 三元红色边框分支一并
//    带了进来，而 quoteFor 属第 2 条暂缓符号 —— 留着就是模板引用未定义函数（render 期报错）。
//    该区域虽非文本冲突，仍显式回退到我方版本；收敛 (a)-(d) 时随第 2 条一起换回上游形态。
// 无损性核验：结果相对 ours 仅 +2 行（第 3、4 条各一处），相对 upstream 删除的行全部集中在
//    第 2 条暂缓组与第 5 条回退组，逐条编号可回溯，无一遗漏。
// 测试交付：__tests__/AmountInputFusion.spec.ts（新增文件）—— 我方 currency 行为 + 上游
//    handleInput 复位的融合断言，加 (a)-(d) 缺席哨兵与我方现状锁。

const props = withDefaults(defineProps<{
  amounts?: number[]
  modelValue: number | null
  min?: number
  max?: number
  currency?: string
}>(), {
  amounts: () => [10, 20, 50, 100, 200, 500, 1000, 2000, 5000],
  min: 0,
  max: 0,
  currency: 'CNY',
})

const emit = defineEmits<{
  'update:modelValue': [value: number | null]
}>()

const { t } = useI18n()

const currencySymbol = computed(() => getCurrencySymbol(props.currency))

const customText = ref('')

// 0 = no limit
const filteredAmounts = computed(() =>
  props.amounts.filter((a) => (props.min <= 0 || a >= props.min) && (props.max <= 0 || a <= props.max))
)

const placeholderText = computed(() => {
  if (props.min > 0 && props.max > 0) return `${props.min} - ${props.max}`
  if (props.min > 0) return `≥ ${props.min}`
  if (props.max > 0) return `≤ ${props.max}`
  return t('payment.enterAmount')
})

const AMOUNT_PATTERN = /^\d*(\.\d{0,2})?$/

function selectAmount(amt: number) {
  customText.value = String(amt)
  emit('update:modelValue', amt)
}

function handleInput(e: Event) {
  const input = e.target as HTMLInputElement
  const val = input.value
  if (!AMOUNT_PATTERN.test(val)) {
    input.value = customText.value
    return
  }
  customText.value = val
  if (val === '') {
    emit('update:modelValue', null)
    return
  }
  const num = parseFloat(val)
  if (!isNaN(num) && num > 0) {
    emit('update:modelValue', num)
  } else {
    emit('update:modelValue', null)
  }
}

watch(() => props.modelValue, (v) => {
  if (v !== null && String(v) !== customText.value) {
    customText.value = String(v)
  }
}, { immediate: true })
</script>
