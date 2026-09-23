import { flushPromises, shallowMount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import UsageRequestAuditDrawer from '../UsageRequestAuditDrawer.vue'

const mocks = vi.hoisted(() => ({
  getRequestAudit: vi.fn(),
}))

vi.mock('@/api/admin/usage', () => ({
  getRequestAudit: mocks.getRequestAudit,
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  const labels: Record<string, string> = {
    'admin.usage.requestAudit.inboundHeaders': 'Inbound headers',
    'admin.usage.requestAudit.wireRequestHeaders': 'Wire request headers',
    'admin.usage.requestAudit.upstreamResponseHeaders': 'Upstream response headers',
    'admin.usage.requestAudit.upstreamStatus': 'Upstream status',
    'admin.usage.requestAudit.requestPayloadBytes': 'Request payload bytes',
    'admin.usage.requestAudit.responsePayloadBytes': 'Response payload bytes',
    'admin.usage.requestAudit.tokenCounts.title': 'Token counts',
    'admin.usage.requestAudit.tokenCounts.input': 'Input tokens',
    'admin.usage.requestAudit.tokenCounts.output': 'Output tokens',
    'admin.usage.requestAudit.responseReadComplete': 'Response read complete',
    'admin.usage.requestAudit.yes': 'Yes',
    'admin.usage.requestAudit.no': 'No',
    'admin.usage.requestAudit.unknown': 'Unknown',
    'admin.usage.requestAudit.captureStatuses.complete': 'Complete',
    'admin.usage.requestAudit.captureStatuses.truncated': 'Truncated',
    'admin.usage.requestAudit.captureStatuses.incomplete': 'Response incomplete',
    'admin.usage.requestAudit.captureStatuses.writeFailed': 'Audit write failed',
    'admin.usage.requestAudit.captureStatuses.notCaptured': 'Not captured',
    'admin.usage.requestAudit.eventFingerprint': 'HMAC fingerprint',
    'admin.usage.requestAudit.modelFingerprint': 'Model fingerprint',
    'admin.usage.requestAudit.protocolFields.title': 'Protocol fields',
    'admin.usage.requestAudit.protocolFields.stream': 'Stream',
    'admin.usage.requestAudit.protocolFields.thinkingType': 'Thinking type',
    'admin.usage.requestAudit.protocolFields.presentFields': 'Present fields',
    'admin.usage.requestAudit.protocolFields.normalizedFields': 'Normalization',
    'admin.usage.requestAudit.protocolFields.thinkingDisabled': 'Disabled',
    'admin.usage.requestAudit.protocolFields.thinkingEnabled': 'Enabled',
    'admin.usage.requestAudit.protocolFields.thinkingAdaptive': 'Adaptive',
    'admin.usage.requestAudit.protocolFields.normalizedThinkingExtraFieldsRemoved': 'Thinking extra fields removed',
    'admin.usage.requestAudit.protocolFields.fieldModel': 'Model',
    'admin.usage.requestAudit.protocolFields.fieldMessages': 'Messages',
    'admin.usage.requestAudit.protocolFields.fieldInput': 'Input',
    'admin.usage.requestAudit.protocolFields.fieldTools': 'Tools',
    'admin.usage.requestAudit.protocolFields.fieldStream': 'Stream',
    'admin.usage.requestAudit.protocolFields.fieldThinking': 'Thinking',
    'admin.usage.requestAudit.protocolFields.unknown': 'Unknown',
    'admin.usage.requestAudit.truncation.originalEvents': 'Original events',
    'admin.usage.requestAudit.truncation.originalBytes': 'Original bytes',
    'admin.usage.requestAudit.truncation.keptEvents': 'Kept events',
    'admin.usage.requestAudit.truncation.keptBytes': 'Kept bytes',
    'admin.usage.requestAudit.truncation.droppedEvents': 'Dropped events',
    'admin.usage.requestAudit.truncation.droppedBytes': 'Dropped bytes',
    'admin.usage.requestAudit.truncation.reasonMaxEvents': 'Maximum event count reached',
    'admin.usage.requestAudit.truncation.reasonMaxBytes': 'Maximum event size reached',
    'admin.usage.requestAudit.truncation.reasonUnknown': 'Unknown truncation reason',
    'admin.usage.requestAudit.clientResponse.title': 'Client response',
    'admin.usage.requestAudit.clientResponse.status': 'Status returned to client',
    'admin.usage.requestAudit.clientResponse.bytes': 'Bytes written to client',
    'admin.usage.requestAudit.clientResponse.observedNote':
      'Handler output observed bytes. These are the bytes the handler wrote out to the client, not proof of delivery and not the total bytes on the wire. No response body is stored.',
    'admin.usage.requestAudit.empty': 'No upstream attempts recorded',
  }
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => labels[key] ?? key }),
  }
})

