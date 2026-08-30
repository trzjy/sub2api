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
        <span v-if="syncResults.length" class="text-sm text-gray-500 dark:text-gray-400">
          <span v-for="r in syncResults" :key="r.account_id" class="mr-2">
            {{ r.account_id }}: {{ r.success ? t('common.success') : r.error }}
          </span>
        </span>
      </div>

      <div class="mb-4 rounded-lg border border-gray-200 dark:border-dark-700">
        <div class="flex items-center justify-between border-b border-gray-200 px-5 py-3 dark:border-dark-700">
          <h2 class="font-semibold">{{ t('admin.xianyu.products.rules') }}</h2>
          <button class="btn btn-primary btn-sm" @click="openRule(null)">
            {{ t('admin.xianyu.products.addRule') }}
          </button>
        </div>
        <div class="p-3">
          <table v-if="rules.length" class="w-full text-sm">
            <thead>
              <tr class="border-b border-gray-100 text-left dark:border-dark-700">
                <th class="px-3 py-2">{{ t('admin.xianyu.products.priority') }}</th>
                <th class="px-3 py-2">{{ t('admin.xianyu.products.accountId') }}</th>
                <th class="px-3 py-2">{{ t('admin.xianyu.products.matchType') }}</th>
                <th class="px-3 py-2">{{ t('admin.xianyu.products.keywordText') }}</th>
                <th class="px-3 py-2">{{ t('admin.xianyu.products.pool') }}</th>
                <th class="px-3 py-2">{{ t('admin.xianyu.products.status') }}</th>
                <th class="px-3 py-2 text-right">{{ t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="rule in rules" :key="rule.id" class="border-b border-gray-100 dark:border-dark-700">
                <td class="px-3 py-2">{{ rule.priority }}</td>
                <td class="px-3 py-2">{{ accountLabel(rule.account_pk) }}</td>
                <td class="px-3 py-2">{{ rule.match_type === 'keyword' ? t('admin.xianyu.products.keyword') : t('admin.xianyu.products.accountDefault') }}</td>
                <td class="px-3 py-2">{{ rule.keyword || '-' }}</td>
                <td class="px-3 py-2">{{ poolLabel(rule.pool_id) }}</td>
                <td class="px-3 py-2">
                  <StatusBadge :status="rule.status" :label="rule.status === 'active' ? t('admin.xianyu.products.ruleActive') : t('admin.xianyu.products.ruleDisabled')" />
                </td>
                <td class="px-3 py-2">
                  <div class="flex items-center justify-end gap-1.5">
                    <button class="btn btn-secondary btn-xs" @click="openRule(rule)">{{ t('common.edit') }}</button>
                    <button class="btn btn-secondary btn-xs" @click="toggleRule(rule)">
                      {{ rule.status === 'active' ? t('admin.xianyu.products.ruleDisabled') : t('admin.xianyu.products.ruleActive') }}
                    </button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
          <div v-else class="px-3 py-2 text-sm text-gray-500">{{ t('admin.xianyu.products.noRules') }}</div>
        </div>
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
                  <button v-if="product.binding_status !== 'mapped'" class="btn btn-primary btn-xs" @click="openBind(product)">
                    {{ t('admin.xianyu.products.bind') }}
                  </button>
                  <template v-else>
                    <button class="btn btn-primary btn-xs" @click="openBind(product)">
                      {{ t('admin.xianyu.products.bind') }}
                    </button>
                    <button class="btn btn-secondary btn-xs" @click="unbind(product)">
                      {{ t('admin.xianyu.products.unbind') }}
                    </button>
                  </template>
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

      <BaseDialog :show="ruleVisible" :title="t('admin.xianyu.products.editRule')" @close="ruleVisible = false">
        <div class="space-y-4">
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.products.accountId') }}</label>
            <Select v-model="ruleForm.account_pk" :options="accountOptions" />
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.products.matchType') }}</label>
            <Select
              v-model="ruleForm.match_type"
              :options="[
                { value: 'keyword', label: t('admin.xianyu.products.keyword') },
                { value: 'account_default', label: t('admin.xianyu.products.accountDefault') }
              ]"
            />
          </div>
          <div v-if="ruleForm.match_type === 'keyword'">
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.products.keywordText') }}</label>
            <input v-model="ruleForm.keyword" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.products.priority') }}</label>
            <input v-model.number="ruleForm.priority" type="number" min="0" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium">{{ t('admin.xianyu.products.pool') }}</label>
            <Select v-model="ruleForm.pool_id" :options="poolOptions" />
          </div>
          <div class="flex items-center gap-2">
            <span class="text-sm font-medium">{{ t('admin.xianyu.products.ruleActive') }}</span>
            <Toggle v-model="ruleStatus" />
          </div>
          <div class="flex justify-end gap-2">
            <button class="btn btn-secondary" @click="ruleVisible = false">{{ t('common.cancel') }}</button>
            <button class="btn btn-primary" @click="saveRule">{{ t('admin.xianyu.products.saveRule') }}</button>
          </div>
        </div>
      </BaseDialog>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type { XianyuProduct, XianyuItemPool, XianyuBindingRule, XianyuAccount } from '@/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import StatusBadge from '@/components/common/StatusBadge.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import Icon from '@/components/icons/Icon.vue'

const { t } = useI18n()
const appStore = useAppStore()

const products = ref<XianyuProduct[]>([])
const pools = ref<XianyuItemPool[]>([])
const accounts = ref<XianyuAccount[]>([])
const rules = ref<XianyuBindingRule[]>([])
const loading = ref(false)
const search = ref('')
const bindingFilter = ref('')
const syncResults = ref<{ account_id: string; success: boolean; error?: string }[]>([])

const bindingOptions = [
  { value: '', label: t('admin.xianyu.products.bindingStatus') },
  { value: 'mapped', label: t('admin.xianyu.products.mapped') },
  { value: 'unmapped', label: t('admin.xianyu.products.unmapped') }
]

const poolOptions = computed(() => pools.value.filter((p) => p.status === 'active').map((p) => ({ value: String(p.id), label: p.name })))
const accountOptions = computed(() => accounts.value.map((a) => ({ value: String(a.id), label: a.account_id })))

function accountLabel(pk: number): string {
  const a = accounts.value.find((x) => x.id === pk)
  return a ? a.account_id : String(pk)
}

function poolLabel(poolId: number): string {
  const p = pools.value.find((x) => x.id === poolId)
  return p ? p.name : String(poolId)
}

const ruleStatus = computed({
  get: () => ruleForm.status === 'active',
  set: (value: boolean) => {
    ruleForm.status = value ? 'active' : 'disabled'
  }
})

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
    const [prods, poolList, ruleList, accountList] = await Promise.all([
      adminAPI.xianyu.listProducts(),
      adminAPI.xianyu.listItemPools(),
      adminAPI.xianyu.listBindingRules(),
      adminAPI.xianyu.listAccounts()
    ])
    products.value = prods
    pools.value = poolList
    rules.value = ruleList
    accounts.value = accountList
  } catch (err) {
    appStore.showError(String(err))
  } finally {
    loading.value = false
  }
}

