<template>
  <section
    class="rounded-xl border border-gray-200 bg-white p-5 dark:border-dark-700 dark:bg-dark-800"
    data-testid="operator-settings"
  >
    <header class="mb-4">
      <h2 class="text-lg font-semibold tracking-tight text-gray-950 dark:text-white">
        {{ t('admin.errorDiagnostics.operator.title') }}
      </h2>
      <p class="mt-1 max-w-3xl text-sm text-gray-500 dark:text-dark-300">
        {{ t('admin.errorDiagnostics.operator.description') }}
      </p>
    </header>

    <div v-if="loading && !status" class="flex justify-center py-6" data-testid="operator-state-loading">
      <svg class="h-6 w-6 animate-spin text-primary-500" fill="none" viewBox="0 0 24 24">
        <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
        <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z" />
      </svg>
    </div>

    <!--
      An unreadable status is stated as such. It is never rendered as "off": the
      server refuses to guess, and the UI must not guess on its behalf.
    -->
    <div
      v-else-if="!status"
      data-testid="operator-state-unavailable"
      role="alert"
      class="rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-900 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-100"
    >
      {{ t('admin.errorDiagnostics.operator.unavailable') }}
    </div>

    <template v-else>
      <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm" data-testid="operator-state">
        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.operator.state.captureLabel') }}</dt>
        <dd data-testid="operator-capture-state" class="font-medium text-gray-900 dark:text-dark-100">
          {{ status.capture_allowed ? t('admin.errorDiagnostics.operator.state.on') : t('admin.errorDiagnostics.operator.state.off') }}
        </dd>

        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.operator.state.retentionLabel') }}</dt>
        <dd data-testid="operator-body-retention-state" class="font-medium text-gray-900 dark:text-dark-100">
          {{ status.body_retention_allowed ? t('admin.errorDiagnostics.operator.state.on') : t('admin.errorDiagnostics.operator.state.off') }}
        </dd>

        <!--
          The 429 header value layer is its own row with its own conclusion: it is
          independent of body retention, and showing one value for "retention" would
          misreport whichever layer is actually running.
        -->
        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.operator.state.headerValuesLabel') }}</dt>
        <dd data-testid="operator-header-values-state" class="font-medium text-gray-900 dark:text-dark-100">
          {{ status.header_values_allowed ? t('admin.errorDiagnostics.operator.state.on') : t('admin.errorDiagnostics.operator.state.off') }}
        </dd>

        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.operator.state.keyLabel') }}</dt>
        <dd data-testid="operator-encryption-key" class="text-gray-900 dark:text-dark-100">
          {{ status.body_encryption_key_available ? t('admin.errorDiagnostics.operator.state.keyAvailable') : t('admin.errorDiagnostics.operator.state.keyUnavailable') }}
        </dd>
      </dl>

      <!--
        A stored flag that is not backed by a current acknowledgement is a state the
        server reports deliberately: the storage says "on" while nothing is running.
        It is explained as exactly that, including what is missing.
      -->
      <p
        v-if="captureMismatchKey"
        data-testid="operator-capture-mismatch"
        class="mt-3 rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-100"
      >
        {{ t(captureMismatchKey) }}
      </p>
      <p
        v-if="retentionMismatchKey"
        data-testid="operator-retention-mismatch"
        class="mt-3 rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-100"
      >
        {{ t(retentionMismatchKey) }}
      </p>
      <!-- The header value layer gets its own explanation: a stored flag that is not
           in effect has its own reason, and it is not the body layer's reason. -->
      <p
        v-if="headerValuesMismatchKey"
        data-testid="operator-header-values-mismatch"
        class="mt-3 rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-100"
      >
        {{ t(headerValuesMismatchKey) }}
      </p>

      <section class="mt-4 rounded-lg border border-gray-200 p-4 dark:border-dark-700">
        <h3 class="text-sm font-medium text-gray-900 dark:text-dark-100">
          {{ t('admin.errorDiagnostics.operator.ack.version') }} {{ status.risk_version }}
        </h3>

        <div v-if="status.risk_acknowledgement" class="mt-2 space-y-1 text-xs" data-testid="operator-ack">
          <div data-testid="operator-ack-version" class="text-gray-600 dark:text-dark-300">
            {{ t('admin.errorDiagnostics.operator.ack.version') }}:
            <span class="font-mono">{{ status.risk_acknowledgement.version }}</span>
          </div>
          <div data-testid="operator-ack-operator" class="text-gray-600 dark:text-dark-300">
            {{ t('admin.errorDiagnostics.operator.ack.operator') }}:
            <span class="font-mono">#{{ status.risk_acknowledgement.admin_user_id }}</span>
          </div>
          <div data-testid="operator-ack-accepted-at" class="text-gray-600 dark:text-dark-300">
            {{ t('admin.errorDiagnostics.operator.ack.acceptedAt') }}:
            <span class="font-mono">{{ formatDateTime(status.risk_acknowledgement.accepted_at) }}</span>
          </div>
          <!-- The recorded statement is shown as plain text; it is not a secret. -->
          <div class="text-gray-600 dark:text-dark-300">
            {{ t('admin.errorDiagnostics.operator.ack.phrase') }}:
            <span class="text-gray-800 dark:text-dark-200">{{ status.risk_acknowledgement.phrase }}</span>
          </div>
        </div>
        <p v-else data-testid="operator-ack-none" class="mt-2 text-xs text-gray-500 dark:text-dark-400">
          {{ t('admin.errorDiagnostics.operator.ack.none') }}
        </p>

        <!--
          A record that does not cover the current statement is an acknowledgement
          that has to be given again; with no record at all, the notice above
          already says so.
        -->
        <p
          v-if="status.risk_acknowledgement && !status.risk_acknowledgement_current"
          data-testid="operator-ack-stale"
          class="mt-2 text-xs text-amber-700 dark:text-amber-300"
        >
          {{ t('admin.errorDiagnostics.operator.ack.stale') }}
        </p>
      </section>

      <form v-if="showEnableForm" class="mt-4 space-y-3" data-testid="operator-enable-form" @submit.prevent="enable">
        <h3 class="text-sm font-medium text-gray-900 dark:text-dark-100">
          {{ canEnableCapture ? t('admin.errorDiagnostics.operator.enable.title') : t('admin.errorDiagnostics.operator.enable.titleRetention') }}
        </h3>
        <p class="max-w-3xl text-xs text-gray-500 dark:text-dark-400">
          {{ t('admin.errorDiagnostics.operator.enable.notice') }}
        </p>

        <fieldset>
          <legend class="mb-1 text-xs font-medium text-gray-600 dark:text-dark-300">
            {{ t('admin.errorDiagnostics.operator.enable.language') }}
          </legend>
          <div class="flex flex-wrap gap-4 text-sm">
            <label v-for="language in OPERATOR_ACK_LANGUAGES" :key="language" class="inline-flex items-center gap-2">
              <input
                v-model="ackLanguage"
                type="radio"
                name="operator-ack-language"
                :value="language"
                :data-testid="`operator-language-${language}`"
                class="h-4 w-4"
              />
              <span class="text-gray-700 dark:text-dark-200">{{ ackLanguageLabel(t, language) }}</span>
            </label>
          </div>
        </fieldset>

        <div>
          <div class="mb-1 flex flex-wrap items-center justify-between gap-2">
            <span class="text-xs font-medium text-gray-600 dark:text-dark-300">
              {{ t('admin.errorDiagnostics.operator.enable.requiredPhrase') }}
            </span>
            <button type="button" class="btn btn-secondary btn-sm" data-testid="operator-copy-phrase" @click="copyPhrase">
              {{ t('admin.errorDiagnostics.operator.enable.copyPhrase') }}
            </button>
          </div>
          <!-- Selectable plain text: the operator must be able to read what they type. -->
          <p
            data-testid="operator-required-phrase"
            class="max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-100 px-3 py-2 font-mono text-xs text-gray-900 dark:bg-dark-900 dark:text-dark-100"
          >
            {{ requiredPhrase }}
          </p>
        </div>

        <div>
          <label for="error-diagnostic-operator-phrase" class="mb-1 block text-xs font-medium text-gray-600 dark:text-dark-300">
            {{ t('admin.errorDiagnostics.operator.enable.phraseLabel') }}
          </label>
          <!--
            The typed statement is component state only: it is never persisted, never
            read back and cleared as soon as the request is over.
          -->
          <textarea
            id="error-diagnostic-operator-phrase"
            v-model="typedPhrase"
            data-testid="operator-phrase-input"
            rows="3"
            autocomplete="off"
            autocapitalize="off"
            autocorrect="off"
            spellcheck="false"
            :disabled="submitting"
            :placeholder="t('admin.errorDiagnostics.operator.enable.phrasePlaceholder')"
            class="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 font-mono text-xs text-gray-900 focus:border-primary-500 focus:outline-none disabled:opacity-60 dark:border-dark-600 dark:bg-dark-900 dark:text-dark-100"
          />
        </div>

        <div v-if="showBodyRetentionToggle">
          <label class="inline-flex items-center gap-2 text-sm">
            <input
              v-model="retainBodies"
              type="checkbox"
              data-testid="operator-retention-toggle"
              :disabled="!keyAvailable"
              class="h-4 w-4"
            />
            <span class="text-gray-700 dark:text-dark-200">
              {{ t('admin.errorDiagnostics.operator.enable.retentionToggle') }}
            </span>
          </label>
          <p v-if="!keyAvailable" data-testid="operator-retention-unavailable" class="mt-1 text-xs text-gray-500 dark:text-dark-400">
            {{ t('admin.errorDiagnostics.operator.enable.retentionUnavailable') }}
          </p>
        </div>

        <!--
          The 429 header value layer is a second, independent switch: it can be turned
          on while request body retention stays off. It is never pre-selected for the
          operator beyond the intent the server already stores, and it is only
          offered when the shared stable key can actually be used.
        -->
        <div v-if="showHeaderValuesToggle">
          <label class="inline-flex items-center gap-2 text-sm">
            <input
              v-model="retainHeaderValues"
              type="checkbox"
              data-testid="operator-header-values-toggle"
              :disabled="!keyAvailable"
              class="h-4 w-4"
            />
            <span class="text-gray-700 dark:text-dark-200">
              {{ t('admin.errorDiagnostics.operator.enable.headerValuesToggle') }}
            </span>
          </label>
          <p v-if="!keyAvailable" data-testid="operator-header-values-unavailable" class="mt-1 text-xs text-gray-500 dark:text-dark-400">
            {{ t('admin.errorDiagnostics.operator.enable.headerValuesUnavailable') }}
          </p>
        </div>

        <!--
          An explicit click (or an explicit Enter in the form) is the only trigger:
          nothing here enables capture as a side effect of typing or of loading.
        -->
        <button type="submit" class="btn btn-primary" data-testid="operator-enable" :disabled="!canEnable" @click.prevent="enable">
          {{ submitting ? t('admin.errorDiagnostics.operator.enable.confirming') : t('admin.errorDiagnostics.operator.enable.confirm') }}
        </button>
      </form>

      <p v-if="retentionBlocked" data-testid="operator-retention-blocked" class="mt-3 text-xs text-gray-500 dark:text-dark-400">
        {{ t('admin.errorDiagnostics.operator.enable.retentionBlocked') }}
      </p>

      <p v-if="errorMessage" data-testid="operator-error" role="alert" class="mt-3 text-sm text-red-600 dark:text-red-400">
        {{ errorMessage }}
      </p>

      <!--
        Turning the gate off is the direction that must always work: no statement, no
        language choice and no prerequisite can block it.
      -->
      <div v-if="showDisable" class="mt-4 border-t border-gray-200 pt-3 dark:border-dark-700">
        <p class="mb-2 text-xs text-gray-500 dark:text-dark-400">
          {{ t('admin.errorDiagnostics.operator.disable.notice') }}
        </p>
        <button
          type="button"
          class="btn btn-secondary"
          data-testid="operator-disable"
          :disabled="submitting"
          @click="disable"
        >
          {{ submitting ? t('admin.errorDiagnostics.operator.disable.disabling') : t('admin.errorDiagnostics.operator.disable.action') }}
        </button>
      </div>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { getLocale } from '@/i18n'
