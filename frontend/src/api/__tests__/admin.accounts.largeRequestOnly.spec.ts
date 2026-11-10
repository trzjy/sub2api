import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post } = vi.hoisted(() => ({
  post: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { post }
}))

import { setAccountLargeRequestOnly } from '@/api/admin/accounts'

const account = {
  id: 7,
  name: 'lro-account',
  platform: 'anthropic',
  type: 'oauth',
  status: 'active',
  extra: { large_request_only: true }
}

describe('admin accounts large-request-only API', () => {
  beforeEach(() => {
    post.mockReset()
  })

  it('posts to the dedicated endpoint with enabled=true', async () => {
    post.mockResolvedValueOnce({ data: account })

    await expect(setAccountLargeRequestOnly(7, true)).resolves.toEqual(account)
    expect(post).toHaveBeenCalledWith('/admin/accounts/7/large-request-only', { enabled: true })
  })

  it('posts enabled=false and unwraps the account from the response envelope', async () => {
    const disabled = { ...account, extra: { large_request_only: false } }
    post.mockResolvedValueOnce({ data: disabled })

    const result = await setAccountLargeRequestOnly(7, false)

    expect(post).toHaveBeenCalledWith('/admin/accounts/7/large-request-only', { enabled: false })
    expect(post).toHaveBeenCalledTimes(1)
    // 返回值必须是响应体本身，而不是 axios response 包装对象
    expect(result).toBe(disabled)
    expect(result).not.toHaveProperty('data')
  })
})
