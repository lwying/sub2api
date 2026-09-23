<template>
  <BaseDialog :show="show" :title="t('admin.usage.requestAudit.title')" width="wide" @close="emit('update:show', false)">
    <div v-if="loading" class="flex justify-center py-10">
      <svg class="h-7 w-7 animate-spin text-primary-500" fill="none" viewBox="0 0 24 24">
        <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
        <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z" />
      </svg>
    </div>

    <div v-else-if="loadError" class="py-8 text-center text-sm text-red-500">
      {{ t('admin.usage.requestAudit.loadFailed') }}
    </div>

    <div v-else-if="detail" class="space-y-5 text-sm">
      <div class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-dark-400">
        {{ t('admin.usage.requestAudit.captureCompleteness') }}:
        <span data-testid="request-audit-capture-status" class="ml-1 font-mono text-gray-900 dark:text-dark-100">
          {{ captureCompletenessLabel(detail.capture_completeness) }}
        </span>
      </div>

      <div
        v-if="detail.capture_completeness === 'not_captured'"
        data-testid="request-audit-not-captured"
        class="rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-amber-800 dark:border-amber-800/60 dark:bg-amber-950/30 dark:text-amber-200"
      >
        {{ notCapturedReasonLabel(detail.capture_reason) }}
      </div>

      <div v-else>
        <div class="mb-2 font-medium text-gray-500 dark:text-dark-400">{{ t('admin.usage.requestAudit.attempts') }}</div>
        <ol v-if="detail.attempts?.length" class="space-y-2">
          <li
            v-for="(attempt, index) in detail.attempts"
            :key="index"
            data-testid="request-audit-attempt"
            class="rounded-lg border border-gray-200 bg-gray-50 px-3 py-2 dark:border-dark-700 dark:bg-dark-900"
          >
            <div class="flex flex-wrap gap-x-4 gap-y-1 font-mono text-xs text-gray-900 dark:text-dark-100">
              <span>{{ attempt.stage || '-' }}</span>
              <span>{{ attempt.protocol || '-' }}</span>
              <span v-if="attempt.account_id != null">{{ attempt.account_id }}</span>
              <span
                v-if="isModelFingerprintDigest(attempt.model_fingerprint)"
                data-testid="request-audit-attempt-model-fingerprint"
                :title="attempt.model_fingerprint"
                class="break-all"
              >
                <span class="text-gray-500 dark:text-dark-400">{{ t('admin.usage.requestAudit.modelFingerprint') }}</span>
                {{ shortModelFingerprint(attempt.model_fingerprint) }}
              </span>
            </div>
            <div v-if="attempt.stage === 'wire' || hasAttemptTelemetry(attempt)" class="mt-2 space-y-2 border-t border-gray-200 pt-2 dark:border-dark-700">
              <div v-if="attempt.stage === 'wire' || (attempt.wire_request_headers && Object.keys(attempt.wire_request_headers).length)">
                <div class="mb-1 text-xs font-medium text-gray-500 dark:text-dark-400">{{ t('admin.usage.requestAudit.wireRequestHeaders') }}</div>
                <dl v-if="attempt.wire_request_headers && Object.keys(attempt.wire_request_headers).length" class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 font-mono text-xs">
                  <template v-for="(value, key) in attempt.wire_request_headers" :key="`wire-${key}`">
                    <dt class="text-gray-500 dark:text-dark-400">{{ key }}</dt>
                    <dd class="break-all text-gray-900 dark:text-dark-100">{{ formatHeaderValue(value) }}</dd>
                  </template>
                </dl>
                <p v-else class="text-xs text-gray-500 dark:text-dark-400">{{ t('admin.usage.requestAudit.unknown') }}</p>
              </div>
              <div v-if="attempt.stage === 'wire' || (attempt.upstream_response_headers && Object.keys(attempt.upstream_response_headers).length)">
                <div class="mb-1 text-xs font-medium text-gray-500 dark:text-dark-400">{{ t('admin.usage.requestAudit.upstreamResponseHeaders') }}</div>
                <dl v-if="attempt.upstream_response_headers && Object.keys(attempt.upstream_response_headers).length" class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 font-mono text-xs">
                  <template v-for="(value, key) in attempt.upstream_response_headers" :key="`response-${key}`">
                    <dt class="text-gray-500 dark:text-dark-400">{{ key }}</dt>
                    <dd class="break-all text-gray-900 dark:text-dark-100">{{ formatHeaderValue(value) }}</dd>
                  </template>
                </dl>
                <p v-else class="text-xs text-gray-500 dark:text-dark-400">{{ t('admin.usage.requestAudit.unknown') }}</p>
              </div>
              <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-xs text-gray-700 dark:text-dark-200">
                <template v-if="attempt.stage === 'wire'">
                  <dt>{{ t('admin.usage.requestAudit.upstreamStatus') }}</dt>
                  <dd class="font-mono">{{ formatOptionalNumber(attempt.upstream_status) }}</dd>
                  <dt>{{ t('admin.usage.requestAudit.requestPayloadBytes') }}</dt>
                  <dd class="font-mono">{{ formatOptionalBytes(attempt.request_payload_bytes) }}</dd>
                  <dt>{{ t('admin.usage.requestAudit.responsePayloadBytes') }}</dt>
                  <dd class="font-mono">{{ formatOptionalBytes(attempt.response_payload_bytes) }}</dd>
                  <dt>{{ t('admin.usage.requestAudit.responseReadComplete') }}</dt>
                  <dd>{{ formatOptionalBoolean(attempt.response_read_complete) }}</dd>
                </template>
                <template v-else>
                  <template v-if="attempt.upstream_status !== undefined">
                    <dt>{{ t('admin.usage.requestAudit.upstreamStatus') }}</dt>
                    <dd class="font-mono">{{ attempt.upstream_status }}</dd>
                  </template>
                  <template v-if="attempt.request_payload_bytes !== undefined">
                    <dt>{{ t('admin.usage.requestAudit.requestPayloadBytes') }}</dt>
                    <dd class="font-mono">{{ formatBytes(attempt.request_payload_bytes) }}</dd>
                  </template>
                  <template v-if="attempt.response_payload_bytes !== undefined">
                    <dt>{{ t('admin.usage.requestAudit.responsePayloadBytes') }}</dt>
                    <dd class="font-mono">{{ formatBytes(attempt.response_payload_bytes) }}</dd>
                  </template>
                  <template v-if="attempt.response_read_complete !== undefined">
                    <dt>{{ t('admin.usage.requestAudit.responseReadComplete') }}</dt>
                    <dd>{{ attempt.response_read_complete ? t('admin.usage.requestAudit.yes') : t('admin.usage.requestAudit.no') }}</dd>
                  </template>
                </template>
              </dl>
            </div>
          </li>
        </ol>
        <p v-else class="text-gray-500 dark:text-dark-400">{{ t('admin.usage.requestAudit.empty') }}</p>
      </div>

      <div v-if="detail.headers && Object.keys(detail.headers).length" data-testid="request-audit-inbound-headers" class="rounded-lg border border-gray-200 px-3 py-2 dark:border-dark-700">
        <div class="mb-2 font-medium text-gray-500 dark:text-dark-400">{{ t('admin.usage.requestAudit.inboundHeaders') }}</div>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 font-mono text-xs">
          <template v-for="(value, key) in detail.headers" :key="`inbound-${key}`">
            <dt class="text-gray-500 dark:text-dark-400">{{ key }}</dt>
            <dd class="break-all text-gray-900 dark:text-dark-100">{{ formatHeaderValue(value) }}</dd>
          </template>
        </dl>
      </div>

      <div v-if="detail.request_fingerprint" data-testid="request-audit-fingerprint" class="rounded-lg border border-gray-200 px-3 py-2 font-mono text-xs dark:border-dark-700">
        {{ detail.request_fingerprint }} (v{{ detail.fingerprint_key_version || 0 }})
      </div>
      <div
        v-if="hasPositiveTokenCounts(detail.metadata?.tokens)"
        data-testid="request-audit-token-counts"
        class="rounded-lg border border-gray-200 px-3 py-2 text-xs dark:border-dark-700"
      >
        <div class="mb-2 font-medium text-gray-500 dark:text-dark-400">
          {{ t('admin.usage.requestAudit.tokenCounts.title') }}
        </div>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1">
          <template v-if="isPositiveTokenCount(detail.metadata?.tokens?.input_tokens)">
            <dt>{{ t('admin.usage.requestAudit.tokenCounts.input') }}</dt>
            <dd class="font-mono">{{ detail.metadata?.tokens?.input_tokens }}</dd>
          </template>
          <template v-if="isPositiveTokenCount(detail.metadata?.tokens?.output_tokens)">
            <dt>{{ t('admin.usage.requestAudit.tokenCounts.output') }}</dt>
            <dd class="font-mono">{{ detail.metadata?.tokens?.output_tokens }}</dd>
          </template>
        </dl>
      </div>
      <div
        v-if="detail.metadata?.protocol_fields"
        data-testid="request-audit-protocol-fields"
        class="rounded-lg border border-gray-200 px-3 py-2 text-xs dark:border-dark-700"
      >
        <div class="mb-2 font-medium text-gray-500 dark:text-dark-400">
          {{ t('admin.usage.requestAudit.protocolFields.title') }}
        </div>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1">
          <dt>{{ t('admin.usage.requestAudit.protocolFields.stream') }}</dt>
          <dd>{{ formatOptionalBoolean(detail.metadata.protocol_fields.stream) }}</dd>
          <dt>{{ t('admin.usage.requestAudit.protocolFields.thinkingType') }}</dt>
          <dd>{{ thinkingTypeLabel(detail.metadata.protocol_fields.thinking_type) }}</dd>
          <template v-if="safePresentFields(detail.metadata.protocol_fields.present_fields).length">
            <dt>{{ t('admin.usage.requestAudit.protocolFields.presentFields') }}</dt>
            <dd>{{ safePresentFields(detail.metadata.protocol_fields.present_fields).map(presentFieldLabel).join(', ') }}</dd>
          </template>
          <template v-if="hasNormalizedThinkingFields(detail.metadata.protocol_fields.normalized_fields)">
            <dt>{{ t('admin.usage.requestAudit.protocolFields.normalizedFields') }}</dt>
            <dd>{{ t('admin.usage.requestAudit.protocolFields.normalizedThinkingExtraFieldsRemoved') }}</dd>
          </template>
        </dl>
      </div>
      <div
        data-testid="request-audit-client-response"
        class="rounded-lg border border-gray-200 px-3 py-2 text-xs dark:border-dark-700"
      >
        <div class="mb-2 font-medium text-gray-500 dark:text-dark-400">
          {{ t('admin.usage.requestAudit.clientResponse.title') }}
        </div>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1">
          <dt>{{ t('admin.usage.requestAudit.clientResponse.status') }}</dt>
          <dd data-testid="request-audit-client-response-status" class="font-mono">
            {{ formatOptionalNumber(clientResponseStatus) }}
          </dd>
          <dt>{{ t('admin.usage.requestAudit.clientResponse.bytes') }}</dt>
          <dd data-testid="request-audit-client-response-bytes" class="font-mono">
            {{ formatOptionalBytes(clientResponseBytes) }}
          </dd>
        </dl>
        <p class="mt-2 text-gray-500 dark:text-dark-400">
          {{ t('admin.usage.requestAudit.clientResponse.observedNote') }}
        </p>
      </div>
      <div v-if="detail.metadata && (detail.metadata.routes || detail.metadata.ids)" class="space-y-1 rounded-lg border border-gray-200 px-3 py-2 text-xs dark:border-dark-700">
        <div v-for="(value, key) in detail.metadata.routes" :key="`route-${key}`">{{ key }}: {{ value }}</div>
        <div v-for="(value, key) in detail.metadata.ids" :key="`id-${key}`">{{ key }}: {{ value }}</div>
      </div>

      <div v-if="detail.events?.length">
        <div class="mb-2 font-medium text-gray-500 dark:text-dark-400">{{ t('admin.usage.requestAudit.events') }}</div>
        <ul class="space-y-2 font-mono text-xs text-gray-700 dark:text-dark-200">
          <li
            v-for="event in detail.events"
            :key="event.index"
            data-testid="request-audit-event"
            class="rounded-lg border border-gray-200 px-3 py-2 dark:border-dark-700"
          >
            <div>{{ event.type }} #{{ event.index }} {{ formatBytes(event.bytes) }}</div>
            <div v-if="event.fingerprint" class="mt-1 break-all">
              <span class="mr-1 text-gray-500 dark:text-dark-400">{{ t('admin.usage.requestAudit.eventFingerprint') }}</span>
              {{ event.fingerprint }}
            </div>
            <dl v-if="isTruncationEvent(event)" data-testid="request-audit-truncation" class="mt-2 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 border-t border-gray-200 pt-2 text-gray-700 dark:border-dark-700 dark:text-dark-200">
              <template v-if="event.original !== undefined">
                <dt>{{ t('admin.usage.requestAudit.truncation.originalEvents') }}</dt>
                <dd>{{ event.original }}</dd>
              </template>
              <template v-if="event.original_bytes !== undefined">
                <dt>{{ t('admin.usage.requestAudit.truncation.originalBytes') }}</dt>
                <dd>{{ formatBytes(event.original_bytes) }}</dd>
              </template>
              <template v-if="event.kept !== undefined">
                <dt>{{ t('admin.usage.requestAudit.truncation.keptEvents') }}</dt>
                <dd>{{ event.kept }}</dd>
              </template>
              <template v-if="event.kept_bytes !== undefined">
                <dt>{{ t('admin.usage.requestAudit.truncation.keptBytes') }}</dt>
                <dd>{{ formatBytes(event.kept_bytes) }}</dd>
              </template>
              <template v-if="event.dropped !== undefined">
                <dt>{{ t('admin.usage.requestAudit.truncation.droppedEvents') }}</dt>
                <dd>{{ event.dropped }}</dd>
              </template>
              <template v-if="event.dropped_bytes !== undefined">
                <dt>{{ t('admin.usage.requestAudit.truncation.droppedBytes') }}</dt>
                <dd>{{ formatBytes(event.dropped_bytes) }}</dd>
              </template>
              <template v-if="event.reason">
                <dt>{{ t('admin.usage.requestAudit.truncation.reason') }}</dt>
                <dd>{{ truncationReasonLabel(event.reason) }}</dd>
              </template>
            </dl>
          </li>
        </ul>
      </div>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { getRequestAudit, type RequestAudit } from '@/api/admin/usage'

