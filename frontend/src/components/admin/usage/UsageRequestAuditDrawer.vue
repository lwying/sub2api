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

      <!--
        Short-term value detail. The envelope (state, reason, deadline, counts) is
        metadata and loads with the audit; the values themselves are encrypted,
        readable for 7 days, and only ever fetched by the explicit button below.
        Everything revealed here is untrusted text rendered through interpolation.
      -->
      <section
        data-testid="request-audit-value-detail"
        class="rounded-lg border border-gray-200 px-3 py-2 text-xs dark:border-dark-700"
      >
        <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
          <div class="font-medium text-gray-500 dark:text-dark-400">
            {{ t('admin.usage.requestAudit.valueDetail.title') }}
          </div>
          <div
            v-if="valueDetail"
            data-testid="request-audit-value-detail-state"
            class="font-mono text-gray-900 dark:text-dark-100"
          >
            {{ valueDetailStateLabel }}
          </div>
        </div>

        <p
          v-if="valueDetailAbsent"
          data-testid="request-audit-value-detail-absent"
          class="text-gray-500 dark:text-dark-400"
        >
          {{ t('admin.usage.requestAudit.valueDetail.absent') }}
        </p>

        <!--
          The envelope could not be read. A failure is not a collection result, so it
          is reported as unreadable rather than as "nothing was collected", and no
          reveal is offered from a state that was never read.
        -->
        <p
          v-else-if="valueDetailUnavailable"
          data-testid="request-audit-value-detail-unavailable"
          class="text-amber-700 dark:text-amber-300"
        >
          {{ t('admin.usage.requestAudit.valueDetail.unavailable') }}
        </p>

        <template v-else-if="valueDetail">
          <p
            v-if="showValueDetailReason"
            data-testid="request-audit-value-detail-reason"
            class="text-gray-600 dark:text-dark-300"
          >
            {{ valueDetailReasonLabel }}
          </p>

          <dl
            v-if="valueDetail.expires_at"
            data-testid="request-audit-value-detail-expires-at"
            class="mt-1 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1"
          >
            <dt class="text-gray-500 dark:text-dark-400">
              {{ t('admin.usage.requestAudit.valueDetail.expiresAt') }}
            </dt>
            <dd class="font-mono text-gray-900 dark:text-dark-100">
              {{ formatTimestamp(valueDetail.expires_at) }}
            </dd>
          </dl>

          <p
            v-if="!valueDetail.capability_enabled"
            data-testid="request-audit-value-detail-disabled"
            class="mt-2 rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-amber-800 dark:border-amber-800/60 dark:bg-amber-950/30 dark:text-amber-200"
          >
            {{ t('admin.usage.requestAudit.valueDetail.disabled') }}
          </p>

          <p class="mt-2 text-gray-500 dark:text-dark-400">
            {{ t('admin.usage.requestAudit.valueDetail.notice') }}
          </p>

          <div v-if="canRevealValueDetail" class="mt-3">
            <button
              type="button"
              data-testid="request-audit-value-detail-reveal"
              class="btn btn-secondary btn-sm"
              :disabled="revealingValueDetail"
              @click="revealValueDetail"
            >
              {{ revealingValueDetail ? t('admin.usage.requestAudit.valueDetail.revealing') : t('admin.usage.requestAudit.valueDetail.reveal') }}
            </button>
          </div>

          <!--
            A step-up refusal that entering a code cannot resolve (no TOTP enrolled,
            or an admin API key session) is not a reveal failure: it is reported with
            the shared step-up wording and no retry is offered.
          -->
          <p
            v-if="valueDetailRevealBlockedLabel"
            data-testid="request-audit-value-detail-reveal-blocked"
            class="mt-3 text-amber-700 dark:text-amber-300"
          >
            {{ valueDetailRevealBlockedLabel }}
          </p>

          <p
            v-else-if="valueDetailRevealFailed"
            data-testid="request-audit-value-detail-reveal-failed"
            class="mt-3 text-red-500"
          >
            {{ t('admin.usage.requestAudit.valueDetail.revealFailed') }}
          </p>

          <div
            v-if="revealedValueDetail"
            data-testid="request-audit-value-detail-values"
            class="mt-3 space-y-3 border-t border-gray-200 pt-3 dark:border-dark-700"
          >
            <p
              v-if="revealedValueDetail.values.truncated"
              data-testid="request-audit-value-detail-truncated"
              class="text-amber-700 dark:text-amber-300"
            >
              {{ t('admin.usage.requestAudit.valueDetail.truncated') }}
            </p>

            <!--
              The read-side counterpart of `truncated`: the server did not drop an
              entry, but this view refused one of the entries the payload carried.
              The two are the same warning for the admin — "this list may not be
              everything" — so at most one of them is rendered, and the server's own
              statement wins when it is set.
            -->
            <p
              v-else-if="revealedValueDetail.values.validation_dropped"
              data-testid="request-audit-value-detail-validation-dropped"
              class="text-amber-700 dark:text-amber-300"
            >
              {{ t('admin.usage.requestAudit.valueDetail.validationDropped') }}
            </p>

            <dl
              v-if="revealedValueDetail.values.model"
              data-testid="request-audit-value-detail-model"
              class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1"
            >
              <dt class="text-gray-500 dark:text-dark-400">
                {{ t('admin.usage.requestAudit.valueDetail.model') }}
              </dt>
              <dd class="break-all font-mono text-gray-900 dark:text-dark-100">
                {{ revealedValueDetail.values.model }}
              </dd>
            </dl>

            <div data-testid="request-audit-value-detail-inbound">
              <div class="mb-1 font-medium text-gray-500 dark:text-dark-400">
                {{ t('admin.usage.requestAudit.valueDetail.inbound') }}
              </div>
              <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1">
                <template
                  v-for="field in valueDetailIdentifiers(revealedValueDetail.values.inbound)"
                  :key="`inbound-id-${field.key}`"
                >
                  <dt class="text-gray-500 dark:text-dark-400">{{ t(`admin.usage.requestAudit.valueDetail.${field.key}`) }}</dt>
                  <dd class="break-all font-mono text-gray-900 dark:text-dark-100">{{ field.value }}</dd>
                </template>
              </dl>
              <dl
                v-if="Object.keys(revealedValueDetail.values.inbound.request_headers).length"
                class="mt-1 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1"
              >
                <template
                  v-for="(values, name) in revealedValueDetail.values.inbound.request_headers"
                  :key="`inbound-header-${name}`"
                >
                  <dt class="text-gray-500 dark:text-dark-400">{{ name }}</dt>
                  <dd class="break-all font-mono text-gray-900 dark:text-dark-100">{{ formatHeaderValueList(values) }}</dd>
                </template>
              </dl>
              <p
                v-if="!valueDetailIdentifiers(revealedValueDetail.values.inbound).length && !Object.keys(revealedValueDetail.values.inbound.request_headers).length"
                class="text-gray-500 dark:text-dark-400"
              >
                {{ t('admin.usage.requestAudit.valueDetail.unknown') }}
              </p>
            </div>

            <div
              v-for="attempt in revealedValueDetail.values.attempts"
              :key="`attempt-${attempt.index}`"
              data-testid="request-audit-value-detail-attempt"
              class="rounded-lg border border-gray-200 px-3 py-2 dark:border-dark-700"
            >
              <div class="flex flex-wrap gap-x-4 gap-y-1 font-mono text-gray-900 dark:text-dark-100">
                <span>#{{ attempt.index }}</span>
                <span v-if="attempt.account_id != null">{{ attempt.account_id }}</span>
                <span v-if="attempt.upstream_status != null">{{ attempt.upstream_status }}</span>
                <span v-if="attempt.model" class="break-all">{{ attempt.model }}</span>
              </div>
              <dl
                v-if="valueDetailAttemptFacts(attempt).length"
                class="mt-1 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1"
              >
                <template
                  v-for="field in valueDetailAttemptFacts(attempt)"
                  :key="`attempt-${attempt.index}-${field.key}`"
                >
                  <dt class="text-gray-500 dark:text-dark-400">{{ t(`admin.usage.requestAudit.valueDetail.${field.key}`) }}</dt>
                  <dd class="break-all font-mono text-gray-900 dark:text-dark-100">{{ field.value }}</dd>
                </template>
              </dl>
              <div
                v-for="direction in valueDetailHeaderDirections(attempt)"
                :key="`attempt-${attempt.index}-${direction.key}`"
                class="mt-2"
              >
                <div class="mb-1 font-medium text-gray-500 dark:text-dark-400">
                  {{ t(`admin.usage.requestAudit.valueDetail.${direction.key}`) }}
                </div>
                <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1">
                  <template v-for="(values, name) in direction.headers" :key="`${direction.key}-${name}`">
                    <dt class="text-gray-500 dark:text-dark-400">{{ name }}</dt>
                    <dd class="break-all font-mono text-gray-900 dark:text-dark-100">{{ formatHeaderValueList(values) }}</dd>
                  </template>
                </dl>
              </div>
            </div>
          </div>
        </template>
      </section>

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

  <!--
    The value-detail reveal is step-up gated server-side. On STEP_UP_REQUIRED the
    admin is asked for a TOTP code and the same explicit POST is retried once; the
    controller is also dismissed when the drawer closes or moves to another row.
  -->
  <TotpStepUpDialog :controller="stepUp" />
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import {
  canRevealRequestAuditValueDetail,
  getRequestAudit,
  getRequestAuditValueDetail,
  isRequestAuditValueDetailExpired,
  isRequestAuditValueDetailNotFound,
  revealRequestAuditValueDetail,
  type RequestAudit,
  type RequestAuditValueDetailAttemptValues,
  type RequestAuditValueDetailEnvelope,
  type RequestAuditValueDetailHeaders,
  type RequestAuditValueDetailInbound,
  type RequestAuditValueDetailReason,
  type RequestAuditValueDetailReveal,
  type RequestAuditValueDetailState,
} from '@/api/admin/usage'
import {
  isStepUpBlocked,
  isStepUpCancelled,
  stepUpBlockReason,
  useStepUp,
} from '@/composables/useStepUp'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'

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