import { updateOperatorSettings } from '../api'
import { ackLanguageLabel, formatDateTime, matchesRiskPhrase, operatorErrorMessage } from '../labels'
import {
  OPERATOR_ACK_LANGUAGES,
  type ErrorDiagnosticOperatorStatus,
  type OperatorAckLanguage,
} from '../types'

const props = defineProps<{
  /**
   * The server's last answer. `null` means "no readable status" (a failure, or a
   * request still in flight) and is rendered as unknown — never as "off".
   */
  status: ErrorDiagnosticOperatorStatus | null
  loading: boolean
}>()

const emit = defineEmits<{
  (e: 'updated', value: ErrorDiagnosticOperatorStatus): void
}>()

const { t } = useI18n()

function defaultAckLanguage(): OperatorAckLanguage {
  return getLocale() === 'zh' ? 'zh' : 'en'
}

const ackLanguage = ref<OperatorAckLanguage>(defaultAckLanguage())
const typedPhrase = ref('')
const retainBodies = ref(false)
const retainHeaderValues = ref(false)
const submitting = ref(false)
const errorMessage = ref('')

const keyAvailable = computed(() => props.status?.body_encryption_key_available === true)
const requiredPhrase = computed(() =>
  ackLanguage.value === 'zh' ? props.status?.risk_phrase_zh ?? '' : props.status?.risk_phrase_en ?? '',
)

