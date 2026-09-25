/**
 * Admin Usage API endpoints
 * Handles admin-level usage logs and statistics retrieval
 */

import { apiClient } from '../client'
import type { AdminUsageLog, UsageQueryParams, PaginatedResponse, UsageRequestType } from '@/types'
import type { EndpointStat } from '@/types'

// ==================== Types ====================

export interface AdminUsageStatsResponse {
  total_requests: number
  total_input_tokens: number
  total_output_tokens: number
  total_cache_tokens: number
  total_cache_creation_tokens: number
  total_cache_read_tokens: number
  total_tokens: number
  total_cost: number
  total_actual_cost: number
  total_account_cost: number
  average_duration_ms: number
  endpoints?: EndpointStat[]
  upstream_endpoints?: EndpointStat[]
  endpoint_paths?: EndpointStat[]
}

export interface SimpleUser {
  id: number
  email: string
  deleted: boolean
}

export interface SimpleApiKey {
  id: number
  name: string
  user_id: number
}

export interface UsageCleanupFilters {
  start_time: string
  end_time: string
  user_id?: number
  api_key_id?: number
  account_id?: number
  group_id?: number
  model?: string | null
  request_type?: UsageRequestType | null
  stream?: boolean | null
  billing_type?: number | null
}

export interface UsageCleanupTask {
  id: number
  status: string
  filters: UsageCleanupFilters
  created_by: number
  deleted_rows: number
  error_message?: string | null
  canceled_by?: number | null
  canceled_at?: string | null
  started_at?: string | null
  finished_at?: string | null
  created_at: string
  updated_at: string
}

export interface CreateUsageCleanupTaskRequest {
  start_date: string
  end_date: string
  user_id?: number
  api_key_id?: number
  account_id?: number
  group_id?: number
  model?: string | null
  request_type?: UsageRequestType | null
  stream?: boolean | null
  billing_type?: number | null
  timezone?: string
}

export interface RequestAuditEventSkeleton {
  type: string
  index: number
  bytes: number
  fingerprint?: string
  truncated?: boolean
  original?: number
  original_bytes?: number
  kept?: number
  kept_bytes?: number
  dropped?: number
  dropped_bytes?: number
  reason?: string
}

export interface RequestAuditProtocolFields {
  stream?: boolean
  thinking_type?: 'disabled' | 'enabled' | 'adaptive'
  present_fields?: string[]
  normalized_fields?: string[]
}

export interface RequestAuditMetadata {
  routes?: Record<string, string>
  ids?: Record<string, string>
  status?: Record<string, number>
  bytes?: Record<string, number>
  tokens?: Record<string, number>
  protocol_fields?: RequestAuditProtocolFields
}

export interface RequestAuditAttempt {
  account_id?: number
  /**
   * @deprecated The backend strips the raw model alias at the persistence boundary, so this
   * field is never populated. Render `model_fingerprint` instead and never display the raw alias.
   */
  model?: string
  /**
   * Request-scoped HMAC digest of the model alias for this attempt (64 lowercase hex characters).
   * It is not reversible and cannot be correlated across requests or users, but equal aliases
   * within one logical request produce the same digest, so stages can be compared.
   */
  model_fingerprint?: string
  protocol?: string
  stage?: string
  wire_request_headers?: Record<string, unknown>
  upstream_response_headers?: Record<string, unknown>
  upstream_status?: number
  request_payload_bytes?: number
  response_payload_bytes?: number
  response_read_complete?: boolean
}

export interface RequestAudit {
  usage_log_id: number
  /** Inbound/client headers only; upstream headers live on their attempt. */
  headers?: Record<string, unknown>
  events: RequestAuditEventSkeleton[]
  attempts?: RequestAuditAttempt[]
  capture_completeness?: string
  capture_reason?: string
  request_fingerprint?: string
  fingerprint_key_version?: number
  metadata?: RequestAuditMetadata
}

// ==================== Request-audit value detail (Claude /v1/messages) ====================

