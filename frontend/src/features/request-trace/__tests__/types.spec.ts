import { describe, expect, it } from 'vitest'
import {
  normalizeRequestTraceDetail,
  normalizeRequestTraceOperatorStatus,
  normalizeRequestTraceStageDecision,
  normalizeRequestTraceStageFacts,
  requestTraceCaptureStates,
  requestTraceDecisionKinds,
  requestTraceDecisionOutcomes,
  requestTraceDecisionSources,
  requestTraceDecisionStage,
  requestTraceStageNames,
  requestTraceStageReasons,
  requestTraceStageStates,
  requestTraceStageViews,
} from '../types'

const status = {
  enabled: true,
  risk_acknowledged: true,
  capture_allowed: false,
  risk_version: 'v2026.09.28',
  risk_phrase_en: 'New statement',
  risk_phrase_zh: '新版语句',
  risk_acknowledgement_current: false,
  plaintext_capture_supported: false,
  plaintext_capture_support_reason: 'unsupported_missing_ownership_foreign_key',
  risk_acknowledgement: {
    version: 'v2026.09.27', admin_user_id: 9, accepted_at: '2026-09-28T00:00:00Z',
    ip_address: '192.0.2.1', user_agent: 'CANARY_AGENT', phrase: 'CANARY_PHRASE',
  },
}

describe('operator status boundary', () => {
  it('retains only the server gate verdict and safe acknowledgement metadata', () => {
    const normalized = normalizeRequestTraceOperatorStatus({ ...status, body: 'CANARY_BODY', headers: { Authorization: 'CANARY_KEY' } })
    expect(normalized).toMatchObject({ enabled: true, capture_allowed: false, plaintext_capture_supported: false })
    expect(JSON.stringify(normalized)).not.toContain('CANARY_BODY')
    expect(JSON.stringify(normalized)).not.toContain('CANARY_KEY')
    expect(JSON.stringify(normalized)).not.toContain('CANARY_AGENT')
    expect(JSON.stringify(normalized)).not.toContain('CANARY_PHRASE')
  })

  it('rejects unknown deployment verdicts rather than falsely claiming capture is off', () => {
    expect(() => normalizeRequestTraceOperatorStatus({ ...status, plaintext_capture_support_reason: 'unsupported_new_unknown' })).toThrow()
  })
})

const attemptFacts = {
  method: 'POST',
  url: 'https://api.example.com/v1/messages?api_key=%5BREDACTED%5D',
  request_headers: { Authorization: ['[REDACTED]'], 'X-Trace': ['first', 'second'] },
  request_headers_omitted: 2,
  response_headers: { 'Content-Type': ['application/json'] },
  response_headers_omitted: 0,
  account_id: 42,
  model: 'claude-sonnet-4-5',
  protocol: 'messages',
  value_protocol: 'anthropic',
  status: 200,
  // Exactly the precision Go's RFC3339Nano emits for a UTC attempt.
  started_at: '2026-09-28T00:00:00.123456789Z',
  ended_at: '2026-09-28T00:00:01.987654321Z',
}

function traceDetail(stage: Record<string, unknown> = {}) {
  return {
    trace_id: 'a'.repeat(32), route_family: 'messages', inbound_endpoint: '/v1/messages',
    created_at: '2026-09-28T00:00:00Z', completed_at: '2026-09-28T00:00:01Z',
    client_status: 200, capture_state: 'partial', usage_log_id: null, cleanup_after: null,
    stages: [{
      ordinal: 1, stage: 'wire_attempt', attempt_index: 0, view_name: '', state: 'not_observed',
      reason: 'wire_observed', observed_bytes: 0, retained_bytes: 0, dropped_events: 0,
      redaction_unverified: false, ...stage,
    }],
  }
}

