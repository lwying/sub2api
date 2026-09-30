<template>
  <section class="card" data-testid="request-trace-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.requestTrace.operator.title') }}</h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.requestTrace.operator.description') }}</p>
    </div>
    <div class="space-y-4 p-6">
      <p v-if="loading && !status" data-testid="request-trace-loading">{{ t('common.loading') }}</p>
      <p v-else-if="!status" role="alert" data-testid="request-trace-unavailable">{{ t('admin.requestTrace.operator.unavailable') }}</p>
      <template v-else>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-2 text-sm">
          <dt>{{ t('admin.requestTrace.operator.stored') }}</dt>
          <dd data-testid="request-trace-stored" :data-state="status.enabled ? 'on' : 'off'">
            {{ status.enabled ? t('admin.requestTrace.operator.on') : t('admin.requestTrace.operator.off') }}
          </dd>
          <dt>{{ t('admin.requestTrace.operator.effective') }}</dt>
          <dd data-testid="request-trace-capture-state" :data-state="status.capture_allowed ? 'on' : 'off'">
            {{ status.capture_allowed ? t('admin.requestTrace.operator.on') : t('admin.requestTrace.operator.off') }}
          </dd>
          <dt>{{ t('admin.requestTrace.operator.deployment') }}</dt>
          <dd data-testid="request-trace-deployment">{{ t(`admin.requestTrace.operator.support.${status.plaintext_capture_support_reason}`) }}</dd>
          <!-- The stored scope is shown from the server's own values, never from the draft below. -->
          <dt>{{ t('admin.requestTrace.operator.scope.storedGroups') }}</dt>
          <dd data-testid="request-trace-scope-stored-groups">{{ storedGroups }}</dd>
          <dt>{{ t('admin.requestTrace.operator.scope.storedModels') }}</dt>
          <dd data-testid="request-trace-scope-stored-models">{{ storedModels }}</dd>
          <dt>{{ t('admin.requestTrace.operator.scope.storedPlatforms') }}</dt>
          <dd data-testid="request-trace-scope-stored-platforms">{{ storedPlatforms }}</dd>
        </dl>
        <p v-if="!status.plaintext_capture_supported" role="alert" data-testid="request-trace-deployment-blocked" class="text-amber-700 dark:text-amber-300">
          {{ t('admin.requestTrace.operator.deploymentBlocked') }}
        </p>
        <p v-if="status.enabled && !status.capture_allowed && !status.risk_acknowledgement_current" role="alert" data-testid="request-trace-ack-stale" class="text-amber-700 dark:text-amber-300">
          {{ t('admin.requestTrace.operator.ackStale') }}
        </p>
        <p class="text-sm">{{ t('admin.requestTrace.operator.riskVersion') }}: <span class="font-mono">{{ status.risk_version }}</span></p>
        <div v-if="!status.capture_allowed" class="space-y-3" data-testid="request-trace-enable-form">
          <label class="block text-sm" for="request-trace-language">{{ t('admin.requestTrace.operator.language') }}</label>
          <select id="request-trace-language" v-model="language" data-testid="request-trace-language" class="rounded-lg border p-2 dark:bg-dark-900">
            <option value="en">{{ ackLanguageLabel(t, 'en') }}</option>
            <option value="zh">{{ ackLanguageLabel(t, 'zh') }}</option>
          </select>
          <p class="text-sm">{{ t('admin.requestTrace.operator.requiredPhrase') }}</p>
          <p class="rounded-lg border p-3 text-xs" data-testid="request-trace-required-phrase">{{ requiredPhrase }}</p>
          <label class="block text-sm" for="request-trace-phrase">{{ t('admin.requestTrace.operator.typePhrase') }}</label>
          <textarea id="request-trace-phrase" v-model="typedPhrase" data-testid="request-trace-phrase-input" rows="3" autocomplete="off" spellcheck="false" class="w-full rounded-lg border p-3 text-xs dark:bg-dark-900" />
          <button type="button" class="btn btn-primary" data-testid="request-trace-enable" :disabled="!canEnable" @click="enable">{{ t('admin.requestTrace.operator.enable') }}</button>
        </div>
        <div v-if="status.enabled" class="border-t pt-3 dark:border-dark-700">
          <button type="button" class="btn btn-secondary" data-testid="request-trace-disable" :disabled="submitting" @click="disable">
            {{ t('admin.requestTrace.operator.disable') }}
          </button>
        </div>

        <!--
          The capture scope controls which *future* requests are captured. It is
          submitted only through its own save action: an enable/disable action
          must never rewrite it, and a scope save always sends the whole scope
          rather than the fields the operator happened to touch.
        -->
        <div class="space-y-4 border-t pt-4 dark:border-dark-700" data-testid="request-trace-scope-form">
          <div>
            <h3 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.requestTrace.operator.scope.title') }}</h3>
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.requestTrace.operator.scope.description') }}</p>
          </div>

          <fieldset class="space-y-2">
            <legend class="text-sm">{{ t('admin.requestTrace.operator.scope.groupsLabel') }}</legend>
            <label class="flex items-center gap-2 text-sm">
              <input v-model="allGroups" data-testid="request-trace-scope-all-groups" type="checkbox" />
              <span>{{ t('admin.requestTrace.operator.scope.allGroups') }}</span>
            </label>
            <template v-if="!allGroups">
              <label class="block text-sm" for="request-trace-scope-group-ids">{{ t('admin.requestTrace.operator.scope.groupIdsLabel') }}</label>
              <input
                id="request-trace-scope-group-ids"
                v-model.trim="groupIDs"
                data-testid="request-trace-scope-group-ids"
                class="w-full rounded-lg border p-2 font-mono text-xs dark:bg-dark-900"
                inputmode="numeric"
                autocomplete="off"
              />
              <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.requestTrace.operator.scope.groupIdsHint') }}</p>
            </template>
            <p class="text-xs text-gray-500 dark:text-gray-400" data-testid="request-trace-scope-groups-note">
              {{ t('admin.requestTrace.operator.scope.groupsNote') }}
            </p>
          </fieldset>

          <fieldset class="space-y-2">
            <legend class="text-sm">{{ t('admin.requestTrace.operator.scope.modelsLabel') }}</legend>
            <select v-model="modelScope" data-testid="request-trace-scope-model-mode" class="w-full rounded-lg border p-2 text-sm dark:bg-dark-900">
              <option value="all">{{ t('admin.requestTrace.operator.scope.modelAll') }}</option>
              <option value="include">{{ t('admin.requestTrace.operator.scope.modelInclude') }}</option>
              <option value="exclude">{{ t('admin.requestTrace.operator.scope.modelExclude') }}</option>
            </select>
            <template v-if="modelScope !== 'all'">
              <label class="block text-sm" for="request-trace-scope-models">{{ t('admin.requestTrace.operator.scope.modelsListLabel') }}</label>
              <input
                id="request-trace-scope-models"
                v-model.trim="models"
                data-testid="request-trace-scope-models"
                class="w-full rounded-lg border p-2 font-mono text-xs dark:bg-dark-900"
                autocomplete="off"
              />
              <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.requestTrace.operator.scope.modelsHint') }}</p>
            </template>
            <p class="text-xs text-gray-500 dark:text-gray-400" data-testid="request-trace-scope-models-note">
              {{ t('admin.requestTrace.operator.scope.modelsNote') }}
            </p>
          </fieldset>

          <fieldset class="space-y-2">
            <legend class="text-sm">{{ t('admin.requestTrace.operator.scope.platformsLabel') }}</legend>
            <select v-model="platformScope" data-testid="request-trace-scope-platform-mode" class="w-full rounded-lg border p-2 text-sm dark:bg-dark-900">
              <option value="all">{{ t('admin.requestTrace.operator.scope.platformAll') }}</option>
              <option value="include">{{ t('admin.requestTrace.operator.scope.platformInclude') }}</option>
              <option value="exclude">{{ t('admin.requestTrace.operator.scope.platformExclude') }}</option>
            </select>
            <template v-if="platformScope !== 'all'">
              <label class="block text-sm" for="request-trace-scope-platforms">{{ t('admin.requestTrace.operator.scope.platformsListLabel') }}</label>
              <input
                id="request-trace-scope-platforms"
                v-model.trim="platforms"
                data-testid="request-trace-scope-platforms"
                class="w-full rounded-lg border p-2 font-mono text-xs dark:bg-dark-900"
                autocomplete="off"
              />
              <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.requestTrace.operator.scope.platformsHint') }}</p>
            </template>
            <p class="text-xs text-gray-500 dark:text-gray-400" data-testid="request-trace-scope-platforms-note">
              {{ t('admin.requestTrace.operator.scope.platformsNote') }}
            </p>
            <p class="text-xs text-amber-700 dark:text-amber-300" data-testid="request-trace-scope-platform-risk">
              {{ t('admin.requestTrace.operator.scope.excludeRisk') }}
            </p>
          </fieldset>

          <p v-if="scopeNeedsPhrase" class="text-xs text-gray-600 dark:text-gray-400" data-testid="request-trace-scope-phrase-note">
            {{ t('admin.requestTrace.operator.scope.phraseRequired') }}
          </p>
          <!--
            Exactly one acknowledgement field is on screen at a time: while the
            enable form is visible it owns the field, otherwise the scope save
            has its own. The two actions stay separate, and the operator always
            types the statement rather than having it replayed for them.
          -->
          <template v-if="showScopePhrase">
            <label class="block text-sm" for="request-trace-scope-phrase">{{ t('admin.requestTrace.operator.scope.phraseLabel') }}</label>
            <textarea
              id="request-trace-scope-phrase"
              v-model="scopePhrase"
              data-testid="request-trace-scope-phrase"
              rows="2"
              autocomplete="off"
              spellcheck="false"
              class="w-full rounded-lg border p-3 text-xs dark:bg-dark-900"
            />
          </template>
          <button type="button" class="btn btn-primary" data-testid="request-trace-scope-save" :disabled="!canSaveScope" @click="saveScope">
            {{ submitting ? t('admin.requestTrace.operator.scope.saving') : t('admin.requestTrace.operator.scope.save') }}
          </button>
          <p v-if="scopeSaved" role="status" data-testid="request-trace-scope-saved" class="text-sm text-green-700 dark:text-green-300">
            {{ t('admin.requestTrace.operator.scope.updated') }}
          </p>
          <p v-if="scopeError" role="alert" data-testid="request-trace-scope-error" class="text-amber-700 dark:text-amber-300">
            {{ t('admin.requestTrace.operator.scope.invalid') }}
          </p>
        </div>

        <p v-if="errorMessage" role="alert" data-testid="request-trace-error">{{ errorMessage }}</p>
      </template>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { getLocale } from '@/i18n'