/**
 * Short-term value detail attached to a usage-owned request audit.
 *
 * A long-lived audit row never carries header *values*, a model name or parsed
 * client identifiers; this sidecar is the narrow, opt-in exception for requests
 * that entered on `/v1/messages` and really called an Anthropic upstream. Its
 * values are encrypted, readable for 7 days, and returned only by an explicit
 * POST — the GET is an envelope, and the envelope type below has no value
 * fields, so "the default read never returns values" is a type guarantee.
 *
 * Everything here is re-validated at the boundary against the same closed
 * allowlists the backend persists with: a tampered or future response cannot
 * smuggle an unknown header name, a credential-shaped value or a model body
 * into the DOM.
 */
export const REQUEST_AUDIT_VALUE_DETAIL_STATES = [
  'not_observed',
  'stored',
  'skipped',
  'expired',
  'purged',
] as const
export type RequestAuditValueDetailState = (typeof REQUEST_AUDIT_VALUE_DETAIL_STATES)[number]

export const REQUEST_AUDIT_VALUE_DETAIL_REASONS = [
  'not_observed',
  'retained',
  'skipped_out_of_scope',
  'skipped_value_retention_disabled',
  'skipped_encryption_unavailable',
  'skipped_invalid_values',
  'skipped_too_many_attempts',
] as const
export type RequestAuditValueDetailReason = (typeof REQUEST_AUDIT_VALUE_DETAIL_REASONS)[number]

/** The GET view: what was collected, why not, and how long it stays readable. No values. */
export interface RequestAuditValueDetailEnvelope {
  usage_log_id: number
  state: RequestAuditValueDetailState
  /** Dropped when it is not one of the stable reason codes. */
  reason?: RequestAuditValueDetailReason
  route?: string
  protocol?: string
  client_status?: number
  attempt_count: number
  entry_count: number
  payload_bytes: number
  started_at?: string
  completed_at?: string
  /** 7-day disclosure deadline; absent when nothing was ever retained. */
  expires_at?: string
  created_at: string
  /** False means the whole capability is off for this instance, not that nothing happened. */
  capability_enabled: boolean
}

/** Header values are a name -> values list; a single-value header is a one-element list. */
export type RequestAuditValueDetailHeaders = Record<string, string[]>

export interface RequestAuditValueDetailInbound {
  request_headers: RequestAuditValueDetailHeaders
  device_id?: string
  account_uuid?: string
  session_id?: string
}

export interface RequestAuditValueDetailAttemptValues {
  index: number
  account_id?: number
  model?: string
  upstream_status?: number
  /**
   * Measured duration of this upstream attempt, in milliseconds. The backend
   * refuses anything above its own 24-hour ceiling, so a value outside the bound
   * is not a measurement and stays absent instead of being shown as one.
   */
  latency_ms?: number
  /**
   * Internal id of the proxy this attempt was sent through. Only a positive id
   * identifies a proxy, so `0` (no proxy) is never rendered as one.
   */
  proxy_id?: number
  request_headers: RequestAuditValueDetailHeaders
  response_headers: RequestAuditValueDetailHeaders
  device_id?: string
  account_uuid?: string
  session_id?: string
}

export interface RequestAuditValueDetailValues {
  /** The final model name. It is encrypted precisely because the caller chooses it freely. */
  model?: string
  inbound: RequestAuditValueDetailInbound
  attempts: RequestAuditValueDetailAttemptValues[]
  /** True when some submitted entries were not accepted; never render it as completeness. */
  truncated: boolean
  /**
   * Derived by this decoder, never sent by the backend: true when the reveal
   * payload carried at least one entry that did not pass the boundary checks
   * below. It is the read-side counterpart of `truncated` and must be rendered
   * too — a view that quietly dropped an entry while claiming to be complete
   * would read as "the client only sent this".
   */
  validation_dropped: boolean
}

export interface RequestAuditValueDetailReveal {
  usage_log_id: number
  values: RequestAuditValueDetailValues
}

/** Raised when a value-detail payload does not match the agreed contract. */
export class RequestAuditValueDetailPayloadError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'RequestAuditValueDetailPayloadError'
  }
}

