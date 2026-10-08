import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountStatusIndicator from '../AccountStatusIndicator.vue'
import type { Account } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

vi.mock('@/utils/format', async () => {
  const actual = await vi.importActual<typeof import('@/utils/format')>('@/utils/format')
  return {
    ...actual,
    formatCountdown: () => '1h'
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

describe('AccountStatusIndicator', () => {
  it('Claude 5 模型限流时显示 Opus 和 Sonnet 的短别名', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          extra: {
            model_rate_limits: {
              'claude-opus-5': {
                rate_limited_at: '2026-07-28T00:00:00Z',
                rate_limit_reset_at: '2099-07-28T00:00:00Z'
              },
              'claude-sonnet-5': {
                rate_limited_at: '2026-07-28T00:00:00Z',
                rate_limit_reset_at: '2099-07-28T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('COpus5')
    expect(wrapper.text()).toContain('CSon5')
    expect(wrapper.text()).not.toContain('claude-sonnet-5')
  })

  it('Grok 账号额度限流时显示自动恢复时间而非临时不可调度', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 5,
          name: 'grok-free-1',
          platform: 'grok',
          rate_limited_at: '2026-07-11T12:00:00Z',
          rate_limit_reset_at: '2099-07-11T13:00:00Z',
          temp_unschedulable_until: '2099-07-11T12:30:00Z',
          temp_unschedulable_reason: 'legacy grok rate limited'
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.find('.badge-warning').text()).toBe('admin.accounts.status.rateLimited')
    expect(wrapper.text()).toContain('admin.accounts.status.rateLimitedAutoResume')
    expect(wrapper.text()).not.toContain('admin.accounts.status.tempUnschedulable')
  })

  it('模型限流 + overages 启用 + 无 AICredits key → 显示 ⚡ (credits_active)', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 1,
          name: 'ag-1',
          extra: {
            allow_overages: true,
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('⚡')
    expect(wrapper.text()).toContain('CSon45')
  })

  it('模型限流 + overages 未启用 → 普通限流样式（无 ⚡）', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 2,
          name: 'ag-2',
          extra: {
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('CSon45')
    expect(wrapper.text()).not.toContain('⚡')
  })

  it('AICredits key 生效 → 显示积分已用尽 (credits_exhausted)', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 3,
          name: 'ag-3',
          extra: {
            allow_overages: true,
            model_rate_limits: {
              'AICredits': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('admin.accounts.status.creditsExhausted')
  })

  it('模型限流 + overages 启用 + AICredits key 生效 → 普通限流样式（积分耗尽，无 ⚡）', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 4,
          name: 'ag-4',
          extra: {
            allow_overages: true,
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              },
              'AICredits': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    // 模型限流 + 积分耗尽 → 不应显示 ⚡
    expect(wrapper.text()).toContain('CSon45')
    expect(wrapper.text()).not.toContain('⚡')
    // AICredits 积分耗尽状态应显示
    expect(wrapper.text()).toContain('admin.accounts.status.creditsExhausted')
  })

  it('TokenHarbor 账号的模型限流徽标整体不渲染（限流状态由用量窗口列承载）', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 6,
          name: 'th-1',
          platform: 'kimi',
          type: 'apikey',
          credentials: { base_url: 'https://api.tokenharbor.ai/v1' },
          extra: {
            model_rate_limits: {
              'deepseek-v4.1-flash:free': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    // 状态列不再出现模型名徽章（用户裁定：显式模型名冗余，要删除）
    expect(wrapper.text()).not.toContain('deepseek-v4.1-flash:free')
    // 徽标内倒计时（mock 返回 '1h'）也不应出现
    expect(wrapper.text()).not.toContain('1h')
  })

  it('非 TokenHarbor 账号的模型限流徽标仍展示内联倒计时', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 7,
          name: 'non-th-1',
          platform: 'kimi',
          type: 'apikey',
          credentials: { base_url: 'https://api.moonshot.cn/v1' },
          extra: {
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    // 非 TokenHarbor：徽标内倒计时（mock 返回 '1h'）仍存在
    expect(wrapper.text()).toContain('CSon45')
    expect(wrapper.text()).toContain('1h')
  })

  // E44：tokenharbor 免费档条目（reason 以 tokenharbor_free_tier_exhausted 开头）的
  // 无信号哨兵（precise_reset === false）即使已过期仍保留在 activeModelStatuses（与后端
  // ActiveTokenHarborFreeTierScopes 权威语义逐位对齐，D5 锁定）。用非 tokenharbor 账号挂载
  // 以便徽标不被 visibleModelStatuses 过滤，从而断言该条目确实未到期剔除。
  it('tokenharbor 哨兵（precise_reset=false）过期 → 仍在 activeModelStatuses', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 8,
          name: 'th-sentinel-1',
          platform: 'antigravity',
          type: 'oauth',
          credentials: { base_url: 'https://api.moonshot.cn/v1' },
          extra: {
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                // 已过期，但 tokenharbor 无信号哨兵不得被到期剔除
                rate_limit_reset_at: '2020-03-15T00:00:00Z',
                reason: 'tokenharbor_free_tier_exhausted:x',
                precise_reset: false
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    // 条目未被剔除：模型短别名徽标仍存在
    expect(wrapper.text()).toContain('CSon45')
  })

  // E44 平行：非 tokenharbor 条目（reason 无该前缀）过期 → 仍按标准行为剔除（不回归）。
  it('非 tokenharbor 条目过期 → 不在 activeModelStatuses（标准到期剔除不变）', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 9,
          name: 'non-th-expired-1',
          platform: 'antigravity',
          type: 'oauth',
          credentials: { base_url: 'https://api.moonshot.cn/v1' },
          extra: {
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2020-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    // 条目被到期剔除：模型短别名徽标不应出现
    expect(wrapper.text()).not.toContain('CSon45')
  })

  // D-FE-001 用例 1：账号级健康探测占位条目（仅 attempted_at，无 rate_limited_at /
  // rate_limit_reset_at / reason，后端 display_state=waiting_probe 语义）不是限流，
  // 不得渲染成"限流至"徽章（此前 new Date(undefined) 恒 false 导致永久常驻）。
  it('探测占位条目（仅 attempted_at，无 reset_at/reason）→ 不渲染模型限流徽章', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 10,
          name: 'probe-placeholder-1',
          platform: 'antigravity',
          type: 'oauth',
          extra: {
            model_rate_limits: {
              tokenharbor_account_level_probe: {
                rate_limited_at: undefined as unknown as string,
                rate_limit_reset_at: undefined as unknown as string,
                attempted_at: '2099-03-15T00:00:00Z'
              } as never
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    // 不渲染任何模型限流徽章：占位条目名与"限流至" tooltip 文案都不出现
    expect(wrapper.text()).not.toContain('tokenharbor_account_level_probe')
    expect(wrapper.text()).not.toContain('admin.accounts.status.modelRateLimitedUntil')
  })

  // D-FE-001 用例 2：真实限流条目（有 rate_limited_at + 未来 rate_limit_reset_at、
  // 无 reason）→ 徽章照常渲染（回归保护）。
  it('真实限流条目（无 reason，reset 在未来）→ 徽章照常渲染', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 11,
          name: 'real-rate-limit-1',
          platform: 'antigravity',
          type: 'oauth',
          extra: {
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('CSon45')
    // tooltip 文案在 hover 才可见，但 wrapper.text() 包含模板静态渲染内容；
    // 徽章本体（模型名）出现即证明条目未被剔除。
  })
})
