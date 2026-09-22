<template>
  <div data-testid="web-auto-login-form" class="space-y-3">
    <p class="input-hint">
      {{ isSmsMode ? t('admin.accounts.webLogin.autoLogin.zhipuKimiHint') : t('admin.accounts.webLogin.autoLogin.deepseekHint') }}
    </p>

    <div v-if="!isSmsMode" class="space-y-3">
      <!-- 注册仅限新建账号：携带 accountId 的是重登表单，注册提交不带 account_id，
           重登场景开放注册会建出新账号而非恢复原账号（外审 2026-09-22 P1）。 -->
      <div v-if="isDeepseek && props.accountId == null" data-testid="web-register-mode-toggle" class="flex items-center gap-2 text-sm">
        <button
          type="button"
          class="rounded px-2 py-1"
          :class="!registerMode ? 'bg-primary-600 text-white' : 'bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300'"
          @click="exitRegisterMode"
        >
          {{ t('admin.accounts.webLogin.register.modeTogglePassword') }}
        </button>
        <button
          type="button"
          class="rounded px-2 py-1"
          :class="registerMode ? 'bg-primary-600 text-white' : 'bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300'"
          @click="enterRegisterMode"
        >
          {{ t('admin.accounts.webLogin.register.modeToggle') }}
        </button>
      </div>

      <div v-if="!registerMode" class="space-y-3">
        <div>
          <label class="input-label">{{ t('admin.accounts.webLogin.autoLogin.emailLabel') }}</label>
          <input v-model="identifierValue" type="text" data-testid="web-auto-login-identifier" class="input" :placeholder="t('admin.accounts.webLogin.autoLogin.emailPlaceholder')" />
        </div>
        <div>
          <label class="input-label">{{ t('admin.accounts.webLogin.autoLogin.passwordLabel') }}</label>
          <input v-model="passwordValue" type="password" data-testid="web-auto-login-password" class="input" :placeholder="t('admin.accounts.webLogin.autoLogin.passwordPlaceholder')" />
        </div>
        <p v-if="errorMsg" data-testid="web-auto-login-error" class="text-sm text-red-600 dark:text-red-400">{{ errorMsg }}</p>
        <button type="button" data-testid="web-auto-login-submit" class="btn btn-primary btn-sm" :disabled="submitting" @click="submitPassword">
          {{ submitting ? t('admin.accounts.webLogin.autoLogin.submitting') : t('admin.accounts.webLogin.autoLogin.submit') }}
        </button>
      </div>

      <div v-else class="space-y-3">
        <div>
          <label class="input-label">{{ t('admin.accounts.webLogin.register.emailLabel') }}</label>
          <input v-model="registerEmail" type="text" data-testid="web-register-email" class="input" :placeholder="t('admin.accounts.webLogin.register.emailPlaceholder')" />
        </div>
        <button
          type="button"
          data-testid="web-register-send-code"
          class="btn btn-primary btn-sm"
          :disabled="registerSendingCode || registerCountdown > 0"
          @click="sendRegisterCode"
        >
          {{ registerSendingCode ? t('admin.accounts.webLogin.register.sendCodeSending') : t('admin.accounts.webLogin.register.sendCode') }}
        </button>
        <p v-if="codeSent" data-testid="web-register-code-sent-hint" class="text-sm text-green-600 dark:text-green-400">{{ codeSentHintText }}</p>
        <div>
          <label class="input-label">{{ t('admin.accounts.webLogin.register.codeLabel') }}</label>
          <input
            :value="registerCode"
            type="text"
            inputmode="numeric"
            data-testid="web-register-code"
            class="input"
            :placeholder="t('admin.accounts.webLogin.register.codePlaceholder')"
            :disabled="!codeSent"
            @input="onRegisterCodeInput"
          />
        </div>
        <div>
          <label class="input-label">{{ t('admin.accounts.webLogin.register.passwordLabel') }}</label>
          <input v-model="registerPassword" type="password" data-testid="web-register-password" class="input" :placeholder="t('admin.accounts.webLogin.register.passwordPlaceholder')" />
          <p class="input-hint">{{ t('admin.accounts.webLogin.register.passwordRuleHint') }}</p>
        </div>
        <p v-if="errorMsg" data-testid="web-register-error" class="text-sm text-red-600 dark:text-red-400">{{ errorMsg }}</p>
        <button type="button" data-testid="web-register-submit" class="btn btn-primary btn-sm" :disabled="submitting" @click="submitRegister">
          {{ submitting ? t('admin.accounts.webLogin.register.submitting') : t('admin.accounts.webLogin.register.submit') }}
        </button>
      </div>
    </div>

    <div v-else class="space-y-3">
      <div>
        <label class="input-label">{{ t('admin.accounts.webLogin.autoLogin.phoneLabel') }}</label>
        <input v-model="phoneValue" type="text" data-testid="web-auto-login-phone" class="input" :placeholder="t('admin.accounts.webLogin.autoLogin.phonePlaceholder')" />
      </div>

      <div data-testid="web-auto-login-challenge" class="rounded-lg border border-slate-200 bg-slate-50 p-3 text-sm dark:border-slate-700 dark:bg-slate-800/40">
        <p class="font-medium text-slate-700 dark:text-slate-200">{{ t('admin.accounts.webLogin.autoLogin.challengeSectionTitle') }}</p>
        <p data-testid="web-auto-login-challenge-status" class="mt-1 text-slate-500 dark:text-slate-400">{{ challengeStatusText }}</p>
        <p v-if="challengeText" data-testid="web-auto-login-challenge-text" class="mt-1">{{ challengeText }}</p>
      </div>

      <button type="button" data-testid="web-auto-login-send-code" class="btn btn-primary btn-sm" :disabled="submitting" @click="sendCode">
        {{ submitting ? t('admin.accounts.webLogin.autoLogin.smsSending') : t('admin.accounts.webLogin.autoLogin.smsSendCode') }}
      </button>

      <div v-if="smsStepReady" class="space-y-3 rounded-lg border border-amber-200 bg-amber-50 p-3 dark:border-amber-800 dark:bg-amber-900/20">
        <p class="text-sm font-medium text-amber-700 dark:text-amber-300">{{ t('admin.accounts.webLogin.autoLogin.smsStepTitle') }}</p>
        <div>
          <label class="input-label">{{ t('admin.accounts.webLogin.autoLogin.smsCodeLabel') }}</label>
          <input v-model="smsCode" type="text" data-testid="web-auto-login-sms-code" class="input" :placeholder="t('admin.accounts.webLogin.autoLogin.smsCodePlaceholder')" />
        </div>
        <p v-if="smsError" data-testid="web-auto-login-sms-error" class="text-sm text-red-600 dark:text-red-400">{{ smsError }}</p>
        <button type="button" data-testid="web-auto-login-sms-submit" class="btn btn-primary btn-sm" :disabled="submitting" @click="submitSms">
          {{ submitting ? t('admin.accounts.webLogin.autoLogin.submitting') : t('admin.accounts.webLogin.autoLogin.smsSubmit') }}
        </button>
      </div>

      <p v-if="errorMsg" data-testid="web-auto-login-error" class="text-sm text-red-600 dark:text-red-400">{{ errorMsg }}</p>
      <p v-if="smsSuccess" data-testid="web-auto-login-success" class="text-sm text-green-600 dark:text-green-400">{{ t('admin.accounts.webLogin.autoLogin.smsLoginSuccess') }}</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { webLoginPassword, webLoginSms, startWebLoginChallenge, getWebLoginChallengeStatus, consumeWebLoginChallenge, webRegisterEmailCode, webRegister, type WebLoginPasswordRequest, type WebLoginChallengeRequest } from '@/api/admin/webAutoLogin'
