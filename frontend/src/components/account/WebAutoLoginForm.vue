<template>
  <div data-testid="web-auto-login-form" class="space-y-3">
    <p class="input-hint">
      {{
        isSmsMode
          ? t('admin.accounts.webLogin.autoLogin.zhipuKimiHint')
          : t('admin.accounts.webLogin.autoLogin.deepseekHint')
      }}
    </p>

    <!-- 账号密码登录（仅 deepseek 官方网页端支持） -->
    <div v-if="!isSmsMode" class="space-y-3">
      <div>
        <label class="input-label">{{ t('admin.accounts.webLogin.autoLogin.emailLabel') }}</label>
        <input
          v-model="identifierValue"
          type="text"
          data-testid="web-auto-login-identifier"
          class="input"
          :placeholder="t('admin.accounts.webLogin.autoLogin.emailPlaceholder')"
        />
      </div>
      <div>
        <label class="input-label">{{ t('admin.accounts.webLogin.autoLogin.passwordLabel') }}</label>
        <input
          v-model="passwordValue"
          type="password"
          data-testid="web-auto-login-password"
          class="input"
          :placeholder="t('admin.accounts.webLogin.autoLogin.passwordPlaceholder')"
        />
      </div>
      <p v-if="errorMsg" data-testid="web-auto-login-error" class="text-sm text-red-600 dark:text-red-400">
        {{ errorMsg }}
      </p>
      <button
        type="button"
        data-testid="web-auto-login-submit"
        class="btn btn-primary btn-sm"
        :disabled="submitting"
        @click="submitPassword"
      >
        {{ submitting ? t('admin.accounts.webLogin.autoLogin.submitting') : t('admin.accounts.webLogin.autoLogin.submit') }}
      </button>
    </div>

    <!-- 手机号 + 短信码登录（zhipu / kimi 唯一用户入口） -->
    <div v-else class="space-y-3">
      <!-- 第一步：手机号 + 获取验证码 -->
      <div>
        <label class="input-label">{{ t('admin.accounts.webLogin.autoLogin.phoneLabel') }}</label>
        <input
          v-model="phoneValue"
          type="text"
          data-testid="web-auto-login-phone"
          class="input"
          :placeholder="t('admin.accounts.webLogin.autoLogin.phonePlaceholder')"
        />
      </div>

      <!-- 人机验证回传：用户在自有浏览器完成滑块/验证码后，把求解值回填到下方字段，
           随发码与登录请求一并回传（挑战值仅作验证求解值，非登录载体）。 -->
      <div
        v-if="isSmsMode"
        data-testid="web-auto-login-challenge-fields"
        class="space-y-3 rounded-lg border border-slate-200 bg-slate-50 p-3 dark:border-slate-700 dark:bg-slate-800/40"
      >
        <p class="text-sm font-medium text-slate-700 dark:text-slate-200">
          {{ t('admin.accounts.webLogin.autoLogin.challengeSectionTitle') }}
        </p>

        <!-- 智谱：数美滑块 rid + 图形校验 md5 + 手机区号 -->
        <template v-if="platform === 'zhipu'">
          <div>
            <label class="input-label">{{ t('admin.accounts.webLogin.autoLogin.zhipuCaptchaRidLabel') }}</label>
            <input
              v-model="zhipuCaptchaRid"
              type="text"
              data-testid="web-auto-login-zhipu-rid"
              class="input"
              :placeholder="t('admin.accounts.webLogin.autoLogin.zhipuCaptchaRidLabel')"
            />
          </div>
          <div>
            <label class="input-label">{{ t('admin.accounts.webLogin.autoLogin.zhipuCaptchaMd5Label') }}</label>
            <input
              v-model="zhipuCaptchaMd5"
              type="text"
              data-testid="web-auto-login-zhipu-md5"
              class="input"
              :placeholder="t('admin.accounts.webLogin.autoLogin.zhipuCaptchaMd5Label')"
            />
          </div>
          <div>
            <label class="input-label">{{ t('admin.accounts.webLogin.autoLogin.zhipuPhoneCodeLabel') }}</label>
            <input
              v-model="zhipuPhoneCode"
              type="text"
              data-testid="web-auto-login-zhipu-phone-code"
              class="input"
              :placeholder="t('admin.accounts.webLogin.autoLogin.zhipuPhoneCodePlaceholder')"
            />
          </div>
        </template>

        <!-- Kimi：网易易盾 validate -->
        <div v-if="platform === 'kimi'">
          <label class="input-label">{{ t('admin.accounts.webLogin.autoLogin.kimiCaptchaValidateLabel') }}</label>
          <input
            v-model="kimiCaptchaValidate"
            type="text"
            data-testid="web-auto-login-kimi-validate"
            class="input"
            :placeholder="t('admin.accounts.webLogin.autoLogin.kimiCaptchaValidateLabel')"
          />
        </div>
      </div>

      <button
        type="button"
        data-testid="web-auto-login-send-code"
        class="btn btn-primary btn-sm"
        :disabled="submitting"
        @click="sendCode"
      >
        {{ submitting ? t('admin.accounts.webLogin.autoLogin.smsSending') : t('admin.accounts.webLogin.autoLogin.smsSendCode') }}
      </button>

      <!-- 人工挑战提示：当后端返回 needs_challenge==="true" 时，提示用户在自有浏览器
           完成滑块/验证码，将回传值（rid/md5 或 validate）填入上方挑战字段后重试。 -->
      <div
        v-if="challengeWaiting"
        data-testid="web-auto-login-challenge"
        class="rounded-lg border border-amber-200 bg-amber-50 p-3 text-sm text-amber-700 dark:border-amber-800 dark:bg-amber-900/20 dark:text-amber-300"
      >
        <p class="font-medium">{{ t('admin.accounts.webLogin.autoLogin.smsChallengeWaiting') }}</p>
        <p v-if="challengeText" class="mt-1">{{ challengeText }}</p>
      </div>

      <!-- 第二步：短信验证码提交 -->
      <div
        v-if="smsStepReady"
        class="space-y-3 rounded-lg border border-amber-200 bg-amber-50 p-3 dark:border-amber-800 dark:bg-amber-900/20"
      >
        <p class="text-sm font-medium text-amber-700 dark:text-amber-300">
          {{ t('admin.accounts.webLogin.autoLogin.smsStepTitle') }}
        </p>
        <div>
          <label class="input-label">{{ t('admin.accounts.webLogin.autoLogin.smsCodeLabel') }}</label>
          <input
            v-model="smsCode"
            type="text"
            data-testid="web-auto-login-sms-code"
            class="input"
            :placeholder="t('admin.accounts.webLogin.autoLogin.smsCodePlaceholder')"
          />
        </div>
        <p v-if="smsError" data-testid="web-auto-login-sms-error" class="text-sm text-red-600 dark:text-red-400">
          {{ smsError }}
        </p>
        <button
          type="button"
          data-testid="web-auto-login-sms-submit"
          class="btn btn-primary btn-sm"
          :disabled="submitting"
          @click="submitSms"
        >
          {{ submitting ? t('admin.accounts.webLogin.autoLogin.submitting') : t('admin.accounts.webLogin.autoLogin.smsSubmit') }}
        </button>
      </div>

      <p v-if="errorMsg" data-testid="web-auto-login-error" class="text-sm text-red-600 dark:text-red-400">
        {{ errorMsg }}
      </p>
      <p
        v-if="smsSuccess"
        data-testid="web-auto-login-success"
        class="text-sm text-green-600 dark:text-green-400"
      >
        {{ t('admin.accounts.webLogin.autoLogin.smsLoginSuccess') }}
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
// zhipu / kimi 官方网页端没有密码登录（微信扫码/短信码），其唯一用户入口是手机号 +
// 短信码双步骤登录（后端 web-login-sms）；deepseek 仍走账号密码登录。两种模式由平台决定，
// deepseek 分支逻辑保持现状不变，zhipu/kimi 直接呈现 send_code → login 流程。
// 人机验证挑战值（滑块 rid/md5、易盾 validate 等）由用户在自有浏览器完成验证后回填表单，
// 随发码与登录请求一并回传，仅作为验证求解值，而非登录载体。
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  webLoginPassword,
  webLoginSms,
  type WebLoginPasswordRequest
} from '@/api/admin/webAutoLogin'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{
  platform: string
  /** 已有账号重登回填时携带，新建账号省略 */
  accountId?: number
}>()