import { updateOperatorSettings } from './api'
import { ackLanguageLabel } from './labels'
import type { RequestTraceOperatorStatus, RequestTraceScope, TraceAckLanguage } from './types'

const props = defineProps<{ status: RequestTraceOperatorStatus | null; loading: boolean }>()
const emit = defineEmits<{ (e: 'updated', status: RequestTraceOperatorStatus): void }>()
const { t } = useI18n()
const language = ref<TraceAckLanguage>(getLocale() === 'zh' ? 'zh' : 'en')
const typedPhrase = ref('')
const submitting = ref(false)
const errorMessage = ref('')
const requiredPhrase = computed(() => language.value === 'zh' ? props.status?.risk_phrase_zh ?? '' : props.status?.risk_phrase_en ?? '')
const canEnable = computed(() =>
  !!props.status && !submitting.value && props.status.plaintext_capture_supported &&
  requiredPhrase.value !== '' && typedPhrase.value.trim() === requiredPhrase.value,
)

// The scope draft. It is a draft, not the stored scope: the read-only summary
// above is the only place the server's own values are shown.
const allGroups = ref(true)
const groupIDs = ref('')
const modelScope = ref<RequestTraceScope>('all')
const models = ref('')
const platformScope = ref<RequestTraceScope>('all')
const platforms = ref('')
const scopePhrase = ref('')
const scopeError = ref(false)
const scopeSaved = ref(false)
// Set while the draft is being replaced by server values, so that a re-sync is
// not mistaken for an operator edit.
let syncingScope = false

