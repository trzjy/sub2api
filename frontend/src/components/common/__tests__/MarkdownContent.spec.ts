import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import MarkdownContent from '../MarkdownContent.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

// 1x1 透明 PNG，DOMPurify 默认放行 data: URI 图片（实测确认），可真实走点击放大链路
const PIXEL_IMAGE_DATA_URI =
  'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=='

describe('MarkdownContent', () => {
  afterEach(() => {
    document.body.innerHTML = ''
  })

  it('renders markdown headings and lists into the shared markdown-body container', async () => {
    const wrapper = mount(MarkdownContent, {
      attachTo: document.body,
      props: {
        content: ['## Markdown heading', '', '- first item', '- second item'].join('\n'),
      },
    })
    await wrapper.vm.$nextTick()

    const content = document.body.querySelector('[data-testid="markdown-content"]')
    expect(content?.querySelector('h2')?.textContent).toBe('Markdown heading')
    expect(content?.querySelectorAll('li')).toHaveLength(2)
    expect(content?.querySelector('li')?.textContent).toBe('first item')
    expect(content?.querySelector('ul')).not.toBeNull()

    wrapper.unmount()
  })

  it('sanitizes injected script tags through DOMPurify', async () => {
    const wrapper = mount(MarkdownContent, {
      attachTo: document.body,
      props: {
        content:
          '## Safe heading\n\n<script>window.__markdownXss = true</script><img src="x" onerror="window.__markdownXssOnError = true">',
      },
    })
    await wrapper.vm.$nextTick()

    const content = document.body.querySelector('[data-testid="markdown-content"]')
    expect(content?.querySelector('h2')?.textContent).toBe('Safe heading')
    expect(content?.querySelector('script')).toBeNull()
    // 内联事件被剥除，img 本体保留（alt/src 等安全属性不受影响）
    expect(content?.querySelector('img')?.getAttribute('onerror')).toBeNull()
    expect(content?.querySelector('img')).not.toBeNull()

    wrapper.unmount()
  })

  it('renders an empty body when content is an empty string', async () => {
    const wrapper = mount(MarkdownContent, {
      attachTo: document.body,
      props: {
        content: '',
      },
    })
    await wrapper.vm.$nextTick()

    const content = document.body.querySelector('[data-testid="markdown-content"]')
    expect(content).not.toBeNull()
    expect(content?.innerHTML).toBe('')
    expect(document.body.querySelector('[data-testid="markdown-image-zoom"]')).toBeNull()

    wrapper.unmount()
  })

  it('opens the image zoom overlay on image click and closes it from the close button', async () => {
    const wrapper = mount(MarkdownContent, {
      attachTo: document.body,
      props: {
        content: `before\n\n![pixel](${PIXEL_IMAGE_DATA_URI})\n\nafter`,
      },
    })
    await wrapper.vm.$nextTick()

    // DOMPurify 净化后 img 真实存在，data: URI 保留
    const image = document.body.querySelector<HTMLImageElement>(
      '[data-testid="markdown-content"] img',
    )
    expect(image).not.toBeNull()
    expect(image?.getAttribute('src')).toBe(PIXEL_IMAGE_DATA_URI)
    expect(document.body.querySelector('[data-testid="markdown-image-zoom"]')).toBeNull()

    image?.click()
    await wrapper.vm.$nextTick()

    const zoom = document.body.querySelector('[data-testid="markdown-image-zoom"]')
    expect(zoom).not.toBeNull()
    expect(zoom?.querySelector('img')?.getAttribute('src')).toBe(PIXEL_IMAGE_DATA_URI)
    expect(
      document.body.querySelector('[data-testid="markdown-image-zoom-close"]'),
    ).not.toBeNull()

    const closeButton = document.body.querySelector<HTMLButtonElement>(
      '[data-testid="markdown-image-zoom-close"]',
    )
    closeButton?.click()
    await wrapper.vm.$nextTick()

    expect(document.body.querySelector('[data-testid="markdown-image-zoom"]')).toBeNull()

    wrapper.unmount()
  })

  it('closes the image zoom overlay when clicking the backdrop', async () => {
    const wrapper = mount(MarkdownContent, {
      attachTo: document.body,
      props: {
        content: `![pixel](${PIXEL_IMAGE_DATA_URI})`,
      },
    })
    await wrapper.vm.$nextTick()

    const image = document.body.querySelector<HTMLImageElement>(
      '[data-testid="markdown-content"] img',
    )
    image?.click()
    await wrapper.vm.$nextTick()
    expect(document.body.querySelector('[data-testid="markdown-image-zoom"]')).not.toBeNull()

    const backdrop = document.body.querySelector<HTMLElement>('[data-testid="markdown-image-zoom"]')
    backdrop?.click()
    await wrapper.vm.$nextTick()

    expect(document.body.querySelector('[data-testid="markdown-image-zoom"]')).toBeNull()

    wrapper.unmount()
  })
})
