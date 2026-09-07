<template>
  <AppLayout>
    <div class="p-6">
      <div class="mb-6 flex items-start justify-between">
        <div>
          <h1 class="text-2xl font-bold">{{ t('admin.xianyu.inventory.title') }}</h1>
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.xianyu.inventory.description') }}</p>
        </div>
        <button class="btn btn-primary" @click="openEdit()">
          <Icon name="plus" size="sm" /> {{ t('admin.xianyu.inventory.addPool') }}
        </button>
      </div>

      <div class="overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-700">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-gray-200 bg-gray-50 text-left dark:border-dark-700 dark:bg-dark-800">
              <th class="px-4 py-2">{{ t('admin.xianyu.inventory.name') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.inventory.slug') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.inventory.cardSpec') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.inventory.remaining') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.inventory.delivered') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.inventory.used') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.inventory.disabled') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.inventory.lowStockThreshold') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.inventory.status') }}</th>
              <th class="px-4 py-2 text-right">{{ t('common.actions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="pool in pools" :key="pool.id" class="border-b border-gray-100 dark:border-dark-700">
              <td class="px-4 py-2 font-medium">{{ pool.name }}</td>
              <td class="px-4 py-2 text-gray-500">{{ pool.slug }}</td>
              <td class="px-4 py-2 text-xs text-gray-500">{{ cardSpecText(pool) }}</td>
              <td class="px-4 py-2">
                <span v-if="stockError" class="text-orange-500">{{ t('admin.xianyu.inventory.stockUnavailable') }}</span>
                <template v-else>
                  <span :class="isLowStock(pool) ? 'font-semibold text-red-600 dark:text-red-400' : ''">{{ remainingFor(pool) }}</span>
                  <span v-if="isLowStock(pool)" class="ml-1 rounded bg-red-100 px-1.5 py-0.5 text-xs text-red-600 dark:bg-red-900 dark:text-red-300">
                    {{ t('admin.xianyu.inventory.lowStock') }}
                  </span>
                </template>
              </td>
              <td class="px-4 py-2">{{ stockError ? '-' : deliveredFor(pool) }}</td>
              <td class="px-4 py-2">{{ stockError ? '-' : usedFor(pool) }}</td>
              <td class="px-4 py-2">{{ stockError ? '-' : disabledFor(pool) }}</td>
              <td class="px-4 py-2">{{ pool.low_stock_threshold }}</td>
              <td class="px-4 py-2">
                <StatusBadge
                  :status="pool.status"
                  :label="pool.status === 'active' ? t('admin.xianyu.inventory.active') : t('admin.xianyu.inventory.inactive')"
                />
              </td>
              <td class="px-4 py-2">
                <div class="flex items-center justify-end gap-1.5">
                  <button class="btn btn-secondary btn-xs" @click="openEdit(pool)">
                    {{ t('common.edit') }}
                  </button>
                  <button class="btn btn-secondary btn-xs" @click="goManageCodes(pool)">
                    {{ t('admin.xianyu.inventory.manageCodes') }}
                  </button>
                  <button class="btn btn-secondary btn-xs" @click="openRestock(pool)">
                    {{ t('admin.xianyu.inventory.generateCodes') }}
                  </button>
                  <button class="btn btn-secondary btn-xs" @click="openDeletePool(pool)">
                    {{ t('common.delete') }}
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <EmptyState v-if="!pools.length" :message="t('admin.xianyu.inventory.noPools')" />

      <BaseDialog :show="editVisible" :title="editingID ? t('admin.xianyu.inventory.editPool') : t('admin.xianyu.inventory.addPool')" @close="editVisible = false">
        <div class="space-y-4">
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.inventory.name') }}</label>
            <input v-model="form.name" class="input w-full" :placeholder="t('admin.xianyu.inventory.name')" />
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.inventory.slug') }}</label>
            <input v-model="form.slug" class="input w-full" :placeholder="t('admin.xianyu.inventory.slug')" :disabled="!!editingID" />
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.inventory.description') }}</label>
            <textarea v-model="form.description" class="input w-full" rows="2"></textarea>
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.inventory.lowStockThreshold') }}</label>
            <input v-model.number="form.low_stock_threshold" type="number" min="0" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.inventory.specGroup') }}</label>
            <select v-model.number="form.group_id" class="input w-full">
              <option :value="null" disabled>{{ t('admin.xianyu.inventory.specGroupPlaceholder') }}</option>
              <option v-for="g in groups" :key="g.id" :value="g.id">{{ g.name }}</option>
            </select>
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.inventory.specDays') }}</label>
            <input v-model.number="form.validity_days" type="number" min="1" max="365" class="input w-full" />
          </div>
          <div class="flex items-center gap-2">
            <span class="text-sm font-medium">{{ t('admin.xianyu.inventory.active') }}</span>
            <Toggle v-model="formStatus" />
          </div>
          <div class="flex justify-end gap-2">
            <button class="btn btn-secondary" @click="editVisible = false">{{ t('common.cancel') }}</button>
            <button class="btn btn-primary" @click="save">{{ t('admin.xianyu.inventory.savePool') }}</button>
          </div>
        </div>
      </BaseDialog>

      <BaseDialog :show="restockVisible" :title="t('admin.xianyu.inventory.restockTitle', { name: restockingPool?.name ?? '' })" @close="restockVisible = false">
        <div class="space-y-4">
          <p class="rounded bg-primary-50 px-3 py-2 text-xs text-primary-700 dark:bg-primary-900/20 dark:text-primary-300">
            {{ t('admin.xianyu.inventory.restockHint') }}
          </p>
          <p v-if="!poolSpecReady(restockingPool)" class="rounded bg-orange-50 px-3 py-2 text-xs text-orange-600 dark:bg-orange-900/20">
            {{ t('admin.xianyu.inventory.restockSpecMissing') }}
          </p>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.inventory.restockCount') }}</label>
            <input v-model.number="restockForm.count" type="number" min="1" max="1000" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.inventory.restockExpiry') }}</label>
            <input v-model.number="restockForm.expires_in_days" type="number" min="0" class="input w-full" :placeholder="t('admin.xianyu.inventory.restockExpiryHint')" />
          </div>
          <div class="flex justify-end gap-2">
            <button class="btn btn-secondary" @click="restockVisible = false">{{ t('common.cancel') }}</button>
            <button class="btn btn-primary" :disabled="!poolSpecReady(restockingPool)" @click="confirmRestock">{{ t('admin.xianyu.inventory.restockConfirm') }}</button>
          </div>
        </div>
      </BaseDialog>

      <ConfirmDialog
        :show="deletePoolVisible"
        :title="t('admin.xianyu.inventory.deletePoolTitle')"
        :message="t('admin.xianyu.inventory.deletePoolConfirm', { name: deletingPool?.name ?? '' })"
        :confirm-text="t('common.delete')"
        :cancel-text="t('common.cancel')"
        danger
        @confirm="confirmDeletePool"
        @cancel="deletePoolVisible = false"
      />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { useRouter } from 'vue-router'
import { adminAPI } from '@/api/admin'
import type { Group, XianyuItemPool, XianyuOverview } from '@/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import StatusBadge from '@/components/common/StatusBadge.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Toggle from '@/components/common/Toggle.vue'
import Icon from '@/components/icons/Icon.vue'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const appStore = useAppStore()
const router = useRouter()

const pools = ref<XianyuItemPool[]>([])
const overview = ref<XianyuOverview | null>(null)
const stockError = ref(false)
const groups = ref<Group[]>([])

async function load() {
  try {
    const [poolList, ov, groupList] = await Promise.all([
      adminAPI.xianyu.listItemPools(),
      adminAPI.xianyu.getOverview(),
      adminAPI.groups.getAll().catch(() => [] as Group[])
    ])
    pools.value = poolList
    overview.value = ov
    // 发码分组仅允许订阅模式分组（后端同样校验）
    groups.value = (groupList as Array<Group & { subscription_type?: string }>).filter(
      (g) => !g.subscription_type || g.subscription_type === 'subscription'
    )
    stockError.value = false
  } catch (err) {
    stockError.value = true
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

function groupName(groupID?: number | null): string {
  if (!groupID) return '-'
  return groups.value.find((g) => g.id === groupID)?.name || `#${groupID}`
}
function cardSpecText(pool: XianyuItemPool): string {
  if (!pool.group_id || !pool.validity_days) return t('admin.xianyu.inventory.specMissing')
  return t('admin.xianyu.inventory.specText', { group: groupName(pool.group_id), days: pool.validity_days })
}
function poolSpecReady(pool: XianyuItemPool | null): boolean {
  return !!pool && !!pool.group_id && !!pool.validity_days
}

function deliveredFor(pool: XianyuItemPool): number {
  const item = overview.value?.pools.find((p) => p.pool.id === pool.id)
  return item ? item.delivered : 0
}
function remainingFor(pool: XianyuItemPool): number {
  const item = overview.value?.pools.find((p) => p.pool.id === pool.id)
  return item ? item.remaining : 0
}
function usedFor(pool: XianyuItemPool): number {
  const item = overview.value?.pools.find((p) => p.pool.id === pool.id)
  return item ? item.used : 0
}
function disabledFor(pool: XianyuItemPool): number {
  const item = overview.value?.pools.find((p) => p.pool.id === pool.id)
  return item ? item.disabled : 0
}
function isLowStock(pool: XianyuItemPool): boolean {
  return pool.status === 'active' && pool.low_stock_threshold > 0 && remainingFor(pool) <= pool.low_stock_threshold
}

const editVisible = ref(false)
const editingID = ref<number | null>(null)
const form = reactive({
  name: '',
  slug: '',
  description: '',
  low_stock_threshold: 0,
  status: 'active',
  group_id: null as number | null,
  validity_days: 1
})

const formStatus = computed({
  get: () => form.status === 'active',
  set: (value: boolean) => {
    form.status = value ? 'active' : 'disabled'
  }
})

function openEdit(pool?: XianyuItemPool) {
  editingID.value = pool?.id ?? null
  form.name = pool?.name ?? ''
  form.slug = pool?.slug ?? ''
  form.description = pool?.description ?? ''
  form.low_stock_threshold = pool?.low_stock_threshold ?? 0
  form.status = pool?.status ?? 'active'
  form.group_id = pool?.group_id ?? null
  form.validity_days = pool?.validity_days ?? 1
  editVisible.value = true
}

async function save() {
  if (!form.name.trim()) {
    appStore.showError(t('admin.xianyu.inventory.poolNameRequired'))
    return
  }
  if (!form.slug.trim()) {
    appStore.showError(t('admin.xianyu.inventory.poolSlugRequired'))
    return
  }
  try {
    await adminAPI.xianyu.saveItemPool({
      id: editingID.value ?? undefined,
      name: form.name.trim(),
      slug: form.slug.trim(),
      description: form.description.trim(),
      low_stock_threshold: Math.max(0, form.low_stock_threshold || 0),
      status: form.status as 'active' | 'disabled',
      code_type: 'subscription',
      group_id: form.group_id,
      validity_days: form.validity_days
    })
    editVisible.value = false
    await load()
    appStore.showSuccess(t('admin.xianyu.inventory.success'))
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

function goManageCodes(pool: XianyuItemPool) {
  router.push({ path: '/admin/redeem', query: { pool: pool.slug, view: 'list' } })
}

const deletePoolVisible = ref(false)
const deletingPool = ref<XianyuItemPool | null>(null)

function openDeletePool(pool: XianyuItemPool) {
  deletingPool.value = pool
  deletePoolVisible.value = true
}

async function confirmDeletePool() {
  if (!deletingPool.value) return
  try {
    await adminAPI.xianyu.deleteItemPool(deletingPool.value.id)
    deletePoolVisible.value = false
    await load()
    appStore.showSuccess(t('admin.xianyu.inventory.deleteSuccess'))
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

const restockVisible = ref(false)
const restockingPool = ref<XianyuItemPool | null>(null)
const restockForm = reactive({ count: 10, expires_in_days: 0 })

function openRestock(pool: XianyuItemPool) {
  restockingPool.value = pool
  restockForm.count = 10
  restockForm.expires_in_days = 0
  restockVisible.value = true
}

async function confirmRestock() {
  if (!restockingPool.value) return
  if (restockForm.count < 1 || restockForm.count > 1000) {
    appStore.showError(t('admin.xianyu.inventory.restockCountInvalid'))
    return
  }
  try {
    const result = await adminAPI.xianyu.stockItemPool(restockingPool.value.id, {
      count: restockForm.count,
      expires_in_days: Math.max(0, restockForm.expires_in_days || 0)
    })
    restockVisible.value = false
    await load()
    appStore.showSuccess(t('admin.xianyu.inventory.restockSuccess', { created: result.created, remaining: result.remaining }))
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

onMounted(load)
</script>