async function doSync() {
  try {
    await adminAPI.xianyu.syncProducts()
    syncResults.value = []
    await load()
    appStore.showSuccess(t('admin.xianyu.products.syncSuccess'))
  } catch (err) {
    syncResults.value = [{ account_id: '-', success: false, error: String(err) }]
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

async function unbind(product: XianyuProduct) {
  try {
    await adminAPI.xianyu.bindProduct(product.id, null)
    await load()
    appStore.showSuccess(t('admin.xianyu.products.unbindSuccess'))
  } catch (err) {
    appStore.showError(String(err))
  }
}

const ruleVisible = ref(false)
const ruleForm = reactive<{ id: number | null; priority: number; account_pk: string; match_type: string; keyword: string; pool_id: string; status: string }>({
  id: null,
  priority: 1,
  account_pk: '',
  match_type: 'keyword',
  keyword: '',
  pool_id: '',
  status: 'active'
})

function openRule(rule: XianyuBindingRule | null) {
  ruleForm.id = rule?.id ?? null
  ruleForm.priority = rule?.priority ?? 1
  ruleForm.account_pk = rule ? String(rule.account_pk) : ''
  ruleForm.match_type = rule?.match_type ?? 'keyword'
  ruleForm.keyword = rule?.keyword ?? ''
  ruleForm.pool_id = rule ? String(rule.pool_id) : ''
  ruleForm.status = rule?.status ?? 'active'
  ruleVisible.value = true
}

async function saveRule() {
  if (!ruleForm.account_pk) {
    appStore.showError(t('admin.xianyu.products.ruleAccountRequired'))
    return
  }
  if (!ruleForm.pool_id) {
    appStore.showError(t('admin.xianyu.products.rulePoolRequired'))
    return
  }
  if (ruleForm.match_type === 'keyword' && !ruleForm.keyword.trim()) {
    appStore.showError(t('admin.xianyu.products.ruleKeywordRequired'))
    return
  }
  try {
    await adminAPI.xianyu.saveBindingRule({
      id: ruleForm.id ?? undefined,
      priority: ruleForm.priority,
      account_pk: Number(ruleForm.account_pk),
      match_type: ruleForm.match_type as 'keyword' | 'account_default',
      keyword: ruleForm.keyword.trim(),
      pool_id: Number(ruleForm.pool_id),
      status: ruleForm.status as 'active' | 'disabled'
    })
    ruleVisible.value = false
    await load()
    appStore.showSuccess(t('admin.xianyu.products.ruleSaved'))
  } catch (err) {
    appStore.showError(String(err))
  }
}

async function toggleRule(rule: XianyuBindingRule) {
  try {
    await adminAPI.xianyu.saveBindingRule({
      id: rule.id,
      priority: rule.priority,
      account_pk: rule.account_pk,
      match_type: rule.match_type,
      keyword: rule.keyword,
      pool_id: rule.pool_id,
      status: rule.status === 'active' ? 'disabled' : 'active'
    })
    await load()
    appStore.showSuccess(t('admin.xianyu.products.ruleSaved'))
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
