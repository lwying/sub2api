export type TraceAckLanguage = 'en' | 'zh'

/**
 * The summary `capture_state` closed set, as a runtime list and a union type. It
 * mirrors the Go contract's `RequestTraceCaptureState` values the summary endpoint
 * can return, and it is the single source of truth for both the parser's `Set` and
 * the localized capture-state labels.
 */
export const requestTraceCaptureStates = ['not_observed', 'stored', 'partial', 'write_failed'] as const

export type RequestTraceCaptureState = (typeof requestTraceCaptureStates)[number]

/**
 * The per-stage `state` closed set. It is the same Go enum as the summary capture
 * state, plus the body-level states only a stage can report.
 */
export const requestTraceStageStates = [
  'not_observed', 'stored', 'truncated', 'unsupported', 'redaction_unverified', 'write_failed',
] as const

export type RequestTraceStageState = (typeof requestTraceStageStates)[number]

/**
 * The stage names the Trace pipeline emits, as the single source of truth for the
 * localized stage labels. A name outside this set (an older or future server) is
 * labelled generically rather than echoed as a raw token. `gateway_decision` is
 * kept in step with `requestTraceDecisionStage` by a test, not by importing it here.
 */
export const requestTraceStageNames = [
  'client_metadata', 'client_entry', 'wire_attempt', 'wire_request',
  'upstream_response', 'client_response', 'capture_gap', 'gateway_decision',
] as const

/** The body views (`view_name`) a stage can report. */
export const requestTraceStageViews = ['decoded', 'transmitted', 'downstream', 'received', 'wire'] as const

/**
 * The bounded reason codes the Trace pipeline currently emits. The server only
 * bounds a reason by pattern, so this list can never be complete: it names the
 * codes worth their own label, and every other bounded code is labelled
 * generically instead of being rendered as a token.
 */
export const requestTraceStageReasons = [
  'metadata_observed', 'retained', 'body_not_observed', 'auth_rejected_body_not_observed',
  'attempt_not_observed', 'wire_observed', 'transport_error', 'wire_protocol_outside_phase1',
  'hijacked_unobservable', 'truncated', 'truncated_unverified', 'credential_redaction_unverified',
  'incomplete_event', 'incomplete_read', 'decode_failed', 'read_failed',
  'stage_budget_exceeded', 'decision_budget_exceeded',
  'decision_recorded', 'auth_accepted', 'auth_rejected', 'route_selected',
  'account_switch', 'model_rewritten', 'identity_sent', 'identity_rewritten', 'identity_not_sent',
] as const

export interface RequestTraceSummary {
  trace_id: string
  route_family: 'messages' | 'chat_completions' | 'responses'
  inbound_endpoint: string
  created_at: string
  completed_at: string | null
  client_status: number
  capture_state: RequestTraceCaptureState
  usage_log_id: number | null
  cleanup_after: string | null
}

export interface RequestTraceStage {
  ordinal: number
  stage: string
  attempt_index: number
  view_name: string
  state: RequestTraceStageState
  reason: string
  observed_bytes: number
  retained_bytes: number
  dropped_events: number
  redaction_unverified: boolean
  payload_text?: string
  facts?: RequestTraceStageFacts
  decision?: RequestTraceStageDecision
}

/**
 * The typed projection of client and wire scalars the server is willing to
 * disclose. A value the server did not observe stays absent (`null`, or an empty
 * map) and is never shown as an observed empty value. Credential placeholders
 * such as `[REDACTED]` are data, not markup: they are preserved verbatim.
 *
 * Headers keep the wire contract's shape: a header name maps to its redacted
 * values (Go `http.Header`), never a list of loose records and never a
 * free-form map of anything else.
 */
export interface RequestTraceStageFacts {
  method: string | null
  url: string | null
  url_omitted: boolean
  request_headers: Record<string, string[]>
  request_headers_omitted: number
  response_headers: Record<string, string[]>
  response_headers_omitted: number
  account_id: number | null
  model: string | null
  protocol: string | null
  value_protocol: string | null
  status: number | null
  started_at: string | null
  ended_at: string | null
}

/**
 * The closed decision sets, as runtime lists as well as union types. The list is
 * the single source of truth: the parser's `Set` is built from it and so is the
 * set of localized labels the detail view renders, so a value the backend
 * contract gains or drops cannot silently keep or lose a label.
 *
 * These mirror the Go contract's enum constants (`RequestTraceDecisionKind`,
 * `RequestTraceDecisionOutcome`, `RequestTraceDecisionSource`).
 */
export const requestTraceDecisionKinds = ['auth', 'route', 'model_mapping', 'account_switch', 'identity'] as const

