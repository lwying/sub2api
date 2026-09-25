/**
 * Types and payload normalization for the admin error-request diagnostics
 * (tickets 01-04).
 *
 * The diagnostics record is short-lived, sensitive and deliberately narrow: one
 * row per real upstream HTTP attempt, metadata only. The stored request body is
 * never part of a list or detail payload and is only ever returned by the
 * explicit, no-store reveal call.
 *
 * Two rules keep the frontend honest about that:
 *
 *   1. `protocol`, `body_state` and `reason` are closed sets. An unknown literal
 *      is rejected (list/detail fail) or dropped (reason) — it is never rendered
 *      as if it were a known state, and never interpolated into the DOM.
 *   2. Normalization copies an explicit allowlist of fields. Any extra key a
 *      backend build or a tampered response might add (a body, a credential, a
 *      model alias, an account id) is dropped at the boundary, so no component
 *      can leak it by rendering the row object.
 *
 * The state/reason literals mirror the persisted storage enums exactly; do not
 * invent values here. `skipped_encryption_unavailable` intentionally collapses
 * "no key configured" and "encryption failed" so the UI cannot act as an oracle.
 */
import type { PaginatedResponse } from '@/types'

export const DIAGNOSTIC_PROTOCOLS = ['messages', 'chat_completions', 'responses'] as const
export type DiagnosticProtocol = (typeof DIAGNOSTIC_PROTOCOLS)[number]

export const DIAGNOSTIC_BODY_STATES = ['not_observed', 'stored', 'skipped', 'expired', 'purged'] as const
export type DiagnosticBodyState = (typeof DIAGNOSTIC_BODY_STATES)[number]

export const DIAGNOSTIC_BODY_REASONS = [
  'not_observed',
  'retained',
  'skipped_not_text_json',
  'skipped_too_large',
  'skipped_attachment',
  'skipped_known_credential',
  'skipped_incomplete_read',
  'skipped_encryption_unavailable',
  'skipped_body_retention_disabled',
] as const
export type DiagnosticBodyReason = (typeof DIAGNOSTIC_BODY_REASONS)[number]

/**
 * Retention state of the 429 header values. Same closed set, and same meaning,
 * as a body state: it is never inferred locally from a reveal failure.
 */
export const DIAGNOSTIC_HEADER_STATES = ['not_observed', 'stored', 'skipped', 'expired', 'purged'] as const
export type DiagnosticHeaderState = (typeof DIAGNOSTIC_HEADER_STATES)[number]

/**
 * Why header values were or were not retained. `not_observed` is the state of
 * every attempt that is not a Messages 429 — out of scope is "not collected",
 * not a failure.
 */
export const DIAGNOSTIC_HEADER_REASONS = [
  'not_observed',
  'retained',
  'skipped_out_of_scope',
  'skipped_header_retention_disabled',
  'skipped_encryption_unavailable',
  'skipped_invalid_values',
] as const
export type DiagnosticHeaderReason = (typeof DIAGNOSTIC_HEADER_REASONS)[number]

/** One real upstream HTTP attempt that received a 4xx/5xx. Metadata only. */
export interface DiagnosticAttempt {
  /** Opaque server-side identifier; never a database row id the client can guess. */
  id: string
  created_at: string
  protocol: DiagnosticProtocol
  attempt_index: number
  upstream_status: number
  /** Present only when the attempt is linked to a usage record. */
  usage_log_id?: number
  body_state: DiagnosticBodyState
  /**
   * Stable retention outcome code. Always present server-side; dropped here when
   * it is not one of the known literals so nothing untrusted reaches a label.
   */
  reason?: DiagnosticBodyReason
  /** Present whenever a body was ever retained (stored / expired / purged). */
  body_expires_at?: string
  metadata_expires_at: string

  /**
   * 429 header values (Claude Messages only). Structurally identical to the body
   * fields but an independent fact: headers can be retained when no body was, and
   * a missing `header_state` means the same as `not_observed` — the conservative
   * reading, which can never imply that values are available.
   */
  header_state?: DiagnosticHeaderState
  header_reason?: DiagnosticHeaderReason
  header_entry_count?: number
  /** Present whenever header values were ever retained (stored / expired / purged). */
  header_expires_at?: string
}

export type DiagnosticAttemptPage = PaginatedResponse<DiagnosticAttempt>

export interface DiagnosticBodyReveal {
  body_text: string
  body_bytes: number
}

