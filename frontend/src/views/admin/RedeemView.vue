<template>
  <AppLayout>
    <!-- Tab switcher: 福利批次 / 兑换码 -->
    <div class="mb-4 flex flex-wrap gap-2">
      <button
        type="button"
        class="btn"
        :class="activeTab === 'welfare' ? 'btn-primary' : 'btn-secondary'"
        @click="switchTab('welfare')"
      >
        {{ t('admin.redeem.tabs.welfare') }}
      </button>
      <button
        type="button"
        class="btn"
        :class="activeTab === 'codes' ? 'btn-primary' : 'btn-secondary'"
        @click="switchTab('codes')"
      >
        {{ t('admin.redeem.tabs.codes') }}
      </button>
    </div>

    <!-- ==================== 福利批次 Tab ==================== -->
    <TablePageLayout v-if="activeTab === 'welfare'">
      <template #filters>
        <div class="flex flex-wrap items-center gap-3">
          <div class="flex flex-1 flex-wrap items-center justify-end gap-2">
            <button
              @click="loadWelfareBatches"
              :disabled="welfareLoading"
              class="btn btn-secondary"
              :title="t('common.refresh')"
            >
              <Icon name="refresh" size="md" :class="welfareLoading ? 'animate-spin' : ''" />
            </button>
            <button data-test="create-welfare-batch-open" @click="openWelfareDialog" class="btn btn-primary">
              {{ t('admin.redeem.welfare.createBatch') }}
            </button>
          </div>
        </div>
      </template>

      <template #table>
        <DataTable :columns="welfareColumns" :data="welfareBatches" :loading="welfareLoading">
          <template #cell-name="{ value }">
            <span class="text-sm font-medium text-gray-900 dark:text-white">{{ value }}</span>
          </template>

          <template #cell-created_at="{ value }">
            <span class="text-sm text-gray-500 dark:text-dark-400">{{ formatDateTime(value) }}</span>
          </template>

          <template #cell-code_count="{ value }">
            <span class="text-sm text-gray-900 dark:text-white">{{ value }}</span>
          </template>

          <template #cell-usage="{ row }">
            <span class="text-sm text-gray-900 dark:text-white">
              {{ row.used_count }} / {{ row.remaining_count }}
            </span>
          </template>

          <template #cell-cleared_amount="{ value }">
            <span class="text-sm text-gray-900 dark:text-white">${{ Number(value || 0).toFixed(2) }}</span>
          </template>

          <template #cell-actions="{ row }">
            <div class="flex items-center gap-2">
              <button
                data-test="copy-unused-codes"
                @click="copyUnusedBatchCodes(row)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-primary-50 hover:text-primary-600 dark:hover:bg-primary-900/20 dark:hover:text-primary-400"
              >
                <Icon name="copy" size="sm" :stroke-width="2" />
                <span class="text-xs">{{ t('admin.redeem.welfare.copyUnused') }}</span>
              </button>
              <button
                data-test="export-batch-csv"
                @click="exportBatchCsv(row)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-primary-50 hover:text-primary-600 dark:hover:bg-primary-900/20 dark:hover:text-primary-400"
              >
                <Icon name="download" size="sm" :stroke-width="2" />
                <span class="text-xs">{{ t('admin.redeem.exportCsv') }}</span>
              </button>
              <button
                data-test="view-batch-codes"
                @click="openBatchCodesDialog(row)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-primary-50 hover:text-primary-600 dark:hover:bg-primary-900/20 dark:hover:text-primary-400"
              >
                <Icon name="eye" size="sm" :stroke-width="2" />
                <span class="text-xs">{{ t('admin.redeem.welfare.viewCodes') }}</span>
              </button>
            </div>
          </template>
        </DataTable>
      </template>

      <template #pagination>
        <Pagination
          v-if="welfarePagination.total > 0"
          :page="welfarePagination.page"
          :total="welfarePagination.total"
          :page-size="welfarePagination.page_size"
          @update:page="handleWelfarePageChange"
          @update:pageSize="handleWelfarePageSizeChange"
        />
      </template>
    </TablePageLayout>

    <!-- ==================== 兑换码 Tab ==================== -->
    <TablePageLayout v-else>
      <template #filters>
        <div class="flex flex-wrap items-center gap-3">
          <!-- Left: Search + Filters -->
          <div class="flex-1 sm:max-w-64">
            <input
              v-model="searchQuery"
              type="text"
              :placeholder="t('admin.redeem.searchCodes')"
              class="input"
              @input="handleSearch"
            />
          </div>
          <Select
            v-model="filters.type"
            :options="filterTypeOptions"
            class="w-36"
            @change="onTypeFilterChange"
          />
          <Select
            v-model="filters.status"
            :options="filterStatusOptions"
            class="w-36"
            @change="loadCodes"
          />
          <Select
            v-model="valueFilter"
            :options="valueFilterOptions"
            class="w-32"
            data-test="value-filter"
            @change="onValueFilterSelect"
          />
          <Select
            v-model="poolFilter"
            :options="poolOptions"
            class="w-40"
            @change="onPoolSelect"
          />
          <span
            v-if="poolFilter"
            class="flex items-center gap-1 rounded-full bg-primary-50 px-3 py-1.5 text-xs text-primary-700 dark:bg-primary-900/20 dark:text-primary-300"
          >
            {{ t('admin.redeem.poolFilter', { pool: poolDisplayName }) }}
            <button
              class="ml-0.5 font-semibold hover:text-primary-900"
              :title="t('common.cancel')"
              @click="clearPoolFilter"
            >
              ×
            </button>
          </span>

          <!-- Right: Action buttons -->
          <div class="flex flex-1 flex-wrap items-center justify-end gap-2">
            <button
              @click="loadCodes"
              :disabled="loading"
              class="btn btn-secondary"
              :title="t('common.refresh')"
            >
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button @click="handleExportCodes" class="btn btn-secondary">
              {{ t('admin.redeem.exportCsv') }}
            </button>
            <button
              data-test="batch-update-open"
              @click="openBatchUpdateDialog"
              :disabled="selectedCount === 0 || batchUpdating"
              class="btn btn-secondary"
            >
              <Icon name="edit" size="md" class="mr-2" />
              {{ t('admin.redeem.batchUpdate') }}
            </button>
            <button
              data-test="delete-selected-open"
              @click="openDeleteSelectedDialog"
              :disabled="selectedCount === 0 || deletingSelected"
              class="btn btn-danger"
            >
              {{ t('admin.redeem.deleteSelected') }}
            </button>
            <button @click="showGenerateDialog = true" class="btn btn-primary">
              {{ t('admin.redeem.generateCodes') }}
            </button>
          </div>
        </div>
      </template>

      <template #table>
        <DataTable
          :columns="columns"
          :data="codes"
          :loading="loading"
          :server-side-sort="true"
          default-sort-key="id"
          default-sort-order="desc"
          @sort="handleSort"
        >
          <template #header-select>
            <input
              data-test="select-all-codes"
              type="checkbox"
              class="h-4 w-4 cursor-pointer rounded border-gray-300 text-primary-600 focus:ring-primary-500"
              :checked="allVisibleSelected"
              @click.stop
              @change="toggleSelectAllVisible($event)"
            />
          </template>

          <template #cell-select="{ row }">
            <input
              data-test="select-code"
              type="checkbox"
              class="h-4 w-4 cursor-pointer rounded border-gray-300 text-primary-600 focus:ring-primary-500"
              :checked="selectedCodeIds.has(row.id)"
              @click.stop
              @change="toggleSelectRow(row.id, $event)"
            />
          </template>

          <template #cell-code="{ value }">
            <div class="flex items-center space-x-2">
              <code class="font-mono text-sm text-gray-900 dark:text-gray-100">{{ value }}</code>
              <button
                @click="copyToClipboard(value)"
                :class="[
                  'flex items-center transition-colors',
                  copiedCode === value
                    ? 'text-green-500'
                    : 'text-gray-400 hover:text-gray-600 dark:hover:text-gray-300'
                ]"
                :title="copiedCode === value ? t('admin.redeem.copied') : t('keys.copyToClipboard')"
              >
                <Icon v-if="copiedCode !== value" name="copy" size="sm" :stroke-width="2" />
                <svg v-else class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path
                    stroke-linecap="round"
                    stroke-linejoin="round"
                    stroke-width="2"
                    d="M5 13l4 4L19 7"
                  />
                </svg>
              </button>
            </div>
          </template>

          <template #cell-type="{ value }">
            <span
              :class="[
                'badge',
                value === 'balance'
                  ? 'badge-success'
                  : value === 'subscription'
                    ? 'badge-warning'
                    : 'badge-primary'
              ]"
            >
              {{ t('admin.redeem.types.' + value) }}
            </span>
          </template>

          <template #cell-value="{ value, row }">
            <span class="text-sm font-medium text-gray-900 dark:text-white">
              <template v-if="row.type === 'balance'">${{ value.toFixed(2) }}</template>
              <template v-else-if="row.type === 'subscription'">
                {{ row.validity_days || 30 }} {{ t('admin.redeem.days') }}
                <span v-if="row.group" class="ml-1 text-xs text-gray-500 dark:text-gray-400"
                  >({{ row.group.name }})</span
                >
              </template>
              <template v-else-if="row.type === 'welfare'">
                <span v-if="value > 0">${{ value.toFixed(2) }}</span>
                <span v-else>-</span>
              </template>
              <template v-else>{{ value }}</template>
            </span>
          </template>

          <template #cell-status="{ value }">
            <span
              :class="[
                'badge',
                value === 'unused'
                  ? 'badge-success'
                  : value === 'used'
                    ? 'badge-gray'
                    : 'badge-danger'
              ]"
            >
              {{ t('admin.redeem.status.' + value) }}
            </span>
          </template>

          <template #cell-used_by="{ value, row }">
            <span class="text-sm text-gray-500 dark:text-dark-400">
              {{ row.user?.email || (value ? t('admin.redeem.userPrefix', { id: value }) : '-') }}
            </span>
          </template>

          <template #cell-used_at="{ value }">
            <span class="text-sm text-gray-500 dark:text-dark-400">{{
              value ? formatDateTime(value) : '-'
            }}</span>
          </template>

          <template #cell-expires_at="{ value, row }">
            <span
              :class="[
                'text-sm',
                row.status === 'expired'
                  ? 'text-red-600 dark:text-red-400'
                  : 'text-gray-500 dark:text-dark-400'
              ]"
            >
              {{ value ? formatDateTime(value) : t('admin.redeem.neverExpires') }}
            </span>
          </template>

          <template #cell-actions="{ row }">
            <div class="flex items-center space-x-2">
              <button
                v-if="row.status === 'unused'"
                @click="handleDelete(row)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400"
              >
                <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path
                    stroke-linecap="round"
                    stroke-linejoin="round"
                    stroke-width="2"
                    d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16"
                  />
                </svg>
                <span class="text-xs">{{ t('common.delete') }}</span>
              </button>
              <button
                v-else-if="row.status === 'delivered'"
                data-test="expire-code-open"
                @click="handleExpire(row)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-amber-50 hover:text-amber-600 dark:hover:bg-amber-900/20 dark:hover:text-amber-400"
              >
                <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path
                    stroke-linecap="round"
                    stroke-linejoin="round"
                    stroke-width="2"
                    d="M18.364 18.364A9 9 0 005.636 5.636m12.728 12.728A9 9 0 015.636 5.636m12.728 12.728L5.636 5.636"
                  />
                </svg>
                <span class="text-xs">{{ t('admin.redeem.expireAction') }}</span>
              </button>
              <span v-else class="text-gray-400 dark:text-dark-500">-</span>
            </div>
          </template>
        </DataTable>
      </template>

      <template #pagination>
        <div
          v-if="selectedCount > 0"
          class="mb-4 flex flex-wrap items-center justify-between gap-3 rounded-lg bg-primary-50 p-3 dark:bg-primary-900/20"
        >
          <span class="text-sm font-medium text-primary-900 dark:text-primary-100">
            {{ t('admin.redeem.selectedCount', { count: selectedCount }) }}
          </span>
          <div class="flex flex-wrap items-center gap-2">
            <button
              type="button"
              class="text-xs font-medium text-primary-700 hover:text-primary-800 dark:text-primary-300 dark:hover:text-primary-200"
              @click="clearSelectedCodes"
            >
              {{ t('admin.redeem.clearSelection') }}
            </button>
            <button
              type="button"
              class="btn btn-primary btn-sm"
              @click="openBatchUpdateDialog"
            >
              {{ t('admin.redeem.batchUpdate') }}
            </button>
            <button
              type="button"
              data-test="delete-selected-banner"
              class="btn btn-danger btn-sm"
              @click="openDeleteSelectedDialog"
            >
              {{ t('admin.redeem.deleteSelected') }}
            </button>
          </div>
        </div>

        <Pagination
          v-if="pagination.total > 0"
          :page="pagination.page"
          :total="pagination.total"
          :page-size="pagination.page_size"
          @update:page="handlePageChange"
          @update:pageSize="handlePageSizeChange"
        />
      </template>
    </TablePageLayout>

    <!-- Delete Confirmation Dialog -->
    <ConfirmDialog
      :show="showDeleteDialog"
      :title="t('admin.redeem.deleteCode')"
      :message="t('admin.redeem.deleteCodeConfirm')"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      danger
      @confirm="confirmDelete"
      @cancel="showDeleteDialog = false"
    />

    <!-- Delete Selected Codes Dialog -->
    <ConfirmDialog
      :show="showDeleteSelectedDialog"
      :title="t('admin.redeem.deleteSelected')"
      :message="t('admin.redeem.deleteSelectedConfirm', { count: selectedCount })"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      danger
      @confirm="confirmDeleteSelected"
      @cancel="showDeleteSelectedDialog = false"
    />

    <!-- Expire (Void) Code Dialog -->
    <ConfirmDialog
      :show="showExpireDialog"
      :title="t('admin.redeem.expireCode')"
      :message="t('admin.redeem.expireCodeConfirm')"
      :confirm-text="t('admin.redeem.expireAction')"
      :cancel-text="t('common.cancel')"
      danger
      @confirm="confirmExpire"
      @cancel="showExpireDialog = false"
    />

    <!-- Generate Codes Dialog -->
    <Teleport to="body">
      <div v-if="showGenerateDialog" class="fixed inset-0 z-50 flex items-center justify-center">
        <div class="fixed inset-0 bg-black/50" @click="showGenerateDialog = false"></div>
        <div
          class="relative z-10 w-full max-w-md rounded-xl bg-white p-6 shadow-xl dark:bg-dark-800"
        >
          <h2 class="mb-4 text-lg font-semibold text-gray-900 dark:text-white">
            {{ t('admin.redeem.generateCodesTitle') }}
          </h2>
          <form @submit.prevent="handleGenerateCodes" class="space-y-4">
            <div>
              <label class="input-label">{{ t('admin.redeem.codeType') }}</label>
              <Select v-model="generateForm.type" :options="typeOptions" />
            </div>
            <!-- 余额/并发类型：显示数值输入 -->
            <div
              v-if="generateForm.type !== 'subscription' && generateForm.type !== 'invitation'"
            >
              <label class="input-label">
                {{
                  generateForm.type === 'balance'
                    ? t('admin.redeem.amount')
                    : t('admin.redeem.columns.value')
                }}
              </label>
              <input
                v-model.number="generateForm.value"
                type="number"
                :step="generateForm.type === 'balance' ? '0.01' : '1'"
                :min="generateForm.type === 'balance' ? '0.01' : '1'"
                required
                class="input"
              />
            </div>
            <!-- 邀请码类型：显示提示信息 -->
            <div v-if="generateForm.type === 'invitation'" class="rounded-lg bg-blue-50 p-3 dark:bg-blue-900/20">
              <p class="text-sm text-blue-700 dark:text-blue-300">
                {{ t('admin.redeem.invitationHint') }}
              </p>
            </div>
            <!-- 订阅类型：显示分组选择和有效天数 -->
            <template v-if="generateForm.type === 'subscription'">
              <div>
                <label class="input-label">{{ t('admin.redeem.selectGroup') }}</label>
                <Select
                  v-model="generateForm.group_id"
                  :options="subscriptionGroupOptions"
                  :placeholder="t('admin.redeem.selectGroupPlaceholder')"
                >
                  <template #selected="{ option }">
                    <GroupBadge
                      v-if="option"
                      :name="(option as unknown as GroupOption).label"
                      :platform="(option as unknown as GroupOption).platform"
                      :subscription-type="(option as unknown as GroupOption).subscriptionType"
                      :rate-multiplier="(option as unknown as GroupOption).rate"
                    />
                    <span v-else class="text-gray-400">{{
                      t('admin.redeem.selectGroupPlaceholder')
                    }}</span>
                  </template>
                  <template #option="{ option, selected }">
                    <GroupOptionItem
                      :name="(option as unknown as GroupOption).label"
                      :platform="(option as unknown as GroupOption).platform"
                      :subscription-type="(option as unknown as GroupOption).subscriptionType"
                      :rate-multiplier="(option as unknown as GroupOption).rate"
                      :description="(option as unknown as GroupOption).description"
                      :selected="selected"
                    />
                  </template>
                </Select>
              </div>
              <div>
                <label class="input-label">{{ t('admin.redeem.validityDays') }}</label>
                <input
                  v-model.number="generateForm.validity_days"
                  type="number"
                  min="1"
                  max="365"
                  required
                  class="input"
                />
              </div>
            </template>
            <div>
              <label class="input-label">{{ t('admin.redeem.codeExpiry') }}</label>
              <div class="grid grid-cols-2 gap-2 sm:grid-cols-5">
                <button
                  v-for="option in redeemCodeExpiryOptions"
                  :key="option.value"
                  type="button"
                  @click="generateForm.expiry_option = option.value"
                  :class="[
                    'rounded-lg border px-3 py-2 text-sm transition-colors',
                    generateForm.expiry_option === option.value
                      ? 'border-primary-500 bg-primary-50 text-primary-700 dark:border-primary-400 dark:bg-primary-900/20 dark:text-primary-300'
                      : 'border-gray-200 text-gray-700 hover:bg-gray-50 dark:border-dark-600 dark:text-gray-300 dark:hover:bg-dark-700'
                  ]"
                >
                  {{ option.label }}
                </button>
              </div>
              <input
                v-if="generateForm.expiry_option === 'custom'"
                v-model.number="generateForm.custom_expiry_days"
                type="number"
                min="1"
                max="3650"
                required
                class="input mt-2"
                :placeholder="t('admin.redeem.customExpiryDays')"
              />
            </div>
            <div>
              <label class="input-label">{{ t('admin.redeem.count') }}</label>
              <input
                v-model.number="generateForm.count"
                type="number"
                min="1"
                max="100"
                required
                class="input"
              />
            </div>
            <div class="flex justify-end gap-3 pt-2">
              <button type="button" @click="showGenerateDialog = false" class="btn btn-secondary">
                {{ t('common.cancel') }}
              </button>
              <button type="submit" :disabled="generating" class="btn btn-primary">
                {{ generating ? t('admin.redeem.generating') : t('admin.redeem.generate') }}
              </button>
            </div>
          </form>
        </div>
      </div>
    </Teleport>

    <!-- Batch Update Dialog -->
    <Teleport to="body">
      <div
        v-if="showBatchUpdateDialog"
        class="fixed inset-0 z-50 flex items-center justify-center p-4"
      >
        <div class="fixed inset-0 bg-black/50" @click="closeBatchUpdateDialog"></div>
        <div
          class="relative z-10 w-full max-w-lg rounded-xl bg-white p-6 shadow-xl dark:bg-dark-800"
        >
          <h2 class="mb-1 text-lg font-semibold text-gray-900 dark:text-white">
            {{ t('admin.redeem.batchUpdateTitle') }}
          </h2>
          <p class="mb-4 text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.redeem.selectedCount', { count: selectedCount }) }}
          </p>

          <form data-test="batch-update-form" class="space-y-4" @submit.prevent="handleBatchUpdate">
            <div class="space-y-2">
              <label class="flex items-center gap-2 text-sm font-medium text-gray-700 dark:text-gray-300">
                <input
                  data-test="batch-field-status"
                  v-model="batchUpdateForm.update_status"
                  type="checkbox"
                  class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
                />
                {{ t('admin.redeem.batchFields.status') }}
              </label>
              <Select
                v-if="batchUpdateForm.update_status"
                v-model="batchUpdateForm.status"
                data-test="batch-status-select"
                :options="batchStatusOptions"
              />
            </div>

            <div class="space-y-2">
              <label class="flex items-center gap-2 text-sm font-medium text-gray-700 dark:text-gray-300">
                <input
                  v-model="batchUpdateForm.update_expires_at"
                  type="checkbox"
                  class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
                />
                {{ t('admin.redeem.batchFields.expiresAt') }}
              </label>
              <template v-if="batchUpdateForm.update_expires_at">
                <Select v-model="batchUpdateForm.expires_mode" :options="batchExpiryModeOptions" />
                <input
                  v-if="batchUpdateForm.expires_mode === 'custom'"
                  v-model="batchUpdateForm.expires_at_local"
                  type="datetime-local"
                  class="input"
                />
                <p v-if="batchUpdateForm.expires_mode === 'custom'" class="input-hint">
                  {{ t('admin.redeem.localTimeZoneHint', { timezone: browserTimeZone }) }}
                </p>
              </template>
            </div>

            <div class="space-y-2">
              <label class="flex items-center gap-2 text-sm font-medium text-gray-700 dark:text-gray-300">
                <input
                  data-test="batch-field-notes"
                  v-model="batchUpdateForm.update_notes"
                  type="checkbox"
                  class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
                />
                {{ t('admin.redeem.batchFields.notes') }}
              </label>
              <textarea
                v-if="batchUpdateForm.update_notes"
                data-test="batch-notes-input"
                v-model="batchUpdateForm.notes"
                rows="3"
                class="input"
                :placeholder="t('admin.redeem.batchNotesPlaceholder')"
              ></textarea>
            </div>

            <div class="space-y-2">
              <label class="flex items-center gap-2 text-sm font-medium text-gray-700 dark:text-gray-300">
                <input
                  v-model="batchUpdateForm.update_group_id"
                  type="checkbox"
                  class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
                />
                {{ t('admin.redeem.batchFields.group') }}
              </label>
              <Select
                v-if="batchUpdateForm.update_group_id"
                v-model="batchUpdateForm.group_id"
                :options="batchGroupOptions"
                :placeholder="t('admin.redeem.selectGroupPlaceholder')"
              />
            </div>

            <div class="flex justify-end gap-3 pt-2">
              <button type="button" @click="closeBatchUpdateDialog" class="btn btn-secondary">
                {{ t('common.cancel') }}
              </button>
              <button
                data-test="batch-update-submit"
                type="submit"
                :disabled="batchUpdating"
                class="btn btn-primary"
              >
                {{ batchUpdating ? t('common.submitting') : t('admin.redeem.batchUpdate') }}
              </button>
            </div>
          </form>
        </div>
      </div>
    </Teleport>

    <!-- Generated Codes Result Dialog -->
    <Teleport to="body">
      <div v-if="showResultDialog" class="fixed inset-0 z-50 flex items-center justify-center p-4">
        <div class="fixed inset-0 bg-black/50" @click="closeResultDialog"></div>
        <div class="relative z-10 w-full max-w-lg rounded-xl bg-white shadow-xl dark:bg-dark-800">
          <!-- Header -->
          <div
            class="flex items-center justify-between border-b border-gray-200 px-5 py-4 dark:border-dark-600"
          >
            <div class="flex items-center gap-3">
              <div
                class="flex h-10 w-10 items-center justify-center rounded-full bg-green-100 dark:bg-green-900/30"
              >
                <svg
                  class="h-5 w-5 text-green-600 dark:text-green-400"
                  fill="none"
                  stroke="currentColor"
                  viewBox="0 0 24 24"
                >
                  <path
                    stroke-linecap="round"
                    stroke-linejoin="round"
                    stroke-width="2"
                    d="M5 13l4 4L19 7"
                  />
                </svg>
              </div>
              <div>
                <h2 class="text-base font-semibold text-gray-900 dark:text-white">
                  {{ t('admin.redeem.generatedSuccessfully') }}
                </h2>
                <p class="text-sm text-gray-500 dark:text-gray-400">
                  {{ t('admin.redeem.codesCreated', { count: generatedCodes.length }) }}
                </p>
              </div>
            </div>
            <button
              @click="closeResultDialog"
              class="rounded-lg p-1.5 text-gray-400 transition-colors hover:bg-gray-100 hover:text-gray-600 dark:hover:bg-dark-700 dark:hover:text-gray-300"
            >
              <Icon name="x" size="md" :stroke-width="2" />
            </button>
          </div>
          <!-- Content -->
          <div class="p-5">
            <div class="relative">
              <textarea
                readonly
                :value="generatedCodesText"
                :style="{ height: textareaHeight }"
                class="w-full resize-none rounded-lg border border-gray-200 bg-gray-50 p-3 font-mono text-sm text-gray-800 focus:outline-none dark:border-dark-600 dark:bg-dark-700 dark:text-gray-200"
              ></textarea>
            </div>
          </div>
          <!-- Footer -->
          <div
            class="flex justify-end gap-2 rounded-b-xl border-t border-gray-200 bg-gray-50 px-5 py-4 dark:border-dark-600 dark:bg-dark-700/50"
          >
            <button
              @click="copyGeneratedCodes"
              :class="[
                'btn flex items-center gap-2 transition-all',
                copiedAll ? 'btn-success' : 'btn-secondary'
              ]"
            >
              <Icon v-if="!copiedAll" name="copy" size="sm" :stroke-width="2" />
              <svg v-else class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path
                  stroke-linecap="round"
                  stroke-linejoin="round"
                  stroke-width="2"
                  d="M5 13l4 4L19 7"
                />
              </svg>
              {{ copiedAll ? t('admin.redeem.copied') : t('admin.redeem.copyAll') }}
            </button>
            <button @click="downloadGeneratedCodes" class="btn btn-primary flex items-center gap-2">
              <Icon name="download" size="sm" :stroke-width="2" />
              {{ t('admin.redeem.download') }}
            </button>
          </div>
        </div>
      </div>
    </Teleport>
    <!-- Create Welfare Batch Dialog -->
    <Teleport to="body">
      <div v-if="showWelfareDialog" class="fixed inset-0 z-50 flex items-center justify-center p-4">
        <div class="fixed inset-0 bg-black/50" @click="closeWelfareDialog"></div>
        <div
          class="relative z-10 w-full max-w-lg rounded-xl bg-white p-6 shadow-xl dark:bg-dark-800"
        >
          <h2 class="mb-4 text-lg font-semibold text-gray-900 dark:text-white">
            {{ t('admin.redeem.welfare.createTitle') }}
          </h2>
          <form data-test="create-welfare-batch-form" class="space-y-4" @submit.prevent="handleCreateWelfareBatch">
            <div>
              <label class="input-label">{{ t('admin.redeem.welfare.batchName') }}</label>
              <input v-model.trim="welfareForm.name" type="text" required class="input" />
            </div>
            <div>
              <label class="input-label">{{ t('admin.redeem.welfare.selectGroups') }}</label>
              <div
                class="max-h-56 space-y-2 overflow-y-auto rounded-lg border border-gray-200 p-3 dark:border-dark-600"
              >
                <p
                  v-if="subscriptionGroupOptions.length === 0"
                  class="text-sm text-gray-400 dark:text-gray-500"
                >
                  {{ t('admin.redeem.welfare.noGroups') }}
                </p>
                <div
                  v-for="option in subscriptionGroupOptions"
                  :key="option.value"
                  class="flex items-center justify-between gap-3"
                >
                  <label class="flex min-w-0 flex-1 cursor-pointer items-center gap-2">
                    <input
                      type="checkbox"
                      class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
                      :checked="welfareForm.groups[option.value] !== undefined"
                      @change="toggleWelfareGroup(option.value, $event)"
                    />
                    <GroupBadge
                      :name="option.label"
                      :platform="option.platform"
                      :subscription-type="option.subscriptionType"
                      :rate-multiplier="option.rate"
                    />
                  </label>
                  <Select
                    v-if="welfareForm.groups[option.value] !== undefined"
                    v-model="welfareForm.groups[option.value]"
                    :options="welfareValidityOptions"
                    class="w-36"
                  />
                </div>
              </div>
            </div>
            <div>
              <label class="input-label">{{ t('admin.redeem.welfare.amountRange') }}</label>
              <div class="grid grid-cols-2 gap-2">
                <input
                  v-model="welfareForm.amountMin"
                  type="number"
                  step="0.01"
                  min="0"
                  class="input"
                  :placeholder="t('admin.redeem.welfare.amountMinPlaceholder')"
                />
                <input
                  v-model="welfareForm.amountMax"
                  type="number"
                  step="0.01"
                  min="0"
                  class="input"
                  :placeholder="t('admin.redeem.welfare.amountMaxPlaceholder')"
                />
              </div>
              <p class="input-hint">{{ t('admin.redeem.welfare.amountRangeHint') }}</p>
            </div>
            <div>
              <label class="input-label">{{ t('admin.redeem.count') }}</label>
              <input
                v-model.number="welfareForm.count"
                type="number"
                min="1"
                max="1000"
                required
                class="input"
              />
            </div>
            <div class="flex justify-end gap-3 pt-2">
              <button type="button" @click="closeWelfareDialog" class="btn btn-secondary">
                {{ t('common.cancel') }}
              </button>
              <button
                data-test="create-welfare-batch-submit"
                type="submit"
                :disabled="welfareSubmitting"
                class="btn btn-primary"
              >
                {{ welfareSubmitting ? t('common.submitting') : t('common.create') }}
              </button>
            </div>
          </form>
        </div>
      </div>
    </Teleport>

    <!-- Welfare Batch Codes Dialog -->
    <Teleport to="body">
      <div v-if="showBatchCodesDialog" class="fixed inset-0 z-50 flex items-center justify-center p-4">
        <div class="fixed inset-0 bg-black/50" @click="closeBatchCodesDialog"></div>
        <div class="relative z-10 w-full max-w-lg rounded-xl bg-white shadow-xl dark:bg-dark-800">
          <div
            class="flex items-center justify-between border-b border-gray-200 px-5 py-4 dark:border-dark-600"
          >
            <div>
              <h2 class="text-base font-semibold text-gray-900 dark:text-white">
                {{ t('admin.redeem.welfare.codesTitle', { name: viewingBatch?.name ?? '' }) }}
              </h2>
              <p class="text-sm text-gray-500 dark:text-gray-400">
                {{ t('admin.redeem.welfare.codesCount', { count: batchCodes.length }) }}
              </p>
            </div>
            <button
              @click="closeBatchCodesDialog"
              class="rounded-lg p-1.5 text-gray-400 transition-colors hover:bg-gray-100 hover:text-gray-600 dark:hover:bg-dark-700 dark:hover:text-gray-300"
            >
              <Icon name="x" size="md" :stroke-width="2" />
            </button>
          </div>
          <div class="max-h-96 overflow-y-auto p-5">
            <div v-if="batchCodesLoading" class="flex justify-center py-8">
              <Icon name="refresh" size="lg" class="animate-spin text-gray-400" />
            </div>
            <p
              v-else-if="batchCodes.length === 0"
              class="py-8 text-center text-sm text-gray-400 dark:text-gray-500"
            >
              {{ t('admin.redeem.welfare.noCodes') }}
            </p>
            <div v-else class="space-y-2">
              <div
                v-for="batchCode in batchCodes"
                :key="batchCode.id"
                class="flex items-center justify-between gap-3 rounded-lg border border-gray-100 px-3 py-2 dark:border-dark-600"
              >
                <code class="font-mono text-sm text-gray-900 dark:text-gray-100">{{
                  batchCode.code
                }}</code>
                <span
                  :class="[
                    'badge',
                    batchCode.status === 'unused'
                      ? 'badge-success'
                      : batchCode.status === 'used'
                        ? 'badge-gray'
                        : 'badge-danger'
                  ]"
                >
                  {{ t('admin.redeem.status.' + batchCode.status) }}
                </span>
              </div>
            </div>
          </div>
          <div
            class="flex justify-end gap-2 rounded-b-xl border-t border-gray-200 bg-gray-50 px-5 py-4 dark:border-dark-600 dark:bg-dark-700/50"
          >
            <button
              @click="copyViewingBatchCodes"
              :disabled="batchCodes.length === 0"
              :class="[
                'btn flex items-center gap-2 transition-all',
                copiedBatchCodes ? 'btn-success' : 'btn-secondary'
              ]"
            >
              <Icon v-if="!copiedBatchCodes" name="copy" size="sm" :stroke-width="2" />
              <svg v-else class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path
                  stroke-linecap="round"
                  stroke-linejoin="round"
                  stroke-width="2"
                  d="M5 13l4 4L19 7"
                />
              </svg>
              {{ copiedBatchCodes ? t('admin.redeem.copied') : t('admin.redeem.copyAll') }}
            </button>
            <button @click="closeBatchCodesDialog" class="btn btn-primary">
              {{ t('common.close') }}
            </button>
          </div>
        </div>
      </div>
    </Teleport>
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, reactive, computed, onMounted, onUnmounted, watch } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { useClipboard } from '@/composables/useClipboard'
import { useTableSelection } from '@/composables/useTableSelection'
import { getPersistedPageSize } from '@/composables/usePersistedPageSize'
import { adminAPI } from '@/api/admin'
import { xianyuAPI } from '@/api/admin'
import type { XianyuItemPool } from '@/types'
import {
  formatDateTime,
  getBrowserTimeZone,
  parseDateTimeLocalInput
} from '@/utils/format'
import type {
  RedeemCode,
  RedeemCodeType,
  Group,
  GroupPlatform,
  SubscriptionType,
  BatchUpdateRedeemCodeFields,
  WelfareBatch
} from '@/types'
import type { Column } from '@/components/common/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Select from '@/components/common/Select.vue'
import GroupBadge from '@/components/common/GroupBadge.vue'
import GroupOptionItem from '@/components/common/GroupOptionItem.vue'
import Icon from '@/components/icons/Icon.vue'

