<template>
  <BaseDialog
    :show="show"
    :title="t('admin.errorDiagnostics.detail.title')"
    width="wide"
    @close="emit('update:show', false)"
  >
    <div v-if="loading" class="flex justify-center py-10" data-testid="error-diagnostic-loading">
      <svg class="h-7 w-7 animate-spin text-primary-500" fill="none" viewBox="0 0 24 24">
        <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
        <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z" />
      </svg>
    </div>

    <div v-else-if="loadError" class="py-8 text-center text-sm text-red-500" data-testid="error-diagnostic-load-failed">
      {{ t('admin.errorDiagnostics.detail.loadFailed') }}
    </div>

    <div v-else-if="detail" class="space-y-5 text-sm">
      <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1" data-testid="error-diagnostic-metadata">
        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.detail.createdAt') }}</dt>
        <dd class="font-mono text-gray-900 dark:text-dark-100">{{ formatDateTime(detail.created_at) }}</dd>

        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.detail.protocol') }}</dt>
        <dd class="text-gray-900 dark:text-dark-100">{{ protocolLabel(t, detail.protocol) }}</dd>

        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.detail.attempt') }}</dt>
        <dd class="font-mono text-gray-900 dark:text-dark-100">{{ detail.attempt_index }}</dd>

        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.detail.upstreamStatus') }}</dt>
        <dd class="font-mono text-gray-900 dark:text-dark-100">{{ detail.upstream_status }}</dd>

        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.detail.metadataExpiresAt') }}</dt>
        <dd class="font-mono text-gray-900 dark:text-dark-100">{{ formatDateTime(detail.metadata_expires_at) }}</dd>

        <template v-if="detail.body_expires_at">
          <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.detail.bodyExpiresAt') }}</dt>
          <dd class="font-mono text-gray-900 dark:text-dark-100">{{ formatDateTime(detail.body_expires_at) }}</dd>
        </template>
      </dl>

      <div class="rounded-lg border border-gray-200 px-3 py-2 dark:border-dark-700">
        <!-- Plain text: there is no deep link to a single usage log, so no link is offered. -->
        <div v-if="detail.usage_log_id" class="text-xs text-gray-900 dark:text-dark-100" data-testid="error-diagnostic-usage-id">
          <span class="text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.detail.usageLink') }}</span>
          <span class="ml-1 font-mono">#{{ detail.usage_log_id }}</span>
        </div>
        <div v-else class="text-xs text-gray-500 dark:text-dark-400" data-testid="error-diagnostic-usage-absent">
          {{ t('admin.errorDiagnostics.detail.usageAbsent') }}
        </div>
      </div>

      <section class="rounded-lg border border-gray-200 px-3 py-3 dark:border-dark-700">
        <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
          <div class="font-medium text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.detail.body') }}</div>
          <div class="text-xs">
            <span data-testid="error-diagnostic-body-state" class="font-mono text-gray-900 dark:text-dark-100">
              {{ effectiveBodyStateLabel }}
            </span>
          </div>
        </div>

        <!--
          The reason code is shown for every non-readable outcome. It is the
          stable, closed-set explanation the spec requires, and it never carries
          model text or a credential.
        -->
        <p v-if="showReason" data-testid="error-diagnostic-body-reason" class="text-xs text-gray-600 dark:text-dark-300">
          {{ bodyReasonLabel(t, detail.reason) }}
        </p>

        <p class="mt-2 text-xs text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.detail.notice') }}</p>

        <div v-if="canReveal" class="mt-3">
          <button
            type="button"
            data-testid="error-diagnostic-reveal"
            class="btn btn-secondary btn-sm"
            :disabled="revealingBody"
            @click="revealBody"
          >
            {{ revealingBody ? t('admin.errorDiagnostics.detail.revealing') : t('admin.errorDiagnostics.detail.reveal') }}
          </button>
        </div>

        <p v-if="bodyRevealFailed" data-testid="error-diagnostic-reveal-failed" class="mt-3 text-xs text-red-500">
          {{ t('admin.errorDiagnostics.detail.revealFailed') }}
        </p>

        <!--
          The revealed payload is untrusted text. It is rendered through
          interpolation only, so its content can never become markup, and it is
          dropped as soon as the drawer closes.
        -->
        <div v-if="revealedBody" class="mt-3" data-testid="error-diagnostic-body">
          <pre class="max-h-96 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-gray-50 p-3 font-mono text-xs text-gray-900 dark:text-dark-100">{{ revealedBody.body_text }}</pre>
          <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
            {{ t('admin.errorDiagnostics.detail.bodyBytes', { bytes: revealedBody.body_bytes }) }}
          </p>
        </div>
      </section>

      <!--
        429 header values (Claude Messages only). A separate retention fact with
        its own window and its own reveal action: it can be readable when no body
        was retained, so it gets its own section rather than a body sub-section.
      -->
      <section
        v-if="showHeaderValues"
        data-testid="error-diagnostic-headers"
        class="rounded-lg border border-gray-200 px-3 py-3 dark:border-dark-700"
      >
        <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
          <div class="font-medium text-gray-500 dark:text-dark-400">
            {{ t('admin.errorDiagnostics.detail.headers') }}
          </div>
          <div class="text-xs">
            <span data-testid="error-diagnostic-header-state" class="font-mono text-gray-900 dark:text-dark-100">
              {{ effectiveHeaderStateLabel }}
            </span>
          </div>
        </div>

        <dl
          v-if="detail.header_expires_at"
          data-testid="error-diagnostic-header-expires"
          class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-xs"
        >
          <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.detail.headerExpiresAt') }}</dt>
          <dd class="font-mono text-gray-900 dark:text-dark-100">{{ formatDateTime(detail.header_expires_at) }}</dd>
        </dl>

        <p v-if="showHeaderReason" data-testid="error-diagnostic-header-reason" class="text-xs text-gray-600 dark:text-dark-300">
          {{ headerReasonLabel(t, detail.header_reason) }}
        </p>

        <p class="mt-2 text-xs text-gray-500 dark:text-dark-400">
          {{ t('admin.errorDiagnostics.detail.headerNotice') }}
        </p>

        <div v-if="canRevealHeaderValues" class="mt-3">
          <button
            type="button"
            data-testid="error-diagnostic-header-reveal"
            class="btn btn-secondary btn-sm"
            :disabled="revealingHeaders"
            @click="revealHeaders"
          >
            {{ revealingHeaders ? t('admin.errorDiagnostics.detail.revealingHeaders') : t('admin.errorDiagnostics.detail.revealHeaders') }}
          </button>
        </div>

        <!--
          A step-up refusal that entering a code cannot resolve (no TOTP enrolled,
          or an admin API key session) is not a reveal failure: it is reported with
          the shared step-up wording, and no values were read.
        -->
        <p
          v-if="headersRevealBlockedLabel"
          data-testid="error-diagnostic-header-reveal-blocked"
          class="mt-3 text-xs text-amber-700 dark:text-amber-300"
        >
          {{ headersRevealBlockedLabel }}
        </p>

        <p v-else-if="headersRevealFailed" data-testid="error-diagnostic-header-reveal-failed" class="mt-3 text-xs text-red-500">
          {{ t('admin.errorDiagnostics.detail.headerRevealFailed') }}
        </p>

        <!--
          Untrusted text, rendered through interpolation only, and dropped as soon
          as the drawer closes or moves to another attempt.
        -->
        <div v-if="revealedHeaders" class="mt-3 space-y-3" data-testid="error-diagnostic-header-values">
          <p class="text-xs text-gray-500 dark:text-dark-400">
            {{ t('admin.errorDiagnostics.detail.headerEntryCount', { count: revealedHeaders.header_entry_count }) }}
          </p>
          <div v-for="direction in headerDirections(revealedHeaders)" :key="direction.key">
            <div class="mb-1 text-xs font-medium text-gray-500 dark:text-dark-400">
              {{ t(`admin.errorDiagnostics.detail.${direction.key}`) }}
            </div>
            <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 font-mono text-xs">
              <template v-for="(value, name) in direction.headers" :key="`${direction.key}-${name}`">
                <dt class="text-gray-500 dark:text-dark-400">{{ name }}</dt>
                <dd class="break-all text-gray-900 dark:text-dark-100">{{ value }}</dd>
              </template>
            </dl>
          </div>
        </div>
      </section>
    </div>
  </BaseDialog>

  <!--
    The 429 header reveal is step-up gated server-side (the body replay is not).
    On STEP_UP_REQUIRED the admin is asked for a TOTP code and the same explicit
    POST is retried once; the prompt is dismissed when the drawer leaves the attempt.
  -->
  <TotpStepUpDialog :controller="stepUp" />
