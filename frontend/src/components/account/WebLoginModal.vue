<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.webLogin.title')"
    width="wide"
    @close="handleClose"
  >
    <div class="space-y-4">
      <!-- 封号风险提示（方案 §2.2） -->
      <div data-testid="web-login-risk-warning" class="rounded-lg border border-red-200 bg-red-50 p-3 dark:border-red-800 dark:bg-red-900/20">
        <p class="text-sm font-medium text-red-700 dark:text-red-300">
          {{ t('admin.accounts.webProviders.riskWarning.title') }}
        </p>
        <p class="mt-1 text-xs text-red-600 dark:text-red-400">
          {{ t('admin.accounts.webProviders.riskWarning.body') }}
        </p>
      </div>

      <!-- 平台说明 -->
      <p class="text-sm text-gray-600 dark:text-gray-400">
        {{ platformHint }}
      </p>

      <!-- Kimi：新窗口引导登录（www.kimi.com） -->
      <div v-if="platform === 'web-kimi'" class="space-y-2">
        <button
          type="button"
          data-testid="web-login-kimi-open"
          class="btn btn-secondary"
          @click="openKimiWindow"
        >
          {{ t('admin.accounts.webLogin.openKimi') }}
        </button>
        <p class="input-hint">{{ t('admin.accounts.webLogin.kimiGuide') }}</p>
      </div>

      <!-- DeepSeek / Zhipu：内嵌 iframe 登录页。
           跨域限制说明（方案 §2.2 已列风险）：官方登录页与站点不同源，iframe 内
           Cookie（HttpOnly / 跨域）无法由本站 JS 读取，自动捕获在浏览器层不可行；
           本弹窗的主要价值是让用户在弹窗内完成官方登录，捕获走下方手动粘贴路径。
           同域登录回调代理（仅管理端）为后续任务，落地后自动捕获在此接入。 -->
      <div v-if="platform !== 'web-kimi'" class="space-y-2">
        <div class="flex items-center justify-between">
          <label class="text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.accounts.webLogin.iframeTitle') }}
          </label>
          <button
            type="button"
            data-testid="web-login-reload-iframe"
            class="text-xs text-primary-600 hover:text-primary-700 dark:text-primary-400"
            @click="reloadIframe"
          >
            {{ t('admin.accounts.webLogin.reload') }}
          </button>
        </div>
        <div data-testid="web-login-iframe-wrap" class="relative h-[420px] w-full overflow-hidden rounded-lg border border-gray-200 dark:border-dark-500">
          <iframe
            :key="iframeKey"
            :src="loginUrl"
            class="h-full w-full"
            :sandbox="iframeSandbox"
            referrerpolicy="no-referrer"
            :title="t('admin.accounts.webLogin.iframeTitle')"
            @load="onIframeLoad"
          ></iframe>
        </div>
        <p class="input-hint">
          {{ iframeBlocked
            ? t('admin.accounts.webLogin.iframeBlockedHint')
            : t('admin.accounts.webLogin.iframeHint') }}
        </p>
      </div>

      <!-- 手动粘贴凭证（兜底路径，方案 §2.3：三平台都必须支持） -->
      <div class="space-y-2">
        <label class="input-label">{{ pasteLabel }}</label>
        <textarea
          v-model="pastedCredentials"
          rows="4"
          :data-testid="webProviderUsesCookie(webPlatform) ? 'web-login-cookie-input' : 'web-login-token-json'"
          class="input font-mono"
          :placeholder="pastePlaceholder"
        ></textarea>
        <p class="input-hint">{{ t('admin.accounts.webLogin.pasteHint') }}</p>
      </div>

      <!-- 校验结果 -->
      <p v-if="validationError" data-testid="web-login-validation-error" class="text-sm text-red-600 dark:text-red-400">
        {{ validationError }}
      </p>

      <div class="flex justify-end gap-2">
        <button type="button" class="btn btn-secondary" @click="handleClose">
          {{ t('common.cancel') }}
        </button>
        <button
          type="button"
          data-testid="web-login-submit"
          class="btn btn-primary"
          :disabled="validating"
          @click="handleValidateAndApply"
        >
          {{ validating ? t('common.loading') + '...' : t('admin.accounts.webLogin.apply') }}
        </button>
      </div>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { buildWebProviderCredentials, isWebProviderPlatform, webProviderUsesCookie, type WebProviderPlatform } from '@/components/account/credentialsBuilder'
import { validateWebCredentials } from '@/api/admin/accounts'

const props = defineProps<{
  show: boolean
  platform: string
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'applied', payload: { platform: string; credentials: Record<string, unknown> }): void
}>()

const { t } = useI18n()