const { t } = useI18n()
const appStore = useAppStore()
const { copyToClipboard: clipboardCopy } = useClipboard()
const browserTimeZone = getBrowserTimeZone()

interface GroupOption {
  value: number
  label: string
  description: string | null
  platform: GroupPlatform
  subscriptionType: SubscriptionType
  rate: number
}

const showGenerateDialog = ref(false)
const showResultDialog = ref(false)
// 生成结果统一存卡密字符串（普通兑换码与福利批次共用结果弹窗）
const generatedCodes = ref<string[]>([])
const subscriptionGroups = ref<Group[]>([])

// 订阅类型分组选项
const subscriptionGroupOptions = computed(() => {
  return subscriptionGroups.value
    .filter((g) => g.subscription_type === 'subscription')
    .map((g) => ({
      value: g.id,
      label: g.name,
      description: g.description,
      platform: g.platform,
      subscriptionType: g.subscription_type,
      rate: g.rate_multiplier
    }))
})

const batchGroupOptions = computed(() => [
  { value: null, label: t('admin.redeem.clearGroup') },
  ...subscriptionGroupOptions.value
])

const generatedCodesText = computed(() => {
  return generatedCodes.value.join('\n')
})

const textareaHeight = computed(() => {
  const lineCount = generatedCodes.value.length
  const lineHeight = 24 // approximate line height in px
  const padding = 24 // top + bottom padding
  const minHeight = 60
  const maxHeight = 240
  const calculatedHeight = Math.min(
    Math.max(lineCount * lineHeight + padding, minHeight),
    maxHeight
  )
  return `${calculatedHeight}px`
})

