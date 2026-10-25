import { describe, expect, it } from 'vitest'
import { formatQuotaKeyError } from '../quotaKeyError'

type TranslateFn = (key: string, params?: Record<string, unknown>) => string

// 注入与现有 i18n 约定一致的文案，便于在单测中断言最终展示字符串。
function makeT(locale: 'zh' | 'en' = 'zh'): TranslateFn {
  const zh = {
    'admin.quotaKeys.errorText.refreshLimitedHours':
      '刷新次数达上限（{count}次），约 {hours} 小时后重置。',
    'admin.quotaKeys.errorText.refreshLimitedMinutes':
      '刷新次数达上限（{count}次），约 {minutes} 分钟后重置。'
  }
  const en = {
    'admin.quotaKeys.errorText.refreshLimitedHours':
      'Refresh limit reached ({count} times); resets in about {hours} hours.',
    'admin.quotaKeys.errorText.refreshLimitedMinutes':
      'Refresh limit reached ({count} times); resets in about {minutes} minutes.'
  }
  const map = locale === 'zh' ? zh : en
  return (key: string, params?: Record<string, unknown>) => {
    let out = map[key as keyof typeof map] ?? key
    if (params) {
      for (const [k, v] of Object.entries(params)) {
        out = out.replace(new RegExp(`\\{${k}\\}`, 'g'), String(v))
      }
    }
    return out
  }
}

describe('formatQuotaKeyError', () => {
  describe('空值 / 空白', () => {
    it('null 返回空串', () => {
      expect(formatQuotaKeyError(null, makeT())).toBe('')
    })
    it('undefined 返回空串', () => {
      expect(formatQuotaKeyError(undefined, makeT())).toBe('')
    })
    it('纯空白返回空串', () => {
      expect(formatQuotaKeyError('   \n\t ', makeT())).toBe('')
    })
    it('空串返回空串', () => {
      expect(formatQuotaKeyError('', makeT())).toBe('')
    })
  })

  describe('剥离 shareai 内部前缀', () => {
    it('单层前缀被剥离', () => {
      const raw = 'shareai: upstream: 当日刷新次数达到上限(5次)，将在每日8:00重置，还需7小时0分钟'
      const result = formatQuotaKeyError(raw, makeT())
      expect(result).toBe('刷新次数达上限（5次），约 7 小时后重置。')
    })

    it('嵌套前缀被逐层剥离', () => {
      const raw =
        'shareai: proxy: shareai: upstream: 当日刷新次数达到上限(3次)，将在每日8:00重置，还需2小时30分钟'
      const result = formatQuotaKeyError(raw, makeT())
      expect(result).toBe('刷新次数达上限（3次），约 2 小时后重置。')
    })

    it('未知结果前缀（shareai 包裹非匹配文案）返回剥离后的原文', () => {
      const raw = 'shareai: upstream: 上游令牌已失效，请重新授权'
      const result = formatQuotaKeyError(raw, makeT())
      expect(result).toBe('上游令牌已失效，请重新授权')
    })

    it('不含 shareai 前缀的普通文案保持原样', () => {
      const raw = '网络超时，请稍后重试'
      expect(formatQuotaKeyError(raw, makeT())).toBe('网络超时，请稍后重试')
    })
  })

  describe('刷新上限固定格式', () => {
    it('小时分支（小时 > 0）输出“约 N 小时后重置”', () => {
      const raw = '当日刷新次数达到上限(5次)，将在每日8:00重置，还需7小时0分钟'
      expect(formatQuotaKeyError(raw, makeT())).toBe('刷新次数达上限（5次），约 7 小时后重置。')
    })

    it('分钟分支（小时为 0）输出“约 N 分钟后重置”', () => {
      const raw = '当日刷新次数达到上限(5次)，将在每日8:00重置，还需0小时45分钟'
      expect(formatQuotaKeyError(raw, makeT())).toBe('刷新次数达上限（5次），约 45 分钟后重置。')
    })

    it('半角括号也能识别', () => {
      const raw = '当日刷新次数达到上限(5次)，将在每日8:00重置，还需1小时20分钟'
      expect(formatQuotaKeyError(raw, makeT())).toBe('刷新次数达上限（5次），约 1 小时后重置。')
    })

    it('全角括号也能识别', () => {
      const raw = '当日刷新次数达到上限（5次），将在每日8:00重置，还需1小时20分钟'
      expect(formatQuotaKeyError(raw, makeT())).toBe('刷新次数达上限（5次），约 1 小时后重置。')
    })

    it('shareai 包裹的刷新上限消息经剥离后正常格式化', () => {
      const raw = 'shareai: ratelimit: 当日刷新次数达到上限(2次)，将在每日8:00重置，还需0小时10分钟'
      expect(formatQuotaKeyError(raw, makeT())).toBe('刷新次数达上限（2次），约 10 分钟后重置。')
    })

    it('英文文案走同一 key 体系', () => {
      const raw = '当日刷新次数达到上限(5次)，将在每日8:00重置，还需7小时0分钟'
      expect(formatQuotaKeyError(raw, makeT('en'))).toBe(
        'Refresh limit reached (5 times); resets in about 7 hours.'
      )
    })
  })

  describe('非匹配中文原文', () => {
    it('不匹配固定格式的中文错误原文原样返回（即使带前缀）', () => {
      const raw = 'shareai: db: 数据库连接异常'
      expect(formatQuotaKeyError(raw, makeT())).toBe('数据库连接异常')
    })

    it('不匹配固定格式且无前缀的中文原文原样返回', () => {
      const raw = '激活码已被上游吊销'
      expect(formatQuotaKeyError(raw, makeT())).toBe('激活码已被上游吊销')
    })
  })
})