</template>

<script setup lang="ts">
import { computed, ref, watch, type Ref } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import {
  isStepUpBlocked,
  isStepUpCancelled,
  stepUpBlockReason,
  useStepUp,
} from '@/composables/useStepUp'
import { getDiagnostic, revealDiagnosticBody, revealDiagnosticHeaders } from '../api'
import {
  bodyReasonLabel,
  bodyStateLabel,
  canRevealBody,
  canRevealHeaders,
  formatDateTime,
  hasHeaderValuesSection,
  headerDirections,
  headerReasonLabel,
  headerStateLabel,
  isHeaderValuesStoredButExpired,
  isStoredButExpired,
  protocolLabel,
} from '../labels'
import type { DiagnosticAttempt, DiagnosticBodyReveal, DiagnosticHeaderReveal } from '../types'

const props = defineProps<{
  show: boolean
  diagnosticId: string | null
}>()

const emit = defineEmits<{
  (e: 'update:show', value: boolean): void
}>()

const { t } = useI18n()

const loading = ref(false)
const loadError = ref(false)
const detail = ref<DiagnosticAttempt | null>(null)

/** The reveal that produced a payload: its attempt and the revision it was issued in. */
interface RevealOwner {
  id: string
  revision: number
}

const revealedBody = ref<DiagnosticBodyReveal | null>(null)
const revealedBodyOwner = ref<RevealOwner | null>(null)
const revealingBody = ref(false)
const bodyRevealFailed = ref(false)