const copiedAll = ref(false)

const closeResultDialog = () => {
  showResultDialog.value = false
  generatedCodes.value = []
  copiedAll.value = false
}

const copyGeneratedCodes = async () => {
  const success = await clipboardCopy(generatedCodesText.value, t('admin.redeem.copied'))
  if (success) {
    copiedAll.value = true
    setTimeout(() => {
      copiedAll.value = false
    }, 2000)
  }
}

const downloadGeneratedCodes = () => {
  const blob = new Blob([generatedCodesText.value], { type: 'text/plain' })
  const url = window.URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = `redeem-codes-${new Date().toISOString().split('T')[0]}.txt`
  document.body.appendChild(link)
  link.click()
  document.body.removeChild(link)
  window.URL.revokeObjectURL(url)
}

const columns = computed<Column[]>(() => [
  { key: 'select', label: '' },
  { key: 'code', label: t('admin.redeem.columns.code') },
  { key: 'type', label: t('admin.redeem.columns.type'), sortable: true },
  { key: 'value', label: t('admin.redeem.columns.value'), sortable: true },
  { key: 'status', label: t('admin.redeem.columns.status'), sortable: true },
  { key: 'used_by', label: t('admin.redeem.columns.usedBy') },
  { key: 'used_at', label: t('admin.redeem.columns.usedAt'), sortable: true },
  { key: 'expires_at', label: t('admin.redeem.columns.expiresAt'), sortable: true },
  { key: 'actions', label: t('admin.redeem.columns.actions') }
])

