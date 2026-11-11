<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div class="min-w-0">
            <h1 class="page-title flex items-center gap-2 text-xl font-black text-gray-900 dark:text-white">
              <span class="inline-flex h-8 w-8 items-center justify-center rounded-xl bg-blue-50 text-blue-500 dark:bg-blue-900/30 dark:text-blue-400">
                <Icon name="key" size="sm" />
              </span>
              {{ t('admin.quotaKeys.title') }}
            </h1>
            <p class="page-description mt-1.5 text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.description') }}
            </p>
          </div>
          <div class="flex flex-1 flex-wrap items-center justify-end gap-2">
            <!-- 上游显示名编辑入口（CARD-KC-18）：与 KC-09 的「上游续费」入口并列，
                 但职责不同——本按钮编辑上游注册表的显示名 / 续费链接 / 备注，
                 不替换 KC-09 既有的 kn-cy.com / ShareAI 静态续费入口。 -->
            <button
              class="btn btn-secondary"
              :disabled="upstreamLoading"
              data-testid="upstream-edit-toggle"
              @click="openUpstreamDialog"
            >
              <Icon name="edit" size="md" class="mr-1" />
              {{ t('admin.quotaKeys.upstream.edit') }}
            </button>

            <!-- 上游续费/采购入口（CARD-KC-09）：kn-cy.com 有公开商铺，直开外链；
                 ShareAI 暂无公开商铺，保留占位项并以提示模态框说明。 -->
            <div ref="upstreamStoreRef" class="relative">
              <button
                class="btn btn-secondary"
                :aria-expanded="upstreamStoreOpen"
                aria-haspopup="menu"
                data-testid="upstream-store-toggle"
                @click="upstreamStoreOpen = !upstreamStoreOpen"
              >
                <Icon name="creditCard" size="md" class="mr-1" />
                {{ t('admin.quotaKeys.upstreamStore.button') }}
                <Icon
                  name="chevronDown"
                  size="sm"
                  class="ml-1 transition-transform duration-200"
                  :class="upstreamStoreOpen ? 'rotate-180' : ''"
                />
              </button>
              <div
                v-if="upstreamStoreOpen"
                class="absolute right-0 z-40 mt-1 w-56 overflow-hidden rounded-xl border border-gray-200 bg-white py-1 shadow-lg dark:border-dark-700 dark:bg-dark-800"
                role="menu"
              >
                <a
                  :href="KNCY_STORE_URL"
                  target="_blank"
                  rel="noopener noreferrer"
                  class="flex items-center justify-between gap-2 px-3 py-2 text-sm text-gray-700 transition-colors hover:bg-gray-50 dark:text-dark-200 dark:hover:bg-dark-700"
                  role="menuitem"
                  data-testid="upstream-store-kncy"
                  :title="t('admin.quotaKeys.upstreamStore.openExternal')"
                  @click="upstreamStoreOpen = false"
                >
                  <span class="truncate">{{ t('admin.quotaKeys.upstreamStore.kncy') }}</span>
                  <Icon name="externalLink" size="sm" class="shrink-0 text-gray-400" />
                </a>
                <button
                  class="flex w-full items-center justify-between gap-2 px-3 py-2 text-left text-sm text-gray-700 transition-colors hover:bg-gray-50 dark:text-dark-200 dark:hover:bg-dark-700"
                  role="menuitem"
                  data-testid="upstream-store-shareai"
                  @click="openShareaiPlaceholder"
                >
                  <span class="truncate">{{ t('admin.quotaKeys.upstreamStore.shareai') }}</span>
                  <span class="shrink-0 rounded bg-gray-100 px-1.5 py-0.5 text-[10px] text-gray-500 dark:bg-dark-700 dark:text-dark-400">
                    {{ t('admin.quotaKeys.upstreamStore.pending') }}
                  </span>
                </button>
              </div>
            </div>
            <button
              class="btn btn-secondary"
              :disabled="loading"
              :title="t('common.refresh')"
              @click="load"
            >
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button class="btn btn-secondary" :disabled="sweeping" @click="openSweepDialog">
              {{ t('admin.quotaKeys.sweepExpired') }}
            </button>
            <button class="btn btn-primary" @click="openCreateDialog">
              <Icon name="plus" size="md" class="mr-1" />
              {{ t('admin.quotaKeys.create') }}
            </button>
          </div>
        </div>
      </template>

      <template #table>
        <DataTable :columns="columns" :data="keys" :loading="loading">
          <template #cell-code_masked="{ value }">
            <code class="font-mono text-sm text-gray-900 dark:text-gray-100">{{ value || t('admin.quotaKeys.na') }}</code>
          </template>

          <!-- CARD-KC-18：激活码列表按上游区分显示。匹配到上游条目 → 显示名 (标识)；
               上游条目缺失但 upstream_key 存在 → 回落显示原始 upstream_key（不空白）；
               后端尚未回填 upstream_key 时 → 占位符。renewal_url 非空时附上游续费外链。 -->
          <template #cell-upstream="{ row }">
            <span
              v-if="upstreamLabel(row)"
              class="inline-flex items-center gap-1 text-sm text-gray-700 dark:text-gray-200"
            >
              {{ upstreamLabel(row) }}
              <a
                v-if="upstreamRenewalUrl(row)"
                :href="upstreamRenewalUrl(row)"
                target="_blank"
                rel="noopener noreferrer"
                class="text-blue-500 hover:text-blue-600 dark:text-blue-400"
                :title="t('admin.quotaKeys.upstream.renewalUrl')"
              >
                <Icon name="externalLink" size="sm" />
              </a>
            </span>
            <span v-else class="text-sm text-gray-400 dark:text-gray-500">{{ t('admin.quotaKeys.na') }}</span>
          </template>

          <!-- §25.6⑦ / §25.7-B：来源额度快照（供应商总额/已用/算术剩余 + 观测时间）。
               无快照 → 不渲染任何数值；最近一次观测失败 → 保留上次成功值并标注查询失败。 -->
          <template #cell-usage="{ row }">
            <QuotaKeyUsageCell v-if="row.usage" :usage="row.usage" />
            <span v-else class="text-sm text-gray-400 dark:text-gray-500">{{ t('admin.quotaKeys.na') }}</span>
          </template>

          <template #cell-status="{ row }">
            <span
              class="inline-flex items-center rounded-md px-2 py-0.5 text-xs font-medium"
              :class="statusBadgeClass(row)"
            >
              {{ statusLabel(row) }}
            </span>
          </template>

          <template #cell-exhausted="{ row }">
            <span :class="row.exhausted ? 'text-red-600 dark:text-red-400' : 'text-gray-500 dark:text-gray-400'">
              {{ row.exhausted ? t('admin.quotaKeys.yes') : t('admin.quotaKeys.no') }}
            </span>
          </template>

          <!-- 优先级：复用号池（账号管理）同一套优先级编辑控件 PriorityStepper，
               悬停出现 [−]/[+]，点数字可就地输入。改动经后端同一事务同步到该码
               名下所有未删除账号（用户裁定 2026-11-03）。 -->
          <template #cell-priority="{ row }">
            <PriorityStepper
              test-id="quota-key-priority"
              :value="row.priority ?? 1"
              :save="(next: number) => savePriority(row, next)"
              @error="(err: unknown) => appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.prioritySaveError')))"
            />
          </template>

          <template #cell-account_count="{ row }">
            <span class="text-sm text-gray-900 dark:text-gray-100">{{ row.account_count ?? 0 }}</span>
          </template>

          <template #cell-working_count="{ row }">
            <span class="text-sm text-gray-900 dark:text-gray-100">{{ row.working_count ?? 0 }}</span>
          </template>

          <template #cell-expires_at="{ row }">
            <span class="text-sm text-gray-500 dark:text-dark-400">{{ formatExpiresAt(row) }}</span>
          </template>

          <template #cell-last_refresh_error="{ row }">
            <span
              v-if="row.last_refresh_error"
              class="text-xs text-red-600 dark:text-red-400"
              :title="formatLastRefreshError(row.last_refresh_error)"
            >
              {{ formatLastRefreshError(row.last_refresh_error) }}
            </span>
            <span v-else class="text-sm text-gray-400 dark:text-gray-500">{{ t('admin.quotaKeys.na') }}</span>
          </template>

          <template #cell-actions="{ row }">
            <div class="flex items-center justify-end gap-1">
              <button
                class="rounded p-1 text-gray-400 transition-colors hover:text-gray-600 dark:hover:text-gray-200"
                :title="t('admin.quotaKeys.actions.detail')"
                @click="openDetail(row)"
              >
                <Icon name="eye" size="sm" />
              </button>
              <button
                class="rounded p-1 text-gray-400 transition-colors hover:text-gray-600 dark:hover:text-gray-200"
                :title="t('admin.quotaKeys.actions.edit')"
                @click="openEditDialog(row)"
              >
                <Icon name="edit" size="sm" />
              </button>
              <button
                class="rounded p-1 text-gray-400 transition-colors hover:text-gray-600 disabled:opacity-40 dark:hover:text-gray-200"
                :disabled="busyId === row.id"
                :title="t('admin.quotaKeys.actions.refresh')"
                @click="handleRefresh(row)"
              >
                <Icon name="sync" size="sm" />
              </button>
              <button
                v-if="row.exhausted"
                class="rounded p-1 text-gray-400 transition-colors hover:text-gray-600 disabled:opacity-40 dark:hover:text-gray-200"
                :disabled="busyId === row.id"
                :title="t('admin.quotaKeys.actions.clearExhausted')"
                @click="handleClearExhausted(row)"
              >
                <Icon name="checkCircle" size="sm" />
              </button>
              <button
                class="rounded p-1 text-gray-400 transition-colors hover:text-blue-600 disabled:opacity-40 dark:hover:text-blue-400"
                :disabled="busyId === row.id"
                :title="t('admin.quotaKeys.probe.button')"
                @click="handleProbe(row)"
              >
                <Icon name="chart" size="sm" />
              </button>
              <button
                class="rounded p-1 text-gray-400 transition-colors hover:text-amber-600 disabled:opacity-40 dark:hover:text-amber-400"
                :disabled="statusTogglingId === row.id"
                :title="row.status === 'disabled' ? t('admin.quotaKeys.enable') : t('admin.quotaKeys.disable')"
                @click="openStatusConfirm(row)"
              >
                <Icon :name="row.status === 'disabled' ? 'play' : 'ban'" size="sm" />
              </button>
              <button
                class="rounded p-1 text-gray-400 transition-colors hover:text-red-600 dark:hover:text-red-400"
                :title="t('admin.quotaKeys.actions.delete')"
                @click="openDeleteDialog(row)"
              >
                <Icon name="trash" size="sm" />
              </button>
            </div>
          </template>

          <template #empty>
            <EmptyState
              :title="t('admin.quotaKeys.emptyTitle')"
              :description="t('admin.quotaKeys.emptyDescription')"
              :action-text="t('admin.quotaKeys.create')"
              @action="openCreateDialog"
            />
          </template>
        </DataTable>
      </template>
    </TablePageLayout>

    <!-- 创建 / 编辑 -->
    <BaseDialog
      :show="showFormDialog"
      :title="editing ? t('admin.quotaKeys.editTitle') : t('admin.quotaKeys.createTitle')"
      width="normal"
      @close="closeFormDialog"
    >
      <div class="space-y-4">
        <!-- F1：明文激活码只在创建时输入一次，不进入持久化 state（submit 后立即清空） -->
        <div v-if="!editing">
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.code') }}
          </label>
          <input
            v-model="form.code"
            type="text"
            autocomplete="off"
            class="input font-mono"
            :placeholder="t('admin.quotaKeys.codePlaceholder')"
            @input="formError = ''"
          />
          <p class="input-hint mt-1">{{ t('admin.quotaKeys.codeHint') }}</p>
        </div>

        <!-- 所属上游：创建时选定，随创建请求提交；创建后不可改（编辑态不渲染）。
             选项 label 沿用列表「显示名 (标识)」口径（与 upstreamLabel 同源）。 -->
        <div v-if="!editing">
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.upstream.selectLabel') }}
          </label>
          <select v-model="form.upstream_key" class="input" data-testid="upstream-select">
            <option value="">{{ t('admin.quotaKeys.upstream.selectPlaceholder') }}</option>
            <option v-for="u in upstreams" :key="u.key" :value="u.key">
              {{ u.name }} ({{ u.key }})
            </option>
          </select>
          <p class="input-hint mt-1">{{ t('admin.quotaKeys.upstream.selectHint') }}</p>
        </div>

        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.expiresAt') }}
          </label>
          <input v-model="form.expires_at" type="datetime-local" class="input" />
          <p class="input-hint mt-1">{{ t('admin.quotaKeys.expiresAtPlaceholder') }}</p>
        </div>

        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.priority') }}
          </label>
          <input
            v-model.number="form.priority"
            type="number"
            min="1"
            step="1"
            class="input"
            :placeholder="t('admin.quotaKeys.priorityPlaceholder')"
            @input="formError = ''"
          />
          <p class="input-hint mt-1">{{ t('admin.quotaKeys.priorityHint') }}</p>
        </div>

        <!-- 并发数（B 批上移，不含余额查询） -->
        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.concurrency') }}
          </label>
          <input
            v-model.number="form.concurrency"
            type="number"
            min="1"
            class="input"
            :placeholder="t('admin.quotaKeys.concurrencyPlaceholder')"
            @input="formError = ''"
          />
          <p class="input-hint mt-1">{{ t('admin.quotaKeys.concurrencyHint') }}</p>
        </div>

        <!-- 负载因子（B 批上移） -->
        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.loadFactor') }}
          </label>
          <input
            v-model.number="form.load_factor"
            type="number"
            min="1"
            max="10000"
            class="input"
            :placeholder="t('admin.quotaKeys.loadFactorPlaceholder')"
            @input="formError = ''"
          />
          <p class="input-hint mt-1">{{ t('admin.quotaKeys.loadFactorHint') }}</p>
        </div>

        <!-- 计费倍率（B 批上移） -->
        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.rateMultiplier') }}
          </label>
          <input
            v-model.number="form.rate_multiplier"
            type="number"
            min="0"
            step="0.001"
            class="input"
            :placeholder="t('admin.quotaKeys.rateMultiplierPlaceholder')"
            @input="formError = ''"
          />
          <p class="input-hint mt-1">{{ t('admin.quotaKeys.rateMultiplierHint') }}</p>
        </div>

        <!-- 分组绑定（D2a）：把激活码绑定到分组；候选分组按所选模型家族过滤，
             并放行 cursor 原生平台分组。未触碰分组时不随更新请求提交。 -->
        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.groupBinding') }}
          </label>
          <GroupSelector
            :groups="accountGroups"
            v-model="form.group_ids"
            :account-models="quotaKeySelectedModels.length > 0 ? quotaKeySelectedModels : undefined"
            :native-platforms="quotaKeyNativePlatforms"
            @update:model-value="groupTouched = true"
          />
          <p class="input-hint mt-1">{{ t('admin.quotaKeys.groupBindingHint') }}</p>
        </div>

        <!-- 模型限制（激活码级权威值，白名单/映射双模式） -->
        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.modelRestriction') }}
          </label>

          <div class="mb-4 flex gap-2">
            <button
              type="button"
              :class="[
                'flex-1 rounded-lg px-4 py-2 text-sm font-medium transition-all',
                modelRestrictionMode === 'whitelist'
                  ? 'bg-primary-100 text-primary-700 dark:bg-primary-900/30 dark:text-primary-400'
                  : 'bg-gray-100 text-gray-600 hover:bg-gray-200 dark:bg-dark-600 dark:text-gray-400 dark:hover:bg-dark-500'
              ]"
              @click="modelRestrictionMode = 'whitelist'; modelRestrictionTouched = true"
            >
              {{ t('admin.accounts.modelWhitelist') }}
            </button>
            <button
              type="button"
              :class="[
                'flex-1 rounded-lg px-4 py-2 text-sm font-medium transition-all',
                modelRestrictionMode === 'mapping'
                  ? 'bg-purple-100 text-purple-700 dark:bg-purple-900/30 dark:text-purple-400'
                  : 'bg-gray-100 text-gray-600 hover:bg-gray-200 dark:bg-dark-600 dark:text-gray-400 dark:hover:bg-dark-500'
              ]"
              @click="modelRestrictionMode = 'mapping'; modelRestrictionTouched = true"
            >
              {{ t('admin.accounts.modelMapping') }}
            </button>
          </div>

          <!-- 白名单模式 -->
          <div v-if="modelRestrictionMode === 'whitelist'">
            <ModelWhitelistSelector
              v-model="allowedModels"
              :model-mappings="modelMappings"
              platform="cursor"
              :custom-sync="customSyncProp"
              @update:model-value="modelRestrictionTouched = true"
            />
            <p v-if="!editing" class="mt-1 text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.syncModelsRequiresSavedKey') }}
            </p>
            <p v-else class="mt-1 text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.modelRestrictionHint') }}
            </p>
          </div>

          <!-- 映射模式 -->
          <div v-else class="space-y-3">
            <!-- 快速映射预设按钮组：按所选模型家族平台提供一键添加映射的预设；
                 已存在于 modelMappings 或 allowedModels 的 from 自动跳过。 -->
            <div>
              <div class="mb-1.5 text-xs font-medium text-gray-600 dark:text-gray-300">
                + {{ t('admin.quotaKeys.quickMapping') }}
              </div>
              <div v-if="quickMappingPresets.length" class="flex flex-wrap gap-1.5">
                <button
                  v-for="preset in quickMappingPresets"
                  :key="preset.from"
                  type="button"
                  :class="preset.color"
                  class="rounded-full px-2.5 py-1 text-xs font-medium transition-colors"
                  @click="addQuickMapping(preset.from, preset.to)"
                >
                  {{ preset.label }}
                </button>
              </div>
              <p v-else class="text-xs text-gray-400 dark:text-gray-500">
                {{ t('admin.accounts.noMatchingModels') }}
              </p>
            </div>

            <div
              v-for="(mapping, index) in modelMappings"
              :key="index"
              class="flex items-center gap-2"
            >
              <input
                v-model="mapping.from"
                type="text"
                class="input flex-1"
                :placeholder="t('admin.accounts.fromModel')"
                @input="modelRestrictionTouched = true"
              />
              <span class="text-gray-400">→</span>
              <input
                v-model="mapping.to"
                type="text"
                class="input flex-1"
                :placeholder="t('admin.accounts.toModel')"
                @input="modelRestrictionTouched = true"
              />
              <button
                type="button"
                class="text-red-500 hover:text-red-700"
                @click="removeModelMapping(index)"
              >
                <Icon name="trash" size="sm" />
              </button>
            </div>
            <button
              type="button"
              class="btn btn-secondary text-sm"
              @click="addModelMapping"
            >
              + {{ t('admin.accounts.addMapping') }}
            </button>
            <p class="text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.accounts.selectedModels', { count: modelMappings.length }) }}
              <span v-if="modelMappings.length === 0" class="text-amber-600 dark:text-amber-400">（{{ t('admin.quotaKeys.modelRestrictionRequired') }}）</span>
            </p>
            <!-- 白名单与映射共存说明（方案 §3 C1）。 -->
            <p class="text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.mappingCoexistHint') }}
            </p>
            <p v-if="invalidWildcardPatterns.length" class="text-xs text-red-600 dark:text-red-400">
              {{ t('admin.accounts.wildcardOnlyAtEnd') }}
            </p>
          </div>
        </div>

        <div>
          <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.quotaKeys.notes') }}
          </label>
          <TextArea
            v-model="form.notes"
            :rows="3"
            :placeholder="t('admin.quotaKeys.notesPlaceholder')"
          />
        </div>

        <p v-if="formError" class="text-sm text-red-600 dark:text-red-400">{{ formError }}</p>
      </div>

      <template #footer>
        <div class="flex items-center justify-end gap-2">
          <button class="btn btn-secondary" @click="closeFormDialog">{{ t('common.cancel') }}</button>
          <button class="btn btn-primary" :disabled="saving" @click="submitForm">
            {{ t('common.save') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <!-- 详情：账号列表默认折叠（§0.1 #7），折叠状态按激活码 id 记忆，不随刷新/翻页重置 -->
    <!-- z-index 45（低于 EditAccountModal 内层 BaseDialog 的默认 50）：两层弹窗嵌套时
         内层账号编辑弹窗必须压在外层详情弹窗之上，而 BaseDialog 的 zIndex prop 只在
         != 50 时才以内联样式生效（50 时走 .modal-overlay 的 z-50 类），故这里下调外层。 -->
    <BaseDialog
      :show="showDetailDialog"
      :title="t('admin.quotaKeys.detailTitle')"
      width="wide"
      :z-index="45"
      @close="showDetailDialog = false"
    >
      <div v-if="detail" class="space-y-4">
        <dl class="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2">
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.maskedCode') }}
            </dt>
            <dd class="mt-0.5 font-mono text-sm text-gray-900 dark:text-gray-100">{{ detail.code_masked }}</dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.status') }}
            </dt>
            <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ statusLabel(detail) }}</dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.exhausted') }}
            </dt>
            <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">
              {{ detail.exhausted ? t('admin.quotaKeys.yes') : t('admin.quotaKeys.no') }}
            </dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.accountCount') }}
            </dt>
            <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ detail.account_count ?? 0 }}</dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.workingCount') }}
            </dt>
            <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ detail.working_count ?? 0 }}</dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.expiresAt') }}
            </dt>
            <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ formatExpiresAt(detail) }}</dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.lastRefreshAt') }}
            </dt>
            <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">
              {{ detail.last_refresh_at ? formatDateTime(detail.last_refresh_at) : t('admin.quotaKeys.na') }}
            </dd>
          </div>
          <div>
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.lastRefreshError') }}
            </dt>
            <dd class="mt-0.5 break-all text-sm text-red-600 dark:text-red-400">
              {{ formatLastRefreshError(detail.last_refresh_error) || t('admin.quotaKeys.na') }}
            </dd>
          </div>
          <div class="sm:col-span-2">
            <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.notes') }}
            </dt>
            <dd class="mt-0.5 whitespace-pre-wrap text-sm text-gray-900 dark:text-gray-100">
              {{ detail.notes || t('admin.quotaKeys.na') }}
            </dd>
          </div>
        </dl>

        <!-- §25.6⑤⑦：详情展示与列表/probe/账号页同一份已提交快照投影（只读，不发网络）。
             无快照时只显示占位符，不补 0、不伪造「无额度」。 -->
        <div class="rounded-lg border border-gray-200 p-2 dark:border-dark-600">
          <QuotaKeyUsageCell v-if="detail.usage" :usage="detail.usage" />
          <span v-else class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.quotaKeys.na') }}</span>
        </div>

        <div class="border-t border-gray-200 pt-3 dark:border-dark-600">
          <button
            class="flex w-full items-center justify-between gap-2 text-left"
            :aria-expanded="isAccountsExpanded(detail.id)"
            @click="toggleAccounts(detail.id)"
          >
            <span class="text-sm font-medium text-gray-900 dark:text-white">
              {{ t('admin.quotaKeys.accountsTitle') }}（{{ detail.account_count ?? 0 }}）
            </span>
            <span class="flex items-center gap-1 text-xs text-gray-500 dark:text-gray-400">
              {{
                isAccountsExpanded(detail.id)
                  ? t('admin.quotaKeys.collapseAccounts')
                  : t('admin.quotaKeys.expandAccounts')
              }}
              <Icon
                :name="isAccountsExpanded(detail.id) ? 'chevronDown' : 'chevronRight'"
                size="sm"
              />
            </span>
          </button>

          <!-- 跳转到账号管理页并带上该激活码的深链筛选（quota_key_id）。
               账号页会自动应用该筛选，这里只负责导航。 -->
          <div class="mt-2 flex justify-end">
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              data-testid="quota-key-view-all-accounts"
              @click="goToAllAccounts"
            >
              <Icon name="users" size="sm" class="mr-1" />
              {{ t('admin.quotaKeys.detail.viewAllAccounts') }}
            </button>
          </div>

          <div v-if="isAccountsExpanded(detail.id)" class="mt-3">
            <!-- P1-B3 两层额度语义：账号层只展示归属与状态，绝不复制 key 级余额数值。 -->
            <p class="mb-2 text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.detail.accountsQuotaHint') }}
            </p>
            <p v-if="!detail.accounts || detail.accounts.length === 0" class="text-sm text-gray-500 dark:text-gray-400">
              {{ t('admin.quotaKeys.noAccounts') }}
            </p>
            <ul v-else class="divide-y divide-gray-100 dark:divide-dark-700">
              <li
                v-for="account in detail.accounts"
                :key="account.id"
                class="flex items-center justify-between gap-3 py-2"
              >
                <div class="min-w-0">
                  <p class="truncate text-sm font-medium text-gray-900 dark:text-white">{{ account.name }}</p>
                  <p class="truncate text-xs text-gray-500 dark:text-gray-400">
                    #{{ account.id }} · {{ account.platform }}
                  </p>
                </div>
                <div class="flex shrink-0 items-center gap-2">
                  <span class="text-xs text-gray-500 dark:text-gray-400">{{ account.status }}</span>
                  <!-- 账号编辑入口：复用账号管理页的 EditAccountModal（不复制其表单逻辑）。
                       激活码与账号是 1:N，故按行给入口（每行一个账号、一个编辑动作）。
                       点「编辑」到弹窗打开之间有 getById 网络延迟，用行内 loading 态反馈。 -->
                  <button
                    type="button"
                    class="rounded p-1 text-gray-400 transition-colors hover:text-blue-600 disabled:opacity-40 dark:hover:text-blue-400"
                    :disabled="accountEditLoadingId !== null"
                    :title="
                      accountEditLoadingId === account.id
                        ? t('admin.quotaKeys.accountEdit.loading')
                        : t('admin.quotaKeys.accountEdit.edit')
                    "
                    :aria-label="t('admin.quotaKeys.accountEdit.edit')"
                    data-testid="quota-key-account-edit"
                    @click="openAccountEditor(account.id)"
                  >
                    <Icon
                      :name="accountEditLoadingId === account.id ? 'refresh' : 'edit'"
                      size="sm"
                      :class="accountEditLoadingId === account.id ? 'animate-spin' : ''"
                    />
                  </button>
                </div>
              </li>
            </ul>
            <Pagination
              v-if="detail.total > detailPageSize"
              class="mt-2"
              :page="detailPage"
              :total="detail.total"
              :page-size="detailPageSize"
              :show-page-size-selector="false"
              @update:page="handleDetailPageChange"
            />
          </div>
        </div>
      </div>

      <template #footer>
        <div class="flex items-center justify-end">
          <button class="btn btn-secondary" @click="showDetailDialog = false">{{ t('common.close') }}</button>
        </div>
      </template>
    </BaseDialog>

    <!-- 账号编辑：复用账号管理页的 EditAccountModal（不复制其表单逻辑）。
         v-if 懒挂载：只有真的要点编辑时才创建该组件，避免每次进激活码页都多打两次
         settings/proxies/groups 请求。关闭内层只改自己的 show，详情弹窗保持打开。 -->
    <EditAccountModal
      v-if="showAccountEditDialog"
      :show="showAccountEditDialog"
      :account="editingAccount"
      :proxies="accountProxies"
      :groups="accountGroups"
      @close="closeAccountEditor"
      @updated="handleAccountUpdated"
    />

    <ConfirmDialog
      :show="showDeleteDialog"
      :title="t('admin.quotaKeys.confirmDeleteTitle')"
      :message="t('admin.quotaKeys.deleteConfirm')"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="confirmDelete"
      @cancel="showDeleteDialog = false"
    />

    <ConfirmDialog
      :show="showSweepDialog"
      :title="t('admin.quotaKeys.confirmSweepTitle')"
      :message="t('admin.quotaKeys.sweepConfirm')"
      :confirm-text="t('common.confirm')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="confirmSweep"
      @cancel="showSweepDialog = false"
    />

    <ConfirmDialog
      :show="statusConfirm.open"
      :title="statusConfirm.next === 'active' ? t('admin.quotaKeys.confirmEnableTitle') : t('admin.quotaKeys.confirmDisableTitle')"
      :message="statusConfirm.next === 'active' ? t('admin.quotaKeys.confirmEnableMsg') : t('admin.quotaKeys.confirmDisableMsg')"
      :confirm-text="t('common.confirm')"
      :cancel-text="t('common.cancel')"
      :danger="statusConfirm.next === 'disabled'"
      @confirm="confirmStatusToggle"
      @cancel="statusConfirm.open = false"
    />

    <!-- 上游续费占位提示：ShareAI 暂无公开商铺（CARD-KC-09） -->
    <BaseDialog
      :show="showShareaiPlaceholder"
      :title="t('admin.quotaKeys.upstreamStore.shareaiUnconfiguredTitle')"
      width="normal"
      @close="showShareaiPlaceholder = false"
    >
      <p class="text-sm text-gray-600 dark:text-gray-300">
        {{ t('admin.quotaKeys.upstreamStore.shareaiUnconfigured') }}
      </p>
      <template #footer>
        <div class="flex items-center justify-end">
          <button class="btn btn-secondary" @click="showShareaiPlaceholder = false">
            {{ t('common.close') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <!-- 入库显示名/续费链接/备注编辑（CARD-KC-18）：与 KC-09 的「上游续费」入口并列，
         但负责维护 quota_upstreams 注册表的显示名等元数据；不替换 KC-09 既有外链入口。 -->
    <BaseDialog
      :show="showUpstreamDialog"
      :title="t('admin.quotaKeys.upstream.editTitle')"
      width="wide"
      @close="showUpstreamDialog = false"
    >
      <div v-if="upstreamLoading" class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">
        {{ t('admin.quotaKeys.upstream.loadError') }}
      </div>
      <p v-else-if="!upstreams.length" class="py-6 text-center text-sm text-gray-500 dark:text-gray-400">
        {{ t('admin.quotaKeys.upstream.empty') }}
      </p>
      <ul v-else class="divide-y divide-gray-100 dark:divide-dark-700">
        <li
          v-for="u in upstreams"
          :key="u.key"
          class="space-y-2 py-3"
        >
          <div class="flex items-center justify-between gap-2">
            <span class="rounded bg-gray-100 px-1.5 py-0.5 text-[10px] text-gray-500 dark:bg-dark-700 dark:text-dark-400">
              {{ u.key }}
            </span>
            <span class="text-xs text-gray-400 dark:text-gray-500">{{ u.type }}</span>
          </div>
          <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
            <div>
              <label class="mb-1 block text-xs font-medium text-gray-700 dark:text-gray-300">
                {{ t('admin.quotaKeys.upstream.name') }}
              </label>
              <input
                v-model="upstreamForms[u.key]!.name"
                type="text"
                class="input"
                :placeholder="t('admin.quotaKeys.upstream.namePlaceholder')"
                :data-testid="`upstream-name-${u.key}`"
              />
            </div>
            <div>
              <label class="mb-1 block text-xs font-medium text-gray-700 dark:text-gray-300">
                {{ t('admin.quotaKeys.upstream.renewalUrl') }}
              </label>
              <input
                v-model="upstreamForms[u.key]!.renewal_url"
                type="text"
                class="input"
                :placeholder="t('admin.quotaKeys.upstream.renewalUrlPlaceholder')"
                :data-testid="`upstream-renewal-${u.key}`"
              />
            </div>
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.quotaKeys.upstream.notes') }}
            </label>
            <TextArea
              v-model="upstreamForms[u.key]!.notes"
              :rows="2"
              :placeholder="t('admin.quotaKeys.upstream.notesPlaceholder')"
              :data-testid="`upstream-notes-${u.key}`"
            />
          </div>
          <div class="flex items-center justify-end gap-2">
            <span v-if="upstreamSavingKey === u.key" class="text-xs text-gray-400">{{ t('common.saving') }}</span>
            <button
              class="btn btn-primary"
              :disabled="upstreamSavingKey === u.key"
              :data-testid="`upstream-save-${u.key}`"
              @click="saveUpstream(u.key)"
            >
              {{ t('admin.quotaKeys.upstream.save') }}
            </button>
          </div>
        </li>
      </ul>

      <template #footer>
        <div class="flex items-center justify-end">
          <button class="btn btn-secondary" @click="showUpstreamDialog = false">
            {{ t('common.close') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <!-- 用量查询（D2-a probe_quota）结果 -->
    <BaseDialog
      :show="showProbeDialog"
      :title="t('admin.quotaKeys.probe.title')"
      width="normal"
      @close="showProbeDialog = false"
    >
      <div v-if="probing" class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">
        {{ t('admin.quotaKeys.probe.probing') }}
      </div>
      <dl v-else-if="probeResult" class="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2">
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.group') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.group || t('admin.quotaKeys.na') }}</dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.status') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.status_code }}</dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.used') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.used }}</dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.max') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.max }}</dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.deviceCount') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.device_count }}</dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.deviceMaxCount') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.device_max_count }}</dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.expiredAt') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">
            {{ probeResult.expired_at ? formatDateTime(probeResult.expired_at) : t('admin.quotaKeys.expiryUnknown') }}
          </dd>
        </div>
        <div>
          <dt class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
            {{ t('admin.quotaKeys.probe.accountsWritten') }}
          </dt>
          <dd class="mt-0.5 text-sm text-gray-900 dark:text-gray-100">{{ probeResult.accounts_written }}</dd>
        </div>
      </dl>
      <!-- P1-B3 两层额度语义：本弹窗整体是 key 级额度快照（总额度/期限/设备/取号结果），
           与账号层余额分属两层，不在账号层复制展示。 -->
      <p v-if="probeResult" class="mt-3 text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.quotaKeys.probe.keyLevelHint') }}
      </p>
      <!-- §25.6⑤：probe 一并回传**已提交**快照投影（与列表/详情/账号页同源同值），
           不是把本次 lookup 结果当已落库事实；无快照/失败按同一渲染纪律处理。 -->
      <div v-if="probeResult?.usage" class="mt-2 border-t border-gray-200 pt-2 dark:border-dark-600">
        <QuotaKeyUsageCell :usage="probeResult.usage" />
      </div>
      <p v-if="!probeResult && probeError" class="text-sm text-red-600 dark:text-red-400">{{ probeError }}</p>

      <template #footer>
        <div class="flex items-center justify-end">
          <button class="btn btn-secondary" @click="showProbeDialog = false">{{ t('common.close') }}</button>
        </div>
      </template>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatDateTime, resolveExpiryDisplay } from '@/utils/format'