describe('stage facts boundary', () => {
  it('normalizes typed redacted facts and keeps credential placeholders verbatim', () => {
    const facts = normalizeRequestTraceStageFacts('wire_attempt', attemptFacts)
    expect(facts).toEqual({
      method: 'POST', url: attemptFacts.url, url_omitted: false,
      request_headers: { Authorization: ['[REDACTED]'], 'X-Trace': ['first', 'second'] },
      request_headers_omitted: 2,
      response_headers: { 'Content-Type': ['application/json'] },
      response_headers_omitted: 0,
      account_id: 42, model: 'claude-sonnet-4-5', protocol: 'messages', value_protocol: 'anthropic',
      status: 200, started_at: attemptFacts.started_at, ended_at: attemptFacts.ended_at,
    })
    expect(Object.keys(facts.request_headers)).toEqual(['Authorization', 'X-Trace'])
  })

  it('keeps the Go http.Header map contract and refuses any other header shape', () => {
    expect(normalizeRequestTraceStageFacts('wire_attempt', { request_headers: {} }).request_headers).toEqual({})
    expect(normalizeRequestTraceStageFacts('wire_attempt', { request_headers: { 'X-One': ['a'] } }).request_headers).toEqual({ 'X-One': ['a'] })
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers: [{ name: 'X-One', values: ['a'] }] })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers: { 'X-One': { value: 'a' } } })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers: { 'X-One': [7] } })).toThrow()
  })

  it('treats a header named __proto__ as data, not as a prototype', () => {
    // Exactly what the wire delivers: a JSON object, where __proto__ is an own key.
    const wire = JSON.parse('{"request_headers":{"__proto__":["[REDACTED]"]}}') as never
    const facts = normalizeRequestTraceStageFacts('wire_attempt', wire)
    expect(Object.keys(facts.request_headers)).toEqual(['__proto__'])
    expect(facts.request_headers['__proto__']).toEqual(['[REDACTED]'])
    expect(Object.getPrototypeOf(facts.request_headers)).toBe(Object.prototype)
    expect(Object.getPrototypeOf({})).toBe(Object.prototype)
  })

  it('reports absent scalars as absent instead of observed empty values', () => {
    const facts = normalizeRequestTraceStageFacts('wire_attempt', {
      method: '', url_omitted: true, request_headers_omitted: 0, account_id: 0, status: 0,
    })
    expect(facts).toEqual({
      method: null, url: null, url_omitted: true, request_headers: {}, request_headers_omitted: 0,
      response_headers: {}, response_headers_omitted: 0, account_id: null, model: null, protocol: null,
      value_protocol: null, status: null, started_at: null, ended_at: null,
    })
    expect(Object.keys(facts)).toHaveLength(14)
  })

  it('refuses any fact key that is not part of the typed projection', () => {
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { metadata: { authorization: 'Bearer SECRET' } })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { ...attemptFacts, authorization: 'Bearer SECRET' })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { ...attemptFacts, method: null })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', 'Bearer SECRET')).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', ['Bearer SECRET'])).toThrow()
  })

  it('allows facts only on the stages the backend can attach them to', () => {
    expect(() => normalizeRequestTraceStageFacts('client_entry', attemptFacts)).toThrow()
    expect(() => normalizeRequestTraceStageFacts('', attemptFacts)).toThrow()
    expect(() => normalizeRequestTraceDetail(traceDetail({ stage: 'client_entry', facts: attemptFacts }))).toThrow()
    expect(normalizeRequestTraceDetail(traceDetail({ stage: 'wire_attempt', attempt_index: 1, facts: attemptFacts })).stages[0].facts?.status).toBe(200)
  })

  it('refuses response-side facts on the client metadata stage', () => {
    for (const key of ['response_headers', 'account_id', 'model', 'protocol', 'value_protocol', 'status', 'started_at', 'ended_at']) {
      expect(() => normalizeRequestTraceStageFacts('client_metadata', { method: 'POST', [key]: attemptFacts[key as keyof typeof attemptFacts] })).toThrow()
    }
    expect(normalizeRequestTraceStageFacts('client_metadata', { method: 'POST', request_headers: { Authorization: ['[REDACTED]'] } }).method).toBe('POST')
  })

  it('bounds the URL and refuses a URL that claims to be omitted at the same time', () => {
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { url: `https://api.example.com/${'a'.repeat(8192)}` })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { url: 'https://api.example.com/v1\nmessages' })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { url: 'https://api.example.com/v1', url_omitted: true })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { url_omitted: 'yes' })).toThrow()
  })

  it('bounds header names, values and counts', () => {
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers: { ['X'.repeat(129)]: ['v'] } })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers: { 'X Bad Name': ['v'] } })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers: { 'x-trace': ['v'] } })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers: { 'X-Trace': [] } })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers: { 'X-Trace': ['v', 'v', 'v', 'v', 'v', 'v', 'v', 'v', 'v'] } })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers: { 'X-Trace': ['v\nv'] } })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers: { 'X-Trace': ['v'.repeat(4097)] } })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers: { 'X-Trace': 'v' } })).toThrow()
    const many = Object.fromEntries(Array.from({ length: 129 }, (_value, index) => [`X${index}`, ['v']]))
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers: many })).toThrow()
    const boundary = Object.fromEntries(Array.from({ length: 128 }, (_value, index) => [`X${index}`, ['v']]))
    expect(Object.keys(normalizeRequestTraceStageFacts('wire_attempt', { request_headers: boundary }).request_headers)).toHaveLength(128)
  })

  it('bounds the encoded stage facts to the same budget as the server', () => {
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { url: `https://api.example.com/${'a'.repeat(4000)}` })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { request_headers_omitted: -1 })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { response_headers_omitted: 1.5 })).toThrow()
  })

  it('bounds scalars and ordering, and never keeps an ended time without a start', () => {
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { account_id: -1 })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { status: 99 })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { status: 600 })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { method: 'POST\nGET' })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { started_at: 'not a time' })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { ended_at: '2026-09-28T00:00:00Z' })).toThrow()
    expect(() => normalizeRequestTraceStageFacts('wire_attempt', { started_at: '2026-09-28T00:00:01Z', ended_at: '2026-09-28T00:00:00Z' })).toThrow()
  })

  it('keeps facts out of the detail unless the stage carries them, and never surfaces free-form metadata', () => {
    const withFacts = normalizeRequestTraceDetail(traceDetail({ metadata: { authorization: 'Bearer SECRET' }, facts: attemptFacts }))
    expect(withFacts.stages[0].facts?.method).toBe('POST')
    expect(JSON.stringify(withFacts)).not.toContain('Bearer SECRET')

    const withoutFacts = normalizeRequestTraceDetail(traceDetail({ metadata: { authorization: 'Bearer SECRET' } }))
    expect(withoutFacts.stages[0].facts).toBeUndefined()
    expect(JSON.stringify(withoutFacts)).not.toContain('Bearer SECRET')
  })
})

