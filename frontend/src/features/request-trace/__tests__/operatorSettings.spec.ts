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
  all_groups: true,
  group_ids: [],
  model_scope: 'all',
  models: [],
  platform_scope: 'all',
  platforms: [],
  ...overrides,
})

function mountGate(current: RequestTraceOperatorStatus | null = status()) {
  return mount(RequestTraceOperatorSettings, { props: { status: current, loading: false } })
}

/** The scope half of the update body, as the form submits it for the given picks. */
function scopeBody(overrides: Record<string, unknown> = {}) {
  return {
    scope_provided: true,
    all_groups: true,
    group_ids: [],
    model_scope: 'all',
    models: [],
    platform_scope: 'all',
    platforms: [],
    ...overrides,
  }
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

  it('labels the risk-statement language selector through the UI locale', () => {
    const wrapper = mountGate()
    const options = wrapper.get('[data-testid="request-trace-language"]').findAll('option')
    expect(options.map(option => option.text())).toEqual([
      'admin.requestTrace.operator.languages.en',
      'admin.requestTrace.operator.languages.zh',
    ])
  })

  it('never renders unexpected credential or body fields supplied with status', () => {
    const wrapper = mountGate({ ...status(), body: 'BODY_CANARY', authorization: 'Bearer secret' } as RequestTraceOperatorStatus)
    expect(wrapper.text()).not.toContain('BODY_CANARY')
    expect(wrapper.text()).not.toContain('Bearer secret')
  })
})