export type RequestTraceDecisionKind = (typeof requestTraceDecisionKinds)[number]

export const requestTraceDecisionOutcomes = [
  'accepted', 'rejected', 'selected', 'unchanged', 'rewritten', 'not_sent', 'unsupported',
] as const

export type RequestTraceDecisionOutcome = (typeof requestTraceDecisionOutcomes)[number]

export const requestTraceDecisionSources = [
  'inbound', 'api_key', 'group', 'account', 'identity', 'protocol_convert',
] as const

export type RequestTraceDecisionSource = (typeof requestTraceDecisionSources)[number]

/**
 * The typed projection of one gateway-side decision. A decision describes what
 * the gateway itself decided, not what a client sent or what went on the wire:
 * it never carries a body, headers, a URL or an observed status.
 *
 * Every enum is a closed set, so a value the gateway cannot record is refused
 * rather than rendered as a decision that never happened. A decision scalar the
 * gateway did not observe stays absent (`null`) and is never shown as an
 * observed empty value; only the enums and `sequence` are always present.
 */
export interface RequestTraceStageDecision {
  decision: RequestTraceDecisionKind
  outcome: RequestTraceDecisionOutcome
  source: RequestTraceDecisionSource
  /** 1-based order of this decision inside the logical request. */
  sequence: number
  model_from: string | null
  model_to: string | null
  protocol_from: string | null
  protocol_to: string | null
  account_id: number | null
  decided_at: string | null
}

export interface RequestTraceDetail extends RequestTraceSummary {
  stages: RequestTraceStage[]
}

export interface RequestTracePage {
  items: RequestTraceSummary[]
  total: number
  page: number
  page_size: number
}

export interface RequestTraceListParams {
  page: number
  page_size: number
  trace_id?: string
  route_family?: RequestTraceSummary['route_family']
  client_status?: number
  created_from?: string
  created_to?: string
  usage_linked?: boolean
  /**
   * The two lookup filters the server resolves without returning any body:
   * `usage_log_id` matches the Trace envelope's linked usage record (used when
   * arriving from an admin usage row), `account_id` matches a real `wire_attempt`
   * stage's typed account fact — never a free-text search over stage metadata.
   */
  usage_log_id?: number
  account_id?: number
}

const traceIDPattern = /^[0-9a-f]{32}$/
const routeFamilies = new Set(['messages', 'chat_completions', 'responses'])
const captureStates = new Set<string>(requestTraceCaptureStates)
const stageStates = new Set<string>(requestTraceStageStates)
const safeStagePattern = /^[a-z][a-z0-9_]*$/

// Mirror of the server-side stage fact budget and per-field bounds. The client
// re-checks them so a compromised or buggy server cannot push unbounded or
// untyped values into the detail view.
const factStages = new Set(['client_metadata', 'wire_attempt'])
const factKeys = new Set([
  'method', 'url', 'url_omitted', 'request_headers', 'request_headers_omitted', 'response_headers',
  'response_headers_omitted', 'account_id', 'model', 'protocol', 'value_protocol', 'status', 'started_at', 'ended_at',
])
/** Facts the server can only attach to a real wire attempt, never to client metadata. */
const attemptOnlyFactKeys = [
  'response_headers', 'response_headers_omitted', 'account_id', 'model', 'protocol', 'value_protocol',
  'status', 'started_at', 'ended_at',
] as const
/**
 * The gateway records decisions on their own stage. That stage is not a wire
 * attempt and not a client observation, so it carries a decision record instead
 * of transport facts — and, symmetrically, no other stage may claim one. The
 * stage is body-less by contract, so it can never report an observed body.
 */
export const requestTraceDecisionStage = 'gateway_decision'
const decisionStages = new Set([requestTraceDecisionStage])
const decisionKeys = new Set([
  'decision', 'outcome', 'source', 'sequence', 'model_from', 'model_to',
  'protocol_from', 'protocol_to', 'account_id', 'decided_at',
])
const decisionKinds = new Set<string>(requestTraceDecisionKinds)
const decisionOutcomes = new Set<string>(requestTraceDecisionOutcomes)
const decisionSources = new Set<string>(requestTraceDecisionSources)
// One logical request cannot accumulate unbounded decision events, so the order
// is bounded the same way the server bounds it.
const maxDecisionSequence = 1000
const factsLimit = 3072
const maxFactURLBytes = 8192
const maxFactHeaderNames = 128
const maxFactHeaderValues = 8
const maxFactHeaderValueLength = 4096
const factTokenPattern = /^[A-Za-z0-9][A-Za-z0-9_./:+-]{0,127}$/
const httpHeaderNamePattern = /^[0-9A-Za-z!#$%&'*+\-.^_`|~]+$/
const unsafeFactTextPattern = /[\r\n\0]/
// A lone surrogate cannot round-trip through the wire as UTF-8; treat it as unsafe text.
const malformedTextPattern = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?:^|[^\uD800-\uDBFF])[\uDC00-\uDFFF]/
const utf8Encoder = new TextEncoder()