import type { CreateAccountRequest } from '@/types'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ platform: string; accountId?: number; accountDraft?: CreateAccountRequest }>()
const emit = defineEmits<{ (e: 'recovered', payload: { platform: string; account_id: number }): void }>()
const { t } = useI18n()
const isSmsMode = computed(() => props.platform === 'zhipu' || props.platform === 'kimi')
const isDeepseek = computed(() => props.platform === 'deepseek')
const identifierValue = ref('')
const passwordValue = ref('')
const phoneValue = ref('')
const smsCode = ref('')
const smsStepReady = ref(false)
const smsError = ref('')
const smsSuccess = ref(false)
const submitting = ref(false)
const errorMsg = ref('')
const challengeStatus = ref<'pending' | 'succeeded' | 'failed' | 'expired' | 'context_gap'>('pending')
const challengeText = ref('')
const challengeSessionId = ref<string | null>(null)
let pollTimer: ReturnType<typeof setTimeout> | undefined
let requestGeneration = 0
let mounted = true
let countdownTimer: ReturnType<typeof setInterval> | undefined

// ---- DeepSeek 邮箱注册模式（仅 deepseek 非短信分支；默认密码登录分支逻辑零改动）----
const registerMode = ref(false)
const registerEmail = ref('')
const registerCode = ref('')
const registerPassword = ref('')
const registerSendingCode = ref(false)
const codeSent = ref(false)
const registerCountdown = ref(0)