const emit = defineEmits<{
  (
    e: 'recovered',
    payload: {
      platform: string
      cookie?: string
      account_id?: number
      access_token?: string
      login_email?: string
      login_phone?: string
      login_password?: string
    }
  ): void
}>()

const { t } = useI18n()

// zhipu / kimi 使用手机号 + 短信码；deepseek 使用账号密码。
const isSmsMode = computed(() => props.platform === 'zhipu' || props.platform === 'kimi')

// ── deepseek：账号密码登录（一步到位，回填 cookie） ──
const identifierValue = ref('')
const passwordValue = ref('')
const submitting = ref(false)
const errorMsg = ref('')

async function submitPassword() {
  errorMsg.value = ''
  const identifier = identifierValue.value.trim()
  const password = passwordValue.value.trim()
  if (!identifier || !password) {
    errorMsg.value = t('admin.accounts.webLogin.autoLogin.fillRequired')
    return
  }
  submitting.value = true
  try {
    const payload: WebLoginPasswordRequest = {
      platform: props.platform,
      login_password: password
    }
    // deepseek 分支：仅使用登录邮箱。
    payload.login_email = identifier
    if (props.accountId != null) payload.account_id = props.accountId

    const resp = await webLoginPassword(payload)
    if (!resp.success) {
      // 业务级失败：如实展示后端 detail（中文），前端兜底 i18n key。
      errorMsg.value = resp.detail || t('admin.accounts.webLogin.autoLogin.loginFailed')
      return
    }
    if (resp.cookie) {
      // 把本次登录所用邮箱与密码一并带出，供建号时写入账号 credentials，
      // 供 Cookie 失效后的自动续期使用（deepseek 密码登录）。
      const recoveredPayload: {
        platform: string
        cookie: string
        login_email?: string
        login_phone?: string
        login_password?: string
      } = { platform: props.platform, cookie: resp.cookie }
      recoveredPayload.login_email = identifier
      recoveredPayload.login_password = password
      emit('recovered', recoveredPayload)
      return
    }
    errorMsg.value = t('admin.accounts.webLogin.autoLogin.loginFailed')
  } catch (err) {
    errorMsg.value = extractApiErrorMessage(err, t('admin.accounts.webLogin.autoLogin.loginFailed'))
  } finally {
    submitting.value = false
  }
}