function traceRecord(value: unknown): Record<string, unknown> {
  if (value !== null && typeof value === 'object' && !Array.isArray(value)) return value as Record<string, unknown>
  throw new Error('Invalid request Trace record')
}

function nonnegativeInt(value: unknown, max = Number.MAX_SAFE_INTEGER): number {
  if (!Number.isSafeInteger(value) || (value as number) < 0 || (value as number) > max) throw new Error('Invalid request Trace count')
  return value as number
}

function optionalTimestamp(value: unknown): string | null {
  if (value == null) return null
  if (typeof value !== 'string' || value.length > 80 || Number.isNaN(Date.parse(value))) throw new Error('Invalid request Trace timestamp')
  return value
}

export function normalizeRequestTraceSummary(value: unknown): RequestTraceSummary {
  const source = traceRecord(value)
  if (typeof source.trace_id !== 'string' || !traceIDPattern.test(source.trace_id)) throw new Error('Invalid request Trace id')
  if (typeof source.route_family !== 'string' || !routeFamilies.has(source.route_family)) throw new Error('Invalid request Trace route')
  if (typeof source.inbound_endpoint !== 'string' || !/^\/[a-z0-9_./-]{1,255}$/.test(source.inbound_endpoint)) throw new Error('Invalid request Trace endpoint')
  if (typeof source.capture_state !== 'string' || !captureStates.has(source.capture_state)) throw new Error('Invalid request Trace state')
  if (typeof source.created_at !== 'string' || optionalTimestamp(source.created_at) === null) throw new Error('Invalid request Trace creation time')
  const usage = source.usage_log_id == null ? null : nonnegativeInt(source.usage_log_id)
  return {
    trace_id: source.trace_id,
    route_family: source.route_family as RequestTraceSummary['route_family'],
    inbound_endpoint: source.inbound_endpoint,
    created_at: source.created_at,
    completed_at: optionalTimestamp(source.completed_at),
    client_status: nonnegativeInt(source.client_status, 599),
    capture_state: source.capture_state as RequestTraceSummary['capture_state'],
    usage_log_id: usage,
    cleanup_after: optionalTimestamp(source.cleanup_after),
  }
}

export function normalizeRequestTracePage(value: unknown): RequestTracePage {
  const source = traceRecord(value)
  if (!Array.isArray(source.items) || source.items.length > 200) throw new Error('Invalid request Trace page')
  const page = nonnegativeInt(source.page)
  const pageSize = nonnegativeInt(source.page_size, 200)
  if (!page || !pageSize) throw new Error('Invalid request Trace pagination')
  return { items: source.items.map(normalizeRequestTraceSummary), total: nonnegativeInt(source.total), page, page_size: pageSize }
}

export function normalizeRequestTraceDetail(value: unknown): RequestTraceDetail {
  const source = traceRecord(value)
  if (!Array.isArray(source.stages) || source.stages.length > 1000) throw new Error('Invalid request Trace stages')
  const stages: RequestTraceStage[] = source.stages.map(raw => {
    const stage = traceRecord(raw)
    if (typeof stage.stage !== 'string' || stage.stage.length > 64 || !safeStagePattern.test(stage.stage)) throw new Error('Invalid request Trace stage')
    if (typeof stage.view_name !== 'string' || stage.view_name.length > 48 || (stage.view_name !== '' && !safeStagePattern.test(stage.view_name))) throw new Error('Invalid request Trace view')
    if (typeof stage.state !== 'string' || !stageStates.has(stage.state)) throw new Error('Invalid request Trace stage state')
    if (typeof stage.reason !== 'string' || stage.reason.length > 96 || !safeStagePattern.test(stage.reason)) throw new Error('Invalid request Trace reason')
    const payload = stage.payload_text
    if (payload != null && (typeof payload !== 'string' || payload.length > 4 * 1024 * 1024)) throw new Error('Invalid request Trace payload')
    if (payload != null && !['stored', 'truncated', 'redaction_unverified'].includes(stage.state)) throw new Error('Unexpected request Trace payload')
    if (typeof stage.redaction_unverified !== 'boolean') throw new Error('Invalid request Trace redaction flag')
    const stageFacts = stage.facts == null ? undefined : normalizeRequestTraceStageFacts(stage.stage, stage.facts)
    const stageDecision = stage.decision == null ? undefined : normalizeRequestTraceStageDecision(stage.stage, stage.decision)
    // The decision stage is body-less by contract: a decision that claims a
    // stored or retained body is not a decision the gateway recorded.
    if (stageDecision && stage.state !== 'not_observed') throw new Error('Unexpected request Trace decision state')
    return {
      ordinal: nonnegativeInt(stage.ordinal),
      stage: stage.stage,
      attempt_index: nonnegativeInt(stage.attempt_index),
      view_name: stage.view_name,
      state: stage.state as RequestTraceStage['state'],
      reason: stage.reason,
      observed_bytes: nonnegativeInt(stage.observed_bytes),
      retained_bytes: nonnegativeInt(stage.retained_bytes, 1048576),
      dropped_events: nonnegativeInt(stage.dropped_events),
      redaction_unverified: stage.redaction_unverified,
      ...(typeof payload === 'string' ? { payload_text: payload } : {}),
      ...(stageFacts ? { facts: stageFacts } : {}),
      ...(stageDecision ? { decision: stageDecision } : {}),
    }
  })
  return { ...normalizeRequestTraceSummary(source), stages }
}

