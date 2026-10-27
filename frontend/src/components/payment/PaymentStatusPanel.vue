<template>
  <div class="space-y-4">
    <!-- ═══ Terminal States: show result, user clicks to return ═══ -->

    <!-- Success -->
    <template v-if="outcome === 'success'">
      <div class="card p-6">
        <div class="flex flex-col items-center space-y-4 py-4">
          <div class="flex h-16 w-16 items-center justify-center rounded-full bg-green-100 dark:bg-green-900/30">
            <Icon name="check" size="lg" class="text-green-500" />
          </div>
          <p class="text-lg font-bold text-gray-900 dark:text-white">{{ props.orderType === 'subscription' ? t('payment.result.subscriptionSuccess') : t('payment.result.success') }}</p>
          <div v-if="paidOrder" class="w-full rounded-xl bg-gray-50 p-4 dark:bg-dark-800">
            <div class="space-y-2 text-sm">
              <div class="flex justify-between">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.orderId') }}</span>
                <span class="font-medium text-gray-900 dark:text-white">#{{ paidOrder.id }}</span>
              </div>
              <div v-if="paidOrder.out_trade_no" class="flex justify-between">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.orderNo') }}</span>
                <span class="font-medium text-gray-900 dark:text-white">{{ paidOrder.out_trade_no }}</span>
              </div>
              <div class="flex justify-between">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.amount') }}</span>
                <span class="font-medium text-gray-900 dark:text-white">
                  {{ creditedAmountSymbol }}{{ paidOrder.amount.toFixed(2) }}
                  <span v-if="(paidOrder.bonus_amount ?? 0) > 0" class="ml-1 text-xs font-normal text-amber-600 dark:text-amber-400">
                    ({{ t('payment.orders.bonusIncluded', { amount: creditedAmountSymbol + (paidOrder.bonus_amount ?? 0).toFixed(2) }) }})
                  </span>
                </span>
              </div>
              <div class="flex justify-between">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.payAmount') }}</span>
                <span class="font-medium text-gray-900 dark:text-white">{{ formatGatewayAmount(paidOrder.pay_amount, paidOrder.currency) }}</span>
              </div>
            </div>
          </div>
          <button class="btn btn-primary" @click="handleDone">{{ t('common.confirm') }}</button>
        </div>
      </div>
    </template>

    <!-- Cancelled -->
    <template v-else-if="outcome === 'cancelled'">
      <div class="card p-6">
        <div class="flex flex-col items-center space-y-4 py-4">
          <div class="flex h-16 w-16 items-center justify-center rounded-full bg-gray-100 dark:bg-dark-700">
            <svg class="h-8 w-8 text-gray-400 dark:text-gray-500" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
              <path stroke-linecap="round" stroke-linejoin="round" d="M6 18L18 6M6 6l12 12" />
            </svg>
          </div>
          <p class="text-lg font-bold text-gray-900 dark:text-white">{{ t('payment.qr.cancelled') }}</p>
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.cancelledDesc') }}</p>
          <button class="btn btn-primary" @click="handleDone">{{ t('common.confirm') }}</button>
        </div>
      </div>
    </template>

    <!-- QR image expired — NOT an order outcome: the order may still be pending
         and payable. Offer the existing re-initiate entry, no auto retry. -->
    <template v-else-if="outcome === 'qr_expired'">
      <div class="card p-6">
        <div class="flex flex-col items-center gap-4 py-4">
          <div class="flex h-16 w-16 items-center justify-center rounded-full bg-amber-100 dark:bg-amber-900/30">
            <svg class="h-8 w-8 text-amber-500" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
              <path stroke-linecap="round" stroke-linejoin="round" d="M12 9v2m0 4h.01M5.07 19h13.86c1.54 0 2.5-1.67 1.73-3L13.73 4c-.77-1.33-2.69-1.33-3.46 0L3.34 16c-.77 1.33.19 3 1.73 3z" />
            </svg>
          </div>
          <p class="text-lg font-bold text-gray-900 dark:text-white">{{ t('payment.qr.qrExpired') }}</p>
          <p class="text-center text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.qrExpiredDesc') }}</p>
          <button class="btn btn-primary" @click="handleReinitiate">{{ t('payment.qr.reinitiatePay') }}</button>
          <button class="btn btn-secondary" @click="handleDone">{{ t('common.close') }}</button>
        </div>
      </div>
    </template>

    <!-- Expired / Failed -->
    <template v-else-if="outcome === 'expired'">
      <div class="card p-6">
        <div class="flex flex-col items-center space-y-4 py-4">
          <div class="flex h-16 w-16 items-center justify-center rounded-full bg-orange-100 dark:bg-orange-900/30">
            <svg class="h-8 w-8 text-orange-500" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
              <path stroke-linecap="round" stroke-linejoin="round" d="M12 6v6h4.5m4.5 0a9 9 0 11-18 0 9 9 0 0118 0z" />
            </svg>
          </div>
          <p class="text-lg font-bold text-gray-900 dark:text-white">{{ t('payment.qr.expired') }}</p>
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.expiredDesc') }}</p>
          <button class="btn btn-primary" @click="handleDone">{{ t('common.confirm') }}</button>
        </div>
      </div>
    </template>

    <!-- ═══ Active States: QR or Popup waiting ═══ -->

    <!-- Mobile Alipay app handoff. The QR fallback stays hidden until launch timeout. -->
    <template v-else-if="isMobileAlipayDeepLink">
      <template v-if="!deepLinkFallbackVisible">
        <div class="card p-6">
          <div class="flex flex-col items-center space-y-4 py-4 text-center">
            <div
              v-if="deepLinkState === 'launching'"
              class="h-10 w-10 animate-spin rounded-full border-4 border-[#00AEEF] border-t-transparent"
            ></div>
            <div
              v-else
              class="flex h-12 w-12 items-center justify-center rounded-full bg-blue-50 dark:bg-blue-950/30"
            >
              <Icon name="checkCircle" size="lg" class="text-[#00AEEF]" />
            </div>
            <p class="text-lg font-semibold text-gray-900 dark:text-white">
              {{ deepLinkState === 'backgrounded' ? t('payment.qr.alipayContinueInApp') : t('payment.qr.alipayOpening') }}
            </p>
            <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.alipayWaitingHint') }}</p>
            <button
              v-if="deepLinkState === 'backgrounded'"
              data-test="reopen-alipay"
              class="btn btn-alipay inline-flex items-center gap-2 text-sm"
              @click="reopenAlipay"
            >
              <Icon name="externalLink" size="sm" />
              {{ t('payment.qr.reopenAlipay') }}
            </button>
          </div>
        </div>
        <div class="card p-4 text-center">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.expiresIn') }}</p>
          <p class="mt-1 text-2xl font-bold tabular-nums text-gray-900 dark:text-white">{{ countdownDisplay }}</p>
          <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('payment.qr.waitingPayment') }}</p>
        </div>
      </template>
      <template v-else>
        <div data-test="alipay-qr-fallback" class="card p-6">
          <div class="flex flex-col items-center space-y-4">
            <div class="text-center">
              <p class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('payment.qr.alipayFallbackTitle') }}</p>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.alipayFallbackHint') }}</p>
            </div>
            <div class="w-full space-y-2 border-y border-gray-100 py-3 text-sm dark:border-dark-600">
              <div class="flex items-start justify-between gap-4">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.payAmount') }}</span>
                <span class="font-semibold text-gray-900 dark:text-white">{{ displayPaymentAmount }}</span>
              </div>
              <div class="flex items-start justify-between gap-4">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.orderNo') }}</span>
                <span class="max-w-[70%] break-all text-right font-mono text-xs text-gray-900 dark:text-white">
                  {{ displayOrderNumber }}
                </span>
              </div>
              <div class="flex items-start justify-between gap-4">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.qr.expiresIn') }}</span>
                <span class="font-semibold tabular-nums text-gray-900 dark:text-white">{{ countdownDisplay }}</span>
              </div>
            </div>
            <div :class="['relative rounded-lg border-2 p-4', qrBorderClass]">
              <canvas ref="qrCanvas" class="mx-auto"></canvas>
              <div class="pointer-events-none absolute inset-0 flex items-center justify-center">
                <span :class="['rounded-full p-2 shadow ring-2 ring-white', qrLogoBgClass]">
                  <img :src="qrLogoIcon" alt="" class="h-5 w-5 brightness-0 invert" />
                </span>
              </div>
            </div>
            <p class="text-center text-sm leading-6 text-gray-600 dark:text-gray-300">
              {{ t('payment.qr.alipaySaveAndScanHint') }}
            </p>
            <div class="grid w-full gap-2 sm:grid-cols-2">
              <button
                data-test="reopen-alipay"
                class="btn btn-alipay inline-flex items-center justify-center gap-2"
                @click="reopenAlipay"
              >
                <Icon name="externalLink" size="sm" />
                {{ t('payment.qr.reopenAlipay') }}
              </button>
              <button
                data-test="save-alipay-qr"
                class="btn btn-secondary inline-flex items-center justify-center gap-2"
                @click="saveQRCode"
              >
                <Icon name="download" size="sm" />
                {{ t('payment.qr.saveQRCode') }}
              </button>
            </div>
            <button class="btn btn-secondary w-full" @click="handleDone">
              {{ t('payment.result.backToRecharge') }}
            </button>
          </div>
        </div>
      </template>
    </template>

    <!-- QR Code Mode -->
    <template v-else-if="showQRCode">
      <!-- 微信内提示：微信支付不允许「从相册识别二维码完成支付」，虎皮椒当前通道
           也不支持微信内调起支付。所以在微信内照着二维码操作必然失败，与其让用户
           撞一次微信的安全弹窗，不如直接说明并引导到能完成的浏览器。
           注意：这里不说「不支持微信内支付」，只说相册扫码这一条路走不通。 -->
      <div
        v-if="showWeChatBrowserNotice"
        class="card border-amber-300 bg-amber-50 p-4 dark:border-amber-700 dark:bg-amber-950/30"
        data-test="wechat-browser-notice"
      >
        <p class="text-sm font-medium text-amber-900 dark:text-amber-200">
          {{ t('payment.qr.wechatBrowserTitle') }}
        </p>
        <p class="mt-1 text-sm text-amber-800 dark:text-amber-300">
          {{ t('payment.qr.wechatBrowserHint') }}
        </p>
        <button class="btn btn-secondary mt-3 w-full" @click="handleDone">
          {{ t('payment.qr.backToCheckout') }}
        </button>
      </div>
      <div class="card p-6">
        <div class="flex flex-col items-center space-y-4">
          <p class="text-lg font-semibold text-gray-900 dark:text-white">{{ scanTitle }}</p>
          <div :class="['relative rounded-lg border-2 p-4', qrBorderClass]">
            <!-- xunhupay 微信：qr_code 是虎皮椒生成的二维码图片 URL（302→PNG），直接展示为图片。
                 官方微信 Native / easypay / alipay：qr_code 是支付串（weixin://、alipays://、opaque token），
                 需经 QRCode.toCanvas 编码成二维码。 -->
            <img
              v-if="isDirectImageQR"
              :src="qrUrl"
              alt="微信扫码支付"
              class="mx-auto"
              style="width: 220px; height: 220px; object-fit: contain"
              data-test="direct-qr-img"
            />
            <canvas v-else ref="qrCanvas" class="mx-auto"></canvas>
            <!-- Brand logo overlay -->
            <div class="pointer-events-none absolute inset-0 flex items-center justify-center">
              <span :class="['rounded-full p-2 shadow ring-2 ring-white', qrLogoBgClass]">
                <img :src="qrLogoIcon" alt="" class="h-5 w-5 brightness-0 invert" />
              </span>
            </div>
          </div>
          <p v-if="scanHint" class="text-center text-sm text-gray-500 dark:text-gray-400">{{ scanHint }}</p>
        </div>
      </div>
      <div class="card p-4 text-center">
        <p class="text-sm text-gray-500 dark:text-gray-400">{{ usesUpstreamQrImage ? t('payment.qr.qrExpiresIn') : t('payment.qr.expiresIn') }}</p>
        <p class="mt-1 text-2xl font-bold tabular-nums text-gray-900 dark:text-white">{{ countdownDisplay }}</p>
        <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('payment.qr.waitingPayment') }}</p>
      </div>
      <button class="btn btn-secondary w-full" :disabled="cancelling" @click="handleCancel">
        {{ cancelling ? t('common.processing') : t('payment.qr.cancelOrder') }}
      </button>
    </template>

    <!-- Waiting for Popup/Redirect Mode -->
    <template v-else>
      <div class="card p-6">
        <div class="flex flex-col items-center space-y-4 py-4">
          <div class="h-10 w-10 animate-spin rounded-full border-4 border-primary-500 border-t-transparent"></div>
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.payInNewWindowHint') }}</p>
          <button v-if="payUrl" class="btn btn-secondary text-sm" @click="reopenPopup">
            {{ t('payment.qr.openPayWindow') }}
          </button>
        </div>
      </div>
      <div class="card p-4 text-center">
        <p class="mt-1 text-2xl font-bold tabular-nums text-gray-900 dark:text-white">{{ countdownDisplay }}</p>
        <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('payment.qr.waitingPayment') }}</p>
      </div>
      <button class="btn btn-secondary w-full" :disabled="cancelling" @click="handleCancel">
        {{ cancelling ? t('common.processing') : t('payment.qr.cancelOrder') }}
      </button>
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { usePaymentStore } from '@/stores/payment'
import { useAppStore } from '@/stores'
import { paymentAPI } from '@/api/payment'
import { extractI18nErrorMessage } from '@/utils/apiError'
import { isWeChatBrowser } from '@/utils/device'
import { getPaymentPopupFeatures, isBuiltInAlipayMethod, isBuiltInWxpayMethod } from '@/components/payment/providerConfig'
import { ACCOUNT_CURRENCY, currencySymbol, formatPaymentAmount, normalizePaymentCurrency } from '@/utils/money'
import type { PaymentOrder } from '@/types/payment'
import Icon from '@/components/icons/Icon.vue'
import QRCode from 'qrcode'
import alipayIcon from '@/assets/icons/alipay.svg'
import wxpayIcon from '@/assets/icons/wxpay.svg'
import paymentIcon from '@/assets/icons/payment.svg'
import {
  createAlipayDeepLinkLauncher,
  type AlipayDeepLinkLauncher,
  type AlipayDeepLinkState,
} from './alipayDeepLink'