const props = defineProps<{
  show: boolean
  usageLogId: number | null
}>()

const emit = defineEmits<{
  (e: 'update:show', v: boolean): void
}>()

const { t } = useI18n()

const loading = ref(false)
const loadError = ref(false)
const detail = ref<RequestAudit | null>(null)
let requestRevision = 0

watch(
  () => [props.show, props.usageLogId] as const,
  ([show, id]) => {
    requestRevision += 1
    if (!show) {
      loading.value = false
      detail.value = null
      loadError.value = false
      return
    }
    if (typeof id === 'number' && id > 0) {
      void fetchDetail(id, requestRevision)
    }
  },
  { immediate: true }
)

function formatHeaderValue(value: unknown): string {
  if (typeof value === 'string') return value
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  if (value == null) return ''
  try {
    return JSON.stringify(value)
  } catch {
    return String(value)
  }
}

function formatBytes(bytes: number): string {
  return `${bytes} B`
}

function formatOptionalNumber(value?: number): string {
  return value === undefined ? t('admin.usage.requestAudit.unknown') : String(value)
}

function formatOptionalBytes(value?: number): string {
  return value === undefined ? t('admin.usage.requestAudit.unknown') : formatBytes(value)
}

function formatOptionalBoolean(value?: boolean): string {
  if (value === undefined) return t('admin.usage.requestAudit.unknown')
  return value ? t('admin.usage.requestAudit.yes') : t('admin.usage.requestAudit.no')
}