/** Replaces the draft with the stored scope. Only complete server values are applied. */
function syncScopeDraft(status: RequestTraceOperatorStatus) {
  syncingScope = true
  allGroups.value = status.all_groups
  groupIDs.value = status.group_ids.join(', ')
  modelScope.value = status.model_scope
  models.value = status.models.join(', ')
  platformScope.value = status.platform_scope
  platforms.value = status.platforms.join(', ')
  syncingScope = false
}

watch(() => props.status, status => {
  if (status) syncScopeDraft(status)
}, { immediate: true })
// A save is confirmed only until the operator edits the draft again.
watch([allGroups, groupIDs, modelScope, models, platformScope, platforms], () => {
  if (!syncingScope) scopeSaved.value = false
}, { flush: 'sync' })

watch([requiredPhrase, () => props.status], () => {
  typedPhrase.value = ''
  scopePhrase.value = ''
  errorMessage.value = ''
  scopeError.value = false
})

/** A group id list is a list of positive integers; anything else is not an ID. */
function parseScopeGroupIDs(raw: string): number[] | null {
  const ids: number[] = []
  for (const part of raw.split(/[\s,]+/)) {
    if (part === '') continue
    if (!/^[0-9]+$/.test(part)) return null
    const value = Number(part)
    if (!Number.isSafeInteger(value) || value <= 0) return null
    if (!ids.includes(value)) ids.push(value)
  }
  return ids.length > 0 ? ids : null
}

/**
 * Model and platform names are compared without regard to case, so a repeated
 * spelling is one entry. Names keep the operator's own spelling; a blank list is
 * refused rather than submitted as "match nothing".
 */
function parseScopeNames(raw: string): string[] | null {
  const names: string[] = []
  const seen = new Set<string>()
  for (const part of raw.split(/[\s,]+/)) {
    if (part === '') continue
    const key = part.toLowerCase()
    if (seen.has(key)) continue
    seen.add(key)
    names.push(part)
  }
  return names.length > 0 ? names : null
}

