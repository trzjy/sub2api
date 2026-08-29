<template>
  <AppLayout>
    <div class="p-6">
      <div class="mb-6 flex items-start justify-between">
        <div>
          <h1 class="text-2xl font-bold">{{ t('admin.xianyu.products.title') }}</h1>
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.xianyu.products.description') }}</p>
        </div>
        <div class="flex items-center gap-2">
          <button class="btn btn-secondary" :disabled="loading" @click="doSync">
            <Icon name="refresh" size="sm" /> {{ t('admin.xianyu.products.refresh') }}
          </button>
        </div>
      </div>

      <div class="mb-4 flex flex-wrap items-center gap-3">
        <input
          v-model="search"
          class="input max-w-xs"
          :placeholder="t('admin.xianyu.products.itemTitle')"
          @keyup.enter="load"
        />
        <Select
          v-model="bindingFilter"
          :options="bindingOptions"
          class="w-40"
          @change="load"
        />
      </div>

      <div v-if="products.length" class="overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-700">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-gray-200 bg-gray-50 text-left dark:border-dark-700 dark:bg-dark-800">
              <th class="px-4 py-2">
                <input v-model="selectAll" type="checkbox" class="rounded border-gray-300 dark:border-dark-600" />
              </th>
              <th class="px-4 py-2">{{ t('admin.xianyu.products.itemTitle') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.products.itemId') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.products.spec') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.products.accountId') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.products.bindingStatus') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.products.bindingSource') }}</th>
              <th class="px-4 py-2">{{ t('admin.xianyu.products.status') }}</th>
              <th class="px-4 py-2 text-right">{{ t('common.actions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="product in filteredProducts"
              :key="product.id"
              class="border-b border-gray-100 dark:border-dark-700"
            >
              <td class="px-4 py-2">
                <input v-model="selectedIDs" type="checkbox" :value="product.id" class="rounded border-gray-300 dark:border-dark-600" />
              </td>
              <td class="px-4 py-2 font-medium">{{ product.title || '-' }}</td>
              <td class="px-4 py-2">{{ product.item_id }}</td>
              <td class="px-4 py-2 text-gray-500">{{ specText(product) }}</td>
              <td class="px-4 py-2">{{ product.account_id }}</td>
              <td class="px-4 py-2">
                <StatusBadge
                  :status="product.binding_status"
                  :label="product.binding_status === 'mapped' ? t('admin.xianyu.products.mapped') : t('admin.xianyu.products.unmapped')"
                />
              </td>
              <td class="px-4 py-2 text-gray-500">{{ sourceLabel(product.binding_source) }}</td>
              <td class="px-4 py-2">
                <StatusBadge
                  :status="product.status"
                  :label="productStatusLabel(product.status)"
                />
              </td>
              <td class="px-4 py-2">
                <div class="flex items-center justify-end gap-1.5">
                  <button class="btn btn-primary btn-xs" @click="openBind(product)">
                    {{ product.binding_status === 'mapped' ? t('admin.xianyu.products.unbind') : t('admin.xianyu.products.bind') }}
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        <div v-if="selectedIDs.length" class="flex items-center gap-2 border-t border-gray-100 p-3 dark:border-dark-700">
          <span class="text-sm text-gray-500">{{ selectedIDs.length }}</span>
          <button class="btn btn-primary btn-sm" @click="openBindBatch">
            {{ t('admin.xianyu.products.bind') }}
          </button>
        </div>
      </div>
      <EmptyState v-else :message="t('admin.xianyu.products.noProducts')" />

      <BaseDialog :show="bindVisible" :title="t('admin.xianyu.products.bindToPool')" @close="bindVisible = false">
        <div class="space-y-4">
          <Select v-model="bindPoolID" :options="poolOptions" :placeholder="t('admin.xianyu.products.selectPool')" />
          <div class="flex justify-end gap-2">
            <button class="btn btn-secondary" @click="bindVisible = false">{{ t('common.cancel') }}</button>
            <button class="btn btn-primary" :disabled="!bindPoolID" @click="confirmBind">{{ t('common.confirm') }}</button>
          </div>
        </div>
      </BaseDialog>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type { XianyuProduct, XianyuItemPool } from '@/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import StatusBadge from '@/components/common/StatusBadge.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'

const { t } = useI18n()
const appStore = useAppStore()

const products = ref<XianyuProduct[]>([])
const pools = ref<XianyuItemPool[]>([])
const loading = ref(false)
const search = ref('')
const bindingFilter = ref('')

const bindingOptions = [
  { value: '', label: t('admin.xianyu.products.bindingStatus') },
  { value: 'mapped', label: t('admin.xianyu.products.mapped') },
  { value: 'unmapped', label: t('admin.xianyu.products.unmapped') }
]

const poolOptions = computed(() => pools.value.map((p) => ({ value: String(p.id), label: p.name })))

const filteredProducts = computed(() => {
  const term = search.value.trim().toLowerCase()
  return products.value.filter((p) => {
    if (bindingFilter.value && p.binding_status !== bindingFilter.value) return false
    if (!term) return true
    return (
      p.title.toLowerCase().includes(term) ||
      p.item_id.toLowerCase().includes(term) ||
      p.account_id.toLowerCase().includes(term)
    )
  })
})

const selectedIDs = ref<number[]>([])
const selectAll = computed({
  get: () => filteredProducts.value.length > 0 && selectedIDs.value.length === filteredProducts.value.length,
  set: (value: boolean) => {
    selectedIDs.value = value ? filteredProducts.value.map((p) => p.id) : []
  }
})

async function load() {
  loading.value = true
  try {
    const [prods, poolList] = await Promise.all([
      adminAPI.xianyu.listProducts(),
      adminAPI.xianyu.listItemPools()
    ])
    products.value = prods
    pools.value = poolList
  } catch (err) {
    appStore.showError(String(err))
  } finally {
    loading.value = false
  }
}

async function doSync() {
  try {
    await adminAPI.xianyu.syncProducts()
    await load()
    appStore.showSuccess(t('admin.xianyu.products.syncSuccess'))
  } catch (err) {
    appStore.showError(String(err))
  }
}

const bindVisible = ref(false)
const bindPoolID = ref('')
const bindTargets = ref<XianyuProduct[]>([])

function openBind(product: XianyuProduct) {
  bindTargets.value = [product]
  bindPoolID.value = product.pool_id ? String(product.pool_id) : ''
  bindVisible.value = true
}

function openBindBatch() {
  bindTargets.value = products.value.filter((p) => selectedIDs.value.includes(p.id))
  bindPoolID.value = ''
  bindVisible.value = true
}

async function confirmBind() {
  try {
    for (const product of bindTargets.value) {
      const poolID = bindPoolID.value ? Number(bindPoolID.value) : null
      await adminAPI.xianyu.bindProduct(product.id, poolID)
    }
    bindVisible.value = false
    await load()
    appStore.showSuccess(t('admin.xianyu.products.bindSuccess'))
  } catch (err) {
    appStore.showError(String(err))
  }
}

function specText(product: XianyuProduct): string {
  if (product.spec_name || product.spec_value) {
    return `${product.spec_name}:${product.spec_value}`
  }
  return '-'
}

function sourceLabel(source: string): string {
  switch (source) {
    case 'manual': return t('admin.xianyu.products.manual')
    case 'account_default': return t('admin.xianyu.products.accountDefault')
    case 'keyword': return t('admin.xianyu.products.keyword')
    default: return t('admin.xianyu.products.autoNew')
  }
}

function productStatusLabel(status: string): string {
  switch (status) {
    case 'active': return t('admin.xianyu.products.active')
    case 'removed': return t('admin.xianyu.products.removed')
    default: return t('admin.xianyu.products.disabled')
  }
}

onMounted(load)
</script>
