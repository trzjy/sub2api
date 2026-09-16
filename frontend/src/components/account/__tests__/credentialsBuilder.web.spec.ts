import { describe, expect, it } from 'vitest'
import {
  isWebProviderPlatform,
  buildWebProviderCredentials,
  webProviderUsesCookie,
} from '../credentialsBuilder'

describe('isWebProviderPlatform', () => {
  it('accepts the three web reverse platforms', () => {
    expect(isWebProviderPlatform('web-deepseek')).toBe(true)
    expect(isWebProviderPlatform('web-zhipu')).toBe(true)
    expect(isWebProviderPlatform('web-kimi')).toBe(true)
  })

  it('rejects API platforms and unknown values', () => {
    expect(isWebProviderPlatform('kimi')).toBe(false)
    expect(isWebProviderPlatform('deepseek')).toBe(false)
    expect(isWebProviderPlatform('web-other')).toBe(false)
    expect(isWebProviderPlatform('')).toBe(false)
  })
})

describe('webProviderUsesCookie', () => {
  it('cookie auth for DeepSeek / Zhipu web, token auth for Kimi web', () => {
    expect(webProviderUsesCookie('web-deepseek')).toBe(true)
    expect(webProviderUsesCookie('web-zhipu')).toBe(true)
    expect(webProviderUsesCookie('web-kimi')).toBe(false)
  })
})

describe('buildWebProviderCredentials', () => {
  it('builds cookie credentials for DeepSeek web with optional base_url', () => {
    const result = buildWebProviderCredentials('web-deepseek', {
      cookie: '  sessionid=abc; HWWAFSESID=xyz  ',
      kimiTokenJson: '',
      baseUrl: 'https://chat.example.com'
    })
    expect(result.error).toBeUndefined()
    expect(result.credentials).toEqual({
      cookie: 'sessionid=abc; HWWAFSESID=xyz',
      base_url: 'https://chat.example.com'
    })
  })

  it('omits base_url when blank', () => {
    const result = buildWebProviderCredentials('web-zhipu', {
      cookie: 'chatglm_token=abc',
      kimiTokenJson: '',
      baseUrl: '   '
    })
    expect(result.error).toBeUndefined()
    expect(result.credentials).toEqual({ cookie: 'chatglm_token=abc' })
  })

  it('rejects blank cookie', () => {
    const result = buildWebProviderCredentials('web-zhipu', {
      cookie: '   ',
      kimiTokenJson: '',
      baseUrl: ''
    })
    expect(result.error).toBe('webCookieRequired')
  })

  it('builds kimi credentials from a full token JSON', () => {
    const result = buildWebProviderCredentials('web-kimi', {
      cookie: '',
      kimiTokenJson: JSON.stringify({
        access_token: ' at ',
        refresh_token: ' rt ',
        user_id: 'u-123'
      }),
      baseUrl: ''
    })
    expect(result.error).toBeUndefined()
    expect(result.credentials).toEqual({
      access_token: 'at',
      refresh_token: 'rt',
      user_id: 'u-123'
    })
  })

  it('accepts kimi JSON with only access_token', () => {
    const result = buildWebProviderCredentials('web-kimi', {
      cookie: '',
      kimiTokenJson: '{"access_token":"at"}',
      baseUrl: ''
    })
    expect(result.error).toBeUndefined()
    expect(result.credentials).toEqual({ access_token: 'at' })
  })

  it('accepts numeric kimi user_id', () => {
    const result = buildWebProviderCredentials('web-kimi', {
      cookie: '',
      kimiTokenJson: '{"access_token":"at","user_id":42}',
      baseUrl: ''
    })
    expect(result.error).toBeUndefined()
    expect(result.credentials).toEqual({ access_token: 'at', user_id: 42 })
  })

  it('rejects unparseable / non-object kimi JSON', () => {
    for (const raw of ['not json', '[1,2]', '"str"', 'null']) {
      const result = buildWebProviderCredentials('web-kimi', {
        cookie: '',
        kimiTokenJson: raw,
        baseUrl: ''
      })
      expect(result.error).toBe('webKimiJsonInvalid')
    }
  })

  it('rejects kimi JSON without access_token', () => {
    const result = buildWebProviderCredentials('web-kimi', {
      cookie: '',
      kimiTokenJson: '{"refresh_token":"rt"}',
      baseUrl: ''
    })
    expect(result.error).toBe('webKimiAccessTokenRequired')
  })
})