function utf8Bytes(value: string): number {
  return utf8Encoder.encode(value).length
}

/** Safe text for display: bounded, single-line and round-trippable through UTF-8. */
function safeFactText(value: string, maxBytes: number): boolean {
  return utf8Bytes(value) <= maxBytes && !unsafeFactTextPattern.test(value) && !malformedTextPattern.test(value)
}

/** Same canonicalization the server applies before it writes a header name. */
function canonicalHeaderName(name: string): string {
  let upper = true
  let out = ''
  for (const char of name) {
    out += upper ? char.toUpperCase() : char.toLowerCase()
    upper = char === '-'
  }
  return out
}

/** Absent stays absent; an empty string is absent rather than an observed empty value. */
function factScalar(value: unknown): string | null {
  if (value === undefined) return null
  if (typeof value !== 'string') throw new Error('Invalid request Trace fact value')
  return value === '' ? null : value
}

function factToken(value: unknown): string | null {
  const token = factScalar(value)
  if (token !== null && !factTokenPattern.test(token)) throw new Error('Invalid request Trace fact token')
  return token
}

/** Absent and zero both mean "not observed"; a negative or non-integer value is a lie. */
function factCount(value: unknown): number | null {
  if (value === undefined) return null
  if (!Number.isSafeInteger(value) || (value as number) < 0) throw new Error('Invalid request Trace fact count')
  return value === 0 ? null : (value as number)
}

function factOmittedCount(source: Record<string, unknown>, key: string): number {
  const value = source[key]
  if (value === undefined) return 0
  if (!Number.isSafeInteger(value) || (value as number) < 0) throw new Error('Invalid request Trace fact count')
  return value as number
}

/**
 * Reads the server's header map (name to redacted values) into a name-sorted
 * copy of the same shape. Validation is per name and per value, so a payload
 * that is not a header map is refused rather than rendered as something it is
 * not. The copy is built with `Object.fromEntries`, which defines own
 * properties, so a header literally named `__proto__` stays an ordinary row
 * instead of reaching the object prototype.
 */
function factHeaders(value: unknown): Record<string, string[]> {
  if (value === undefined) return {}
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid request Trace fact headers')
  const source = value as Record<string, unknown>
  const names = Object.keys(source)
  if (names.length > maxFactHeaderNames) throw new Error('Too many request Trace fact headers')
  const entries: [string, string[]][] = []
  for (const name of names.sort()) {
    if (name.length > 128 || !httpHeaderNamePattern.test(name) || canonicalHeaderName(name) !== name) {
      throw new Error('Invalid request Trace fact header name')
    }
    const raw = source[name]
    // An empty value list is what a header nobody observed looks like; the server
    // never writes one, so it stays unrepresentable here too.
    if (!Array.isArray(raw) || raw.length === 0 || raw.length > maxFactHeaderValues) {
      throw new Error('Invalid request Trace fact header values')
    }
    entries.push([name, raw.map(item => {
      if (typeof item !== 'string' || !safeFactText(item, maxFactHeaderValueLength)) throw new Error('Invalid request Trace fact header value')
      return item
    })])
  }
  return Object.fromEntries(entries)
}

/**
 * Normalizes one stage's typed facts. Unknown keys, non-canonical header names
 * and facts on a stage the server cannot produce them for are refused, so an
 * arbitrary map (for example a raw `metadata` or `authorization` object) can
 * never be rendered as if it were an observed fact.
 */