const decisionFacts = {
  decision: 'model_mapping',
  outcome: 'rewritten',
  source: 'group',
  sequence: 2,
  model_from: 'gpt-5.3-codex',
  model_to: 'gpt-5.3-codex-spark',
  protocol_from: 'messages',
  protocol_to: 'chat_completions',
  account_id: 42,
  decided_at: '2026-09-28T00:00:00.123456789Z',
}

/** A gateway decision is its own stage shape: it carries a decision record, never transport facts. */
function decisionDetail(decision: unknown = decisionFacts, stage: Record<string, unknown> = {}) {
  return traceDetail({ stage: 'gateway_decision', reason: 'decision_recorded', decision, ...stage })
}

describe('gateway decision stage boundary', () => {
  it('pins the closed sets to the backend contract', () => {
    // The drawer's label maps are keyed by these lists, so a change here is a
    // contract change that must be acknowledged on both sides.
    expect([...requestTraceDecisionKinds]).toEqual(['auth', 'route', 'model_mapping', 'account_switch', 'identity'])
    expect([...requestTraceDecisionOutcomes]).toEqual([
      'accepted', 'rejected', 'selected', 'unchanged', 'rewritten', 'not_sent', 'unsupported',
    ])
    expect([...requestTraceDecisionSources]).toEqual([
      'inbound', 'api_key', 'group', 'account', 'identity', 'protocol_convert',
    ])
  })

  it('accepts every value of each closed set, not only the sampled ones', () => {
    for (const decision of requestTraceDecisionKinds) {
      expect(normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, decision }).decision).toBe(decision)
    }
    for (const outcome of requestTraceDecisionOutcomes) {
      expect(normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, outcome }).outcome).toBe(outcome)
    }
    for (const source of requestTraceDecisionSources) {
      expect(normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, source }).source).toBe(source)
    }
  })

  it('normalizes a typed gateway decision and keeps every enum verbatim', () => {
    const decision = normalizeRequestTraceStageDecision('gateway_decision', decisionFacts)
    expect(decision).toEqual(decisionFacts)
    expect(Object.keys(decision)).toEqual([
      'decision', 'outcome', 'source', 'sequence', 'model_from', 'model_to',
      'protocol_from', 'protocol_to', 'account_id', 'decided_at',
    ])
  })

  it('reports an unobserved decision scalar as absent instead of an empty or zero value', () => {
    const decision = normalizeRequestTraceStageDecision('gateway_decision', {
      decision: 'auth', outcome: 'rejected', source: 'api_key', sequence: 1,
      model_from: null, model_to: '', protocol_from: undefined, protocol_to: null, account_id: 0, decided_at: null,
    })
    expect(decision).toEqual({
      decision: 'auth', outcome: 'rejected', source: 'api_key', sequence: 1,
      model_from: null, model_to: null, protocol_from: null, protocol_to: null, account_id: null, decided_at: null,
    })
    expect(Object.keys(decision)).toHaveLength(10)
  })

  it('refuses any decision key that is not part of the typed projection', () => {
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, metadata: { authorization: 'Bearer SECRET' } })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, headers: { Authorization: ['Bearer SECRET'] } })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, reason: 'decision_recorded' })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, model: 'gpt-5.3-codex' })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', 'Bearer SECRET')).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', null)).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', ['Bearer SECRET'])).toThrow()
  })

  it('allows a decision only on the stage the gateway records it for', () => {
    expect(() => normalizeRequestTraceStageDecision('wire_attempt', decisionFacts)).toThrow()
    expect(() => normalizeRequestTraceStageDecision('client_metadata', decisionFacts)).toThrow()
    expect(() => normalizeRequestTraceStageDecision('', decisionFacts)).toThrow()
  })

  it('refuses a decision enum value the gateway never records', () => {
    for (const key of ['decision', 'outcome', 'source'] as const) {
      expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, [key]: 'unknown' })).toThrow()
      expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, [key]: '' })).toThrow()
      expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, [key]: null })).toThrow()
      expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, [key]: 7 })).toThrow()
    }
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, decision: 'Decision' })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, outcome: 'ACCEPTED' })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, source: 'protocol-convert' })).toThrow()
    const missing = { ...decisionFacts } as Record<string, unknown>
    delete missing.outcome
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', missing)).toThrow()
  })

  it('keeps the decision sequence a positive order within the logical request', () => {
    expect(normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, sequence: 1 }).sequence).toBe(1)
    expect(normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, sequence: 1000 }).sequence).toBe(1000)
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, sequence: 0 })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, sequence: -1 })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, sequence: 1.5 })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, sequence: 1001 })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, sequence: '2' })).toThrow()
    const missing = { ...decisionFacts } as Record<string, unknown>
    delete missing.sequence
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', missing)).toThrow()
  })

  it('bounds model and protocol tokens and never lets one carry unsafe text', () => {
    expect(normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, model_from: 'a'.repeat(128) }).model_from).toHaveLength(128)
    for (const key of ['model_from', 'model_to', 'protocol_from', 'protocol_to'] as const) {
      expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, [key]: 'a'.repeat(129) })).toThrow()
      expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, [key]: 'not a token' })).toThrow()
      expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, [key]: 'gpt\n5' })).toThrow()
      expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, [key]: '<img src=x onerror=alert(1)>' })).toThrow()
      expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, [key]: 5 })).toThrow()
      expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, [key]: { value: 'x' } })).toThrow()
    }
  })

  it('bounds the decision account and time without inventing either', () => {
    expect(normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, account_id: 0 }).account_id).toBeNull()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, account_id: -1 })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, account_id: 1.5 })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, account_id: '42' })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, decided_at: 'not a time' })).toThrow()
    expect(() => normalizeRequestTraceStageDecision('gateway_decision', { ...decisionFacts, decided_at: 7 })).toThrow()
  })

  it('keeps each stage shape apart: a decision stage carries no transport facts and other stages carry no decision', () => {
    const withDecision = normalizeRequestTraceDetail(decisionDetail())
    expect(withDecision.stages[0].decision?.outcome).toBe('rewritten')
    expect(withDecision.stages[0].facts).toBeUndefined()

    expect(() => normalizeRequestTraceStageFacts('gateway_decision', attemptFacts)).toThrow()
    expect(() => normalizeRequestTraceDetail(decisionDetail(decisionFacts, { facts: attemptFacts }))).toThrow()
    expect(() => normalizeRequestTraceDetail(traceDetail({ decision: decisionFacts }))).toThrow()
    expect(() => normalizeRequestTraceDetail(traceDetail({ stage: 'client_metadata', decision: decisionFacts }))).toThrow()
  })

  it('keeps a decision stage body-less: no observed body state and no retained bytes', () => {
    expect(normalizeRequestTraceDetail(decisionDetail()).stages[0].state).toBe('not_observed')
    for (const state of ['stored', 'truncated', 'redaction_unverified'] as const) {
      expect(() => normalizeRequestTraceDetail(decisionDetail(decisionFacts, { state }))).toThrow()
      expect(() => normalizeRequestTraceDetail(decisionDetail(decisionFacts, { state, payload_text: 'CANARY_BODY' }))).toThrow()
    }
    expect(() => normalizeRequestTraceDetail(decisionDetail(decisionFacts, { state: 'write_failed' }))).toThrow()
    expect(() => normalizeRequestTraceDetail(decisionDetail(decisionFacts, { state: 'unsupported' }))).toThrow()
  })

  it('never surfaces a decision the gateway did not make, nor free-form stage metadata', () => {
    const withMetadata = normalizeRequestTraceDetail(decisionDetail(decisionFacts, { metadata: { authorization: 'Bearer SECRET' } }))
    expect(withMetadata.stages[0].decision?.decision).toBe('model_mapping')
    expect(JSON.stringify(withMetadata)).not.toContain('Bearer SECRET')

    const withoutDecision = normalizeRequestTraceDetail(decisionDetail(undefined, { decision: undefined, metadata: { authorization: 'Bearer SECRET' } }))
    expect(withoutDecision.stages[0].decision).toBeUndefined()
    expect(JSON.stringify(withoutDecision)).not.toContain('Bearer SECRET')
  })
})