import { formatQuotaKeyError } from '@/utils/quotaKeyError'
import { adminAPI } from '@/api/admin'
import {
  listUpstreams,
  updateUpstream
} from '@/api/admin/quotaKeys'
import type {
  QuotaKey,
  QuotaKeyDetail,
  CreateQuotaKeyRequest,
  UpdateQuotaKeyRequest,
  ProbeQuotaResponse,
  QuotaUpstream
} from '@/api/admin/quotaKeys'
import type { Column } from '@/components/common/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import TextArea from '@/components/common/TextArea.vue'
import Icon from '@/components/icons/Icon.vue'
import QuotaKeyUsageCell from '@/components/quotaKeys/QuotaKeyUsageCell.vue'
import Pagination from '@/components/common/Pagination.vue'
import PriorityStepper from '@/components/common/PriorityStepper.vue'
// 账号编辑复用账号管理页的现成表单弹窗（不复制一份账号表单，避免双份漂移）。
// 直接引用单文件而非 @/components/account 桶文件：桶会连带引入全部账号组件，
// 激活码页只需要这一个弹窗。
import EditAccountModal from '@/components/account/EditAccountModal.vue'
// 模型限制（白名单/映射）：复用在账号管理页落地的 ModelWhitelistSelector（支持 customSync），
// 以及 useModelWhitelist 的辅助函数（构建 model_mapping 对象、按平台取模型、通配符校验、解析回填）。
import ModelWhitelistSelector from '@/components/account/ModelWhitelistSelector.vue'
import {
  buildModelMappingObject,
  getPresetMappingsByPlatform,
  isValidWildcardPattern,
  splitModelMappingObject
} from '@/composables/useModelWhitelist'
// D2a：分组选择器（复用账号管理页的 GroupSelector）与按模型家族推导候选原生平台。
import GroupSelector from '@/components/common/GroupSelector.vue'
import { CURSOR_NATIVE_GROUP_PLATFORMS, inferGroupPlatformsFromModels } from '@/constants/platforms'
import type { Account, AdminGroup, Proxy as AccountProxy, GroupPlatform } from '@/types'