const MAX_HEADER_ENTRIES = 40
const MAX_HEADER_VALUES = 4
const MAX_HEADER_VALUE_BYTES = 256
const MAX_IDENTIFIER_BYTES = 128
const MAX_MODEL_BYTES = 200
/** The backend's own ceiling for one attempt's latency (24 hours). */
const MAX_ATTEMPT_LATENCY_MS = 24 * 60 * 60 * 1000

/**
 * Canonical names the backend persists, by direction, compared lower-cased.
 * A name outside these maps is dropped here exactly as it is dropped server-side:
 * an unlisted header can be an authentication channel, so it is never rendered.
 */
const CLAUDE_REQUEST_HEADER_NAMES: Record<string, string> = {
  host: 'Host',
  'user-agent': 'User-Agent',
  'anthropic-beta': 'Anthropic-Beta',
  'anthropic-version': 'Anthropic-Version',
  'anthropic-dangerous-direct-browser-access': 'Anthropic-Dangerous-Direct-Browser-Access',
  accept: 'Accept',
  'accept-encoding': 'Accept-Encoding',
  'accept-language': 'Accept-Language',
  'content-type': 'Content-Type',
  'x-app': 'X-App',
  'x-request-id': 'X-Request-Id',
  'x-client-request-id': 'X-Client-Request-Id',
  'x-claude-code-session-id': 'X-Claude-Code-Session-Id',
  'x-stainless-retry-count': 'X-Stainless-Retry-Count',
  'x-stainless-timeout': 'X-Stainless-Timeout',
  'x-stainless-lang': 'X-Stainless-Lang',
  'x-stainless-package-version': 'X-Stainless-Package-Version',
  'x-stainless-os': 'X-Stainless-OS',
  'x-stainless-arch': 'X-Stainless-Arch',
  'x-stainless-runtime': 'X-Stainless-Runtime',
  'x-stainless-runtime-version': 'X-Stainless-Runtime-Version',
  'x-stainless-helper-method': 'X-Stainless-Helper-Method',
}

const CLAUDE_RESPONSE_HEADER_NAMES: Record<string, string> = {
  'content-type': 'Content-Type',
  'cache-control': 'Cache-Control',
  'retry-after': 'Retry-After',
  'request-id': 'Request-Id',
  'x-request-id': 'X-Request-Id',
  'anthropic-ratelimit-requests-limit': 'Anthropic-Ratelimit-Requests-Limit',
  'anthropic-ratelimit-requests-remaining': 'Anthropic-Ratelimit-Requests-Remaining',
  'anthropic-ratelimit-requests-reset': 'Anthropic-Ratelimit-Requests-Reset',
  'anthropic-ratelimit-input-tokens-limit': 'Anthropic-Ratelimit-Input-Tokens-Limit',
  'anthropic-ratelimit-input-tokens-remaining': 'Anthropic-Ratelimit-Input-Tokens-Remaining',
  'anthropic-ratelimit-input-tokens-reset': 'Anthropic-Ratelimit-Input-Tokens-Reset',
  'anthropic-ratelimit-output-tokens-limit': 'Anthropic-Ratelimit-Output-Tokens-Limit',
  'anthropic-ratelimit-output-tokens-remaining': 'Anthropic-Ratelimit-Output-Tokens-Remaining',
  'anthropic-ratelimit-output-tokens-reset': 'Anthropic-Ratelimit-Output-Tokens-Reset',
}

/**
 * Credential headers the backend records as presence only. They are absent from
 * the allowlists above, so this set is a second, explicit refusal: the UI must
 * never show a credential even if an allowlist is ever widened by mistake.
 */
const CLIENT_CREDENTIAL_HEADER_NAMES = new Set([
  'authorization',
  'cookie',
  'set-cookie',
  'api-key',
  'x-api-key',
  'proxy-authorization',
  'proxy-authenticate',
  'www-authenticate',
])

/**
 * The unambiguous credential prefixes, compared lower-cased. This is the
 * backend's own rule for the value-detail fields that have no closed per-name
 * semantics — the model name and the parsed `metadata.user_id` components
 * (`requestAuditValueDetailCredentialPrefixes`) — and the same prefixes are the
 * only value-level refusal applied here.
 *
 * It deliberately holds no generic word. A substring scan for `secret`, `key`,
 * `apikey` or `password` would refuse values the backend legitimately retained —
 * an opaque device id, a beta feature token, a host label that happens to contain
 * one — and the view would then silently show fewer entries than were collected,
 * disagreeing with the count the server reported. The name allowlists are the
 * closed set; a value is refused only when it *begins* with something that only a
 * credential begins with.
 */
