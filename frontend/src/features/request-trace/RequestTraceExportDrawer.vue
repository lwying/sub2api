<template>
  <BaseDialog :show="show" :title="t('admin.requestTrace.export.task.title')" width="wide" @close="emit('update:show', false)">
    <div class="space-y-4 text-sm">
      <p class="text-xs text-gray-500 dark:text-dark-300" data-testid="request-trace-export-task-session-note">
        {{ t('admin.requestTrace.export.task.sessionOnly') }}
      </p>
      <p v-if="!taskId" class="text-gray-500 dark:text-dark-300" data-testid="request-trace-export-task-absent">
        {{ t('admin.requestTrace.export.task.absent') }}
      </p>
      <p v-else-if="loading && !task" role="status" class="py-6 text-center text-gray-500 dark:text-dark-300" data-testid="request-trace-export-task-loading">
        {{ t('admin.requestTrace.export.task.loading') }}
      </p>
      <template v-else-if="!task">
        <p v-if="failed" role="alert" class="rounded-lg border border-amber-200 bg-amber-50 p-3 text-amber-800 dark:border-amber-900 dark:bg-amber-950/20 dark:text-amber-200" data-testid="request-trace-export-task-unreadable">
          {{ t('admin.requestTrace.export.task.unreadable') }}
        </p>
        <p v-else role="status" class="py-6 text-center text-gray-500 dark:text-dark-300" data-testid="request-trace-export-task-loading">
          {{ t('admin.requestTrace.export.task.loading') }}
        </p>
        <button type="button" class="btn btn-secondary" data-testid="request-trace-export-task-retry" @click="load(taskId)">
          {{ t('admin.requestTrace.export.task.retry') }}
        </button>
      </template>
      <template v-else>
        <div data-testid="request-trace-export-task" :data-state="displayState">
          <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-2">
            <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.export.progress.state') }}</dt>
            <dd data-testid="request-trace-export-state">{{ t(stateKey(displayState)) }}</dd>
            <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.export.progress.created') }}</dt>
            <dd>{{ formatDate(task.created_at) }}</dd>
            <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.export.progress.completedAt') }}</dt>
            <dd data-testid="request-trace-export-completed">{{ task.completed_at ? formatDate(task.completed_at) : t('admin.requestTrace.export.progress.notCompleted') }}</dd>
            <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.export.progress.downloadUntil') }}</dt>
            <dd data-testid="request-trace-export-download-until">{{ task.download_until ? formatDate(task.download_until) : t('admin.requestTrace.export.progress.unknownDeadline') }}</dd>
            <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.export.progress.exportedCount') }}</dt>
            <dd data-testid="request-trace-export-rows">{{ task.rows_exported }}</dd>
            <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.export.progress.skippedCount') }}</dt>
            <dd data-testid="request-trace-export-skipped">{{ task.rows_skipped }}</dd>
            <!-- 跳过不是一个总数：管理员要能分辨"读取失败"与"记录消失"各多少条。
                 没有任何分类型计数时这一行不出现，聚合数仍在上一行。 -->
            <template v-if="skipBreakdown.length">
              <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.export.progress.skippedByReason') }}</dt>
              <dd data-testid="request-trace-export-skipped-by-reason">
                <ul class="space-y-1">
                  <li
                    v-for="entry in skipBreakdown"
                    :key="entry.reason"
                    :data-testid="`request-trace-export-skipped-reason-${entry.reason}`"
                    :data-reason="entry.reason"
                  >
                    {{ t(`admin.requestTrace.export.reason.${entry.reason}`) }}: {{ entry.count }}
                  </li>
                </ul>
              </dd>
            </template>
            <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.export.progress.bytes') }}</dt>
            <dd data-testid="request-trace-export-bytes">{{ task.bytes_exported }}</dd>
            <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.export.progress.shards') }}</dt>
            <dd data-testid="request-trace-export-shard-count">{{ task.shard_count }}</dd>
            <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.export.progress.scope') }}</dt>
            <dd class="break-all" data-testid="request-trace-export-task-scope">{{ scopeSummary }}</dd>
          </dl>

          <p v-if="statusUnknown" role="status" class="mt-3 text-amber-700 dark:text-amber-300" data-testid="request-trace-export-status-unknown">
            {{ t('admin.requestTrace.export.progress.statusUnknown') }}
          </p>
          <p v-if="displayState === 'failed'" class="mt-3 text-red-700 dark:text-red-300" data-testid="request-trace-export-failure">
            {{ t('admin.requestTrace.export.progress.failedNote') }}
          </p>
          <!--
            A truncated task is not a smaller success: it delivered the shards it
            wrote and the manifest says which part of the source it never saw.
          -->
          <p v-if="task.truncated && !manifestUnreadable" role="alert" class="mt-3 rounded-lg border border-amber-200 bg-amber-50 p-3 text-amber-800 dark:border-amber-900 dark:bg-amber-950/20 dark:text-amber-200" data-testid="request-trace-export-incomplete">
            {{ t('admin.requestTrace.export.progress.incompleteNote') }}
            <span class="font-mono" data-testid="request-trace-export-incomplete-reason">{{ reasonLabel }}</span>
          </p>
          <p v-if="!task.truncated" class="mt-3 text-xs text-gray-500 dark:text-dark-300" data-testid="request-trace-export-not-snapshot">
            {{ t('admin.requestTrace.export.progress.notSnapshot') }}
          </p>
          <!-- 只有"已完成但窗口已关"才谈得上过期。排队中/进行中/失败都不是过期，
               把它们说成"下载窗口已关闭"会让管理员以为任务白跑了。 -->
          <p v-if="displayState === 'expired'" class="mt-3 text-amber-700 dark:text-amber-300" data-testid="request-trace-export-expired">
            {{ t('admin.requestTrace.export.progress.expiredNote') }}
          </p>
          <!-- 清单已丢失：要么服务端读回任务时就这么说，要么一次下载被这样拒绝。
               两条路说的是同一件事，因此渲染同一句话。 -->
          <p v-if="manifestLost || manifestUnreadable" class="mt-3 text-amber-700 dark:text-amber-300" data-testid="request-trace-export-file-lost">
            {{ t('admin.requestTrace.export.progress.fileLost') }}
          </p>
          <p v-if="expectedShards.length && downloadable" class="mt-3 text-xs text-gray-500 dark:text-dark-300" data-testid="request-trace-export-shards-note">
            {{ t('admin.requestTrace.export.download.shardsNote', { count: task.shard_count }) }}
          </p>

          <div class="mt-4 flex flex-wrap gap-2 border-t border-gray-200 pt-4 dark:border-dark-700">
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              data-testid="request-trace-export-manifest"
              :disabled="!canDownload || downloading === 'manifest' || manifestLost"
              @click="downloadFile(null)"
            >
              {{ downloading === 'manifest' ? t('admin.requestTrace.export.download.downloading') : t('admin.requestTrace.export.download.manifest') }}
            </button>
            <button
              v-for="part in expectedShards"
              :key="part"
              type="button"
              class="btn btn-secondary btn-sm"
              :data-testid="`request-trace-export-shard-${part}`"
              :data-part="part"
              :disabled="!canDownload || downloading === `part:${part}` || shardLost(part)"
              @click="downloadFile(part)"
            >
              {{ downloading === `part:${part}` ? t('admin.requestTrace.export.download.downloading') : t('admin.requestTrace.export.download.shard', { part }) }}
            </button>
          </div>
          <p v-if="downloadError" role="alert" class="mt-2 text-red-700 dark:text-red-300" data-testid="request-trace-export-download-error">
            {{ t('admin.requestTrace.export.download.failed') }}
          </p>
          <p v-if="windowClosed" role="alert" class="mt-2 text-amber-700 dark:text-amber-300" data-testid="request-trace-export-file-expired">
            {{ t('admin.requestTrace.export.progress.expiredNote') }}
          </p>
          <p v-if="notReady" role="status" class="mt-2 text-amber-700 dark:text-amber-300" data-testid="request-trace-export-not-ready">
            {{ t('admin.requestTrace.export.download.notReady') }}
          </p>
          <ul v-if="lostFiles.length" class="mt-2 space-y-1 text-amber-700 dark:text-amber-300" data-testid="request-trace-export-lost-files">
            <li v-for="key in lostFiles" :key="key" :data-testid="`request-trace-export-lost-${key.replace(':', '-')}`">
              {{ t('admin.requestTrace.export.download.fileLostFor', { file: fileLabel(key) }) }}
            </li>
          </ul>
        </div>
      </template>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { downloadTraceExport, downloadTraceExportPart, getTraceExport } from './api'
