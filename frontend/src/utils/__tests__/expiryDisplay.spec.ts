import { describe, expect, it } from 'vitest'
import { resolveExpiryDisplay } from '@/utils/format'

// CUR-F2 裁定 4：激活码到期时间为 NULL 时不得显示"永不过期"，应显示"到期时间未知"类文案。
// 空串同样归一到 unknown——激活码不存在永不过期形态，见 2026-10-24 裁定。
describe('resolveExpiryDisplay', () => {
  it('null → unknown（不再断言为永不过期）', () => {
    expect(resolveExpiryDisplay(null)).toEqual({ status: 'unknown' })
  })

  it('undefined → unknown', () => {
    expect(resolveExpiryDisplay(undefined)).toEqual({ status: 'unknown' })
  })

  it('空串（及纯空白）→ unknown（激活码不存在永不过期形态）', () => {
    expect(resolveExpiryDisplay('')).toEqual({ status: 'unknown' })
    expect(resolveExpiryDisplay('   ')).toEqual({ status: 'unknown' })
  })

  it('真实日期 → date 分支且返回非空日期串', () => {
    const r = resolveExpiryDisplay('2026-01-02T03:04:05Z')
    expect(r.status).toBe('date')
    if (r.status === 'date') {
      expect(r.text).not.toBe('')
      expect(r.text).not.toBe('到期时间未知')
      expect(r.text).not.toBe('永不过期')
    }
  })
})