/**
 * 429 header values are a separate secret with a separate reveal: keeping them in
 * their own state means a header payload is never cleared by a body reveal (or
 * the reverse), and each can only be discarded by the request that produced it.
 */
const revealedHeaders = ref<DiagnosticHeaderReveal | null>(null)
const revealedHeadersOwner = ref<RevealOwner | null>(null)
const revealingHeaders = ref(false)
const headersRevealFailed = ref(false)
/**
 * Step-up refusal code that a TOTP code cannot resolve (`STEP_UP_TOTP_NOT_ENABLED`
 * or `STEP_UP_ADMIN_API_KEY_FORBIDDEN`). Empty while there is no such refusal, and
 * never set for the plain failures above: the two are different outcomes.
 */
const headersRevealBlockedReason = ref('')

/** Wraps the step-up gated header reveal: prompt on refusal, retry once, then discard. */
const stepUp = useStepUp()

/** Discards a stale response when the admin switches rows or closes the drawer. */
let revision = 0

watch(
  () => [props.show, props.diagnosticId] as const,
  ([show, id]) => {
    revision += 1
    clearSensitiveState()
    // A prompt belongs to the attempt that was on screen when it was opened. Once
    // that attempt (or the whole drawer) goes away, so does the prompt, and the
    // abandoned retry is discarded rather than applied to whatever is opened next.
    dismissStepUpPrompt()
    if (!show) return
    if (typeof id === 'string' && id !== '') {
      void load(id, revision)
    }
  },
  { immediate: true },
)

/** Closes an open TOTP prompt, which resolves the pending reveal as cancelled. */
function dismissStepUpPrompt() {
  if (stepUp.visible.value) {
    stepUp.onCancel()
  }
}

/**
 * Clears everything derived from a stored body or retained header values. Called
 * on every open/close and before any new fetch so a previously revealed payload
 * never survives into the next diagnostic, and is gone from memory once the
 * drawer is closed.
 */
