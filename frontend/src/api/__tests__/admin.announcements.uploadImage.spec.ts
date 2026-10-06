import { beforeEach, describe, expect, it, vi } from 'vitest'

// 回归测试（2026-09-23 生产实证）：apiClient 实例级默认 Content-Type 为 application/json，
// uploadImage 若不显式覆盖，后端 ParseMultipartForm 收到 application/json 直接报
// "request Content-Type isn't multipart/form-data"。必须随 FormData 显式传 multipart 头。
const { post } = vi.hoisted(() => ({
  post: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { post }
}))

import { uploadImage } from '@/api/admin/announcements'

describe('announcement uploadImage API', () => {
  beforeEach(() => {
    post.mockReset()
    post.mockResolvedValue({ data: { url: 'https://cdn.example.com/a.png' } })
  })

  it('sends FormData with an explicit multipart Content-Type header', async () => {
    const file = new File(['png'], 'a.png', { type: 'image/png' })

    const { url } = await uploadImage(42, file)

    expect(url).toBe('https://cdn.example.com/a.png')
    expect(post).toHaveBeenCalledTimes(1)
    const [path, body, config] = post.mock.calls[0]
    expect(path).toBe('/admin/announcements/42/upload-image')
    expect(body).toBeInstanceOf(FormData)
    expect((body as FormData).get('file')).toBe(file)
    expect(config?.headers?.['Content-Type']).toBe('multipart/form-data')
  })
})