const codeSentHintText = computed(() => {
  if (registerCountdown.value > 0) return t('admin.accounts.webLogin.register.codeSentHint', { secs: registerCountdown.value })
  return t('admin.accounts.webLogin.register.codeSentHintNoWindow')
})

function clearCountdownTimer() {
  if (countdownTimer !== undefined) {
    clearInterval(countdownTimer)
    countdownTimer = undefined
  }
}

function startRegisterCountdown(secs: number) {
  clearCountdownTimer()
  registerCountdown.value = secs
  countdownTimer = setInterval(() => {
    if (registerCountdown.value > 0) registerCountdown.value -= 1
    if (registerCountdown.value <= 0) clearCountdownTimer()
  }, 1000)
}

/** 前端密码强度预校验：≥8 位且同时包含字母和数字（与上游规则一致，方案 §1.1）。 */
function isPasswordStrongEnough(password: string): boolean {
  return password.length >= 8 && /[A-Za-z]/.test(password) && /\d/.test(password)
}

function enterRegisterMode() {
  // 注册仅限新建账号（外审 2026-09-22 P1）：重登表单不允许进入注册模式。
  if (props.accountId != null) return
  registerMode.value = true
  errorMsg.value = ''
  codeSent.value = false
  registerCode.value = ''
}

function exitRegisterMode() {
  // 已在密码登录模式时 no-op（外审 2026-09-22 P2）：密码页按钮点击不得使在途
  // 密码登录请求失效。仅真正退出注册模式时才使注册类在途请求过期。
  if (!registerMode.value) return
  registerMode.value = false
  requestGeneration += 1
  // 代次递增会使在途请求的 finally 不再清理提交状态，此处同步清除（外审 P1）。
  submitting.value = false
  errorMsg.value = ''
  clearCountdownTimer()
  registerCountdown.value = 0
  registerSendingCode.value = false
  codeSent.value = false
  registerCode.value = ''
}

/**
 * 注册失败时切回「密码登录」并把已填邮箱/密码带过去。
 * 「注册成功但建号失败」（errorRegisteredUsePasswordLogin）场景用户可直接走密码登录。
 */
function fallbackToPasswordMode(passwordLoginEmail: string, passwordLoginPassword: string) {
  registerMode.value = false
  // 与 exitRegisterMode 同口径：使在途发码/注册请求过期，防止响应回写已重置状态。
  requestGeneration += 1
  // 代次递增会使在途请求的 finally 不再清理提交状态，此处同步清除，
  // 否则密码登录提交按钮持续禁用（外审 2026-09-22 P1）。
  submitting.value = false
  clearCountdownTimer()
  registerCountdown.value = 0
  registerSendingCode.value = false
  codeSent.value = false
  registerCode.value = ''
  identifierValue.value = passwordLoginEmail
  passwordValue.value = passwordLoginPassword
}

