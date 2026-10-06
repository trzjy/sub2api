import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'

// 后端未接入（T1/T2 并行中）：本 spec 按契约 mock POST /admin/announcements/:id/upload-image
// → {"url": "..."}，错误经 response.ErrorFrom 的 detail 透出。
const {
  listAnnouncements,
  createAnnouncement,
  updateAnnouncement,
  deleteAnnouncement,
  uploadImage,
  getAllGroups,
  showError,
  showSuccess
} = vi.hoisted(() => ({
  listAnnouncements: vi.fn(),
  createAnnouncement: vi.fn(),
  updateAnnouncement: vi.fn(),
  deleteAnnouncement: vi.fn(),
  uploadImage: vi.fn(),
  getAllGroups: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    announcements: {
      list: listAnnouncements,
      getById: vi.fn(),
      create: createAnnouncement,
      update: updateAnnouncement,
      delete: deleteAnnouncement,
      uploadImage,
      getReadStatus: vi.fn()
    },
    groups: { getAll: getAllGroups }
  }
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess })
}))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import AnnouncementsView from '../AnnouncementsView.vue'

const BaseDialogStub = defineComponent({
  props: { show: { type: Boolean, default: false } },
  template: `<div v-if="show" data-testid="edit-dialog"><slot /><slot name="footer" /></div>`
})

function mountView() {
  return mount(AnnouncementsView, {
    attachTo: document.body,
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
        DataTable: true,
        Pagination: true,
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        Select: true,
        EmptyState: true,
        Icon: true,
        AnnouncementTargetingEditor: true,
        AnnouncementReadStatusDialog: true,
        AnnouncementPopup: true
      }
    }
  })
}

const announcement = (over: Record<string, unknown> = {}) => ({
  id: 1,
  title: 't',
  content: 'c',
  status: 'draft',
  notify_mode: 'silent',
  targeting: { any_of: [] },
  starts_at: null,
  ends_at: null,
  created_at: '2026-09-23T00:00:00Z',
  updated_at: '2026-09-23T00:00:00Z',
  ...over
})

const pngFile = (name = 'a.png') => new File(['x'], name, { type: 'image/png' })

function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

async function flush() {
  await flushPromises()
  await flushPromises()
  await flushPromises()
}

async function openCreateDialog(wrapper: ReturnType<typeof mountView>) {
  const createBtn = wrapper.findAll('button').find((b) => b.text() === 'admin.announcements.createAnnouncement')
  await createBtn!.trigger('click')
}

async function pasteImage(wrapper: ReturnType<typeof mountView>, file: File) {
  const textarea = wrapper.find('textarea')
  await textarea.trigger('paste', {
    clipboardData: {
      items: [{ kind: 'file', type: 'image/png', getAsFile: () => file }]
    }
  })
}

async function clickCancel(wrapper: ReturnType<typeof mountView>) {
  const cancelBtn = wrapper.findAll('button').find((b) => b.text() === 'common.cancel')
  await cancelBtn!.trigger('click')
}