// ── zhipu / kimi：手机号 + 短信码双步骤登录 ──
const phoneValue = ref('')
const smsCode = ref('')
const smsStepReady = ref(false)
const smsError = ref('')
const smsSuccess = ref(false)
const challengeWaiting = ref(false)
const challengeText = ref('')

// 人机验证挑战值：用户在自有浏览器完成滑块/验证码后回填，随发码与登录请求回传。
const zhipuCaptchaRid = ref('')
const zhipuCaptchaMd5 = ref('')
const zhipuPhoneCode = ref('')
const kimiCaptchaValidate = ref('')

// 把拦截器摊平的平面错误解析为「人工挑战」或「普通错误」展示文案。
// 挑战失败/超时：metadata.needs_challenge==="true"，优先展示 hint（无 hint 回退 message）。
function resolveSmsError(err: unknown): { isChallenge: boolean; text: string } {
  const e = err as { message?: string; metadata?: Record<string, unknown> } | null
  if (e?.metadata && e.metadata.needs_challenge === 'true') {
    const hint = typeof e.metadata.hint === 'string' ? e.metadata.hint : ''
    return { isChallenge: true, text: hint || e?.message || '' }
  }
  return { isChallenge: false, text: extractApiErrorMessage(err, t('admin.accounts.webLogin.autoLogin.loginFailed')) }
}

