<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.webLogin.title')"
    width="wide"
    @close="handleClose"
  >
    <div class="space-y-4">
      <!-- Tab 切换：Cookie 捕获（现状保留）/ 自动登录（账号密码自动登录） -->
      <div class="flex gap-2 border-b border-gray-200 pb-2 dark:border-dark-500">
        <button
          type="button"
          data-testid="web-login-tab-capture"
          class="btn btn-sm"
          :class="activeTab === 'capture' ? 'btn-primary' : 'btn-secondary'"
          @click="setActiveTab('capture')"
        >
          {{ t('admin.accounts.webLogin.tabs.capture') }}
        </button>
        <button
          type="button"
          data-testid="web-login-tab-auto"
          class="btn btn-sm"
          :class="activeTab === 'auto' ? 'btn-primary' : 'btn-secondary'"
          @click="setActiveTab('auto')"
        >
          {{ t('admin.accounts.webLogin.tabs.auto') }}
        </button>
      </div>

      <!-- Cookie 捕获（现状保留：iframe + 手动粘贴 Token 路径不变） -->
      <div v-if="activeTab === 'capture'" class="space-y-4">
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

        <!-- 新窗口打开官方登录页（兜底路径）：官方登录页普遍通过 X-Frame-Options/CSP
             禁止内嵌展示，代理 iframe 不可用时仍可新标签页登录后手动粘贴。 -->
        <div class="space-y-2">
          <button
            type="button"
            data-testid="web-login-open-new-tab"
            class="btn btn-secondary"
            @click="openLoginWindow"
          >
            {{ t('admin.accounts.webLogin.openOfficial') }}
          </button>
          <p class="input-hint">{{ t('admin.accounts.webLogin.openOfficialHint') }}</p>
        </div>

        <!-- 登录代理 iframe：后端提供代理登录页，登录态由后端自动捕获 Cookie。
             隔离 origin 已阻断官方页脚本读取后台存储，故不设 sandbox；保留 referrerpolicy 收紧。
             Kimi 不启动轮询，保持手动 Token 粘贴。 -->
        <div
          v-if="proxyUrl"
          data-testid="web-login-proxy-iframe-wrap"
          class="relative h-[420px] w-full overflow-hidden rounded-lg border border-gray-200 dark:border-dark-500"
        >
          <iframe
            :src="proxyUrl"
            class="h-full w-full"
            referrerpolicy="no-referrer"
            :title="t('admin.accounts.webLogin.proxyTitle')"
          ></iframe>
        </div>

        <!-- 代理不可用：降级为官方页登录 + 手动粘贴 -->
        <p
          v-if="!proxyUrl && proxyUnavailable"
          data-testid="web-login-proxy-unavailable"
          class="text-sm text-amber-600 dark:text-amber-400"
        >
          {{ t('admin.accounts.webLogin.proxyUnavailable') }}
        </p>

        <!-- 自动捕获状态提示 -->
        <p
          v-if="capturing"
          data-testid="web-login-autocapturing"
          class="text-sm text-primary-600 dark:text-primary-400"
        >
          {{ t('admin.accounts.webLogin.autoCapturing') }}
        </p>
        <p
          v-if="capturedCookie"
          data-testid="web-login-autofilled"
          class="text-sm text-green-600 dark:text-green-400"
        >
          {{ t('admin.accounts.webLogin.autoCaptureSuccess') }}
        </p>
        <p
          v-if="timedOut"
          data-testid="web-login-autocapture-timeout"
          class="text-sm text-amber-600 dark:text-amber-400"
        >
          {{ t('admin.accounts.webLogin.autoCaptureTimeout') }}
        </p>

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

      <!-- 自动登录（账号密码自动登录，回填 Cookie；Kimi 手动 Token 路径不变） -->
      <div v-else class="space-y-3">
        <p class="input-hint">{{ t('admin.accounts.webLogin.autoLogin.title') }}</p>
        <WebAutoLoginForm
          :platform="webPlatform"
          @recovered="handleAutoRecovered"
        />
      </div>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import BaseDialog from '@/components/common/BaseDialog.vue'
import WebAutoLoginForm from '@/components/account/WebAutoLoginForm.vue'
import { buildWebProviderCredentials, isWebProviderPlatform, webProviderUsesCookie, type WebProviderPlatform } from '@/components/account/credentialsBuilder'
import {
  validateWebCredentials,
  createWebLoginProxySession,
  deleteWebLoginProxySession
} from '@/api/admin/accounts'
import { useWebLoginCapture } from '@/composables/useWebLoginCapture'

