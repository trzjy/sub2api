<template>
  <div class="mb-4 flex items-center justify-between rounded-lg bg-primary-50 p-3 dark:bg-primary-900/20">
    <div class="flex flex-wrap items-center gap-2">
      <span v-if="allResultsSelected" class="text-sm font-medium text-primary-900 dark:text-primary-100">
        {{ t('admin.accounts.bulkActions.selectedAll', { count: selectedIds.length }) }}
      </span>
      <span v-else-if="selectedIds.length > 0" class="text-sm font-medium text-primary-900 dark:text-primary-100">
        {{ t('admin.accounts.bulkActions.selected', { count: selectedIds.length }) }}
      </span>
      <span v-else class="text-sm font-medium text-primary-900 dark:text-primary-100">
        {{ t('admin.accounts.bulkEdit.title') }}
      </span>
      <template v-if="selectedIds.length > 0">
        <button
          @click="$emit('select-page')"
          class="text-xs font-medium text-primary-700 hover:text-primary-800 dark:text-primary-300 dark:hover:text-primary-200"
        >
          {{ t('admin.accounts.bulkActions.selectCurrentPage') }}
        </button>
      </template>
      <template v-if="!allResultsSelected && totalResults > selectedIds.length">
        <span v-if="selectedIds.length > 0" class="text-gray-300 dark:text-primary-800">•</span>
        <button
          :disabled="selectingAll"
          @click="$emit('select-all-results')"
          class="text-xs font-medium text-primary-700 hover:text-primary-800 disabled:cursor-not-allowed disabled:opacity-60 dark:text-primary-300 dark:hover:text-primary-200"
        >
          {{
            selectingAll
              ? t('admin.accounts.bulkActions.selectingAll')
              : t('admin.accounts.bulkActions.selectAllResults', { count: totalResults })
          }}
        </button>
      </template>
      <template v-if="selectedIds.length > 0">
        <span class="text-gray-300 dark:text-primary-800">•</span>
        <button
          @click="$emit('clear')"
          class="text-xs font-medium text-primary-700 hover:text-primary-800 dark:text-primary-300 dark:hover:text-primary-200"
        >
          {{ t('admin.accounts.bulkActions.clear') }}
        </button>
      </template>
    </div>
    <div class="flex gap-2">
      <template v-if="selectedIds.length > 0">
        <button @click="$emit('delete')" class="btn btn-danger btn-sm">{{ t('admin.accounts.bulkActions.delete') }}</button>
        <button @click="$emit('reset-status')" class="btn btn-secondary btn-sm">{{ t('admin.accounts.bulkActions.resetStatus') }}</button>
        <button @click="$emit('refresh-token')" class="btn btn-secondary btn-sm">{{ t('admin.accounts.bulkActions.refreshToken') }}</button>
        <button @click="$emit('probe-upstream-billing')" class="btn btn-secondary btn-sm">{{ t('admin.accounts.bulkActions.probeUpstreamBilling') }}</button>
        <button @click="$emit('toggle-schedulable', true)" class="btn btn-success btn-sm">{{ t('admin.accounts.bulkActions.enableScheduling') }}</button>
        <button @click="$emit('toggle-schedulable', false)" class="btn btn-warning btn-sm">{{ t('admin.accounts.bulkActions.disableScheduling') }}</button>
        <button @click="$emit('edit-selected')" class="btn btn-primary btn-sm">{{ t('admin.accounts.bulkActions.edit') }}</button>
        <template v-if="showWebActions">
          <span class="mx-1 h-5 w-px bg-gray-300 dark:bg-primary-800"></span>
          <button @click="$emit('web-login')" class="btn btn-secondary btn-sm">{{ t('admin.accounts.batch.login') }}</button>
          <button @click="$emit('web-test')" class="btn btn-secondary btn-sm">{{ t('admin.accounts.batch.test') }}</button>
          <button @click="$emit('delete-banned')" class="btn btn-danger btn-sm">{{ t('admin.accounts.batch.deleteBanned') }}</button>
          <button @click="$emit('web-enable')" class="btn btn-success btn-sm">{{ t('admin.accounts.batch.enable') }}</button>
          <button @click="$emit('web-disable')" class="btn btn-warning btn-sm">{{ t('admin.accounts.batch.disable') }}</button>
          <button @click="$emit('web-export')" class="btn btn-secondary btn-sm">{{ t('admin.accounts.batch.export') }}</button>
        </template>
      </template>
      <button @click="$emit('edit-filtered')" class="btn btn-primary btn-sm">
        {{ t('admin.accounts.bulkEdit.submit') }}
      </button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { isWebAccessAccount } from '@/components/account/credentialsBuilder'

const props = defineProps<{
  selectedIds: number[]
  selectedAccounts?: { platform?: string; credentials?: Record<string, unknown> | null }[]
  totalResults: number
  selectingAll: boolean
  allResultsSelected: boolean
}>()

// Web 专属按钮（登录/测试/删除封禁/启用/停用/导出）仅在选中项全部为 web access
// 账号时展示：平台归并后同一官方平台可共存普通 API 账号，仅凭 platform 放行会
// 让 API 账号误触 Web 逆向操作。
const showWebActions = computed(() => {
  const list = props.selectedAccounts
  return !!list && list.length > 0 && list.every(account => isWebAccessAccount(account))
})

defineEmits([
  'delete',
  'edit-selected',
  'edit-filtered',
  'clear',
  'select-page',
  'select-all-results',
  'toggle-schedulable',
  'reset-status',
  'refresh-token',
  'probe-upstream-billing',
  'web-login',
  'web-test',
  'delete-banned',
  'web-enable',
  'web-disable',
  'web-export'
])

const { t } = useI18n()
</script>