/**
 * Value detail. `valueDetail` is the envelope (metadata only, no values),
 * `valueDetailAbsent` records that this usage log has no value-detail row at all,
 * `valueDetailUnavailable` records that the envelope could not be read, and
 * `revealedValueDetail` holds decrypted values that exist only while this drawer
 * stays open on the same usage log.
 */
const valueDetail = ref<RequestAuditValueDetailEnvelope | null>(null)
const valueDetailAbsent = ref(false)
const valueDetailUnavailable = ref(false)
const revealedValueDetail = ref<RequestAuditValueDetailReveal | null>(null)
const revealingValueDetail = ref(false)
const valueDetailRevealFailed = ref(false)
/**
 * Step-up refusal code that a TOTP code cannot resolve (`STEP_UP_TOTP_NOT_ENABLED`
 * or `STEP_UP_ADMIN_API_KEY_FORBIDDEN`). Empty while there is no such refusal, and
 * never set for the plain failures above: the two are different outcomes.
 */
const valueDetailRevealBlockedReason = ref('')
/** The revision that produced `revealedValueDetail`; never rendered. */
let revealedValueDetailRevision: number | null = null
/**
 * The clock the value-detail window is read against.
 *
 * `Date.now()` alone would freeze a computed: an open drawer would keep claiming
 * `stored` and keep offering a read after the deadline passed, because nothing
 * reactive would have changed. This ref is refreshed when the envelope arrives and
 * when the scheduled deadline fires, so the state, the offer and the plaintext all
 * follow the window exactly.
 */
