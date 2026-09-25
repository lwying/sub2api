import { flushPromises, shallowMount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import UsageRequestAuditDrawer from '../UsageRequestAuditDrawer.vue'

const mocks = vi.hoisted(() => ({
  getRequestAudit: vi.fn(),
  getRequestAuditValueDetail: vi.fn(),
  revealRequestAuditValueDetail: vi.fn(),
}))

/**
 * The value-detail "transport" mocks return RAW payloads which are then passed
 * through the real normalizers. That keeps the allowlist under test: a canary the
 * real boundary drops proves the guard, not the stub. `getRequestAudit` returns a
 * payload the component consumes as-is, so it is mocked directly.
 */
vi.mock('@/api/admin/usage', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/admin/usage')>()
  return {
    ...actual,
    getRequestAudit: mocks.getRequestAudit,
    getRequestAuditValueDetail: async (id: number) =>
      actual.normalizeRequestAuditValueDetailEnvelope(await mocks.getRequestAuditValueDetail(id)),
    revealRequestAuditValueDetail: async (id: number) =>
      actual.normalizeRequestAuditValueDetailReveal(await mocks.revealRequestAuditValueDetail(id)),
  }
})

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
    'admin.usage.requestAudit.valueDetail.title': 'Short-term value detail',
    'admin.usage.requestAudit.valueDetail.absent': 'No short-term value detail was collected for this request.',
    'admin.usage.requestAudit.valueDetail.unavailable':
      'The short-term value detail could not be read. This is not a statement that nothing was collected.',
    'admin.usage.requestAudit.valueDetail.expiresAt': 'Values readable until',
    'admin.usage.requestAudit.valueDetail.disabled':
      'Short-term value capture is off for this instance, so values are not being collected.',
    'admin.usage.requestAudit.valueDetail.notice':
      'Header values, parsed client identifiers and the model name below are decrypted only when requested, are readable for a few days, and are never cached. Credentials, cookies and request bodies are never collected here.',
    'admin.usage.requestAudit.valueDetail.reveal': 'Reveal short-term values',
    'admin.usage.requestAudit.valueDetail.revealing': 'Revealing…',
    'admin.usage.requestAudit.valueDetail.revealFailed': 'The short-term values could not be revealed',
    'admin.usage.requestAudit.valueDetail.truncated':
      'Some submitted entries were not accepted, so this view may not list everything the client sent.',
    'admin.usage.requestAudit.valueDetail.validationDropped':
      "Some retained values did not pass this view's validation, so it may not list everything the record holds.",
    'admin.usage.requestAudit.valueDetail.model': 'Model',
    'admin.usage.requestAudit.valueDetail.inbound': 'Inbound values',
    'admin.usage.requestAudit.valueDetail.deviceId': 'Device ID',
    'admin.usage.requestAudit.valueDetail.accountUuid': 'Account UUID',
    'admin.usage.requestAudit.valueDetail.sessionId': 'Session ID',
    'admin.usage.requestAudit.valueDetail.latencyMs': 'Attempt latency',
    'admin.usage.requestAudit.valueDetail.proxyId': 'Proxy ID',
    'admin.usage.requestAudit.valueDetail.requestHeaders': 'Request header values',
    'admin.usage.requestAudit.valueDetail.responseHeaders': 'Response header values',
    'admin.usage.requestAudit.valueDetail.unknown': 'No value was recorded',
    'admin.usage.requestAudit.valueDetail.states.notObserved': 'Not collected',
    'admin.usage.requestAudit.valueDetail.states.stored': 'Values available',
    'admin.usage.requestAudit.valueDetail.states.skipped': 'Not retained',
    'admin.usage.requestAudit.valueDetail.states.expired': 'Values expired',
    'admin.usage.requestAudit.valueDetail.states.purged': 'Values cleared',
    'admin.usage.requestAudit.valueDetail.reasons.notObserved': 'No short-term value was observed for this request',
    'admin.usage.requestAudit.valueDetail.reasons.retained': 'Short-term values retained',
    'admin.usage.requestAudit.valueDetail.reasons.skippedOutOfScope':
      'This request did not call an Anthropic upstream',
    'admin.usage.requestAudit.valueDetail.reasons.skippedRetentionDisabled':
      'Short-term value retention is disabled',
    'admin.usage.requestAudit.valueDetail.reasons.skippedEncryptionUnavailable': 'Encryption was unavailable',
    'admin.usage.requestAudit.valueDetail.reasons.skippedInvalidValues':
      'The collected values did not pass validation',
    'admin.usage.requestAudit.valueDetail.reasons.skippedTooManyAttempts':
      'The request had more upstream attempts than can be recorded',
    'admin.usage.requestAudit.valueDetail.reasons.unknown': 'Unknown retention outcome',
    'stepUp.notEnabled': 'Enable two-factor authentication on your profile first',
    'stepUp.adminApiKeyForbidden': 'Admin API keys cannot perform this operation',
  }
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => labels[key] ?? key }),
  }
})