export function normalizeRequestTraceStageFacts(stage: string, value: unknown): RequestTraceStageFacts {
  if (!factStages.has(stage)) throw new Error('Unexpected request Trace facts')
  const source = traceRecord(value)
  for (const key of Object.keys(source)) {
    if (!factKeys.has(key)) throw new Error('Invalid request Trace fact field')
  }
  if (utf8Bytes(JSON.stringify(source)) > factsLimit) throw new Error('Request Trace facts exceed the budget')
  if (stage === 'client_metadata') {
    for (const key of attemptOnlyFactKeys) {
      if (source[key] !== undefined) throw new Error('Unexpected request Trace fact field for this stage')
    }
  }
  const url = factScalar(source.url)
  if (url !== null && !safeFactText(url, maxFactURLBytes)) throw new Error('Invalid request Trace fact URL')
  const omitted = source.url_omitted
  if (omitted !== undefined && typeof omitted !== 'boolean') throw new Error('Invalid request Trace fact URL omission')
  if (url !== null && omitted === true) throw new Error('Invalid request Trace fact URL omission')
  const status = factCount(source.status)
  if (status !== null && (status < 100 || status > 599)) throw new Error('Invalid request Trace fact status')
  const startedAt = optionalFactTimestamp(source.started_at)
  const endedAt = optionalFactTimestamp(source.ended_at)
  if (endedAt !== null && (startedAt === null || Date.parse(endedAt) < Date.parse(startedAt))) {
    throw new Error('Invalid request Trace fact time order')
  }
  return {
    method: factToken(source.method),
    url,
    url_omitted: omitted === true,
    request_headers: factHeaders(source.request_headers),
    request_headers_omitted: factOmittedCount(source, 'request_headers_omitted'),
    response_headers: factHeaders(source.response_headers),
    response_headers_omitted: factOmittedCount(source, 'response_headers_omitted'),
    account_id: factCount(source.account_id),
    model: factToken(source.model),
    protocol: factToken(source.protocol),
    value_protocol: factToken(source.value_protocol),
    status,
    started_at: startedAt,
    ended_at: endedAt,
  }
}

function optionalFactTimestamp(value: unknown): string | null {
  if (value === undefined) return null
  if (typeof value !== 'string' || value.length > 80 || Number.isNaN(Date.parse(value))) throw new Error('Invalid request Trace fact time')
  return value
}

function decisionEnum(value: unknown, allowed: Set<string>, message: string): string {
  if (typeof value !== 'string' || !allowed.has(value)) throw new Error(message)
  return value
}

/** A decision order starts at 1; a zero or negative order is not an order. */
function decisionSequence(value: unknown): number {
  if (!Number.isSafeInteger(value) || (value as number) < 1 || (value as number) > maxDecisionSequence) {
    throw new Error('Invalid request Trace decision sequence')
  }
  return value as number
}

/**
 * An optional decision scalar. A missing key, an explicit `null` and an empty
 * string all mean "not observed" and stay absent, so a decision the gateway
 * left blank is never rendered as an observed empty value.
 */
function decisionOptional(value: unknown): unknown {
  return value === null || value === undefined || value === '' ? undefined : value
}

/** Absent and zero both mean "no account observed"; a negative or fractional id is a lie. */
function decisionAccountID(value: unknown): number | null {
  const present = decisionOptional(value)
  if (present === undefined) return null
  if (!Number.isSafeInteger(present) || (present as number) < 0) throw new Error('Invalid request Trace decision account')
  return present === 0 ? null : (present as number)
}

/**
 * Normalizes one gateway decision. Unknown keys are refused, so an arbitrary
 * map (a raw `metadata`, an `authorization` header or any other free-form
 * object) can never be rendered as if the gateway had decided it. A decision is
 * also only representable on the stage the gateway records it for, so a
 * decision smuggled onto a wire attempt or client metadata stage is refused
 * too.
 */
export function normalizeRequestTraceStageDecision(stage: string, value: unknown): RequestTraceStageDecision {
  if (!decisionStages.has(stage)) throw new Error('Unexpected request Trace decision')
  const source = traceRecord(value)
  for (const key of Object.keys(source)) {
    if (!decisionKeys.has(key)) throw new Error('Invalid request Trace decision field')
  }
  return {
    decision: decisionEnum(source.decision, decisionKinds, 'Invalid request Trace decision kind') as RequestTraceDecisionKind,
    outcome: decisionEnum(source.outcome, decisionOutcomes, 'Invalid request Trace decision outcome') as RequestTraceDecisionOutcome,
    source: decisionEnum(source.source, decisionSources, 'Invalid request Trace decision source') as RequestTraceDecisionSource,
    sequence: decisionSequence(source.sequence),
    model_from: factToken(decisionOptional(source.model_from)),
    model_to: factToken(decisionOptional(source.model_to)),
    protocol_from: factToken(decisionOptional(source.protocol_from)),
    protocol_to: factToken(decisionOptional(source.protocol_to)),
    account_id: decisionAccountID(source.account_id),
    decided_at: optionalFactTimestamp(decisionOptional(source.decided_at)),
  }
}