const nowMs = ref(Date.now())
/** Pending "the window closed" check; cleared on close, on row change and on unmount. */
let valueDetailExpiryTimer: ReturnType<typeof setTimeout> | null = null

/** Wraps the step-up gated reveal: prompt on refusal, retry once, then discard. */
const stepUp = useStepUp()

/** Discards a stale response when the admin switches rows or closes the drawer. */
let requestRevision = 0

watch(
  () => [props.show, props.usageLogId] as const,
  ([show, id]) => {
    requestRevision += 1
    clearValueDetailState()
    // A prompt belongs to the row that was on screen when it was opened. Once that
    // row (or the whole drawer) goes away, so does the prompt, and the abandoned
    // retry is discarded rather than applied to whatever is opened next.
    dismissStepUpPrompt()
    if (!show) {
      loading.value = false
      detail.value = null
      loadError.value = false
      return
    }
    if (typeof id === 'number' && id > 0) {
      nowMs.value = Date.now()
      void fetchDetail(id, requestRevision)
      void fetchValueDetail(id, requestRevision)
    }
  },
  { immediate: true }
)

onBeforeUnmount(cancelValueDetailExpiryCheck)

/** Closes an open TOTP prompt, which resolves the pending reveal as cancelled. */
function dismissStepUpPrompt() {
  if (stepUp.visible.value) {
    stepUp.onCancel()
  }
}