const CLIENT_RESPONSE_KEY = 'client_response'

/**
 * Reads one observed fact from a closed metadata map. Only the literal `client_response` entry is
 * ever considered: other keys describe other stages, and a record written by an older or tampered
 * build can carry arbitrary keys that must never become page text.
 */
function readObservedClientResponseValue(map?: Record<string, number>): unknown {
  if (map == null || !Object.prototype.hasOwnProperty.call(map, CLIENT_RESPONSE_KEY)) {
    return undefined
  }
  return map[CLIENT_RESPONSE_KEY]
}

/**
 * The status the handler committed to the client. Absent means the response was never observed as
 * sent, so it stays unknown instead of rendering a fabricated code; only a plain integer inside the
 * HTTP range is a fact.
 */
function observedClientResponseStatus(map?: Record<string, number>): number | undefined {
  const value = readObservedClientResponseValue(map)
  if (typeof value !== 'number' || !Number.isInteger(value) || value < 100 || value > 599) {
    return undefined
  }
  return value
}

/**
 * Bytes the handler observed writing to the client, present only once a response was committed.
 * A reported 0 is an observed zero and must stay distinct from unknown. Counts that cannot be shown
 * exactly are treated as unknown: an inexact number is a misleading audit fact.
 */
function observedClientResponseBytes(map?: Record<string, number>): number | undefined {
  const value = readObservedClientResponseValue(map)
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) {
    return undefined
  }
  return value
}