/**
 * Capture is "not running" either because it is off, or because it is stored as on
 * without a valid acknowledgement of the current statement. Both are fixed by the
 * same deliberate action, so both offer the same enable form.
 */
const canEnableCapture = computed(
  () => props.status !== null && (!props.status.enabled || !props.status.capture_allowed),
)
const canEnableRetention = computed(
  () =>
    props.status !== null &&
    props.status.enabled &&
    props.status.capture_allowed &&
    !props.status.body_retention_enabled &&
    keyAvailable.value,
)
const canEnableHeaderValues = computed(
  () =>
    props.status !== null &&
    props.status.enabled &&
    props.status.capture_allowed &&
    !props.status.header_values_enabled &&
    keyAvailable.value,
)
/**
 * Either retention layer being off is a reason to show the form on its own: the
 * layers are independent, so "body retention is already on" must not hide the
 * switch that turns header value retention on.
 */
const showEnableForm = computed(
  () => canEnableCapture.value || canEnableRetention.value || canEnableHeaderValues.value,
)
/**
 * A toggle is only rendered for a layer this form can actually change: when
 * capture itself has to be (re-)enabled both layers are shown, otherwise only the
 * layer that is still off. A layer that is already on is not offered for turning
 * off here — disabling is the documented action for that, and it turns both off.
 */
