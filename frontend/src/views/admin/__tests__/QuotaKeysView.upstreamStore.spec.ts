import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import QuotaKeysView from '../QuotaKeysView.vue'
import { adminAPI } from '@/api/admin'
import * as adminUpstreams from '@/api/admin/quotaKeys'

// CARD-KC-18：上游显示名编辑新增的命名导出（listUpstreams / updateUpstream），
// 与既有 `@/api/admin` 的 quotaKeys 命名空间 mock 并存。
vi.mock('@/api/admin/quotaKeys', () => ({
  listUpstreams: vi.fn(),
  updateUpstream: vi.fn()
}))

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

// CARD-KC-18：上游显示名编辑 + 激活码列表按上游区分显示。
//   - 操作栏提供「编辑上游」按钮，打开弹窗后可改显示名 / 续费链接 / 备注；
//   - 保存后调用 updateUpstream(key, payload)，并刷新激活码列表，使显示名立即生效；
//   - 激活码行按 upstream_key 匹配上游显示名；上游条目缺失时回落显示原始 upstream_key。

describe('QuotaKeysView 上游显示名编辑（CARD-KC-18）', () => {
  // 增强版挂载：DataTable 桩渲染行并透传 cell-upstream 槽，便于断言按上游区分的显示名。
  function mountUpstreamView() {
    return mount(QuotaKeysView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          TablePageLayout: {
            template: '<div><slot name="filters" /><slot name="table" /></div>'
          },
          DataTable: {
            props: ['columns', 'data', 'loading'],
            template: `<div class="datatable-stub">
              <template v-for="(row, i) in data" :key="i">
                <span class="cell-code">{{ row.code_masked }}</span>
                <slot name="cell-upstream" :row="row" />
              </template>
            </div>`
          },
          PriorityStepper: true,
          QuotaKeyUsageCell: true,
          Pagination: true,
          ConfirmDialog: true,
          EmptyState: true,
          TextArea: true,
          Icon: true,
          AutoEntryConfigModal: true,
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

  const upstreamsFixture = [
    { id: 1, key: 'kn_cy', name: 'kn-cy.com 网关', type: 'kncy', renewal_url: 'https://shop.kn-cy.com/products', notes: null },
    { id: 2, key: 'shareai', name: 'ShareAI 号池', type: 'shareai', renewal_url: null, notes: null }
  ]

  const quotaKeyWithUpstream = {
    id: 1,
    status: 'active',
    exhausted: false,
    code_masked: '****std0',
    notes: null,
    last_refresh_at: null,
    last_refresh_error: null,
    expires_at: '2026-12-31T00:00:00.000Z',
    account_count: 0,
    working_count: 0,
    upstream_key: 'kn_cy'
  }

  beforeEach(() => {
    vi.mocked(adminUpstreams.listUpstreams).mockReset()
    vi.mocked(adminUpstreams.listUpstreams).mockResolvedValue({ items: upstreamsFixture } as never)
    vi.mocked(adminUpstreams.updateUpstream).mockReset()
    vi.mocked(adminUpstreams.updateUpstream).mockImplementation(
      async (key: string, payload: { name: string; renewal_url?: string | null; notes?: string | null }) =>
        ({
          id: upstreamsFixture.find((u) => u.key === key)?.id ?? 0,
          key,
          name: payload.name,
          type: upstreamsFixture.find((u) => u.key === key)?.type ?? '',
          renewal_url: payload.renewal_url ?? null,
          notes: payload.notes ?? null
        } as never)
    )
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [quotaKeyWithUpstream],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    } as never)
  })

  it('操作栏渲染「编辑上游」按钮', async () => {
    const wrapper = mountUpstreamView()
    await flushPromises()

    const toggle = wrapper.find('[data-testid="upstream-edit-toggle"]')
    expect(toggle.exists()).toBe(true)
    expect(toggle.text()).toContain('admin.quotaKeys.upstream.edit')
    wrapper.unmount()
  })

  it('激活码行按 upstream_key 匹配上游显示名（显示名 + 标识）', async () => {
    const wrapper = mountUpstreamView()
    await flushPromises()

    // 默认展示上游显示名 (标识)。
    expect(wrapper.text()).toContain('kn-cy.com 网关 (kn_cy)')
    wrapper.unmount()
  })

  it('上游条目缺失时回落显示原始 upstream_key（不空白）', async () => {
    // 把激活码指到一个不存在的上游标识。
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [{ ...quotaKeyWithUpstream, upstream_key: 'ghost_upstream' }],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    } as never)
    const wrapper = mountUpstreamView()
    await flushPromises()

    // 不展示已知的「kn-cy.com 网关 (kn_cy)」，回落为原始标识且不空白。
    expect(wrapper.text()).not.toContain('kn-cy.com 网关 (kn_cy)')
    expect(wrapper.text()).toContain('ghost_upstream')
    wrapper.unmount()
  })

  it('打开编辑弹窗可改显示名并保存后立即生效（含激活码列表）', async () => {
    const wrapper = mountUpstreamView()
    await flushPromises()

    // 打开弹窗。
    await wrapper.find('[data-testid="upstream-edit-toggle"]').trigger('click')
    await nextTick()
    await flushPromises()

    const nameInput = wrapper.find('[data-testid="upstream-name-kn_cy"]')
    expect(nameInput.exists()).toBe(true)

    await nameInput.setValue('kn-cy.com 网关（改名）')
    await nextTick()

    await wrapper.find('[data-testid="upstream-save-kn_cy"]').trigger('click')
    await flushPromises()
    await nextTick()
    await flushPromises()

    // 调用了 updateUpstream，且 key 不变、仅提交可写字段。
    expect(adminUpstreams.updateUpstream).toHaveBeenCalledTimes(1)
    const callArgs = vi.mocked(adminUpstreams.updateUpstream).mock.calls[0]
    expect(callArgs[0]).toBe('kn_cy')
    expect((callArgs[1] as { name: string }).name).toBe('kn-cy.com 网关（改名）')

    // 保存后刷新激活码列表，激活码行立即显示新名字。
    expect(adminAPI.quotaKeys.list).toHaveBeenCalled()
    expect(wrapper.text()).toContain('kn-cy.com 网关（改名） (kn_cy)')
    wrapper.unmount()
  })

  it('上游条目缺失（listUpstreams 失败）时弹窗显示空状态而非崩溃', async () => {
    vi.mocked(adminUpstreams.listUpstreams).mockRejectedValue(new Error('network'))
    const wrapper = mountUpstreamView()
    await flushPromises()

    await wrapper.find('[data-testid="upstream-edit-toggle"]').trigger('click')
    await nextTick()
    await flushPromises()

    expect(wrapper.text()).toContain('admin.quotaKeys.upstream.empty')
    wrapper.unmount()
  })
})
