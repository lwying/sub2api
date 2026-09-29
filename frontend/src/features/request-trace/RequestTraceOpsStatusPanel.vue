<template>
  <section class="rounded-xl border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800" data-testid="request-trace-ops">
    <div class="flex flex-wrap items-start justify-between gap-3">
      <div>
        <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.requestTrace.ops.title') }}</h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.description') }}</p>
      </div>
      <button type="button" class="btn btn-secondary btn-sm" data-testid="request-trace-ops-refresh" @click="load">
        {{ loading ? t('admin.requestTrace.ops.refreshing') : t('admin.requestTrace.ops.refresh') }}
      </button>
    </div>
    <p class="mt-2 text-xs text-gray-500 dark:text-dark-300" data-testid="request-trace-ops-gate-note">{{ t('admin.requestTrace.ops.unknownGateNote') }}</p>

    <template v-if="status">
      <dl class="mt-4 grid grid-cols-[max-content_1fr] items-baseline gap-x-4 gap-y-2 text-sm">
        <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.probeLabel') }}</dt>
        <dd data-testid="request-trace-ops-probe">{{ probeLabel(status.storage_probe) }}</dd>
        <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.capture.storageLabel') }}</dt>
        <dd data-testid="request-trace-ops-storage">{{ storageLabel(status.capture.storage) }}</dd>
        <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.capture.repository') }}</dt>
        <dd data-testid="request-trace-ops-repository">{{ yesNo(status.capture.repository_available) }}</dd>
        <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.capture.stopped') }}</dt>
        <dd data-testid="request-trace-ops-stopped">{{ yesNo(status.capture.stopped) }}</dd>
        <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.capture.queue') }}</dt>
        <dd class="font-mono" data-testid="request-trace-ops-queue">{{ status.capture.queue_depth }} / {{ status.capture.queue_capacity }}</dd>
      </dl>

      <div class="mt-4 grid gap-3 sm:grid-cols-3 xl:grid-cols-5">
        <div v-for="counter in queueCounters" :key="counter.key" class="rounded-lg border border-gray-200 p-3 dark:border-dark-700">
          <div class="text-xs text-gray-500 dark:text-dark-300">{{ counter.label }}</div>
          <div class="mt-1 font-mono text-lg" :data-testid="counter.testid">{{ counter.value }}</div>
        </div>
      </div>
      <p class="mt-2 text-xs text-gray-500 dark:text-dark-300" data-testid="request-trace-ops-counters-note">{{ t('admin.requestTrace.ops.capture.countersNote') }}</p>

      <div class="mt-4 border-t border-gray-200 pt-4 dark:border-dark-700">
        <h3 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.requestTrace.ops.export.title') }}</h3>
        <dl class="mt-2 grid grid-cols-[max-content_1fr] items-baseline gap-x-4 gap-y-1 text-sm" data-testid="request-trace-ops-export">
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.export.started') }}</dt>
          <dd data-testid="request-trace-ops-export-started">{{ yesNo(status.export.worker_started) }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.export.ticks') }}</dt>
          <dd class="font-mono">{{ status.export.ticks }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.export.tasksRun') }}</dt>
          <dd class="font-mono">{{ status.export.tasks_run }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.export.tasksCompleted') }}</dt>
          <dd class="font-mono">{{ status.export.tasks_completed }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.export.tasksFailed') }}</dt>
          <dd class="font-mono">{{ status.export.tasks_failed }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.export.failures') }}</dt>
          <dd class="font-mono">{{ status.export.failures }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.export.disabledTicks') }}</dt>
          <dd class="font-mono">{{ status.export.disabled_ticks }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.export.cleanups') }}</dt>
          <dd class="font-mono">{{ status.export.cleanups }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.export.cleanedFiles') }}</dt>
          <dd class="font-mono">{{ status.export.cleaned_files }}</dd>
        </dl>
      </div>

      <div class="mt-4 border-t border-gray-200 pt-4 dark:border-dark-700">
        <h3 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.requestTrace.ops.cleanup.title') }}</h3>
        <dl class="mt-2 grid grid-cols-[max-content_1fr] items-baseline gap-x-4 gap-y-1 text-sm">
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.cleanup.runs') }}</dt>
          <dd class="font-mono" data-testid="request-trace-ops-cleanup-runs">{{ status.cleanup.runs }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.cleanup.deleted') }}</dt>
          <dd class="font-mono">{{ status.cleanup.deleted }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.cleanup.failures') }}</dt>
          <dd class="font-mono">{{ status.cleanup.failures }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.cleanup.lastDeleted') }}</dt>
          <dd class="font-mono">{{ status.cleanup.last_deleted }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.cleanup.backlogLabel') }}</dt>
          <dd data-testid="request-trace-ops-backlog" :data-state="status.cleanup.backlog_state">
            <span class="font-mono" data-testid="request-trace-ops-backlog-value">{{ backlogValue(status) }}</span>
            <span class="ml-2 text-xs text-gray-500 dark:text-dark-300" data-testid="request-trace-ops-backlog-state">{{ backlogStateLabel(status.cleanup.backlog_state) }}</span>
          </dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.ops.cleanup.backlogLimit') }}</dt>
          <dd class="font-mono" data-testid="request-trace-ops-backlog-limit">{{ status.cleanup.backlog_limit }}</dd>
        </dl>
        <p v-if="status.cleanup.backlog_state === 'at_least'" class="mt-2 text-xs text-amber-700 dark:text-amber-300" data-testid="request-trace-ops-backlog-note">{{ t('admin.requestTrace.ops.cleanup.atLeastNote') }}</p>
        <p v-else-if="status.cleanup.backlog_state === 'unavailable'" class="mt-2 text-xs text-amber-700 dark:text-amber-300" data-testid="request-trace-ops-backlog-note">{{ t('admin.requestTrace.ops.cleanup.unavailableNote') }}</p>
      </div>
    </template>
    <p v-else-if="failed" role="alert" class="mt-4 rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-900 dark:bg-red-950/20" data-testid="request-trace-ops-failed">{{ t('admin.requestTrace.ops.failed') }}</p>
    <p v-else role="status" class="mt-4 py-4 text-center text-sm text-gray-500 dark:text-dark-300" data-testid="request-trace-ops-loading">{{ t('admin.requestTrace.ops.loading') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { getTraceOpsStatus } from './api'
import {
  requestTraceBacklogStates,
  requestTraceStorageProbeStates,
  requestTraceStorageStates,
  type RequestTraceOpsStatus,
} from './types'

const { t } = useI18n()
const status = ref<RequestTraceOpsStatus | null>(null)
const failed = ref(false)
const loading = ref(false)
let revision = 0
let controller: AbortController | null = null

/**
 * The one key a closed-set value renders through: its own label, or the generic
 * fallback when the value is outside the set this build knows about. The raw
 * wire value is never the visible text.
 */
function closedKey(scope: string, values: readonly string[], value: string): string {
  return `admin.requestTrace.ops.${scope}.${values.includes(value) ? value : 'unknown'}`
}

function probeLabel(value: string): string {
  return t(closedKey('storageProbe', requestTraceStorageProbeStates, value))
}

function storageLabel(value: string): string {
  return t(closedKey('capture.storage', requestTraceStorageStates, value))
}

function backlogStateLabel(value: string): string {
  return t(closedKey('cleanup.backlogState', requestTraceBacklogStates, value))
}

/** A flag renders as its own bounded label, never as a raw `true`/`false` token. */
function yesNo(value: boolean): string {
  return value ? t('admin.requestTrace.ops.yes') : t('admin.requestTrace.ops.no')
}

/**
 * The backlog count is only shown where it means something: a capped or
 * unmeasured count is not a total, so an unmeasured one renders as absent
 * rather than as the zero the server sends alongside it.
 */
function backlogValue(current: RequestTraceOpsStatus): string {
  return current.cleanup.backlog_state === 'unavailable' ? '—' : String(current.cleanup.unlinked_backlog)
}

const queueCounters = computed(() => {
  const capture = status.value?.capture
  if (!capture) return []
  return [
    { key: 'accepted', testid: 'request-trace-ops-accepted', label: t('admin.requestTrace.ops.capture.accepted'), value: capture.accepted },
    { key: 'stored', testid: 'request-trace-ops-stored', label: t('admin.requestTrace.ops.capture.stored'), value: capture.stored },
    { key: 'write_failed', testid: 'request-trace-ops-write-failed', label: t('admin.requestTrace.ops.capture.writeFailed'), value: capture.write_failed },
    { key: 'dropped', testid: 'request-trace-ops-dropped', label: t('admin.requestTrace.ops.capture.dropped'), value: capture.dropped },
    { key: 'rejected', testid: 'request-trace-ops-rejected', label: t('admin.requestTrace.ops.capture.rejected'), value: capture.rejected },
  ]
})

/**
 * One manual read. The counters move slowly and the endpoint takes a bounded
 * database read, so there is no poller here — and a read that cannot be
 * completed leaves the panel unknown instead of showing the previous numbers
 * as if they were current.
 */
async function load() {
  const current = ++revision
  controller?.abort()
  const requestController = new AbortController()
  controller = requestController
  loading.value = true
  try {
    const next = await getTraceOpsStatus({ signal: requestController.signal })
    if (current !== revision) return
    status.value = next
    failed.value = false
  } catch {
    if (current !== revision) return
    status.value = null
    failed.value = true
  } finally {
    if (current === revision) loading.value = false
  }
}

onMounted(() => { void load() })
onBeforeUnmount(() => {
  revision += 1
  controller?.abort()
  status.value = null
})
</script>
