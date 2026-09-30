<template>
  <BaseDialog :show="show" :title="showTrace ? t('admin.requestTrace.detail.title') : t('admin.usage.detail.title')" width="full" @close="emit('update:show', false)">
    <div v-if="showTrace" class="space-y-4" data-testid="usage-trace-panel">
      <button type="button" class="btn btn-secondary btn-sm" data-testid="usage-trace-back" @click="showTrace = false">{{ t('admin.usage.detail.backToUsage') }}</button>
      <RequestTraceDetailContent :show="show && showTrace" :trace-id="traceID" />
    </div>
    <div v-else-if="usage" class="space-y-6 p-6" data-testid="usage-detail-summary">
      <div class="flex flex-wrap items-center justify-end gap-3">
        <button v-if="traceID" type="button" class="btn btn-secondary btn-sm" data-testid="usage-view-trace" @click="showTrace = true">
          {{ t('admin.usage.detail.viewTrace') }}
        </button>
        <span v-else class="text-xs text-gray-500 dark:text-gray-400" data-testid="usage-trace-unavailable">{{ t('admin.usage.detail.traceUnavailable') }}</span>
        <button v-if="usage.request_audit_forced_available" type="button" class="btn btn-secondary btn-sm" data-testid="usage-forced-audit" @click="openForcedAudit">
          {{ t('admin.usage.detail.forcedAudit') }}
        </button>
      </div>
      <dl class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <div v-for="fact in facts" :key="fact.label" class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
          <dt class="text-xs font-bold uppercase tracking-wider text-gray-400">{{ t(fact.label) }}</dt>
          <dd class="mt-1 break-all text-sm font-medium text-gray-900 dark:text-white">{{ fact.value }}</dd>
        </div>
      </dl>
      <section v-if="forcedDetail || forcedLoading || forcedError" class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900" data-testid="usage-forced-audit-detail">
        <h3 class="font-medium">{{ t('admin.usage.detail.forcedAudit') }}</h3>
        <p v-if="forcedLoading" role="status">{{ t('common.loading') }}</p>
        <p v-else-if="forcedError" role="alert">{{ t('admin.usage.detail.forcedUnavailable') }}</p>
        <template v-else-if="forcedDetail">
          <p class="mt-2 text-sm">{{ t('admin.usage.requestAudit.captureCompleteness') }}: {{ forcedCompletenessLabel(forcedDetail.capture_completeness) }}</p>
          <p v-if="forcedDetail.capture_reason" class="mt-1 text-xs text-amber-700 dark:text-amber-300" data-testid="usage-forced-audit-reason">{{ forcedReasonLabel(forcedDetail.capture_reason) }}</p>
          <ol v-if="forcedDetail.attempts?.length" class="mt-3 space-y-2">
            <li v-for="(attempt, index) in forcedDetail.attempts" :key="index" class="rounded-lg border border-gray-200 p-3 text-xs dark:border-dark-700">
              {{ t('admin.usage.requestAudit.attempts') }} #{{ index + 1 }} · {{ attempt.protocol || '—' }} · {{ attempt.account_id ?? '—' }}
            </li>
          </ol>
          <p v-else class="mt-3 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.usage.requestAudit.empty') }}</p>
        </template>
      </section>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import RequestTraceDetailContent from '@/features/request-trace/RequestTraceDetailContent.vue'
import { getForcedRequestAudit, type RequestAudit } from '@/api/admin/usage'
import { formatDateTime } from '@/utils/format'
import type { AdminUsageLog } from '@/types'

const props = defineProps<{ show: boolean; usage: AdminUsageLog | null }>()
const emit = defineEmits<{ (event: 'update:show', value: boolean): void }>()
const { t } = useI18n()
const showTrace = ref(false)
const forcedDetail = ref<RequestAudit | null>(null)
const forcedLoading = ref(false)
const forcedError = ref(false)
let revision = 0

function forcedCompletenessLabel(value?: string): string {
  const known: Record<string, string> = { complete: 'complete', incomplete: 'incomplete', truncated: 'truncated', write_failed: 'writeFailed', not_captured: 'notCaptured' }
  return t(`admin.usage.requestAudit.captureStatuses.${known[value ?? ''] ?? 'unknown'}`)
}
function forcedReasonLabel(reason: string): string {
  const known = new Set(['missing_reservation', 'finalization_failed', 'write_failed_after_response_started', 'reservation_missing_after_response_started', 'cyber_policy_audit_partial'])
  return t(`admin.usage.detail.forcedReasons.${known.has(reason) ? reason : 'unknown'}`)
}

const traceID = computed(() => {
  if (!props.usage?.request_trace_available) return null
  const value = props.usage.request_trace_id?.trim() ?? ''
  return /^[0-9a-f]{32}$/.test(value) ? value : null
})
const facts = computed(() => {
  const row = props.usage
  if (!row) return []
  const value = (v: string | number | null | undefined): string => v === null || v === undefined || v === '' ? '—' : String(v)
  return [
    { label: 'admin.usage.detail.time', value: formatDateTime(row.created_at) },
    { label: 'admin.usage.detail.requestId', value: value(row.request_id) },
    { label: 'admin.usage.detail.user', value: row.user?.email ? `${row.user.email} (#${row.user_id})` : `#${row.user_id}` },
    { label: 'admin.usage.detail.apiKey', value: row.api_key?.name ? `${row.api_key.name} (#${row.api_key_id})` : `#${row.api_key_id}` },
    { label: 'admin.usage.detail.account', value: row.account?.name ? `${row.account.name} (#${row.account_id})` : value(row.account_id) },
    { label: 'admin.usage.detail.group', value: row.group?.name ? `${row.group.name} (#${row.group_id})` : value(row.group_id) },
    { label: 'admin.usage.detail.model', value: row.model_mapping_chain || (row.upstream_model && row.upstream_model !== row.model ? `${row.model} → ${row.upstream_model}` : row.model) },
    { label: 'admin.usage.detail.inbound', value: value(row.inbound_endpoint) },
    { label: 'admin.usage.detail.upstream', value: value(row.upstream_endpoint) },
    { label: 'admin.usage.detail.inputTokens', value: value(row.input_tokens) },
    { label: 'admin.usage.detail.outputTokens', value: value(row.output_tokens) },
    { label: 'admin.usage.detail.cacheReadTokens', value: value(row.cache_read_tokens) },
    { label: 'admin.usage.detail.cacheCreationTokens', value: value(row.cache_creation_tokens) },
    { label: 'admin.usage.detail.cost', value: value(row.actual_cost) },
    { label: 'admin.usage.detail.duration', value: value(row.duration_ms) },
    { label: 'admin.usage.detail.firstToken', value: value(row.first_token_ms) },
  ]
})

function openForcedAudit() {
  if (!props.show || !props.usage?.request_audit_forced_available) return
  const id = props.usage.id
  const current = ++revision
  forcedDetail.value = null
  forcedLoading.value = true
  forcedError.value = false
  void getForcedRequestAudit(id).then(result => {
    if (current === revision && props.show && props.usage?.id === id) forcedDetail.value = result
  }).catch(() => {
    if (current === revision) forcedError.value = true
  }).finally(() => {
    if (current === revision) forcedLoading.value = false
  })
}
watch(() => [props.show, props.usage?.id] as const, () => {
  revision += 1
  showTrace.value = false
  forcedDetail.value = null
  forcedLoading.value = false
  forcedError.value = false
}, { immediate: true })
</script>
