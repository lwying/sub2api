/**
 * Presentation helpers for the diagnostics UI.
 *
 * Every helper maps a closed-set wire value to an i18n key. A value outside the
 * set (an older or tampered record, a future literal) resolves to an explicit
 * "unknown" label — the raw value is never returned as display text.
 */
import type { DiagnosticBodyReason, DiagnosticBodyState, DiagnosticProtocol, OperatorAckLanguage } from './types'
import { isBodyExpired } from './types'

export type Translate = (key: string, params?: Record<string, unknown>) => string

const PROTOCOL_KEYS: Record<DiagnosticProtocol, string> = {
  messages: 'messages',
  chat_completions: 'chat_completions',
  responses: 'responses',
}

const BODY_STATE_KEYS: Record<DiagnosticBodyState, string> = {
  not_observed: 'notObserved',
  stored: 'stored',
  skipped: 'skipped',
  expired: 'expired',
  purged: 'purged',
}

const BODY_REASON_KEYS: Record<DiagnosticBodyReason, string> = {
  not_observed: 'not_observed',
  retained: 'retained',
  skipped_not_text_json: 'skipped_not_text_json',
  skipped_too_large: 'skipped_too_large',
  skipped_attachment: 'skipped_attachment',
  skipped_known_credential: 'skipped_known_credential',
  skipped_incomplete_read: 'skipped_incomplete_read',
  skipped_encryption_unavailable: 'skipped_encryption_unavailable',
  skipped_body_retention_disabled: 'skipped_body_retention_disabled',
}

export function protocolLabel(t: Translate, protocol: string | undefined): string {
  const key = protocol ? PROTOCOL_KEYS[protocol as DiagnosticProtocol] : undefined
  return t(`admin.errorDiagnostics.protocols.${key ?? 'unknown'}`)
}

export function bodyStateLabel(t: Translate, state: string | undefined): string {
  const key = state ? BODY_STATE_KEYS[state as DiagnosticBodyState] : undefined
  return t(`admin.errorDiagnostics.bodyStates.${key ?? 'unknown'}`)
}

export function bodyReasonLabel(t: Translate, reason: string | undefined): string {
  const key = reason ? BODY_REASON_KEYS[reason as DiagnosticBodyReason] : undefined
  return t(`admin.errorDiagnostics.reasons.${key ?? 'unknown'}`)
}

/**
 * A body may be revealed only while it is really readable: `stored` and not past
 * its own expiry. Expired, purged, skipped and not-observed bodies never get an
 * action, so the UI cannot invite a read the server must refuse.
 */
export function canRevealBody(
  state: string | undefined,
  bodyExpiresAt: string | undefined,
  now: number = Date.now(),
): boolean {
  return state === 'stored' && !isBodyExpired(bodyExpiresAt, now)
}

/** True when `stored` has silently become unreadable because its window passed. */
export function isStoredButExpired(
  state: string | undefined,
  bodyExpiresAt: string | undefined,
  now: number = Date.now(),
): boolean {
  return state === 'stored' && isBodyExpired(bodyExpiresAt, now)
}

export function formatDateTime(value: string | undefined): string {
  if (!value) return '-'
  const parsed = Date.parse(value)
  if (Number.isNaN(parsed)) return '-'
  return new Date(parsed).toLocaleString()
}

const ACK_LANGUAGE_KEYS: Record<OperatorAckLanguage, string> = {
  en: 'en',
  zh: 'zh',
}

export function ackLanguageLabel(t: Translate, language: OperatorAckLanguage): string {
  return t(`admin.errorDiagnostics.operator.languages.${ACK_LANGUAGE_KEYS[language]}`)
}

/**
 * The server compares the submitted statement with the expected one after trimming
 * whitespace only — no case folding, no punctuation repair. The client must not be
 * more permissive than that, or the operator would be told a statement matches that
 * the server will refuse.
 */
export function matchesRiskPhrase(typed: string, required: string): boolean {
  return required !== '' && typed.trim() === required
}

/**
 * Stable reason codes -> copy. An unknown or missing reason (a malformed request
 * body or a storage failure comes back without one) is a generic rejection: it is
 * never rendered as success and never echoed as raw server text.
 */
const OPERATOR_ERROR_KEYS: Record<string, string> = {
  ERROR_DIAGNOSTIC_RISK_ACK_REQUIRED: 'phraseRequired',
  ERROR_DIAGNOSTIC_RISK_ACK_INVALID: 'phraseInvalid',
  ERROR_DIAGNOSTIC_BODY_KEY_UNAVAILABLE: 'keyUnavailable',
  ERROR_DIAGNOSTIC_OPERATOR_SESSION_REQUIRED: 'sessionRequired',
  ERROR_DIAGNOSTIC_ADMIN_API_KEY_FORBIDDEN: 'adminApiKeyForbidden',
  ERROR_DIAGNOSTIC_SETTINGS_UNAVAILABLE: 'unavailable',
}

export function operatorErrorMessage(t: Translate, reason: unknown): string {
  const key = typeof reason === 'string' ? OPERATOR_ERROR_KEYS[reason] : undefined
  return t(`admin.errorDiagnostics.operator.errors.${key ?? 'generic'}`)
}
