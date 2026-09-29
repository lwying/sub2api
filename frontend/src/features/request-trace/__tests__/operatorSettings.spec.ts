import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({ updateOperatorSettings: vi.fn() }))
vi.mock('../api', () => ({ updateOperatorSettings: api.updateOperatorSettings }))
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import RequestTraceOperatorSettings from '../RequestTraceOperatorSettings.vue'
import type { RequestTraceOperatorStatus } from '../types'

const phrase = 'New Trace risk: raw fragments may contain credentials, plaintext exports survive usage deletion.'
const status = (overrides: Partial<RequestTraceOperatorStatus> = {}): RequestTraceOperatorStatus => ({
  enabled: false,
  capture_allowed: false,
  risk_acknowledged: false,
  risk_acknowledgement_current: false,
  risk_version: 'v2026.09.28',
  risk_phrase_en: phrase,
  risk_phrase_zh: '新版请求跟踪明文风险确认。',
  plaintext_capture_supported: true,
  plaintext_capture_support_reason: 'supported',
  ...overrides,
})

function mountGate(current: RequestTraceOperatorStatus | null = status()) {
  return mount(RequestTraceOperatorSettings, { props: { status: current, loading: false } })
}

describe('Trace operator gate', () => {
  beforeEach(() => {
    api.updateOperatorSettings.mockReset()
    sessionStorage.clear()
    localStorage.clear()
  })

  it('shows stored on but effective off without treating an older acknowledgement as capture permission', () => {
    const wrapper = mountGate(status({ enabled: true, capture_allowed: false, risk_acknowledgement_current: false }))
    expect(wrapper.get('[data-testid="request-trace-stored"]').attributes('data-state')).toBe('on')
    expect(wrapper.get('[data-testid="request-trace-capture-state"]').attributes('data-state')).toBe('off')
    expect(wrapper.find('[data-testid="request-trace-ack-stale"]').exists()).toBe(true)
    expect(api.updateOperatorSettings).not.toHaveBeenCalled()
  })

  it('keeps a deployment error distinct from missing risk acknowledgement', () => {
    const wrapper = mountGate(status({ enabled: true, capture_allowed: false, risk_acknowledgement_current: true, plaintext_capture_supported: false, plaintext_capture_support_reason: 'probe_failed' }))
    expect(wrapper.find('[data-testid="request-trace-deployment-blocked"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="request-trace-ack-stale"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="request-trace-deployment"]').text()).toContain('probe_failed')
  })

  it('enables only after the exact server statement is typed, then drops the phrase', async () => {
    api.updateOperatorSettings.mockResolvedValue(status({ enabled: true, capture_allowed: true, risk_acknowledgement_current: true }))
    const wrapper = mountGate()
    const enable = wrapper.get('[data-testid="request-trace-enable"]')
    expect((enable.element as HTMLButtonElement).disabled).toBe(true)
    await wrapper.get('[data-testid="request-trace-phrase-input"]').setValue(phrase + ' changed')
    expect((enable.element as HTMLButtonElement).disabled).toBe(true)
    await wrapper.get('[data-testid="request-trace-phrase-input"]').setValue(phrase)
    expect((enable.element as HTMLButtonElement).disabled).toBe(false)
    await enable.trigger('click')
    await flushPromises()
    expect(api.updateOperatorSettings).toHaveBeenCalledWith({ enabled: true, language: 'en', phrase })
    expect((wrapper.get('[data-testid="request-trace-phrase-input"]').element as HTMLTextAreaElement).value).toBe('')
    expect(localStorage.getItem('request_trace_phrase')).toBeNull()
    expect(sessionStorage.getItem('request_trace_phrase')).toBeNull()
  })

  it('disables a stored-on gate without any risk phrase even on unsupported deployment', async () => {
    api.updateOperatorSettings.mockResolvedValue(status())
    const wrapper = mountGate(status({ enabled: true, plaintext_capture_supported: false, plaintext_capture_support_reason: 'unsupported_partitioned_usage_logs' }))
    expect(wrapper.find('[data-testid="request-trace-deployment-blocked"]').exists()).toBe(true)
    await wrapper.get('[data-testid="request-trace-disable"]').trigger('click')
    await flushPromises()
    expect(api.updateOperatorSettings).toHaveBeenCalledWith({ enabled: false, language: 'en', phrase: '' })
  })

  it('never renders unexpected credential or body fields supplied with status', () => {
    const wrapper = mountGate({ ...status(), body: 'BODY_CANARY', authorization: 'Bearer secret' } as RequestTraceOperatorStatus)
    expect(wrapper.text()).not.toContain('BODY_CANARY')
    expect(wrapper.text()).not.toContain('Bearer secret')
  })
})