const showBodyRetentionToggle = computed(() => canEnableCapture.value || canEnableRetention.value)
const showHeaderValuesToggle = computed(() => canEnableCapture.value || canEnableHeaderValues.value)
/**
 * No usable key: neither layer can be turned on, and the notice names both so the
 * operator is not left guessing why the switches are unavailable. It is never
 * rendered as a silent no-op toggle.
 */
const retentionBlocked = computed(
  () =>
    props.status !== null &&
    props.status.enabled &&
    props.status.capture_allowed &&
    !keyAvailable.value &&
    (!props.status.body_retention_enabled || !props.status.header_values_enabled),
)
const showDisable = computed(
  () =>
    props.status !== null &&
    (props.status.enabled || props.status.body_retention_enabled || props.status.header_values_enabled),
)

/** Stored on but nothing is running: which acknowledgement is missing. */
const captureMismatchKey = computed(() => {
  const status = props.status
  if (status === null || !status.enabled || status.capture_allowed) return ''
  return status.risk_acknowledged && !status.risk_acknowledgement_current
    ? 'admin.errorDiagnostics.operator.state.captureMismatchStaleAck'
    : 'admin.errorDiagnostics.operator.state.captureMismatchNoAck'
})

/** Stored retention that is not in effect: no usable key, or capture is not running. */
const retentionMismatchKey = computed(() => {
  const status = props.status
  if (status === null || !status.body_retention_enabled || status.body_retention_allowed) return ''
  return status.body_encryption_key_available
    ? 'admin.errorDiagnostics.operator.state.retentionMismatchCapture'
    : 'admin.errorDiagnostics.operator.state.retentionMismatchKey'
})