const { t } = useI18n()
const appStore = useAppStore()
const router = useRouter()

// --- 上游续费/采购入口（CARD-KC-09）-------------------------------------
// kn-cy.com 已有公开商铺，直达产品列表页；ShareAI 暂无公开商铺，
// 菜单项保留为占位（配置项留待后端/设置页落地），点击给出友好提示而非静默失败。
const KNCY_STORE_URL = 'https://shop.kn-cy.com/products'
const upstreamStoreOpen = ref(false)
const upstreamStoreRef = ref<HTMLElement | null>(null)
const showShareaiPlaceholder = ref(false)

function openShareaiPlaceholder() {
  upstreamStoreOpen.value = false
  // 配置项接入后此处可直接跳转；当前无链接，必须明确告知用户。
  showShareaiPlaceholder.value = true
}

function handleUpstreamStoreClickOutside(event: MouseEvent) {
  const target = event.target as Node
  if (upstreamStoreRef.value && !upstreamStoreRef.value.contains(target)) {
    upstreamStoreOpen.value = false
  }
}

const keys = ref<QuotaKey[]>([])
const loading = ref(false)

// --- 上游注册表（CARD-KC-18）-------------------------------------------------
// 拉取 quota_upstreams 列表，按 quota_key.upstream_key 匹配上游显示名；
// 不持有任何凭据（上游条目本身也不含凭据）。
const upstreams = ref<QuotaUpstream[]>([])
const upstreamLoading = ref(false)
const showUpstreamDialog = ref(false)
const upstreamSavingKey = ref<string | null>(null)
// 编辑表单：按上游 key 存一份可编辑副本（name / renewal_url / notes）。
const upstreamForms = reactive<Record<string, { name: string; renewal_url: string; notes: string }>>({})