function onRegisterCodeInput(event: Event) {
  const el = event.target as HTMLInputElement | null
  registerCode.value = (el?.value || '').replace(/\D/g, '').slice(0, 6)
}

function isRecaptchaError(err: unknown) {
  const e = err as { status?: number; metadata?: Record<string, unknown> } | null
  const detail = typeof e?.metadata?.detail === 'string' ? String(e.metadata.detail).toUpperCase() : ''
  return e?.status === 502 || detail.includes('RECAPTCHA') || detail.includes('人机验证')
}

function isEmailDomainError(err: unknown) {
  // 仅按后端明确错误 token 判定（外审 2026-09-22 P1）：后端注册/发码失败统一
  // 返回 HTTP 400 + detail 文案（含 EMAIL_DOMAIN_NOT_SUPPORTED），HTTP 422 的
  // 其它校验失败不得误归为邮箱域名错误，须走统一透传。
  const e = err as { status?: number; metadata?: Record<string, unknown> } | null
  const detail = typeof e?.metadata?.detail === 'string' ? String(e.metadata.detail).toUpperCase() : ''
  return detail.includes('EMAIL_DOMAIN')
}

function isRegisteredUsePasswordLoginError(err: unknown) {
  const e = err as { status?: number; metadata?: Record<string, unknown> } | null
  const detail = typeof e?.metadata?.detail === 'string' ? String(e.metadata.detail) : ''
  return detail.includes('密码登录') || detail.toUpperCase().includes('EMAIL_EXISTS')
}

function registerErrorText(err: unknown): string {
  if (isRecaptchaError(err)) return t('admin.accounts.webLogin.register.errorRecaptcha')
  if (isEmailDomainError(err)) return t('admin.accounts.webLogin.register.errorEmailDomain')
  // 409 web_credential_duplicate：按任务卡 §2.2.3 展示后端返回的既有文案（透传 detail）。
  if ((err as { status?: number } | null)?.status === 409) return webLoginErrorText(err, t('admin.accounts.webLogin.register.registerFailed'))
  if (isRegisteredUsePasswordLoginError(err)) return t('admin.accounts.webLogin.register.errorRegisteredUsePasswordLogin')
  return webLoginErrorText(err, t('admin.accounts.webLogin.register.registerFailed'))
}

async function sendRegisterCode() {
  if (registerSendingCode.value || registerCountdown.value > 0) return
  errorMsg.value = ''
  codeSent.value = false
  const email = registerEmail.value.trim()
  if (!email) {
    errorMsg.value = t('admin.accounts.webLogin.register.emailRequired')
    return
  }
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) {
    errorMsg.value = t('admin.accounts.webLogin.register.emailInvalid')
    return
  }
  registerSendingCode.value = true
  // 请求代次（外审 2026-09-22 P2）：发码与提交共用 requestGeneration——等待期间
  // 切换平台/退出注册模式/其他提交都会使本请求过期，响应不得回写注册状态。
  const generation = ++requestGeneration
  try {
    const resp = await webRegisterEmailCode(props.platform, email)
    if (!mounted || generation !== requestGeneration) return
    if (!resp.success) {
      errorMsg.value = t('admin.accounts.webLogin.register.sendCodeFailed')
      fallbackToPasswordMode(email, registerPassword.value)
      return
    }
    codeSent.value = true
    startRegisterCountdown(resp.send_window_secs && resp.send_window_secs > 0 ? resp.send_window_secs : 60)
  } catch (err) {
    if (mounted && generation === requestGeneration) {
      errorMsg.value = registerErrorText(err)
      // 后端错误（人机验证 / 邮箱域不支持等）按任务卡 §2.2.3 切回「密码登录」并带回已填邮箱。
      fallbackToPasswordMode(email, registerPassword.value)
    }
  } finally {
    if (mounted && generation === requestGeneration) registerSendingCode.value = false
  }
}