/** Raised when a payload does not match the agreed diagnostics contract. */
export class DiagnosticPayloadError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'DiagnosticPayloadError'
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.trim() !== ''
}

function isIntegerInRange(value: unknown, min: number, max: number): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= min && value <= max
}

/** A usable timestamp string; anything unparseable is treated as absent. */
function isTimestamp(value: unknown): value is string {
  return isNonEmptyString(value) && !Number.isNaN(Date.parse(value))
}

function oneOf<T extends string>(allowed: readonly T[], value: unknown): value is T {
  return typeof value === 'string' && (allowed as readonly string[]).includes(value)
}

export function normalizeDiagnosticAttempt(raw: unknown): DiagnosticAttempt {
  if (!isRecord(raw)) {
    throw new DiagnosticPayloadError('diagnostic payload is not an object')
  }

  const { id, created_at: createdAt, protocol, attempt_index: attemptIndex } = raw
  const { upstream_status: upstreamStatus, body_state: bodyState, metadata_expires_at: metadataExpiresAt } = raw

  if (!isNonEmptyString(id)) {
    throw new DiagnosticPayloadError('diagnostic id is missing')
  }
  if (!isTimestamp(createdAt)) {
    throw new DiagnosticPayloadError('diagnostic created_at is missing')
  }
  if (!isTimestamp(metadataExpiresAt)) {
    throw new DiagnosticPayloadError('diagnostic metadata_expires_at is missing')
  }
  if (!oneOf(DIAGNOSTIC_PROTOCOLS, protocol)) {
    throw new DiagnosticPayloadError('diagnostic protocol is not a known value')
  }
  if (!isIntegerInRange(attemptIndex, 1, Number.MAX_SAFE_INTEGER)) {
    throw new DiagnosticPayloadError('diagnostic attempt_index is not a positive integer')
  }
  if (!isIntegerInRange(upstreamStatus, 100, 599)) {
    throw new DiagnosticPayloadError('diagnostic upstream_status is not an HTTP status')
  }
  if (!oneOf(DIAGNOSTIC_BODY_STATES, bodyState)) {
    throw new DiagnosticPayloadError('diagnostic body_state is not a known value')
  }

  const attempt: DiagnosticAttempt = {
    id,
    created_at: createdAt,
    protocol,
    attempt_index: attemptIndex,
    upstream_status: upstreamStatus,
    body_state: bodyState,
    metadata_expires_at: metadataExpiresAt,
  }

  // An absence is a fact ("no usage record"), so a missing usage link is not an
  // error; a malformed one is simply not carried through.
  if (isIntegerInRange(raw.usage_log_id, 1, Number.MAX_SAFE_INTEGER)) {
    attempt.usage_log_id = raw.usage_log_id
  }
  if (oneOf(DIAGNOSTIC_BODY_REASONS, raw.reason)) {
    attempt.reason = raw.reason
  }
  if (isTimestamp(raw.body_expires_at)) {
    attempt.body_expires_at = raw.body_expires_at
  }

  // The header fields are additive: an older row that does not carry them is read
  // as "no header values were observed", never as if a header payload existed.
  if (raw.header_state !== undefined) {
    if (!oneOf(DIAGNOSTIC_HEADER_STATES, raw.header_state)) {
      throw new DiagnosticPayloadError('diagnostic header_state is not a known value')
    }
    attempt.header_state = raw.header_state
  }
  if (oneOf(DIAGNOSTIC_HEADER_REASONS, raw.header_reason)) {
    attempt.header_reason = raw.header_reason
  }
  if (isIntegerInRange(raw.header_entry_count, 0, Number.MAX_SAFE_INTEGER)) {
    attempt.header_entry_count = raw.header_entry_count
  }
  if (isTimestamp(raw.header_expires_at)) {
    attempt.header_expires_at = raw.header_expires_at
  }

  return attempt
}

export function normalizeDiagnosticAttemptPage(raw: unknown): DiagnosticAttemptPage {
  if (!isRecord(raw) || !Array.isArray(raw.items)) {
    throw new DiagnosticPayloadError('diagnostic page is not a list payload')
  }

  const items = raw.items.map(normalizeDiagnosticAttempt)
  const total = isIntegerInRange(raw.total, 0, Number.MAX_SAFE_INTEGER) ? raw.total : items.length
  const page = isIntegerInRange(raw.page, 1, Number.MAX_SAFE_INTEGER) ? raw.page : 1
  const pageSize = isIntegerInRange(raw.page_size, 1, Number.MAX_SAFE_INTEGER) ? raw.page_size : items.length || 1
  const pages = isIntegerInRange(raw.pages, 0, Number.MAX_SAFE_INTEGER)
    ? raw.pages
    : Math.ceil(total / pageSize)

  return { items, total, page, page_size: pageSize, pages }
}