const clientResponseStatus = computed(() => observedClientResponseStatus(detail.value?.metadata?.status))
const clientResponseBytes = computed(() => observedClientResponseBytes(detail.value?.metadata?.bytes))

function thinkingTypeLabel(value?: string): string {
  switch (value) {
    case 'disabled':
      return t('admin.usage.requestAudit.protocolFields.thinkingDisabled')
    case 'enabled':
      return t('admin.usage.requestAudit.protocolFields.thinkingEnabled')
    case 'adaptive':
      return t('admin.usage.requestAudit.protocolFields.thinkingAdaptive')
    default:
      return t('admin.usage.requestAudit.protocolFields.unknown')
  }
}

function safePresentFields(fields?: string[]): string[] {
  const allowed = new Set(['model', 'messages', 'input', 'tools', 'stream', 'thinking'])
  return [...new Set((fields ?? []).filter((field) => allowed.has(field)))]
}

function presentFieldLabel(field: string): string {
  const labels: Record<string, string> = {
    model: 'fieldModel',
    messages: 'fieldMessages',
    input: 'fieldInput',
    tools: 'fieldTools',
    stream: 'fieldStream',
    thinking: 'fieldThinking',
  }
  const label = labels[field]
  return t(`admin.usage.requestAudit.protocolFields.${label}`)
}

