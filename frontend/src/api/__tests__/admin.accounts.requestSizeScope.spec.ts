import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post } = vi.hoisted(() => ({
  post: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { post }
}))

import { setAccountRequestSizeScope } from '@/api/admin/accounts'

const account = {
  id: 7,
  name: 'lro-account',
  platform: 'anthropic',
  type: 'oauth',
  status: 'active',
  extra: { large_request_only: true }
}

describe('admin accounts request-size-scope API', () => {
  beforeEach(() => {
    post.mockReset()
  })

  it.each([
    ['large_only', { large_request_only: true }],
    ['small_only', { small_request_only: true }],
    ['any', {}]
  ] as const)('posts scope=%s to the dedicated endpoint', async (scope, extra) => {
    const updated = { ...account, extra }
    post.mockResolvedValueOnce({ data: updated })

    const result = await setAccountRequestSizeScope(7, scope)

    expect(post).toHaveBeenCalledWith('/admin/accounts/7/request-size-scope', { scope })
    expect(result).toBe(updated)
    expect(result).not.toHaveProperty('data')
  })

  it('unwraps the account from the response envelope', async () => {
    const updated = { ...account, extra: { small_request_only: true } }
    post.mockResolvedValueOnce({ data: updated })

    const result = await setAccountRequestSizeScope(7, 'small_only')

    expect(post).toHaveBeenCalledTimes(1)
    expect(result.extra.small_request_only).toBe(true)
  })
})