const CREDENTIAL_PREFIX_MARKERS = [
  'bearer ',
  'basic ',
  'sk-',
  'sk_',
  'ghp_',
  'gho_',
  'xoxb-',
  'xoxp-',
  '-----begin',
]

function hasCredentialPrefix(value: string): boolean {
  const lower = value.toLowerCase()
  return CREDENTIAL_PREFIX_MARKERS.some((prefix) => lower.startsWith(prefix))
}

/** Bounded opaque identifiers only: no whitespace, no free text, same charset as the backend. */
const IDENTIFIER_PATTERN = /^[A-Za-z0-9._:+/-]{1,128}$/
const hasControlCharacter = (value: string): boolean => { for (let index = 0; index < value.length; index += 1) { const code = value.charCodeAt(index); if (code < 0x20 || code === 0x7f) return true } return false }

/**
 * Records that this decoder refused an entry the reveal payload carried.
 *
 * The backend already states whether an entry was dropped before persistence
 * (`truncated`); this is the read-side counterpart. Without it, an entry this
 * boundary refuses — a tampered name, a future shape, a value starting with a
 * credential prefix — would vanish and the remaining view would look complete.
 * Credential *headers* are not counted: the backend never stores a value for
 * them, so their absence is a designed exclusion, not an entry that was lost.
 */
