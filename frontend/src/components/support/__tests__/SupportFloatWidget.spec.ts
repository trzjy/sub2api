import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import SupportFloatWidget from '../SupportFloatWidget.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

const QR_URL = 'data:image/png;base64,iVBORw0KGgo='

async function mountWidget(): Promise<ReturnType<typeof mount>> {
  const { useAppStore } = await import('@/stores/app')
  const appStore = useAppStore()
  appStore.supportQrcodeUrl = QR_URL
  return mount(SupportFloatWidget)
}

describe('SupportFloatWidget', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
  })

  it('默认直接展示二维码卡片(未收起过)', async () => {
    const wrapper = await mountWidget()
    expect(wrapper.find('img').attributes('src')).toBe(QR_URL)
    expect(wrapper.find('button[aria-label="supportWidget.title"]').exists()).toBe(false)
  })

  it('点 X 收起为圆钮并写入 localStorage', async () => {
    const wrapper = await mountWidget()
    await wrapper.find('button[aria-label="supportWidget.close"]').trigger('click')
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.find('button[aria-label="supportWidget.title"]').exists()).toBe(true)
    expect(localStorage.getItem('support_widget_collapsed')).toBe('1')
  })

  it('收起过的设备再进页面只出圆钮,不再自动弹卡片', async () => {
    localStorage.setItem('support_widget_collapsed', '1')
    const wrapper = await mountWidget()
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.find('button[aria-label="supportWidget.title"]').exists()).toBe(true)
  })

  it('点圆钮展开后清除收起记忆,刷新后保持展开', async () => {
    localStorage.setItem('support_widget_collapsed', '1')
    const wrapper = await mountWidget()
    await wrapper.find('button[aria-label="supportWidget.title"]').trigger('click')
    expect(wrapper.find('img').exists()).toBe(true)
    expect(localStorage.getItem('support_widget_collapsed')).toBeNull()
  })

  it('二维码 144px / 卡片 192px(2026-09-24 缩小裁定)', async () => {
    const wrapper = await mountWidget()
    expect(wrapper.find('img').classes()).toContain('h-36')
    expect(wrapper.find('img').classes()).toContain('w-36')
    const card = wrapper.find('img').element.parentElement as HTMLElement
    expect(card.className).toContain('w-48')
  })
})