/**
 * Clears everything derived from a decrypted value payload. Called on every
 * open/close and before any new fetch, so a revealed payload never survives into
 * the next usage log and is gone from memory once the drawer closes.
 */
function clearValueDetailState() {
  cancelValueDetailExpiryCheck()
  valueDetail.value = null
  valueDetailAbsent.value = false
  valueDetailUnavailable.value = false
  revealedValueDetail.value = null
  revealedValueDetailRevision = null
  revealingValueDetail.value = false
  valueDetailRevealFailed.value = false
  valueDetailRevealBlockedReason.value = ''
}

/** Cancels the scheduled window check; safe to call whether or not one is pending. */
function cancelValueDetailExpiryCheck() {
  if (valueDetailExpiryTimer !== null) {
    clearTimeout(valueDetailExpiryTimer)
    valueDetailExpiryTimer = null
  }
}

/**
 * A deadline further out than one bounded wait is re-armed instead of trusted to a
 * single very long timer, so a suspended or resumed clock still lands on the truth.
 */
const MAX_VALUE_DETAIL_EXPIRY_WAIT_MS = 24 * 60 * 60 * 1000

/**
 * Schedules the moment this row's 7-day window closes.
 *
 * The deadline belongs to the envelope, so it is scheduled when the envelope
 * arrives and guarded by the revision that fetched it: a timer left over from a row
 * the admin has already left says nothing about the row on screen now.
 */
function scheduleValueDetailExpiryCheck(revision: number) {
  cancelValueDetailExpiryCheck()
  const expiresAt = valueDetail.value?.expires_at
  if (typeof expiresAt !== 'string') return
  const deadline = Date.parse(expiresAt)
  if (Number.isNaN(deadline)) return

  const remaining = deadline - Date.now()
  if (remaining > MAX_VALUE_DETAIL_EXPIRY_WAIT_MS) {
    valueDetailExpiryTimer = setTimeout(() => {
      valueDetailExpiryTimer = null
      if (revision === requestRevision) scheduleValueDetailExpiryCheck(revision)
    }, MAX_VALUE_DETAIL_EXPIRY_WAIT_MS)
    return
  }
  valueDetailExpiryTimer = setTimeout(() => {
    valueDetailExpiryTimer = null
    if (revision !== requestRevision) return
    nowMs.value = Date.now()
    // The plaintext is no longer readable, so it does not stay on screen: holding
    // decrypted values past the window the server enforces is exactly what this
    // view must not do. The revision guard above is the same one the reveal uses.
    revealedValueDetail.value = null
    revealedValueDetailRevision = null
  }, Math.max(remaining, 0) + 1)
}

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

const VALUE_DETAIL_STATE_LABELS: Record<RequestAuditValueDetailState, string> = {
  not_observed: 'notObserved',
  stored: 'stored',
  skipped: 'skipped',
  expired: 'expired',
  purged: 'purged',
}

const VALUE_DETAIL_REASON_LABELS: Record<RequestAuditValueDetailReason, string> = {
  not_observed: 'notObserved',
  retained: 'retained',
  skipped_out_of_scope: 'skippedOutOfScope',
  skipped_value_retention_disabled: 'skippedRetentionDisabled',
  skipped_encryption_unavailable: 'skippedEncryptionUnavailable',
  skipped_invalid_values: 'skippedInvalidValues',
  skipped_too_many_attempts: 'skippedTooManyAttempts',
}

/**
 * `stored` is only a server statement about the ciphertext; once the 7-day window
 * has passed the client must present it as expired rather than offering a read the
 * server will refuse.
 */
const valueDetailStateLabel = computed(() => {
  const envelope = valueDetail.value
  if (!envelope) return ''
  if (envelope.state === 'stored' && isRequestAuditValueDetailExpired(envelope.expires_at, nowMs.value)) {
    return t('admin.usage.requestAudit.valueDetail.states.expired')
  }
  return t(`admin.usage.requestAudit.valueDetail.states.${VALUE_DETAIL_STATE_LABELS[envelope.state]}`)
})

/**
 * `retained` is the reason of values that WERE retained, so it explains nothing
 * the state does not already say and is not shown.
 */