/**
 * Stand-in for the real TOTP dialog, which needs a Pinia store and the API
 * client. It renders only while the controller says the prompt is open and keeps
 * the controller reachable through its props, so the tests drive the real
 * `useStepUp` flow (prompt, verify, cancel, blocked) the drawer actually wires.
 */
const TotpStepUpDialogStub = {
  name: 'TotpStepUpDialog',
  props: { controller: { type: Object, required: true } },
  template: '<div v-if="controller.visible.value" data-testid="totp-step-up-dialog" />',
}

/** The controller the drawer handed to the (stubbed) TOTP dialog. */
function stepUpController(wrapper: any) {
  return wrapper.findComponent(TotpStepUpDialogStub).props('controller')
}

describe('UsageRequestAuditDrawer', () => {
  beforeEach(() => {
    mocks.getRequestAudit.mockReset()
    mocks.getRequestAuditValueDetail.mockReset()
    mocks.revealRequestAuditValueDetail.mockReset()
    // "No row for this usage log" is the ordinary outcome for a non-Messages
    // request, so it is the default here; individual tests opt into an envelope.
    // The shape is the one the API client rejects with: a bare Error would be an
    // unattributable failure, not a 404.
    mocks.getRequestAuditValueDetail.mockRejectedValue({ status: 404, message: 'not found' })
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

/**
 * The short-term value detail is the narrow exception to "an audit row never
 * holds header values, model names or parsed identifiers". These tests pin the
 * contract that keeps it an exception: the envelope is metadata and loads with
 * the audit, the values are only ever fetched by an explicit click, and whatever
 * comes back is re-validated before it can reach the DOM.
 */
describe('UsageRequestAuditDrawer value detail', () => {
  const VALUE_DETAIL_ENVELOPE = {
    usage_log_id: 51,
    state: 'stored',
    reason: 'retained',
    route: '/v1/messages',
    protocol: 'anthropic.messages',
    client_status: 200,
    attempt_count: 2,
    entry_count: 6,
    payload_bytes: 512,
    started_at: '2026-09-24T00:00:00Z',
    completed_at: '2026-09-24T00:00:02Z',
    expires_at: '2999-01-01T00:00:00Z',
    created_at: '2026-09-24T00:00:03Z',
    capability_enabled: true,
  }

  const audit = (usageLogId: number) => ({
    usage_log_id: usageLogId,
    headers: {},
    events: [],
    attempts: [],
    capture_completeness: 'complete',
  })

  const mountValueDetailDrawer = (usageLogId = 51) => {
    mocks.getRequestAudit.mockResolvedValue(audit(usageLogId))
    return shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId },
      global: {
        stubs: {
          BaseDialog: { template: '<div><slot /></div>' },
          Icon: true,
          TotpStepUpDialog: TotpStepUpDialogStub,
        },
      },
    })
  }

  const REVEAL_SELECTOR = '[data-testid="request-audit-value-detail-reveal"]'
  const VALUES_SELECTOR = '[data-testid="request-audit-value-detail-values"]'

  beforeEach(() => {
    mocks.getRequestAuditValueDetail.mockReset()
    mocks.revealRequestAuditValueDetail.mockReset()
  })

  it('loads only the metadata envelope on open and never fetches values by itself', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)

    const wrapper = mountValueDetailDrawer()
    await flushPromises()

    expect(mocks.getRequestAuditValueDetail).toHaveBeenCalledWith(51)
    expect(mocks.revealRequestAuditValueDetail).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="request-audit-value-detail-state"]').text()).toBe('Values available')
    expect(wrapper.find(VALUES_SELECTOR).exists()).toBe(false)
    expect(wrapper.text()).toContain('decrypted only when requested')
  })

  it('reveals the values only after a click and renders them as text, never as markup', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockResolvedValue({
      usage_log_id: 51,
      values: {
        model: '<img src=x onerror="alert(1)">claude-sonnet-4-5',
        inbound: {
          request_headers: { 'User-Agent': ['claude-cli/2.0.0 (external, cli)'] },
          device_id: 'dev-1',
          session_id: '11111111-2222-3333-4444-555555555555',
        },
        attempts: [
          {
            index: 1,
            account_id: 88,
            model: 'claude-sonnet-4-5',
            upstream_status: 200,
            request_headers: { 'Anthropic-Beta': ['token-counting-2024-11-01'] },
            response_headers: { 'Retry-After': ['12'] },
            session_id: '11111111-2222-3333-4444-555555555555',
          },
        ],
      },
    })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()

    expect(wrapper.find(VALUES_SELECTOR).exists()).toBe(false)
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    expect(mocks.revealRequestAuditValueDetail).toHaveBeenCalledWith(51)
    const values = wrapper.get(VALUES_SELECTOR)
    // The model name is untrusted free text: it is shown verbatim as text.
    expect(values.text()).toContain('<img src=x onerror="alert(1)">claude-sonnet-4-5')
    expect(values.find('img').exists()).toBe(false)
    expect(values.text()).toContain('claude-cli/2.0.0 (external, cli)')
    expect(values.text()).toContain('dev-1')
    expect(values.text()).toContain('token-counting-2024-11-01')
    expect(values.findAll('[data-testid="request-audit-value-detail-attempt"]')).toHaveLength(1)
    // A legitimate beta value contains "token" but is not treated as a credential.
    expect(wrapper.find('[data-testid="request-audit-value-detail-truncated"]').exists()).toBe(false)
  })

  it('renders a measured attempt latency and proxy id alongside the parsed identifiers', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockResolvedValue({
      usage_log_id: 51,
      values: {
        inbound: { request_headers: {} },
        attempts: [
          {
            index: 1,
            account_id: 88,
            upstream_status: 200,
            latency_ms: 1500,
            proxy_id: 4,
            request_headers: {},
            response_headers: {},
            session_id: '11111111-2222-3333-4444-555555555555',
          },
          { index: 2, latency_ms: 0, request_headers: {}, response_headers: {} },
        ],
      },
    })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    const attempts = wrapper.findAll('[data-testid="request-audit-value-detail-attempt"]')
    expect(attempts).toHaveLength(2)
    expect(attempts[0].text()).toContain('Attempt latency1500 ms')
    expect(attempts[0].text()).toContain('Proxy ID4')
    expect(attempts[0].text()).toContain('Session ID11111111-2222-3333-4444-555555555555')
    // A measured zero is a measurement, and the absent proxy id stays absent.
    expect(attempts[1].text()).toContain('Attempt latency0 ms')
    expect(attempts[1].text()).not.toContain('Proxy ID')
  })

  it('never renders an attempt latency or proxy id the reveal boundary rejected', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockResolvedValue({
      usage_log_id: 51,
      values: {
        inbound: { request_headers: {} },
        attempts: [
          {
            index: 1,
            // Beyond the backend's 24-hour ceiling, and a zero id that is not an id.
            latency_ms: 24 * 60 * 60 * 1000 + 1,
            proxy_id: 0,
            request_headers: {},
            response_headers: {},
          },
        ],
      },
    })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    const attempt = wrapper.get('[data-testid="request-audit-value-detail-attempt"]')
    expect(attempt.text()).not.toContain('Attempt latency')
    expect(attempt.text()).not.toContain('Proxy ID')
    expect(attempt.text()).not.toContain(String(24 * 60 * 60 * 1000 + 1))
  })

  it('renders the truncated marker instead of claiming the view is complete', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockResolvedValue({
      usage_log_id: 51,
      values: { inbound: { request_headers: {} }, attempts: [], truncated: true },
    })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="request-audit-value-detail-truncated"]').text()).toContain(
      'may not list everything the client sent',
    )
    expect(wrapper.get('[data-testid="request-audit-value-detail-inbound"]').text()).toContain(
      'No value was recorded',
    )
  })

  /**
   * The reveal payload is re-validated at this boundary, but only with the
   * backend's own rules: a value that merely *contains* a credential word is a
   * legitimate opaque id or header value the backend retained. Dropping it would
   * make the view silently disagree with what was actually collected, so this
   * decoder refuses only the unambiguous credential prefixes and reports every
   * entry it does drop.
   */
  it('keeps a legitimate opaque id and header value that merely contain a credential word', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockResolvedValue({
      usage_log_id: 51,
      values: {
        model: 'vendor-secret-key-model',
        inbound: {
          request_headers: { Host: ['secret.internal'] },
          device_id: 'device-secret-9f2c',
        },
        attempts: [],
      },
    })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    const values = wrapper.get(VALUES_SELECTOR)
    expect(values.text()).toContain('vendor-secret-key-model')
    expect(values.text()).toContain('device-secret-9f2c')
    expect(values.text()).toContain('secret.internal')
    // Nothing was refused, so nothing claims the view is incomplete.
    expect(wrapper.find('[data-testid="request-audit-value-detail-validation-dropped"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="request-audit-value-detail-truncated"]').exists()).toBe(false)
  })

  it('warns when the boundary refused an entry the reveal payload carried', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockResolvedValue({
      usage_log_id: 51,
      values: {
        inbound: {
          request_headers: { 'X-Unknown-Header': ['canary'], Accept: ['application/json'] },
        },
        attempts: [],
      },
    })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    expect(
      wrapper.get('[data-testid="request-audit-value-detail-validation-dropped"]').text(),
    ).toContain("did not pass this view's validation")
    // The refused entry is gone, the rest of the view is still usable, and the
    // server's own truncation statement stays a separate fact from this one.
    expect(wrapper.html()).not.toContain('X-Unknown-Header')
    expect(wrapper.get(VALUES_SELECTOR).text()).toContain('application/json')
    expect(wrapper.find('[data-testid="request-audit-value-detail-truncated"]').exists()).toBe(false)
  })

  /**
   * A value detail is readable for 7 days. A drawer opened before the deadline can
   * outlive it, so the client must not keep showing `stored`, offering a read the
   * server will refuse, or holding decrypted plaintext past the window. These tests
   * drive the clock with fake timers and leave `setImmediate` real so the usual
   * promise flushing keeps working.
   */
  const FAKE_TIMERS = { toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] } as const
  const STATE_SELECTOR = '[data-testid="request-audit-value-detail-state"]'

  it('clears the revealed plaintext and stops offering a read when the window closes while the drawer stays open', async () => {
    vi.useFakeTimers(FAKE_TIMERS)
    try {
      vi.setSystemTime(new Date('2026-09-24T00:00:00Z'))
      mocks.getRequestAuditValueDetail.mockResolvedValue({
        ...VALUE_DETAIL_ENVELOPE,
        expires_at: '2026-09-24T00:01:00Z',
      })
      mocks.revealRequestAuditValueDetail.mockResolvedValue({
        usage_log_id: 51,
        values: {
          inbound: { request_headers: { Accept: ['plaintext-canary'] } },
          attempts: [],
        },
      })

      const wrapper = mountValueDetailDrawer()
      await flushPromises()
      await wrapper.get(REVEAL_SELECTOR).trigger('click')
      await flushPromises()

      expect(wrapper.get(VALUES_SELECTOR).text()).toContain('plaintext-canary')
      expect(wrapper.get(STATE_SELECTOR).text()).toBe('Values available')

      vi.advanceTimersByTime(60_001)
      await flushPromises()

      expect(wrapper.text()).not.toContain('plaintext-canary')
      expect(wrapper.find(VALUES_SELECTOR).exists()).toBe(false)
      expect(wrapper.get(STATE_SELECTOR).text()).toBe('Values expired')
      expect(wrapper.find(REVEAL_SELECTOR).exists()).toBe(false)
      wrapper.unmount()
    } finally {
      vi.useRealTimers()
    }
  })

  it('discards a reveal that resolves after the window closed instead of rendering it', async () => {
    vi.useFakeTimers(FAKE_TIMERS)
    try {
      vi.setSystemTime(new Date('2026-09-24T00:00:00Z'))
      let resolveReveal!: (value: unknown) => void
      mocks.getRequestAuditValueDetail.mockResolvedValue({
        ...VALUE_DETAIL_ENVELOPE,
        expires_at: '2026-09-24T00:01:00Z',
      })
      mocks.revealRequestAuditValueDetail.mockReturnValue(
        new Promise((resolve) => {
          resolveReveal = resolve
        }),
      )

      const wrapper = mountValueDetailDrawer()
      await flushPromises()
      await wrapper.get(REVEAL_SELECTOR).trigger('click')

      vi.advanceTimersByTime(60_001)
      await flushPromises()

      resolveReveal({
        usage_log_id: 51,
        values: {
          inbound: { request_headers: { Accept: ['late-plaintext-canary'] } },
          attempts: [],
        },
      })
      await flushPromises()

      expect(wrapper.text()).not.toContain('late-plaintext-canary')
      expect(wrapper.find(VALUES_SELECTOR).exists()).toBe(false)
      expect(wrapper.get(STATE_SELECTOR).text()).toBe('Values expired')
      wrapper.unmount()
    } finally {
      vi.useRealTimers()
    }
  })

  it('does not expire the row now on screen when the previous row’s window closes', async () => {
    vi.useFakeTimers(FAKE_TIMERS)
    try {
      vi.setSystemTime(new Date('2026-09-24T00:00:00Z'))
      mocks.getRequestAuditValueDetail.mockResolvedValue({
        ...VALUE_DETAIL_ENVELOPE,
        expires_at: '2026-09-24T00:01:00Z',
      })
      mocks.revealRequestAuditValueDetail.mockResolvedValue({
        usage_log_id: 52,
        values: {
          inbound: { request_headers: { Accept: ['row-52-canary'] } },
          attempts: [],
        },
      })

      const wrapper = mountValueDetailDrawer()
      await flushPromises()

      mocks.getRequestAuditValueDetail.mockResolvedValue({
        ...VALUE_DETAIL_ENVELOPE,
        usage_log_id: 52,
        expires_at: '2999-01-01T00:00:00Z',
      })
      await wrapper.setProps({ usageLogId: 52 })
      await flushPromises()
      await wrapper.get(REVEAL_SELECTOR).trigger('click')
      await flushPromises()

      vi.advanceTimersByTime(60_001)
      await flushPromises()

      expect(wrapper.get(VALUES_SELECTOR).text()).toContain('row-52-canary')
      expect(wrapper.get(STATE_SELECTOR).text()).toBe('Values available')
      wrapper.unmount()
    } finally {
      vi.useRealTimers()
    }
  })

  it('never renders a credential, a cookie or a non-allowlisted header from a tampered payload', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockResolvedValue({
      usage_log_id: 51,
      values: {
        inbound: {
          request_headers: {
            Authorization: ['Bearer sk-canary'],
            Cookie: ['session=canary'],
            'X-Api-Key': ['sk-canary'],
            'X-Unknown-Header': ['canary'],
            // An allowlisted name carrying a credential-shaped value.
            'User-Agent': ['sk-canary'],
            // A legitimate multi-value header is kept as a list.
            'Accept-Language': ['en-US', 'zh-CN'],
          },
          device_id: 'sk-canary-device',
        },
        attempts: [
          {
            index: 1,
            request_headers: { 'X-Api-Key': ['sk-canary'] },
            response_headers: { 'Set-Cookie': ['session=canary'], 'Retry-After': ['30'] },
          },
        ],
        body_text: 'BODY_CANARY_DO_NOT_RENDER',
        authorization: 'Bearer canary',
      },
    })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    const html = wrapper.html()
    expect(html).not.toContain('sk-canary')
    expect(html).not.toContain('Bearer')
    expect(html).not.toContain('session=canary')
    expect(html).not.toContain('BODY_CANARY_DO_NOT_RENDER')
    expect(html).not.toContain('X-Unknown-Header')
    expect(html).not.toContain('Authorization')
    expect(html).not.toContain('Cookie')
    expect(html).not.toContain('X-Api-Key')
    // The allowlisted, innocuous values still come through.
    expect(wrapper.get(VALUES_SELECTOR).text()).toContain('en-US, zh-CN')
    expect(wrapper.get(VALUES_SELECTOR).text()).toContain('30')
  })

  it('offers no reveal for values that are expired, purged, skipped or not collected', async () => {
    const envelopes = [
      { ...VALUE_DETAIL_ENVELOPE, state: 'expired', expires_at: '2026-09-25T00:00:00Z' },
      { ...VALUE_DETAIL_ENVELOPE, state: 'purged', expires_at: '2026-09-25T00:00:00Z' },
      { ...VALUE_DETAIL_ENVELOPE, state: 'skipped', reason: 'skipped_encryption_unavailable', expires_at: undefined },
      { ...VALUE_DETAIL_ENVELOPE, state: 'not_observed', reason: 'not_observed', expires_at: undefined },
      { ...VALUE_DETAIL_ENVELOPE, state: 'stored', expires_at: '2020-01-01T00:00:00Z' },
    ]

    for (const envelope of envelopes) {
      mocks.getRequestAuditValueDetail.mockResolvedValue(envelope)

      const wrapper = mountValueDetailDrawer()
      await flushPromises()

      expect(wrapper.find(REVEAL_SELECTOR).exists()).toBe(false)
      wrapper.unmount()
    }

    mocks.getRequestAuditValueDetail.mockResolvedValue({ ...VALUE_DETAIL_ENVELOPE, state: 'stored', expires_at: '2020-01-01T00:00:00Z' })
    const expired = mountValueDetailDrawer()
    await flushPromises()
    expect(expired.get('[data-testid="request-audit-value-detail-state"]').text()).toBe('Values expired')
  })

  it('reports the capability as off without offering a reveal', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue({
      ...VALUE_DETAIL_ENVELOPE,
      state: 'not_observed',
      reason: 'skipped_value_retention_disabled',
      expires_at: undefined,
      capability_enabled: false,
    })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()

    expect(wrapper.get('[data-testid="request-audit-value-detail-disabled"]').text()).toContain(
      'Short-term value capture is off',
    )
    expect(wrapper.find(REVEAL_SELECTOR).exists()).toBe(false)
    expect(mocks.revealRequestAuditValueDetail).not.toHaveBeenCalled()
  })

  it('reports a 404 envelope as not collected rather than inventing a state', async () => {
    mocks.getRequestAuditValueDetail.mockRejectedValue({ status: 404, message: 'not found' })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()

    expect(wrapper.get('[data-testid="request-audit-value-detail-absent"]').text()).toContain(
      'No short-term value detail was collected',
    )
    expect(wrapper.find('[data-testid="request-audit-value-detail-state"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="request-audit-value-detail-unavailable"]').exists()).toBe(false)
    expect(wrapper.find(REVEAL_SELECTOR).exists()).toBe(false)
  })

  it.each([
    ['a storage failure', { status: 500, message: 'storage failed' }],
    ['a temporarily unavailable service', { status: 503, message: 'unavailable' }],
    ['a conflict the client cannot read as absence', { status: 409, message: 'not retained' }],
    ['a request that never reached the server', new Error('Network Error')],
  ])('reports %s as unreadable, never as not collected', async (_description, failure) => {
    mocks.getRequestAuditValueDetail.mockRejectedValue(failure)

    const wrapper = mountValueDetailDrawer()
    await flushPromises()

    const unavailable = wrapper.get('[data-testid="request-audit-value-detail-unavailable"]')
    expect(unavailable.text()).toContain('could not be read')
    // An unreadable envelope must not claim a collection outcome the server never sent.
    expect(wrapper.find('[data-testid="request-audit-value-detail-absent"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="request-audit-value-detail-state"]').exists()).toBe(false)
    // And no reveal may be offered from a state nobody read.
    expect(wrapper.find(REVEAL_SELECTOR).exists()).toBe(false)
    expect(mocks.revealRequestAuditValueDetail).not.toHaveBeenCalled()
  })

  it('clears the unreadable state when the drawer moves to another usage log', async () => {
    mocks.getRequestAuditValueDetail.mockRejectedValueOnce({ status: 503, message: 'unavailable' })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    expect(wrapper.find('[data-testid="request-audit-value-detail-unavailable"]').exists()).toBe(true)

    mocks.getRequestAuditValueDetail.mockResolvedValue({ ...VALUE_DETAIL_ENVELOPE, usage_log_id: 52 })
    await wrapper.setProps({ usageLogId: 52 })
    await flushPromises()

    expect(wrapper.find('[data-testid="request-audit-value-detail-unavailable"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="request-audit-value-detail-state"]').text()).toBe('Values available')

    // A stale refusal for the previous row must not come back as absence either.
    expect(wrapper.find('[data-testid="request-audit-value-detail-absent"]').exists()).toBe(false)
  })

  it('clears the unreadable state when the drawer closes and reopens on a readable row', async () => {
    mocks.getRequestAuditValueDetail.mockRejectedValueOnce({ status: 500, message: 'storage failed' })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    expect(wrapper.find('[data-testid="request-audit-value-detail-unavailable"]').exists()).toBe(true)

    await wrapper.setProps({ show: false })
    await flushPromises()
    expect(wrapper.find('[data-testid="request-audit-value-detail-unavailable"]').exists()).toBe(false)

    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    await wrapper.setProps({ show: true })
    await flushPromises()

    expect(wrapper.find('[data-testid="request-audit-value-detail-unavailable"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="request-audit-value-detail-state"]').text()).toBe('Values available')
  })

  it('rejects an envelope whose state is not a known literal', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue({ ...VALUE_DETAIL_ENVELOPE, state: 'leaked' })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()

    expect(wrapper.find('[data-testid="request-audit-value-detail-state"]').exists()).toBe(false)
    // An envelope that arrived but did not match the contract is unreadable, not a
    // collection result: only the backend's own 404 says nothing was collected.
    expect(wrapper.get('[data-testid="request-audit-value-detail-unavailable"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="request-audit-value-detail-absent"]').exists()).toBe(false)
    expect(wrapper.find(REVEAL_SELECTOR).exists()).toBe(false)
    expect(wrapper.text()).not.toContain('leaked')
  })

  it('keeps the failure visible while the values are still retained, and re-reads the envelope otherwise', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockRejectedValue(new Error('410 gone'))

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    // Still stored and unexpired: the refusal is a real error worth showing.
    expect(wrapper.get('[data-testid="request-audit-value-detail-reveal-failed"]').exists()).toBe(true)
    expect(mocks.getRequestAuditValueDetail).toHaveBeenCalledTimes(2)

    wrapper.unmount()

    // The envelope now says the values are gone: the refusal is the expected
    // outcome, so the error is not kept and no reveal is offered.
    mocks.getRequestAuditValueDetail.mockReset()
    mocks.getRequestAuditValueDetail
      .mockResolvedValueOnce(VALUE_DETAIL_ENVELOPE)
      .mockResolvedValue({ ...VALUE_DETAIL_ENVELOPE, state: 'expired', expires_at: '2020-01-01T00:00:00Z' })

    const gone = mountValueDetailDrawer()
    await flushPromises()
    await gone.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    expect(gone.find('[data-testid="request-audit-value-detail-reveal-failed"]').exists()).toBe(false)
    expect(gone.find(VALUES_SELECTOR).exists()).toBe(false)
  })

  it('drops revealed values when the drawer closes or moves to another usage log', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockResolvedValue({
      usage_log_id: 51,
      values: {
        inbound: { request_headers: { Accept: ['application/json'] } },
        attempts: [],
      },
    })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()
    expect(wrapper.get(VALUES_SELECTOR).text()).toContain('application/json')

    await wrapper.setProps({ usageLogId: 52 })
    await flushPromises()
    expect(wrapper.find(VALUES_SELECTOR).exists()).toBe(false)

    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()
    await wrapper.setProps({ show: false })
    await flushPromises()

    expect(wrapper.find(VALUES_SELECTOR).exists()).toBe(false)
    expect(mocks.revealRequestAuditValueDetail).toHaveBeenCalledTimes(2)
  })

  it('ignores a reveal that resolves after the row it was requested for is gone', async () => {
    let resolveReveal!: (value: unknown) => void
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockReturnValue(
      new Promise((resolve) => {
        resolveReveal = resolve
      }),
    )

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')

    await wrapper.setProps({ usageLogId: 77 })
    await flushPromises()

    resolveReveal({
      usage_log_id: 51,
      values: {
        inbound: { request_headers: { Accept: ['late-canary'] } },
        attempts: [],
      },
    })
    await flushPromises()

    expect(wrapper.text()).not.toContain('late-canary')
  })

  /**
   * The reveal route is step-up gated server-side. A STEP_UP_REQUIRED is a prompt,
   * not a failure: the admin verifies and the very same explicit POST is retried
   * once. Nothing here may auto-run: the first POST is still only issued by the
   * click, and the retry is issued by the verification.
   */
  const STEP_UP_REQUIRED = { status: 403, code: 'STEP_UP_REQUIRED', message: 'recent verification required' }
  const TOTP_DIALOG_SELECTOR = '[data-testid="totp-step-up-dialog"]'
  const BLOCKED_SELECTOR = '[data-testid="request-audit-value-detail-reveal-blocked"]'
  const FAILED_SELECTOR = '[data-testid="request-audit-value-detail-reveal-failed"]'

  it('prompts for step-up on a refused reveal and retries it once after verification', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail
      .mockRejectedValueOnce(STEP_UP_REQUIRED)
      .mockResolvedValueOnce({
        usage_log_id: 51,
        values: { inbound: { request_headers: { Accept: ['application/json'] } }, attempts: [] },
      })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(false)

    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    // The refusal asks for verification instead of reporting a failed reveal.
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(true)
    expect(stepUpController(wrapper).visible.value).toBe(true)
    expect(wrapper.find(FAILED_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(BLOCKED_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(VALUES_SELECTOR).exists()).toBe(false)
    expect(mocks.revealRequestAuditValueDetail).toHaveBeenCalledTimes(1)
    // A refused reveal is not a retention change, so the envelope is not re-read.
    expect(mocks.getRequestAuditValueDetail).toHaveBeenCalledTimes(1)

    stepUpController(wrapper).onVerified()
    await flushPromises()

    expect(mocks.revealRequestAuditValueDetail).toHaveBeenCalledTimes(2)
    expect(wrapper.get(VALUES_SELECTOR).text()).toContain('application/json')
    expect(wrapper.find(FAILED_SELECTOR).exists()).toBe(false)
  })

  it('shows no failure and drops the prompt when the step-up verification is cancelled', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockRejectedValue(STEP_UP_REQUIRED)

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(true)

    stepUpController(wrapper).onCancel()
    await flushPromises()

    // Cancelling is not a failure and reads nothing: no error text, no values.
    expect(wrapper.find(FAILED_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(BLOCKED_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(VALUES_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(false)
    expect(mocks.revealRequestAuditValueDetail).toHaveBeenCalledTimes(1)

    // The action is available again, so the admin can retry deliberately.
    expect(wrapper.get(REVEAL_SELECTOR).attributes('disabled')).toBeUndefined()
  })

  it.each([
    ['STEP_UP_TOTP_NOT_ENABLED', 'Enable two-factor authentication on your profile first'],
    ['STEP_UP_ADMIN_API_KEY_FORBIDDEN', 'Admin API keys cannot perform this operation'],
  ])('reports %s with the shared step-up wording instead of a reveal failure', async (code, expected) => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockRejectedValue({ status: 403, code, message: 'refused' })

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    expect(wrapper.get(BLOCKED_SELECTOR).text()).toBe(expected)
    expect(wrapper.find(FAILED_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(VALUES_SELECTOR).exists()).toBe(false)
    expect(mocks.revealRequestAuditValueDetail).toHaveBeenCalledTimes(1)
  })

  it('discards a step-up retry that resolves after the drawer moved to another usage log', async () => {
    let resolveRetry!: (value: unknown) => void
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail
      .mockRejectedValueOnce(STEP_UP_REQUIRED)
      .mockReturnValueOnce(
        new Promise((resolve) => {
          resolveRetry = resolve
        }),
      )

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    stepUpController(wrapper).onVerified()
    await flushPromises()
    expect(mocks.revealRequestAuditValueDetail).toHaveBeenCalledTimes(2)

    mocks.getRequestAuditValueDetail.mockResolvedValue({ ...VALUE_DETAIL_ENVELOPE, usage_log_id: 77 })
    await wrapper.setProps({ usageLogId: 77 })
    await flushPromises()

    resolveRetry({
      usage_log_id: 51,
      values: { inbound: { request_headers: { Accept: ['late-step-up-canary'] } }, attempts: [] },
    })
    await flushPromises()

    expect(wrapper.text()).not.toContain('late-step-up-canary')
    expect(wrapper.find(VALUES_SELECTOR).exists()).toBe(false)
  })

  it('dismisses an open step-up prompt when the drawer closes', async () => {
    mocks.getRequestAuditValueDetail.mockResolvedValue(VALUE_DETAIL_ENVELOPE)
    mocks.revealRequestAuditValueDetail.mockRejectedValue(STEP_UP_REQUIRED)

    const wrapper = mountValueDetailDrawer()
    await flushPromises()
    await wrapper.get(REVEAL_SELECTOR).trigger('click')
    await flushPromises()
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(true)

    await wrapper.setProps({ show: false })
    await flushPromises()

    // A prompt left over from a closed drawer would float over the list and its
    // retry would target a row nobody is looking at.
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(false)
    expect(stepUpController(wrapper).visible.value).toBe(false)
    expect(mocks.revealRequestAuditValueDetail).toHaveBeenCalledTimes(1)
    expect(wrapper.find(FAILED_SELECTOR).exists()).toBe(false)
  })
})
