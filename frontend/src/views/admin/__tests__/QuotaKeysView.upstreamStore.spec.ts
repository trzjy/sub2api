import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import QuotaKeysView from '../QuotaKeysView.vue'
import { adminAPI } from '@/api/admin'

// CARD-KC-09：激活码页顶部操作栏的上游续费/采购入口。
//   - 入口按钮位于【入库配置】【添加激活码】所在的操作栏；
//   - 展开后 kn-cy.com 项是新标签页外链（shop.kn-cy.com/products）；
//   - ShareAI 暂无公开商铺，点击给出「尚未配置续费链接」提示模态框。

const KNCY_STORE_URL = 'https://shop.kn-cy.com/products'

vi.mock('@/api/admin', () => ({
  adminAPI: {
    quotaKeys: {
      list: vi.fn(),
      get: vi.fn(),
      probeQuota: vi.fn(),
      refresh: vi.fn(),
      clearExhausted: vi.fn(),
      remove: vi.fn(),
      sweepExpired: vi.fn(),
      update: vi.fn(),
      create: vi.fn(),
      refreshAutoPool: vi.fn(),
      batchImportAutoCards: vi.fn()
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn()
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

function mountView() {
  return mount(QuotaKeysView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template: '<div><slot name="filters" /><slot name="table" /></div>'
        },
        DataTable: true,
        ConfirmDialog: true,
        EmptyState: true,
        TextArea: true,
        Icon: true,
        AutoEntryConfigModal: true,
        // BaseDialog 按 show 条件渲染，便于断言提示模态框是否可见。
        BaseDialog: {
          props: ['show', 'title'],
          template: `<div v-if="show" class="dialog-stub" :data-title="title">
            <slot />
            <slot name="footer" />
          </div>`
        }
      }
    }
  })
}

function listEmpty() {
  return { items: [], total: 0, page: 1, page_size: 20, pages: 1 } as never
}

describe('QuotaKeysView 上游续费入口（CARD-KC-09）', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue(listEmpty())
  })

  it('操作栏渲染上游续费下拉按钮', async () => {
    const wrapper = mountView()
    await flushPromises()

    const toggle = wrapper.find('[data-testid="upstream-store-toggle"]')
    expect(toggle.exists()).toBe(true)
    expect(toggle.text()).toContain('admin.quotaKeys.upstreamStore.button')
    wrapper.unmount()
  })

  it('展开后 kn-cy.com 项为 _blank 外链且指向公开商铺', async () => {
    const wrapper = mountView()
    await flushPromises()

    await wrapper.find('[data-testid="upstream-store-toggle"]').trigger('click')
    await nextTick()

    const kncy = wrapper.find('[data-testid="upstream-store-kncy"]')
    expect(kncy.exists()).toBe(true)
    expect(kncy.attributes('href')).toBe(KNCY_STORE_URL)
    expect(kncy.attributes('target')).toBe('_blank')
    expect(kncy.attributes('rel')).toContain('noopener')
    expect(kncy.text()).toContain('admin.quotaKeys.upstreamStore.kncy')
    wrapper.unmount()
  })

  it('ShareAI 占位项点击后弹出「尚未配置」提示模态框', async () => {
    const wrapper = mountView()
    await flushPromises()

    await wrapper.find('[data-testid="upstream-store-toggle"]').trigger('click')
    await nextTick()

    // 初始不渲染任何提示弹窗（只有 BaseDialog 的 show=false 被过滤后剩余的表单/详情弹窗）。
    const dialogTitlesBefore = wrapper
      .findAll('.dialog-stub')
      .map((d) => d.attributes('data-title'))
    expect(dialogTitlesBefore).not.toContain(
      'admin.quotaKeys.upstreamStore.shareaiUnconfiguredTitle'
    )

    await wrapper.find('[data-testid="upstream-store-shareai"]').trigger('click')
    await nextTick()

    const titles = wrapper.findAll('.dialog-stub').map((d) => d.attributes('data-title'))
    expect(titles).toContain('admin.quotaKeys.upstreamStore.shareaiUnconfiguredTitle')
    expect(wrapper.text()).toContain('admin.quotaKeys.upstreamStore.shareaiUnconfigured')

    // 点击后下拉菜单收起，避免遮挡弹窗。
    expect(wrapper.find('[data-testid="upstream-store-shareai"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('再次点击入口按钮可收起下拉菜单', async () => {
    const wrapper = mountView()
    await flushPromises()

    const toggle = () => wrapper.find('[data-testid="upstream-store-toggle"]')
    await toggle().trigger('click')
    await nextTick()
    expect(wrapper.find('[data-testid="upstream-store-kncy"]').exists()).toBe(true)

    await toggle().trigger('click')
    await nextTick()
    expect(wrapper.find('[data-testid="upstream-store-kncy"]').exists()).toBe(false)
    wrapper.unmount()
  })
})