const props = defineProps<{
  orderId: number
  amount?: number
  payAmount?: number
  qrCode: string
  expiresAt: string
  paymentType: string
  payUrl?: string
  orderType?: string
  currency?: string
  outTradeNo?: string
  mobileAlipayDeepLink?: boolean
  /** Order creation time in ms — the evidence-backed issue time of an
   * upstream-issued QR image. Only consulted for explicitly QR-configured
   * channels; ignored otherwise so other channels keep their order timeout. */
  qrIssuedAt?: number
  paymentMode?: string
  providerKey?: string
}>()

type PaymentOutcome = 'success' | 'cancelled' | 'expired' | 'qr_expired'

const emit = defineEmits<{ done: []; success: []; settled: [outcome: PaymentOutcome] }>()

const i18n = useI18n()
const { t } = i18n
const paymentStore = usePaymentStore()
const appStore = useAppStore()

const qrCanvas = ref<HTMLCanvasElement | null>(null)
const qrUrl = ref('')
const remainingSeconds = ref(0)
const cancelling = ref(false)
const paidOrder = ref<PaymentOrder | null>(null)
const deepLinkState = ref<AlipayDeepLinkState>('idle')
const deepLinkFallbackVisible = ref(false)
const paymentCurrency = computed(() => normalizePaymentCurrency(props.currency))
const creditedAmountSymbol = currencySymbol(ACCOUNT_CURRENCY)
const localeCode = computed(() => {
  const raw = i18n.locale as unknown
  if (typeof raw === 'string') return raw
  if (raw && typeof raw === 'object' && 'value' in raw) {
    return String((raw as { value?: string }).value || '')
  }
  return undefined
})

