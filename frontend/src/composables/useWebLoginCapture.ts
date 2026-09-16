import { ref, onUnmounted } from 'vue'
import { getWebLoginProxyCapture } from '@/api/admin/accounts'

/**
 * Web-login cookie capture composable.
 *
 * After the backend opens a same-origin proxy session (createWebLoginProxySession),
 * the login page is embedded in an iframe pointing at the proxy url. We poll the
 * backend for the captured cookie and, once available, surface it via `capturedCookie`.
 *
 * Polling contract (mirrors useCodebuddyOAuth):
 *   - POLL_INTERVAL_MS between attempts, POLL_MAX_ATTEMPTS hard ceiling.
 *   - captured=true -> write cookie, stop.
 *   - attempts reach the ceiling -> set timedOut, stop.
 *   - request error -> keep polling (network blips must not abort capture).
 *   - stop()/unmount -> clear the timer.
 */

const POLL_INTERVAL_MS = 3000
const POLL_MAX_ATTEMPTS = 100

export function useWebLoginCapture() {
  const capturing = ref(false)
  const capturedCookie = ref('')
  const timedOut = ref(false)

  let timer: ReturnType<typeof setTimeout> | null = null
  let attempts = 0
  let stopped = true

  const stop = () => {
    stopped = true
    capturing.value = false
    if (timer) {
      clearTimeout(timer)
      timer = null
    }
  }

  const start = (token: string) => {
    stopped = false
    attempts = 0
    capturedCookie.value = ''
    timedOut.value = false
    capturing.value = true

    const tick = async () => {
      if (stopped) return
      attempts += 1
      try {
        const result = await getWebLoginProxyCapture(token)
        if (stopped) return
        if (result.captured) {
          capturedCookie.value = result.cookie
          capturing.value = false
          stop()
          return
        }
      } catch {
        // Network/transient error: keep polling, do not abort capture.
      }

      if (stopped) return
      if (attempts >= POLL_MAX_ATTEMPTS) {
        timedOut.value = true
        capturing.value = false
        stop()
        return
      }
      timer = setTimeout(() => void tick(), POLL_INTERVAL_MS)
    }

    void tick()
  }

  onUnmounted(() => stop())

  return {
    capturing,
    capturedCookie,
    timedOut,
    start,
    stop
  }
}
