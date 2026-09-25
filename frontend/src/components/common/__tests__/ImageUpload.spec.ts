import { mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import ImageUpload from '../ImageUpload.vue'

type MountOptions = Parameters<typeof mount>[1]

function mountUpload(options: MountOptions = {}) {
  return mount(ImageUpload, {
    props: { modelValue: '', ...(options.props as Record<string, unknown>) },
    global: { stubs: { Icon: true }, ...(options.global as object) }
  }) as VueWrapper
}

const dropzone = (wrapper: VueWrapper) => wrapper.find('[data-testid="image-upload-dropzone"]')
const errorText = (wrapper: VueWrapper) => wrapper.find('.text-red-500').exists()
  ? wrapper.findAll('p').map((p) => p.text()).join(' ')
  : ''

// jsdom 不实现 FileReader：按仓库既有 mock 惯例，用可断言的假实现替换。
// readAsDataURL/readAsText 分别回填对应 result，onload 同步派发。
function installFileReaderMock() {
  class MockFileReader {
    onload: ((e: { target?: { result: unknown } }) => void) | null = null
    onerror: (() => void) | null = null

    readAsDataURL(file: File) {
      this.onload?.({ target: { result: `data:${file.type};base64,bW9jay1kYXRh` } })
    }

    readAsText(_file: File) {
      this.onload?.({ target: { result: mockTextResult } })
    }
  }

  const original = globalThis.FileReader
  vi.stubGlobal('FileReader', MockFileReader)
  return () => vi.stubGlobal('FileReader', original)
}

let mockTextResult = '<svg viewBox="0 0 1 1"></svg>'
let restoreFileReader: (() => void) | null = null

beforeEach(() => {
  mockTextResult = '<svg viewBox="0 0 1 1"></svg>'
  restoreFileReader = installFileReaderMock()
})

afterEach(() => {
  restoreFileReader?.()
  restoreFileReader = null
  vi.unstubAllGlobals()
})

const pngFile = (over: Partial<{ name: string; type: string; size: number }> = {}) =>
  new File(['x'], over.name ?? 'a.png', { type: over.type ?? 'image/png' })

async function drop(wrapper: VueWrapper, file?: File) {
  await dropzone(wrapper).trigger('drop', {
    dataTransfer: file ? { files: [file] } : { files: [] }
  })
}

describe('ImageUpload drag & drop', () => {
  it('emits a dataURL when a valid image is dropped in image mode', async () => {
    const wrapper = mountUpload()

    await drop(wrapper, pngFile())

    expect(wrapper.emitted('update:modelValue')).toHaveLength(1)
    expect(wrapper.emitted('update:modelValue')![0]).toEqual(['data:image/png;base64,bW9jay1kYXRh'])
    expect(errorText(wrapper)).toBe('')
  })

  it('shows the too-large error and does not emit when the dropped file exceeds maxSize', async () => {
    const wrapper = mountUpload({ props: { maxSize: 1024 } })
    const big = pngFile()
    Object.defineProperty(big, 'size', { value: 4096 })

    await drop(wrapper, big)

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(errorText(wrapper)).toContain('common.fileTooLargeKb')
  })

  it('shows selectImageFile and does not emit for a non-image drop in image mode', async () => {
    const wrapper = mountUpload()
    const text = new File(['hello'], 'a.txt', { type: 'text/plain' })

    await drop(wrapper, text)

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(errorText(wrapper)).toContain('common.selectImageFile')
  })

  it('emits the svg text content when an svg file is dropped in svg mode', async () => {
    const wrapper = mountUpload({ props: { mode: 'svg' } })
    const svg = new File(['<svg/>'], 'a.svg', { type: 'image/svg+xml' })

    await drop(wrapper, svg)

    expect(wrapper.emitted('update:modelValue')).toEqual([['<svg viewBox="0 0 1 1"></svg>']])
  })

  it('ignores a drop that carries no file', async () => {
    const wrapper = mountUpload()

    await drop(wrapper)

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(errorText(wrapper)).toBe('')
  })

  it('highlights on dragenter and keeps the highlight while a child element is entered', async () => {
    const wrapper = mountUpload()
    expect(dropzone(wrapper).classes()).not.toContain('border-blue-500')

    await dropzone(wrapper).trigger('dragenter')
    expect(dropzone(wrapper).classes()).toContain('border-blue-500')
    expect(dropzone(wrapper).classes()).toContain('bg-blue-50')

    // 进入子元素（img/svg）再离开：计数器不归零，高亮不抖动
    await dropzone(wrapper).trigger('dragenter')
    await dropzone(wrapper).trigger('dragleave')
    expect(dropzone(wrapper).classes()).toContain('border-blue-500')

    await dropzone(wrapper).trigger('dragleave')
    expect(dropzone(wrapper).classes()).not.toContain('border-blue-500')
  })

  it('clears the highlight on drop', async () => {
    const wrapper = mountUpload()

    await dropzone(wrapper).trigger('dragenter')
    expect(dropzone(wrapper).classes()).toContain('border-blue-500')

    await drop(wrapper, pngFile())

    expect(dropzone(wrapper).classes()).not.toContain('border-blue-500')
    expect(dropzone(wrapper).classes()).not.toContain('bg-blue-50')
  })

  it('never lets the drag depth counter go negative', async () => {
    const wrapper = mountUpload()

    await dropzone(wrapper).trigger('dragleave')
    await dropzone(wrapper).trigger('dragleave')

    expect(dropzone(wrapper).classes()).not.toContain('border-blue-500')
  })
})

describe('ImageUpload file input regression', () => {
  it('still emits a dataURL through the input change path', async () => {
    const wrapper = mountUpload()
    const input = wrapper.find('input[type="file"]')
    Object.defineProperty(input.element, 'files', { value: [pngFile()] })

    await input.trigger('change')

    expect(wrapper.emitted('update:modelValue')).toEqual([['data:image/png;base64,bW9jay1kYXRh']])
  })

  it('still surfaces the too-large error through the input change path', async () => {
    const wrapper = mountUpload({ props: { maxSize: 1024 } })
    const input = wrapper.find('input[type="file"]')
    const big = pngFile()
    Object.defineProperty(big, 'size', { value: 4096 })
    Object.defineProperty(input.element, 'files', { value: [big] })

    await input.trigger('change')

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(errorText(wrapper)).toContain('common.fileTooLargeKb')
  })

  it('still reads svg text through the input change path', async () => {
    const wrapper = mountUpload({ props: { mode: 'svg' } })
    const input = wrapper.find('input[type="file"]')
    Object.defineProperty(input.element, 'files', {
      value: [new File(['<svg/>'], 'a.svg', { type: 'image/svg+xml' })]
    })

    await input.trigger('change')

    expect(wrapper.emitted('update:modelValue')).toEqual([['<svg viewBox="0 0 1 1"></svg>']])
  })
})