const busyId = ref<number | null>(null)
const saving = ref(false)
const sweeping = ref(false)

// D2-c：启用 / 停用（status.active ↔ status.disabled）二次确认状态。
const statusTogglingId = ref<number | null>(null)
const statusConfirm = reactive<{ open: boolean; row: QuotaKey | null; next: 'active' | 'disabled' }>({
  open: false, row: null, next: 'active'
})

const showFormDialog = ref(false)
const showDetailDialog = ref(false)
const showDeleteDialog = ref(false)
const showSweepDialog = ref(false)
// D2-a：用量查询弹窗。
const showProbeDialog = ref(false)
const probing = ref(false)
const probeResult = ref<ProbeQuotaResponse | null>(null)
const probeError = ref('')
const editing = ref<QuotaKey | null>(null)
const deleting = ref<QuotaKey | null>(null)
const detail = ref<QuotaKeyDetail | null>(null)
// 详情弹窗账号列表分页状态（后端默认 page_size=20；翻页时重新拉取详情）
const detailPage = ref(1)
const detailPageSize = ref(20)
const formError = ref('')

// --- 账号编辑（详情弹窗内按行打开账号管理页的 EditAccountModal）------------
// 详情列表里的账号是脱敏最小投影（只有 id/name/platform/status），编辑需要完整账号，
// 故点「编辑」时先按 id 取全量账号再打开弹窗。
const showAccountEditDialog = ref(false)
// 正在加载详情的账号 id（非空表示该行处于 loading 态，行内按钮禁用并转圈）
const accountEditLoadingId = ref<number | null>(null)
// 被编辑的完整账号（null = 未打开编辑弹窗）
const editingAccount = ref<Account | null>(null)
// EditAccountModal 必需的 proxies / groups：首次打开编辑前懒加载一次并复用
const accountProxies = ref<AccountProxy[]>([])
const accountGroups = ref<AdminGroup[]>([])
let accountEditRefsLoaded = false
let accountEditRefsPromise: Promise<void> | null = null
// --- 分组选择器（D2a：激活码绑定分组）---------------------------------
// 直接复用 accountGroups：该 ref 既供详情弹窗内账号编辑的 GroupSelector 使用，
// 也供本页创建/编辑弹窗的分组选择器使用；用 Promise 缓存懒加载一次，失败不缓存。
let groupsLoaded = false
let groupsPromise: Promise<void> | null = null
const groupTouched = ref(false)