export type TraceDeploymentReason =
  | 'supported'
  | 'unsupported_partitioned_usage_logs'
  | 'unsupported_missing_ownership_foreign_key'
  | 'unsupported_unknown_deployment'
  | 'probe_failed'
  | 'probe_unavailable'

export interface RequestTraceOperatorStatus {
  enabled: boolean
  capture_allowed: boolean
  risk_acknowledged: boolean
  risk_version: string
  risk_phrase_en: string
  risk_phrase_zh: string
  risk_acknowledgement_current: boolean
  risk_acknowledgement?: {
    version: string
    admin_user_id: number
    accepted_at: string
  }
  plaintext_capture_supported: boolean
  plaintext_capture_support_reason: TraceDeploymentReason
}

export interface RequestTraceOperatorUpdateInput {
  enabled: boolean
  language: TraceAckLanguage
  phrase: string
}

const deploymentReasons = new Set<TraceDeploymentReason>([
  'supported',
  'unsupported_partitioned_usage_logs',
  'unsupported_missing_ownership_foreign_key',
  'unsupported_unknown_deployment',
  'probe_failed',
  'probe_unavailable',
])

function record(raw: unknown): Record<string, unknown> {
  if (raw !== null && typeof raw === 'object' && !Array.isArray(raw)) return raw as Record<string, unknown>
  throw new Error('Trace operator status is not an object')
}

export function normalizeRequestTraceOperatorStatus(raw: unknown): RequestTraceOperatorStatus {
  const source = record(raw)
  if (typeof source.enabled !== 'boolean' || typeof source.capture_allowed !== 'boolean') {
    throw new Error('Trace operator state is unavailable')
  }
  if (typeof source.risk_version !== 'string' || typeof source.risk_phrase_en !== 'string' || typeof source.risk_phrase_zh !== 'string') {
    throw new Error('Trace risk statement is unavailable')
  }
  const reason = source.plaintext_capture_support_reason
  if (typeof reason !== 'string' || !deploymentReasons.has(reason as TraceDeploymentReason)) {
    throw new Error('Trace deployment support is unavailable')
  }
  if (typeof source.risk_acknowledgement_current !== 'boolean' || typeof source.plaintext_capture_supported !== 'boolean') {
    throw new Error('Trace gate status is unavailable')
  }
  const status: RequestTraceOperatorStatus = {
    enabled: source.enabled,
    capture_allowed: source.capture_allowed,
    risk_acknowledged: source.risk_acknowledged === true,
    risk_version: source.risk_version,
    risk_phrase_en: source.risk_phrase_en,
    risk_phrase_zh: source.risk_phrase_zh,
    risk_acknowledgement_current: source.risk_acknowledgement_current,
    plaintext_capture_supported: source.plaintext_capture_supported,
    plaintext_capture_support_reason: reason as TraceDeploymentReason,
  }
  if (source.risk_acknowledgement !== undefined && source.risk_acknowledgement !== null) {
    const ack = record(source.risk_acknowledgement)
    if (typeof ack.version === 'string' && typeof ack.admin_user_id === 'number' && typeof ack.accepted_at === 'string') {
      status.risk_acknowledgement = { version: ack.version, admin_user_id: ack.admin_user_id, accepted_at: ack.accepted_at }
    }
  }
  return status
}

export type RequestTraceExportStatus = 'pending' | 'running' | 'completed' | 'failed'

/**
 * Bounded background export scope: the same metadata-only filter surface as the
 * Trace list. There is no body, query or free-text search here on purpose.
 */
export interface RequestTraceExportFilter {
  trace_id?: string
  route_family?: RequestTraceSummary['route_family']
  client_status?: number
  created_from?: string
  created_to?: string
  usage_linked?: boolean
}

/**
 * Server-owned task view. Completion and download deadlines are absent until the
 * server records them, and `downloadable` is the server's verdict for "this
 * session, right now, may download" — the client never recomputes it from a clock.
 * A gone temporary file is not a field here: it is only ever read from the
 * bounded reason of a refused download.
 */
export interface RequestTraceExportTask {
  id: string
  status: RequestTraceExportStatus
  filter: RequestTraceExportFilter
  rows_exported: number
  rows_skipped: number
  bytes_exported: number
  created_at: string
  completed_at: string | null
  download_until: string | null
  downloadable: boolean
}

