import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import QuotaKeyUsageCell from '../QuotaKeyUsageCell.vue'
import type { QuotaKeyUsageProjection } from '@/types'

// 方案 §25.6 消费闭包⑤⑥⑦ / §25.7-B 展示口径的组件级锁：
//   ① 无已提交快照 → 不渲染任何数值（缺失不是 0，绝不补零/补百分比）；
//   ② 最近一次观测失败 → 保留「上次成功值」+ 观测时间，另标「查询失败」，不写 0、不抹旧值；
//   ③ vendor_account_quota provided=false → 只渲染 note，结构上没有余额数字/百分比渲染位。
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

const successUsage: QuotaKeyUsageProjection = {
  scope: 'quota_key',
  label: '所属激活码额度',
  total: 400,
  used: 86,
  remaining: 314,
  status_code: 200,
  checked_at: '2026-10-22T02:00:00Z',
  attempted_at: '2026-10-22T02:00:00Z',
  has_snapshot: true,
  query_failed: false
}

/** 失败观测：total/used 是**上次成功值**，attempted_at/error_code 描述最近一次失败。 */
const failedObservationUsage: QuotaKeyUsageProjection = {
  ...successUsage,
  checked_at: '2026-10-22T02:00:00Z',
  attempted_at: '2026-10-22T03:30:00Z',
  error_code: 'quota_lookup_timeout',
  query_failed: true
}

describe('QuotaKeyUsageCell 来源额度快照展示（§25.6⑦）', () => {
  it('无快照时整个单元格不渲染，且不出现任何数值（缺失不是 0）', () => {
    const wrapper = mount(QuotaKeyUsageCell, { props: { usage: null } })

    expect(wrapper.find('[data-test="quota-key-usage-cell"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="quota-key-usage"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="quota-key-usage-total"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="quota-key-usage-used"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="quota-key-usage-remaining"]').exists()).toBe(false)
    // 空渲染：不含任何数字字符（杜绝 0 / 0% 之类的伪造）。
    expect(wrapper.text().trim()).toBe('')
    expect(wrapper.text()).not.toMatch(/\d/)
  })

  it('成功快照渲染供应商总额/已用/算术剩余 + 观测时间，并带「所属激活码额度」标签', () => {
    const wrapper = mount(QuotaKeyUsageCell, { props: { usage: successUsage } })

    expect(wrapper.get('[data-test="quota-key-usage-label"]').text()).toBe('所属激活码额度')
    expect(wrapper.get('[data-test="quota-key-usage-total"]').text()).toBe('400')
    expect(wrapper.get('[data-test="quota-key-usage-used"]').text()).toBe('86')
    expect(wrapper.get('[data-test="quota-key-usage-remaining"]').text()).toBe('314')
    expect(wrapper.get('[data-test="quota-key-usage-status"]').text()).toBe('HTTP 200')
    // 观测时间必须一起展示：数字离开时间就成了无法核验的断言。
    expect(wrapper.get('[data-test="quota-key-usage-checked-at"]').text().length).toBeGreaterThan(0)
    // 成功观测不出现失败标注。
    expect(wrapper.find('[data-test="quota-key-usage-query-failed"]').exists()).toBe(false)
    // 算术剩余 = total − used，只做减法，不宣称赠额消耗顺序。
    expect(400 - 86).toBe(314)
  })

  it('最近一次观测失败：保留上次成功值 + 时间，并标注查询失败与错误码', () => {
    const wrapper = mount(QuotaKeyUsageCell, { props: { usage: failedObservationUsage } })

    // 上次成功值不被抹掉、也不被写成 0。
    expect(wrapper.get('[data-test="quota-key-usage-total"]').text()).toBe('400')
    expect(wrapper.get('[data-test="quota-key-usage-used"]').text()).toBe('86')
    expect(wrapper.get('[data-test="quota-key-usage-remaining"]').text()).toBe('314')
    expect(wrapper.get('[data-test="quota-key-usage-checked-at"]').text().length).toBeGreaterThan(0)

    // 失败标注：文案 + 脱敏错误码 + 失败尝试时间。
    const failed = wrapper.get('[data-test="quota-key-usage-query-failed"]')
    expect(failed.text()).toContain('admin.accounts.opencodeGo.failed')
    expect(wrapper.get('[data-test="quota-key-usage-error-code"]').text()).toContain('quota_lookup_timeout')
    expect(wrapper.get('[data-test="quota-key-usage-attempted-at"]').text().length).toBeGreaterThan(0)
  })

  it('只有失败观测、没有任何成功值时：不渲染数值，只渲染失败标注', () => {
    const failureOnly: QuotaKeyUsageProjection = {
      scope: 'quota_key',
      label: '所属激活码额度',
      attempted_at: '2026-10-22T03:30:00Z',
      error_code: 'quota_lookup_timeout',
      has_snapshot: true,
      query_failed: true
    }
    const wrapper = mount(QuotaKeyUsageCell, { props: { usage: failureOnly } })

    expect(wrapper.find('[data-test="quota-key-usage-total"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="quota-key-usage-used"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="quota-key-usage-remaining"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="quota-key-usage-query-failed"]').exists()).toBe(true)
  })
})

describe('QuotaKeyUsageCell 账号页供应商额度口径（§25.7-B）', () => {
  it('vendor_account_quota provided=false 只渲染 note：无数字、无百分比', () => {
    const wrapper = mount(QuotaKeyUsageCell, {
      props: {
        usage: null,
        vendorAccountQuota: { provided: false, note: '供应商账号额度未提供' }
      }
    })

    expect(wrapper.get('[data-test="vendor-account-quota-note"]').text()).toBe('供应商账号额度未提供')
    // 只渲染 note：没有来源快照数值块，也没有任何数字/百分比。
    expect(wrapper.find('[data-test="quota-key-usage"]').exists()).toBe(false)
    expect(wrapper.text()).not.toMatch(/\d/)
    expect(wrapper.text()).not.toContain('%')
    expect(wrapper.html()).not.toContain('progress')
  })

  it('两维同时存在时分开表达：所属激活码额度 + 供应商额度未提供注明', () => {
    const wrapper = mount(QuotaKeyUsageCell, {
      props: {
        usage: successUsage,
        vendorAccountQuota: { provided: false, note: '供应商账号额度未提供' }
      }
    })

    // 第一层：来源（激活码）快照，带明确标签。
    expect(wrapper.get('[data-test="quota-key-usage-label"]').text()).toBe('所属激活码额度')
    expect(wrapper.get('[data-test="quota-key-usage-total"]').text()).toBe('400')
    // 第二层：供应商账号额度未提供——只有注明。
    expect(wrapper.get('[data-test="vendor-account-quota-note"]').text()).toBe('供应商账号额度未提供')
    // 账号维度不得出现任何伪造百分比。
    expect(wrapper.text()).not.toContain('%')
  })

  it('provided 非 false 时不渲染任何余额注明（不缺省编造）', () => {
    const wrapper = mount(QuotaKeyUsageCell, {
      props: {
        usage: null,
        vendorAccountQuota: { provided: true, note: '' }
      }
    })

    expect(wrapper.find('[data-test="quota-key-usage-cell"]').exists()).toBe(false)
    expect(wrapper.text().trim()).toBe('')
  })
})
