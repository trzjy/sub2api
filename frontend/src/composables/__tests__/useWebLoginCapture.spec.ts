import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { useWebLoginCapture } from '../useWebLoginCapture'

const getCaptureMock = vi.hoisted(() => vi.fn())

vi.mock('@/api/admin/accounts', () => ({
  getWebLoginProxyCapture: getCaptureMock
}))

const POLL_INTERVAL_MS = 3000
const POLL_MAX_ATTEMPTS = 100

const HostComp = defineComponent({
  setup() {
    const cap = useWebLoginCapture()
    return { cap }
  },
  template: '<div></div>'
})

describe('useWebLoginCapture', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    getCaptureMock.mockReset()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('stops polling once captured and writes the cookie', async () => {
    getCaptureMock.mockResolvedValue({ captured: true, cookie: 'sessionid=abc', expires_at: '' })
    const wrapper = mount(HostComp)
    const cap = (wrapper.vm as any).cap

    cap.start('t1')
    await flushPromises()

    expect(cap.capturedCookie.value).toBe('sessionid=abc')
    expect(cap.capturing.value).toBe(false)
    expect(cap.timedOut.value).toBe(false)

    // Further timers must not trigger additional capture calls.
    vi.advanceTimersByTime(POLL_INTERVAL_MS * 5)
    await flushPromises()
    expect(getCaptureMock).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('sets timedOut and stops after reaching the attempt ceiling', async () => {
    getCaptureMock.mockResolvedValue({ captured: false, cookie: '', expires_at: '' })
    const wrapper = mount(HostComp)
    const cap = (wrapper.vm as any).cap

    cap.start('t1')
    await flushPromises() // first immediate tick

    let guard = 0
    while (!cap.timedOut.value && guard < POLL_MAX_ATTEMPTS + 5) {
      vi.advanceTimersByTime(POLL_INTERVAL_MS)
      await flushPromises()
      guard += 1
    }

    expect(cap.timedOut.value).toBe(true)
    expect(cap.capturing.value).toBe(false)
    expect(cap.capturedCookie.value).toBe('')
    expect(getCaptureMock).toHaveBeenCalledTimes(POLL_MAX_ATTEMPTS)

    // No further calls past the ceiling.
    vi.advanceTimersByTime(POLL_INTERVAL_MS * 3)
    await flushPromises()
    expect(getCaptureMock).toHaveBeenCalledTimes(POLL_MAX_ATTEMPTS)
    wrapper.unmount()
  })

  it('keeps polling on request error', async () => {
    getCaptureMock.mockRejectedValue(new Error('network'))
    const wrapper = mount(HostComp)
    const cap = (wrapper.vm as any).cap

    cap.start('t1')
    await flushPromises() // immediate tick rejects
    expect(getCaptureMock).toHaveBeenCalledTimes(1)
    expect(cap.timedOut.value).toBe(false)
    expect(cap.capturedCookie.value).toBe('')

    vi.advanceTimersByTime(POLL_INTERVAL_MS)
    await flushPromises()
    vi.advanceTimersByTime(POLL_INTERVAL_MS)
    await flushPromises()

    expect(getCaptureMock).toHaveBeenCalledTimes(3)
    expect(cap.capturedCookie.value).toBe('')
    expect(cap.timedOut.value).toBe(false)
    wrapper.unmount()
  })

  it('stop() clears the timer and prevents further polling', async () => {
    getCaptureMock.mockResolvedValue({ captured: false, cookie: '', expires_at: '' })
    const wrapper = mount(HostComp)
    const cap = (wrapper.vm as any).cap

    cap.start('t1')
    await flushPromises()
    expect(getCaptureMock).toHaveBeenCalledTimes(1)

    cap.stop()
    expect(cap.capturing.value).toBe(false)

    vi.advanceTimersByTime(POLL_INTERVAL_MS * 4)
    await flushPromises()
    expect(getCaptureMock).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('auto-stops on unmount', async () => {
    getCaptureMock.mockResolvedValue({ captured: false, cookie: '', expires_at: '' })
    const wrapper = mount(HostComp)
    const cap = (wrapper.vm as any).cap

    cap.start('t1')
    await flushPromises()
    expect(getCaptureMock).toHaveBeenCalledTimes(1)

    wrapper.unmount()
    vi.advanceTimersByTime(POLL_INTERVAL_MS * 4)
    await flushPromises()
    expect(getCaptureMock).toHaveBeenCalledTimes(1)
  })
})