const showValueDetailReason = computed(
  () => valueDetail.value?.reason !== undefined && valueDetail.value.reason !== 'retained',
)

const valueDetailReasonLabel = computed(() => {
  const reason = valueDetail.value?.reason
  return t(
    `admin.usage.requestAudit.valueDetail.reasons.${
      reason ? VALUE_DETAIL_REASON_LABELS[reason] : 'unknown'
    }`,
  )
})

const canRevealValueDetail = computed(() =>
  canRevealRequestAuditValueDetail(valueDetail.value?.state, valueDetail.value?.expires_at, nowMs.value),
)

/**
 * A step-up refusal that entering a code cannot fix is explained with the shared
 * step-up wording, because the reveal itself never started. Everything else stays
 * on the reveal-failure path.
 */
const valueDetailRevealBlockedLabel = computed(() => {
  if (!valueDetailRevealBlockedReason.value) return ''
  return valueDetailRevealBlockedReason.value === 'STEP_UP_ADMIN_API_KEY_FORBIDDEN'
    ? t('stepUp.adminApiKeyForbidden')
    : t('stepUp.notEnabled')
})

function formatHeaderValueList(values: string[]): string {
  return values.join(', ')
}

function formatTimestamp(value: string): string {
  const parsed = Date.parse(value)
  return Number.isNaN(parsed) ? value : new Date(parsed).toLocaleString()
}

/** One rendered fact of a revealed value payload; the key is its i18n label. */
type ValueDetailFieldKey = 'deviceId' | 'accountUuid' | 'sessionId' | 'latencyMs' | 'proxyId'

/**
 * The parsed identifiers that are actually present, in a fixed order. Both the
 * inbound block and every attempt carry the same three components; one that was
 * never parsed stays absent instead of being rendered as an empty fact.
 */
function valueDetailIdentifiers(
  source: RequestAuditValueDetailInbound | RequestAuditValueDetailAttemptValues,
): Array<{ key: ValueDetailFieldKey; value: string }> {
  const fields: Array<{ key: ValueDetailFieldKey; value: string }> = []
  if (source.device_id) fields.push({ key: 'deviceId', value: source.device_id })
  if (source.account_uuid) fields.push({ key: 'accountUuid', value: source.account_uuid })
  if (source.session_id) fields.push({ key: 'sessionId', value: source.session_id })
  return fields
}

/**
 * Everything one attempt states about itself: the parsed identifiers plus the two
 * attempt-level scalars, in a fixed order.
 *
 * The scalars are the same facts the backend keeps for the attempt itself — the
 * measured latency and the proxy it went through. Both are optional and stay absent
 * when the reveal carried none, so "this attempt was not measured / went through no
 * proxy" never becomes a fabricated `0`. The decoder has already bounded both; this
 * only formats a fact that survived that boundary.
 */
function valueDetailAttemptFacts(
  attempt: RequestAuditValueDetailAttemptValues,
): Array<{ key: ValueDetailFieldKey; value: string }> {
  const fields = valueDetailIdentifiers(attempt)
  if (attempt.latency_ms !== undefined) {
    fields.push({ key: 'latencyMs', value: `${attempt.latency_ms} ms` })
  }
  if (attempt.proxy_id !== undefined) {
    fields.push({ key: 'proxyId', value: String(attempt.proxy_id) })
  }
  return fields
}

/** Only a direction that actually carries values gets a heading. */
function valueDetailHeaderDirections(
  attempt: RequestAuditValueDetailAttemptValues,
): Array<{ key: 'requestHeaders' | 'responseHeaders'; headers: RequestAuditValueDetailHeaders }> {
  const directions: Array<{
    key: 'requestHeaders' | 'responseHeaders'
    headers: RequestAuditValueDetailHeaders
  }> = []
  if (Object.keys(attempt.request_headers).length) {
    directions.push({ key: 'requestHeaders', headers: attempt.request_headers })
  }
  if (Object.keys(attempt.response_headers).length) {
    directions.push({ key: 'responseHeaders', headers: attempt.response_headers })
  }
  return directions
}

/**
 * True only while the drawer still shows the usage log a pending reveal was
 * started for. A response arriving after the drawer closed, or after it moved to
 * another usage log, belongs to a row nobody is looking at any more.
 */
