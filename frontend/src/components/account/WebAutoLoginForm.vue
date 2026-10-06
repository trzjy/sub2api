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
import { webLoginPassword, webLoginSms, startWebLoginChallenge, getWebLoginChallengeStatus, consumeWebLoginChallenge, webRegisterEmailCode, webRegister, newWebRegisterIdempotencyKey, type WebLoginPasswordRequest, type WebLoginChallengeRequest, type WebRegisterRequest } from '@/api/admin/webAutoLogin'
import type { CreateAccountRequest } from '@/types'
import { extractApiErrorMessage, extractApiErrorCode } from '@/utils/apiError'

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
// 外审 2026-09-22 R2-P2：本次注册操作的幂等键。提交开始时生成；出站前即与请求快照
// 登记配对（外审 R6-P1）；结果不明（网络错误/超时/5xx，服务端可能已注册建号）时保留，
// 用户重试复用同键 → 协调器重放首次结果；收到明确结果（成功/F4/409/业务码终态）、
// 切换平台或显式重发码时废弃。
let registerIdempotencyKey: string | null = null
// 外审 2026-09-22 R3-P3：与幂等键配对保留的原提交 payload（出站前即登记，外审
// R6-P1——在途期间退出重进也能据此判漂移，不再依赖失败响应路径补写）。同键重试必须
// 用原验证码原 payload（协调器 fingerprint 校验 payload 一致；真实重发码会得到新
// 验证码导致 payload 漂移，旧键触发 fingerprint 冲突无法重放）。重进注册模式时回填
// 原字段（codeSent 视为已发，可直接提交同键重试）；用户显式重发码 = 明确开启新操作，
// 轮换新键并清除本配对（首次注册可能已成功，重试后端按 EMAIL_EXISTS/终态指引收敛）。
let registerRetryPayload: { payload: WebRegisterRequest; idempotencyKey: string } | null = null

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
  // 外审 R3-P3：结果不明后的重进 = 同键重试路径——回填原提交字段（含原验证码），
  // codeSent 视为已发，用户可直接提交复用原键原 payload。
  if (registerRetryPayload) {
    const p = registerRetryPayload.payload
    registerEmail.value = String(p.email ?? '')
    registerCode.value = String(p.email_verification_code ?? '')
    registerPassword.value = String(p.password ?? '')
    codeSent.value = true
    return
  }
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
  // 幂等键与重试 payload 不在此废弃（外审 R2-P2/R3-P3）：结果不明（网络错误/5xx）
  // 后的 fallback 正是「切走再切回重试」的路径，键+原 payload 必须跨模式切换保留，
  // 重进时回填原字段供同键重试。只随明确结果或显式重发码（新操作）废弃。
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

// 外审 2026-09-22 P1-3：post-success 终态（上游注册已成功、登录/建号失败）由后端以
// metadata.post_success_register=true 显式标记，F4 恢复指引在 message、原始原因在
// metadata.detail。必须先识别该标记再取 detail，否则 detail 会把恢复指引顶掉，
// 用户不知道账号已注册、不能再次注册。
function isPostSuccessRegisterError(err: unknown) {
  return Boolean((err as { metadata?: Record<string, unknown> } | null)?.metadata?.post_success_register)
}