/** Server-generated export task id: 32 lowercase hex characters, same shape as a Trace ID. */
export const requestTraceExportIDPattern = /^[0-9a-f]{32}$/
const exportStatuses = new Set<RequestTraceExportStatus>(['pending', 'running', 'completed', 'failed'])

function normalizeRequestTraceExportFilter(value: unknown): RequestTraceExportFilter {
  const source = traceRecord(value)
  const filter: RequestTraceExportFilter = {}
  if (source.trace_id != null) {
    if (typeof source.trace_id !== 'string' || !traceIDPattern.test(source.trace_id)) throw new Error('Invalid request Trace export filter')
    filter.trace_id = source.trace_id
  }
  if (source.route_family != null) {
    if (typeof source.route_family !== 'string' || !routeFamilies.has(source.route_family)) throw new Error('Invalid request Trace export filter')
    filter.route_family = source.route_family as RequestTraceExportFilter['route_family']
  }
  if (source.client_status != null) filter.client_status = nonnegativeInt(source.client_status, 599)
  if (source.usage_linked != null) {
    if (typeof source.usage_linked !== 'boolean') throw new Error('Invalid request Trace export filter')
    filter.usage_linked = source.usage_linked
  }
  for (const key of ['created_from', 'created_to'] as const) {
    const parsed = optionalTimestamp(source[key])
    if (parsed !== null) filter[key] = parsed
  }
  return filter
}

/**
 * The value-free operational-status closed sets, as runtime lists as well as
 * union types. Each list is the single source of truth for the parser's `Set`
 * and for the labels the status panel renders, so a value the backend contract
 * gains or drops cannot silently keep or lose a label.
 *
 * These mirror the Go contract's enum constants (`RequestTraceStorageState`,
 * `RequestTraceStorageProbeState`, `RequestTraceBacklogState`).
 */
export const requestTraceStorageProbeStates = ['not_configured', 'reachable', 'unavailable'] as const

export type RequestTraceStorageProbeState = (typeof requestTraceStorageProbeStates)[number]

export const requestTraceStorageStates = ['not_wired', 'no_traffic', 'ok', 'write_failed'] as const

export type RequestTraceStorageState = (typeof requestTraceStorageStates)[number]

export const requestTraceBacklogStates = ['unavailable', 'measured', 'at_least'] as const

export type RequestTraceBacklogState = (typeof requestTraceBacklogStates)[number]

/**
 * The capture queue's value-free state: counts and two flags, never an error
 * text, a host, a statement or an observed trace.
 */
export interface RequestTraceOpsCapture {
  storage: RequestTraceStorageState
  repository_available: boolean
  stopped: boolean
  queue_depth: number
  queue_capacity: number
  accepted: number
  stored: number
  write_failed: number
  dropped: number
  rejected: number
}

/** The export worker's cumulative counters, since this process started. */
export interface RequestTraceOpsExport {
  worker_started: boolean
  ticks: number
  tasks_run: number
  tasks_completed: number
  tasks_failed: number
  failures: number
  disabled_ticks: number
  cleanups: number
  cleaned_files: number
}

/**
 * The cleanup service's counters plus the bounded backlog of unlinked traces
 * that are past their planned cleanup point. `backlog_state` is what keeps a
 * capped count from being read as a total, and an unmeasured one from being
 * read as zero.
 */
export interface RequestTraceOpsCleanup {
  runs: number
  deleted: number
  failures: number
  last_deleted: number
  unlinked_backlog: number
  backlog_limit: number
  backlog_state: RequestTraceBacklogState
}

/**
 * The whole value-free answer. It deliberately carries no enablement flag: the
 * endpoint reports what the pipeline did, never whether the capture gate is on,
 * so no consumer can infer "capture is enabled" from a healthy count.
 */
export interface RequestTraceOpsStatus {
  storage_probe: RequestTraceStorageProbeState
  capture: RequestTraceOpsCapture
  export: RequestTraceOpsExport
  cleanup: RequestTraceOpsCleanup
}

const storageProbeStates = new Set<string>(requestTraceStorageProbeStates)
const storageStates = new Set<string>(requestTraceStorageStates)
const backlogStates = new Set<string>(requestTraceBacklogStates)

function opsEnum(value: unknown, allowed: Set<string>, message: string): string {
  if (typeof value !== 'string' || !allowed.has(value)) throw new Error(message)
  return value
}

/** A flag is a boolean or it is not an observed flag; `"true"` and `1` are refused. */
function opsBool(value: unknown, message: string): boolean {
  if (typeof value !== 'boolean') throw new Error(message)
  return value
}

/** A counter is a non-negative integer or it is not a count. */
function opsCount(value: unknown, message: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < 0) throw new Error(message)
  return value as number
}