export function normalizeDiagnosticBodyReveal(raw: unknown): DiagnosticBodyReveal {
  if (!isRecord(raw) || typeof raw.body_text !== 'string') {
    throw new DiagnosticPayloadError('revealed body payload has no text')
  }
  if (!isIntegerInRange(raw.body_bytes, 0, Number.MAX_SAFE_INTEGER)) {
    throw new DiagnosticPayloadError('revealed body payload has no byte count')
  }

  return { body_text: raw.body_text, body_bytes: raw.body_bytes }
}

/** True when a `stored` body is already past its own expiry and must not be read. */
export function isBodyExpired(bodyExpiresAt: string | undefined, now: number = Date.now()): boolean {
  if (!isTimestamp(bodyExpiresAt)) return false
  return Date.parse(bodyExpiresAt) <= now
}

// ---------------------------------------------------------------------------
// 429 header values (Claude Messages only)
// ---------------------------------------------------------------------------

/**
 * The decrypted 429 header values, by direction, plus their entry count and the
 * moment they stop being readable.
 *
 * The server already binds these to canonical, allowlisted names and refuses to
 * capture credential headers at all. This module re-validates anyway, because the
 * payload crosses a trust boundary: an unknown header name must not reach the DOM
 * (an unlisted header can be an authentication channel), and neither must a
 * credential-shaped value sitting under an allowlisted name.
 */
export interface DiagnosticHeaderReveal {
  request_headers: Record<string, string>
  response_headers: Record<string, string>
  header_entry_count: number
  header_expires_at?: string
}

/**
 * Canonical names this surface may render, by direction, compared lower-cased.
 * Mirrors the persisted header-value allowlist for this route, name for name; a
 * name outside it is dropped rather than displayed.
 *
 * Mirroring matters for the count, not only for display: the entry count the
 * server reports is the number of entries it retained, so a name retained
 * server-side but missing here makes the rendered list disagree with that count.
 * `anthropic-dangerous-direct-browser-access` is exactly such a name — the
 * default Claude Code OAuth client sends it on every request.
 */
const DIAGNOSTIC_REQUEST_HEADER_NAMES: Record<string, string> = {
  host: 'Host',
  'user-agent': 'User-Agent',
  'anthropic-version': 'Anthropic-Version',
  'anthropic-beta': 'Anthropic-Beta',
  'anthropic-dangerous-direct-browser-access': 'Anthropic-Dangerous-Direct-Browser-Access',
  'accept-language': 'Accept-Language',
  accept: 'Accept',
  'accept-encoding': 'Accept-Encoding',
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

const DIAGNOSTIC_RESPONSE_HEADER_NAMES: Record<string, string> = {
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
 * Credential headers. They are absent from the allowlists above, so this set is
 * an explicit second refusal: a credential must never be rendered even if an
 * allowlist is ever widened by mistake.
 */
const DIAGNOSTIC_CREDENTIAL_HEADER_NAMES = new Set([
  'authorization',
  'cookie',
  'set-cookie',
  'api-key',
  'x-api-key',
  'proxy-authorization',
  'proxy-authenticate',
  'www-authenticate',
])

/** Value-shaped credential markers; the same second line of defence the backend applies. */
const CREDENTIAL_VALUE_MARKERS = [
  'bearer ',
  'basic ',
  'sk-',
  'sk_',
  'ghp_',
  'gho_',
  'xoxb-',
  'xoxp-',
  '-----begin',
  'apikey',
  'api_key',
  'api-key',
  'password',
  'passwd',
  'secret',
  'cookie',
  'session=',
  'access_token',
  'refresh_token',
]

const MAX_HEADER_VALUE_BYTES = 256
const MAX_HEADER_ENTRIES = 40

function hasControlCharacter(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index)
    if (code < 0x20 || code === 0x7f) return true
  }
  return false
}

function isRenderableHeaderValue(value: string): boolean {
  if (value === '' || value.length > MAX_HEADER_VALUE_BYTES) return false
  if (hasControlCharacter(value)) return false
  const lower = value.toLowerCase()
  return !CREDENTIAL_VALUE_MARKERS.some((marker) => lower.includes(marker))
}

