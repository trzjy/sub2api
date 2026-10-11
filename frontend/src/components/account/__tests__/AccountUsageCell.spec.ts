import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account } from '@/types'

const { getUsage } = vi.hoisted(() => ({
  getUsage: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getUsage
    },
    cnProviders: {
      queryQuota: vi.fn().mockResolvedValue({ success: true, tiers: [] }),
      queryBalance: vi.fn().mockResolvedValue({ success: true })
    }
  }
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

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'antigravity',
    type: 'oauth',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: '2026-03-15T00:00:00Z',
    updated_at: '2026-03-15T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  }
}

function makeOllamaUsage(accountId: number, overrides: Partial<NonNullable<Account['ollama_cloud_usage']>> = {}) {
  return {
    account_id: accountId,
    eligible: true,
    configured: true,
    auto_refresh_enabled: true,
    encryption_key_configured: true,
    snapshot: {
      status: 'ok' as const,
      last_attempt_at: '2026-07-23T00:00:00Z',
      next_refresh_at: '2026-07-23T01:00:00Z',
      data: {
        five_hour: { used_percent: 12 },
        seven_day: { used_percent: 34 }
      }
    },
    ...overrides,
  }
}

// CN 平台 Ollama Cloud 用例共用的子组件 stub：按 data-test 断言渲染与否
const cnUsageCellStubs = {
  OllamaCloudUsageCell: {
    props: ['account'],
    template: '<div data-test="embedded-ollama">ollama</div>'
  },
  CNProviderQuotaCell: {
    template: '<div data-test="cn-quota-cell" />'
  },
  CNProviderBalanceCell: {
    template: '<div data-test="cn-balance-cell" />'
  }
}