describe('UsageRequestAuditDrawer', () => {
  beforeEach(() => {
    mocks.getRequestAudit.mockReset()
  })

  it('ignores a stale response after switching usage rows', async () => {
    let resolveFirst!: (value: any) => void
    let resolveSecond!: (value: any) => void
    mocks.getRequestAudit
      .mockReturnValueOnce(new Promise((resolve) => { resolveFirst = resolve }))
      .mockReturnValueOnce(new Promise((resolve) => { resolveSecond = resolve }))

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 7 },
      global: {
        stubs: {
          BaseDialog: { template: '<div><slot /></div>' },
          Icon: true,
        },
      },
    })
    await wrapper.setProps({ usageLogId: 8 })

    resolveSecond({
      usage_log_id: 8,
      attempts: [{ account_id: 808, model_fingerprint: '8'.repeat(64), protocol: 'openai.responses', stage: 'wire' }],
      events: [],
      headers: {},
      capture_completeness: 'complete',
    })
    await flushPromises()
    expect(wrapper.text()).toContain('808')

    resolveFirst({
      usage_log_id: 7,
      attempts: [{ account_id: 707, model_fingerprint: '7'.repeat(64), protocol: 'openai.responses', stage: 'wire' }],
      events: [],
      headers: {},
      capture_completeness: 'complete',
    })
    await flushPromises()

    expect(wrapper.text()).toContain('808')
    expect(wrapper.text()).not.toContain('707')
  })

  it('renders an explicit first-phase uncovered reason without fake attempts', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 9,
      headers: {},
      events: [],
      attempts: [],
      capture_completeness: 'not_captured',
      capture_reason: 'phase1_uncovered',
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 9 },
      global: {
        stubs: {
          BaseDialog: { template: '<div><slot /></div>' },
          Icon: true,
        },
      },
    })
    await flushPromises()

    expect(wrapper.get('[data-testid="request-audit-not-captured"]').text()).toContain(
      'admin.usage.requestAudit.notCapturedReasons.phase1Uncovered'
    )
    expect(wrapper.findAll('[data-testid="request-audit-attempt"]')).toHaveLength(0)
    expect(wrapper.text()).not.toContain('admin.usage.requestAudit.empty')
  })

  it('renders multiple upstream attempts on one timeline without model body', async () => {
    const clientAliasDigest = '1'.repeat(64)
    const normalizedAliasDigest = '2'.repeat(64)
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 7,
      headers: {
        'X-Stainless-Lang': 'js',
        Authorization: { present: true },
      },
      events: [
        { type: 'response.created', index: 0, bytes: 12 },
        { type: 'response.output_text.delta', index: 1, bytes: 40 },
      ],
      attempts: [
        { model_fingerprint: clientAliasDigest, protocol: 'openai.chat.completions', stage: 'client_entry' },
        { model_fingerprint: normalizedAliasDigest, protocol: 'openai.chat.completions', stage: 'post_normalize' },
        { account_id: 701, model_fingerprint: clientAliasDigest, protocol: 'openai.chat.completions', stage: 'wire' },
        { account_id: 702, model_fingerprint: clientAliasDigest, protocol: 'openai.chat.completions', stage: 'wire' },
      ],
      capture_completeness: 'complete',
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 7 },
      global: {
        stubs: {
          BaseDialog: { template: '<div><slot /></div>' },
          Icon: true,
        },
      },
    })
    await flushPromises()

    expect(mocks.getRequestAudit).toHaveBeenCalledWith(7)
    const attempts = wrapper.findAll('[data-testid="request-audit-attempt"]')
    expect(attempts).toHaveLength(4)
    expect(attempts[2].text()).toContain('701')
    expect(attempts[3].text()).toContain('702')
    expect(attempts[0].text()).toContain('client_entry')
    expect(attempts[1].text()).toContain('post_normalize')
    expect(wrapper.text()).not.toContain('secret prompt')
    expect(wrapper.text()).not.toContain('sk-')
    expect(wrapper.text()).not.toContain('secret delta')
  })

  it('renders the model fingerprint digest with its stage, matching equal aliases and separating different ones', async () => {
    const clientAliasDigest = 'a1'.repeat(32)
    const normalizedAliasDigest = 'b2'.repeat(32)
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 31,
      headers: {},
      events: [],
      attempts: [
        { protocol: 'openai.chat.completions', stage: 'client_entry', model_fingerprint: clientAliasDigest },
        { protocol: 'openai.chat.completions', stage: 'post_normalize', model_fingerprint: normalizedAliasDigest },
        { account_id: 701, protocol: 'openai.chat.completions', stage: 'wire', model_fingerprint: clientAliasDigest },
        { account_id: 702, protocol: 'openai.chat.completions', stage: 'wire', model_fingerprint: normalizedAliasDigest },
      ],
      capture_completeness: 'complete',
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 31 },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()

    const attempts = wrapper.findAll('[data-testid="request-audit-attempt"]')
    expect(attempts).toHaveLength(4)
    const fingerprints = attempts.map((attempt) =>
      attempt.get('[data-testid="request-audit-attempt-model-fingerprint"]')
    )

    // Stage stays attached to the digest it belongs to.
    expect(attempts[0].text()).toContain('client_entry')
    expect(attempts[1].text()).toContain('post_normalize')
    expect(attempts[2].text()).toContain('wire')

    // Digest is shown truncated so a full digest never becomes page text, while the
    // complete digest stays available for cross-attempt comparison.
    expect(fingerprints[0].text()).toContain('Model fingerprint')
    expect(fingerprints[0].text()).toContain('a1a1a1a1a1a1')
    expect(fingerprints[0].text()).not.toContain(clientAliasDigest)
    expect(fingerprints[0].attributes('title')).toBe(clientAliasDigest)
    expect(fingerprints[1].attributes('title')).toBe(normalizedAliasDigest)

    // Same alias at different stages is comparable: same digest, same rendering.
    expect(fingerprints[2].text()).toBe(fingerprints[0].text())
    expect(fingerprints[2].attributes('title')).toBe(clientAliasDigest)
    expect(fingerprints[3].text()).toBe(fingerprints[1].text())
    // Different aliases never collapse into the same rendering.
    expect(fingerprints[1].text()).not.toBe(fingerprints[0].text())
    expect(fingerprints[1].attributes('title')).not.toBe(fingerprints[0].attributes('title'))
  })

  it('renders only 64-character lowercase hex fingerprints and never a raw model alias', async () => {
    const validDigest = 'c3'.repeat(32)
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 32,
      headers: {},
      events: [],
      attempts: [
        {
          stage: 'client_entry',
          model: 'RAW-MODEL-ALIAS-CANARY',
          model_fingerprint: 'FINGERPRINT-CANARY',
        },
        { stage: 'post_normalize', model_fingerprint: validDigest.toUpperCase() },
        { stage: 'wire', model_fingerprint: `${validDigest.slice(0, 63)}g` },
        { stage: 'wire', model_fingerprint: validDigest.slice(0, 63) },
        { stage: 'wire', model: 'RAW-MODEL-ALIAS-CANARY', model_fingerprint: validDigest },
      ],
      capture_completeness: 'complete',
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 32 },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()

    const fingerprints = wrapper.findAll('[data-testid="request-audit-attempt-model-fingerprint"]')
    expect(fingerprints).toHaveLength(1)
    expect(fingerprints[0].attributes('title')).toBe(validDigest)
    expect(wrapper.text()).toContain('c3c3c3c3c3c3')

    const text = wrapper.text()
    expect(text).not.toContain('RAW-MODEL-ALIAS-CANARY')
    expect(text).not.toContain('FINGERPRINT-CANARY')
    expect(text).not.toContain(validDigest.toUpperCase())
    expect(text).not.toContain(`${validDigest.slice(0, 63)}g`)
    expect(text).not.toContain(validDigest.slice(0, 63))
  })

  it('shows wire metadata as facts for the matching upstream attempt', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 11,
      headers: { 'X-Client-Trace': 'inbound-trace-11' },
      events: [],
      attempts: [{
        account_id: 711,
        protocol: 'openai.responses',
        stage: 'wire',
        wire_request_headers: { 'X-Request-Id': 'request-11' },
        upstream_response_headers: { 'X-Upstream-Id': 'upstream-11' },
        upstream_status: 429,
        request_payload_bytes: 0,
        response_payload_bytes: 42,
        response_read_complete: false,
      }],
      capture_completeness: 'complete',
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 11 },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()

    const text = wrapper.text()
    expect(text).toContain('Inbound headers')
    expect(text).toContain('inbound-trace-11')
    expect(text).toContain('Wire request headers')
    expect(text).toContain('X-Request-Id')
    expect(text).toContain('request-11')
    expect(text).toContain('Upstream response headers')
    expect(text).toContain('X-Upstream-Id')
    expect(text).toContain('upstream-11')
    expect(text).toContain('Upstream status')
    expect(text).toContain('429')
    expect(text).toContain('Request payload bytes')
    expect(text).toContain('0 B')
    expect(text).toContain('Response payload bytes')
    expect(text).toContain('42 B')
    expect(text).toContain('Response read complete')
    expect(text).toContain('No')
    expect(wrapper.find('[data-testid="request-audit-attempt"]').text()).not.toContain('inbound-trace-11')
  })

  it('distinguishes unknown telemetry from observed zero and false', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 12,
      events: [],
      attempts: [
        {
          stage: 'wire',
          upstream_status: 0,
          request_payload_bytes: 0,
          response_payload_bytes: 0,
          response_read_complete: false,
        },
        { stage: 'wire' },
      ],
      capture_completeness: 'complete',
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 12 },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()

    const attempts = wrapper.findAll('[data-testid="request-audit-attempt"]')
    expect(attempts).toHaveLength(2)
    expect(attempts[0].text()).toContain('Upstream status0')
    expect(attempts[0].text()).toContain('Request payload bytes0 B')
    expect(attempts[0].text()).toContain('Response payload bytes0 B')
    expect(attempts[0].text()).toContain('Response read completeNo')
    expect(attempts[1].text()).toContain('Upstream statusUnknown')
    expect(attempts[1].text()).toContain('Request payload bytesUnknown')
    expect(attempts[1].text()).toContain('Response payload bytesUnknown')
    expect(attempts[1].text()).toContain('Response read completeUnknown')
    expect(attempts[1].text()).toContain('Wire request headersUnknown')
    expect(attempts[1].text()).toContain('Upstream response headersUnknown')
  })

  it('shows event HMACs and truncation counts without rendering event data', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 14,
      headers: {},
      attempts: [],
      events: [
        {
          type: 'message_start',
          index: 0,
          bytes: 23,
          fingerprint: 'a'.repeat(64),
          data: 'EVENT_DELTA_SENTINEL_MUST_NOT_RENDER',
        },
        {
          type: 'truncated',
          index: 1,
          bytes: 0,
          truncated: true,
          original: 2003,
          original_bytes: 260000,
          kept: 1,
          kept_bytes: 23,
          dropped: 2002,
          dropped_bytes: 259977,
          reason: 'max_events',
          data: 'TRUNCATION_EVENT_BODY_MUST_NOT_RENDER',
        },
      ],
      capture_completeness: 'truncated',
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 14 },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()

    expect(wrapper.get('[data-testid="request-audit-capture-status"]').text()).toContain('Truncated')
    const event = wrapper.get('[data-testid="request-audit-event"]')
    expect(event.text()).toContain('HMAC fingerprint')
    expect(event.text()).toContain('a'.repeat(64))
    expect(event.text()).not.toContain('EVENT_DELTA_SENTINEL_MUST_NOT_RENDER')
    expect(wrapper.get('[data-testid="request-audit-truncation"]').text()).toContain('Original events2003')
    expect(wrapper.get('[data-testid="request-audit-truncation"]').text()).toContain('Original bytes260000 B')
    expect(wrapper.get('[data-testid="request-audit-truncation"]').text()).toContain('Kept events1')
    expect(wrapper.get('[data-testid="request-audit-truncation"]').text()).toContain('Kept bytes23 B')
    expect(wrapper.get('[data-testid="request-audit-truncation"]').text()).toContain('Dropped events2002')
    expect(wrapper.get('[data-testid="request-audit-truncation"]').text()).toContain('Dropped bytes259977 B')
    expect(wrapper.get('[data-testid="request-audit-truncation"]').text()).toContain('Maximum event count reached')
    expect(wrapper.text()).not.toContain('TRUNCATION_EVENT_BODY_MUST_NOT_RENDER')
  })

  it.each([
    ['incomplete', 'Response incomplete'],
    ['write_failed', 'Audit write failed'],
  ])('shows %s capture status', async (captureCompleteness, expectedStatus) => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 15,
      headers: {},
      attempts: [],
      events: [],
      capture_completeness: captureCompleteness,
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 15 },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()

    expect(wrapper.get('[data-testid="request-audit-capture-status"]').text()).toContain(expectedStatus)
  })

  it('renders allowlisted protocol fields while distinguishing false from unknown', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 21,
      events: [],
      attempts: [],
      metadata: {
        protocol_fields: {
          stream: false,
          thinking_type: 'adaptive',
          present_fields: ['model', 'thinking'],
          normalized_fields: ['thinking.extra_fields_removed'],
        },
      },
      capture_completeness: 'complete',
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 21 },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()

    const protocolFields = wrapper.get('[data-testid="request-audit-protocol-fields"]')
    expect(protocolFields.text()).toContain('Protocol fields')
    expect(protocolFields.text()).toContain('StreamNo')
    expect(protocolFields.text()).toContain('Thinking typeAdaptive')
    expect(protocolFields.text()).toContain('Present fieldsModel, Thinking')
    expect(protocolFields.text()).toContain('NormalizationThinking extra fields removed')
  })

  it('shows unknown protocol values as unknown and never renders untrusted metadata strings', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 22,
      events: [],
      attempts: [],
      metadata: {
        protocol_fields: {
          thinking_type: 'adaptive-canary',
          present_fields: ['model', 'tool_name-canary'],
          normalized_fields: ['thinking.tool_args-canary'],
        },
      },
      capture_completeness: 'complete',
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 22 },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()

    const protocolFields = wrapper.get('[data-testid="request-audit-protocol-fields"]')
    expect(protocolFields.text()).toContain('StreamUnknown')
    expect(protocolFields.text()).toContain('Thinking typeUnknown')
    expect(protocolFields.text()).toContain('Present fieldsModel')
    expect(protocolFields.text()).not.toContain('adaptive-canary')
    expect(protocolFields.text()).not.toContain('tool_name-canary')
    expect(protocolFields.text()).not.toContain('thinking.tool_args-canary')
  })

  it('renders only positive input and output token counts from metadata', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 23,
      events: [],
      attempts: [],
      metadata: {
        tokens: {
          input_tokens: 123,
          output_tokens: 45,
          cache_read_tokens: 999,
          malicious_unknown_key: 987654,
          zero_count: 0,
        },
      },
      capture_completeness: 'complete',
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 23 },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()

    const tokenCounts = wrapper.get('[data-testid="request-audit-token-counts"]')
    expect(tokenCounts.text()).toContain('Token counts')
    expect(tokenCounts.text()).toContain('Input tokens123')
    expect(tokenCounts.text()).toContain('Output tokens45')
    expect(tokenCounts.text()).not.toContain('cache_read_tokens')
    expect(tokenCounts.text()).not.toContain('malicious_unknown_key')
    expect(tokenCounts.text()).not.toContain('987654')
    expect(tokenCounts.text()).not.toContain('0')
  })

  it.each([
    ['zero', { input_tokens: 0, output_tokens: 0 }],
    ['unknown', {}],
  ])('does not render zero or unknown token counts (%s)', async (_description, tokens) => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 24,
      events: [],
      attempts: [],
      metadata: { tokens },
      capture_completeness: 'complete',
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 24 },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()

    expect(wrapper.find('[data-testid="request-audit-token-counts"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Input tokens')
    expect(wrapper.text()).not.toContain('Output tokens')
  })

  it('keeps older audit records readable without rendering request or response bodies', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 13,
      headers: { 'X-Legacy-Client': 'legacy-client' },
      events: [],
      capture_completeness: 'complete',
      request_body: 'private prompt text',
      response_body: 'private completion text',
    })

    const wrapper = shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId: 13 },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()

    expect(wrapper.text()).toContain('Inbound headers')
    expect(wrapper.text()).toContain('legacy-client')
    expect(wrapper.text()).toContain('No upstream attempts recorded')
    expect(wrapper.text()).not.toContain('private prompt text')
    expect(wrapper.text()).not.toContain('private completion text')
  })

  function mountAuditDrawer(usageLogId: number) {
    return shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' }, Icon: true } },
    })
  }

  const clientResponseSectionSelector = '[data-testid="request-audit-client-response"]'
  const clientResponseStatusSelector = '[data-testid="request-audit-client-response-status"]'
  const clientResponseBytesSelector = '[data-testid="request-audit-client-response-bytes"]'

  it('shows unknown client response facts for records written before the stage was observed', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 41,
      headers: {},
      events: [],
      attempts: [],
      capture_completeness: 'complete',
    })

    const wrapper = mountAuditDrawer(41)
    await flushPromises()

    const section = wrapper.get(clientResponseSectionSelector)
    expect(section.get(clientResponseStatusSelector).text()).toBe('Unknown')
    expect(section.get(clientResponseBytesSelector).text()).toBe('Unknown')
    // A legacy record has no observed client response, so nothing here may look like a fact.
    expect(section.text()).not.toContain('0 B')
  })

  it('renders the observed client response status and bytes with the handler-output caveat', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 42,
      headers: {},
      events: [],
      attempts: [],
      metadata: { status: { client_response: 200 }, bytes: { client_response: 4096 } },
      capture_completeness: 'complete',
    })

    const wrapper = mountAuditDrawer(42)
    await flushPromises()

    const section = wrapper.get(clientResponseSectionSelector)
    expect(section.get(clientResponseStatusSelector).text()).toBe('200')
    expect(section.get(clientResponseBytesSelector).text()).toBe('4096 B')
    // The observed byte count is handler output, so the drawer must say so instead of implying delivery.
    expect(section.text()).toContain('Handler output observed bytes')
    expect(section.text()).toContain('No response body is stored')
  })

  it('shows a committed client response status with observed zero bytes', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 43,
      headers: {},
      events: [],
      attempts: [],
      metadata: { status: { client_response: 404 }, bytes: { client_response: 0 } },
      capture_completeness: 'complete',
    })

    const wrapper = mountAuditDrawer(43)
    await flushPromises()

    const section = wrapper.get(clientResponseSectionSelector)
    expect(section.get(clientResponseStatusSelector).text()).toBe('404')
    expect(section.get(clientResponseBytesSelector).text()).toBe('0 B')
  })

  it('shows unknown for a client response fact that was never observed, distinct from an observed zero', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 44,
      headers: {},
      events: [],
      attempts: [],
      metadata: { status: { client_response: 404 } },
      capture_completeness: 'complete',
    })

    const withoutBytes = mountAuditDrawer(44)
    await flushPromises()

    const noBytesSection = withoutBytes.get(clientResponseSectionSelector)
    expect(noBytesSection.get(clientResponseStatusSelector).text()).toBe('404')
    expect(noBytesSection.get(clientResponseBytesSelector).text()).toBe('Unknown')

    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 45,
      headers: {},
      events: [],
      attempts: [],
      metadata: { bytes: { client_response: 0 } },
      capture_completeness: 'complete',
    })

    const withoutStatus = mountAuditDrawer(45)
    await flushPromises()

    const noStatusSection = withoutStatus.get(clientResponseSectionSelector)
    expect(noStatusSection.get(clientResponseStatusSelector).text()).toBe('Unknown')
    expect(noStatusSection.get(clientResponseBytesSelector).text()).toBe('0 B')
  })

  it('renders only the closed client_response keys and ignores arbitrary status or byte entries', async () => {
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 46,
      headers: {},
      events: [],
      attempts: [],
      metadata: {
        status: {
          client_response: 404,
          upstream: 418,
          client_response_evil: 999,
          Client_Response: 500,
          'client_response ': 501,
        },
        bytes: {
          client_response: 512,
          upstream: 987654,
          client_response_x: 123,
        },
      },
      capture_completeness: 'complete',
    })

    const wrapper = mountAuditDrawer(46)
    await flushPromises()

    const section = wrapper.get(clientResponseSectionSelector)
    expect(section.get(clientResponseStatusSelector).text()).toBe('404')
    expect(section.get(clientResponseBytesSelector).text()).toBe('512 B')

    const text = section.text()
    for (const untrusted of ['418', '999', '500', '501', '987654', '123', 'upstream', 'evil']) {
      expect(text).not.toContain(untrusted)
    }
  })

  it('never renders an invalid client response status', async () => {
    const invalidStatuses: unknown[] = [42, 600, 200.5, '200', true, null, {}]

    for (const invalid of invalidStatuses) {
      mocks.getRequestAudit.mockResolvedValue({
        usage_log_id: 47,
        headers: {},
        events: [],
        attempts: [],
        metadata: { status: { client_response: invalid }, bytes: { client_response: 0 } },
        capture_completeness: 'complete',
      })

      const wrapper = mountAuditDrawer(47)
      await flushPromises()

      const section = wrapper.get(clientResponseSectionSelector)
      expect(section.get(clientResponseStatusSelector).text()).toBe('Unknown')
      expect(section.get(clientResponseBytesSelector).text()).toBe('0 B')
      expect(section.text()).not.toContain(String(invalid))
      wrapper.unmount()
    }
  })

  it('never renders an invalid observed byte count', async () => {
    const invalidByteCounts: unknown[] = [-1, 1.5, 1e300, '2048', true, null, {}]

    for (const invalid of invalidByteCounts) {
      mocks.getRequestAudit.mockResolvedValue({
        usage_log_id: 48,
        headers: {},
        events: [],
        attempts: [],
        metadata: { status: { client_response: 200 }, bytes: { client_response: invalid } },
        capture_completeness: 'complete',
      })

      const wrapper = mountAuditDrawer(48)
      await flushPromises()

      const section = wrapper.get(clientResponseSectionSelector)
      expect(section.get(clientResponseStatusSelector).text()).toBe('200')
      expect(section.get(clientResponseBytesSelector).text()).toBe('Unknown')
      expect(section.text()).not.toContain(String(invalid))
      wrapper.unmount()
    }
  })
})