const form = reactive({
  code: '',
  expires_at: '',
  notes: '',
  // 分组绑定（D2a）：激活码绑定到哪些分组；创建态默认 []（即不绑定），
  // 编辑态回填 row.group_ids；未触碰时不随更新请求提交（保持后端既有绑定）。
  group_ids: [] as number[],
  // 激活码优先级：数值越小越优先，与账号 priority 同一语义。该码名下账号的优先级
  // 跟随此值（后端拉号时写入、改优先级时同事务同步）。
  priority: 1,
  // 所属上游：创建态随请求提交；空串在提交时会被拦截（失败关闭，不再回落 shareai 默认）。
  upstream_key: '',
  // 并发数（B 批上移，不含余额查询）：null / 空 = 沿用账号列默认 3；正整数（≥ 0）同步名下账号。
  concurrency: null as number | null,
  // 负载因子（B 批上移）：null / 空 = 清除；正整数 1..10000。
  load_factor: null as number | null,
  // 计费倍率（B 批上移）：≥ 0，默认 1，允许 0；同步名下账号的计费倍率。
  rate_multiplier: 1
})

// --- 模型限制（激活码级权威值，与 credentials.model_mapping 同构）----------------
// Create 与 Edit 共用弹框，均渲染白名单/映射双模式；打开弹窗时重置 touched=false，
// 用户切换模式 / 改 allowedModels / 改 modelMappings 置 true（用于「禁止空模型落库」判定）。
const modelRestrictionMode = ref<'whitelist' | 'mapping'>('whitelist')
const allowedModels = ref<string[]>([])
const modelMappings = ref<{ from: string; to: string }[]>([])
const modelRestrictionTouched = ref(false)

// 编辑态才暴露「同步上游支持的模型」自定义接口（码级模型限制预览，不改写单个账号 DB）。
const customSyncProp = computed(() =>
  editing.value ? () => adminAPI.quotaKeys.syncUpstreamModels(editing.value!.id) : undefined
)

// --- D2a：分组选择器候选推导 --------------------------------------------
// 当前激活码「选择的模型」集合：白名单模式取 allowedModels，映射模式取各 from 字段，
// 两者去重；用于驱动 GroupSelector 的 accountModels（按模型家族过滤 composite 聚合组）。
const quotaKeySelectedModels = computed<string[]>(() => {
  const set = new Set<string>()
  for (const m of allowedModels.value) {
    const v = m.trim()
    if (v) set.add(v)
  }
  for (const m of modelMappings.value) {
    const v = m.from.trim()
    if (v) set.add(v)
  }
  return Array.from(set)
})
// 候选原生平台：由所选模型推导；推导为空（尚未选模型）时回落 cursor 全量原生平台，
// 保证创建态默认即可选中 cursor 全家族分组。
const quotaKeyNativePlatforms = computed<GroupPlatform[]>(() => {
  const inferred = inferGroupPlatformsFromModels(quotaKeySelectedModels.value)
  return inferred.length > 0 ? inferred : [...CURSOR_NATIVE_GROUP_PLATFORMS]
})

// 映射 Tab 的「快速映射」预设家族平台：取所选模型推导出的第一个家族平台，
// 推导为空（尚未选模型）时回落 anthropic。
const quotaKeyFamilyPlatform = computed<string>(() => {
  const inferred = inferGroupPlatformsFromModels(quotaKeySelectedModels.value)
  return inferred.length > 0 ? inferred[0] : 'anthropic'
})

// 「快速映射」预设按钮组：按家族平台取预设映射，已存在于 modelMappings 或 allowedModels
// 的 from 跳过（避免重复添加 / 与白名单恒等条目冲突）。
const quickMappingPresets = computed(() => {
  const existingFroms = new Set<string>()
  for (const m of modelMappings.value) {
    const v = m.from.trim()
    if (v) existingFroms.add(v)
  }
  for (const m of allowedModels.value) {
    const v = m.trim()
    if (v) existingFroms.add(v)
  }
  return getPresetMappingsByPlatform(quotaKeyFamilyPlatform.value).filter((p) =>
    !existingFroms.has(p.from.trim())
  )
})

// 一键添加预设映射：追加到映射列表并标记已触碰模型限制。
function addQuickMapping(from: string, to: string) {
  modelMappings.value.push({ from, to })
  modelRestrictionTouched.value = true
}

/**
 * §0.1 #7 折叠裁定：账号列表默认折叠。
 * 展开状态按激活码 id 记忆在组件作用域内，不随列表刷新/重挂载重置；
 * 未显式展开过的 id 即视为折叠，因此默认值天然是「折叠」。
 */
const expandedAccounts = reactive(new Set<number>())