function clearSensitiveState() {
  revealedBody.value = null
  revealedBodyOwner.value = null
  revealingBody.value = false
  bodyRevealFailed.value = false
  revealedHeaders.value = null
  revealedHeadersOwner.value = null
  revealingHeaders.value = false
  headersRevealFailed.value = false
  headersRevealBlockedReason.value = ''
  detail.value = null
  loadError.value = false
  loading.value = false
}

const canReveal = computed(() => canRevealBody(detail.value?.body_state, detail.value?.body_expires_at))

const canRevealHeaderValues = computed(() =>
  canRevealHeaders(detail.value?.header_state, detail.value?.header_expires_at),
)

/**
 * A step-up refusal that entering a code cannot fix is explained with the shared
 * step-up wording, because the reveal itself never started. Everything else stays
 * on the reveal-failure path.
 */
const headersRevealBlockedLabel = computed(() => {
  if (!headersRevealBlockedReason.value) return ''
  return headersRevealBlockedReason.value === 'STEP_UP_ADMIN_API_KEY_FORBIDDEN'
    ? t('stepUp.adminApiKeyForbidden')
    : t('stepUp.notEnabled')
})

/** Out-of-scope attempts report `not_observed`, so no empty header section is shown. */
const showHeaderValues = computed(() => hasHeaderValuesSection(detail.value?.header_state))

/**
 * `retained` is the reason of values that WERE retained, so it explains nothing
 * about the header state the section already shows.
 */
const showHeaderReason = computed(
  () => detail.value?.header_reason !== undefined && detail.value.header_reason !== 'retained',
)

/** `stored` past its window is presented as expired, never as readable. */
const effectiveHeaderStateLabel = computed(() => {
  if (isHeaderValuesStoredButExpired(detail.value?.header_state, detail.value?.header_expires_at)) {
    return headerStateLabel(t, 'expired')
  }
  return headerStateLabel(t, detail.value?.header_state)
})

/**
 * `stored` is only a server statement about the ciphertext; once the retention
 * window has passed the client must present it as expired rather than offering a
 * read the server will refuse.
 */
const effectiveBodyStateLabel = computed(() => {
  if (isStoredButExpired(detail.value?.body_state, detail.value?.body_expires_at)) {
    return bodyStateLabel(t, 'expired')
  }
  return bodyStateLabel(t, detail.value?.body_state)
})

/**
 * The reason code is the authoritative explanation for a body the admin cannot
 * read. `retained` is the reason of a body that WAS retained, so it explains
 * nothing about a missing one and is not shown.
 */
const showReason = computed(() => {
  const reason = detail.value?.reason
  return reason !== undefined && reason !== 'retained'
})

async function load(id: string, currentRevision: number) {
  loading.value = true
  loadError.value = false
  try {
    const next = await getDiagnostic(id)
    if (currentRevision === revision) {
      detail.value = next
    }
  } catch {
    if (currentRevision === revision) {
      loadError.value = true
    }
  } finally {
    if (currentRevision === revision) {
      loading.value = false
    }
  }
}

/**
 * True only while the drawer still shows the attempt a pending reveal was started
 * for. A response that arrives after the drawer closed, or after it moved to
 * another attempt, belongs to a row nobody is looking at any more.
 */
function isRevealStillCurrent(currentRevision: number, id: string): boolean {
  return (
    currentRevision === revision &&
    props.show &&
    props.diagnosticId === id &&
    detail.value?.id === id
  )
}

/**
 * Discards the plaintext a superseded reveal returned. Only that request's own
 * payload is cleared: a reveal issued after the row changed must survive an older
 * response landing late.
 */
function discardReveal<T>(
  target: Ref<T | null>,
  owner: Ref<RevealOwner | null>,
  id: string,
  currentRevision: number,
) {
  const current = owner.value
  if (current !== null && current.id === id && current.revision === currentRevision) {
    target.value = null
    owner.value = null
  }
}

