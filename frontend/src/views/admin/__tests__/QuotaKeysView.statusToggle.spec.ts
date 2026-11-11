import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import QuotaKeysView from '../QuotaKeysView.vue'
import { adminAPI } from '@/api/admin'
import zh from '@/i18n/locales/zh'

// D2-c：列表行「启用 / 停用」一键启停（status.active ↔ status.disabled）+ 二次确认。
// 锁死：
//   1. status=active 行显示「停用」入口；点击 → 二次确认 → 调 setStatus(id, 'disabled')；
//   2. status=disabled 行显示「启用」入口；点击 → 二次确认 → 调 setStatus(id, 'active')；
//   3. statusLabel 对 disabled 输出中文「已停用」。

// 把 zh locale 拍平，供 t() 在测试中解析出真实中文文案（而非仅回显 key），
// 以便断言「已停用」「停用」「启用」等用户可见文本。
function flatten(obj: Record<string, unknown>, prefix = ''): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(obj)) {
    const p = prefix ? `${prefix}.${k}` : k
    if (v && typeof v === 'object') Object.assign(out, flatten(v as Record<string, unknown>, p))
    else if (typeof v === 'string') out[p] = v
  }
  return out
}
const zhFlat = flatten(zh as unknown as Record<string, unknown>)
const t = (key: string) => zhFlat[key] ?? key

vi.mock('@/api/admin', () => ({
  adminAPI: {
    quotaKeys: {
      list: vi.fn(),
      get: vi.fn(),
      setStatus: vi.fn(), // D2-c 新增
      probeQuota: vi.fn(),
      refresh: vi.fn(),
      clearExhausted: vi.fn(),
      remove: vi.fn(),
      sweepExpired: vi.fn(),
      update: vi.fn(),
      create: vi.fn()
    }
  }
}))

// 顺带桩掉 listUpstreams，避免挂载时发起真实 HTTP（loadUpstreams 会捕获错误，但桩掉更快更稳）。
vi.mock('@/api/admin/quotaKeys', () => ({
  listUpstreams: vi.fn().mockResolvedValue({ items: [] }),
  updateUpstream: vi.fn().mockResolvedValue({})
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: vi.fn() }),
  useRoute: () => ({ path: '/admin/quota-keys', query: {} })
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t })
  }
})

const activeRow = {
  id: 101,
  status: 'active',
  exhausted: false,
  code_masked: '****active',
  last_refresh_at: null,
  last_refresh_error: null,
  expires_at: '2026-12-31T00:00:00.000Z',
  account_count: 1,
  working_count: 1
}

const disabledRow = {
  ...activeRow,
  id: 202,
  status: 'disabled',
  code_masked: '****disabled'
}

function mountView() {
  return mount(QuotaKeysView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template: '<div><slot name="filters" /><slot name="table" /></div>'
        },
        // 渲染 cell-actions 槽，使启停按钮可被点击。
        DataTable: {
          props: ['columns', 'data', 'loading'],
          template: `<div>
            <template v-for="(row, i) in data" :key="i">
              <slot name="cell-actions" :row="row" />
            </template>
            <slot name="empty" />
          </div>`
        },
        // 可交互的二次确认弹窗桩：show 为 true 时渲染 confirm/cancel 按钮。
        ConfirmDialog: {
          props: ['show', 'title', 'message', 'confirmText', 'cancelText', 'danger'],
          emits: ['confirm', 'cancel'],
          template: `<div v-if="show" class="cd">
            <span class="cd-title">{{ title }}</span>
            <span class="cd-msg">{{ message }}</span>
            <button class="cd-confirm" @click="$emit('confirm')">{{ confirmText }}</button>
            <button class="cd-cancel" @click="$emit('cancel')">{{ cancelText }}</button>
          </div>`
        },
        EmptyState: true,
        TextArea: true,
        Icon: true,
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }
      }
    }
  })
}

function findButtonByTitle(wrapper: ReturnType<typeof mountView>, title: string) {
  return wrapper.findAll('button').find((b) => b.attributes('title') === title)
}

describe('QuotaKeysView 启用/停用按钮（D2-c）', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.quotaKeys.list).mockReset()
    vi.mocked(adminAPI.quotaKeys.setStatus).mockReset()
    vi.mocked(adminAPI.quotaKeys.setStatus).mockResolvedValue({} as never)
  })

  it('status=active 行显示「停用」入口，二次确认后调 setStatus(id, "disabled")', async () => {
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [activeRow],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    } as never)

    const wrapper = mountView()
    await flushPromises()
    await nextTick()

    // 1) active 行显示「停用」入口。
    const toggleBtn = findButtonByTitle(wrapper, '停用')
    expect(toggleBtn).toBeTruthy()

    // 2) 点击 → 打开二次确认弹窗，且 next 状态收敛为 disabled。
    await toggleBtn!.trigger('click')
    await nextTick()
    expect((wrapper.vm as unknown as { statusConfirm: { next: string } }).statusConfirm.next).toBe('disabled')
    const confirmBtn = wrapper.find('.cd-confirm')
    expect(confirmBtn.exists()).toBe(true)
    // 确认标题为「停用激活码」。
    expect(wrapper.find('.cd-title').text()).toContain('停用激活码')

    // 3) 确认 → 调 setStatus(id, 'disabled')。
    await confirmBtn.trigger('click')
    await flushPromises()
    await nextTick()

    expect(adminAPI.quotaKeys.setStatus).toHaveBeenCalledTimes(1)
    expect(adminAPI.quotaKeys.setStatus).toHaveBeenCalledWith(activeRow.id, 'disabled')
    // 切换结束后解除按钮禁用态。
    expect((wrapper.vm as unknown as { statusTogglingId: number | null }).statusTogglingId).toBeNull()
    wrapper.unmount()
  })

  it('status=disabled 行显示「启用」入口，二次确认后调 setStatus(id, "active")', async () => {
    vi.mocked(adminAPI.quotaKeys.list).mockResolvedValue({
      items: [disabledRow],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    } as never)

    const wrapper = mountView()
    await flushPromises()
    await nextTick()

    // 1) disabled 行显示「启用」入口。
    const toggleBtn = findButtonByTitle(wrapper, '启用')
    expect(toggleBtn).toBeTruthy()

    // 2) 点击 → 打开二次确认弹窗，next 收敛为 active。
    await toggleBtn!.trigger('click')
    await nextTick()
    expect((wrapper.vm as unknown as { statusConfirm: { next: string } }).statusConfirm.next).toBe('active')
    const confirmBtn = wrapper.find('.cd-confirm')
    expect(confirmBtn.exists()).toBe(true)
    expect(wrapper.find('.cd-title').text()).toContain('启用激活码')

    // 3) 确认 → 调 setStatus(id, 'active')。
    await confirmBtn.trigger('click')
    await flushPromises()
    await nextTick()

    expect(adminAPI.quotaKeys.setStatus).toHaveBeenCalledTimes(1)
    expect(adminAPI.quotaKeys.setStatus).toHaveBeenCalledWith(disabledRow.id, 'active')
    wrapper.unmount()
  })

  it('statusLabel 对 disabled 输出「已停用」', async () => {
    const wrapper = mountView()
    await flushPromises()
    const vm = wrapper.vm as unknown as {
      statusLabel: (row: { status: string; exhausted: boolean }) => string
    }
    expect(vm.statusLabel({ status: 'disabled', exhausted: false })).toBe('已停用')
    // active 仍输出「正常」，不受 disabled 分支影响。
    expect(vm.statusLabel({ status: 'active', exhausted: false })).toBe('正常')
    wrapper.unmount()
  })
})