describe('closed state and stage label sets', () => {
  it('pins the closed state sets to the backend contract', () => {
    // The list column and the drawer key their label maps off these lists, so a
    // change here is a contract change that must be acknowledged on both sides.
    expect([...requestTraceCaptureStates]).toEqual(['not_observed', 'stored', 'partial', 'write_failed'])
    expect([...requestTraceStageStates]).toEqual([
      'not_observed', 'stored', 'truncated', 'unsupported', 'redaction_unverified', 'write_failed',
    ])
  })

  it('keeps the decision stage name inside the stage label set', () => {
    // The drawer labels `stage.stage` through this set; the bodyless decision
    // stage must stay addressable there.
    expect([...requestTraceStageNames]).toContain(requestTraceDecisionStage)
  })

  it('names only bounded, pattern-safe stage names, views and reason codes', () => {
    // The client refuses anything that is not a bounded token, so a label set
    // value that could never travel the wire would be a dead label.
    for (const value of [...requestTraceStageNames, ...requestTraceStageViews, ...requestTraceStageReasons]) {
      expect(value, value).toMatch(/^[a-z][a-z0-9_]*$/)
      expect(value.length, value).toBeLessThanOrEqual(96)
    }
  })

  it('keeps every label set free of duplicates', () => {
    for (const [name, values] of Object.entries({
      requestTraceStageNames, requestTraceStageViews, requestTraceStageReasons,
      requestTraceCaptureStates, requestTraceStageStates,
    })) {
      expect(new Set(values).size, name).toBe(values.length)
    }
  })
})