beforeEach(() => {
  listAnnouncements.mockReset().mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
  createAnnouncement.mockReset().mockResolvedValue(announcement({ id: 42 }))
  updateAnnouncement.mockReset().mockResolvedValue(announcement())
  deleteAnnouncement.mockReset().mockResolvedValue({ message: 'ok' })
  uploadImage.mockReset().mockResolvedValue({ url: 'https://cdn.example.com/a.png' })
  getAllGroups.mockReset().mockResolvedValue([])
  showError.mockReset()
  showSuccess.mockReset()
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe('AnnouncementsView image upload', () => {
  it('creates a placeholder draft then uploads on paste and inserts the markdown image', async () => {
    const wrapper = mountView()
    await flush()
    await openCreateDialog(wrapper)

    await pasteImage(wrapper, pngFile())
    await flush()

    // 占位草稿：status=draft + 占位 title/content 必过后端非空校验
    expect(createAnnouncement).toHaveBeenCalledTimes(1)
    expect(createAnnouncement).toHaveBeenCalledWith(
      expect.objectContaining({
        status: 'draft',
        title: expect.stringContaining('admin.announcements.untitledDraft'),
        content: 'admin.announcements.draftPlaceholder'
      })
    )
    expect(uploadImage).toHaveBeenCalledTimes(1)
    expect(uploadImage).toHaveBeenCalledWith(42, expect.any(File))

    const markdown = '![announcement-image](https://cdn.example.com/a.png)'
    const textarea = wrapper.find('textarea')
    expect((textarea.element as HTMLTextAreaElement).value).toContain(markdown)
    expect(showSuccess).toHaveBeenCalledWith('admin.announcements.uploadSuccess')
  })

  it('keeps the POST create path when saving in create mode without any image', async () => {
    const wrapper = mountView()
    await flush()
    await openCreateDialog(wrapper)

    const dialog = wrapper.find('[data-testid="edit-dialog"]')
    await dialog.find('input[type="text"]').setValue('My Post')
    await dialog.find('textarea').setValue('Body')

    await dialog.find('form').trigger('submit')
    await flush()

    expect(createAnnouncement).toHaveBeenCalledTimes(1)
    expect(createAnnouncement).toHaveBeenCalledWith(
      expect.objectContaining({ title: 'My Post', content: 'Body' })
    )
    expect(updateAnnouncement).not.toHaveBeenCalled()
    expect(uploadImage).not.toHaveBeenCalled()
  })

  it('deletes the placeholder draft on cancel when the title is unchanged', async () => {
    const wrapper = mountView()
    await flush()
    await openCreateDialog(wrapper)

    await pasteImage(wrapper, pngFile())
    await flush()
    expect(createAnnouncement).toHaveBeenCalledTimes(1)

    await clickCancel(wrapper)
    await flush()

    expect(deleteAnnouncement).toHaveBeenCalledTimes(1)
    expect(deleteAnnouncement).toHaveBeenCalledWith(42)
    expect(wrapper.find('[data-testid="edit-dialog"]').exists()).toBe(false)
  })

  it('keeps the draft on cancel when the title was substantively changed', async () => {
    const wrapper = mountView()
    await flush()
    await openCreateDialog(wrapper)

    await pasteImage(wrapper, pngFile())
    await flush()

    const dialog = wrapper.find('[data-testid="edit-dialog"]')
    await dialog.find('input[type="text"]').setValue('Renamed by admin')

    await clickCancel(wrapper)
    await flush()

    expect(deleteAnnouncement).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="edit-dialog"]').exists()).toBe(false)
  })

  it('shows the backend detail toast on upload failure and leaves content unchanged', async () => {
    uploadImage.mockRejectedValue({ response: { data: { detail: '图片存储未启用' } } })
    const wrapper = mountView()
    await flush()
    await openCreateDialog(wrapper)

    await pasteImage(wrapper, pngFile())
    await flush()

    expect(createAnnouncement).toHaveBeenCalledTimes(1)
    expect(uploadImage).toHaveBeenCalledTimes(1)
    const textarea = wrapper.find('textarea')
    expect((textarea.element as HTMLTextAreaElement).value).not.toContain('announcement-image')
    expect(showError).toHaveBeenCalledWith('图片存储未启用')
    expect(wrapper.find('[data-testid="announcement-upload-error"]').text()).toBe('图片存储未启用')
  })

  it('serializes uploads (R2): save/cancel are no-ops while uploading, second paste queues, one draft only', async () => {
    const createD = deferred<Record<string, unknown>>()
    const up1 = deferred<{ url: string }>()
    createAnnouncement.mockReturnValue(createD.promise)
    uploadImage.mockReturnValueOnce(up1.promise).mockResolvedValueOnce({ url: 'https://cdn.example.com/b.png' })

    const wrapper = mountView()
    await flush()
    await openCreateDialog(wrapper)

    await pasteImage(wrapper, pngFile('1.png'))
    await flushPromises()
    // 建草稿请求进行中 → uploading 互斥已生效
    expect(wrapper.find('[data-testid="announcement-upload-loading"]').exists()).toBe(true)

    // 上传中再次贴图 → 入队，不并发建草稿
    await pasteImage(wrapper, pngFile('2.png'))
    expect(createAnnouncement).toHaveBeenCalledTimes(1)
    expect(uploadImage).toHaveBeenCalledTimes(0)

    // 上传中保存 → no-op
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(createAnnouncement).toHaveBeenCalledTimes(1)
    expect(updateAnnouncement).not.toHaveBeenCalled()

    // 上传中取消 → no-op，对话框保持打开
    await clickCancel(wrapper)
    await flushPromises()
    expect(wrapper.find('[data-testid="edit-dialog"]').exists()).toBe(true)
    expect(deleteAnnouncement).not.toHaveBeenCalled()

    createD.resolve(announcement({ id: 42 }))
    await flushPromises()
    expect(uploadImage).toHaveBeenCalledTimes(1)
    expect(uploadImage).toHaveBeenCalledWith(42, expect.any(File))

    up1.resolve({ url: 'https://cdn.example.com/a.png' })
    await flush()
    // 两张图串行完成：仅一个占位草稿、两次上传
    expect(createAnnouncement).toHaveBeenCalledTimes(1)
    expect(uploadImage).toHaveBeenCalledTimes(2)
    const value = (wrapper.find('textarea').element as HTMLTextAreaElement).value
    expect(value).toContain('![announcement-image](https://cdn.example.com/a.png)')
    expect(value).toContain('![announcement-image](https://cdn.example.com/b.png)')
    expect(wrapper.find('[data-testid="announcement-upload-loading"]').exists()).toBe(false)

    // 上传结束后取消 → 清理占位草稿并关闭
    await clickCancel(wrapper)
    await flush()
    expect(deleteAnnouncement).toHaveBeenCalledWith(42)
    expect(wrapper.find('[data-testid="edit-dialog"]').exists()).toBe(false)
  })

  it('does not intercept plain text paste', async () => {
    const wrapper = mountView()
    await flush()
    await openCreateDialog(wrapper)

    const textarea = wrapper.find('textarea')
    await textarea.trigger('paste', {
      clipboardData: { items: [{ kind: 'string', type: 'text/plain' }] }
    })
    await flush()

    expect(createAnnouncement).not.toHaveBeenCalled()
    expect(uploadImage).not.toHaveBeenCalled()
  })

  it('uploads via the insert-image fallback button and hidden file input', async () => {
    const wrapper = mountView()
    await flush()
    await openCreateDialog(wrapper)

    const btn = wrapper.find('[data-testid="announcement-insert-image-btn"]')
    expect(btn.exists()).toBe(true)
    expect(wrapper.find('[data-testid="announcement-image-paste-hint"]').exists()).toBe(true)

    const input = wrapper.find('input[type="file"]')
    Object.defineProperty(input.element, 'files', { value: [pngFile()] })
    await input.trigger('change')
    await flush()

    expect(uploadImage).toHaveBeenCalledTimes(1)
    expect(uploadImage).toHaveBeenCalledWith(42, expect.any(File))
    expect((wrapper.find('textarea').element as HTMLTextAreaElement).value).toContain(
      '![announcement-image](https://cdn.example.com/a.png)'
    )
  })
})

describe('AnnouncementsView editor preview tab', () => {
  it('renders markdown content in the preview tab and restores the textarea on switch back', async () => {
    const wrapper = mountView()
    await flush()
    await openCreateDialog(wrapper)

    const dialog = wrapper.find('[data-testid="edit-dialog"]')
    await dialog.find('textarea').setValue('# Hello\n\n**bold** text')

    // 默认编辑态：预览容器不存在
    expect(wrapper.find('[data-testid="announcement-content-preview"]').exists()).toBe(false)

    await wrapper.find('[data-testid="announcement-editor-tab-preview"]').trigger('click')

    // 预览态：真实 MarkdownContent 渲染，编辑件隐藏
    const preview = wrapper.find('[data-testid="announcement-content-preview"]')
    expect(preview.exists()).toBe(true)
    expect(preview.find('h1').text()).toBe('Hello')
    expect(preview.find('strong').text()).toBe('bold')
    expect(wrapper.find('textarea').exists()).toBe(false)
    expect(wrapper.find('[data-testid="announcement-insert-image-btn"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="announcement-image-paste-hint"]').exists()).toBe(false)

    // 切回编辑态：textarea 恢复且内容保留，预览容器消失
    await wrapper.find('[data-testid="announcement-editor-tab-edit"]').trigger('click')

    const textarea = wrapper.find('textarea')
    expect(textarea.exists()).toBe(true)
    expect((textarea.element as HTMLTextAreaElement).value).toBe('# Hello\n\n**bold** text')
    expect(wrapper.find('[data-testid="announcement-content-preview"]').exists()).toBe(false)
  })

  it('resets to the edit tab when the dialog is reopened', async () => {
    const wrapper = mountView()
    await flush()
    await openCreateDialog(wrapper)

    await wrapper.find('[data-testid="announcement-editor-tab-preview"]').trigger('click')
    expect(wrapper.find('[data-testid="announcement-content-preview"]').exists()).toBe(true)

    await clickCancel(wrapper)
    await flush()
    await openCreateDialog(wrapper)

    expect(wrapper.find('[data-testid="announcement-content-preview"]').exists()).toBe(false)
    expect(wrapper.find('textarea').exists()).toBe(true)
  })
})
