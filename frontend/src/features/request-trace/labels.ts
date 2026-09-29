/**
 * Presentation helpers for the Trace UI.
 *
 * Every helper maps a closed-set wire value to an i18n key. A value outside the
 * set (an older or tampered record, a future literal) resolves to an explicit
 * generic label — the raw value is never returned as display text.
 */
import {
  requestTraceCaptureStates,
  requestTraceStageNames,
  requestTraceStageReasons,
  requestTraceStageStates,
  requestTraceStageViews,
  type TraceAckLanguage,
} from './types'

export type Translate = (key: string, params?: Record<string, unknown>) => string

const DETAIL_PREFIX = 'admin.requestTrace.detail'
const LIST_PREFIX = 'admin.requestTrace.list'

/**
 * The one key a closed value renders through: its own label, or the fallback when
 * the value is not part of the set the frontend knows about.
 */
function closedSetKey(prefix: string, scope: string, values: readonly string[], value: string, fallback: string): string {
  return `${prefix}.${scope}.${values.includes(value) ? value : fallback}`
}

/** Which client request/response body view a stage reports. */
export function stageViewLabel(t: Translate, viewName: string): string {
  return t(closedSetKey(DETAIL_PREFIX, 'viewLabel', requestTraceStageViews, viewName, 'unknown'))
}

/** The stage name, e.g. `wire_attempt`. */
export function stageLabel(t: Translate, stage: string): string {
  return t(closedSetKey(DETAIL_PREFIX, 'stageLabel', requestTraceStageNames, stage, 'unknown'))
}

/** A stage's body state, e.g. `redaction_unverified`. */
export function stageStateLabel(t: Translate, state: string): string {
  return t(closedSetKey(DETAIL_PREFIX, 'stateLabel', requestTraceStageStates, state, 'unknown'))
}

/** A summary row's capture state, e.g. `partial`. */
export function captureStateLabel(t: Translate, state: string): string {
  return t(closedSetKey(LIST_PREFIX, 'captureStateLabel', requestTraceCaptureStates, state, 'unknown'))
}

/**
 * The reason code a stage reports. A reason is only bounded by a pattern server
 * side, so an unknown code is labelled generically; the raw code is never the
 * visible text (a caller may still surface it in a non-display attribute).
 */
export function stageReasonLabel(t: Translate, reason: string): string {
  return t(closedSetKey(DETAIL_PREFIX, 'reasonLabel', requestTraceStageReasons, reason, 'other'))
}

/** The language the risk statement is acknowledged in, as its own name. */
export function ackLanguageLabel(t: Translate, language: TraceAckLanguage): string {
  return t(`admin.requestTrace.operator.languages.${language === 'zh' ? 'zh' : 'en'}`)
}
