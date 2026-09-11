import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import RedeemView from '../RedeemView.vue'

const { listRedeemCodes, listRedeemValues, batchDeleteRedeemCodes, showSuccess, showError, showInfo } =
  vi.hoisted(() => ({
    listRedeemCodes: vi.fn(),
    listRedeemValues: vi.fn(),
    batchDeleteRedeemCodes: vi.fn(),
    showSuccess: vi.fn(),
    showError: vi.fn(),
    showInfo: vi.fn()
  }))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: vi.fn() }),
  useRoute: () => ({ path: '/admin/redeem', query: {} }),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    redeem: {
      list: listRedeemCodes,
      listValues: listRedeemValues,
      generate: vi.fn(),
      delete: vi.fn(),
      batchDelete: batchDeleteRedeemCodes,
      batchUpdate: vi.fn(),
      exportCodes: vi.fn()
    },
    groups: {
      getAll: vi.fn().mockResolvedValue([])
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showSuccess,
    showError,
    showInfo
  })
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard: vi.fn()
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const DataTableStub = {
  props: ['columns', 'data'],
  template: `
    <table>
      <thead>
        <tr>
          <th v-for="column in columns" :key="column.key">
            <slot :name="'header-' + column.key" :column="column">{{ column.label }}</slot>
          </th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="row in data" :key="row.id">
          <td v-for="column in columns" :key="column.key">
            <slot :name="'cell-' + column.key" :row="row" :value="row[column.key]">
              {{ row[column.key] }}
            </slot>
          </td>
        </tr>
      </tbody>
    </table>
  `
}

const SelectStub = {
  props: ['modelValue', 'options'],
  emits: ['update:modelValue', 'change'],
  setup(props: { options: Array<{ value: unknown; label: string }> }, { emit }: { emit: (event: string, ...args: unknown[]) => void }) {
    const onChange = (event: Event) => {
      const raw = (event.target as HTMLSelectElement).value
      const option = props.options.find((item) => String(item.value ?? '') === raw)
      const value = option ? option.value : raw
      emit('update:modelValue', value)
      emit('change', value, option ?? null)
    }
    return { onChange }
  },
  template: `
    <select v-bind="$attrs" :value="modelValue ?? ''" @change="onChange">
      <option v-for="option in options" :key="String(option.value ?? '')" :value="option.value ?? ''">
        {{ option.label }}
      </option>
    </select>
  `
}

const ConfirmDialogStub = {
  props: ['show', 'title', 'message'],
  emits: ['confirm', 'cancel'],
  template: `
    <div v-if="show" data-test="confirm-dialog">
      <button data-test="confirm-ok" @click="$emit('confirm')">OK</button>
    </div>
  `
}

const mountOptions = {
  attachTo: document.body,
  global: {
    stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      TablePageLayout: {
        template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
      },
      DataTable: DataTableStub,
      Pagination: true,
      ConfirmDialog: ConfirmDialogStub,
      Select: SelectStub,
      GroupBadge: true,
      GroupOptionItem: true,
      Icon: true,
      Teleport: true
    }
  }
}

describe('admin RedeemView value filter and delete selected', () => {
  beforeEach(() => {
    localStorage.clear()
    document.body.innerHTML = ''

    listRedeemCodes.mockReset()
    listRedeemValues.mockReset()
    batchDeleteRedeemCodes.mockReset()
    showSuccess.mockReset()
    showError.mockReset()
    showInfo.mockReset()

    listRedeemCodes.mockResolvedValue({
      items: [
        {
          id: 1,
          code: 'CODE-1',
          type: 'balance',
          value: 10,
          status: 'unused',
          used_by: null,
          used_at: null,
          created_at: '2026-01-01T00:00:00Z',
          expires_at: null
        },
        {
          id: 2,
          code: 'CODE-2',
          type: 'balance',
          value: 20,
          status: 'unused',
          used_by: null,
          used_at: null,
          created_at: '2026-01-01T00:00:00Z',
          expires_at: null
        }
      ],
      total: 2,
      page: 1,
      page_size: 20,
      pages: 1
    })
    listRedeemValues.mockResolvedValue([10, 20])
    batchDeleteRedeemCodes.mockResolvedValue({ deleted: 2, message: 'ok' })
  })

  it('loads face value options and filters the list by selected value', async () => {
    const wrapper = mount(RedeemView, mountOptions)
    await flushPromises()

    expect(listRedeemValues).toHaveBeenCalledWith(undefined)

    await wrapper.get('[data-test="value-filter"]').setValue('10')
    await flushPromises()

    expect(listRedeemCodes).toHaveBeenLastCalledWith(
      1,
      20,
      expect.objectContaining({ value: 10 }),
      expect.anything()
    )
  })

  it('deletes selected codes after confirmation and clears the selection', async () => {
    const wrapper = mount(RedeemView, mountOptions)
    await flushPromises()

    await wrapper.findAll('[data-test="select-code"]')[0].setValue(true)
    await wrapper.findAll('[data-test="select-code"]')[1].setValue(true)

    await wrapper.get('[data-test="delete-selected-open"]').trigger('click')
    await flushPromises()
    expect(batchDeleteRedeemCodes).not.toHaveBeenCalled()

    await wrapper.get('[data-test="confirm-ok"]').trigger('click')
    await flushPromises()

    expect(batchDeleteRedeemCodes).toHaveBeenCalledWith([1, 2])
    expect(showSuccess).toHaveBeenCalledWith('admin.redeem.selectedCodesDeleted')
    expect(wrapper.find('[data-test="delete-selected-banner"]').exists()).toBe(false)
  })

  it('keeps the delete selected button disabled until a row is selected', async () => {
    const wrapper = mount(RedeemView, mountOptions)
    await flushPromises()

    const button = wrapper.get('[data-test="delete-selected-open"]').element as HTMLButtonElement
    expect(button.disabled).toBe(true)

    await wrapper.findAll('[data-test="select-code"]')[0].setValue(true)
    expect((wrapper.get('[data-test="delete-selected-open"]').element as HTMLButtonElement).disabled).toBe(false)
  })
})