async function submitRegister() {
  if (submitting.value) return
  // 注册仅限新建账号（外审 2026-09-22 P1）：重登表单提交不带 account_id，双保险守卫。
  if (props.accountId != null) return
  errorMsg.value = ''
  const email = registerEmail.value.trim()
  const code = registerCode.value.trim()
  const password = registerPassword.value
  if (!email) {
    errorMsg.value = t('admin.accounts.webLogin.register.emailRequired')
    return
  }
  if (!codeSent.value || !code) {
    errorMsg.value = t('admin.accounts.webLogin.register.codeRequired')
    return
  }
  if (!/^\d{6}$/.test(code)) {
    errorMsg.value = t('admin.accounts.webLogin.register.codeInvalid')
    return
  }
  if (!password.trim()) {
    errorMsg.value = t('admin.accounts.webLogin.register.passwordRequired')
    return
  }
  // 前端先校验密码强度，不通过不发请求（任务卡 §2.2 / 方案 §1.1）。
  if (!isPasswordStrongEnough(password)) {
    errorMsg.value = t('admin.accounts.webLogin.register.passwordWeak')
    return
  }
  if (props.accountId == null && props.accountDraft && !String(props.accountDraft.name || '').trim()) {
    errorMsg.value = t('admin.accounts.webLogin.autoLogin.accountNameRequired')
    return
  }
  submitting.value = true
  const generation = ++requestGeneration
  try {
    const resp = await webRegister({ platform: props.platform, email, email_verification_code: code, password, account_draft: props.accountDraft as CreateAccountRequest })
    if (!mounted || generation !== requestGeneration) return
    if (!resp.success || resp.account_id == null) {
      errorMsg.value = t('admin.accounts.webLogin.register.registerFailed')
      // HTTP 200 但业务失败（success:false）同样按任务卡 §2.2-3 切回「密码登录」
      // 并带回已填邮箱/密码，避免用户停留在无法继续的注册表单。
      fallbackToPasswordMode(email, password)
      return
    }
    emit('recovered', { platform: props.platform, account_id: resp.account_id })
  } catch (err) {
    if (mounted && generation === requestGeneration) {
      errorMsg.value = registerErrorText(err)
      // 任何注册链路错误都切回「密码登录」并带回已填邮箱/密码（任务卡 §2.2 第 3 条）。
      fallbackToPasswordMode(email, password)
    }
  } finally {
    if (mounted && generation === requestGeneration) submitting.value = false
  }
}
// ---- DeepSeek 邮箱注册模式结束 ----

const challengeStatusText = computed(() => t(`admin.accounts.webLogin.autoLogin.challengeStatus.${challengeStatus.value}`))

function clearPollTimer() {
  if (pollTimer !== undefined) {
    clearTimeout(pollTimer)
    pollTimer = undefined
  }
}

function challengePayload(): WebLoginChallengeRequest {
  const payload: WebLoginChallengeRequest = {
    platform: props.platform as 'zhipu' | 'kimi',
    phone: phoneValue.value.trim()
  }
  if (props.accountId != null) payload.account_id = props.accountId
  else if (props.accountDraft) payload.account_draft = props.accountDraft
  return payload
}

function isContextGapError(err: unknown) {
  const e = err as { status?: number; code?: number | string; metadata?: Record<string, unknown> } | null
  return e?.status === 501 || e?.code === 501 || e?.metadata?.reason === 'context_gap' || e?.metadata?.code === 'context_gap'
}

function webLoginErrorText(err: unknown, fallback: string) {
  const detail = (err as { metadata?: { detail?: unknown } } | null)?.metadata?.detail
  if (typeof detail === 'string' && detail.trim()) return detail
  return extractApiErrorMessage(err, fallback)
}

