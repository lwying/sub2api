<template>
  <AppLayout>
    <div class="mx-auto max-w-[1400px] pb-8">
      <header class="mb-6 flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 class="mt-1 text-2xl font-semibold tracking-tight text-gray-950 dark:text-white">
            {{ t('admin.errorDiagnostics.title') }}
          </h1>
          <p class="mt-2 max-w-3xl text-sm text-gray-500 dark:text-dark-300">
            {{ t('admin.errorDiagnostics.description') }}
          </p>
        </div>
        <button
          type="button"
          class="btn btn-secondary"
          data-testid="error-diagnostics-refresh"
          @click="refresh()"
        >
          {{ t('admin.errorDiagnostics.list.refresh') }}
        </button>
      </header>

      <!--
        The gate is the operator's control surface and the source of truth for
        whether anything below can exist at all. It is rendered before the list so
        "capture is off" is read first, not inferred from an empty table.
      -->
      <ErrorDiagnosticOperatorSettings
        class="mb-6"
        :status="operatorStatus"
        :loading="operatorLoading"
        @updated="onOperatorStatusUpdated"
      />

      <!--
        Capture can be off while records written before it was turned off are
        still inside their retention window, and the read endpoints are not gated
        by the capture switch. The paused notice therefore sits above the list
        rather than replacing it: nothing already stored is hidden, and an empty
        result is never presented as "nothing failed".
      -->
      <div
        v-if="capturePaused && !notEnabled"
        data-testid="error-diagnostics-capture-paused"
        role="status"
        class="mb-6 rounded-xl border border-amber-200 bg-amber-50 p-5 text-sm text-amber-800 dark:border-amber-900/60 dark:bg-amber-950/30 dark:text-amber-200"
      >
        <p>{{ t('admin.errorDiagnostics.list.capturePaused') }}</p>
        <p class="mt-2 text-xs">
          {{ t('admin.errorDiagnostics.list.captureOffNotice') }}
        </p>
      </div>

      <div
        v-if="notEnabled"
        data-testid="error-diagnostics-not-enabled"
        class="rounded-xl border border-gray-200 bg-gray-50 p-5 text-sm text-gray-600 dark:border-dark-700 dark:bg-dark-900 dark:text-dark-300"
      >
        <p>{{ t('admin.errorDiagnostics.list.notEnabled') }}</p>
        <!--
          An off gate is the reason nothing is listed, so an empty list must not be
          read as "no failure has ever happened".
        -->
        <p class="mt-2 text-xs text-gray-500 dark:text-dark-400">
          {{ t('admin.errorDiagnostics.list.captureOffNotice') }}
        </p>
      </div>

      <div
        v-else-if="loadError"
        data-testid="error-diagnostics-load-error"
        role="alert"
        class="rounded-xl border border-red-200 bg-red-50 p-5 text-sm text-red-700 dark:border-red-900 dark:bg-red-950/30 dark:text-red-300"
      >
        {{ t('admin.errorDiagnostics.list.loadFailed') }}
      </div>

      <div
        v-else-if="loading && !items.length"
        class="flex justify-center py-12"
        data-testid="error-diagnostics-loading"
      >
        <svg class="h-7 w-7 animate-spin text-primary-500" fill="none" viewBox="0 0 24 24">
          <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
          <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z" />
        </svg>
      </div>

      <div
        v-else-if="!items.length && !capturePaused"
        data-testid="error-diagnostics-empty"
        class="rounded-xl border border-gray-200 bg-gray-50 p-5 text-sm text-gray-600 dark:border-dark-700 dark:bg-dark-900 dark:text-dark-300"
      >
        {{ t('admin.errorDiagnostics.list.empty') }}
      </div>

      <template v-else-if="items.length">
        <!--
          Metadata only. The template renders an explicit allowlist of cells and
          never the row object, so even a payload that unexpectedly carried a
          body or a credential could not reach the DOM through this table.
        -->
        <div class="overflow-x-auto rounded-xl border border-gray-200 dark:border-dark-700">
          <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
            <thead class="bg-gray-50 dark:bg-dark-900">
              <tr>
                <th scope="col" class="px-3 py-2 text-left font-medium text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.list.createdAt') }}</th>
                <th scope="col" class="px-3 py-2 text-left font-medium text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.list.protocol') }}</th>
                <th scope="col" class="px-3 py-2 text-left font-medium text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.list.attempt') }}</th>
                <th scope="col" class="px-3 py-2 text-left font-medium text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.list.upstreamStatus') }}</th>
                <th scope="col" class="px-3 py-2 text-left font-medium text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.list.body') }}</th>
                <th scope="col" class="px-3 py-2 text-left font-medium text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.list.expiresAt') }}</th>
                <th scope="col" class="px-3 py-2 text-left font-medium text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.list.usage') }}</th>
                <th scope="col" class="px-3 py-2 text-right font-medium text-gray-500 dark:text-dark-400">{{ t('admin.errorDiagnostics.list.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-200 bg-white dark:divide-dark-700 dark:bg-dark-800">
              <tr v-for="item in items" :key="item.id" data-testid="error-diagnostic-row">
                <td class="whitespace-nowrap px-3 py-2 font-mono text-xs text-gray-900 dark:text-dark-100">{{ formatDateTime(item.created_at) }}</td>
                <td class="px-3 py-2 text-gray-900 dark:text-dark-100">{{ protocolLabel(t, item.protocol) }}</td>
                <td class="px-3 py-2 font-mono text-gray-900 dark:text-dark-100">{{ item.attempt_index }}</td>
                <td class="px-3 py-2 font-mono text-gray-900 dark:text-dark-100">{{ item.upstream_status }}</td>
                <td class="px-3 py-2">
                  <div class="text-gray-900 dark:text-dark-100">{{ bodyStateLabel(t, item.body_state) }}</div>
                  <div v-if="item.reason !== undefined" class="text-xs text-gray-500 dark:text-dark-400" data-testid="error-diagnostic-row-reason">
                    {{ bodyReasonLabel(t, item.reason) }}
                  </div>
                </td>
                <td class="whitespace-nowrap px-3 py-2 font-mono text-xs text-gray-900 dark:text-dark-100">{{ formatDateTime(item.body_expires_at ?? item.metadata_expires_at) }}</td>
                <td class="px-3 py-2 text-xs">
                  <!--
                    The usage record id is shown as plain text: there is no deep
                    link to a single usage log, and a link to the generic usage
                    page would be misleading. An absent id is stated, not faked.
                  -->
                  <span v-if="item.usage_log_id" data-testid="error-diagnostic-usage-id" class="font-mono text-gray-900 dark:text-dark-100">
                    #{{ item.usage_log_id }}
                  </span>
                  <span v-else data-testid="error-diagnostic-usage-absent" class="text-gray-500 dark:text-dark-400">
                    {{ t('admin.errorDiagnostics.list.usageAbsent') }}
                  </span>
                </td>
                <td class="whitespace-nowrap px-3 py-2 text-right">
                  <button
                    type="button"
                    data-testid="error-diagnostic-view"
                    class="btn btn-secondary btn-sm"
                    @click="openDetail(item.id)"
                  >
                    {{ t('admin.errorDiagnostics.list.view') }}
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <Pagination
          class="mt-4"
          :total="total"
          :page="page"
          :page-size="pageSize"
          @update:page="load"
          @update:page-size="changePageSize"
        />
      </template>

      <ErrorDiagnosticDetailDrawer
        :show="detailOpen"
        :diagnostic-id="selectedId"
        @update:show="detailOpen = $event"
      />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import ErrorDiagnosticDetailDrawer from './components/ErrorDiagnosticDetailDrawer.vue'
import ErrorDiagnosticOperatorSettings from './components/ErrorDiagnosticOperatorSettings.vue'
import { getOperatorSettings, listDiagnostics } from './api'
import { bodyReasonLabel, bodyStateLabel, formatDateTime, protocolLabel } from './labels'
import type { DiagnosticAttempt, ErrorDiagnosticOperatorStatus } from './types'

const { t } = useI18n()

const items = ref<DiagnosticAttempt[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const loading = ref(false)
const loadError = ref(false)
const notEnabled = ref(false)
const detailOpen = ref(false)
const selectedId = ref<string | null>(null)
const operatorStatus = ref<ErrorDiagnosticOperatorStatus | null>(null)
const operatorLoading = ref(false)

let revision = 0
/**
 * The gate settles independently of the list, so it gets its own counter: a list
 * load must not invalidate a gate read that is still in flight, and vice versa.
 */
let operatorRevision = 0

async function load(nextPage: number) {
  const currentRevision = ++revision
  page.value = nextPage
  loading.value = true
  loadError.value = false
  notEnabled.value = false

  try {
    const result = await listDiagnostics({ page: nextPage, page_size: pageSize.value })
    if (currentRevision !== revision) return
    items.value = result.items
    total.value = result.total
    page.value = result.page
    pageSize.value = result.page_size
    // A stale selection must not survive a reload.
    closeDetail()
  } catch (error) {
    if (currentRevision !== revision) return
    items.value = []
    total.value = 0
    closeDetail()
    // The capture switch defaults to off server-side and surfaces as a 404; that
    // is a disabled feature, not a failure, and must not look like an error.
    if (isStatus(error, 404)) {
      notEnabled.value = true
    } else {
      loadError.value = true
    }
  } finally {
    if (currentRevision === revision) {
      loading.value = false
    }
  }
}

function changePageSize(size: number) {
  pageSize.value = size
  void load(1)
}

function openDetail(id: string) {
  selectedId.value = id
  detailOpen.value = true
}

function closeDetail() {
  detailOpen.value = false
  selectedId.value = null
}

function isStatus(error: unknown, status: number): boolean {
  return typeof error === 'object' && error !== null && (error as { status?: unknown }).status === status
}

/**
 * Reads the capture gate. A failure leaves the status unknown rather than stubbing
 * a default: "could not be read" and "disabled" are different statements and the
 * server is the only one allowed to make the second.
 *
 * Only the newest read may apply. An older read (a first bootstrap that was still
 * in flight, or a refresh the operator overtook) would otherwise answer with a gate
 * state that has since been superseded and put it back on screen.
 */
async function loadOperatorStatus() {
  const currentRevision = ++operatorRevision
  operatorLoading.value = true
  try {
    const status = await getOperatorSettings()
    if (currentRevision !== operatorRevision) return
    operatorStatus.value = status
  } catch {
    if (currentRevision !== operatorRevision) return
    operatorStatus.value = null
  } finally {
    if (currentRevision === operatorRevision) {
      operatorLoading.value = false
    }
  }
}

/**
 * A gate that is known to be off. It stops new records being written, but it does
 * not delete the ones already stored and it does not gate reads, so the list is
 * still shown underneath a notice rather than being replaced by one.
 */
const capturePaused = computed(
  () => operatorStatus.value !== null && !operatorStatus.value.capture_allowed,
)

/**
 * Reads the gate, then the list.
 *
 * The list is always asked for: records written before capture was turned off stay
 * readable for the rest of their retention window, so skipping the request would
 * hide them behind a "not enabled" screen. The gate only decides whether the
 * paused notice explains the result.
 */
async function bootstrap() {
  await loadOperatorStatus()
  await load(1)
}

/**
 * Reloads the page: the gate first, because it explains what the list means, then
 * the current page. Re-reading the gate is also the only way to recover from an
 * earlier "could not be read" state.
 */
async function refresh() {
  await loadOperatorStatus()
  await load(page.value)
}

/** Applies a gate state the operator just produced; the server's answer is used as-is. */
async function onOperatorStatusUpdated(next: ErrorDiagnosticOperatorStatus) {
  // The PUT answer is newer than any read still in flight, so it claims the
  // revision: a read that answers afterwards is discarded rather than allowed to
  // put the gate state the operator already moved past back on screen. The gate is
  // settled by this answer, so it is also no longer loading.
  operatorRevision += 1
  operatorStatus.value = next
  operatorLoading.value = false
  // Both directions re-read the list: enabling may bring new records, disabling
  // leaves the stored ones readable and they must stay visible.
  await load(1)
}

onMounted(() => {
  void bootstrap()
})
</script>