const typeOptions = computed(() => [
  { value: 'balance', label: t('admin.redeem.balance') },
  { value: 'concurrency', label: t('admin.redeem.concurrency') },
  { value: 'subscription', label: t('admin.redeem.subscription') },
  { value: 'invitation', label: t('admin.redeem.invitation') }
])

const filterTypeOptions = computed(() => [
  { value: '', label: t('admin.redeem.allTypes') },
  { value: 'balance', label: t('admin.redeem.balance') },
  { value: 'concurrency', label: t('admin.redeem.concurrency') },
  { value: 'subscription', label: t('admin.redeem.subscription') },
  { value: 'invitation', label: t('admin.redeem.invitation') },
  { value: 'welfare', label: t('admin.redeem.welfareCard') }
])

const filterStatusOptions = computed(() => [
  { value: '', label: t('admin.redeem.allStatus') },
  { value: 'unused', label: t('admin.redeem.unused') },
  { value: 'delivered', label: t('admin.redeem.status.delivered') },
  { value: 'used', label: t('admin.redeem.used') },
  { value: 'expired', label: t('admin.redeem.status.expired') },
  { value: 'disabled', label: t('admin.redeem.status.disabled') }
])

const batchStatusOptions = computed(() => [
  { value: 'unused', label: t('admin.redeem.status.unused') },
  { value: 'disabled', label: t('admin.redeem.status.disabled') },
  { value: 'expired', label: t('admin.redeem.status.expired') }
])

