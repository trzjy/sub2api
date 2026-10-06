<template>
  <div class="flex items-center gap-1.5">
    <span
      :class="[
        'inline-block h-2 w-2 rounded-full',
        variantClass
      ]"
    ></span>
    <span class="text-sm text-gray-700 dark:text-gray-300">
      {{ label }}
    </span>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{
  status: string
  label: string
}>()

const variantClass = computed(() => {
  switch (props.status) {
    case 'active':
    case 'success':
    case 'sent': // 闲鱼发货：已发送
      return 'bg-green-500'
    case 'pending': // 闲鱼发货：待处理
    case 'disabled':
    case 'inactive':
    case 'warning':
      return 'bg-yellow-500'
    case 'error':
    case 'danger':
    case 'failed': // 闲鱼发货：失败
      return 'bg-red-500'
    default:
      return 'bg-gray-400'
  }
})
</script>