const props = defineProps<{
  show: boolean
  platform: string
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'applied', payload: { platform: string; credentials: Record<string, unknown> }): void
}>()

const { t } = useI18n()
const appStore = useAppStore()

// 自动登录 / Cookie 捕获 双 Tab。
const activeTab = ref<'capture' | 'auto'>('capture')

// 弹窗仅从网页接入模式表单区打开；platform 为官方 CN 平台（kimi/zhipu/deepseek），
// 非网页接入平台时收敛到默认（isWebProviderPlatform 守卫），确保 webProviderUsesCookie
// 等强类型调用安全。
const webPlatform = computed<WebProviderPlatform>(() =>
  isWebProviderPlatform(props.platform) ? props.platform : 'deepseek')

const pastedCredentials = ref('')
const validating = ref(false)
const validationError = ref('')

// 登录代理会话状态。
const proxyUrl = ref('')
const proxyToken = ref('')
const proxyUnavailable = ref(false)

// 登录代理轮询：captured 后由 watch 回填并复用既有校验逻辑。
const {
  capturing,
  capturedCookie,
  timedOut,
  start: startCapture,
  stop: stopCapture
} = useWebLoginCapture()

// 新标签直连官方登录页（兜底路径，始终可用）。
const loginUrl = computed(() => {
  switch (webPlatform.value) {
    case 'deepseek':
      return 'https://chat.deepseek.com/'
    case 'zhipu':
      return 'https://chatglm.cn/'
    case 'kimi':
      return 'https://www.kimi.com/'
    default:
      return ''
  }
})

// 平台 → 翻译键映射：platform 为官方平台（deepseek），语言包键为驼峰
// （deepseek），动态拼键前需转换，否则 i18n 缺失直接显示原始 key。
const platformHint = computed(() =>
  t(`admin.accounts.webLogin.platformHint.${props.platform.replace(/-([a-z])/g, (_, c: string) => c.toUpperCase())}`))

const pasteLabel = computed(() =>
  webProviderUsesCookie(webPlatform.value)
    ? t('admin.accounts.webProviders.cookieLabel')
    : t('admin.accounts.webProviders.kimiTokenLabel'))

const pastePlaceholder = computed(() =>
  webProviderUsesCookie(webPlatform.value)
    ? t('admin.accounts.webProviders.cookiePlaceholder')
    : t('admin.accounts.webProviders.kimiTokenPlaceholder'))

// 代理会话请求代际计数：弹窗关闭/卸载与异步创建请求并发时，旧请求返回后不得
// 重新启动捕获轮询或遗留会话（旧会话立即删除，不等 TTL）。
// 声明于 watch 之前：watch immediate: true 首轮即调用 setupProxySession，
// 若计数在其后声明会触发 TDZ ReferenceError。
let proxySessionGeneration = 0

// 打开弹窗：重置表单并尝试建立登录代理会话。
// 关闭弹窗（show true→false）：清理代理会话并停止轮询——父组件仅置 show=false，
// 子组件常驻不卸载，若不清理则 session 保留到 TTL、轮询/代理槽位持续占用。
// cleanupSession 幂等（token 已清空时不重复删除），重复触发安全。
watch(() => props.show, (open) => {
  if (open) {
    activeTab.value = 'capture'
    void setupProxySession()
  } else {
    cleanupSession()
  }
}, { immediate: true })

/**
 * 建立登录代理会话（隔离 origin 代理登录页）。成功则嵌入 proxyUrl 并（非 kimi）启动
 * 捕获轮询；origin 缺失或创建失败则降级为官方页登录 + 手动粘贴（proxyUnavailable）。
 */