/**
 * Accepts one direction, dropping every name or value that is not explicitly
 * permitted.
 *
 * Dropping rather than failing keeps the other direction readable; an entry that
 * fails here is evidence of a tampered or newer payload, and the entry count the
 * server reported is what tells the admin how many entries existed at all.
 */
function normalizeHeaderDirection(
  raw: unknown,
  allowed: Record<string, string>,
): Record<string, string> {
  if (raw === undefined || raw === null) return {}
  if (!isRecord(raw)) {
    throw new DiagnosticPayloadError('revealed header values are not an object')
  }

  const headers: Record<string, string> = {}
  for (const [rawName, rawValue] of Object.entries(raw)) {
    if (Object.keys(headers).length >= MAX_HEADER_ENTRIES) break
    const name = rawName.trim().toLowerCase()
    if (DIAGNOSTIC_CREDENTIAL_HEADER_NAMES.has(name)) continue
    const canonical = allowed[name]
    if (!canonical) continue
    if (typeof rawValue !== 'string') continue
    const value = rawValue.trim()
    if (!isRenderableHeaderValue(value)) continue
    headers[canonical] = value
  }
  return headers
}

export function normalizeDiagnosticHeaderReveal(raw: unknown): DiagnosticHeaderReveal {
  if (!isRecord(raw)) {
    throw new DiagnosticPayloadError('revealed headers payload is not an object')
  }

  const requestHeaders = normalizeHeaderDirection(raw.request_headers, DIAGNOSTIC_REQUEST_HEADER_NAMES)
  const responseHeaders = normalizeHeaderDirection(raw.response_headers, DIAGNOSTIC_RESPONSE_HEADER_NAMES)

  const reveal: DiagnosticHeaderReveal = {
    request_headers: requestHeaders,
    response_headers: responseHeaders,
    // The server's own count is the honest total: this view may hold fewer
    // entries than were retained, and reporting the local count as the total
    // would make a filtered payload look complete.
    header_entry_count: isIntegerInRange(raw.header_entry_count, 0, Number.MAX_SAFE_INTEGER)
      ? raw.header_entry_count
      : Object.keys(requestHeaders).length + Object.keys(responseHeaders).length,
  }
  if (isTimestamp(raw.header_expires_at)) {
    reveal.header_expires_at = raw.header_expires_at
  }
  return reveal
}

/** True when `stored` header values are already past their own expiry. */
export function isHeaderValuesExpired(
  headerExpiresAt: string | undefined,
  now: number = Date.now(),
): boolean {
  if (!isTimestamp(headerExpiresAt)) return false
  return Date.parse(headerExpiresAt) <= now
}

// ---------------------------------------------------------------------------
// Operator gate (GET/PUT /admin/settings/error-diagnostic)
// ---------------------------------------------------------------------------

/**
 * Languages the written risk acknowledgement can be given in. The server decides
 * which statement a request has to match (`zh*` -> zh, everything else -> en), so
 * the UI only ever offers these two and always shows the server's own text.
 */
export const OPERATOR_ACK_LANGUAGES = ['en', 'zh'] as const
export type OperatorAckLanguage = (typeof OPERATOR_ACK_LANGUAGES)[number]

/** The recorded acknowledgement, allowlisted to the four fields that may be shown. */
export interface ErrorDiagnosticRiskAcknowledgementView {
  version: string
  phrase: string
  admin_user_id: number
  accepted_at: string
}

/**
 * Current state of the capture gate, as the server reports it.
 *
 * `enabled` / `risk_acknowledged` / `body_retention_enabled` /
 * `header_values_enabled` are the stored flags, while `capture_allowed` /
 * `body_retention_allowed` / `header_values_allowed` are the **verified**
 * conclusions: capture also requires a valid acknowledgement of the current
 * statement, and each retention layer additionally requires its own stored flag
 * and a usable key. A stored flag that is not backed by a current
 * acknowledgement is therefore a deliberate, reportable state (`enabled` on,
 * `capture_allowed` off), not an inconsistency — and both sides of it are carried
 * here verbatim, because the server is the authority and nothing is derived from
 * the other locally.
 *
 * The two retention layers are orthogonal: body retention can be off while 429
 * header value retention is on, and the other way round. `body_encryption_key_available`
 * is the availability of the one stable key both layers encrypt with, so it gates
 * both toggles.
 */
