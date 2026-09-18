<template>
  <div data-testid="web-auto-login-form" class="space-y-3">
    <p class="input-hint">
      {{ needsPhone ? t('admin.accounts.webLogin.autoLogin.zhipuKimiHint') : t('admin.accounts.webLogin.autoLogin.deepseekHint') }}
    </p>

    <!-- 账号密码登录（第一步） -->
    <div v-if="!needsSms" class="space-y-3">
      <div>
        <label class="input-label">{{ identifierLabel }}</label>
        <input
          v-model="identifierValue"
          type="text"
          data-testid="web-auto-login-identifier"
          class="input"
          :placeholder="identifierPlaceholder"
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

    <!-- 短信验证码登录（第二步：zhipu / kimi needs_sms 时） -->
    <div v-else data-testid="web-auto-login-sms" class="space-y-3 rounded-lg border border-amber-200 bg-amber-50 p-3 dark:border-amber-800 dark:bg-amber-900/20">
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
  </div>
</template>

<script setup lang="ts">
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
  (e: 'recovered', payload: { platform: string; cookie: string }): void
}>()

const { t } = useI18n()

// zhipu / kimi 使用手机号；deepseek 使用邮箱。
const needsPhone = computed(() =>
  props.platform === 'zhipu' || props.platform === 'kimi'
)
const identifierLabel = computed(() =>
  needsPhone.value
    ? t('admin.accounts.webLogin.autoLogin.phoneLabel')
    : t('admin.accounts.webLogin.autoLogin.emailLabel')
)
const identifierPlaceholder = computed(() =>
  needsPhone.value
    ? t('admin.accounts.webLogin.autoLogin.phonePlaceholder')
    : t('admin.accounts.webLogin.autoLogin.emailPlaceholder')
)

const identifierValue = ref('')
const passwordValue = ref('')
const submitting = ref(false)
const errorMsg = ref('')

const needsSms = ref(false)
const sessionToken = ref('')
const smsCode = ref('')
const smsError = ref('')

async function submitPassword() {
  errorMsg.value = ''
  smsError.value = ''
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
    if (needsPhone.value) payload.login_phone = identifier
    else payload.login_email = identifier
    if (props.accountId != null) payload.account_id = props.accountId

    const resp = await webLoginPassword(payload)
    if (!resp.success) {
      // 业务级失败：如实展示后端 detail（中文），前端兜底 i18n key。
      errorMsg.value = resp.detail || t('admin.accounts.webLogin.autoLogin.loginFailed')
      return
    }
    if (resp.needs_sms) {
      needsSms.value = true
      sessionToken.value = resp.session_token ?? ''
      return
    }
    if (resp.cookie) {
      emit('recovered', { platform: props.platform, cookie: resp.cookie })
      return
    }
    errorMsg.value = t('admin.accounts.webLogin.autoLogin.loginFailed')
  } catch (err) {
    errorMsg.value = extractApiErrorMessage(err, t('admin.accounts.webLogin.autoLogin.loginFailed'))
  } finally {
    submitting.value = false
  }
}

async function submitSms() {
  smsError.value = ''
  const code = smsCode.value.trim()
  if (!code) {
    smsError.value = t('admin.accounts.webLogin.autoLogin.smsCodeRequired')
    return
  }
  submitting.value = true
  try {
    const resp = await webLoginSms({ session_token: sessionToken.value, sms_code: code })
    if (!resp.success) {
      // 本期短信发码通道尚未接入：如实展示后端返回的细化错误，不伪造成功。
      smsError.value = resp.detail || t('admin.accounts.webLogin.autoLogin.smsNotConnected')
      return
    }
    if (resp.cookie) {
      emit('recovered', { platform: props.platform, cookie: resp.cookie })
      return
    }
    smsError.value = t('admin.accounts.webLogin.autoLogin.smsNotConnected')
  } catch (err) {
    smsError.value = extractApiErrorMessage(err, t('admin.accounts.webLogin.autoLogin.smsNotConnected'))
  } finally {
    submitting.value = false
  }
}
</script>