function scopeValues(kind: RequestTraceScope, entries: readonly (string | number)[]): string {
  if (kind === 'all') return t('admin.requestTrace.operator.scope.allValues')
  if (entries.length === 0) return t('admin.requestTrace.operator.scope.emptyValues')
  const values = entries.join(', ')
  return kind === 'include'
    ? t('admin.requestTrace.operator.scope.onlyValues', { values })
    : t('admin.requestTrace.operator.scope.exceptValues', { values })
}

const storedGroups = computed(() => {
  if (!props.status) return ''
  return props.status.all_groups
    ? t('admin.requestTrace.operator.scope.allValues')
    : scopeValues('include', props.status.group_ids)
})
const storedModels = computed(() => props.status ? scopeValues(props.status.model_scope, props.status.models) : '')
const storedPlatforms = computed(() => props.status ? scopeValues(props.status.platform_scope, props.status.platforms) : '')

/**
 * Saving the scope is a settings write like any other: while capture is stored
 * as on, the server writes a new acknowledgement with it, so the statement has
 * to be typed again. The field the operator types into is the enable form's own
 * field whenever that form is on screen.
 */
const scopeNeedsPhrase = computed(() => props.status?.enabled === true)
const showScopePhrase = computed(() => scopeNeedsPhrase.value && props.status?.capture_allowed === true)
const scopePhraseInput = computed(() => showScopePhrase.value ? scopePhrase.value : typedPhrase.value)
const scopePhraseReady = computed(() => !scopeNeedsPhrase.value || scopePhraseInput.value.trim() === requiredPhrase.value)
const canSaveScope = computed(() => !!props.status && !submitting.value && scopePhraseReady.value)

/** The complete scope object, or `null` when the draft is not a usable scope. */
function scopePayload(): {
  all_groups: boolean
  group_ids: number[]
  model_scope: RequestTraceScope
  models: string[]
  platform_scope: RequestTraceScope
  platforms: string[]
} | null {
  const ids = allGroups.value ? [] : parseScopeGroupIDs(groupIDs.value)
  if (ids === null) return null
  let modelList: string[] = []
  if (modelScope.value !== 'all') {
    const parsed = parseScopeNames(models.value)
    if (parsed === null) return null
    modelList = parsed
  }
  let platformList: string[] = []
  if (platformScope.value !== 'all') {
    const parsed = parseScopeNames(platforms.value)
    if (parsed === null) return null
    platformList = parsed
  }
  return {
    all_groups: allGroups.value,
    group_ids: ids,
    model_scope: modelScope.value,
    models: modelList,
    platform_scope: platformScope.value,
    platforms: platformList,
  }
}

async function saveScope() {
  const current = props.status
  if (!current || submitting.value) return
  scopeError.value = false
  errorMessage.value = ''
  const scope = scopePayload()
  if (scope === null) {
    scopeError.value = true
    return
  }
  // The enable form owns the field while it is on screen; the button is disabled
  // until the statement matches either way.
  if (scopeNeedsPhrase.value && scopePhraseInput.value.trim() !== requiredPhrase.value) {
    scopeError.value = true
    return
  }
  submitting.value = true
  try {
    // The switch keeps its stored value: this action edits the scope, not the
    // switch. `scope_provided` is what tells the server to replace the scope.
    const next = await updateOperatorSettings({
      enabled: current.enabled,
      language: language.value,
      phrase: scopePhraseInput.value.trim(),
      scope_provided: true,
      ...scope,
    })
    scopePhrase.value = ''
    // The acknowledgement is consumed only when the gate is on; a scope save on
    // a disabled gate never used the field, so anything typed for enabling stays.
    if (scopeNeedsPhrase.value) typedPhrase.value = ''
    scopeSaved.value = true
    emit('updated', next)
  } catch {
    errorMessage.value = t('admin.requestTrace.operator.scope.saveFailed')
  } finally {
    submitting.value = false
  }
}

async function enable() {
  if (!canEnable.value) return
  submitting.value = true
  errorMessage.value = ''
  try {
    // A switch update never carries scope fields, so the stored scope survives it.
    const next = await updateOperatorSettings({ enabled: true, language: language.value, phrase: typedPhrase.value.trim() })
    typedPhrase.value = ''
    emit('updated', next)
  } catch {
    errorMessage.value = t('admin.requestTrace.operator.updateFailed')
  } finally {
    submitting.value = false
  }
}

async function disable() {
  if (!props.status || submitting.value) return
  submitting.value = true
  errorMessage.value = ''
  try {
    const next = await updateOperatorSettings({ enabled: false, language: language.value, phrase: '' })
    typedPhrase.value = ''
    emit('updated', next)
  } catch {
    errorMessage.value = t('admin.requestTrace.operator.updateFailed')
  } finally {
    submitting.value = false
  }
}
</script>
