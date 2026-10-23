import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

const pollOrderStatus = vi.hoisted(() => vi.fn())
const cancelOrder = vi.hoisted(() => vi.fn())
const verifyOrder = vi.hoisted(() => vi.fn())
const showError = vi.hoisted(() => vi.fn())
const toCanvas = vi.hoisted(() => vi.fn())

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

vi.mock('@/stores/payment', () => ({
  usePaymentStore: () => ({
    pollOrderStatus,
  }),
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showError,
  }),
}))

vi.mock('@/api/payment', () => ({
  paymentAPI: {
    cancelOrder,
    verifyOrder,
  },
}))

vi.mock('qrcode', () => ({
  default: {
    toCanvas,
  },
}))

import PaymentStatusPanel from '../PaymentStatusPanel.vue'

const orderFactory = (status: string) => ({
  id: 42,
  user_id: 9,
  amount: 88,
  pay_amount: 88,
  fee_rate: 0,
  payment_type: 'alipay',
  out_trade_no: 'sub2_20260420abcd1234',
  status,
  order_type: 'balance',
  created_at: '2026-04-20T12:00:00Z',
  expires_at: '2099-01-01T12:30:00Z',
  refund_amount: 0,
})

describe('PaymentStatusPanel', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    pollOrderStatus.mockReset()
    cancelOrder.mockReset()
    verifyOrder.mockReset()
    showError.mockReset()
    toCanvas.mockReset().mockResolvedValue(undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('treats RECHARGING as a successful terminal state', async () => {
    pollOrderStatus.mockResolvedValue(orderFactory('RECHARGING'))

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        qrCode: 'https://pay.example.com/qr/42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
      },
      global: {
        stubs: {
          Icon: true,
        },
      },
    })

    await flushPromises()
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(pollOrderStatus).toHaveBeenCalledWith(42)
    expect(wrapper.text()).toContain('payment.result.success')
    expect(wrapper.emitted('success')).toHaveLength(1)
  })

  it('does not show the reopen-pay-window button in QR mode', async () => {
    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        qrCode: 'https://pay.example.com/qr/42',
        payUrl: 'https://pay.example.com/session/42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
      },
      global: {
        stubs: {
          Icon: true,
        },
      },
    })

    await flushPromises()
    // QR 模式下不再展示"打开支付窗口"按钮：xunhupay 的 url 与 url_qrcode
    // 禁止两段式使用（官方文档明确"不能先显示 url_qrcode 二维码再跳转 url"），
    // 二维码模式下只保留扫码路径。
    expect(wrapper.text()).not.toContain('payment.qr.openPayWindow')
  })

  it('uses generic QR copy for custom methods that contain built-in names', async () => {
    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        qrCode: 'https://pay.example.com/qr/42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'card_alipay',
        orderType: 'balance',
      },
      global: {
        stubs: {
          Icon: true,
        },
      },
    })

    await flushPromises()

    expect(wrapper.text()).toContain('payment.qr.scanToPay')
    expect(wrapper.text()).not.toContain('payment.qr.scanAlipay')
  })

  // xunhupay 的 qr_code 是虎皮椒直接返回的二维码图片 URL（https://... 302→PNG），
  // 官方文档明确"不用再转二维码"，应直接作为 <img> src 展示，不应经 QRCode.toCanvas
  // 二次编码（会产生多余的中间层二维码）。
  it('renders xunhupay https qr_code as <img> directly (not via QRCode.toCanvas)', async () => {
    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 58,
        qrCode: 'https://api.xunhupay.com/payments/wechat/qrcode_v3?id=20307171935&nonce_str=abc&time=123&appid=201906187599&hash=def',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'wxpay',
        orderType: 'balance',
        paymentMode: 'qrcode',
        providerKey: 'xunhupay',
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()

    // 直接 <img> 展示，不走 QRCode.toCanvas 重编码
    expect(toCanvas).not.toHaveBeenCalled()
    const img = wrapper.find('[data-test="direct-qr-img"]')
    expect(img.exists()).toBe(true)
    expect(img.attributes('src')).toContain('qrcode_v3')
  })

  // 原图直出与 payment_mode 配置无关：即便通道未配置二维码模式（paymentMode 为空），
  // 只要 provider 是 xunhupay 且 qr_code 为 https 链接，仍需按 <img> 原样展示，
  // 禁止 QRCode.toCanvas 二次编码（否则 PC 端会复发"打开虎皮椒网页"死路）。
  it('renders xunhupay https qr_code as <img> directly even when paymentMode is empty', async () => {
    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 591,
        qrCode: 'https://api.xunhupay.com/payments/wechat/qrcode_v3?id=20307171935&nonce_str=abc&time=123&appid=201906187599&hash=def',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'wxpay',
        orderType: 'balance',
        providerKey: 'xunhupay',
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()

    // 配置为空时仍按 <img> 直出，不走 QRCode.toCanvas 重编码
    expect(toCanvas).not.toHaveBeenCalled()
    const img = wrapper.find('[data-test="direct-qr-img"]')
    expect(img.exists()).toBe(true)
    expect(img.attributes('src')).toContain('qrcode_v3')
  })

  // 仅凭 https 不能当作二维码图片：easypay/alipay 的支付串也可能是 https 链接，
  // 当成 <img> 会渲染成裂图且扫不出来，必须仍走 QRCode.toCanvas 编码。
  it('does not treat an https payment string from another channel as a QR image', async () => {
    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 59,
        qrCode: 'https://pay.example.com/pay/abc123',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'wxpay',
        orderType: 'balance',
        paymentMode: 'qrcode',
        providerKey: 'easypay',
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()

    expect(wrapper.find('[data-test="direct-qr-img"]').exists()).toBe(false)
    expect(toCanvas).toHaveBeenCalledWith(
      expect.anything(),
      'https://pay.example.com/pay/abc123',
      expect.anything(),
    )
  })

  // 未配置二维码模式的通道沿用订单超时倒计时，不套用上游二维码 5 分钟时效
  it('keeps the order-timeout countdown for channels not configured as QR mode', async () => {
    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 60,
        qrCode: 'https://api.xunhupay.com/payments/wechat/qrcode_v3?id=1',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'wxpay',
        orderType: 'balance',
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()

    expect(wrapper.text()).not.toContain('payment.qr.qrExpiresIn')
    expect(wrapper.find('[data-test="direct-qr-img"]').exists()).toBe(false)
  })

  // 二维码倒计时按签发时刻 +5 分钟计算，刷新/恢复不会重新获得 5 分钟
  it('counts the QR validity down from the issuance time, not from page mount', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-20T12:00:00Z'))
    const issuedAt = new Date('2026-10-20T12:00:00Z').getTime()

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 61,
        qrCode: 'https://api.xunhupay.com/payments/wechat/qrcode_v3?id=1',
        expiresAt: '2026-10-20T12:30:00Z',
        paymentType: 'wxpay',
        orderType: 'balance',
        paymentMode: 'qrcode',
        providerKey: 'xunhupay',
        qrIssuedAt: issuedAt,
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()
    expect(wrapper.text()).toContain('05:00')

    // 用户离开 3 分钟后再打开同一笔订单的付款页：剩余应为 2 分钟，不能回到 5 分钟
    vi.setSystemTime(new Date('2026-10-20T12:03:00Z'))
    const reopened = mount(PaymentStatusPanel, {
      props: {
        orderId: 61,
        qrCode: 'https://api.xunhupay.com/payments/wechat/qrcode_v3?id=1',
        expiresAt: '2026-10-20T12:30:00Z',
        paymentType: 'wxpay',
        orderType: 'balance',
        paymentMode: 'qrcode',
        providerKey: 'xunhupay',
        qrIssuedAt: issuedAt,
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()
    expect(reopened.text()).toContain('02:00')

    vi.useRealTimers()
  })

  // 二维码到期是二维码事件，不得等同于订单取消或付款失败
  it('reports an expired QR as its own state, not as a cancelled or failed order', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-20T12:00:00Z'))

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 62,
        qrCode: 'https://api.xunhupay.com/payments/wechat/qrcode_v3?id=1',
        expiresAt: '2026-10-20T12:30:00Z',
        paymentType: 'wxpay',
        orderType: 'balance',
        paymentMode: 'qrcode',
        providerKey: 'xunhupay',
        qrIssuedAt: new Date('2026-10-20T12:00:00Z').getTime(),
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()
    // 签发后 5 分钟，二维码到期；订单超时是 30 分钟，订单此时并未过期
    vi.advanceTimersByTime(301 * 1000)
    await flushPromises()

    const text = wrapper.text()
    expect(text).toContain('payment.qr.qrExpired')
    expect(text).not.toContain('payment.qr.cancelled')
    expect(text).not.toContain('payment.qr.expired')
    // 复用既有重新发起入口，不自动重试
    expect(text).toContain('payment.qr.reinitiatePay')

    vi.useRealTimers()
  })

  it('actively verifies a stuck pending order and settles it when upstream confirms payment', async () => {
    pollOrderStatus.mockResolvedValue(orderFactory('PENDING'))
    verifyOrder.mockResolvedValue({
      data: orderFactory('COMPLETED'),
    })

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        qrCode: 'https://pay.example.com/qr/42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'wxpay',
        orderType: 'balance',
      },
      global: {
        stubs: {
          Icon: true,
        },
      },
    })

    await flushPromises()
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(pollOrderStatus).toHaveBeenCalledWith(42)
    expect(verifyOrder).toHaveBeenCalledWith('sub2_20260420abcd1234')
    expect(wrapper.text()).toContain('payment.result.success')
    expect(wrapper.emitted('success')).toHaveLength(1)
  })

  it('actively verifies a pending mobile Alipay precreate order', async () => {
    const originalLocation = window.location
    const originalHidden = Object.getOwnPropertyDescriptor(document, 'hidden')
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { assign: vi.fn() },
    })
    Object.defineProperty(document, 'hidden', {
      configurable: true,
      get: () => false,
    })
    pollOrderStatus.mockResolvedValue(orderFactory('PENDING'))
    verifyOrder.mockResolvedValue({ data: orderFactory('COMPLETED') })

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        amount: 88,
        payAmount: 88,
        qrCode: 'https://qr.alipay.com/dynamic-order-42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
        outTradeNo: 'sub2_20260420abcd1234',
        mobileAlipayDeepLink: true,
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(verifyOrder).toHaveBeenCalledWith('sub2_20260420abcd1234')
    expect(wrapper.emitted('success')).toHaveLength(1)

    wrapper.unmount()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
    if (originalHidden) Object.defineProperty(document, 'hidden', originalHidden)
  })

  it('actively verifies a pending desktop Alipay order', async () => {
    pollOrderStatus.mockResolvedValue(orderFactory('PENDING'))
    verifyOrder.mockResolvedValue({ data: orderFactory('COMPLETED') })

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        amount: 88,
        payAmount: 88,
        qrCode: 'https://qr.alipay.com/desktop-order-42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
        outTradeNo: 'sub2_20260420abcd1234',
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(verifyOrder).toHaveBeenCalledWith('sub2_20260420abcd1234')
    expect(wrapper.emitted('success')).toHaveLength(1)

    wrapper.unmount()
  })

  it('keeps the QR fallback hidden until the Alipay app launch times out', async () => {
    const originalLocation = window.location
    const originalHidden = Object.getOwnPropertyDescriptor(document, 'hidden')
    const assign = vi.fn()
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { assign },
    })
    Object.defineProperty(document, 'hidden', {
      configurable: true,
      get: () => false,
    })

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        amount: 88,
        payAmount: 88,
        qrCode: 'https://qr.alipay.com/dynamic-order-42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
        outTradeNo: 'sub2_20260420abcd1234',
        mobileAlipayDeepLink: true,
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()
    expect(assign).toHaveBeenCalledWith(expect.stringContaining('alipays://platformapi/startapp?saId=10000007&qrcode='))
    expect(wrapper.find('[data-test="alipay-qr-fallback"]').exists()).toBe(false)

    await vi.advanceTimersByTimeAsync(2200)
    await flushPromises()

    expect(wrapper.find('[data-test="alipay-qr-fallback"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('payment.qr.saveQRCode')
    expect(wrapper.text()).toContain('sub2_20260420abcd1234')
    expect(toCanvas).toHaveBeenCalledWith(expect.any(HTMLCanvasElement), 'https://qr.alipay.com/dynamic-order-42', expect.any(Object))

    wrapper.unmount()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
    if (originalHidden) Object.defineProperty(document, 'hidden', originalHidden)
  })

  it('does not show the QR fallback after the page enters the background', async () => {
    const originalLocation = window.location
    const originalHidden = Object.getOwnPropertyDescriptor(document, 'hidden')
    let hidden = false
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { assign: vi.fn() },
    })
    Object.defineProperty(document, 'hidden', {
      configurable: true,
      get: () => hidden,
    })

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        amount: 88,
        payAmount: 88,
        qrCode: 'https://qr.alipay.com/dynamic-order-42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
        outTradeNo: 'sub2_20260420abcd1234',
        mobileAlipayDeepLink: true,
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()
    hidden = true
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(2200)
    await flushPromises()

    expect(wrapper.find('[data-test="alipay-qr-fallback"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('payment.qr.alipayContinueInApp')

    wrapper.unmount()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
    if (originalHidden) Object.defineProperty(document, 'hidden', originalHidden)
  })
})