function registerErrorText(err: unknown): string {
  if (isPostSuccessRegisterError(err)) {
    // F4 恢复指引（本地化文案）+ 原始失败原因（后端 metadata.detail）同屏展示。
    const detail = typeof (err as { metadata?: Record<string, unknown> } | null)?.metadata?.detail === 'string'
      ? String((err as { metadata?: Record<string, unknown> }).metadata!.detail)
      : ''
    const guide = t('admin.accounts.webLogin.register.errorRegisteredUsePasswordLogin')
    return detail && detail !== guide ? `${guide}（${detail}）` : guide
  }
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
  // 外审 R3-P3：显式重发码 = 明确开启新操作。旧键配对的原验证码作废（真实重发得到
  // 新验证码，同键+新 payload 会触发协调器 fingerprint 冲突），轮换新键并清除重试
  // 配对；首次注册可能已成功——重试若命中 EMAIL_EXISTS/终态指引，按既定文案收敛。
  registerIdempotencyKey = null
  registerRetryPayload = null
  registerSendingCode.value = true
  // 请求代次（外审 2026-09-22 P2）：发码与提交共用 requestGeneration——等待期间
  // 切换平台/退出注册模式/其他提交都会使本请求过期，响应不得回写注册状态。
  const generation = ++requestGeneration
  try {
    const resp = await webRegisterEmailCode(props.platform, email, props.accountDraft?.proxy_id ?? null)
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
  // 幂等键管理（外审 R2-P2 + R3-P3 + R4-P2 + R6-P1）：键与完整请求快照配对，且配对
  // 在出站前即登记。后续提交先比较配对快照：仅当表单可控字段完全一致（含 account_
  // draft——父组件草稿可变）才复用同键重试；任何漂移（新验证码/改草稿名称/分组/代理等）
  // = 明确新操作，轮换新键，避免协调器 fingerprint 冲突使已完成的建号结果无法重放。
  // 结果不明（网络错误/超时/5xx）时保留键+快照配对供同键重试；明确结果（成功/400/
  // 409/422/success:false）到达即废弃。
  // 外审 R5-P3：出站前即冻结完整载荷——account_draft 深拷贝。父组件在在途期间
  // 原地修改草稿（名称/分组/代理）不得改变已出站请求的语义；同一冻结快照既用于
  // 发送也用于重试配对保存，保证同键重试与首次出站字节一致。
  const payload: WebRegisterRequest = {
    platform: props.platform,
    email,
    email_verification_code: code,
    password,
    account_draft: JSON.parse(JSON.stringify(props.accountDraft ?? {})) as CreateAccountRequest
  }
  if (registerRetryPayload) {
    const paired = registerRetryPayload.payload
    // 漂移判定只看表单可控字段（邮箱/验证码/密码/平台）——account_draft 不参与：
    // 父组件可在在途期间原地变异草稿对象（外审 R5-P3），变异不是本表单的用户操作，
    // 不得被判为「新操作」轮换键。同操作重试直接复用配对的冻结快照出站，
    // 保证同键重试与首次出站字节一致（协调器 fingerprint 重放才能命中）。
    const sameOperation =
      paired.platform === payload.platform &&
      paired.email === payload.email &&
      paired.email_verification_code === payload.email_verification_code &&
      paired.password === payload.password
    if (!sameOperation) {
      // 表单字段漂移（改邮箱/换验证码/改密码）= 新操作：轮换键并清除旧配对。
      registerIdempotencyKey = null
      registerRetryPayload = null
    } else {
      payload.account_draft = paired.account_draft
    }
  }
  if (!registerIdempotencyKey) {
    registerIdempotencyKey = newWebRegisterIdempotencyKey('register')
  }
  const usedKey = registerIdempotencyKey
  // 外审 R6-P1：出站前即登记键+冻结快照配对——在途期间用户退出重进改表单再提交时，
  // 后续提交能先比较该快照判漂移（漂移则轮换新键），而不是拿旧键发新载荷触发指纹冲突。
  // 此时 payload.account_draft 可能已被同操作分支替换为上一轮配对的冻结快照——这正是
  // 要保存的对象。演进说明：R4-P3 曾以「仅持键者可写」守卫在响应路径补写配对，本条
  // 前置登记使所有响应路径都不再写配对，过期响应无从覆盖，守卫职责自然终结。
  registerRetryPayload = { payload: JSON.parse(JSON.stringify(payload)), idempotencyKey: usedKey }
  try {
    const resp = await webRegister(payload, { idempotencyKey: usedKey })
    if (!mounted || generation !== requestGeneration) {
      // 代次已过期（用户切走/后续请求已发起）：结果对当前界面不可见，静默丢弃。
      // 配对已在出站前登记（外审 R6-P1），此处不再有任何配对写点。
      return
    }
    // 明确结果：废弃键与重试配对，后续提交是新操作。
    registerIdempotencyKey = null
    registerRetryPayload = null
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
      const status = (err as { status?: number } | null)?.status
      const errCode = extractApiErrorCode(err)
      // 外审 R5-P2：409 分两类——协调器 processing/退避冲突（IDEMPOTENCY_IN_PROGRESS /
      // IDEMPOTENCY_RETRY_BACKOFF）不是终态：首次请求仍在处理，必须保留键与快照，
      // 按 Retry-After 走同键重试重放；仅明确业务终态（web_credential_duplicate 去重、
      // IDEMPOTENCY_KEY_CONFLICT 指纹冲突）与其他 4xx 才弃键。
      const isCoordinatorConflict409 =
        status === 409 &&
        (errCode === 'IDEMPOTENCY_IN_PROGRESS' || errCode === 'IDEMPOTENCY_RETRY_BACKOFF')
      const outcomeKnown =
        status === 409
          ? !isCoordinatorConflict409
          : status === 400 || status === 422 || (typeof status === 'number' && status < 500)
      if (outcomeKnown) {
        // 后端明确业务终态（400 F4/结果不明指引/409 去重/422 校验）：该键已消费，废弃。
        registerIdempotencyKey = null
        registerRetryPayload = null
      }
      // 结果不明（5xx/网络错误）分支：配对已在出站前登记（外审 R6-P1），此处不再
      // 需要写点——R4-P3「仅持键者可写」守卫随响应路径写点消失而自然终结。
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
  // 外审 R6-P2：切平台 = 新上下文，清掉旧平台的注册幂等键与重试配对——
  // 否则切走再切回会 enterRegisterMode 回填旧邮箱/验证码并复用旧键重放前一次注册。
  registerIdempotencyKey = null
  registerRetryPayload = null
})

onBeforeUnmount(() => {
  mounted = false
  clearPollTimer()
  clearCountdownTimer()
  requestGeneration += 1
})
</script>