function stopChallenge(message: string, status: 'failed' | 'expired' | 'context_gap' = 'failed') {
  clearPollTimer()
  challengeStatus.value = status
  challengeSessionId.value = null
  smsStepReady.value = false
  errorMsg.value = message
}

async function pollChallenge(sessionId: string, generation: number): Promise<boolean> {
  while (mounted && generation === requestGeneration) {
    try {
      const resp = await getWebLoginChallengeStatus(sessionId)
      if (!mounted || generation !== requestGeneration) return false
      const status = resp.status
      if (status === 'succeeded') {
        challengeStatus.value = 'succeeded'
        return true
      }
      if (status === 'context_gap') {
        stopChallenge(resp.detail || t('admin.accounts.webLogin.autoLogin.challengeUnavailable'), 'context_gap')
        return false
      }
      if (status === 'failed' || status === 'expired') {
        stopChallenge(resp.detail || t('admin.accounts.webLogin.autoLogin.loginFailed'), status)
        return false
      }
      await new Promise<void>((resolve) => {
        pollTimer = setTimeout(resolve, 1000)
      })
    } catch (err) {
      if (mounted && generation === requestGeneration) {
        stopChallenge(isContextGapError(err)
          ? t('admin.accounts.webLogin.autoLogin.challengeUnavailable')
          : webLoginErrorText(err, t('admin.accounts.webLogin.autoLogin.loginFailed')),
        isContextGapError(err) ? 'context_gap' : 'failed')
      }
      return false
    }
  }
  return false
}

async function submitPassword() {
  if (submitting.value) return
  errorMsg.value = ''
  // identifier 可 trim；密码必须保留原始值（首尾空格可能是密码的一部分），仅空检查时 trim。
  const identifier = identifierValue.value.trim()
  const password = passwordValue.value
  if (!identifier || !password.trim()) {
    errorMsg.value = t('admin.accounts.webLogin.autoLogin.fillRequired')
    return
  }
  submitting.value = true
  const generation = ++requestGeneration
  try {
    const payload: WebLoginPasswordRequest = { platform: props.platform, login_email: identifier, login_password: password }
    if (props.accountId != null) payload.account_id = props.accountId
    else if (props.accountDraft) payload.account_draft = props.accountDraft
    const resp = await webLoginPassword(payload)
    if (!mounted || generation !== requestGeneration) return
    if (!resp.success || resp.account_id == null) {
      errorMsg.value = resp.detail || t('admin.accounts.webLogin.autoLogin.loginFailed')
      return
    }
    challengeStatus.value = 'succeeded'
    emit('recovered', { platform: props.platform, account_id: resp.account_id })
  } catch (err) {
    if (mounted && generation === requestGeneration) errorMsg.value = webLoginErrorText(err, t('admin.accounts.webLogin.autoLogin.loginFailed'))
  } finally {
    if (mounted && generation === requestGeneration) submitting.value = false
  }
}

function buildSmsPayload(action: 'send_code' | 'login'): Parameters<typeof webLoginSms>[0] {
  const payload: Parameters<typeof webLoginSms>[0] = { action, platform: props.platform as 'zhipu' | 'kimi', phone: phoneValue.value.trim() }
  if (challengeSessionId.value) payload.challenge_session_id = challengeSessionId.value
  if (action === 'login') payload.sms_code = smsCode.value.trim()
  return payload
}