interface RevealRequest<T> {
  fetch: (id: string) => Promise<T>
  target: Ref<T | null>
  owner: Ref<RevealOwner | null>
  pending: Ref<boolean>
  failed: Ref<boolean>
  /**
   * Present only for an action behind the step-up gate. A step-up refusal is then
   * reported through it instead of as a reveal failure; an ungated action (the
   * body replay) can never be refused that way, so it carries none and keeps its
   * behaviour byte for byte.
   */
  blockedReason?: Ref<string>
  /** Read against the reloaded metadata, so a stale payload is never claimed. */
  stillAvailable: () => boolean
}

/**
 * Explicit admin action only. Nothing here auto-runs: the plaintext exists in
 * this component's memory until the drawer closes, and is never stored, cached
 * or echoed from a server error message.
 *
 * The reveal is asynchronous and can outlive the row it was requested for, so its
 * result is only accepted while that same attempt is still the one on screen;
 * otherwise the plaintext is discarded instead of being rendered under whatever
 * the admin opened next. The body and the 429 headers share this flow because
 * they share the rule; only the retention they re-check differs.
 */
async function revealSensitive<T>(request: RevealRequest<T>) {
  const id = detail.value?.id ?? props.diagnosticId
  if (!id || request.pending.value) return

  const currentRevision = revision
  request.pending.value = true
  request.failed.value = false
  if (request.blockedReason) request.blockedReason.value = ''
  try {
    const revealed = await request.fetch(id)
    if (!isRevealStillCurrent(currentRevision, id)) {
      discardReveal(request.target, request.owner, id, currentRevision)
      return
    }
    request.target.value = revealed
    request.owner.value = { id, revision: currentRevision }
  } catch (error) {
    if (!isRevealStillCurrent(currentRevision, id)) {
      // A refusal for an abandoned attempt says nothing about the one on screen.
      discardReveal(request.target, request.owner, id, currentRevision)
      request.failed.value = false
      if (request.blockedReason) request.blockedReason.value = ''
      return
    }
    if (request.blockedReason) {
      if (isStepUpCancelled(error)) {
        // The admin dismissed the prompt: nothing was read, so this is neither a
        // failure nor a retention outcome.
        return
      }
      if (isStepUpBlocked(error)) {
        // Step-up cannot be satisfied by entering a code here, so say why instead
        // of reporting a reveal that never happened.
        request.blockedReason.value = stepUpBlockReason(error)
        return
      }
    }
    // The refusal is deliberately not used to infer why the values are gone;
    // re-read the metadata, which is the authoritative retention state.
    request.failed.value = true
    await load(id, currentRevision)
    if (!isRevealStillCurrent(currentRevision, id)) {
      request.failed.value = false
      return
    }
    if (request.stillAvailable()) {
      // Still readable: the failure is a real error, keep it visible.
      request.failed.value = true
    } else {
      request.failed.value = false
    }
  } finally {
    // Only the reveal that owns the current revision may clear the pending state;
    // an abandoned one must not cancel a newer request made after it.
    if (currentRevision === revision) {
      request.pending.value = false
    }
  }
}

function revealBody() {
  return revealSensitive({
    fetch: revealDiagnosticBody,
    target: revealedBody,
    owner: revealedBodyOwner,
    pending: revealingBody,
    failed: bodyRevealFailed,
    stillAvailable: () => detail.value?.body_state === 'stored' && canReveal.value,
  })
}

/** Revealing the headers never reads the body, and never reveals it: a separate action. */
function revealHeaders() {
  return revealSensitive({
    // This route is step-up gated: a refusal opens the prompt and retries this
    // same explicit POST once. The retry is still a consequence of the click.
    fetch: (id) => stepUp.run(() => revealDiagnosticHeaders(id)),
    target: revealedHeaders,
    owner: revealedHeadersOwner,
    pending: revealingHeaders,
    failed: headersRevealFailed,
    blockedReason: headersRevealBlockedReason,
    stillAvailable: () => detail.value?.header_state === 'stored' && canRevealHeaderValues.value,
  })
}
</script>