interface ValueDetailDropTracker {
  dropped: boolean
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isSafeText(value: string, maxBytes: number): boolean {
  return (
    value !== '' &&
    value.length <= maxBytes &&
    !hasControlCharacter(value) &&
    !hasCredentialPrefix(value)
  )
}

/**
 * Accepts one header name's values, or drops the whole entry.
 *
 * Dropping (rather than throwing) keeps the rest of a reveal usable: the backend
 * already refuses to persist anything outside the allowlist, so an entry that
 * fails here is evidence of a tampered or newer payload. The caller records the
 * drop, because the admin must be able to tell this view apart from a complete
 * one; the backend's own `truncated` statement covers the collection side.
 */
function readHeaderValues(rawValues: unknown): string[] | null {
  if (!Array.isArray(rawValues) || rawValues.length === 0 || rawValues.length > MAX_HEADER_VALUES) {
    return null
  }

  const values: string[] = []
  for (const raw of rawValues) {
    if (typeof raw !== 'string') return null
    const value = raw.trim()
    if (!isSafeText(value, MAX_HEADER_VALUE_BYTES)) return null
    values.push(value)
  }
  return values
}

function readHeaderMap(
  raw: unknown,
  direction: 'request' | 'response',
  drops: ValueDetailDropTracker,
): RequestAuditValueDetailHeaders {
  if (raw === undefined || raw === null) return {}
  if (!isPlainRecord(raw)) {
    throw new RequestAuditValueDetailPayloadError('value detail headers are not an object')
  }
  const allowed = direction === 'request' ? CLAUDE_REQUEST_HEADER_NAMES : CLAUDE_RESPONSE_HEADER_NAMES
  const headers: RequestAuditValueDetailHeaders = {}
  for (const [rawName, rawValues] of Object.entries(raw)) {
    const name = rawName.trim().toLowerCase()
    // A credential header is a designed exclusion, not a lost entry: the backend
    // records those as presence only and never stores a value for them, so it is
    // not counted as something this view dropped.
    if (CLIENT_CREDENTIAL_HEADER_NAMES.has(name)) continue
    const canonical = allowed[name]
    if (!canonical || Object.keys(headers).length >= MAX_HEADER_ENTRIES) {
      drops.dropped = true
      continue
    }
    const values = readHeaderValues(rawValues)
    if (values === null) {
      drops.dropped = true
      continue
    }
    headers[canonical] = values
  }
  return headers
}

/** A parsed identifier is a fact only when it has the bounded opaque shape. */
function readIdentifier(raw: unknown, drops: ValueDetailDropTracker): string | undefined {
  if (raw === undefined || raw === null) return undefined
  if (typeof raw !== 'string') {
    drops.dropped = true
    return undefined
  }
  const value = raw.trim()
  // An empty component is a legitimate fact (account_uuid is often absent), not a
  // dropped entry: the backend writes it as absent for exactly the same reason.
  if (value === '') return undefined
  if (value.length > MAX_IDENTIFIER_BYTES || !IDENTIFIER_PATTERN.test(value) || hasCredentialPrefix(value)) {
    drops.dropped = true
    return undefined
  }
  return value
}

function readModel(raw: unknown, drops: ValueDetailDropTracker): string | undefined {
  if (raw === undefined || raw === null) return undefined
  if (typeof raw !== 'string' || !isSafeText(raw.trim(), MAX_MODEL_BYTES)) {
    drops.dropped = true
    return undefined
  }
  return raw.trim()
}

function readOptionalInteger(raw: unknown, min: number, max: number): number | undefined {
  if (typeof raw !== 'number' || !Number.isSafeInteger(raw) || raw < min || raw > max) return undefined
  return raw
}

/**
 * Reads one optional attempt scalar. A value that is absent (or JSON `null`) is a
 * fact of its own and is not a drop; a value that is present but outside its
 * bound is a measurement this view refuses, and is recorded as one.
 */
function readAttemptScalar(
  raw: unknown,
  min: number,
  max: number,
  drops: ValueDetailDropTracker,
): number | undefined {
  if (raw === undefined || raw === null) return undefined
  const value = readOptionalInteger(raw, min, max)
  if (value === undefined) drops.dropped = true
  return value
}

function readOptionalTimestamp(raw: unknown): string | undefined {
  if (typeof raw !== 'string' || raw.trim() === '' || Number.isNaN(Date.parse(raw))) return undefined
  return raw
}

function readAttempt(raw: unknown, drops: ValueDetailDropTracker): RequestAuditValueDetailAttemptValues | null {
  if (!isPlainRecord(raw)) {
    drops.dropped = true
    return null
  }
  const index = readOptionalInteger(raw.index, 1, Number.MAX_SAFE_INTEGER)
  if (index === undefined) {
    drops.dropped = true
    return null
  }

  const attempt: RequestAuditValueDetailAttemptValues = {
    index,
    request_headers: readHeaderMap(raw.request_headers, 'request', drops),
    response_headers: readHeaderMap(raw.response_headers, 'response', drops),
  }
  const accountId = readAttemptScalar(raw.account_id, 1, Number.MAX_SAFE_INTEGER, drops)
  if (accountId !== undefined) attempt.account_id = accountId
  const model = readModel(raw.model, drops)
  if (model !== undefined) attempt.model = model
  const status = readAttemptScalar(raw.upstream_status, 100, 599, drops)
  if (status !== undefined) attempt.upstream_status = status
  // A measured zero latency is a measurement, so the floor is 0; a proxy id of 0 is
  // "no proxy", which is not an identifier and stays absent.
  const latencyMs = readAttemptScalar(raw.latency_ms, 0, MAX_ATTEMPT_LATENCY_MS, drops)
  if (latencyMs !== undefined) attempt.latency_ms = latencyMs
  const proxyId = readAttemptScalar(raw.proxy_id, 1, Number.MAX_SAFE_INTEGER, drops)
  if (proxyId !== undefined) attempt.proxy_id = proxyId
  const deviceId = readIdentifier(raw.device_id, drops)
  if (deviceId !== undefined) attempt.device_id = deviceId
  const accountUuid = readIdentifier(raw.account_uuid, drops)
  if (accountUuid !== undefined) attempt.account_uuid = accountUuid
  const sessionId = readIdentifier(raw.session_id, drops)
  if (sessionId !== undefined) attempt.session_id = sessionId
  return attempt
}

export function normalizeRequestAuditValueDetailEnvelope(
  raw: unknown,
): RequestAuditValueDetailEnvelope {
  if (!isPlainRecord(raw)) {
    throw new RequestAuditValueDetailPayloadError('value detail envelope is not an object')
  }
  const usageLogId = readOptionalInteger(raw.usage_log_id, 1, Number.MAX_SAFE_INTEGER)
  if (usageLogId === undefined) {
    throw new RequestAuditValueDetailPayloadError('value detail usage_log_id is missing')
  }
  const state = raw.state
  if (typeof state !== 'string' || !(REQUEST_AUDIT_VALUE_DETAIL_STATES as readonly string[]).includes(state)) {
    throw new RequestAuditValueDetailPayloadError('value detail state is not a known value')
  }
  const createdAt = readOptionalTimestamp(raw.created_at)
  if (createdAt === undefined) {
    throw new RequestAuditValueDetailPayloadError('value detail created_at is missing')
  }

  const envelope: RequestAuditValueDetailEnvelope = {
    usage_log_id: usageLogId,
    state: state as RequestAuditValueDetailState,
    attempt_count: readOptionalInteger(raw.attempt_count, 0, Number.MAX_SAFE_INTEGER) ?? 0,
    entry_count: readOptionalInteger(raw.entry_count, 0, Number.MAX_SAFE_INTEGER) ?? 0,
    payload_bytes: readOptionalInteger(raw.payload_bytes, 0, Number.MAX_SAFE_INTEGER) ?? 0,
    created_at: createdAt,
    // Missing is read as "the capability is off", which is the conservative claim:
    // it can never imply that values are available for reading.
    capability_enabled: raw.capability_enabled === true,
  }

  const reason = raw.reason
  if (
    typeof reason === 'string' &&
    (REQUEST_AUDIT_VALUE_DETAIL_REASONS as readonly string[]).includes(reason)
  ) {
    envelope.reason = reason as RequestAuditValueDetailReason
  }
  for (const key of ['route', 'protocol'] as const) {
    const value = raw[key]
    if (typeof value === 'string' && value.trim() !== '' && !hasControlCharacter(value)) {
      envelope[key] = value.trim()
    }
  }
  const clientStatus = readOptionalInteger(raw.client_status, 100, 599)
  if (clientStatus !== undefined) envelope.client_status = clientStatus
  for (const key of ['started_at', 'completed_at', 'expires_at'] as const) {
    const value = readOptionalTimestamp(raw[key])
    if (value !== undefined) envelope[key] = value
  }
  return envelope
}

export function normalizeRequestAuditValueDetailReveal(
  raw: unknown,
): RequestAuditValueDetailReveal {
  if (!isPlainRecord(raw)) {
    throw new RequestAuditValueDetailPayloadError('value detail reveal is not an object')
  }
  const usageLogId = readOptionalInteger(raw.usage_log_id, 1, Number.MAX_SAFE_INTEGER)
  if (usageLogId === undefined) {
    throw new RequestAuditValueDetailPayloadError('value detail reveal has no usage_log_id')
  }
  const rawValues = raw.values
  if (!isPlainRecord(rawValues)) {
    throw new RequestAuditValueDetailPayloadError('value detail reveal has no values')
  }
  const rawInbound = rawValues.inbound
  if (!isPlainRecord(rawInbound)) {
    throw new RequestAuditValueDetailPayloadError('value detail reveal has no inbound block')
  }

  const drops: ValueDetailDropTracker = { dropped: false }

  const inbound: RequestAuditValueDetailInbound = {
    request_headers: readHeaderMap(rawInbound.request_headers, 'request', drops),
  }
  const deviceId = readIdentifier(rawInbound.device_id, drops)
  if (deviceId !== undefined) inbound.device_id = deviceId
  const accountUuid = readIdentifier(rawInbound.account_uuid, drops)
  if (accountUuid !== undefined) inbound.account_uuid = accountUuid
  const sessionId = readIdentifier(rawInbound.session_id, drops)
  if (sessionId !== undefined) inbound.session_id = sessionId

  const values: RequestAuditValueDetailValues = {
    inbound,
    attempts: [],
    truncated: rawValues.truncated === true,
    // Filled in below from what this boundary actually refused; a payload cannot
    // set it, in either direction.
    validation_dropped: false,
  }
  const model = readModel(rawValues.model, drops)
  if (model !== undefined) values.model = model
  if (Array.isArray(rawValues.attempts)) {
    for (const rawAttempt of rawValues.attempts) {
      const attempt = readAttempt(rawAttempt, drops)
      if (attempt) values.attempts.push(attempt)
    }
  } else if (rawValues.attempts !== undefined) {
    // A present attempts block that is not a list is malformed, not empty.
    drops.dropped = true
  }
  values.validation_dropped = drops.dropped

  return { usage_log_id: usageLogId, values }
}

/**
 * True only when the backend stated that this usage log has no value-detail row.
 *
 * The GET answers `404 REQUEST_AUDIT_VALUE_DETAIL_NOT_FOUND` for "no row" and uses
 * other statuses for the outcomes that are not absence: `500`/`503` when the row
 * could not be read, `409` for a row that retained nothing and `410` for values
 * that were retained and are now unavailable. The API client rejects with a plain
 * `{ status, code, message }` for HTTP failures and `{ status: 0 }` when the
 * request never reached the server, so a missing status is not a 404 either.
 *
 * Anything that is not a 404 must not be shown as "not collected": a failure is not
 * a collection result, and a request the server never answered proves nothing.
 */
export function isRequestAuditValueDetailNotFound(error: unknown): boolean {
  const candidate = error as { status?: unknown } | null | undefined
  return candidate?.status === 404
}

/** True when a `stored` value detail is already past its own 7-day window. */
export function isRequestAuditValueDetailExpired(
  expiresAt: string | undefined,
  now: number = Date.now(),
): boolean {
  if (typeof expiresAt !== 'string') return false
  const parsed = Date.parse(expiresAt)
  if (Number.isNaN(parsed)) return false
  return parsed <= now
}

/** The values may be revealed only while they really exist and are still readable. */
export function canRevealRequestAuditValueDetail(
  state: RequestAuditValueDetailState | undefined,
  expiresAt: string | undefined,
  now: number = Date.now(),
): boolean {
  return state === 'stored' && !isRequestAuditValueDetailExpired(expiresAt, now)
}

export interface AdminUsageQueryParams extends UsageQueryParams {
  user_id?: number
  exact_total?: boolean
  billing_mode?: string
  upstream_model_mismatch?: boolean
  sort_by?: string
  sort_order?: 'asc' | 'desc'
  // 错误请求 tab 专属筛选(仅传给错误列表接口;共用同一 filters 对象)
  error_phase?: string | null
  error_category?: string | null
  status_code?: number | null
}

// ==================== API Functions ====================

/**
 * Short-lived value reveals must not be cached by a browser, a proxy or a shared
 * cache. The server sets the same header; sending it on the request as well keeps
 * an intermediary from serving a copy even when it ignores the response header.
 */
const NO_STORE_HEADERS = {
  'Cache-Control': 'no-store',
  Pragma: 'no-cache'
} as const

/**
 * List all usage logs with optional filters (admin only)
 * @param params - Query parameters for filtering and pagination
 * @returns Paginated list of usage logs
 */
export async function list(
  params: AdminUsageQueryParams,
  options?: { signal?: AbortSignal }
): Promise<PaginatedResponse<AdminUsageLog>> {
  const { data } = await apiClient.get<PaginatedResponse<AdminUsageLog>>('/admin/usage', {
    params,
    signal: options?.signal
  })
  return data
}

/**
 * Get usage statistics with optional filters (admin only)
 * @param params - Query parameters for filtering
 * @returns Usage statistics
 */
export async function getStats(params: {
  user_id?: number
  api_key_id?: number
  account_id?: number
  group_id?: number
  model?: string
  request_type?: UsageRequestType
  stream?: boolean
  native_compaction_v2?: boolean | null
  upstream_model_mismatch?: boolean
  period?: string
  start_date?: string
  end_date?: string
  timezone?: string
  nocache?: number
}): Promise<AdminUsageStatsResponse> {
  const { data } = await apiClient.get<AdminUsageStatsResponse>('/admin/usage/stats', {
    params
  })
  return data
}

/**
 * Search users by email keyword (admin only)
 * @param keyword - Email keyword to search
 * @returns List of matching users (max 30)
 */
export async function searchUsers(keyword: string): Promise<SimpleUser[]> {
  const { data } = await apiClient.get<SimpleUser[]>('/admin/usage/search-users', {
    params: { q: keyword }
  })
  return data
}

/**
 * Search API keys by user ID and/or keyword (admin only)
 * @param userId - Optional user ID to filter by
 * @param keyword - Optional keyword to search in key name
 * @returns List of matching API keys (max 30)
 */
export async function searchApiKeys(userId?: number, keyword?: string): Promise<SimpleApiKey[]> {
  const params: Record<string, unknown> = {}
  if (userId !== undefined) {
    params.user_id = userId
  }
  if (keyword) {
    params.q = keyword
  }
  const { data } = await apiClient.get<SimpleApiKey[]>('/admin/usage/search-api-keys', {
    params
  })
  return data
}

/**
 * List usage cleanup tasks (admin only)
 * @param params - Query parameters for pagination
 * @returns Paginated list of cleanup tasks
 */
export async function listCleanupTasks(
  params: { page?: number; page_size?: number },
  options?: { signal?: AbortSignal }
): Promise<PaginatedResponse<UsageCleanupTask>> {
  const { data } = await apiClient.get<PaginatedResponse<UsageCleanupTask>>('/admin/usage/cleanup-tasks', {
    params,
    signal: options?.signal
  })
  return data
}

/**
 * Create a usage cleanup task (admin only)
 * @param payload - Cleanup task parameters
 * @returns Created cleanup task
 */
export async function createCleanupTask(payload: CreateUsageCleanupTaskRequest): Promise<UsageCleanupTask> {
  const { data } = await apiClient.post<UsageCleanupTask>('/admin/usage/cleanup-tasks', payload)
  return data
}

/**
 * Cancel a usage cleanup task (admin only)
 * @param taskId - Task ID to cancel
 */
export async function cancelCleanupTask(taskId: number): Promise<{ id: number; status: string }> {
  const { data } = await apiClient.post<{ id: number; status: string }>(
    `/admin/usage/cleanup-tasks/${taskId}/cancel`
  )
  return data
}

/**
 * Get request-audit metadata for a usage log (admin only).
 * Never includes model body or credential plaintext.
 */
export async function getRequestAudit(
  id: number,
  options?: { signal?: AbortSignal }
): Promise<RequestAudit> {
  const { data } = await apiClient.get<RequestAudit>(`/admin/usage/${id}/request-audit`, {
    signal: options?.signal
  })
  return data
}

/**
 * Get the value-detail envelope for a usage log (admin only).
 *
 * The envelope is metadata: state, reason, counts and the 7-day deadline. It has
 * no value fields, which is why reading it on open is safe even though the values
 * themselves are only ever fetched by an explicit reveal.
 */
export async function getRequestAuditValueDetail(
  id: number,
  options?: { signal?: AbortSignal }
): Promise<RequestAuditValueDetailEnvelope> {
  const { data } = await apiClient.get<unknown>(`/admin/usage/${id}/request-audit/value-detail`, {
    headers: NO_STORE_HEADERS,
    signal: options?.signal
  })
  return normalizeRequestAuditValueDetailEnvelope(data)
}

/**
 * Decrypts and returns the short-term values for a usage log.
 *
 * Only call this from an explicit admin action. It is a POST on purpose: a GET
 * could be issued by a prefetcher, a speculator or a cache before the admin ever
 * asked for it, and its URL would end up in history. The response is marked
 * no-store so no intermediary keeps a copy even if it ignores the response header.
 */
export async function revealRequestAuditValueDetail(
  id: number
): Promise<RequestAuditValueDetailReveal> {
  const { data } = await apiClient.post<unknown>(
    `/admin/usage/${id}/request-audit/value-detail`,
    undefined,
    { headers: NO_STORE_HEADERS }
  )
  return normalizeRequestAuditValueDetailReveal(data)
}

export const adminUsageAPI = {
  list,
  getStats,
  searchUsers,
  searchApiKeys,
  listCleanupTasks,
  createCleanupTask,
  cancelCleanupTask,
  getRequestAudit,
  getRequestAuditValueDetail,
  revealRequestAuditValueDetail
}

export default adminUsageAPI