describe('AccountUsageCell', () => {
  beforeEach(() => {
    getUsage.mockReset()
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockImplementation(() => ({
        matches: true,
        media: '(min-width: 768px)',
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn(),
      }))
    })
  })

  it('renders eligible Ollama Cloud state and forwards query updates', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9001,
          platform: 'openai',
          type: 'apikey',
          ollama_cloud_usage: {
            account_id: 9001,
            eligible: true,
            configured: true,
            auto_refresh_enabled: true,
            encryption_key_configured: true,
            snapshot: {
              status: 'ok',
              last_attempt_at: '2026-07-23T00:00:00Z',
              next_refresh_at: '2026-07-23T01:00:00Z',
              data: {
                five_hour: { used_percent: 12 },
                seven_day: { used_percent: 34 }
              }
            }
          }
        })
      },
      global: {
        stubs: {
          OllamaCloudUsageCell: {
            props: ['account'],
            emits: ['updated'],
            template: '<button data-test="embedded-ollama" @click="$emit(\'updated\', { ...account.ollama_cloud_usage, auto_refresh_enabled: false })">{{ account.ollama_cloud_usage.snapshot.data.five_hour.used_percent }}</button>'
          },
          UsageProgressBar: true,
          AccountQuotaInfo: true
        }
      }
    })

    expect(wrapper.get('[data-test="embedded-ollama"]').text()).toBe('12')
    expect(getUsage).not.toHaveBeenCalled()

    await wrapper.get('[data-test="embedded-ollama"]').trigger('click')

    const updatedAccount = wrapper.emitted<Account[]>('account-updated')?.[0]?.[0]
    expect(updatedAccount?.id).toBe(9001)
    expect(updatedAccount?.ollama_cloud_usage?.auto_refresh_enabled).toBe(false)
  })

  it.each(['kimi', 'zhipu', 'deepseek', 'minimax'] as const)(
    '%s apikey 账号 Ollama Cloud eligible 时渲染 Ollama 用量单元格并跳过 CN 子单元格',
    async (platform) => {
      const wrapper = mount(AccountUsageCell, {
        props: {
          account: makeAccount({
            id: 9002,
            platform,
            type: 'apikey',
            credentials: { account_mode: 'coding' },
            ollama_cloud_usage: makeOllamaUsage(9002)
          })
        },
        global: {
          stubs: { ...cnUsageCellStubs, UsageProgressBar: true, AccountQuotaInfo: true }
        }
      })

      await flushPromises()

      expect(wrapper.find('[data-test="embedded-ollama"]').exists()).toBe(true)
      expect(wrapper.find('[data-test="cn-quota-cell"]').exists()).toBe(false)
      expect(wrapper.find('[data-test="cn-balance-cell"]').exists()).toBe(false)
      expect(wrapper.find('div[title="admin.accounts.cnProviders.noBalanceEndpoint"]').exists()).toBe(false)
      expect(getUsage).not.toHaveBeenCalled()
    }
  )

  it('CN 平台 Ollama Cloud eligible 账号的用量更新经 account-updated 透传', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9003,
          platform: 'kimi',
          type: 'apikey',
          credentials: { account_mode: 'coding' },
          ollama_cloud_usage: makeOllamaUsage(9003)
        })
      },
      global: {
        stubs: {
          ...cnUsageCellStubs,
          OllamaCloudUsageCell: {
            props: ['account'],
            emits: ['updated'],
            template: '<button data-test="embedded-ollama" @click="$emit(\'updated\', { ...account.ollama_cloud_usage, auto_refresh_enabled: false })" />'
          },
          UsageProgressBar: true,
          AccountQuotaInfo: true
        }
      }
    })

    await wrapper.get('[data-test="embedded-ollama"]').trigger('click')

    const updatedAccount = wrapper.emitted<Account[]>('account-updated')?.[0]?.[0]
    expect(updatedAccount?.id).toBe(9003)
    expect(updatedAccount?.ollama_cloud_usage?.auto_refresh_enabled).toBe(false)
  })

  it.each([
    { name: '无 ollama_cloud_usage', usage: undefined },
    { name: 'eligible=false', usage: makeOllamaUsage(9004, { eligible: false }) }
  ])('普通 kimi apikey 账号（$name）仍渲染 CN 子单元格', async ({ usage }) => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9004,
          platform: 'kimi',
          type: 'apikey',
          credentials: { account_mode: 'coding' },
          ollama_cloud_usage: usage
        })
      },
      global: {
        stubs: { ...cnUsageCellStubs, UsageProgressBar: true, AccountQuotaInfo: true }
      }
    })

    await flushPromises()

    expect(wrapper.find('[data-test="cn-quota-cell"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="cn-balance-cell"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="embedded-ollama"]').exists()).toBe(false)
  })

  it('Antigravity 图片用量会聚合新旧 image 模型', async () => {
    getUsage.mockResolvedValue({
      antigravity_quota: {
        'gemini-2.5-flash-image': {
          utilization: 45,
          reset_time: '2026-03-01T11:00:00Z'
        },
        'gemini-3.1-flash-image': {
          utilization: 20,
          reset_time: '2026-03-01T10:00:00Z'
        },
        'gemini-3-pro-image': {
          utilization: 70,
          reset_time: '2026-03-01T09:00:00Z'
        }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 1001,
          platform: 'antigravity',
          type: 'oauth',
          extra: {}
        })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ resetsAt }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.usageWindow.gemini3Image|70|2026-03-01T09:00:00Z')
  })

  it('Antigravity 会显示 AI Credits 余额信息', async () => {
    getUsage.mockResolvedValue({
      ai_credits: [
        {
          credit_type: 'GOOGLE_ONE_AI',
          amount: 25,
          minimum_balance: 5
        }
      ]
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 1002,
          platform: 'antigravity',
          type: 'oauth',
          extra: {}
        })
      },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.aiCreditsBalance')
    expect(wrapper.text()).toContain('25')
  })


  it('OpenAI OAuth 快照已过期时首屏会重新请求 usage', async () => {
    getUsage.mockResolvedValue({
      five_hour: {
        utilization: 15,
        resets_at: '2026-03-08T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 3,
          tokens: 300,
          cost: 0.03,
          standard_cost: 0.03,
          user_cost: 0.03
        }
      },
      seven_day: {
        utilization: 77,
        resets_at: '2026-03-13T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 3,
          tokens: 300,
          cost: 0.03,
          standard_cost: 0.03,
          user_cost: 0.03
        }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2000,
          platform: 'openai',
          type: 'oauth',
          extra: {
            codex_usage_updated_at: '2026-03-07T00:00:00Z',
            codex_5h_used_percent: 12,
            codex_5h_reset_at: '2026-03-08T12:00:00Z',
            codex_7d_used_percent: 34,
            codex_7d_reset_at: '2026-03-13T12:00:00Z'
          }
        })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'windowStats', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(getUsage).toHaveBeenCalledWith(2000)
    expect(wrapper.text()).toContain('5h|15|300')
    expect(wrapper.text()).toContain('7d|77|300')
  })

  it('OpenAI OAuth 有 codex 快照时仍然使用 /usage API 数据渲染', async () => {
    getUsage.mockResolvedValue({
      five_hour: {
        utilization: 18,
        resets_at: '2099-03-07T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 9,
          tokens: 900,
          cost: 0.09,
          standard_cost: 0.09,
          user_cost: 0.09
        }
      },
      seven_day: {
        utilization: 36,
        resets_at: '2099-03-13T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 9,
          tokens: 900,
          cost: 0.09,
          standard_cost: 0.09,
          user_cost: 0.09
        }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2001,
          platform: 'openai',
          type: 'oauth',
          extra: {
            codex_usage_updated_at: '2099-03-07T10:00:00Z',
            codex_5h_used_percent: 12,
            codex_5h_reset_at: '2099-03-07T12:00:00Z',
            codex_7d_used_percent: 34,
            codex_7d_reset_at: '2099-03-13T12:00:00Z'
          }
        })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'windowStats', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(getUsage).toHaveBeenCalledWith(2001)
    // 单一数据源：始终使用 /usage API 返回值，忽略 codex 快照
    expect(wrapper.text()).toContain('5h|18|900')
    expect(wrapper.text()).toContain('7d|36|900')
  })

  it('仅为 OpenAI OAuth 7d 窗口计算预计总费用', async () => {
    getUsage.mockResolvedValue({
      five_hour: {
        utilization: 25,
        resets_at: null,
        remaining_seconds: 0,
        window_stats: { requests: 1, tokens: 100, cost: 2 }
      },
      seven_day: {
        utilization: 40,
        resets_at: null,
        remaining_seconds: 0,
        window_stats: { requests: 2, tokens: 200, cost: 12 }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 6752, platform: 'openai', type: 'oauth' })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'estimatedTotalCost'],
            template: '<div class="usage-bar">{{ label }}|{{ estimatedTotalCost ?? "none" }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('5h|none')
    expect(wrapper.text()).toContain('7d|30')
  })

  it.each([
    { id: 6801, utilization: 0, cost: 12 },
    { id: 6802, utilization: -1, cost: 12 },
    { id: 6803, utilization: Number.NaN, cost: 12 },
    { id: 6804, utilization: 40, cost: 0 },
    { id: 6805, utilization: 40, cost: Number.POSITIVE_INFINITY }
  ])('OpenAI OAuth 7d 输入无效时不显示预计总费用 (%o)', async ({ id, utilization, cost }) => {
    getUsage.mockResolvedValue({
      seven_day: {
        utilization,
        resets_at: null,
        remaining_seconds: 0,
        window_stats: { requests: 1, tokens: 100, cost }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id,
          platform: 'openai',
          type: 'oauth'
        })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'estimatedTotalCost'],
            template: '<div class="usage-bar">{{ label }}|{{ estimatedTotalCost ?? "none" }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('7d|none')
    expect(wrapper.text()).not.toMatch(/Infinity|NaN/)
  })

  it('OpenAI OAuth 有现成快照时，手动刷新信号会触发 usage 重拉', async () => {
    getUsage.mockResolvedValue({
      five_hour: {
        utilization: 18,
        resets_at: '2099-03-07T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 9,
          tokens: 900,
          cost: 0.09,
          standard_cost: 0.09,
          user_cost: 0.09
        }
      },
      seven_day: {
        utilization: 36,
        resets_at: '2099-03-13T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 9,
          tokens: 900,
          cost: 0.09,
          standard_cost: 0.09,
          user_cost: 0.09
        }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2010,
          platform: 'openai',
          type: 'oauth',
          extra: {
            codex_usage_updated_at: '2099-03-07T10:00:00Z',
            codex_5h_used_percent: 12,
            codex_5h_reset_at: '2099-03-07T12:00:00Z',
            codex_7d_used_percent: 34,
            codex_7d_reset_at: '2099-03-13T12:00:00Z'
          },
          rate_limit_reset_at: null
        }),
        manualRefreshToken: 0
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'windowStats', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()
    // mount 时已经拉取一次
    expect(getUsage).toHaveBeenCalledTimes(1)

    await wrapper.setProps({ manualRefreshToken: 1 })
    await flushPromises()

    // 手动刷新再拉一次
    expect(getUsage).toHaveBeenCalledTimes(2)
    expect(getUsage).toHaveBeenCalledWith(2010)
    // 单一数据源：始终使用 /usage API 值
    expect(wrapper.text()).toContain('5h|18|900')
  })

  it('OpenAI OAuth 在无 codex 快照时会回退显示 usage 接口窗口', async () => {
	getUsage.mockResolvedValue({
	  five_hour: {
	    utilization: 0,
	    resets_at: null,
	    remaining_seconds: 0,
	    window_stats: {
	      requests: 2,
	      tokens: 27700,
	      cost: 0.06,
	      standard_cost: 0.06,
	      user_cost: 0.06
	    }
	  },
	  seven_day: {
	    utilization: 0,
	    resets_at: null,
	    remaining_seconds: 0,
	    window_stats: {
	      requests: 2,
	      tokens: 27700,
	      cost: 0.06,
	      standard_cost: 0.06,
	      user_cost: 0.06
	    }
	  }
	})

		const wrapper = mount(AccountUsageCell, {
		  props: {
		    account: makeAccount({
		      id: 2002,
		      platform: 'openai',
		      type: 'oauth',
		      extra: {}
		    })
		  },
	  global: {
	    stubs: {
	      UsageProgressBar: {
	        props: ['label', 'utilization', 'resetsAt', 'windowStats', 'color'],
	        template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
	      },
	      AccountQuotaInfo: true
	    }
	  }
	})

	await flushPromises()

	expect(getUsage).toHaveBeenCalledWith(2002)
	expect(wrapper.text()).toContain('5h|0|27700')
	expect(wrapper.text()).toContain('7d|0|27700')
  })

  it('OpenAI OAuth 在行数据刷新但仍无 codex 快照时会重新拉取 usage', async () => {
	getUsage
	  .mockResolvedValueOnce({
	    five_hour: {
	      utilization: 0,
	      resets_at: null,
	      remaining_seconds: 0,
	      window_stats: {
	        requests: 1,
	        tokens: 100,
	        cost: 0.01,
	        standard_cost: 0.01,
	        user_cost: 0.01
	      }
	    },
	    seven_day: null
	  })
	  .mockResolvedValueOnce({
	    five_hour: {
	      utilization: 0,
	      resets_at: null,
	      remaining_seconds: 0,
	      window_stats: {
	        requests: 2,
	        tokens: 200,
	        cost: 0.02,
	        standard_cost: 0.02,
	        user_cost: 0.02
	      }
	    },
	    seven_day: null
	  })

		const wrapper = mount(AccountUsageCell, {
		  props: {
		    account: makeAccount({
		      id: 2003,
		      platform: 'openai',
		      type: 'oauth',
		      updated_at: '2026-03-07T10:00:00Z',
		      extra: {}
		    })
		  },
	  global: {
	    stubs: {
	      UsageProgressBar: {
	        props: ['label', 'utilization', 'resetsAt', 'windowStats', 'color'],
	        template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
	      },
	      AccountQuotaInfo: true
	    }
	  }
	})

	await flushPromises()
	expect(wrapper.text()).toContain('5h|0|100')
	expect(getUsage).toHaveBeenCalledTimes(1)

	await wrapper.setProps({
	  account: {
	    id: 2003,
	    platform: 'openai',
	    type: 'oauth',
	    updated_at: '2026-03-07T10:01:00Z',
	    extra: {}
	  }
	})

	await flushPromises()
	expect(getUsage).toHaveBeenCalledTimes(2)
	expect(wrapper.text()).toContain('5h|0|200')
  })

  it('OpenAI 重置响应更新账号行后重新拉取 usage', async () => {
    getUsage.mockResolvedValue({
      five_hour: {
        utilization: 0,
        resets_at: null,
        remaining_seconds: 0
      },
      seven_day: null
    })
    const account = makeAccount({
      id: 2004,
      platform: 'openai',
      type: 'oauth',
      updated_at: '2026-03-07T10:00:00Z',
      extra: {}
    })
    const wrapper = mount(AccountUsageCell, {
      props: { account },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true,
          OpenAIQuotaResetCell: {
            props: ['account'],
            emits: ['account-updated'],
            template: '<button data-test="quota-reset-result" @click="$emit(\'account-updated\', { ...account, updated_at: \'2026-03-07T10:01:00Z\' })" />'
          }
        }
      }
    })

    await flushPromises()
    expect(getUsage).toHaveBeenCalledTimes(1)

    await wrapper.get('[data-test="quota-reset-result"]').trigger('click')
    const updatedAccount = wrapper.emitted<Account[]>('account-updated')?.[0]?.[0]
    expect(updatedAccount?.updated_at).toBe('2026-03-07T10:01:00Z')

    await wrapper.setProps({ account: updatedAccount as Account })
    await flushPromises()

    expect(getUsage).toHaveBeenCalledTimes(2)
  })

  it('OpenAI OAuth 已限额时显示 /usage API 返回的限额数据', async () => {
	getUsage.mockResolvedValue({
	  five_hour: {
	    utilization: 100,
	    resets_at: '2026-03-07T12:00:00Z',
	    remaining_seconds: 3600,
	    window_stats: {
	      requests: 211,
	      tokens: 106540000,
	      cost: 38.13,
	      standard_cost: 38.13,
	      user_cost: 38.13
	    }
	  },
	  seven_day: {
	    utilization: 100,
	    resets_at: '2026-03-13T12:00:00Z',
	    remaining_seconds: 3600,
	    window_stats: {
	      requests: 211,
	      tokens: 106540000,
	      cost: 38.13,
	      standard_cost: 38.13,
	      user_cost: 38.13
	    }
	  }
	})

		const wrapper = mount(AccountUsageCell, {
		  props: {
		    account: makeAccount({
		      id: 2004,
		      platform: 'openai',
		      type: 'oauth',
		      rate_limit_reset_at: '2099-03-07T12:00:00Z',
		      extra: {
		        codex_5h_used_percent: 0,
		        codex_7d_used_percent: 0
		      }
		    })
		  },
	  global: {
	    stubs: {
	      UsageProgressBar: {
	        props: ['label', 'utilization', 'resetsAt', 'windowStats', 'color'],
	        template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
	      },
	      AccountQuotaInfo: true
	    }
	  }
	})

	await flushPromises()

  expect(getUsage).toHaveBeenCalledWith(2004)
  expect(wrapper.text()).toContain('5h|100|106540000')
  expect(wrapper.text()).toContain('7d|100|106540000')
  })

  it('Key 账号会展示 today stats 徽章并带 A/U 提示', async () => {
		const wrapper = mount(AccountUsageCell, {
		  props: {
		    account: makeAccount({
		      id: 3001,
		      platform: 'anthropic',
		      type: 'apikey'
		    }),
		    todayStats: {
		      requests: 1_000_000,
		      tokens: 1_000_000_000,
		      cost: 12.345,
		      standard_cost: 12.345,
		      user_cost: 6.789
		    }
		  },
		  global: {
		    stubs: {
		      UsageProgressBar: true,
		      AccountQuotaInfo: true
		    }
		  }
		})

		await flushPromises()

		expect(wrapper.text()).toContain('1.0M req')
		expect(wrapper.text()).toContain('1.0B')
		expect(wrapper.text()).toContain('A $12.35')
		expect(wrapper.text()).toContain('U $6.79')

		const badges = wrapper.findAll('span[title]')
		expect(badges.some(node => node.attributes('title') === 'usage.accountBilled')).toBe(true)
		expect(badges.some(node => node.attributes('title') === 'usage.userBilled')).toBe(true)
  })

  it('Grok OAuth compact UI drops local chips and header quota bars', async () => {
    getUsage.mockResolvedValue({
      grok_local_usage: {
        requests: 4,
        tokens: 1200,
        cost: 0.12,
        standard_cost: 0.12,
        user_cost: 0.34
      },
      grok_request_quota: {
        limit: 10,
        remaining: -2,
        reset_at: '2026-07-09T16:00:00Z'
      },
      grok_quota_snapshot_state: 'observed'
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 3861, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ resetsAt }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()
    expect(getUsage).toHaveBeenCalledWith(3861)
    expect(wrapper.text()).not.toContain('4 req')
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.grokRequests|')
  })

  it('Grok paid monthly limits show 30d bar without free 24h', async () => {
    getUsage.mockResolvedValue({
      grok_billing: {
        period_type: 'weekly',
        usage_percent: null,
        used_percent: 12,
        monthly_limit_cents: 25_000,
        used_cents: 3_000,
        plan: ''
      },
      grok_entitlement_status: 'free',
      grok_token_quota: { limit: 1_000, remaining: 250 }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4402, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()
    expect(wrapper.text()).toContain('30d|')
    expect(wrapper.text()).not.toContain('24h|')
  })

  it('Grok OAuth uses the official weekly billing percentage when available', async () => {
    getUsage.mockResolvedValue({
      grok_billing: {
        period_type: 'weekly',
        usage_percent: 37,
        period_end: '2026-07-16T03:25:00Z',
        plan: 'SuperGrok'
      },
      grok_local_usage: {
        requests: 5,
        tokens: 2_200_000,
        cost: 4.42,
        standard_cost: 4.42,
        user_cost: 0.44
      },
      grok_request_quota: { limit: 100, remaining: 100 },
      grok_token_quota: { limit: 2_000_000, remaining: 2_000_000 }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4201, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'remainingCapacity'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ resetsAt }}|{{ remainingCapacity }}</div>'
          },
          AccountQuotaInfo: true,
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('7d|37|2026-07-16T03:25:00Z')
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.grokRequests|')
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.grokTokens|')
    expect(wrapper.text()).not.toContain('2M|')
  })

  it.each([
    { tokens: 0, expected: 0 },
    { tokens: 500_000, expected: 50 },
    { tokens: 1_000_000, expected: 100 },
    { tokens: 1_100_000, expected: 100 }
  ])('Grok Free derives its 1M quota from local tokens: $tokens -> $expected%', async ({ tokens, expected }) => {
    getUsage.mockResolvedValue({
      grok_free_token_limit: 1_000_000,
      grok_billing: {
        period_type: 'weekly',
        usage_percent: null,
        plan: ''
      },
      grok_local_usage_24h: {
        requests: 5,
        tokens,
        cost: 0,
        standard_cost: 0,
        user_cost: 0
      },
      grok_request_quota: { limit: 100, remaining: 100 },
      grok_token_quota: { limit: 1_000_000, remaining: 1_000_000 }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4300 + expected, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}</div>'
          },
          AccountQuotaInfo: true,
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain(`24h|${expected}`)
    expect(wrapper.findAll('.usage-bar')).toHaveLength(1)
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.grokRequests|')
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.grokTokens|')
    expect(wrapper.text()).not.toContain('7d|')
  })

  it('Grok Free uses rolling 24h usage instead of today-only usage', async () => {
    getUsage.mockResolvedValue({
      grok_free_token_limit: 1_000_000,
      grok_billing: { period_type: 'weekly', usage_percent: null, plan: '' },
      grok_local_usage: {
        requests: 2,
        tokens: 250_000,
        cost: 0,
        standard_cost: 0
      },
      grok_local_usage_24h: {
        requests: 12,
        tokens: 750_000,
        cost: 0,
        standard_cost: 0
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4398, platform: 'grok', type: 'oauth', extra: {} }),
        todayStats: {
          requests: 2,
          tokens: 200_000,
          cost: 0,
          standard_cost: 0
        }
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'title'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ title }}</div>'
          },
          AccountQuotaInfo: true,
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('24h|75|admin.accounts.usageWindow.grokFreeQuota24hHint')
    expect(wrapper.text()).not.toContain('7d|')
    expect(wrapper.text()).not.toContain('200.0K')
    expect(wrapper.text()).not.toContain('250.0K')
  })

  it('Grok Free does not substitute today stats when rolling 24h usage is unavailable', async () => {
    getUsage.mockResolvedValue({
      grok_free_token_limit: 1_000_000,
      grok_billing: { period_type: 'weekly', usage_percent: null, plan: '' },
      grok_local_usage: {
        requests: 1,
        tokens: 250_000,
        cost: 0,
        standard_cost: 0,
        user_cost: 0
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4399, platform: 'grok', type: 'oauth', extra: {} }),
        todayStats: {
          requests: 4,
          tokens: 1_000_000,
          cost: 0,
          standard_cost: 0,
          user_cost: 0
        }
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}</div>'
          },
          AccountQuotaInfo: true,
        }
      }
    })

    await flushPromises()

    expect(wrapper.findAll('.usage-bar')).toHaveLength(0)
    expect(wrapper.text()).not.toContain('24h|')
    expect(wrapper.text()).not.toContain('1.0M')
    expect(wrapper.text()).not.toContain('250.0K')
  })

  it('Grok JWT free tier shows 24h bar even when leftover Heavy billing metrics remain', async () => {
    getUsage.mockResolvedValue({
      grok_free_token_limit: 500_000,
      subscription_tier: 'free',
      grok_billing: {
        plan: 'SuperGrok Heavy',
        monthly_limit_cents: 150_000,
        usage_percent: 10,
        used_percent: 5
      },
      grok_local_usage_24h: {
        requests: 2,
        tokens: 250_000,
        cost: 0,
        standard_cost: 0
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4404, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()
    expect(wrapper.text()).toContain('24h|')
    expect(wrapper.text()).not.toContain('7d|')
    expect(wrapper.text()).not.toContain('30d|')
  })

  it('Grok SuperGrok Lite stays on paid 7d bar, not free 24h', async () => {
    getUsage.mockResolvedValue({
      subscription_tier: 'supergrok_lite',
      grok_billing: {
        period_type: 'weekly',
        plan: 'SuperGrok',
        usage_percent: 20
      },
      grok_local_usage_24h: {
        requests: 1,
        tokens: 100,
        cost: 0,
        standard_cost: 0
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4405, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()
    expect(wrapper.text()).toContain('7d|')
    expect(wrapper.text()).not.toContain('24h|')
  })

  it('Grok credential Free tier keeps the 1M fallback when billing is unavailable', async () => {
    getUsage.mockResolvedValue({
      grok_free_token_limit: 1_000_000,
      subscription_tier: 'FREE',
      grok_local_usage_24h: {
        requests: 3,
        tokens: 1_000_000,
        cost: 0,
        standard_cost: 0
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4403, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}</div>'
          },
          AccountQuotaInfo: true,
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('24h|100')
  })

  it('Grok Free 24h bar shows rolling local usage chips', async () => {
    getUsage.mockResolvedValue({
      grok_free_token_limit: 1_000_000,
      grok_billing: { period_type: 'weekly', usage_percent: null, plan: '' },
      grok_local_usage: {
        requests: 2,
        tokens: 250_000,
        cost: 0,
        standard_cost: 0
      },
      grok_local_usage_24h: {
        requests: 12,
        tokens: 750_000,
        cost: 0.12,
        standard_cost: 0.12,
        user_cost: 0.04
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4410, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'windowStats'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()
    expect(wrapper.text()).toContain('24h|75|750000')
    expect(wrapper.text()).not.toContain('|250000')
    expect(wrapper.text()).not.toContain('7d|')
  })

  it('Grok SuperGrok and Heavy bars show period-aligned local 7d and 30d usage', async () => {
    getUsage.mockResolvedValue({
      subscription_tier: 'SuperGrok Heavy',
      grok_billing: {
        period_type: 'weekly',
        usage_percent: 37,
        used_percent: 12,
        monthly_limit_cents: 150_000,
        period_end: '2026-07-16T03:25:00Z',
        billing_period_end: '2026-08-01T00:00:00Z',
        plan: 'SuperGrok Heavy'
      },
      grok_local_usage: {
        requests: 1,
        tokens: 99,
        cost: 0,
        standard_cost: 0
      },
      grok_local_usage_24h: {
        requests: 2,
        tokens: 100,
        cost: 0,
        standard_cost: 0
      },
      grok_local_usage_7d: {
        requests: 8,
        tokens: 2_200_000,
        cost: 4.42,
        standard_cost: 4.42
      },
      grok_local_usage_monthly: {
        requests: 20,
        tokens: 8_000_000,
        cost: 18.5,
        standard_cost: 18.5
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4411, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'windowStats'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()
    expect(wrapper.text()).toContain('7d|37|2200000')
    expect(wrapper.text()).toContain('30d|12|8000000')
    expect(wrapper.text()).not.toContain('|99')
    expect(wrapper.text()).not.toContain('|100')
    expect(wrapper.text()).not.toContain('24h|')
  })

  it('Grok paid bars fall back to official seven_day and thirty_day window_stats', async () => {
    getUsage.mockResolvedValue({
      subscription_tier: 'SuperGrok',
      grok_billing: {
        period_type: 'weekly',
        usage_percent: 20,
        used_percent: 8,
        monthly_limit_cents: 25_000,
        plan: 'SuperGrok'
      },
      seven_day: {
        utilization: 20,
        window_stats: {
          requests: 6,
          tokens: 1_500_000,
          cost: 3.1,
          standard_cost: 3.1
        }
      },
      thirty_day: {
        utilization: 8,
        window_stats: {
          requests: 14,
          tokens: 4_400_000,
          cost: 9.2,
          standard_cost: 9.2
        }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4412, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'windowStats'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()
    expect(wrapper.text()).toContain('7d|20|1500000')
    expect(wrapper.text()).toContain('30d|8|4400000')
  })

  it('Grok paid hides zero prepaid and hides used/limit when monthly limit is 0', async () => {
    getUsage.mockResolvedValue({
      subscription_tier: 'SuperGrok',
      grok_billing: {
        period_type: 'weekly',
        usage_percent: 20,
        prepaid_balance: 0,
        monthly_limit: 0,
        monthly_used: 3.5,
        plan: 'SuperGrok'
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4413, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.grokPrepaid')
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.grokUsed')
    expect(wrapper.text()).not.toContain('3.5/0')
  })

  it('Grok paid shows used/limit without prepaid, and prepaid without a zero monthly limit', async () => {
    getUsage.mockResolvedValueOnce({
      subscription_tier: 'SuperGrok',
      grok_billing: {
        period_type: 'weekly',
        usage_percent: 20,
        monthly_limit: 25,
        monthly_used: 3.5,
        plan: 'SuperGrok'
      }
    })

    const usedOnly = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4414, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true
        }
      }
    })
    await flushPromises()
    expect(usedOnly.text()).not.toContain('admin.accounts.usageWindow.grokPrepaid')
    expect(usedOnly.text()).toContain('admin.accounts.usageWindow.grokUsed')
    expect(usedOnly.text()).toContain('3.50/25.0')

    getUsage.mockResolvedValueOnce({
      subscription_tier: 'SuperGrok Heavy',
      grok_billing: {
        period_type: 'weekly',
        usage_percent: 20,
        prepaid_balance: 12.5,
        monthly_limit: 0,
        monthly_used: 8,
        plan: 'SuperGrok Heavy'
      }
    })
    const prepaidOnly = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 4415, platform: 'grok', type: 'oauth', extra: {} })
      },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true
        }
      }
    })
    await flushPromises()
    expect(prepaidOnly.text()).toContain('admin.accounts.usageWindow.grokPrepaid')
    expect(prepaidOnly.text()).toContain('$12.5')
    expect(prepaidOnly.text()).not.toContain('admin.accounts.usageWindow.grokUsed')
    expect(prepaidOnly.text()).not.toContain('8.00/0')
  })

  it('Key 账号在 today stats loading 时显示骨架屏', async () => {
		const wrapper = mount(AccountUsageCell, {
		  props: {
		    account: makeAccount({
		      id: 3002,
		      platform: 'anthropic',
		      type: 'apikey'
		    }),
		    todayStats: null,
		    todayStatsLoading: true
		  },
		  global: {
		    stubs: {
		      UsageProgressBar: true,
		      AccountQuotaInfo: true
		    }
		  }
		})

		await flushPromises()

		expect(wrapper.findAll('.animate-pulse').length).toBeGreaterThan(0)
  })

  it('Key 账号在无 today stats 且无配额时显示兜底短横线', async () => {
		const wrapper = mount(AccountUsageCell, {
		  props: {
		    account: makeAccount({
		      id: 3003,
		      platform: 'anthropic',
		      type: 'apikey',
		      quota_limit: 0,
		      quota_daily_limit: 0,
		      quota_weekly_limit: 0
		    }),
		    todayStats: null,
		    todayStatsLoading: false
		  },
		  global: {
		    stubs: {
		      UsageProgressBar: true,
		      AccountQuotaInfo: true
		    }
		  }
		})

		await flushPromises()

		expect(wrapper.text().trim()).toBe('-')
  })

  it('Vertex 账号会在 Gemini 用量窗口里展示 today stats 徽章', async () => {
		const wrapper = mount(AccountUsageCell, {
		  props: {
		    account: makeAccount({
		      id: 4001,
		      platform: 'gemini',
		      type: 'service_account',
          credentials: {
            tier_id: 'vertex',
            project_id: 'vertex-proj',
            client_email: 'svc@vertex-proj.iam.gserviceaccount.com',
            location: 'global'
          },
		      extra: {}
		    }),
		    todayStats: {
		      requests: 0,
		      tokens: 0,
		      cost: 0,
		      standard_cost: 0,
		      user_cost: 0
		    }
		  },
		  global: {
		    stubs: {
		      UsageProgressBar: true,
		      AccountQuotaInfo: true
		    }
		  }
		})

		await flushPromises()

		expect(wrapper.text()).toContain('0 req')
		expect(wrapper.text()).toContain('0')
		expect(wrapper.text()).toContain('A $0.00')
		expect(wrapper.text()).toContain('U $0.00')
  })

  it('Anthropic OAuth 会渲染 7d F (Fable) 进度条，且 7d S 逻辑保留', async () => {
    getUsage.mockResolvedValue({
      source: 'passive',
      five_hour: {
        utilization: 41,
        resets_at: '2026-07-03T10:00:00Z',
        remaining_seconds: 3600
      },
      seven_day: {
        utilization: 56,
        resets_at: '2026-07-06T22:00:00Z',
        remaining_seconds: 300000
      },
      seven_day_sonnet: {
        utilization: 30,
        resets_at: '2026-07-06T22:00:00Z',
        remaining_seconds: 300000
      },
      seven_day_fable: {
        utilization: 100,
        resets_at: '2026-07-06T22:00:00Z',
        remaining_seconds: 300000
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 3001,
          platform: 'anthropic',
          type: 'oauth',
          extra: {}
        })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}</div>'
          },
          AccountQuotaInfo: true,
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('5h|41')
    expect(wrapper.text()).toContain('7d|56')
    expect(wrapper.text()).toContain('7d S|30')
    expect(wrapper.text()).toContain('7d F|100')
  })

  it('Anthropic OAuth 无 Fable 数据时不渲染 7d F 进度条', async () => {
    getUsage.mockResolvedValue({
      source: 'passive',
      five_hour: {
        utilization: 41,
        resets_at: '2026-07-03T10:00:00Z',
        remaining_seconds: 3600
      },
      seven_day: {
        utilization: 56,
        resets_at: '2026-07-06T22:00:00Z',
        remaining_seconds: 300000
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 3002,
          platform: 'anthropic',
          type: 'oauth',
          extra: {}
        })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}</div>'
          },
          AccountQuotaInfo: true,
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('5h|41')
    expect(wrapper.text()).toContain('7d|56')
    expect(wrapper.text()).not.toContain('7d S')
    expect(wrapper.text()).not.toContain('7d F')
  })

  // D-QL-005 L5 互斥路由：TH 账号（base_url 含 tokenharbor.ai 是唯一事实源）在
  // AccountUsageCell 层走 CNQuotaUsageCell，不得再渲染 CN 配额/余额子单元格。
  it('TokenHarbor 账号渲染 CNQuotaUsageCell，且不渲染 CN 配额/余额子单元格', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9100,
          platform: 'kimi',
          type: 'apikey',
          credentials: { account_mode: 'coding', base_url: 'https://api.tokenharbor.ai/v1' },
          extra: {}
        })
      },
      global: {
        stubs: {
          ...cnUsageCellStubs,
          CNQuotaUsageCell: {
            props: ['account'],
            template: '<div data-test="cn-quota-usage-cell" />'
          },
          UsageProgressBar: true,
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(wrapper.find('[data-test="cn-quota-usage-cell"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="cn-quota-cell"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-balance-cell"]').exists()).toBe(false)
    expect(getUsage).not.toHaveBeenCalled()
  })

  // ===== 派发单 BAL-GATE-D3 C1/C2：余额单元格可见性能力门控（DOM 契约）=====

  // Done when #2：zhipu + quota(coding) + probe 组合下 quota 单元格与余额单元格
  // 各渲染恰好一次（free/paid 双维并存，不是重复渲染）；因两者之一可见，:520 兜底占位符不渲染。
  it('zhipu coding + balance_probe_capable → quota & balance cells render once, no fallback placeholder', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9101,
          platform: 'zhipu',
          type: 'apikey',
          balance_probe_capable: true,
          credentials: {
            account_mode: 'coding',
            base_url: 'https://zhipu-probe.example.com/v1/usage',
            balance_probe: { enabled: true, url: 'https://zhipu-probe.example.com/v1/usage' }
          }
        })
      },
      global: {
        stubs: { ...cnUsageCellStubs, UsageProgressBar: true, AccountQuotaInfo: true }
      }
    })

    await flushPromises()

    // quota 单元格（zhipu coding 恒可见）+ 余额单元格（probe 启用）各恰好一次
    expect(wrapper.findAll('[data-test="cn-quota-cell"]')).toHaveLength(1)
    expect(wrapper.findAll('[data-test="cn-balance-cell"]')).toHaveLength(1)
    // 两者之一可见 → 兜底占位符不渲染
    expect(wrapper.find('div[title="admin.accounts.cnProviders.noBalanceEndpoint"]').exists()).toBe(false)
  })

  // Done when #2：zhipu payg 无探测 → quota 与余额均不可见 → :520 兜底占位符渲染（旧行为不变）。
  it('zhipu payg + no balance_probe → fallback placeholder renders (both quota & balance hidden)', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9102,
          platform: 'zhipu',
          type: 'apikey',
          credentials: { account_mode: 'payg' }
        })
      },
      global: {
        stubs: { ...cnUsageCellStubs, UsageProgressBar: true, AccountQuotaInfo: true }
      }
    })

    await flushPromises()

    // 智谱 payg 无公开余额端点、无探测 → quota 与余额均不可见
    expect(wrapper.find('[data-test="cn-quota-cell"]').exists()).toBe(true) // stub 始终挂载，仅作占位
    expect(wrapper.find('[data-test="cn-balance-cell"]').exists()).toBe(true) // stub 始终挂载
    // 因两者均不可见 → 兜底占位符渲染（:520 逻辑不变）
    expect(wrapper.find('div[title="admin.accounts.cnProviders.noBalanceEndpoint"]').exists()).toBe(true)
  })

  // Done when #3（经由 CN 块内嵌）：openai 不在 CN 平台集合，CN 块整体不渲染——
  // 此处锁死火山订阅号 + probe 时余额单元格在 CN 块内可见，证明 probe 优先于火山排除。
  it('volcano deepseek + balance_probe_capable → balance cell visible inside CN block (capable overrides volcano)', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9103,
          platform: 'deepseek',
          type: 'apikey',
          balance_probe_capable: true,
          credentials: {
            account_mode: 'payg',
            base_url: 'https://ark.cn-beijing.volces.com/api/plan',
            balance_probe: { enabled: true, url: 'https://ark.cn-beijing.volces.com/api/plan/v1/usage' }
          }
        })
      },
      global: {
        stubs: { ...cnUsageCellStubs, UsageProgressBar: true, AccountQuotaInfo: true }
      }
    })

    await flushPromises()

    expect(wrapper.findAll('[data-test="cn-balance-cell"]')).toHaveLength(1)
    // 火山订阅号默认隐藏余额单元格，但 probe 启用后可见 → 兜底占位符不渲染
    expect(wrapper.find('div[title="admin.accounts.cnProviders.noBalanceEndpoint"]').exists()).toBe(false)
  })

  // ===== 派发单 BAL-GATE-D4 F03：双维 DOM 契约测试（真实证据，非 stub）=====

  // Done when #1：openai apikey + balance_probe.enabled → 真实（非 stub）渲染
  // CNProviderBalanceCell 的 cn-provider-balance-value；非 CN 平台 → 配额单元格自门控不渲染。
  it('F03/D4 #1: openai apikey + balance_probe_capable renders REAL cn-provider-balance-value (not stubbed)', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9501,
          platform: 'openai',
          type: 'apikey',
          balance_probe_capable: true,
          credentials: {
            base_url: 'https://openai-probe.example.com/v1/usage',
            balance_probe: { enabled: true, url: 'https://openai-probe.example.com/v1/usage' }
          }
        })
      },
      global: {
        // 故意不 stub CNProviderBalanceCell / CNProviderQuotaCell：断言真实 DOM
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true,
          OpenAIQuotaResetCell: true,
          OllamaCloudUsageCell: true,
          CNQuotaUsageCell: true
        }
      }
    })

    await flushPromises()

    // 真实余额单元格渲染出余额值标记（证明单元格已"显示"而非仅"挂载"）
    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(true)
    // 非 CN 平台 → 配额单元格 visible=false，真实 cn-provider-quota 根节点不渲染
    expect(wrapper.find('[data-test="cn-provider-quota"]').exists()).toBe(false)
  })

  // Done when #1（逆向）：openai apikey 无 balance_probe → CN 余额单元格绝不渲染（父门排除路径）。
  it('F03/D4 #1b: openai apikey without balance_probe does NOT render cn-provider-balance-value', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9502,
          platform: 'openai',
          type: 'apikey'
        })
      },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true,
          OpenAIQuotaResetCell: true,
          OllamaCloudUsageCell: true,
          CNQuotaUsageCell: true
        }
      }
    })

    await flushPromises()

    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(false)
    // 也无 legacy 余额探测入口（无 probe → 非 CN 平台不进入 CN 块，旧入口条件不满足）
    expect(wrapper.text()).not.toContain('admin.accounts.cnProviders.probe')
  })

  // 方案乙核心语义（BAL-GATE-D5）：openai + balance_probe.enabled=true，但后端投影
  // balance_probe_capable 缺省/false（配置开关打开却无能解析 URL 且非官方端点）→
  // CN 余额单元格绝不渲染（display 条件已升级为后端投影的有效探测能力，非配置开关）。
  it('F03/D5: openai apikey + balance_probe.enabled=true but capable=false does NOT render cn-provider-balance-value', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9502,
          platform: 'openai',
          type: 'apikey',
          // 注意：故意不设置 balance_probe_capable（或设为 false）→ 开关 alone 不驱动显示
          credentials: { balance_probe: { enabled: true } }
        })
      },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true,
          OpenAIQuotaResetCell: true,
          OllamaCloudUsageCell: true,
          CNQuotaUsageCell: true
        }
      }
    })

    await flushPromises()

    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(false)
  })

  // Done when #2：zhipu coding + quota + probe 双维组合 → 真实 quota 标记与真实余额标记
  // 同时各恰好出现一次（不 stub 叶子组件）。
  it('F03/D4 #2: zhipu coding + balance_probe_capable → REAL cn-provider-quota & cn-provider-balance-value each exactly once', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9503,
          platform: 'zhipu',
          type: 'apikey',
          balance_probe_capable: true,
          credentials: {
            account_mode: 'coding',
            base_url: 'https://zhipu-probe.example.com/v1/usage',
            balance_probe: { enabled: true, url: 'https://zhipu-probe.example.com/v1/usage' }
          },
          // 给近未来快照时间，避免挂载时自动探测触发真实 API
          extra: { zhipu_usage_updated_at: '2099-01-01T00:00:00Z' }
        })
      },
      global: {
        // 不 stub CNProviderQuotaCell / CNProviderBalanceCell：断言真实渲染标记
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true,
          OllamaCloudUsageCell: true,
          CNQuotaUsageCell: true
        }
      }
    })

    await flushPromises()

    // 真实 quota 根节点（CNProviderQuotaCell visible=true）恰好一次
    expect(wrapper.findAll('[data-test="cn-provider-quota"]')).toHaveLength(1)
    // 真实余额值标记（CNProviderBalanceCell visible=true）恰好一次
    expect(wrapper.findAll('[data-test="cn-provider-balance-value"]')).toHaveLength(1)
    // 两者之一可见 → 兜底占位符不渲染
    expect(wrapper.find('div[title="admin.accounts.cnProviders.noBalanceEndpoint"]').exists()).toBe(false)
  })

  // Done when #3：gemini + balance_probe → gemini 用量展示不变，CN 余额单元格绝不出现
  // （gemini 被父门能力项排除，继续走专有 gemini 分支）。
  it('F03/D4 #3: gemini + balance_probe keeps gemini display, CN balance cell absent', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9504,
          platform: 'gemini',
          type: 'service_account',
          credentials: {
            tier_id: 'vertex',
            project_id: 'vertex-proj',
            client_email: 'svc@vertex-proj.iam.gserviceaccount.com',
            location: 'global',
            balance_probe: { enabled: true }
          },
          extra: {}
        }),
        todayStats: {
          requests: 5,
          tokens: 100,
          cost: 0.01,
          standard_cost: 0.01,
          user_cost: 0
        }
      },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true,
          OpenAIQuotaResetCell: true,
          OllamaCloudUsageCell: true,
          CNQuotaUsageCell: true
        }
      }
    })

    await flushPromises()

    // gemini 用量窗口仍展示 today stats 徽章（原 gemini 展示路径未被 CN 分支劫持）
    expect(wrapper.text()).toContain('5 req')
    // CN 余额单元格绝不出现（gemini 被父门能力项排除，不进入 CN 块）
    expect(wrapper.find('[data-test="cn-provider-balance-value"]').exists()).toBe(false)
  })
})
