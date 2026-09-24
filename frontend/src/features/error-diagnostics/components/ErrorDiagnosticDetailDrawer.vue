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
            :disabled="revealing"
            @click="reveal"
          >
            {{ revealing ? t('admin.errorDiagnostics.detail.revealing') : t('admin.errorDiagnostics.detail.reveal') }}
          </button>
        </div>

        <p v-if="revealFailed" data-testid="error-diagnostic-reveal-failed" class="mt-3 text-xs text-red-500">
          {{ t('admin.errorDiagnostics.detail.revealFailed') }}
        </p>

        <!--
          The revealed payload is untrusted text. It is rendered through
          interpolation only, so its content can never become markup, and it is
          dropped as soon as the drawer closes.
        -->
        <div v-if="revealedBody" class="mt-3" data-testid="error-diagnostic-body">
          <pre class="max-h-96 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-gray-50 p-3 font-mono text-xs text-gray-900 dark:bg-dark-900 dark:text-dark-100">{{ revealedBody.body_text }}</pre>
          <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
            {{ t('admin.errorDiagnostics.detail.bodyBytes', { bytes: revealedBody.body_bytes }) }}
          </p>
        </div>
      </section>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { getDiagnostic, revealDiagnosticBody } from '../api'
import { bodyReasonLabel, bodyStateLabel, canRevealBody, formatDateTime, isStoredButExpired, protocolLabel } from '../labels'
import type { DiagnosticAttempt, DiagnosticBodyReveal } from '../types'

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
const revealedBody = ref<DiagnosticBodyReveal | null>(null)
/**
 * The reveal request that produced `revealedBody`: its attempt and the revision it
 * was issued in. Never rendered; it exists so a late response can only ever
 * discard its own payload, never one a newer request already delivered.
 */
const revealedBodyOwner = ref<{ id: string; revision: number } | null>(null)
const revealing = ref(false)
const revealFailed = ref(false)

/** Discards a stale response when the admin switches rows or closes the drawer. */
let revision = 0

watch(
  () => [props.show, props.diagnosticId] as const,
  ([show, id]) => {
    revision += 1
    clearSensitiveState()
    if (!show) return
    if (typeof id === 'string' && id !== '') {
      void load(id, revision)
    }
  },
  { immediate: true },
)

/**
 * Clears everything derived from a stored body. Called on every open/close and
 * before any new fetch so a previously revealed payload never survives into the
 * next diagnostic, and is gone from memory once the drawer is closed.
 */
function clearSensitiveState() {
  revealedBody.value = null
  revealedBodyOwner.value = null
  revealing.value = false
  revealFailed.value = false
  detail.value = null
  loadError.value = false
  loading.value = false
}

const canReveal = computed(() => canRevealBody(detail.value?.body_state, detail.value?.body_expires_at))

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
function discardRevealedBody(id: string, currentRevision: number) {
  const owner = revealedBodyOwner.value
  if (owner !== null && owner.id === id && owner.revision === currentRevision) {
    revealedBody.value = null
    revealedBodyOwner.value = null
  }
}

/**
 * Explicit admin action only. Nothing here auto-runs: the plaintext exists in
 * this component's memory until the drawer closes, and is never stored, cached
 * or echoed from a server error message.
 *
 * The reveal is asynchronous and can outlive the row it was requested for, so its
 * result is only accepted while that same attempt is still the one on screen;
 * otherwise the plaintext is discarded instead of being rendered under whatever
 * the admin opened next.
 */
async function reveal() {
  const id = detail.value?.id ?? props.diagnosticId
  if (!id || revealing.value) return

  const currentRevision = revision
  revealing.value = true
  revealFailed.value = false
  try {
    const revealed = await revealDiagnosticBody(id)
    if (!isRevealStillCurrent(currentRevision, id)) {
      discardRevealedBody(id, currentRevision)
      return
    }
    revealedBody.value = revealed
    revealedBodyOwner.value = { id, revision: currentRevision }
  } catch {
    if (!isRevealStillCurrent(currentRevision, id)) {
      // A refusal for an abandoned attempt says nothing about the one on screen.
      discardRevealedBody(id, currentRevision)
      revealFailed.value = false
      return
    }
    // The reveal error is deliberately not used to infer why the body is gone;
    // re-read the metadata, which is the authoritative body_state.
    revealFailed.value = true
    await load(id, currentRevision)
    if (!isRevealStillCurrent(currentRevision, id)) {
      revealFailed.value = false
      return
    }
    if (detail.value?.body_state === 'stored' && canReveal.value) {
      // Still stored: the failure is a real error, keep it visible.
      revealFailed.value = true
    } else {
      revealFailed.value = false
    }
  } finally {
    // Only the reveal that owns the current revision may clear the pending state;
    // an abandoned one must not cancel a newer request made after it.
    if (currentRevision === revision) {
      revealing.value = false
    }
  }
}
</script>