function hasNormalizedThinkingFields(fields?: string[]): boolean {
  return (fields ?? []).includes('thinking.extra_fields_removed')
}

function isPositiveTokenCount(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0
}

function hasPositiveTokenCounts(tokens?: Record<string, number>): boolean {
  return isPositiveTokenCount(tokens?.input_tokens) || isPositiveTokenCount(tokens?.output_tokens)
}

const MODEL_FINGERPRINT_DIGEST = /^[0-9a-f]{64}$/

/**
 * Only a 64-character lowercase hex digest is a model fingerprint. Anything else — including a
 * raw model alias left behind by an older or tampered record — must never reach the DOM.
 */
function isModelFingerprintDigest(value?: string): value is string {
  return typeof value === 'string' && MODEL_FINGERPRINT_DIGEST.test(value)
}

/** Digests are truncated for display; the full value stays on the element title for comparison. */
function shortModelFingerprint(value?: string): string {
  if (!isModelFingerprintDigest(value)) return ''
  return `${value.slice(0, 12)}…`
}

function hasAttemptTelemetry(attempt: NonNullable<RequestAudit['attempts']>[number]): boolean {
  return Boolean(
    (attempt.wire_request_headers && Object.keys(attempt.wire_request_headers).length) ||
    (attempt.upstream_response_headers && Object.keys(attempt.upstream_response_headers).length) ||
    attempt.upstream_status !== undefined ||
    attempt.request_payload_bytes !== undefined ||
    attempt.response_payload_bytes !== undefined ||
    attempt.response_read_complete !== undefined
  )
}

function captureCompletenessLabel(completeness?: string): string {
  const labels: Record<string, string> = {
    complete: 'complete',
    truncated: 'truncated',
    incomplete: 'incomplete',
    write_failed: 'writeFailed',
    not_captured: 'notCaptured',
  }
  const label = completeness ? labels[completeness] : undefined
  return t(`admin.usage.requestAudit.captureStatuses.${label ?? 'unknown'}`)
}

function isTruncationEvent(event: RequestAudit['events'][number]): boolean {
  return event.truncated === true || event.type === 'truncated'
}

function truncationReasonLabel(reason: string): string {
  const labels: Record<string, string> = {
    max_events: 'reasonMaxEvents',
    max_bytes: 'reasonMaxBytes',
  }
  const label = labels[reason] ?? 'reasonUnknown'
  return t(`admin.usage.requestAudit.truncation.${label}`)
}

function notCapturedReasonLabel(reason?: string) {
  if (reason === 'phase1_uncovered') {
    return t('admin.usage.requestAudit.notCapturedReasons.phase1Uncovered')
  }
  return t('admin.usage.requestAudit.notCapturedReasons.unknown')
}

async function fetchDetail(id: number, revision: number) {
  loading.value = true
  loadError.value = false
  detail.value = null
  try {
    const nextDetail = await getRequestAudit(id)
    if (revision === requestRevision) {
      detail.value = nextDetail
    }
  } catch {
    if (revision === requestRevision) {
      loadError.value = true
    }
  } finally {
    if (revision === requestRevision) {
      loading.value = false
    }
  }
}
</script>