const batchExpiryModeOptions = computed(() => [
  { value: 'clear', label: t('admin.redeem.neverExpires') },
  { value: 'custom', label: t('admin.redeem.customExpiry') }
])

const codes = ref<RedeemCode[]>([])
const loading = ref(false)
const generating = ref(false)
const batchUpdating = ref(false)
const searchQuery = ref('')
const filters = reactive({
  type: '',
  status: ''
})
const pagination = reactive({
  page: 1,
  page_size: getPersistedPageSize(),
  total: 0,
  pages: 0
})
const sortState = reactive({
  sort_by: 'id',
  sort_order: 'desc' as 'asc' | 'desc'
})

let abortController: AbortController | null = null

const showDeleteDialog = ref(false)
const showDeleteSelectedDialog = ref(false)
const showExpireDialog = ref(false)
const expiringCode = ref<RedeemCode | null>(null)
const showBatchUpdateDialog = ref(false)
const deletingCode = ref<RedeemCode | null>(null)
const deletingSelected = ref(false)
const copiedCode = ref<string | null>(null)

const {
  selectedSet: selectedCodeIds,
  selectedCount,
  allVisibleSelected,
  select,
  deselect,
  clear: clearSelectedCodes,
  toggleVisible
} = useTableSelection<RedeemCode>({
  rows: codes,
  getId: (code) => code.id
})