function isValueDetailRevealCurrent(revision: number, id: number): boolean {
  return revision === requestRevision && props.show && props.usageLogId === id
}

/** Discards the decrypted values a superseded reveal returned. */
function discardRevealedValueDetail(revision: number) {
  if (revealedValueDetailRevision === revision) {
    revealedValueDetail.value = null
    revealedValueDetailRevision = null
  }
}

async function fetchValueDetail(id: number, revision: number) {
  try {
    const next = await getRequestAuditValueDetail(id)
    if (revision === requestRevision) {
      valueDetail.value = next
      valueDetailAbsent.value = false
      valueDetailUnavailable.value = false
      // The window this envelope states is now the one on screen, so the clock and
      // the scheduled check are refreshed together with it.
      nowMs.value = Date.now()
      scheduleValueDetailExpiryCheck(revision)
    }
  } catch (error) {
    if (revision !== requestRevision) return
    valueDetail.value = null
    // No row for this usage log is a normal outcome: the value detail only
    // applies to Claude /v1/messages requests, and "no row" is how the backend
    // expresses 未采集 (not collected). It is reported as an absence, never as a
    // state the server did not send.
    //
    // Only the backend's own 404 says that. A 5xx, a conflict or a request that
    // never reached the server says the envelope could not be read — reporting
    // that as "nothing was collected" would state a collection result nobody
    // sent, so it stays an unreadable envelope.
    valueDetailAbsent.value = isRequestAuditValueDetailNotFound(error)
    valueDetailUnavailable.value = !valueDetailAbsent.value
  }
}

/**
 * Explicit admin action only. Nothing here auto-runs, and no value is fetched,
 * prefetched or cached: the decrypted payload exists in this component's memory
 * until the drawer closes, and a server error message is never echoed.
 *
 * The route is step-up gated, so the click may be answered with STEP_UP_REQUIRED:
 * the controller then asks for a code and retries this same explicit POST once.
 * That retry is still a consequence of the click, never of a timer or a prefetch.
 */
async function revealValueDetail() {
  const id = props.usageLogId
  if (typeof id !== 'number' || id <= 0 || revealingValueDetail.value) return

  const currentRevision = requestRevision
  revealingValueDetail.value = true
  valueDetailRevealFailed.value = false
  valueDetailRevealBlockedReason.value = ''
  try {
    const revealed = await stepUp.run(() => revealRequestAuditValueDetail(id))
    if (!isValueDetailRevealCurrent(currentRevision, id)) {
      discardRevealedValueDetail(currentRevision)
      return
    }
    if (isRequestAuditValueDetailExpired(valueDetail.value?.expires_at, Date.now())) {
      // The window closed while this reveal was in flight: what came back describes
      // values that are no longer readable, so it is dropped instead of rendered.
      nowMs.value = Date.now()
      discardRevealedValueDetail(currentRevision)
      return
    }
    revealedValueDetail.value = revealed
    revealedValueDetailRevision = currentRevision
  } catch (error) {
    if (!isValueDetailRevealCurrent(currentRevision, id)) {
      // A refusal for an abandoned usage log says nothing about the one on screen.
      discardRevealedValueDetail(currentRevision)
      valueDetailRevealFailed.value = false
      valueDetailRevealBlockedReason.value = ''
      return
    }
    if (isStepUpCancelled(error)) {
      // The admin dismissed the prompt: nothing was read, so this is neither a
      // failure nor a collection outcome.
      return
    }
    if (isStepUpBlocked(error)) {
      // Step-up cannot be satisfied by entering a code here, so say why instead of
      // reporting a reveal that never happened.
      valueDetailRevealBlockedReason.value = stepUpBlockReason(error)
      return
    }
    // The refusal is deliberately not used to infer why the values are gone;
    // re-read the envelope, which is the authoritative state.
    valueDetailRevealFailed.value = true
    await fetchValueDetail(id, currentRevision)
    if (!isValueDetailRevealCurrent(currentRevision, id)) {
      valueDetailRevealFailed.value = false
      return
    }
    if (valueDetail.value?.state === 'stored' && canRevealValueDetail.value) {
      // Still readable: the failure is a real error, keep it visible.
      valueDetailRevealFailed.value = true
    } else {
      valueDetailRevealFailed.value = false
    }
  } finally {
    if (currentRevision === requestRevision) {
      revealingValueDetail.value = false
    }
  }
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
