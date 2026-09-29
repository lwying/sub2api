<template>
  <section class="rounded-xl border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800" data-testid="request-trace-export">
    <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.requestTrace.export.title') }}</h2>
    <p class="mt-1 text-sm text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.export.description') }}</p>
    <ul class="mt-3 space-y-1 rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-800 dark:border-amber-900 dark:bg-amber-950/20 dark:text-amber-200" data-testid="request-trace-export-disclosure">
      <li>{{ t('admin.requestTrace.export.download.sessionOnly') }}</li>
      <li>{{ t('admin.requestTrace.export.download.plaintextNote') }}</li>
      <li>{{ t('admin.requestTrace.export.download.notErasable') }}</li>
      <li>{{ t('admin.requestTrace.export.progress.notSnapshot') }}</li>
    </ul>

    <div class="mt-4 grid gap-3 md:grid-cols-2 xl:grid-cols-4">
      <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
        <span>{{ t('admin.requestTrace.export.scope.traceId') }}</span>
        <input v-model.trim="filters.trace_id" data-testid="request-trace-export-id-filter" class="input w-full font-mono" maxlength="32" autocomplete="off" />
      </label>
      <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
        <span>{{ t('admin.requestTrace.export.scope.routeFamily') }}</span>
        <select v-model="filters.route_family" data-testid="request-trace-export-route-family" class="input w-full">
          <option value="">{{ t('admin.requestTrace.export.scope.any') }}</option>
          <option value="messages">{{ t('admin.requestTrace.export.scope.messages') }}</option>
          <option value="chat_completions">{{ t('admin.requestTrace.export.scope.chat_completions') }}</option>
          <option value="responses">{{ t('admin.requestTrace.export.scope.responses') }}</option>
        </select>
      </label>
      <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
        <span>{{ t('admin.requestTrace.export.scope.status') }}</span>
        <input v-model="filters.client_status" data-testid="request-trace-export-status" class="input w-full" type="number" min="0" max="599" />
      </label>
      <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
        <span>{{ t('admin.requestTrace.export.scope.usageLinked') }}</span>
        <select v-model="filters.usage_linked" data-testid="request-trace-export-usage-linked" class="input w-full">
          <option value="">{{ t('admin.requestTrace.export.scope.any') }}</option>
          <option value="true">{{ t('admin.requestTrace.export.scope.linked') }}</option>
          <option value="false">{{ t('admin.requestTrace.export.scope.unlinked') }}</option>
        </select>
      </label>
      <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
        <span>{{ t('admin.requestTrace.export.scope.from') }}</span>
        <input v-model="filters.created_from" data-testid="request-trace-export-from" class="input w-full" type="datetime-local" />
      </label>
      <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
        <span>{{ t('admin.requestTrace.export.scope.to') }}</span>
        <input v-model="filters.created_to" data-testid="request-trace-export-to" class="input w-full" type="datetime-local" />
      </label>
    </div>

    <p v-if="filterError" role="alert" class="mt-3 text-sm text-amber-700 dark:text-amber-300" data-testid="request-trace-export-filter-error">{{ t('admin.requestTrace.export.scope.invalidFilter') }}</p>
    <button type="button" class="btn btn-primary mt-3" data-testid="request-trace-export-create" :disabled="creating" @click="create">
      {{ creating ? t('admin.requestTrace.export.scope.creating') : t('admin.requestTrace.export.scope.create') }}
    </button>
    <p v-if="createError" role="alert" class="mt-3 text-sm text-red-700 dark:text-red-300" data-testid="request-trace-export-create-error">{{ t('admin.requestTrace.export.createFailed') }}</p>

    <div v-if="task" class="mt-4 space-y-3 border-t border-gray-200 pt-4 dark:border-dark-700" data-testid="request-trace-export-task" :data-state="displayState">
      <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-2 text-sm">
        <dt>{{ t('admin.requestTrace.export.progress.state') }}</dt>
        <dd data-testid="request-trace-export-state">{{ t(`admin.requestTrace.export.progress.${displayState}`) }}</dd>
        <dt>{{ t('admin.requestTrace.export.progress.created') }}</dt>
        <dd>{{ formatDate(task.created_at) }}</dd>
        <dt>{{ t('admin.requestTrace.export.progress.completedAt') }}</dt>
        <dd data-testid="request-trace-export-completed">{{ task.completed_at ? formatDate(task.completed_at) : t('admin.requestTrace.export.progress.notCompleted') }}</dd>
        <dt>{{ t('admin.requestTrace.export.progress.downloadUntil') }}</dt>
        <dd data-testid="request-trace-export-download-until">{{ task.download_until ? formatDate(task.download_until) : t('admin.requestTrace.export.progress.unknownDeadline') }}</dd>
        <dt>{{ t('admin.requestTrace.export.progress.exportedCount') }}</dt>
        <dd data-testid="request-trace-export-rows">{{ task.rows_exported }}</dd>
        <dt>{{ t('admin.requestTrace.export.progress.skippedCount') }}</dt>
        <dd data-testid="request-trace-export-skipped">{{ task.rows_skipped }}</dd>
        <dt>{{ t('admin.requestTrace.export.progress.bytes') }}</dt>
        <dd data-testid="request-trace-export-bytes">{{ task.bytes_exported }}</dd>
      </dl>

      <p v-if="displayState === 'failed'" class="text-sm text-red-700 dark:text-red-300" data-testid="request-trace-export-failure">{{ t('admin.requestTrace.export.progress.failedNote') }}</p>
      <p v-if="statusUnknown" role="status" class="text-sm text-amber-700 dark:text-amber-300" data-testid="request-trace-export-status-unknown">{{ t('admin.requestTrace.export.progress.statusUnknown') }}</p>
      <p v-if="displayState === 'file_lost'" class="text-sm text-amber-700 dark:text-amber-300" data-testid="request-trace-export-file-lost">{{ t('admin.requestTrace.export.progress.fileLost') }}</p>
      <p v-if="displayState === 'expired'" class="text-sm text-amber-700 dark:text-amber-300" data-testid="request-trace-export-expired">{{ t('admin.requestTrace.export.progress.expiredNote') }}</p>

      <button type="button" class="btn btn-secondary" data-testid="request-trace-export-download" :disabled="displayState !== 'completed' || downloading" @click="download">
        {{ downloading ? t('admin.requestTrace.export.download.downloading') : t('admin.requestTrace.export.download.action') }}
      </button>
      <p v-if="downloadError" role="alert" class="text-sm text-red-700 dark:text-red-300" data-testid="request-trace-export-download-error">{{ t('admin.requestTrace.export.download.failed') }}</p>
    </div>
    <p v-else class="mt-4 text-sm text-gray-500 dark:text-dark-300" data-testid="request-trace-export-empty">{{ t('admin.requestTrace.export.empty') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { createTraceExport, downloadTraceExport, getTraceExport } from './api'
import type { RequestTraceExportFilter, RequestTraceExportStatus, RequestTraceExportTask } from './types'

type ExportDisplayState = 'idle' | RequestTraceExportStatus | 'file_lost' | 'expired'

const POLL_INTERVAL_MS = 3000
const traceIDPattern = /^[0-9a-f]{32}$/
const routeFamilies = ['messages', 'chat_completions', 'responses'] as const

const { t } = useI18n()
const filters = reactive({ trace_id: '', route_family: '', client_status: '', usage_linked: '', created_from: '', created_to: '' })
const task = ref<RequestTraceExportTask | null>(null)
const creating = ref(false)
const downloading = ref(false)
const filterError = ref(false)
const createError = ref(false)
const downloadError = ref(false)
const statusUnknown = ref(false)
/** Set only from a refused download's bounded outcome; never inferred locally. */
const downloadRefusal = ref<'file_lost' | 'expired' | null>(null)

let pollTimer: ReturnType<typeof setInterval> | null = null
let pollController: AbortController | null = null
let createController: AbortController | null = null
let downloadController: AbortController | null = null
let revision = 0

function formatDate(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function isActive(status: RequestTraceExportStatus): boolean {
  return status === 'pending' || status === 'running'
}

function stopPolling() {
  if (pollTimer !== null) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

/** Metadata-only scope. No body, query or free-text search can be expressed here. */
function buildFilter(): RequestTraceExportFilter | null {
  const filter: RequestTraceExportFilter = {}
  if (filters.trace_id) {
    if (!traceIDPattern.test(filters.trace_id)) return null
    filter.trace_id = filters.trace_id
  }
  if (routeFamilies.includes(filters.route_family as (typeof routeFamilies)[number])) {
    filter.route_family = filters.route_family as RequestTraceExportFilter['route_family']
  }
  if (filters.client_status !== '') {
    const status = Number(filters.client_status)
    if (!Number.isSafeInteger(status) || status < 0 || status > 599) return null
    filter.client_status = status
  }
  if (filters.usage_linked !== '') filter.usage_linked = filters.usage_linked === 'true'
  if (filters.created_from) {
    const from = new Date(filters.created_from)
    if (Number.isNaN(from.getTime())) return null
    filter.created_from = from.toISOString()
  }
  if (filters.created_to) {
    const to = new Date(filters.created_to)
    if (Number.isNaN(to.getTime())) return null
    filter.created_to = to.toISOString()
  }
  return filter
}

const displayState = computed<ExportDisplayState>(() => {
  const current = task.value
  if (!current) return 'idle'
  if (current.status !== 'completed') return current.status
  if (downloadRefusal.value === 'expired' || !current.downloadable) return 'expired'
  if (downloadRefusal.value === 'file_lost') return 'file_lost'
  return 'completed'
})

function startPollingIfActive() {
  stopPolling()
  const current = task.value
  if (!current || !isActive(current.status)) return
  const id = current.id
  pollTimer = setInterval(() => { void poll(id) }, POLL_INTERVAL_MS)
}

async function poll(id: string) {
  const current = ++revision
  pollController?.abort()
  const controller = new AbortController()
  pollController = controller
  try {
    const next = await getTraceExport(id, { signal: controller.signal })
    if (current !== revision) return
    task.value = next
    statusUnknown.value = false
    if (!isActive(next.status)) stopPolling()
  } catch {
    // An unreadable task is unknown, not failed: keep polling and say so.
    if (current !== revision) return
    statusUnknown.value = true
  }
}

async function create() {
  if (creating.value) return
  filterError.value = false
  createError.value = false
  downloadError.value = false
  const filter = buildFilter()
  if (filter === null) {
    filterError.value = true
    return
  }
  creating.value = true
  stopPolling()
  pollController?.abort()
  createController?.abort()
  const controller = new AbortController()
  createController = controller
  // The same revision guard `poll()` uses: if the panel unmounts (or another
  // create starts) while the POST is in flight, this response must not adopt a
  // task or register a poller that nothing will ever clear.
  const current = ++revision
  try {
    const created = await createTraceExport(filter, { signal: controller.signal })
    if (current !== revision) return
    task.value = created
    downloadRefusal.value = null
    statusUnknown.value = false
    startPollingIfActive()
  } catch {
    if (current !== revision) return
    task.value = null
    createError.value = true
  } finally {
    if (createController === controller) createController = null
    creating.value = false
  }
}

function saveBlob(blob: Blob, fileName: string) {
  const url = URL.createObjectURL(blob)
  try {
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = fileName
    anchor.rel = 'noopener'
    document.body.appendChild(anchor)
    anchor.click()
    anchor.remove()
  } finally {
    URL.revokeObjectURL(url)
  }
}

async function download() {
  const current = task.value
  if (!current || downloading.value || displayState.value !== 'completed') return
  downloading.value = true
  downloadError.value = false
  downloadController?.abort()
  const controller = new AbortController()
  downloadController = controller
  try {
    const result = await downloadTraceExport(current.id, { signal: controller.signal })
    if (result.outcome === 'ready') {
      saveBlob(result.blob, result.fileName)
      return
    }
    if (result.outcome === 'unavailable') downloadError.value = true
    else downloadRefusal.value = result.outcome
  } catch {
    downloadError.value = true
  } finally {
    downloading.value = false
  }
}

onBeforeUnmount(() => {
  revision += 1
  stopPolling()
  pollController?.abort()
  createController?.abort()
  downloadController?.abort()
  // The task handle is deliberately kept in memory only; nothing is persisted.
  task.value = null
})
</script>