describe('PaymentStatusPanel WeChat in-app browser notice', () => {
  const originalUA = window.navigator.userAgent
  const setUA = (value: string) => {
    Object.defineProperty(window.navigator, 'userAgent', {
      value,
      configurable: true,
    })
  }
  const qrProps = {
    qrCode: 'https://api.xunhupay.com/payments/wechat/qrcode_v3?id=1',
    expiresAt: '2099-01-01T12:30:00Z',
    paymentType: 'wxpay',
    orderType: 'balance',
    paymentMode: 'qrcode',
    providerKey: 'xunhupay',
  }
  const mountPanel = () => mount(PaymentStatusPanel, {
    props: { orderId: 70, ...qrProps },
    global: { stubs: { Icon: true } },
  })

  afterEach(() => setUA(originalUA))

  // 微信支付不允许「从相册识别二维码完成支付」。在微信内继续提示"从相册选择"
  // 只会让用户撞一次必然失败的弹窗，必须改为提示在浏览器打开。
  it('warns and drops the album-scan instruction inside the WeChat browser', async () => {
    setUA('Mozilla/5.0 (iPhone) MicroMessenger/8.0.49')

    const wrapper = mountPanel()
    await flushPromises()

    const notice = wrapper.find('[data-test="wechat-browser-notice"]')
    expect(notice.exists()).toBe(true)
    expect(wrapper.text()).toContain('payment.qr.wechatBrowserTitle')
    // 不再出现会把用户带进死路的相册指引
    expect(wrapper.text()).not.toContain('payment.qr.scanWxpayAlbumHint')
  })

  it('shows no notice in a normal mobile browser', async () => {
    setUA('Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Safari/605.1')

    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.find('[data-test="wechat-browser-notice"]').exists()).toBe(false)
    // 非微信内仍保留原有的相册扫码指引
    expect(wrapper.text()).toContain('payment.qr.scanWxpayAlbumHint')
  })

  // 支付宝在微信内也能正常唤起支付，不应展示该提示
  it('does not show the notice for alipay inside the WeChat browser', async () => {
    setUA('Mozilla/5.0 (iPhone) MicroMessenger/8.0.49')

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 71,
        qrCode: 'https://openapi.alipay.com/gateway.do?x=1',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
        paymentMode: 'qrcode',
        providerKey: 'alipay',
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()

    expect(wrapper.find('[data-test="wechat-browser-notice"]').exists()).toBe(false)
  })
})