// Terminal outcome: null = still active, 'success' | 'cancelled' | 'expired' | 'qr_expired'
const outcome = ref<PaymentOutcome | null>(null)

// 虎皮椒官方文档：url_qrcode 二维码有效期五分钟（"二维码有效期五分钟，PC端可将该参数
// 展示出来进行扫码支付，不用再转二维码"）。该五分钟是【二维码本身】的时效，与网站
// 订单超时（ORDER_TIMEOUT_MINUTES，默认 30 分钟）不是一个时钟，不能互相替代。
// 仅当该通道被显式配置为二维码模式时才启用，因为只有此时本站才把上游签发的
// 二维码原图交给用户；其它通道的 qrCode 由本站/上游按各自契约生成，沿用订单超时。
const XUNHUPAY_QR_TTL_MS = 5 * 60 * 1000
const normalizedPaymentMode = computed(() => (props.paymentMode || '').trim().toLowerCase())
const usesUpstreamQrImage = computed(() =>
  normalizedPaymentMode.value === 'qrcode' || normalizedPaymentMode.value === 'native')

// 微信内浏览器提示：仅对微信扫码类通道展示，且只在微信内展示。
// 支付宝在微信内打开也能正常唤起，故排除。
const inWeChatBrowser = ref(false)
const showWeChatBrowserNotice = computed(() =>
  inWeChatBrowser.value && isWxpay.value && !isAlipay.value)

