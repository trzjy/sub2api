import { ref, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type { CodeBuddyTokenInfo } from '@/api/admin/codebuddy'

/**
 * CodeBuddy OAuth composable.
 *
 * CodeBuddy uses a *polling* device-flow (no PKCE, upstream-issued state):
 *   1. generateAuthUrl -> open auth_url in browser
 *   2. startPolling    -> repeatedly call pollToken({ state }) until the user
 *                         finishes the browser login.
 *   3. On HTTP 400 + "登录未完成…" the backend means "login still pending" — we
 *      MUST keep polling, not treat it as a fatal error (plan §9.8).
 *   4. On a successful tokenInfo the onSuccess callback fires and polling stops.
 */

const POLL_INTERVAL_MS = 3000
const POLL_MAX_ATTEMPTS = 120 // ~6 minutes ceiling before giving up

export function useCodebuddyOAuth() {
  const appStore = useAppStore()
  const { t } = useI18n()

  const authUrl = ref('')
  const state = ref('')
  const loading = ref(false)
  const error = ref('')
  const polling = ref(false)
  const pollStatus = ref('')
  const pollAttempt = ref(0)

  let pollTimer: ReturnType<typeof setTimeout> | null = null
  let pollAborted = false

  const resetState = () => {
    stopPolling()
    authUrl.value = ''
    state.value = ''
    loading.value = false
    error.value = ''
    pollStatus.value = ''
    pollAttempt.value = 0
  }

  const generateAuthUrl = async (proxyId: number | null | undefined): Promise<boolean> => {
    loading.value = true
    authUrl.value = ''
    state.value = ''
    error.value = ''

    try {
      const payload: Record<string, unknown> = {}
      if (proxyId) payload.proxy_id = proxyId

      const response = await adminAPI.codebuddy.generateAuthUrl(payload as any)
      authUrl.value = response.auth_url
      state.value = response.state
      return true
    } catch (err: any) {
      error.value = err?.message || t('admin.accounts.oauth.codebuddy.failedToGenerateUrl')
      appStore.showError(error.value)
      return false
    } finally {
      loading.value = false
    }
  }

  const stopPolling = () => {
    polling.value = false
    pollAborted = true
    if (pollTimer) {
      clearTimeout(pollTimer)
      pollTimer = null
    }
  }

  const doPoll = async (
    onSuccess?: (tokenInfo: CodeBuddyTokenInfo) => void,
    onError?: (err: unknown) => void
  ) => {
    if (pollAborted || !state.value) return
    pollAttempt.value += 1

    try {
      const tokenInfo = await adminAPI.codebuddy.pollToken({ state: state.value })
      // Success: login completed.
      stopPolling()
      pollStatus.value = t('admin.accounts.oauth.codebuddy.loginCompleted')
      onSuccess?.(tokenInfo as CodeBuddyTokenInfo)
    } catch (err: any) {
      // Pending (plan §9.8): HTTP 400 + "登录未完成…" => keep polling.
      if (err?.status === 400 && err?.message && err.message.includes('登录未完成')) {
        pollStatus.value = t('admin.accounts.oauth.codebuddy.polling')
        if (pollAttempt.value >= POLL_MAX_ATTEMPTS) {
          stopPolling()
          error.value = t('admin.accounts.oauth.codebuddy.pollTimeout')
          onError?.(err)
          return
        }
        pollTimer = setTimeout(() => void doPoll(onSuccess, onError), POLL_INTERVAL_MS)
        return
      }
      // Fatal error.
      stopPolling()
      error.value = err?.message || t('admin.accounts.oauth.codebuddy.failedToPoll')
      onError?.(err)
    }
  }

  const startPolling = (opts?: {
    onSuccess?: (tokenInfo: CodeBuddyTokenInfo) => void
    onError?: (err: unknown) => void
  }) => {
    if (!state.value) {
      error.value = t('admin.accounts.oauth.codebuddy.missingExchangeParams')
      opts?.onError?.(new Error(error.value))
      return
    }
    pollAborted = false
    polling.value = true
    pollAttempt.value = 0
    pollStatus.value = t('admin.accounts.oauth.codebuddy.polling')
    void doPoll(opts?.onSuccess, opts?.onError)
  }

  const validateRefreshToken = async (
    refreshToken: string,
    _proxyId?: number | null
  ): Promise<CodeBuddyTokenInfo | null> => {
    if (!refreshToken.trim()) {
      error.value = t('admin.accounts.oauth.codebuddy.pleaseEnterRefreshToken')
      return null
    }

    loading.value = true
    error.value = ''

    try {
      const tokenInfo = await adminAPI.codebuddy.refreshCodeBuddyToken({
        refresh_token: refreshToken.trim()
      })
      return tokenInfo as CodeBuddyTokenInfo
    } catch (err: any) {
      error.value = err?.message || t('admin.accounts.oauth.codebuddy.failedToValidateRT')
      return null
    } finally {
      loading.value = false
    }
  }

  const buildCredentials = (tokenInfo: CodeBuddyTokenInfo): Record<string, unknown> => {
    const creds: Record<string, unknown> = {
      access_token: tokenInfo.access_token,
      refresh_token: tokenInfo.refresh_token
    }
    // Backend persists expires_at as a unix-seconds string.
    if (typeof tokenInfo.expires_at === 'number' && Number.isFinite(tokenInfo.expires_at)) {
      creds.expires_at = Math.floor(tokenInfo.expires_at).toString()
    } else if (typeof tokenInfo.expires_at === 'string' && tokenInfo.expires_at.trim()) {
      creds.expires_at = tokenInfo.expires_at.trim()
    }
    if (tokenInfo.domain) creds.domain = tokenInfo.domain
    if (tokenInfo.uid) creds.uid = tokenInfo.uid
    if (tokenInfo.enterprise_id) creds.enterprise_id = tokenInfo.enterprise_id
    if (tokenInfo.nickname) creds.nickname = tokenInfo.nickname
    return creds
  }

  onUnmounted(() => stopPolling())

  return {
    authUrl,
    state,
    loading,
    error,
    polling,
    pollStatus,
    pollAttempt,
    resetState,
    generateAuthUrl,
    startPolling,
    stopPolling,
    validateRefreshToken,
    buildCredentials
  }
}
