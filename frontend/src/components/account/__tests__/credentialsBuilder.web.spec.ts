import { describe, expect, it } from 'vitest'
import {
  WEB_PROVIDER_PLATFORMS,
  isWebProviderPlatform,
  buildWebProviderCredentials,
  isUpstreamBillingProbeEligible,
  webProviderUsesCookie,
} from '../credentialsBuilder'

describe('isWebProviderPlatform', () => {
  it('accepts the official provider platforms that support web access mode', () => {
    // 平台归并（PR-3）：web-* 不再是独立枚举，网页接入下沉为账号级 access_mode='web'。
    expect(WEB_PROVIDER_PLATFORMS).toEqual(['kimi', 'zhipu', 'deepseek'])
    for (const platform of WEB_PROVIDER_PLATFORMS) {
      expect(isWebProviderPlatform(platform)).toBe(true)
    }
  })

  it('rejects non-web providers and unknown values', () => {
    expect(isWebProviderPlatform('openai')).toBe(false)
    expect(isWebProviderPlatform('anthropic')).toBe(false)
    expect(isWebProviderPlatform('other')).toBe(false)
    expect(isWebProviderPlatform('codebuddy')).toBe(false)
    expect(isWebProviderPlatform('')).toBe(false)
  })
})

describe('webProviderUsesCookie', () => {
  it('cookie auth for DeepSeek / Zhipu web, token auth for Kimi web', () => {
    expect(webProviderUsesCookie('deepseek')).toBe(true)
    expect(webProviderUsesCookie('zhipu')).toBe(true)
    expect(webProviderUsesCookie('kimi')).toBe(false)
  })
})

describe('isUpstreamBillingProbeEligible', () => {
  // 与后端 IsUpstreamBillingProbeIdentity 共享白名单：仅这些平台的 apikey 账号
  // 可持有对上游 /v1/sub2api/billing 探测的静态密钥。web-* / other / codebuddy
  // 不在名单内，创建请求不得携带 upstream_billing_probe_enabled=true。
  it('returns true for the eligible apikey platforms', () => {
    for (const platform of [
      'openai',
      'anthropic',
      'gemini',
      'antigravity',
      'grok',
      'kimi',
      'zhipu',
      'deepseek',
      'minimax',
    ]) {
      expect(isUpstreamBillingProbeEligible(platform, 'apikey')).toBe(true)
    }
  })

  it('returns false for web reverse / other / codebuddy platforms even as apikey', () => {
    for (const platform of [
      'web-other',
      'other',
      'codebuddy',
    ]) {
      expect(isUpstreamBillingProbeEligible(platform, 'apikey')).toBe(false)
    }
  })

  it('returns false for the eligible platforms when type is not apikey (e.g. oauth)', () => {
    for (const platform of [
      'openai',
      'anthropic',
      'gemini',
      'antigravity',
      'grok',
      'kimi',
      'zhipu',
      'deepseek',
      'minimax',
    ]) {
      expect(isUpstreamBillingProbeEligible(platform, 'oauth')).toBe(false)
    }
  })

  it('returns false for an unknown platform regardless of type', () => {
    expect(isUpstreamBillingProbeEligible('does-not-exist', 'apikey')).toBe(false)
    expect(isUpstreamBillingProbeEligible('', 'apikey')).toBe(false)
  })
})

describe('buildWebProviderCredentials', () => {
  it('builds cookie credentials for DeepSeek web with optional base_url', () => {
    const result = buildWebProviderCredentials('deepseek', {
      cookie: '  sessionid=abc; HWWAFSESID=xyz  ',
      baseUrl: 'https://chat.example.com'
    })
    expect(result.error).toBeUndefined()
    expect(result.credentials).toEqual({
      access_mode: 'web',
      cookie: 'sessionid=abc; HWWAFSESID=xyz',
      base_url: 'https://chat.example.com'
    })
  })

  it('omits base_url when blank', () => {
    const result = buildWebProviderCredentials('zhipu', {
      cookie: 'chatglm_token=abc',
      baseUrl: '   '
    })
    expect(result.error).toBeUndefined()
    expect(result.credentials).toEqual({ access_mode: 'web', cookie: 'chatglm_token=abc' })
  })

  it('rejects blank cookie', () => {
    const result = buildWebProviderCredentials('zhipu', {
      cookie: '   ',
      baseUrl: ''
    })
    expect(result.error).toBe('webCookieRequired')
  })

  it('fails closed for kimi (SMS login only, no manual token paste)', () => {
    const result = buildWebProviderCredentials('kimi', {
      cookie: '',
      baseUrl: ''
    })
    expect(result.error).toBe('webKimiAccessTokenRequired')
  })

  it('writes auto-renewal login credentials when provided (deepseek)', () => {
    const result = buildWebProviderCredentials('deepseek', {
      cookie: 'sessionid=abc',
      baseUrl: '',
      loginEmail: '  user@deepseek.com  ',
      loginPassword: '  pw  ',
      loginPhone: ' 13800000000 '
    })
    expect(result.error).toBeUndefined()
    expect(result.credentials).toEqual({
      access_mode: 'web',
      cookie: 'sessionid=abc',
      login_email: 'user@deepseek.com',
      login_password: 'pw',
      login_phone: '13800000000'
    })
  })

  it('omits login_* keys when not provided (default behavior unchanged)', () => {
    const result = buildWebProviderCredentials('deepseek', {
      cookie: 'sessionid=abc',
      baseUrl: ''
    })
    expect(result.error).toBeUndefined()
    expect(result.credentials).toEqual({ access_mode: 'web', cookie: 'sessionid=abc' })
  })

  it('omits login_* keys when provided as blank', () => {
    const result = buildWebProviderCredentials('deepseek', {
      cookie: 'sessionid=abc',
      baseUrl: '',
      loginEmail: '   ',
      loginPassword: '   ',
      loginPhone: ''
    })
    expect(result.error).toBeUndefined()
    expect(result.credentials).toEqual({ access_mode: 'web', cookie: 'sessionid=abc' })
  })
})