const batchUpdateForm = reactive({
  update_status: false,
  status: 'disabled' as 'unused' | 'disabled',
  update_expires_at: false,
  expires_mode: 'clear' as 'clear' | 'custom',
  expires_at_local: '',
  update_notes: false,
  notes: '',
  update_group_id: false,
  group_id: null as number | null
})

type RedeemCodeExpiryOption = 'never' | '1' | '3' | '7' | 'custom'

const redeemCodeExpiryOptions = computed<{ value: RedeemCodeExpiryOption; label: string }[]>(() => [
  { value: 'never', label: t('admin.redeem.neverExpires') },
  { value: '1', label: t('admin.redeem.expiryPresetDays', { days: 1 }) },
  { value: '3', label: t('admin.redeem.expiryPresetDays', { days: 3 }) },
  { value: '7', label: t('admin.redeem.expiryPresetDays', { days: 7 }) },
  { value: 'custom', label: t('admin.redeem.customExpiry') }
])

const generateForm = reactive({
  type: 'balance' as RedeemCodeType,
  value: 10,
  count: 1,
  group_id: null as number | null,
  validity_days: 30,
  expiry_option: 'never' as RedeemCodeExpiryOption,
  custom_expiry_days: 7
})

// 监听类型变化，邀请码不需要数值
watch(
  () => generateForm.type,
  (newType) => {
    if (newType === 'invitation') {
      generateForm.value = 0
    } else if (generateForm.value === 0) {
      generateForm.value = 10
    }
  }
)

function clearPoolFilter() {
  poolFilter.value = ''
  pagination.page = 1
  loadCodes()
}

// 面值筛选：选项来自管理端 distinct values 接口，随类型筛选联动。
const valueFilter = ref('')
const distinctValues = ref<number[]>([])

const formatFaceValue = (v: number) =>
  filters.type === 'balance' ? `$${v.toFixed(2)}` : String(v)

const valueFilterOptions = computed(() => [
  { value: '', label: t('admin.redeem.allValues') },
  ...distinctValues.value.map((v) => ({ value: String(v), label: formatFaceValue(v) }))
])

const refreshValueOptions = async () => {
  try {
    distinctValues.value = await adminAPI.redeem.listValues(
      (filters.type || undefined) as RedeemCodeType | undefined
    )
    // 类型切换后旧面值可能不存在，失效则重置
    if (
      valueFilter.value !== '' &&
      !distinctValues.value.some((v) => String(v) === valueFilter.value)
    ) {
      valueFilter.value = ''
    }
  } catch (error) {
    console.error('Error loading redeem code values:', error)
    distinctValues.value = []
  }
}

const onTypeFilterChange = () => {
  pagination.page = 1
  void refreshValueOptions()
  loadCodes()
}

const onValueFilterSelect = () => {
  pagination.page = 1
  loadCodes()
}

const buildRedeemQueryFilters = () => ({
  type: (filters.type || undefined) as RedeemCodeType | undefined,
  status: (filters.status || undefined) as
    | 'used'
    | 'delivered'
    | 'expired'
    | 'unused'
    | 'disabled'
    | undefined,
  search: searchQuery.value || undefined,
  pool: poolFilter.value || undefined,
  value: valueFilter.value === '' ? undefined : Number(valueFilter.value),
  sort_by: sortState.sort_by,
  sort_order: sortState.sort_order
})

const loadCodes = async () => {
  if (abortController) {
    abortController.abort()
  }
  const currentController = new AbortController()
  abortController = currentController
  loading.value = true
  try {
    const response = await adminAPI.redeem.list(
      pagination.page,
      pagination.page_size,
      buildRedeemQueryFilters(),
      {
        signal: currentController.signal
      }
    )
    if (currentController.signal.aborted) {
      return
    }
    codes.value = response.items
    pagination.total = response.total
    pagination.pages = response.pages
  } catch (error: any) {
    if (
      currentController.signal.aborted ||
      error?.name === 'AbortError' ||
      error?.code === 'ERR_CANCELED'
    ) {
      return
    }
    appStore.showError(t('admin.redeem.failedToLoad'))
    console.error('Error loading redeem codes:', error)
  } finally {
    if (abortController === currentController && !currentController.signal.aborted) {
      loading.value = false
      abortController = null
    }
  }
}

let searchTimeout: ReturnType<typeof setTimeout>
const handleSearch = () => {
  clearTimeout(searchTimeout)
  searchTimeout = setTimeout(() => {
    pagination.page = 1
    loadCodes()
  }, 300)
}

const handlePageChange = (page: number) => {
  pagination.page = page
  loadCodes()
}

const handlePageSizeChange = (pageSize: number) => {
  pagination.page_size = pageSize
  pagination.page = 1
  loadCodes()
}

const handleSort = (key: string, order: 'asc' | 'desc') => {
  sortState.sort_by = key
  sortState.sort_order = order
  pagination.page = 1
  loadCodes()
}

const toggleSelectRow = (id: number, event: Event) => {
  const target = event.target as HTMLInputElement
  if (target.checked) {
    select(id)
    return
  }
  deselect(id)
}

const toggleSelectAllVisible = (event: Event) => {
  const target = event.target as HTMLInputElement
  toggleVisible(target.checked)
}

const getRedeemCodeExpiresInDays = () => {
  if (generateForm.expiry_option === 'never') {
    return undefined
  }
  if (generateForm.expiry_option === 'custom') {
    if (
      !Number.isFinite(generateForm.custom_expiry_days) ||
      generateForm.custom_expiry_days < 1
    ) {
      return null
    }
    return Math.floor(generateForm.custom_expiry_days)
  }
  return Number(generateForm.expiry_option)
}