/** The same question for the header value layer, answered from its own two flags. */
const headerValuesMismatchKey = computed(() => {
  const status = props.status
  if (status === null || !status.header_values_enabled || status.header_values_allowed) return ''
  return status.body_encryption_key_available
    ? 'admin.errorDiagnostics.operator.state.headerValuesMismatchCapture'
    : 'admin.errorDiagnostics.operator.state.headerValuesMismatchKey'
})

/**
 * The stored intent of one layer, which is what the matching toggle is pre-set to.
 *
 * This is not an automatic opt-in: the flag is already recorded server-side, the
 * toggle is rendered with that value so re-giving an acknowledgement does not
 * silently drop it, and the operator can clear it before submitting. It is only
 * ever pre-set when the shared key can be used.
 */
function storedRetentionIntent(
  status: ErrorDiagnosticOperatorStatus | null,
  layer: 'body_retention_enabled' | 'header_values_enabled',
): boolean {
  if (status === null || !status.body_encryption_key_available) return false
  return status[layer] === true
}

/**
 * The confirmation is deliberate every time: the server refuses an enable without a
 * freshly typed statement, so the button stays unavailable until the exact text is
 * present, and never for a statement the operator has not typed themselves.
 */
const canEnable = computed(
  () => showEnableForm.value && !submitting.value && matchesRiskPhrase(typedPhrase.value, requiredPhrase.value),
)

// A new statement (or a new server state) discards anything already typed.
watch(requiredPhrase, () => {
  typedPhrase.value = ''
})
watch(
  () => props.status,
  (status) => {
    typedPhrase.value = ''
    // Each layer's intent comes from the server's stored flag, so re-giving an
    // acknowledgement does not silently drop a retention choice that is already
    // recorded. It is only offered when the key can actually be used.
    retainBodies.value = storedRetentionIntent(status, 'body_retention_enabled')
    retainHeaderValues.value = storedRetentionIntent(status, 'header_values_enabled')
    errorMessage.value = ''
  },
  { immediate: true },
)

async function enable() {
  if (!canEnable.value || !props.status) return
  const status = props.status

  submitting.value = true
  errorMessage.value = ''
  try {
    const next = await updateOperatorSettings({
      enabled: true,
      // Both layers are always stated, because the server reads an omitted field as
      // off. A layer this form is changing is sent exactly as the operator sees it;
      // a layer it is not touching keeps the value the server last reported.
      body_retention_enabled: showBodyRetentionToggle.value
        ? retainBodies.value && keyAvailable.value
        : status.body_retention_enabled,
      header_values_enabled: showHeaderValuesToggle.value
        ? retainHeaderValues.value && keyAvailable.value
        : status.header_values_enabled,
      language: ackLanguage.value,
      phrase: typedPhrase.value.trim(),
    })
    // The statement is not kept: the next enable has to be given again.
    typedPhrase.value = ''
    retainBodies.value = false
    retainHeaderValues.value = false
    // The server's answer is the state; nothing is assumed from what was requested.
    emit('updated', next)
  } catch (error) {
    errorMessage.value = operatorErrorMessage(t, (error as { reason?: unknown } | null)?.reason)
  } finally {
    submitting.value = false
  }
}

async function disable() {
  submitting.value = true
  errorMessage.value = ''
  try {
    const next = await updateOperatorSettings({
      enabled: false,
      // Turning the gate off turns both retention layers off with it; the server
      // enforces that, and this states the same intent.
      body_retention_enabled: false,
      header_values_enabled: false,
      language: ackLanguage.value,
      // Disabling is not an acknowledgement, so no statement is carried with it.
      phrase: '',
    })
    typedPhrase.value = ''
    retainBodies.value = false
    retainHeaderValues.value = false
    emit('updated', next)
  } catch (error) {
    errorMessage.value = operatorErrorMessage(t, (error as { reason?: unknown } | null)?.reason)
  } finally {
    submitting.value = false
  }
}

/** Explicit action only — the statement is never auto-copied anywhere. */
async function copyPhrase() {
  const statement = requiredPhrase.value
  if (statement === '') return
  try {
    await navigator.clipboard?.writeText?.(statement)
  } catch {
    // Copying is a convenience; a refused clipboard must not look like a failure of
    // the gate itself.
  }
}
</script>