/**
 * Normalizes the operational status. Only the allowlisted counters, flags and
 * closed sets are read, so an unexpected field — a stored database message, a
 * body, a header value, a raw query or a filename — is dropped rather than
 * carried into the view. An enum outside its set is refused instead of being
 * rendered as a state the pipeline reported.
 */
export function normalizeRequestTraceOpsStatus(value: unknown): RequestTraceOpsStatus {
  const source = traceRecord(value)
  const capture = traceRecord(source.capture)
  const exportCounters = traceRecord(source.export)
  const cleanup = traceRecord(source.cleanup)
  return {
    storage_probe: opsEnum(source.storage_probe, storageProbeStates, 'Invalid request Trace storage probe') as RequestTraceStorageProbeState,
    capture: {
      storage: opsEnum(capture.storage, storageStates, 'Invalid request Trace storage state') as RequestTraceStorageState,
      repository_available: opsBool(capture.repository_available, 'Invalid request Trace repository flag'),
      stopped: opsBool(capture.stopped, 'Invalid request Trace queue flag'),
      queue_depth: opsCount(capture.queue_depth, 'Invalid request Trace queue depth'),
      queue_capacity: opsCount(capture.queue_capacity, 'Invalid request Trace queue capacity'),
      accepted: opsCount(capture.accepted, 'Invalid request Trace accepted count'),
      stored: opsCount(capture.stored, 'Invalid request Trace stored count'),
      write_failed: opsCount(capture.write_failed, 'Invalid request Trace write failure count'),
      dropped: opsCount(capture.dropped, 'Invalid request Trace drop count'),
      rejected: opsCount(capture.rejected, 'Invalid request Trace rejection count'),
    },
    export: {
      worker_started: opsBool(exportCounters.worker_started, 'Invalid request Trace export worker flag'),
      ticks: opsCount(exportCounters.ticks, 'Invalid request Trace export tick count'),
      tasks_run: opsCount(exportCounters.tasks_run, 'Invalid request Trace export task count'),
      tasks_completed: opsCount(exportCounters.tasks_completed, 'Invalid request Trace export completion count'),
      tasks_failed: opsCount(exportCounters.tasks_failed, 'Invalid request Trace export failure count'),
      failures: opsCount(exportCounters.failures, 'Invalid request Trace export failure count'),
      disabled_ticks: opsCount(exportCounters.disabled_ticks, 'Invalid request Trace export disabled tick count'),
      cleanups: opsCount(exportCounters.cleanups, 'Invalid request Trace export cleanup count'),
      cleaned_files: opsCount(exportCounters.cleaned_files, 'Invalid request Trace export cleaned file count'),
    },
    cleanup: {
      runs: opsCount(cleanup.runs, 'Invalid request Trace cleanup run count'),
      deleted: opsCount(cleanup.deleted, 'Invalid request Trace cleanup deletion count'),
      failures: opsCount(cleanup.failures, 'Invalid request Trace cleanup failure count'),
      last_deleted: opsCount(cleanup.last_deleted, 'Invalid request Trace cleanup batch count'),
      unlinked_backlog: opsCount(cleanup.unlinked_backlog, 'Invalid request Trace backlog count'),
      backlog_limit: opsCount(cleanup.backlog_limit, 'Invalid request Trace backlog probe limit'),
      backlog_state: opsEnum(cleanup.backlog_state, backlogStates, 'Invalid request Trace backlog state') as RequestTraceBacklogState,
    },
  }
}

export function normalizeRequestTraceExportTask(value: unknown): RequestTraceExportTask {
  const source = traceRecord(value)
  if (typeof source.id !== 'string' || !requestTraceExportIDPattern.test(source.id)) throw new Error('Invalid request Trace export id')
  if (typeof source.status !== 'string' || !exportStatuses.has(source.status as RequestTraceExportStatus)) throw new Error('Invalid request Trace export status')
  if (typeof source.created_at !== 'string' || optionalTimestamp(source.created_at) === null) throw new Error('Invalid request Trace export creation time')
  if (typeof source.downloadable !== 'boolean') throw new Error('Invalid request Trace export download state')
  return {
    id: source.id,
    status: source.status as RequestTraceExportStatus,
    filter: normalizeRequestTraceExportFilter(source.filter),
    rows_exported: nonnegativeInt(source.rows_exported),
    rows_skipped: nonnegativeInt(source.rows_skipped),
    bytes_exported: nonnegativeInt(source.bytes_exported),
    created_at: source.created_at,
    completed_at: optionalTimestamp(source.completed_at),
    download_until: optionalTimestamp(source.download_until),
    downloadable: source.downloadable,
  }
}