const toDatetimeLocalInputValue = (date: Date) => {
  const pad = (value: number) => String(value).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(
    date.getHours()
  )}:${pad(date.getMinutes())}`
}

const resetBatchUpdateForm = () => {
  batchUpdateForm.update_status = false
  batchUpdateForm.status = 'disabled'
  batchUpdateForm.update_expires_at = false
  batchUpdateForm.expires_mode = 'clear'
  batchUpdateForm.expires_at_local = toDatetimeLocalInputValue(
    new Date(Date.now() + 24 * 60 * 60 * 1000)
  )
  batchUpdateForm.update_notes = false
  batchUpdateForm.notes = ''
  batchUpdateForm.update_group_id = false
  batchUpdateForm.group_id = null
}

const openBatchUpdateDialog = () => {
  if (selectedCount.value === 0) {
    appStore.showInfo(t('admin.redeem.selectCodesFirst'))
    return
  }
  resetBatchUpdateForm()
  showBatchUpdateDialog.value = true
}

const closeBatchUpdateDialog = () => {
  showBatchUpdateDialog.value = false
}

const buildBatchUpdateFields = (): BatchUpdateRedeemCodeFields | null => {
  const fields: BatchUpdateRedeemCodeFields = {}

  if (batchUpdateForm.update_status) {
    fields.status = batchUpdateForm.status
  }
  if (batchUpdateForm.update_expires_at) {
    if (batchUpdateForm.expires_mode === 'clear') {
      fields.expires_at = null
    } else {
      const expiresAt = parseDateTimeLocalInput(batchUpdateForm.expires_at_local)
      if (expiresAt === null) {
        appStore.showError(t('admin.redeem.expiryDateRequired'))
        return null
      }
      fields.expires_at = new Date(expiresAt * 1000).toISOString()
    }
  }
  if (batchUpdateForm.update_notes) {
    fields.notes = batchUpdateForm.notes
  }
  if (batchUpdateForm.update_group_id) {
    fields.group_id =
      batchUpdateForm.group_id == null ? null : Number(batchUpdateForm.group_id)
  }

  return Object.keys(fields).length > 0 ? fields : null
}

const handleGenerateCodes = async () => {
  // 订阅类型必须选择分组
  if (generateForm.type === 'subscription' && !generateForm.group_id) {
    appStore.showError(t('admin.redeem.groupRequired'))
    return
  }
  const expiresInDays = getRedeemCodeExpiresInDays()
  if (expiresInDays === null) {
    appStore.showError(t('admin.redeem.expiryDaysRequired'))
    return
  }

  generating.value = true
  try {
    const result = await adminAPI.redeem.generate(
      generateForm.count,
      generateForm.type,
      generateForm.value,
      generateForm.type === 'subscription' ? generateForm.group_id : undefined,
      generateForm.type === 'subscription' ? generateForm.validity_days : undefined,
      expiresInDays
    )
    showGenerateDialog.value = false
    generatedCodes.value = result.map((code) => code.code)
    showResultDialog.value = true
    // 重置表单
    generateForm.group_id = null
    generateForm.validity_days = 30
    generateForm.expiry_option = 'never'
    generateForm.custom_expiry_days = 7
    loadCodes()
  } catch (error: any) {
    appStore.showError(error.response?.data?.detail || t('admin.redeem.failedToGenerate'))
    console.error('Error generating codes:', error)
  } finally {
    generating.value = false
  }
}

const copyToClipboard = async (text: string) => {
  const success = await clipboardCopy(text, t('admin.redeem.copied'))
  if (success) {
    copiedCode.value = text
    setTimeout(() => {
      copiedCode.value = null
    }, 2000)
  }
}

const handleExportCodes = async () => {
  try {
    const blob = await adminAPI.redeem.exportCodes(buildRedeemQueryFilters())

    // Create download link
    const url = window.URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = `redeem-codes-${new Date().toISOString().split('T')[0]}.csv`
    document.body.appendChild(link)
    link.click()
    document.body.removeChild(link)
    window.URL.revokeObjectURL(url)

    appStore.showSuccess(t('admin.redeem.codesExported'))
  } catch (error: any) {
    appStore.showError(error.response?.data?.message || error.response?.data?.detail || t('admin.redeem.failedToExport'))
    console.error('Error exporting codes:', error)
  }
}

const handleDelete = (code: RedeemCode) => {
  deletingCode.value = code
  showDeleteDialog.value = true
}

const confirmDelete = async () => {
  if (!deletingCode.value) return

  try {
    await adminAPI.redeem.delete(deletingCode.value.id)
    appStore.showSuccess(t('admin.redeem.codeDeleted'))
    showDeleteDialog.value = false
    deletingCode.value = null
    loadCodes()
  } catch (error: any) {
    appStore.showError(error.response?.data?.message || error.response?.data?.detail || t('admin.redeem.failedToDelete'))
    console.error('Error deleting code:', error)
  }
}

const handleExpire = (code: RedeemCode) => {
  expiringCode.value = code
  showExpireDialog.value = true
}

const confirmExpire = async () => {
  if (!expiringCode.value) return

  try {
    await adminAPI.redeem.expire(expiringCode.value.id)
    appStore.showSuccess(t('admin.redeem.codeExpired'))
    showExpireDialog.value = false
    expiringCode.value = null
    loadCodes()
  } catch (error: any) {
    appStore.showError(error.response?.data?.message || error.response?.data?.detail || t('admin.redeem.failedToExpire'))
    console.error('Error expiring code:', error)
  }
}

const confirmDeleteSelected = async () => {
  const ids = Array.from(selectedCodeIds.value)
  if (ids.length === 0) {
    showDeleteSelectedDialog.value = false
    return
  }

  deletingSelected.value = true
  try {
    const result = await adminAPI.redeem.batchDelete(ids)
    appStore.showSuccess(t('admin.redeem.selectedCodesDeleted', { count: result.deleted }))
    showDeleteSelectedDialog.value = false
    clearSelectedCodes()
    loadCodes()
  } catch (error: any) {
    appStore.showError(error.response?.data?.message || error.response?.data?.detail || t('admin.redeem.failedToDeleteSelected'))
    console.error('Error deleting selected codes:', error)
  } finally {
    deletingSelected.value = false
  }
}

const openDeleteSelectedDialog = () => {
  if (selectedCount.value === 0) {
    appStore.showInfo(t('admin.redeem.selectCodesFirst'))
    return
  }
  showDeleteSelectedDialog.value = true
}

const handleBatchUpdate = async () => {
  const ids = Array.from(selectedCodeIds.value)
  if (ids.length === 0) {
    appStore.showInfo(t('admin.redeem.selectCodesFirst'))
    return
  }

  const hasSelectedFields =
    batchUpdateForm.update_status ||
    batchUpdateForm.update_expires_at ||
    batchUpdateForm.update_notes ||
    batchUpdateForm.update_group_id
  if (!hasSelectedFields) {
    appStore.showError(t('admin.redeem.noBatchFieldsSelected'))
    return
  }

  const fields = buildBatchUpdateFields()
  if (!fields) {
    return
  }

  batchUpdating.value = true
  try {
    const result = await adminAPI.redeem.batchUpdate(ids, fields)
    appStore.showSuccess(t('admin.redeem.batchUpdateSuccess', { count: result.updated }))
    showBatchUpdateDialog.value = false
    clearSelectedCodes()
    loadCodes()
  } catch (error: any) {
    appStore.showError(error.response?.data?.message || error.response?.data?.detail || t('admin.redeem.failedToBatchUpdate'))
    console.error('Error batch updating codes:', error)
  } finally {
    batchUpdating.value = false
  }
}

// ==================== 福利批次 Tab ====================

type RedeemTabKey = 'welfare' | 'codes'
const activeTab = ref<RedeemTabKey>('welfare')

const switchTab = (tab: RedeemTabKey) => {
  activeTab.value = tab
}

const welfareBatches = ref<WelfareBatch[]>([])
const welfareLoading = ref(false)
const welfarePagination = reactive({
  page: 1,
  page_size: getPersistedPageSize(),
  total: 0,
  pages: 0
})

const welfareColumns = computed<Column[]>(() => [
  { key: 'name', label: t('admin.redeem.welfare.columns.name') },
  { key: 'created_at', label: t('admin.redeem.welfare.columns.createdAt') },
  { key: 'code_count', label: t('admin.redeem.welfare.columns.codeCount') },
  { key: 'usage', label: t('admin.redeem.welfare.columns.usage') },
  { key: 'cleared_amount', label: t('admin.redeem.welfare.columns.clearedAmount') },
  { key: 'actions', label: t('admin.redeem.columns.actions') }
])

const loadWelfareBatches = async () => {
  welfareLoading.value = true
  try {
    const response = await adminAPI.redeem.listWelfareBatches(
      welfarePagination.page,
      welfarePagination.page_size
    )
    welfareBatches.value = response.items
    welfarePagination.total = response.total
    welfarePagination.pages = response.pages
  } catch (error: any) {
    appStore.showError(t('admin.redeem.welfare.failedToLoadBatches'))
    console.error('Error loading welfare batches:', error)
  } finally {
    welfareLoading.value = false
  }
}

const handleWelfarePageChange = (page: number) => {
  welfarePagination.page = page
  loadWelfareBatches()
}

const handleWelfarePageSizeChange = (pageSize: number) => {
  welfarePagination.page_size = pageSize
  welfarePagination.page = 1
  loadWelfareBatches()
}

// 新建福利批次
const showWelfareDialog = ref(false)
const welfareSubmitting = ref(false)
const welfareForm = reactive({
  name: '',
  // 已勾选分组：key=group_id, value=validity_days（天卡=1/周卡=7/月卡=30）
  groups: {} as Record<number, number>,
  amountMin: '',
  amountMax: '',
  count: 10
})

const welfareValidityOptions = computed(() => [
  { value: 1, label: t('admin.redeem.welfare.dayCard') },
  { value: 7, label: t('admin.redeem.welfare.weekCard') },
  { value: 30, label: t('admin.redeem.welfare.monthCard') }
])

const defaultWelfareBatchName = () => {
  const d = new Date()
  const pad = (v: number) => String(v).padStart(2, '0')
  return `${t('admin.redeem.welfare.defaultBatchName')}-${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}-${pad(d.getHours())}${pad(d.getMinutes())}`
}

const openWelfareDialog = () => {
  welfareForm.name = defaultWelfareBatchName()
  welfareForm.groups = {}
  welfareForm.amountMin = ''
  welfareForm.amountMax = ''
  welfareForm.count = 10
  showWelfareDialog.value = true
}

const closeWelfareDialog = () => {
  showWelfareDialog.value = false
}

const toggleWelfareGroup = (groupId: number, event: Event) => {
  const checked = (event.target as HTMLInputElement).checked
  if (checked) {
    // 默认天卡：手滑直接创建时损失最小（此前默认 30 天月卡）
    welfareForm.groups[groupId] = 1
  } else {
    delete welfareForm.groups[groupId]
  }
}

const handleCreateWelfareBatch = async () => {
  const groups = Object.entries(welfareForm.groups).map(([groupId, validityDays]) => ({
    group_id: Number(groupId),
    validity_days: validityDays
  }))

  // v-model 绑定 type="number" 输入框时 Vue 会自动把值转成 number，不能用字符串方法
  const amountMinEmpty = String(welfareForm.amountMin ?? '').trim() === ''
  const amountMaxEmpty = String(welfareForm.amountMax ?? '').trim() === ''
  let amountMin = 0
  let amountMax = 0
  if (!amountMinEmpty || !amountMaxEmpty) {
    if (amountMinEmpty || amountMaxEmpty) {
      appStore.showError(t('admin.redeem.welfare.amountRangeIncomplete'))
      return
    }
    amountMin = Math.round(Number(welfareForm.amountMin) * 100) / 100
    amountMax = Math.round(Number(welfareForm.amountMax) * 100) / 100
    if (!Number.isFinite(amountMin) || !Number.isFinite(amountMax) || amountMin < 0 || amountMax < amountMin) {
      appStore.showError(t('admin.redeem.welfare.amountRangeInvalid'))
      return
    }
  }

  if (groups.length === 0 && amountMax === 0) {
    appStore.showError(t('admin.redeem.welfare.requireOneBenefit'))
    return
  }
  if (!Number.isInteger(welfareForm.count) || welfareForm.count < 1 || welfareForm.count > 1000) {
    appStore.showError(t('admin.redeem.welfare.countInvalid'))
    return
  }

  welfareSubmitting.value = true
  try {
    const result = await adminAPI.redeem.createWelfareBatch({
      name: welfareForm.name,
      groups,
      amount_min: amountMin,
      amount_max: amountMax,
      count: welfareForm.count
    })
    showWelfareDialog.value = false
    generatedCodes.value = result.codes
    showResultDialog.value = true
    loadWelfareBatches()
  } catch (error: any) {
    appStore.showError(
      error.response?.data?.message ||
        error.response?.data?.detail ||
        error.message ||
        t('admin.redeem.welfare.failedToCreate')
    )
    console.error('Error creating welfare batch:', error)
  } finally {
    welfareSubmitting.value = false
  }
}

// 批次卡密操作
const showBatchCodesDialog = ref(false)
const viewingBatch = ref<WelfareBatch | null>(null)
const batchCodes = ref<RedeemCode[]>([])
const batchCodesLoading = ref(false)
const copiedBatchCodes = ref(false)

const copyUnusedBatchCodes = async (batch: WelfareBatch) => {
  try {
    const codes = await adminAPI.redeem.listWelfareBatchCodes(batch.id, 'unused')
    if (codes.length === 0) {
      appStore.showInfo(t('admin.redeem.welfare.noUnusedCodes'))
      return
    }
    await clipboardCopy(
      codes.map((c) => c.code).join('\n'),
      t('admin.redeem.welfare.unusedCopied', { count: codes.length })
    )
  } catch (error: any) {
    appStore.showError(t('admin.redeem.welfare.failedToLoadCodes'))
    console.error('Error loading unused batch codes:', error)
  }
}

const csvEscape = (value: string) => {
  if (/[",\n]/.test(value)) {
    return `"${value.replace(/"/g, '""')}"`
  }
  return value
}