async function sendCode() {
  if (submitting.value) return
  errorMsg.value = ''
  smsError.value = ''
  smsSuccess.value = false
  clearPollTimer()
  challengeSessionId.value = null
  if (!phoneValue.value.trim()) {
    errorMsg.value = t('admin.accounts.webLogin.autoLogin.phoneRequired')
    return
  }
  if (props.accountId == null && props.accountDraft && !String(props.accountDraft.name || '').trim()) {
    errorMsg.value = t('admin.accounts.webLogin.autoLogin.accountNameRequired')
    return
  }
  submitting.value = true
  challengeStatus.value = 'pending'
  challengeText.value = ''
  const generation = ++requestGeneration
  try {
    const resp = await startWebLoginChallenge(challengePayload())
    if (!mounted || generation !== requestGeneration) return
    if (!resp.success || !resp.session_id) {
      stopChallenge(resp.detail || t('admin.accounts.webLogin.autoLogin.challengeUnavailable'), resp.status === 'context_gap' ? 'context_gap' : 'failed')
      return
    }
    challengeSessionId.value = resp.session_id
    if (!(await pollChallenge(resp.session_id, generation))) return
    if (!mounted || generation !== requestGeneration) return
    const consumed = await consumeWebLoginChallenge(resp.session_id, challengePayload())
    if (!mounted || generation !== requestGeneration) return
    if (!consumed.success || (consumed.status !== 'consumed' && consumed.status !== 'succeeded')) {
      stopChallenge(consumed.detail || t('admin.accounts.webLogin.autoLogin.challengeUnavailable'))
      return
    }
    const smsResp = await webLoginSms(buildSmsPayload('send_code'))
    if (!mounted || generation !== requestGeneration) return
    if (!smsResp.success) {
      errorMsg.value = smsResp.detail || t('admin.accounts.webLogin.autoLogin.sendCodeFailed')
      return
    }
    smsStepReady.value = true
  } catch (err) {
    if (mounted && generation === requestGeneration) {
      stopChallenge(isContextGapError(err)
        ? t('admin.accounts.webLogin.autoLogin.challengeUnavailable')
        : webLoginErrorText(err, t('admin.accounts.webLogin.autoLogin.sendCodeFailed')),
      isContextGapError(err) ? 'context_gap' : 'failed')
    }
  } finally {
    if (mounted && generation === requestGeneration) submitting.value = false
  }
}

async function submitSms() {
  if (submitting.value) return
  errorMsg.value = ''
  smsError.value = ''
  smsSuccess.value = false
  if (!challengeSessionId.value || challengeStatus.value !== 'succeeded') {
    smsError.value = t('admin.accounts.webLogin.autoLogin.challengeUnavailable')
    return
  }
  if (!smsCode.value.trim()) {
    smsError.value = t('admin.accounts.webLogin.autoLogin.smsCodeRequired')
    return
  }
  submitting.value = true
  const generation = ++requestGeneration
  try {
    const resp = await webLoginSms(buildSmsPayload('login'))
    if (!mounted || generation !== requestGeneration) return
    if (!resp.success || resp.account_id == null) {
      smsError.value = resp.detail || t('admin.accounts.webLogin.autoLogin.loginFailed')
      return
    }
    smsSuccess.value = true
    emit('recovered', { platform: props.platform, account_id: resp.account_id })
  } catch (err) {
    if (mounted && generation === requestGeneration) {
      smsError.value = isContextGapError(err)
        ? t('admin.accounts.webLogin.autoLogin.challengeUnavailable')
        : webLoginErrorText(err, t('admin.accounts.webLogin.autoLogin.loginFailed'))
    }
  } finally {
    if (mounted && generation === requestGeneration) submitting.value = false
  }
}

watch(() => props.platform, () => {
  clearPollTimer()
  requestGeneration += 1
  submitting.value = false
  smsStepReady.value = false
  challengeSessionId.value = null
  smsCode.value = ''
  smsError.value = ''
  smsSuccess.value = false
  errorMsg.value = ''
  challengeStatus.value = 'pending'
  challengeText.value = ''
  // 注册模式状态重置（切平台时回到默认密码登录）
  registerMode.value = false
  clearCountdownTimer()
  registerCountdown.value = 0
  registerSendingCode.value = false
  registerEmail.value = ''
  registerCode.value = ''
  registerPassword.value = ''
  codeSent.value = false
})

onBeforeUnmount(() => {
  mounted = false
  clearPollTimer()
  clearCountdownTimer()
  requestGeneration += 1
})
</script>