describe('Trace capture scope', () => {
  beforeEach(() => {
    api.updateOperatorSettings.mockReset()
    sessionStorage.clear()
    localStorage.clear()
  })

  it('labels empty all-scope lists as all rather than no matches', () => {
    const wrapper = mountGate()
    for (const dimension of ['groups', 'models', 'platforms']) {
      expect(wrapper.get(`[data-testid="request-trace-scope-stored-${dimension}"]`).text())
        .toBe('admin.requestTrace.operator.scope.allValues')
    }
  })

  it('shows the stored scope from the server and saves it back whole', async () => {
    const current = status({
      all_groups: false, group_ids: [4, 7],
      model_scope: 'include', models: ['claude-sonnet-4-5'],
      platform_scope: 'exclude', platforms: ['antigravity'],
    })
    api.updateOperatorSettings.mockResolvedValue(current)
    const wrapper = mountGate(current)

    expect(wrapper.get('[data-testid="request-trace-scope-stored-groups"]').text()).toBe('admin.requestTrace.operator.scope.onlyValues')
    expect(wrapper.get('[data-testid="request-trace-scope-stored-models"]').text()).toBe('admin.requestTrace.operator.scope.onlyValues')
    expect(wrapper.get('[data-testid="request-trace-scope-stored-platforms"]').text()).toBe('admin.requestTrace.operator.scope.exceptValues')
    expect((wrapper.get('[data-testid="request-trace-scope-all-groups"]').element as HTMLInputElement).checked).toBe(false)

    await wrapper.get('[data-testid="request-trace-scope-save"]').trigger('click')
    await flushPromises()
    expect(api.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: false,
      language: 'en',
      phrase: '',
      scope_provided: true,
      all_groups: false,
      group_ids: [4, 7],
      model_scope: 'include',
      models: ['claude-sonnet-4-5'],
      platform_scope: 'exclude',
      platforms: ['antigravity'],
    })
  })

  it('sends a complete scope for an edited draft and never a partial one', async () => {
    api.updateOperatorSettings.mockResolvedValue(status())
    const wrapper = mountGate()
    await wrapper.get('[data-testid="request-trace-scope-all-groups"]').setValue(false)
    await wrapper.get('[data-testid="request-trace-scope-group-ids"]').setValue('7 9')
    await wrapper.get('[data-testid="request-trace-scope-model-mode"]').setValue('exclude')
    await wrapper.get('[data-testid="request-trace-scope-models"]').setValue('gpt-5.3-codex, claude-sonnet-4-5')
    await wrapper.get('[data-testid="request-trace-scope-save"]').trigger('click')
    await flushPromises()
    expect(api.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: false,
      language: 'en',
      phrase: '',
      ...scopeBody({
        all_groups: false,
        group_ids: [7, 9],
        model_scope: 'exclude',
        models: ['gpt-5.3-codex', 'claude-sonnet-4-5'],
      }),
    })
    expect(wrapper.find('[data-testid="request-trace-scope-error"]').exists()).toBe(false)
  })

  it('refuses an incomplete scope instead of storing a scope that matches nothing', async () => {
    api.updateOperatorSettings.mockResolvedValue(status())
    const wrapper = mountGate()
    await wrapper.get('[data-testid="request-trace-scope-model-mode"]').setValue('include')
    await wrapper.get('[data-testid="request-trace-scope-save"]').trigger('click')
    expect(wrapper.get('[data-testid="request-trace-scope-error"]').exists()).toBe(true)
    expect(api.updateOperatorSettings).not.toHaveBeenCalled()
    await wrapper.get('[data-testid="request-trace-scope-models"]').setValue('claude-sonnet-4-5')
    await wrapper.get('[data-testid="request-trace-scope-all-groups"]').setValue(false)
    await wrapper.get('[data-testid="request-trace-scope-group-ids"]').setValue('0')
    await wrapper.get('[data-testid="request-trace-scope-save"]').trigger('click')
    expect(wrapper.get('[data-testid="request-trace-scope-error"]').exists()).toBe(true)
    expect(api.updateOperatorSettings).not.toHaveBeenCalled()
  })

  it('saves the scope while capture is on without touching the switch, and only with a freshly typed statement', async () => {
    const current = status({ enabled: true, capture_allowed: true, risk_acknowledged: true, risk_acknowledgement_current: true })
    api.updateOperatorSettings.mockResolvedValue(current)
    const wrapper = mountGate(current)
    // The statement is never replayed from the stored acknowledgement.
    const save = wrapper.get('[data-testid="request-trace-scope-save"]')
    expect((save.element as HTMLButtonElement).disabled).toBe(true)
    expect((wrapper.get('[data-testid="request-trace-scope-phrase"]').element as HTMLTextAreaElement).value).toBe('')
    await wrapper.get('[data-testid="request-trace-scope-phrase"]').setValue(phrase)
    expect((save.element as HTMLButtonElement).disabled).toBe(false)
    await save.trigger('click')
    await flushPromises()
    expect(api.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      language: 'en',
      phrase,
      ...scopeBody(),
    })
  })

  it('keeps a switch update free of scope fields', async () => {
    api.updateOperatorSettings.mockResolvedValue(status({ enabled: true, capture_allowed: true }))
    const wrapper = mountGate()
    await wrapper.get('[data-testid="request-trace-phrase-input"]').setValue(phrase)
    await wrapper.get('[data-testid="request-trace-enable"]').trigger('click')
    await flushPromises()
    const [body] = api.updateOperatorSettings.mock.calls[0]
    expect(Object.keys(body as object).sort()).toEqual(['enabled', 'language', 'phrase'])
    expect(body).toEqual({ enabled: true, language: 'en', phrase })
  })

  it('surfaces the exclusion risk and the group semantics instead of promising a platform never appears', () => {
    const wrapper = mountGate()
    expect(wrapper.get('[data-testid="request-trace-scope-platform-risk"]').text())
      .toBe('admin.requestTrace.operator.scope.excludeRisk')
    expect(wrapper.get('[data-testid="request-trace-scope-platforms-note"]').text())
      .toBe('admin.requestTrace.operator.scope.platformsNote')
    expect(wrapper.get('[data-testid="request-trace-scope-models-note"]').text())
      .toBe('admin.requestTrace.operator.scope.modelsNote')
    expect(wrapper.get('[data-testid="request-trace-scope-groups-note"]').text())
      .toBe('admin.requestTrace.operator.scope.groupsNote')
  })

  it('offers no unknown-platform toggle, because the backend has no such setting', () => {
    // `service.RequestTraceOperatorStatus` carries no `platform_exclude_unknown`:
    // an unknown platform is never captured under "only"/"except", and only "all
    // platforms" covers it. A control here would let the operator set a rule the
    // server cannot store, and reading the field would make the panel fail to
    // load against a real backend.
    for (const platformScope of ['all', 'include', 'exclude'] as const) {
      const wrapper = mountGate(status({ platform_scope: platformScope, platforms: platformScope === 'all' ? [] : ['antigravity'] }))
      expect(wrapper.find('[data-testid="request-trace-scope-platform-exclude-unknown"]').exists()).toBe(false)
      expect(wrapper.find('[data-testid="request-trace-scope-stored-platform-unknown"]').exists()).toBe(false)
      // The exclusion risk is still stated, in every platform scope.
      expect(wrapper.get('[data-testid="request-trace-scope-platform-risk"]').text())
        .toBe('admin.requestTrace.operator.scope.excludeRisk')
    }
  })

  it('confirms a saved scope only until the draft is edited again', async () => {
    api.updateOperatorSettings.mockResolvedValue(status())
    const wrapper = mountGate()
    await wrapper.get('[data-testid="request-trace-scope-save"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="request-trace-scope-saved"]').text()).toBe('admin.requestTrace.operator.scope.updated')
    await wrapper.get('[data-testid="request-trace-scope-all-groups"]').setValue(false)
    expect(wrapper.find('[data-testid="request-trace-scope-saved"]').exists()).toBe(false)
  })

  it('reports a refused scope save without claiming the server accepted it', async () => {
    api.updateOperatorSettings.mockRejectedValue(new Error('REQUEST_TRACE_DEPLOYMENT_UNSUPPORTED'))
    const wrapper = mountGate()
    await wrapper.get('[data-testid="request-trace-scope-save"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="request-trace-error"]').text()).toBe('admin.requestTrace.operator.scope.saveFailed')
    expect(wrapper.find('[data-testid="request-trace-scope-saved"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('REQUEST_TRACE_DEPLOYMENT_UNSUPPORTED')
  })
})
