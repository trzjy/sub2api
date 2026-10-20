interface NavigatorUADataLike {
  mobile?: boolean
}

interface NavigatorLike {
  userAgent?: string
  platform?: string
  maxTouchPoints?: number
  userAgentData?: NavigatorUADataLike
}

interface MediaQueryResultLike {
  matches: boolean
}

interface DeviceDetectionEnvironment {
  navigator?: NavigatorLike
  matchMedia?: (query: string) => MediaQueryResultLike | null | undefined
}

const MOBILE_UA_RE = /\b(Mobi|Android|iPhone|iPod|Windows Phone|webOS|BlackBerry|IEMobile)\b/i
const TABLET_UA_RE = /\b(iPad|Tablet)\b/i
const IOS_UA_RE = /\b(iPhone|iPad|iPod)\b/i
const WECHAT_UA_RE = /MicroMessenger/i

function matchesQuery(
  matchMedia: DeviceDetectionEnvironment['matchMedia'],
  query: string,
): boolean {
  try {
    return matchMedia?.(query)?.matches === true
  } catch {
    return false
  }
}

export function detectMobileDevice(env: DeviceDetectionEnvironment = {}): boolean {
  const nav = env.navigator
  if (!nav) return false

  if (nav.userAgentData?.mobile === true) {
    return true
  }

  const userAgent = nav.userAgent || ''
  const maxTouchPoints = nav.maxTouchPoints ?? 0
  const isIPadOSDesktopMode = nav.platform === 'MacIntel' && maxTouchPoints > 1
  const isMobileUA = MOBILE_UA_RE.test(userAgent)
  const isTabletUA = TABLET_UA_RE.test(userAgent) || isIPadOSDesktopMode
  const coarsePointer = matchesQuery(env.matchMedia, '(pointer: coarse)')
  const noHover = matchesQuery(env.matchMedia, '(hover: none)')
  const hasTouch = maxTouchPoints > 0

  return isMobileUA || isTabletUA || (coarsePointer && noHover && hasTouch)
}

export function isMobileDevice(): boolean {
  if (typeof navigator === 'undefined') return false

  return detectMobileDevice({
    navigator,
    matchMedia: typeof window !== 'undefined' ? window.matchMedia.bind(window) : undefined,
  })
}

export function detectIOSDevice(env: DeviceDetectionEnvironment = {}): boolean {
  const nav = env.navigator
  if (!nav) return false

  const userAgent = nav.userAgent || ''
  const maxTouchPoints = nav.maxTouchPoints ?? 0
  // iPadOS 13+ 桌面模式下 UA 伪装成 macOS，需要靠触控点数识别。
  const isIPadOSDesktopMode = nav.platform === 'MacIntel' && maxTouchPoints > 1

  return IOS_UA_RE.test(userAgent) || isIPadOSDesktopMode
}

export function isIOSDevice(): boolean {
  if (typeof navigator === 'undefined') return false

  return detectIOSDevice({ navigator })
}

export function detectWeChatBrowser(env: DeviceDetectionEnvironment = {}): boolean {
  return WECHAT_UA_RE.test(env.navigator?.userAgent || '')
}

/** True when the page runs inside the WeChat in-app browser. Some payment
 * channels cannot complete a purchase there, so callers use this to guide the
 * user to a browser where the flow does work. */
export function isWeChatBrowser(): boolean {
  if (typeof navigator === 'undefined') return false

  return detectWeChatBrowser({ navigator })
}