async function setupProxySession() {
  const generation = ++proxySessionGeneration
  pastedCredentials.value = ''
  validationError.value = ''
  validating.value = false
  proxyUrl.value = ''
  proxyToken.value = ''
  proxyUnavailable.value = false
  stopCapture()

  try {
    const session = await createWebLoginProxySession(webPlatform.value)
    if (generation !== proxySessionGeneration) {
      // 弹窗已重新打开或已关闭/卸载：本次会话作废，立即删除，不等 TTL。
      void deleteWebLoginProxySession(session.token).catch((err) => {
        console.warn('[WebLoginModal] failed to clean up stale proxy session (waits for TTL)', err)
      })
      return
    }
    // 隔离 origin：若 public settings 提供 web_login_proxy_origin（独立监听端口的独立源），
    // 官方页脚本无法读取管理端 :3300 的 auth_token/localStorage。
    // 同源回退已移除（安全红线）：proxyUrl 仅在 proxyOrigin 非空时设置；
    // origin 空/缺失视为代理不可用，走官方页新标签 + 手动粘贴降级，绝不回退主站同源路径。
    const proxyOrigin = appStore.cachedPublicSettings?.web_login_proxy_origin
    if (proxyOrigin) {
      proxyUrl.value = `${proxyOrigin}${session.url}`
      proxyToken.value = session.token
      // Kimi 无自动 Cookie 捕获（手动 Token 粘贴），仅展示代理 iframe。
      if (webPlatform.value !== 'kimi') {
        startCapture(session.token)
      }
    } else {
      // 隔离 origin 未配置：会话已创建但代理不可用 → 立即删除已创建 session，不等
      // TTL（会话占用代理槽位），降级手动粘贴（不回退同源）。
      void deleteWebLoginProxySession(session.token).catch((err) => {
        console.warn('[WebLoginModal] failed to clean up proxy session without origin (waits for TTL)', err)
      })
      proxyUnavailable.value = true
    }
  } catch {
    // 代理不可用：不阻断，降级手动粘贴。
    proxyUnavailable.value = true
  }
}

// 捕获到 Cookie 后回填并复用既有校验/应用逻辑（不直接 emit）。
watch(() => capturedCookie.value, async (cookie) => {
  if (cookie) {
    pastedCredentials.value = cookie
    await handleValidateAndApply()
  }
})

function openLoginWindow() {
  window.open(loginUrl.value, '_blank', 'noopener')
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
    // 成功关闭前清理代理会话（直接 emit('close') 不走 handleClose）：
    // 不清理则 session 保留到 TTL、轮询/代理槽位持续占用。
    cleanupSession()
    emit('close')
  } catch (err) {
    // 后端错误文案不透传凭证值；登录会话重复（同平台既有账号已持有相同登录态）
    // 用专门提示，其余统一回落通用失败提示。
    const errInfo = err as { message?: string; reason?: string }
    if (errInfo?.reason === 'web_credential_duplicate') {
      validationError.value = t('admin.accounts.webProviders.errors.webCredentialDuplicate')
      return
    }
    validationError.value = errInfo?.message || t('admin.accounts.webLogin.validationFailed')
  } finally {
    validating.value = false
  }
}

/**
 * 自动登录成功：构建与手动粘贴同构的 credentials，复用既有 applied → 回填路径
 * （handleWebLoginApplied 把 Cookie 写入父表单的 webCookieInput）。Kimi 因 needs_sms
 * 本期不会触发 recovered，由表单内如实展示未接入发码通道。
 */
function setActiveTab(tab: 'capture' | 'auto') {
  activeTab.value = tab
}

function handleAutoRecovered(payload: { platform: string; cookie: string }) {
  const usesCookie = webProviderUsesCookie(webPlatform.value)
  const built = buildWebProviderCredentials(webPlatform.value, {
    cookie: usesCookie ? payload.cookie : '',
    kimiTokenJson: usesCookie ? '' : payload.cookie,
    baseUrl: ''
  })
  if (built.error || !built.credentials) {
    return
  }
  emit('applied', { platform: props.platform, credentials: built.credentials })
  // 成功关闭前清理代理会话（幂等，auto tab 通常无活跃 session）。
  cleanupSession()
  emit('close')
}

/**
 * 关闭时 best-effort 清理代理会话（失败记录日志、由 TTL 兜底），并停止轮询。
 * 同时递增代际计数：关闭/卸载与异步创建请求并发时，挂起请求返回后走
 * stale 分支立即删除已创建 session，不重启捕获轮询、不遗留会话。
 */
function cleanupSession() {
  proxySessionGeneration++
  stopCapture()
  if (proxyToken.value) {
    void deleteWebLoginProxySession(proxyToken.value).catch((err) => {
      console.warn('[WebLoginModal] proxy session cleanup failed (waits for TTL)', err)
    })
    proxyToken.value = ''
  }
}

function handleClose() {
  if (validating.value) return
  cleanupSession()
  emit('close')
}

onUnmounted(() => cleanupSession())
</script>
