import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import AccountBulkActionsBar from '../AccountBulkActionsBar.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

describe('AccountBulkActionsBar', () => {
  it('allows selecting all results before any row is selected', async () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [],
        totalResults: 45,
        selectingAll: false,
        allResultsSelected: false
      }
    })

    const button = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.selectAllResults')
    )

    expect(button).toBeDefined()
    await button!.trigger('click')
    expect(wrapper.emitted('select-all-results')).toHaveLength(1)
  })

  it('preserves the upstream billing probe action from v0.1.166', async () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1],
        totalResults: 45,
        selectingAll: false,
        allResultsSelected: false
      }
    })

    const button = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.probeUpstreamBilling')
    )

    expect(button).toBeDefined()
    await button!.trigger('click')
    expect(wrapper.emitted('probe-upstream-billing')).toHaveLength(1)
  })
})

// 平台归并后 web 专属按钮的可见性由 access_mode 而非仅 platform 决定。
describe('AccountBulkActionsBar web access-mode convergence', () => {
  const webAccessAccount = {
    platform: 'deepseek',
    credentials: { access_mode: 'web', cookie: 'ck' }
  }
  // 同平台普通 API 账号：platform 命中官方平台但无 access_mode。
  const apiAccount = { platform: 'zhipu', credentials: { api_key: 'sk-x' } }

  const webButtonTexts = (wrapper: ReturnType<typeof mount>): string[] =>
    wrapper
      .findAll('button')
      .map(item => item.text())
      .filter(text => text.startsWith('admin.accounts.batch.'))

  it('hides all six web buttons when only ordinary API accounts are selected', () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1, 2],
        selectedAccounts: [apiAccount, { platform: 'zhipu' }],
        totalResults: 2,
        selectingAll: false,
        allResultsSelected: false
      }
    })

    expect(webButtonTexts(wrapper)).toHaveLength(0)
    // 非 Web 按钮行为保持不变
    const texts = wrapper.findAll('button').map(item => item.text())
    expect(texts).toContain('admin.accounts.bulkActions.delete')
    expect(texts).toContain('admin.accounts.bulkActions.probeUpstreamBilling')
    expect(texts).toContain('admin.accounts.bulkActions.edit')
  })

  it('hides the web buttons when the selection mixes a web and an API account', () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1, 2],
        selectedAccounts: [webAccessAccount, apiAccount],
        totalResults: 2,
        selectingAll: false,
        allResultsSelected: false
      }
    })

    expect(webButtonTexts(wrapper)).toHaveLength(0)
  })

  it('renders all six web buttons when every selected account is web access mode', () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1, 2],
        selectedAccounts: [
          webAccessAccount,
          { platform: 'kimi', credentials: { access_mode: 'web', access_token: 't' } }
        ],
        totalResults: 2,
        selectingAll: false,
        allResultsSelected: false
      }
    })

    expect(webButtonTexts(wrapper)).toEqual([
      'admin.accounts.batch.login',
      'admin.accounts.batch.test',
      'admin.accounts.batch.deleteBanned',
      'admin.accounts.batch.enable',
      'admin.accounts.batch.disable',
      'admin.accounts.batch.export'
    ])
  })

  it('hides the web buttons when no selected account list is provided', () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1],
        totalResults: 1,
        selectingAll: false,
        allResultsSelected: false
      }
    })

    expect(webButtonTexts(wrapper)).toHaveLength(0)
  })
})