let pollTimer: ReturnType<typeof setInterval> | null = null
let countdownTimer: ReturnType<typeof setInterval> | null = null
let verifyAttempts = 0
let lastVerifyAt = 0
let alipayLauncher: AlipayDeepLinkLauncher | null = null

const VERIFY_RETRY_INTERVAL_MS = 15000
const VERIFY_RETRY_MAX_ATTEMPTS = 6

const isAlipay = computed(() => isBuiltInAlipayMethod(props.paymentType))
const isWxpay = computed(() => isBuiltInWxpayMethod(props.paymentType))
// xunhupay 的 qr_code 是虎皮椒收银台/二维码页 URL（实测 content-type 为 text/html 网页，
// 内嵌或重定向到二维码图，并非直接返回 PNG 字节流）。它只能由原样 <img> 直接展示，
// 由浏览器自行加载该页内的二维码图——绝不能经 QRCode.toCanvas 二次编码，否则用户扫出的
// 是"打开虎皮椒网页"的死路。此判定与通道的 payment_mode 配置无关：无论何种模式，
// 只要 provider 是 xunhupay 且 qr_code 为 https 链接，一律按 <img> 原图直出。
// 仅凭 HTTPS 仍不能当作通用二维码图片：easypay/alipay 的支付串也可能是 https 链接，
// 当成 <img> 会渲染成裂图且扫不出来，必须仍走 QRCode.toCanvas 编码。
const isDirectImageQR = computed(() =>
  props.providerKey === 'xunhupay'
  && /^https?:\/\//i.test(qrUrl.value))
const isMobileAlipayDeepLink = computed(() => props.mobileAlipayDeepLink === true && isAlipay.value && !!qrUrl.value)
const showQRCode = computed(() => !!qrUrl.value && (!isMobileAlipayDeepLink.value || deepLinkFallbackVisible.value))

const qrBorderClass = computed(() => {
  if (isAlipay.value) return 'border-[#00AEEF] bg-blue-50 dark:border-[#00AEEF]/70 dark:bg-blue-950/20'
  if (isWxpay.value) return 'border-[#2BB741] bg-green-50 dark:border-[#2BB741]/70 dark:bg-green-950/20'
  return 'border-gray-200 bg-white dark:border-dark-600 dark:bg-dark-800'
})

const qrLogoBgClass = computed(() => {
  if (isAlipay.value) return 'bg-[#00AEEF]'
  if (isWxpay.value) return 'bg-[#2BB741]'
  return 'bg-gray-400'
})

const qrLogoIcon = computed(() => {
  if (isAlipay.value) return alipayIcon
  if (isWxpay.value) return wxpayIcon
  return paymentIcon
})

const scanTitle = computed(() => {
  if (isAlipay.value) return t('payment.qr.scanAlipay')
  if (isWxpay.value) return t('payment.qr.scanWxpay')
  return t('payment.qr.scanToPay')
})

const scanHint = computed(() => {
  if (isAlipay.value) return t('payment.qr.scanAlipayHint')
  // 微信内不能从相册识别支付二维码（微信支付统一限制），这时再提示"从相册选择"
  // 只会让用户撞一次必然失败的弹窗。
  if (showWeChatBrowserNotice.value) return t('payment.qr.scanWxpayHint')
  // 虎皮椒当前通道不支持微信内调起支付，只能扫码收款，因此明确引导用户
  // 截图保存 → 打开微信扫一扫 → 从相册选择，而不是提示"打开微信扫描"。
  if (usesUpstreamQrImage.value) return t('payment.qr.scanWxpayAlbumHint')
  if (isWxpay.value) return t('payment.qr.scanWxpayHint')
  return ''
})

const countdownDisplay = computed(() => {
  const m = Math.floor(remainingSeconds.value / 60)
  const s = remainingSeconds.value % 60
  return m.toString().padStart(2, '0') + ':' + s.toString().padStart(2, '0')
})

const displayPaymentAmount = computed(() => formatGatewayAmount(props.payAmount || props.amount || 0))
const displayOrderNumber = computed(() => props.outTradeNo || `#${props.orderId}`)

function formatGatewayAmount(value: number, currency?: string | null): string {
  return formatPaymentAmount(value, currency || paymentCurrency.value, localeCode.value)
}

function isSuccessStatus(status: string | null | undefined): boolean {
  return status === 'COMPLETED' || status === 'PAID' || status === 'RECHARGING'
}

function reopenPopup() {
  if (props.payUrl) {
    const win = window.open(props.payUrl, 'paymentPopup', getPaymentPopupFeatures())
    if (!win || win.closed) {
      window.location.href = props.payUrl
    }
  }
}

function setOutcome(next: PaymentOutcome) {
  if (outcome.value === next) return
  outcome.value = next
  emit('settled', next)
}

async function renderQR() {
  await nextTick()
  if (!showQRCode.value || !qrCanvas.value || !qrUrl.value) return
  if (isDirectImageQR.value) return
  await QRCode.toCanvas(qrCanvas.value, qrUrl.value, {
    width: 220, margin: 2,
    errorCorrectionLevel: 'M',
  })
}

function updateDeepLinkState(state: AlipayDeepLinkState) {
  deepLinkState.value = state
  if (state === 'fallback') {
    deepLinkFallbackVisible.value = true
    renderQR()
  } else if (state === 'backgrounded') {
    deepLinkFallbackVisible.value = false
  }
}

function reopenAlipay() {
  alipayLauncher?.launch()
}

function saveQRCode() {
  const canvas = qrCanvas.value
  if (!canvas) return
  const link = document.createElement('a')
  link.href = canvas.toDataURL('image/png')
  link.download = `alipay-${props.outTradeNo || props.orderId}.png`
  document.body.appendChild(link)
  link.click()
  link.remove()
}

async function tryRecoverPendingOrder(order: PaymentOrder): Promise<PaymentOrder> {
  if (!isWxpay.value && !isAlipay.value) return order
  const outTradeNo = String(order.out_trade_no || '').trim()
  if (!outTradeNo) return order
  const normalizedStatus = String(order.status || '').trim().toUpperCase()
  if (normalizedStatus !== 'PENDING') return order
  const now = Date.now()
  if (verifyAttempts >= VERIFY_RETRY_MAX_ATTEMPTS || now - lastVerifyAt < VERIFY_RETRY_INTERVAL_MS) {
    return order
  }

  lastVerifyAt = now
  verifyAttempts += 1
  try {
    const result = await paymentAPI.verifyOrder(outTradeNo)
    return result.data ?? order
  } catch {
    return order
  }
}

let pollInFlight = false
async function pollStatus() {
  if (!props.orderId || outcome.value) return
  // 防重入：接口（含 verifyOrder 二次确认）响应慢于 3 秒轮询间隔时避免并发重叠请求。
  if (pollInFlight) return
  pollInFlight = true
  try {
    let order = await paymentStore.pollOrderStatus(props.orderId)
    if (!order) return
    // 已进入终态则不再处理迟到的响应。
    if (outcome.value) return
    order = await tryRecoverPendingOrder(order)
    if (outcome.value) return
    if (isSuccessStatus(order.status)) {
      cleanup()
      paidOrder.value = order
      setOutcome('success')
      emit('success')
    } else if (order.status === 'CANCELLED') {
      cleanup()
      setOutcome('cancelled')
    } else if (order.status === 'EXPIRED' || order.status === 'FAILED') {
      cleanup()
      setOutcome('expired')
    }
  } finally {
    pollInFlight = false
  }
}

function startCountdown(seconds: number) {
  remainingSeconds.value = Math.max(0, seconds)
  // 以绝对截止时刻计时（避免标签页后台定时器节流导致的倒计时漂移）；
  // 过期态按是否走上游二维码图区分 qr_expired / expired（保留本仓库语义）。
  const deadline = Date.now() + remainingSeconds.value * 1000
  if (remainingSeconds.value <= 0) { setQrOutcome(); return }
  countdownTimer = setInterval(() => {
    remainingSeconds.value = Math.max(0, Math.floor((deadline - Date.now()) / 1000))
    if (remainingSeconds.value <= 0) { setQrOutcome(); cleanup() }
  }, 1000)
}

// A QR image expiring is NOT an order outcome. The order may still be payable
// through the remaining order window, and expiry says nothing about whether
// money arrived. Report it as its own state so the UI can offer the existing
// re-initiate entry instead of claiming cancellation or payment failure.
function setQrOutcome() {
  setOutcome(usesUpstreamQrImage.value ? 'qr_expired' : 'expired')
}

async function handleCancel() {
  if (!props.orderId || cancelling.value) return
  cancelling.value = true
  try {
    await paymentAPI.cancelOrder(props.orderId)
    cleanup()
    setOutcome('cancelled')
  } catch (err: unknown) {
    appStore.showError(extractI18nErrorMessage(err, t, 'payment.errors', t('common.error')))
  } finally {
    cancelling.value = false
  }
}

function handleDone() { cleanup(); emit('done') }

// QR image expired: hand control back to the existing checkout entry so the
// user explicitly re-initiates. No automatic retry, no automatic new order.
function handleReinitiate() { cleanup(); emit('done') }

function cleanup() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null }
  if (countdownTimer) { clearInterval(countdownTimer); countdownTimer = null }
  alipayLauncher?.dispose()
  alipayLauncher = null
}

