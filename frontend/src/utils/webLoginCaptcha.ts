// 网页登录人机验证 SDK 加载器（zhipu 数美滑块 / kimi 易盾）。
//
// 证据来源（已逆向线上 bundle，非猜测）：
//   - kimi 易盾：13 号取证 user-BEVNGF-E.js → CAPTCHA_ID="2752f01d87dc45948de1a1e0ac2b7160"，
//     初始化 window.initNECaptchaWithFallback / loadExternalScript("yidun-captcha.js")，
//     {captchaId, mode:"popup", apiVersion:2}；onSuccess(instance, data) → data.validate。
//     公共 SDK 加载器：https://cstaticdun.126.net/load.min.js（注册 window.initNECaptcha）。
//   - zhipu 数美：本轮逆向 chatglm.cn/main.*.js → 数美 SDK 自托管于 https://chatglm.cn/smcp/smcp.min.js
//     （注册 window.initSMCaptcha）；initSMCaptcha({organization:"599zinRadlRxTLrOkTR9", appendTo,
//     product:"embed", width, style})；onSuccess(res) → res.rid（=pic_captcha_id）、res.pass。
//     md5（图形校验值）来源未在混淆 SDK 中确认，onSuccess 结果若含 md5/token/validate 一并尽力回填。
//
// 交互语义：验证码在本管理页内解出（rid/validate 为 JS 回调值），随 web-login-sms 请求回传后端，
// 由 chatglm/kimi 服务端向数美/易盾服务端校验——校验与嵌入域无关，故内嵌可行。若供应商对
// referer 域做校验导致 SDK 加载/验证失败，调用方应回退到手动回填框（见 WebAutoLoginForm）。

// ── 易盾（kimi）──

export interface YidunCaptchaResult {
  validate: string
  [key: string]: unknown
}

export type YidunCaptchaConstructor = (config: Record<string, unknown>) => YidunCaptchaInstance

export interface YidunCaptchaInstance {
  destroy?: () => void
  refresh?: () => void
}

declare global {
  interface Window {
    initNECaptcha?: YidunCaptchaConstructor
    initSMCaptcha?: ShumeiCaptchaConstructor
  }
}

const YIDUN_SDK_SRC = 'https://cstaticdun.126.net/load.min.js'

/** kimi 易盾验证码 ID（13 号取证固定值，已写死于后端 web_sms_login.go）。 */
export const KIMI_CAPTCHA_ID = '2752f01d87dc45948de1a1e0ac2b7160'

let yidunPromise: Promise<YidunCaptchaConstructor> | null = null

export function loadYidunCaptcha(): Promise<YidunCaptchaConstructor> {
  if (window.initNECaptcha) return Promise.resolve(window.initNECaptcha)
  if (yidunPromise) return yidunPromise
  yidunPromise = loadScript(YIDUN_SDK_SRC).then(() => {
    if (!window.initNECaptcha) throw new Error('易盾验证码 SDK 未就绪')
    return window.initNECaptcha
  })
  return yidunPromise
}

// ── 数美（zhipu）──

export const ZHIPU_SHUMEI_ORG = '599zinRadlRxTLrOkTR9'

const SHUMEI_SDK_SRC = 'https://chatglm.cn/smcp/smcp.min.js'

export interface ShumeiCaptchaResult {
  rid?: string
  pass?: string | boolean
  md5?: string
  token?: string
  validate?: string
  [key: string]: unknown
}

export type ShumeiCaptchaConstructor = (config: Record<string, unknown>) => ShumeiCaptchaInstance

export interface ShumeiCaptchaInstance {
  onSuccess: (cb: (res: ShumeiCaptchaResult) => void) => void
  onClose?: (cb: () => void) => void
  onReady?: (cb: (...args: unknown[]) => void) => void
  reset?: () => void
  destroy?: () => void
}

let shumeiPromise: Promise<ShumeiCaptchaConstructor> | null = null

export function loadShumeiCaptcha(): Promise<ShumeiCaptchaConstructor> {
  if (window.initSMCaptcha) return Promise.resolve(window.initSMCaptcha)
  if (shumeiPromise) return shumeiPromise
  shumeiPromise = loadScript(SHUMEI_SDK_SRC).then(() => {
    if (!window.initSMCaptcha) throw new Error('数美验证码 SDK 未就绪')
    return window.initSMCaptcha
  })
  return shumeiPromise
}

// 数美 slide 组件基础样式（取自 chatglm.cn main.js 既有配置，避免渲染异常）。
const SHUMEI_SLIDE_STYLE = {
  slideBar: {
    color: '#999',
    background: '#F3F7FA',
    successColor: '#FFF',
    border: '1px solid #3e6ffb',
    process: {
      background: 'rgba(62, 111, 251, .1)',
      successBackground: 'rgba(62, 111, 251, .1)',
      failBackground: '#F5E0E2'
    },
    button: {
      boxShadow: 'none',
      color: '#fff',
      successColor: '#3e6ffb',
      failColor: '#Fff',
      background: '#3e6ffb',
      successBackground: '#3e6ffb'
    }
  }
}

/**
 * mountShumeiCaptcha 在指定 DOM 容器内渲染数美滑块并绑定求解回调。
 * 成功后回调 onSolved({rid, md5?})；渲染/加载失败回调 onError（调用方回退手动框）。
 */
export function mountShumeiCaptcha(
  containerId: string,
  onSolved: (res: ShumeiCaptchaResult) => void,
  onError: (err: unknown) => void
): void {
  loadShumeiCaptcha()
    .then((init) => {
      const instance = init({
        organization: ZHIPU_SHUMEI_ORG,
        appendTo: containerId,
        product: 'embed',
        width: '100%',
        hideRefreshOnImage: false,
        style: SHUMEI_SLIDE_STYLE
      })
      instance.onSuccess((res: ShumeiCaptchaResult) => onSolved(res))
      instance.onClose?.(() => {})
    })
    .catch((err) => onError(err))
}

/**
 * launchYidunCaptcha 以弹窗模式拉起易盾验证码，求解后回调 onSolved({validate})。
 * 加载/渲染失败回调 onError（调用方回退手动框）。
 */
export function launchYidunCaptcha(
  onSolved: (validate: string) => void,
  onError: (err: unknown) => void
): void {
  loadYidunCaptcha()
    .then((init) => {
      init({
        captchaId: KIMI_CAPTCHA_ID,
        mode: 'popup',
        apiVersion: 2,
        onSuccess: (_instance: YidunCaptchaInstance, data: YidunCaptchaResult) => {
          if (data && data.validate) onSolved(String(data.validate))
          else onError(new Error('易盾验证结果缺少 validate'))
        },
        onError: (err: unknown) => onError(err)
      })
    })
    .catch((err) => onError(err))
}

function loadScript(src: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const existing = document.querySelector(`script[src="${src}"]`)
    if (existing) {
      resolve()
      return
    }
    const script = document.createElement('script')
    script.src = src
    script.async = true
    script.onload = () => resolve()
    script.onerror = () => reject(new Error('验证码 SDK 加载失败: ' + src))
    document.head.appendChild(script)
  })
}
