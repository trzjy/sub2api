<template>
  <div data-testid="web-auto-login-form" class="space-y-3">
    <p class="input-hint">
      {{ isSmsMode ? t('admin.accounts.webLogin.autoLogin.zhipuKimiHint') : t('admin.accounts.webLogin.autoLogin.deepseekHint') }}
    </p>

    <div v-if="!isSmsMode" class="space-y-3">
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
import { webLoginPassword, webLoginSms, startWebLoginChallenge, getWebLoginChallengeStatus, consumeWebLoginChallenge, type WebLoginPasswordRequest, type WebLoginChallengeRequest } from '@/api/admin/webAutoLogin'
import type { CreateAccountRequest } from '@/types'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ platform: string; accountId?: number; accountDraft?: CreateAccountRequest }>()
const emit = defineEmits<{ (e: 'recovered', payload: { platform: string; account_id: number }): void }>()
const { t } = useI18n()
const isSmsMode = computed(() => props.platform === 'zhipu' || props.platform === 'kimi')
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
          : extractApiErrorMessage(err, t('admin.accounts.webLogin.autoLogin.loginFailed')),
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
  const identifier = identifierValue.value.trim()
  const password = passwordValue.value.trim()
  if (!identifier || !password) {
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
    if (mounted && generation === requestGeneration) errorMsg.value = extractApiErrorMessage(err, t('admin.accounts.webLogin.autoLogin.loginFailed'))
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
        : extractApiErrorMessage(err, t('admin.accounts.webLogin.autoLogin.sendCodeFailed')),
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
        : extractApiErrorMessage(err, t('admin.accounts.webLogin.autoLogin.loginFailed'))
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
})

onBeforeUnmount(() => {
  mounted = false
  clearPollTimer()
  requestGeneration += 1
})
</script>
