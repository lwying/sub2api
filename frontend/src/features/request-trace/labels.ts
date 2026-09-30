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
  type RequestTraceExportFilter,
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

function summaryDate(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

/**
 * What an export will cover, stated in the query's own facet names.
 *
 * It renders a filter that was **executed**, so the operator can see the scope
 * the export carries rather than the one still sitting in the form. A scope with
 * no condition says so: "everything" is a scope, and leaving the line blank
 * would let the operator read it as "nothing".
 */
export function exportScopeSummary(t: Translate, filter: RequestTraceExportFilter): string {
  const parts: string[] = []
  if (filter.trace_ids?.length) parts.push(t('admin.requestTrace.export.scope.selected', { count: filter.trace_ids.length }))
  if (filter.trace_id) parts.push(`${t('admin.requestTrace.list.traceId')}: ${filter.trace_id}`)
  if (filter.route_family) parts.push(t(`admin.requestTrace.list.${filter.route_family}`))
  if (filter.client_status !== undefined) parts.push(`${t('admin.requestTrace.list.status')}: ${filter.client_status}`)
  if (filter.usage_linked !== undefined) parts.push(t(filter.usage_linked ? 'admin.requestTrace.list.linked' : 'admin.requestTrace.list.unlinked'))
  if (filter.usage_log_id !== undefined) parts.push(`${t('admin.requestTrace.list.usage')}: #${filter.usage_log_id}`)
  if (filter.account_id !== undefined) parts.push(`${t('admin.requestTrace.list.accountId')}: #${filter.account_id}`)
  if (filter.group_id !== undefined) parts.push(`${t('admin.requestTrace.list.group')}: #${filter.group_id}`)
  if (filter.group_unknown) parts.push(`${t('admin.requestTrace.list.group')}: ${t('admin.requestTrace.list.unknownValue')}`)
  if (filter.requested_model) parts.push(`${t('admin.requestTrace.list.requestedModel')}: ${filter.requested_model}`)
  if (filter.model_unknown) parts.push(`${t('admin.requestTrace.list.requestedModel')}: ${t('admin.requestTrace.list.unknownValue')}`)
  if (filter.platform) parts.push(`${t('admin.requestTrace.list.platform')}: ${filter.platform}`)
  if (filter.platform_unknown) parts.push(`${t('admin.requestTrace.list.platform')}: ${t('admin.requestTrace.list.unknownValue')}`)
  if (filter.created_from) parts.push(`${t('admin.requestTrace.list.from')}: ${summaryDate(filter.created_from)}`)
  if (filter.created_to) parts.push(`${t('admin.requestTrace.list.to')}: ${summaryDate(filter.created_to)}`)
  return parts.length ? parts.join(' · ') : t('admin.requestTrace.export.scope.none')
}
