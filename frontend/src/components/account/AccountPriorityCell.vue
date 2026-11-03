<template>
  <!-- 号池优先级单元格：交互与数值口径由通用 PriorityStepper 提供（与激活码管理共用同一实现），
       这里只负责把它接到 accounts.update 上，并保持既有对外事件契约不变。 -->
  <PriorityStepper
    test-id="account-priority"
    :value="account.priority"
    :save="savePriority"
    @error="handleError"
  />
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import PriorityStepper from '@/components/common/PriorityStepper.vue'
import { update as updateAccount } from '@/api/admin/accounts'
import { extractApiErrorMessage } from '@/utils/apiError'
import type { Account } from '@/types'

const props = defineProps<{ account: Account }>()
const emit = defineEmits<{
  (e: 'updated', account: Account): void
  (e: 'error', message: string): void
}>()

const { t } = useI18n()

async function savePriority(next: number) {
  const updated = await updateAccount(props.account.id, { priority: next })
  emit('updated', updated)
}

function handleError(error: unknown) {
  emit('error', extractApiErrorMessage(error, t('admin.accounts.priorityQuick.failed')))
}
</script>