function buildSmsPayload(action: 'send_code' | 'login'): Parameters<typeof webLoginSms>[0] {
  const payload: Parameters<typeof webLoginSms>[0] = {
    action,
    platform: props.platform as 'zhipu' | 'kimi',
    phone: phoneValue.value.trim()
  }
  if (props.accountId != null) payload.account_id = props.accountId
  if (action === 'login') payload.sms_code = smsCode.value.trim()
  // 挑战求解值：用户在自有浏览器完成滑块/验证码后回填，已填才拼入（空字符串不拼），
  // 随 send_code 与 login 两个 action 一并回传；zhipu 需三个字段，kimi 仅需 validate。
  const zhipuRid = zhipuCaptchaRid.value.trim()
  const zhipuMd5 = zhipuCaptchaMd5.value.trim()
  const zhipuPhoneCodeVal = zhipuPhoneCode.value.trim()
  const kimiValidate = kimiCaptchaValidate.value.trim()
  if (zhipuRid) payload.zhipu_captcha_rid = zhipuRid
  if (zhipuMd5) payload.zhipu_captcha_md5 = zhipuMd5
  if (zhipuPhoneCodeVal) payload.zhipu_phone_code = zhipuPhoneCodeVal
  if (kimiValidate) payload.kimi_captcha_validate = kimiValidate
  return payload
}

async function sendCode() {
  errorMsg.value = ''
  smsError.value = ''
  smsSuccess.value = false
  const phone = phoneValue.value.trim()
  if (!phone) {
    errorMsg.value = t('admin.accounts.webLogin.autoLogin.phoneRequired')
    return
  }
  submitting.value = true
  try {
    const resp = await webLoginSms(buildSmsPayload('send_code'))
    if (resp.success) {
      // 发码成功（后端不回传会话令牌，E0 取证）：进入第二步输入短信码；清除挑战等待态。
      smsStepReady.value = true
      challengeWaiting.value = false
      challengeText.value = ''
      return
    }
    // 合约上 send_code 成功为 {success:true}，fail-closed 兜底（不应出现）。
    errorMsg.value = resp.detail || t('admin.accounts.webLogin.autoLogin.sendCodeFailed')
  } catch (err) {
    const { isChallenge, text } = resolveSmsError(err)
    if (isChallenge) {
      // 人工挑战失败/超时：保持手机号步骤可见，允许重试获取验证码。
      challengeWaiting.value = true
      challengeText.value = text
      return
    }
    errorMsg.value = text
  } finally {
    submitting.value = false
  }
}

async function submitSms() {
  errorMsg.value = ''
  smsError.value = ''
  smsSuccess.value = false
  const code = smsCode.value.trim()
  if (!code) {
    smsError.value = t('admin.accounts.webLogin.autoLogin.smsCodeRequired')
    return
  }
  submitting.value = true
  try {
    const resp = await webLoginSms(buildSmsPayload('login'))
    if (!resp.success) {
      smsError.value = resp.detail || t('admin.accounts.webLogin.autoLogin.loginFailed')
      return
    }
    // 登录成功：后端已新建或回填账号并回传登录态，原样带出（不落前端日志）。
    smsSuccess.value = true
    const recovered: {
      platform: string
      cookie?: string
      account_id?: number
      access_token?: string
    } = { platform: props.platform }
    if (resp.account_id != null) recovered.account_id = resp.account_id
    if (resp.cookie) recovered.cookie = resp.cookie
    if (resp.access_token) recovered.access_token = resp.access_token
    emit('recovered', recovered)
  } catch (err) {
    const { isChallenge, text } = resolveSmsError(err)
    if (isChallenge) {
      // 提交阶段人工挑战失败/超时：保持短信码步骤可见，允许重试提交。
      challengeWaiting.value = true
      challengeText.value = text
      return
    }
    smsError.value = text
  } finally {
    submitting.value = false
  }
}
</script>