import { exportScopeSummary } from './labels'
import {
  requestTraceExportDisplayStates,
  requestTraceExportIDPattern,
  requestTraceExportIncompleteReasons,
  requestTraceExportSkipReasons,
  type RequestTraceExportDisplayState,
  type RequestTraceExportTask,
} from './types'

const POLL_INTERVAL_MS = 3000
/** Refusal keys: the manifest, or one shard. */
type FileKey = 'manifest' | `part:${number}`

const props = defineProps<{ show: boolean; taskId: string | null }>()
const emit = defineEmits<{ (e: 'update:show', value: boolean): void }>()

const { t } = useI18n()
const task = ref<RequestTraceExportTask | null>(null)
const loading = ref(false)
const failed = ref(false)
const statusUnknown = ref(false)
const downloadError = ref(false)
const downloading = ref<FileKey | ''>('')
/** Only ever set from a refused download's bounded outcome; never inferred locally. */
const refusals = ref<Partial<Record<FileKey, 'file_lost' | 'expired' | 'not_ready'>>>({})

let pollTimer: ReturnType<typeof setInterval> | null = null
let pollController: AbortController | null = null
let downloadController: AbortController | null = null
let revision = 0

function formatDate(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function isActive(status: RequestTraceExportTask['status']): boolean {
  return status === 'pending' || status === 'running'
}

/**
 * The server's own code for "this task's manifest could not be read": the file
 * that lists the shards and states whether the delivery was whole is itself
 * gone. Nothing is known about what was delivered, so the row is neither
 * complete nor a smaller success, and the truncation sentence — which says which
 * part of the *source* was never seen — would be a different claim entirely.
 */
const manifestLostReason = 'manifest_lost'

/**
 * True when the server reported the manifest unreadable for this task. A server
 * that says so also says the task is not complete, and the download is refused,
 * so this is a verdict about the file, not a guess about it.
 */
const manifestUnreadable = computed(() =>
  task.value?.truncated === true && task.value?.incomplete_reason === manifestLostReason,
)

const displayState = computed<RequestTraceExportDisplayState>(() => {
  const current = task.value
  if (!current) return 'pending'
  if (current.status !== 'completed') return current.status
  // A delivery whose own record is gone has its own state: it is not "incomplete"
  // (nothing was observed to be missing) and it is not "done".
  if (manifestUnreadable.value) return 'file_lost'
  // Incompleteness is not a footnote on success: it is its own state.
  if (current.truncated) return 'incomplete'
  return current.downloadable ? 'completed' : 'expired'
})

const stateKeys = new Set<string>(requestTraceExportDisplayStates)

/** The one key a state renders through; a state outside the set has no label of its own. */
function stateKey(state: RequestTraceExportDisplayState): string {
  return `admin.requestTrace.export.state.${stateKeys.has(state) ? state : 'unknown'}`
}

const reasonKeys = new Set<string>(requestTraceExportIncompleteReasons)

/**
 * The server's reason code, rendered through its own label. An unrecognized code
 * is labelled generically rather than shown raw, and never turns the task back
 * into "complete".
 */
const reasonLabel = computed(() => {
  const code = task.value?.incomplete_reason ?? ''
  const known = reasonKeys.has(code) ? code : 'unknown'
  return t(`admin.requestTrace.export.reason.${known}`)
})

/**
 * The typed skip counts, in the closed set's own order so the breakdown reads the
 * same every time. Only a reason with a count is listed: an absent class is a
 * real zero and the aggregate row above already states the total, and the labels
 * are the same closed-set labels the task's own reason code renders through.
 */
const skipBreakdown = computed(() =>
  requestTraceExportSkipReasons
    .map(reason => ({ reason, count: task.value?.skipped_by_reason?.[reason] ?? 0 }))
    .filter(entry => entry.count > 0),
)

/** The shard indexes the task says it wrote, 1-based, exactly as the download expects them. */
const expectedShards = computed(() => {
  const count = task.value?.shard_count ?? 0
  return Array.from({ length: Math.max(0, count) }, (_, index) => index + 1)
})

/** Downloading is the server's verdict for this session right now, never a local clock. */
const downloadable = computed(() => {
  const current = task.value
  return current !== null && current.status === 'completed' && current.downloadable
})
const canDownload = computed(() => downloadable.value && !windowClosed.value)

const lostFiles = computed(() => (Object.keys(refusals.value) as FileKey[]).filter(key => refusals.value[key] === 'file_lost'))
const manifestLost = computed(() => refusals.value.manifest === 'file_lost')
/**
 * The server can answer the download window closed even while the task view this
 * session last read still said otherwise. That verdict is about the task, not one
 * file, so it stops every download here until the task is read again.
 */
const windowClosed = computed(() => Object.values(refusals.value).some(outcome => outcome === 'expired'))
/** 任务尚未产出文件：这是重试提示，不是过期或丢失。 */
const notReady = computed(() => Object.values(refusals.value).some(outcome => outcome === 'not_ready'))

function shardLost(part: number): boolean {
  return refusals.value[`part:${part}`] === 'file_lost'
}

function fileLabel(key: FileKey): string {
  return key === 'manifest'
    ? t('admin.requestTrace.export.download.manifest')
    : t('admin.requestTrace.export.download.shard', { part: Number(key.slice('part:'.length)) })
}

/** The task's own filter echo, stated in the query's own facet names. */
const scopeSummary = computed(() => {
  const filter = task.value?.filter
  return filter ? exportScopeSummary(t, filter) : t('admin.requestTrace.export.scope.none')
})

function stopPolling() {
  if (pollTimer !== null) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

function startPollingIfActive(id: string) {
  stopPolling()
  const current = task.value
  if (!current || !isActive(current.status)) return
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

async function load(id: string) {
  const current = ++revision
  stopPolling()
  pollController?.abort()
  const controller = new AbortController()
  pollController = controller
  loading.value = true
  failed.value = false
  statusUnknown.value = false
  downloadError.value = false
  refusals.value = {}
  task.value = null
  try {
    const next = await getTraceExport(id, { signal: controller.signal })
    if (current !== revision) return
    task.value = next
    startPollingIfActive(id)
  } catch {
    if (current !== revision) return
    failed.value = true
  } finally {
    if (current === revision) loading.value = false
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

async function downloadFile(part: number | null) {
  const current = task.value
  if (!current || !canDownload.value || downloading.value !== '') return
  const key: FileKey = part === null ? 'manifest' : `part:${part}`
  downloading.value = key
  downloadError.value = false
  downloadController?.abort()
  const controller = new AbortController()
  downloadController = controller
  try {
    const result = part === null
      ? await downloadTraceExport(current.id, { signal: controller.signal })
      : await downloadTraceExportPart(current.id, part, { signal: controller.signal })
    if (result.outcome === 'ready') {
      saveBlob(result.blob, result.fileName)
      return
    }
    if (result.outcome === 'unavailable') {
      downloadError.value = true
      return
    }
    // A refused file is a bounded verdict, and it is about that file only.
    refusals.value = { ...refusals.value, [key]: result.outcome }
  } catch {
    downloadError.value = true
  } finally {
    downloading.value = ''
  }
}

watch([() => props.show, () => props.taskId], ([show, id]) => {
  if (!show || !id || !requestTraceExportIDPattern.test(id)) {
    revision += 1
    stopPolling()
    pollController?.abort()
    return
  }
  void load(id)
}, { immediate: true })

onBeforeUnmount(() => {
  revision += 1
  stopPolling()
  pollController?.abort()
  downloadController?.abort()
  task.value = null
})
</script>