const columns = computed<Column[]>(() => [
  { key: 'code_masked', label: t('admin.quotaKeys.columns.maskedCode'), sortable: false },
  // CARD-KC-18：按上游区分显示（显示名 + 标识）；上游条目缺失时回落显示原始 upstream_key。
  { key: 'upstream', label: t('admin.quotaKeys.columns.upstream'), sortable: false },
  // 来源额度快照列（§25.6⑦）：列名沿用「激活码用量」口径，数值为一层（key/来源）快照。
  { key: 'usage', label: t('admin.quotaKeys.probe.title'), sortable: false },
  { key: 'status', label: t('admin.quotaKeys.columns.status'), sortable: false },
  { key: 'priority', label: t('admin.quotaKeys.columns.priority'), sortable: false },
  { key: 'exhausted', label: t('admin.quotaKeys.columns.exhausted'), sortable: false },
  { key: 'account_count', label: t('admin.quotaKeys.columns.accountCount'), sortable: false },
  { key: 'working_count', label: t('admin.quotaKeys.columns.workingCount'), sortable: false },
  { key: 'expires_at', label: t('admin.quotaKeys.columns.expiresAt'), sortable: false },
  { key: 'last_refresh_error', label: t('admin.quotaKeys.columns.lastRefreshError'), sortable: false },
  { key: 'actions', label: t('admin.quotaKeys.columns.actions'), sortable: false }
])

function statusLabel(row: QuotaKey | QuotaKeyDetail): string {
  if (row.exhausted) return t('admin.quotaKeys.status.exhausted')
  switch (row.status) {
    case 'active':
      return t('admin.quotaKeys.status.active')
    case 'expired':
      return t('admin.quotaKeys.status.expired')
    case 'disabled':
      return t('admin.quotaKeys.status.inactive')
    default:
      return row.status || t('admin.quotaKeys.status.unknown')
  }
}

function statusBadgeClass(row: QuotaKey | QuotaKeyDetail): string {
  if (row.exhausted) return 'bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300'
  switch (row.status) {
    case 'active':
      return 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-300'
    case 'expired':
      return 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300'
    case 'disabled':
      return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
    default:
      return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
  }
}

// 到期列渲染：按整行 status + 到期值收敛语义（C-U7-B 展示收敛）。
// - status=expired 且无时间 → "已过期"
// - active + 有时间 → 具体日期
// - active + 无时间（含空串） → "到期时间未知"
//   注：激活码不存在永不过期形态，上游给的码必然带期限；后端对 NULL 也只是
//   "无期限约束"，不是业务承诺。原 never 分支（空串 ⇒ 永不过期）已删除。
function formatExpiresAt(row: QuotaKey | QuotaKeyDetail): string {
  if (row.status === 'expired' && (row.expires_at === null || row.expires_at === undefined)) {
    return t('admin.quotaKeys.status.expired')
  }
  const r = resolveExpiryDisplay(row.expires_at)
  if (r.status === 'unknown') return t('admin.quotaKeys.expiryUnknown')
  return r.text
}

// QKERR-C：最近错误展示层格式化（剥离内部 shareai 前缀 + 刷新上限压缩为短句）。
// 列表单元格与详情弹窗共用同一函数，确保无第二套文本解析；
// 空值/空白返回空串，由调用处回退到 na 文案。
function formatLastRefreshError(raw: string | null | undefined): string {
  return formatQuotaKeyError(raw, t)
}

function isAccountsExpanded(id: number): boolean {
  return expandedAccounts.has(id)
}

function toggleAccounts(id: number): void {
  if (expandedAccounts.has(id)) {
    expandedAccounts.delete(id)
  } else {
    expandedAccounts.add(id)
  }
}

async function load() {
  loading.value = true
  try {
    const data = await adminAPI.quotaKeys.list()
    keys.value = data.items
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.loadError')))
  } finally {
    loading.value = false
  }
}

// 按上游标识建索引：quota_key.upstream_key → 上游条目。
const upstreamByKey = computed<Record<string, QuotaUpstream>>(() => {
  const map: Record<string, QuotaUpstream> = {}
  for (const u of upstreams.value) {
    map[u.key] = u
  }
  return map
})

// 激活码行展示的上游标签：显示名 (标识)；上游条目缺失但 upstream_key 存在时回落显示
// 原始标识；后端尚未回填 upstream_key 时返回空串（由模板回落到占位符，不空白）。
function upstreamLabel(row: QuotaKey): string {
  const key = row.upstream_key
  if (!key) return ''
  const u = upstreamByKey.value[key]
  if (u) return `${u.name} (${u.key})`
  return key
}

// 上游非空续费链接（KC-18 条目级链接，与 KC-09 既有 kn-cy/ShareAI 静态入口共存）。
function upstreamRenewalUrl(row: QuotaKey): string {
  const key = row.upstream_key
  if (!key) return ''
  const u = upstreamByKey.value[key]
  return u?.renewal_url || ''
}

async function loadUpstreams() {
  upstreamLoading.value = true
  try {
    const data = await listUpstreams()
    upstreams.value = data.items
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.upstream.loadError')))
  } finally {
    upstreamLoading.value = false
  }
}

// 打开上游编辑弹窗：用当前上游条目回填可编辑表单副本。
function openUpstreamDialog() {
  for (const u of upstreams.value) {
    upstreamForms[u.key] = {
      name: u.name,
      renewal_url: u.renewal_url ?? '',
      notes: u.notes ?? ''
    }
  }
  showUpstreamDialog.value = true
}

// 保存单个上游条目：仅提交 name / renewal_url / notes（key 与 type 不可改）。
// 成功后更新本地条目并刷新激活码列表，使按上游区分的显示名立即生效（含激活码列表）。
async function saveUpstream(key: string) {
  const form = upstreamForms[key]
  if (!form || !form.name.trim()) {
    appStore.showError(t('admin.quotaKeys.upstream.namePlaceholder'))
    return
  }
  upstreamSavingKey.value = key
  try {
    const payload = {
      name: form.name.trim(),
      renewal_url: form.renewal_url.trim() ? form.renewal_url.trim() : null,
      notes: form.notes.trim() ? form.notes.trim() : null
    }
    const updated = await updateUpstream(key, payload)
    const idx = upstreams.value.findIndex((u) => u.key === key)
    if (idx >= 0) {
      upstreams.value[idx] = updated
    }
    appStore.showSuccess(t('admin.quotaKeys.upstream.saved'))
    // 刷新激活码列表，使按上游区分的显示名立刻生效。
    await load()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.upstream.saveError')))
  } finally {
    upstreamSavingKey.value = null
  }
}

function openCreateDialog() {
  editing.value = null
  // D2a：懒加载分组候选（供 GroupSelector 显示），失败不阻断弹窗打开。
  void ensureGroups()
  form.code = ''
  form.expires_at = ''
  form.notes = ''
  form.priority = 1
  // 重置为空串：不默认选第一个上游；提交时若仍为空串会被拦截（失败关闭，不再回落 shareai）。
  form.upstream_key = ''
  // B 批上移字段：并发/负载因子默认空（沿用默认/清除），计费倍率默认 1。
  form.concurrency = null
  form.load_factor = null
  form.rate_multiplier = 1
  // D2a：创建态默认不绑定分组、未触碰。
  form.group_ids = []
  groupTouched.value = false
  // 模型限制：创建态默认白名单模式、空列表、未触碰。
  modelRestrictionMode.value = 'whitelist'
  allowedModels.value = []
  modelMappings.value = []
  modelRestrictionTouched.value = false
  formError.value = ''
  showFormDialog.value = true
}

function openEditDialog(row: QuotaKey) {
  editing.value = row
  // D2a：懒加载分组候选（供 GroupSelector 显示），失败不阻断弹窗打开。
  void ensureGroups()
  form.code = ''
  form.expires_at = row.expires_at ? toLocalInputValue(row.expires_at) : ''
  form.notes = row.notes ?? ''
  form.priority = row.priority ?? 1
  // B 批上移字段回填。
  form.concurrency = row.concurrency ?? null
  form.load_factor = row.load_factor ?? null
  form.rate_multiplier = row.rate_multiplier ?? 1
  // 模型限制回填（方案 §3 C1：白名单 + 映射共存，不再互抹）：
  // 用 splitModelMappingObject 把存量 model_mapping 拆成「恒等条目 → 白名单」与
  // 「非恒等条目 → 映射」两部分，两类同时保留。
  // 模式判定：仅当存在非恒等映射且没有任何恒等条目时才切到 mapping，否则保持 whitelist
  // （whitelist 模式下也能渲染映射 Tab，二者共存）。
  const parsed = splitModelMappingObject(row.model_mapping as Record<string, unknown> | null)
  allowedModels.value = parsed.allowedModels
  modelMappings.value = parsed.modelMappings
  modelRestrictionMode.value =
    parsed.modelMappings.length > 0 && parsed.allowedModels.length === 0 ? 'mapping' : 'whitelist'
  modelRestrictionTouched.value = false
  // D2a：编辑态回填已有分组绑定（row.group_ids 缺省视为未绑定），未触碰。
  form.group_ids = [...(row.group_ids ?? [])]
  groupTouched.value = false
  formError.value = ''
  showFormDialog.value = true
}

