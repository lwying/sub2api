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
import type { RequestTraceOperatorStatus, TraceAckLanguage } from './types'

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
watch([requiredPhrase, () => props.status], () => {
  typedPhrase.value = ''
  errorMessage.value = ''
})

async function enable() {
  if (!canEnable.value) return
  submitting.value = true
  errorMessage.value = ''
  try {
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