const exportBatchCsv = async (batch: WelfareBatch) => {
  try {
    const codes = await adminAPI.redeem.listWelfareBatchCodes(batch.id)
    const header = 'code,value,status,used_by_email,used_at,expires_at'
    const rows = codes.map((c) =>
      [
        c.code,
        c.value.toFixed(2),
        c.status,
        c.user?.email ?? '',
        c.used_at ?? '',
        c.expires_at ?? ''
      ]
        .map(csvEscape)
        .join(',')
    )
    const blob = new Blob(['\uFEFF' + [header, ...rows].join('\n')], {
      type: 'text/csv;charset=utf-8'
    })
    const url = window.URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = `welfare-batch-${batch.id}-${new Date().toISOString().split('T')[0]}.csv`
    document.body.appendChild(link)
    link.click()
    document.body.removeChild(link)
    window.URL.revokeObjectURL(url)
    appStore.showSuccess(t('admin.redeem.welfare.batchExported'))
  } catch (error: any) {
    appStore.showError(t('admin.redeem.welfare.failedToExportBatch'))
    console.error('Error exporting batch codes:', error)
  }
}

const openBatchCodesDialog = async (batch: WelfareBatch) => {
  viewingBatch.value = batch
  batchCodes.value = []
  copiedBatchCodes.value = false
  showBatchCodesDialog.value = true
  batchCodesLoading.value = true
  try {
    batchCodes.value = await adminAPI.redeem.listWelfareBatchCodes(batch.id)
  } catch (error: any) {
    appStore.showError(t('admin.redeem.welfare.failedToLoadCodes'))
    console.error('Error loading batch codes:', error)
  } finally {
    batchCodesLoading.value = false
  }
}

const closeBatchCodesDialog = () => {
  showBatchCodesDialog.value = false
  viewingBatch.value = null
  batchCodes.value = []
  copiedBatchCodes.value = false
}

const copyViewingBatchCodes = async () => {
  const success = await clipboardCopy(
    batchCodes.value.map((c) => c.code).join('\n'),
    t('admin.redeem.copied')
  )
  if (success) {
    copiedBatchCodes.value = true
    setTimeout(() => {
      copiedBatchCodes.value = false
    }, 2000)
  }
}

// 加载订阅类型分组
const loadSubscriptionGroups = async () => {
  try {
    const groups = await adminAPI.groups.getAll()
    subscriptionGroups.value = groups
  } catch (error) {
    console.error('Error loading subscription groups:', error)
  }
}

// 库存池页"管理库存码"带 pool 查询参数跳转到本页，首次 loadCodes 前按池过滤库存码。
const route = useRoute()
const poolFilter = ref(typeof route.query.pool === 'string' ? route.query.pool.trim() : '')

// 库存池清单（按名称筛选，值为 slug）：管理端库存池接口，失败静默降级为仅芯片筛选。
const pools = ref<XianyuItemPool[]>([])
const poolNameBySlug = computed(() => {
  const map: Record<string, string> = {}
  for (const p of pools.value) map[p.slug] = p.name
  return map
})
const poolOptions = computed(() => [
  { value: '', label: t('admin.redeem.allPools') },
  ...pools.value.map((p) => ({ value: p.slug, label: p.name }))
])
const poolDisplayName = computed(
  () => poolNameBySlug.value[poolFilter.value] || poolFilter.value
)

const onPoolSelect = (value: string | number | boolean | null) => {
  poolFilter.value = String(value ?? '')
  pagination.page = 1
  loadCodes()
}

async function loadPools() {
  try {
    pools.value = (await xianyuAPI.listItemPools()) || []
  } catch {
    pools.value = []
  }
}

onMounted(() => {
  loadCodes()
  loadWelfareBatches()
  loadSubscriptionGroups()
  void loadPools()
  void refreshValueOptions()
})

onUnmounted(() => {
  clearTimeout(searchTimeout)
  abortController?.abort()
})
</script>