// 弹窗仅从网页逆向平台表单区打开；platform 为宽 union，非 web 平台时收敛到默认
// （isWebProviderPlatform 守卫），确保 webProviderUsesCookie 等强类型调用安全。
const webPlatform = computed<WebProviderPlatform>(() =>
  isWebProviderPlatform(props.platform) ? props.platform : 'web-deepseek')

const pastedCredentials = ref('')
const validating = ref(false)
const validationError = ref('')
const iframeKey = ref(0)
const iframeBlocked = ref(false)

const loginUrl = computed(() => {
  switch (props.platform) {
    case 'web-deepseek':
      return 'https://chat.deepseek.com/'
    case 'web-zhipu':
      return 'https://chatglm.cn/'
    default:
      return ''
  }
})

// iframe sandbox：allow-same-origin 不给（跨域登录页不应获得本站 origin 语义）；
// allow-scripts 保留让官方登录页自身可运行；allow-forms / allow-popups 支持登录交互。
const iframeSandbox = 'allow-scripts allow-forms allow-popups'

const platformHint = computed(() =>
  t(`admin.accounts.webLogin.platformHint.${props.platform}`))

const pasteLabel = computed(() =>
  webProviderUsesCookie(webPlatform.value)
    ? t('admin.accounts.webProviders.cookieLabel')
    : t('admin.accounts.webProviders.kimiTokenLabel'))

const pastePlaceholder = computed(() =>
  webProviderUsesCookie(webPlatform.value)
    ? t('admin.accounts.webProviders.cookiePlaceholder')
    : t('admin.accounts.webProviders.kimiTokenPlaceholder'))

watch(() => props.show, (open) => {
  if (open) {
    pastedCredentials.value = ''
    validationError.value = ''
    validating.value = false
    iframeBlocked.value = false
    iframeKey.value++
  }
})

function reloadIframe() {
  iframeBlocked.value = false
  iframeKey.value++
}

// onIframeLoad：iframe load 事件对 XFO 拦截页同样触发（浏览器渲染拦截页），
// 无法可靠探测 X-Frame-Options；跨域 contentDocument 访问必抛，仅作降级探测信号。
function onIframeLoad() {
  try {
    const frame = document.querySelector<HTMLIFrameElement>('[data-testid="web-login-iframe-wrap"] iframe')
    if (!frame) return
    // 跨域时该访问抛 SecurityError → 保持 iframe 展示（登录页可交互）。
    // 同源探测成功与否均不改变捕获路径（跨域捕获不可行，见模板注释）。
    void frame.contentDocument
  } catch {
    // 跨域（预期）：登录页已可交互，不标记阻断。
  }
}

function openKimiWindow() {
  window.open('https://www.kimi.com/', '_blank', 'noopener')
}

// handleValidateAndApply：调后端预创建校验端点确认凭证形状可用（真实上游可用性
// 经转发路径验证），再 emit 给 CreateAccountModal 预填现有表单——保持单一创建
// 路径（方案 §2.1「校验后创建」中"创建"复用 W1 现有提交链路，不另建第二实现）。
async function handleValidateAndApply() {
  validationError.value = ''
  const raw = pastedCredentials.value.trim()
  if (!raw) {
    validationError.value = webProviderUsesCookie(webPlatform.value)
      ? t('admin.accounts.webProviders.errors.webCookieRequired')
      : t('admin.accounts.webProviders.errors.webKimiJsonInvalid')
    return
  }

  // 构建凭证（复用 W1 credentialsBuilder 同一实现，前端形状校验 + Kimi JSON 解析）。
  let credentials: Record<string, unknown> | null = null
  let buildError: string | null = null
  try {
    const built = buildWebProviderCredentials(webPlatform.value, {
      cookie: webProviderUsesCookie(webPlatform.value) ? raw : '',
      kimiTokenJson: webProviderUsesCookie(webPlatform.value) ? '' : raw,
      baseUrl: ''
    })
    if (built.error || !built.credentials) {
      buildError = t(`admin.accounts.webProviders.errors.${built.error ?? 'webCookieRequired'}`)
    } else {
      credentials = built.credentials
    }
  } catch {
    buildError = t('admin.accounts.webProviders.errors.webKimiJsonInvalid')
  }
  if (buildError || !credentials) {
    validationError.value = buildError ?? t('admin.accounts.webProviders.errors.webCookieRequired')
    return
  }

  validating.value = true
  try {
    const ok = await validateWebCredentials(props.platform, credentials)
    if (!ok) {
      validationError.value = t('admin.accounts.webLogin.validationFailed')
      return
    }
    emit('applied', { platform: props.platform, credentials })
    emit('close')
  } catch {
    // 后端错误文案不透传凭证值；统一回落通用失败提示。
    validationError.value = t('admin.accounts.webLogin.validationFailed')
  } finally {
    validating.value = false
  }
}

function handleClose() {
  if (validating.value) return
  emit('close')
}
</script>