function closeFormDialog() {
  showFormDialog.value = false
  editing.value = null
  // F1：明文激活码用完即弃，绝不驻留前端状态
  form.code = ''
  // 关闭弹窗一并清空模型限制草稿，避免下次打开残留。
  modelRestrictionMode.value = 'whitelist'
  allowedModels.value = []
  modelMappings.value = []
  modelRestrictionTouched.value = false
  // D2a：关闭弹窗清空分组绑定草稿，避免下次打开残留。
  form.group_ids = []
  groupTouched.value = false
}

function toLocalInputValue(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

// 将表单里的并发/负载因子空串归一为 null（v-model.number 清空输入会得到 ''）。
function normalizeInt(v: unknown): number | null {
  if (v === null || v === undefined || v === '') return null
  const n = Number(v)
  return Number.isNaN(n) ? null : n
}

// 计费倍率：空/缺省回落默认 1；非有限数回落 1（非有限值会在 submitForm 校验处拦截）。
// v-model.number 清空输入会得到 ''，故按 unknown 处理以防类型比较报错。
function rateMultiplierValue(): number {
  const v = form.rate_multiplier as unknown
  if (v === '' || v === null || v === undefined) return 1
  const n = Number(v)
  return Number.isFinite(n) ? n : 1
}

// 映射模式 from 字段通配符校验（* 只能放在末尾；to 不能含 *），用于 UI 实时提示。
const invalidWildcardPatterns = computed(() => {
  const bad: string[] = []
  for (const m of modelMappings.value) {
    const from = m.from.trim()
    const to = m.to.trim()
    if (from && !isValidWildcardPattern(from)) bad.push(from)
    if (to.includes('*')) bad.push(`${from}→${to}`)
  }
  return Array.from(new Set(bad))
})

function addModelMapping() {
  modelMappings.value.push({ from: '', to: '' })
  modelRestrictionTouched.value = true
}

function removeModelMapping(index: number) {
  modelMappings.value.splice(index, 1)
  modelRestrictionTouched.value = true
}

function buildPayload(modelMapping?: Record<string, string> | null): CreateQuotaKeyRequest | UpdateQuotaKeyRequest {
  const expiresAt = form.expires_at ? new Date(form.expires_at).toISOString() : null
  const payload: Record<string, unknown> = {
    expires_at: expiresAt,
    priority: form.priority,
    // B 批上移字段：空归 null；计费倍率默认 1。
    concurrency: normalizeInt(form.concurrency),
    load_factor: normalizeInt(form.load_factor),
    rate_multiplier: rateMultiplierValue()
  }
  payload.notes = form.notes.trim() ? form.notes.trim() : null
  // 创建态随请求提交所选上游；编辑态不放（update 接口无该字段，放了亦被忽略且是噪声）。
  if (!editing.value) {
    payload.upstream_key = form.upstream_key
  }
  // 仅在调用方明确要发送模型限制时带上（避免编辑态「仅改备注/优先级」时覆盖码级模型）。
  if (modelMapping !== undefined) {
    payload.model_mapping = modelMapping
  }
  // D2a：分组绑定提交规则（与模型限制同构的「三态」裁定）：
  // - 创建态（editing 为空）：必带 group_ids（默认 [] 即不绑定任何分组）；
  // - 编辑态：仅当 groupTouched（用户改过分组）时才发送，未触碰则不覆盖后端既有绑定，
  //   以免「仅改备注/优先级」误清空分组。
  if (!editing.value || groupTouched.value) {
    payload.group_ids = form.group_ids
  }
  return payload as UpdateQuotaKeyRequest
}

async function submitForm() {
  if (!editing.value && !form.code.trim()) {
    formError.value = t('admin.quotaKeys.codePlaceholder')
    return
  }
  // 优先级：正整数，数值越小越优先（与账号 priority 同一口径）。
  if (!Number.isInteger(form.priority) || form.priority < 1) {
    formError.value = t('admin.quotaKeys.priorityInvalid')
    return
  }
  // 创建态必须选定所属上游：不选即拦截，失败关闭。
  // 上游列表为空（加载失败/被软删）时同样拦截——上游归属无法确认时不得静默落默认 'shareai'，
  // 否则一张 kn-cy 卡密会被送给 ShareAI 拉号接口。
  if (!editing.value && !form.upstream_key) {
    formError.value = t('admin.quotaKeys.upstream.selectRequired')
    return
  }
  // 并发数：非空时必须为 ≥ 0 的整数。
  const concurrencyVal = normalizeInt(form.concurrency)
  if (concurrencyVal !== null && (!Number.isInteger(concurrencyVal) || concurrencyVal < 0)) {
    formError.value = t('admin.quotaKeys.concurrencyInvalid')
    return
  }
  // 负载因子：非空时必须为 1..10000 的整数。
  const loadFactorVal = normalizeInt(form.load_factor)
  if (
    loadFactorVal !== null &&
    (!Number.isInteger(loadFactorVal) || loadFactorVal < 1 || loadFactorVal > 10000)
  ) {
    formError.value = t('admin.quotaKeys.loadFactorInvalid')
    return
  }
  // 计费倍率：必须为 ≥ 0 的有限数。
  const rateVal = rateMultiplierValue()
  if (!Number.isFinite(rateVal) || rateVal < 0) {
    formError.value = t('admin.quotaKeys.rateMultiplierInvalid')
    return
  }

  // 模型限制：计算映射对象并应用「禁止空模型落库」裁定（用户裁定 #3）。
  // 统一用 'combined'：白名单恒等条目 + 映射条目共存，不再互抹（方案 §3 C1）。
  const modelMapping = buildModelMappingObject(
    'combined',
    allowedModels.value,
    modelMappings.value
  )

  // 失败关闭（外审 R2a must_fix）：禁止静默删减存量条目。
  // buildModelMappingObject('combined') 会跳过三类条目：白名单里含 * 的、
  // from 通配符不在末尾的、to 含 * 的。后端 normalizeQuotaKeySettings 只做去空白，
  // 存量里可能存在这三类，若不拦截保存会把它们静默删掉。
  // 源键集合（表单里所有会被写进 model_mapping 的请求侧名字）。
  const sourceKeys = new Set<string>()
  for (const m of allowedModels.value) {
    const v = m.trim()
    if (v) sourceKeys.add(v)
  }
  for (const m of modelMappings.value) {
    const v = m.from.trim()
    if (v && m.to.trim()) sourceKeys.add(v)
  }
  const builtKeys = new Set(Object.keys(modelMapping ?? {}))
  const dropped = [...sourceKeys].filter((k) => !builtKeys.has(k))
  if (dropped.length > 0) {
    formError.value = t('admin.quotaKeys.modelEntryNotPersistable', { models: dropped.join(', ') })
    return
  }
  let sendModelMapping = false
  if (editing.value) {
    const orig = editing.value.model_mapping
    const origHasMapping = !!orig && Object.keys(orig).length > 0
    // 编辑态：原已有模型限制，或用户已触碰模型，且算出来为空 → 阻断保存。
    if ((origHasMapping || modelRestrictionTouched.value) && !modelMapping) {
      formError.value = t('admin.quotaKeys.modelRestrictionRequired')
      return
    }
    // 仅当原 model_mapping 为 null/未设置 且 未触碰模型时，不发送（保持原状，不阻塞仅改备注/优先级）。
    sendModelMapping = !(orig == null && !modelRestrictionTouched.value)
  } else {
    // 创建态：未触碰模型 → 不发送（沿用上游默认实供清单）。
    if (modelRestrictionTouched.value) {
      // 触碰后算出为空 → 阻断保存（用户选过/切到映射/删空都不允许落空）。
      if (!modelMapping) {
        formError.value = t('admin.quotaKeys.modelRestrictionRequired')
        return
      }
      sendModelMapping = true
    } else {
      sendModelMapping = false
    }
  }

  saving.value = true
  try {
    const payload = buildPayload(sendModelMapping && modelMapping ? modelMapping : undefined)
    if (editing.value) {
      await adminAPI.quotaKeys.update(editing.value.id, payload as UpdateQuotaKeyRequest)
      appStore.showSuccess(t('admin.quotaKeys.updated'))
    } else {
      const fullPayload = { ...(payload as CreateQuotaKeyRequest), code: form.code.trim() }
      await adminAPI.quotaKeys.create(fullPayload)
      appStore.showSuccess(t('admin.quotaKeys.created'))
    }
    closeFormDialog()
    await load()
  } catch (err: unknown) {
    const message = extractApiErrorMessage(
      err,
      editing.value ? t('admin.quotaKeys.updateError') : t('admin.quotaKeys.createError')
    )
    formError.value = message
    appStore.showError(message)
  } finally {
    saving.value = false
  }
}

async function openDetail(row: QuotaKey) {
  showDetailDialog.value = true
  detail.value = null
  detailPage.value = 1
  try {
    detail.value = await adminAPI.quotaKeys.get(row.id, detailPage.value, detailPageSize.value)
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.detailLoadError')))
    showDetailDialog.value = false
  }
}

// 详情弹窗 → 账号管理页：带 quota_key_id 深链筛选，账号页自动应用该筛选。
// 跳转后关闭详情弹窗，但不清空 detail（沿用详情弹窗既有关闭方式，避免关闭瞬间内容闪烁）。
function goToAllAccounts() {
  if (!detail.value) return
  const quotaKeyID = String(detail.value.id)
  showDetailDialog.value = false
  void router.push({
    path: '/admin/accounts',
    query: { quota_key_id: quotaKeyID }
  })
}

// 详情弹窗翻页：保持弹窗打开，仅重拉当前激活码的该页账号。
async function handleDetailPageChange(nextPage: number) {
  if (!detail.value) return
  const id = detail.value.id
  try {
    const next = await adminAPI.quotaKeys.get(id, nextPage, detailPageSize.value)
    detail.value = next
    detailPage.value = nextPage
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.detailLoadError')))
  }
}

// --- 账号编辑（复用账号管理页 EditAccountModal）----------------------------
// 详情弹窗里的账号是脱敏最小投影（只有 id/name/platform/status），不含
// credentials / extra / group_ids，而 EditAccountModal 的表单要按完整账号回填，
// 因此点「编辑」时先 GET /admin/accounts/:id 取全量账号再打开弹窗。
async function openAccountEditor(accountId: number) {
  if (accountEditLoadingId.value !== null) return
  accountEditLoadingId.value = accountId
  try {
    // proxies / groups 供 GroupSelector 显示挂组选择，与账号详情并行加载；
    // 两者只随弹窗打开时才需要，故懒加载并缓存（同一页面生命周期内复用）。
    const [full] = await Promise.all([
      adminAPI.accounts.getById(accountId),
      ensureAccountEditorRefs()
    ])
    editingAccount.value = full
    showAccountEditDialog.value = true
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.accountEdit.loadError')))
  } finally {
    accountEditLoadingId.value = null
  }
}

// 懒加载 EditAccountModal 必需的 proxies / groups（AccountsView 同样依赖这两份数据）。
// 用 Promise 缓存避免并发点击重复请求；失败不阻断账号编辑本身（分组/代理选择器会空），
// 且不缓存失败结果，下次点「编辑」可重试。
async function ensureAccountEditorRefs(): Promise<void> {
  if (accountEditRefsLoaded) return
  if (!accountEditRefsPromise) {
    accountEditRefsPromise = (async () => {
      const [proxiesResult, groupsResult] = await Promise.allSettled([
        adminAPI.proxies.getAll(),
        adminAPI.groups.getAll()
      ])
      if (proxiesResult.status === 'fulfilled') {
        accountProxies.value = proxiesResult.value
      } else {
        console.error('Failed to load proxies:', proxiesResult.reason)
      }
      if (groupsResult.status === 'fulfilled') {
        accountGroups.value = groupsResult.value
      } else {
        console.error('Failed to load groups:', groupsResult.reason)
      }
      // 两份都拿到才算加载完成（失败下次可重试）。
      accountEditRefsLoaded =
        proxiesResult.status === 'fulfilled' && groupsResult.status === 'fulfilled'
    })().catch((err: unknown) => {
      // 整体失败（如接口形状不符）时不缓存失败态，允许下次重试。
      accountEditRefsPromise = null
      console.error('Failed to load account editor dependencies:', err)
    })
  }
  await accountEditRefsPromise
}

// D2a：懒加载分组候选（供创建/编辑弹窗的 GroupSelector 显示）。
// 与 ensureAccountEditorRefs 同源 adminAPI.groups.getAll()（写入同一个 accountGroups ref），
// 但独立缓存以避免与账号编辑弹窗的加载互相耦合。
// 失败处理（QKG-D2-FIX）：捕获方法缺失/网络拒绝等一切失败，候选置为空数组；
// groupsLoaded 保持 falsy（失败的 Promise 不永久缓存），groupsPromise 清空以便下次打开弹窗可重试；
// 按组件既有机制（appStore.showError）报错一次；不引入全局兜底、新依赖或新错误通道。
// 用 IIFE 包裹 try/catch：同步抛错（adminAPI.groups 未提供）与异步 reject 都会被捕获，
// 返回的 Promise 始终 resolve，避免 void ensureGroups() 产生未捕获 Promise 异常。
async function ensureGroups() {
  if (groupsLoaded) return
  if (!groupsPromise) {
    groupsPromise = (async () => {
      try {
        const list = await adminAPI.groups.getAll()
        accountGroups.value = list
        groupsLoaded = true
      } catch (err: unknown) {
        accountGroups.value = []
        groupsLoaded = false
        groupsPromise = null
        appStore.showError(extractApiErrorMessage(err, '分组候选加载失败，可稍后重试'))
      }
    })()
  }
  await groupsPromise
}

// 账号保存成功：账号 status / schedulable 可能已变 → 重拉激活码详情（账号行）与列表
// （working_count = 可调度且 active 的账号数，随账号状态变化），与本页 handleProbe 同一口径。
async function handleAccountUpdated() {
  const id = detail.value?.id
  await load()
  if (id === undefined) return
  try {
    detail.value = await adminAPI.quotaKeys.get(id, detailPage.value, detailPageSize.value)
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.detailLoadError')))
  }
}

// 关闭编辑弹窗：只关内层，详情弹窗保持打开。
function closeAccountEditor() {
  showAccountEditDialog.value = false
  editingAccount.value = null
}

// --- 优先级编辑 -----------------------------------------------------------
// 交互由公共组件 PriorityStepper 提供（与号池账号优先级同一实现），这里只负责落库：
// 只发 priority 一个字段，后端在同一事务内把该码名下未删除账号的 priority 一并改写。
async function savePriority(row: QuotaKey, next: number) {
  const updated = await adminAPI.quotaKeys.update(row.id, { priority: next })
  const synced = updated?.account_count ?? row.account_count ?? 0
  appStore.showSuccess(
    t('admin.quotaKeys.prioritySaved', { priority: next, count: synced })
  )
  await load()
}

async function handleRefresh(row: QuotaKey) {
  if (busyId.value !== null) return
  busyId.value = row.id
  try {
    await adminAPI.quotaKeys.refresh(row.id)
    appStore.showSuccess(t('admin.quotaKeys.refreshed'))
    await load()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.refreshError')))
  } finally {
    busyId.value = null
  }
}

async function handleClearExhausted(row: QuotaKey) {
  if (busyId.value !== null) return
  busyId.value = row.id
  try {
    await adminAPI.quotaKeys.clearExhausted(row.id)
    appStore.showSuccess(t('admin.quotaKeys.cleared'))
    await load()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.clearError')))
  } finally {
    busyId.value = null
  }
}

// 用量查询（D2-a probe_quota）：调后端 lookup 并弹窗展示 key_info 摘要。
async function handleProbe(row: QuotaKey) {
  if (busyId.value !== null) return
  busyId.value = row.id
  showProbeDialog.value = true
  probing.value = true
  probeResult.value = null
  probeError.value = ''
  try {
    const info = await adminAPI.quotaKeys.probeQuota(row.id)
    probeResult.value = info
    // O03：probe 已把 expires_at 写库，刷新列表与已开详情以反映最新事实源。
    await load()
    if (detail.value && detail.value.id === row.id) {
      detail.value = await adminAPI.quotaKeys.get(row.id, detailPage.value, detailPageSize.value)
    }
  } catch (err: unknown) {
    probeError.value = extractApiErrorMessage(err, t('admin.quotaKeys.probe.error'))
  } finally {
    probing.value = false
    busyId.value = null
  }
}

function openDeleteDialog(row: QuotaKey) {
  deleting.value = row
  showDeleteDialog.value = true
}

async function confirmDelete() {
  if (!deleting.value) return
  const target = deleting.value
  try {
    await adminAPI.quotaKeys.remove(target.id)
    appStore.showSuccess(t('admin.quotaKeys.deleted'))
    showDeleteDialog.value = false
    deleting.value = null
    await load()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.deleteError')))
  }
}

function openStatusConfirm(row: QuotaKey) {
  statusConfirm.row = row
  statusConfirm.next = row.status === 'disabled' ? 'active' : 'disabled'
  statusConfirm.open = true
}

async function confirmStatusToggle() {
  const row = statusConfirm.row
  if (!row) return
  statusTogglingId.value = row.id
  try {
    await adminAPI.quotaKeys.setStatus(row.id, statusConfirm.next)
    appStore.showSuccess(statusConfirm.next === 'active'
      ? t('admin.quotaKeys.enabled') : t('admin.quotaKeys.disabledMsg'))
    statusConfirm.open = false
    await load()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.statusToggleError')))
  } finally {
    statusTogglingId.value = null
  }
}

function openSweepDialog() {
  showSweepDialog.value = true
}

async function confirmSweep() {
  sweeping.value = true
  try {
    const res = await adminAPI.quotaKeys.sweepExpired()
    appStore.showSuccess(t('admin.quotaKeys.swept', { count: res.swept ?? 0 }))
    showSweepDialog.value = false
    await load()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.quotaKeys.sweepError')))
  } finally {
    sweeping.value = false
  }
}

onMounted(() => {
  void load()
  void loadUpstreams()
  document.addEventListener('click', handleUpstreamStoreClickOutside)
})

onBeforeUnmount(() => {
  document.removeEventListener('click', handleUpstreamStoreClickOutside)
})
</script>