// Initialize on mount
qrUrl.value = props.qrCode
inWeChatBrowser.value = isWeChatBrowser()
verifyAttempts = 0
lastVerifyAt = 0
let seconds = 30 * 60
if (props.expiresAt) {
  seconds = Math.floor((new Date(props.expiresAt).getTime() - Date.now()) / 1000)
}
// For an upstream-issued QR image, remaining time must be measured from the
// moment the QR was issued (order creation), not from when this page mounted.
// Recomputing "now + 5min" on every load would hand the user a fresh five
// minutes on every refresh; anchoring to the issue time makes refresh and
// page-restore converge on the same deadline.
if (usesUpstreamQrImage.value) {
  const issuedAt = Number(props.qrIssuedAt)
  if (Number.isFinite(issuedAt) && issuedAt > 0) {
    seconds = Math.floor((issuedAt + XUNHUPAY_QR_TTL_MS - Date.now()) / 1000)
  }
}
startCountdown(seconds)
pollTimer = setInterval(pollStatus, 3000)
renderQR()

watch([() => qrUrl.value, showQRCode], () => renderQR())
onMounted(() => {
  if (!isMobileAlipayDeepLink.value) return
  alipayLauncher = createAlipayDeepLinkLauncher({
    qrCode: qrUrl.value,
    document,
    lifecycleTarget: window,
    userAgent: window.navigator.userAgent,
    assignLocation: (url) => window.location.assign(url),
    onStateChange: updateDeepLinkState,
  })
  alipayLauncher.launch()
})
onUnmounted(() => cleanup())
</script>