export interface ErrorDiagnosticOperatorStatus {
  enabled: boolean
  risk_acknowledged: boolean
  body_retention_enabled: boolean
  /** Stored flag of the 429 header value layer (Claude Messages only). */
  header_values_enabled: boolean
  capture_allowed: boolean
  body_retention_allowed: boolean
  /** Verified conclusion of the 429 header value layer. */
  header_values_allowed: boolean
  body_encryption_key_available: boolean
  risk_version: string
  risk_phrase_en: string
  risk_phrase_zh: string
  risk_acknowledgement?: ErrorDiagnosticRiskAcknowledgementView
  /** False both when no record exists and when the record covers an older statement. */
  risk_acknowledgement_current: boolean
}

/**
 * One whole-state update. The server treats an omitted field as off, so both
 * retention layers are always stated explicitly; the server refuses either of
 * them without a freshly typed statement and a usable key.
 */
export interface ErrorDiagnosticOperatorUpdateInput {
  enabled: boolean
  body_retention_enabled: boolean
  header_values_enabled: boolean
  language: OperatorAckLanguage
  phrase: string
}

/** Every gate flag must be a real boolean; a missing one must not be read as false. */
const OPERATOR_GATE_FLAGS = [
  'enabled',
  'risk_acknowledged',
  'body_retention_enabled',
  'header_values_enabled',
  'capture_allowed',
  'body_retention_allowed',
  'header_values_allowed',
  'body_encryption_key_available',
] as const

function normalizeRiskAcknowledgement(raw: unknown): ErrorDiagnosticRiskAcknowledgementView | undefined {
  if (!isRecord(raw)) return undefined
  // A record missing its version, operator or acceptance time is not evidence, so
  // it is reported as "no acknowledgement" rather than shown in a half-read state.
  if (!isNonEmptyString(raw.version) || !isNonEmptyString(raw.phrase)) return undefined
  if (!isIntegerInRange(raw.admin_user_id, 1, Number.MAX_SAFE_INTEGER)) return undefined
  if (!isTimestamp(raw.accepted_at)) return undefined

  return {
    version: raw.version,
    phrase: raw.phrase,
    admin_user_id: raw.admin_user_id,
    accepted_at: raw.accepted_at,
  }
}

/**
 * Normalizes the operator gate payload.
 *
 * `ip_address`, `user_agent`, keys and bodies are deliberately not part of the
 * result: the operator's own source identity stays in the audit record server-side
 * and never travels to this surface.
 */
export function normalizeErrorDiagnosticOperatorStatus(raw: unknown): ErrorDiagnosticOperatorStatus {
  if (!isRecord(raw)) {
    throw new DiagnosticPayloadError('operator status payload is not an object')
  }

  for (const flag of OPERATOR_GATE_FLAGS) {
    if (typeof raw[flag] !== 'boolean') {
      // "Could not be read" must not be rendered as "off": fail instead of defaulting.
      throw new DiagnosticPayloadError(`operator status ${flag} is not a boolean`)
    }
  }
  if (!isNonEmptyString(raw.risk_version)) {
    throw new DiagnosticPayloadError('operator status risk_version is missing')
  }
  if (!isNonEmptyString(raw.risk_phrase_en) || !isNonEmptyString(raw.risk_phrase_zh)) {
    throw new DiagnosticPayloadError('operator status risk statement is missing')
  }

  const status: ErrorDiagnosticOperatorStatus = {
    enabled: raw.enabled as boolean,
    risk_acknowledged: raw.risk_acknowledged as boolean,
    body_retention_enabled: raw.body_retention_enabled as boolean,
    header_values_enabled: raw.header_values_enabled as boolean,
    capture_allowed: raw.capture_allowed as boolean,
    body_retention_allowed: raw.body_retention_allowed as boolean,
    header_values_allowed: raw.header_values_allowed as boolean,
    body_encryption_key_available: raw.body_encryption_key_available as boolean,
    risk_version: raw.risk_version as string,
    risk_phrase_en: raw.risk_phrase_en as string,
    risk_phrase_zh: raw.risk_phrase_zh as string,
    risk_acknowledgement_current: raw.risk_acknowledgement_current === true,
  }

  const acknowledgement = normalizeRiskAcknowledgement(raw.risk_acknowledgement)
  if (acknowledgement) {
    status.risk_acknowledgement = acknowledgement
  }

  return status
}
