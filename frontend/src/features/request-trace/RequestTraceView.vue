<template>
  <AppLayout>
    <div class="mx-auto max-w-[1400px] space-y-5 pb-8">
      <header class="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 class="text-2xl font-semibold text-gray-950 dark:text-white">{{ t('admin.requestTrace.title') }}</h1>
          <p class="mt-2 text-sm text-gray-500 dark:text-dark-300">{{ t('admin.requestTrace.description') }}</p>
        </div>
        <button type="button" class="btn btn-secondary" data-testid="request-trace-refresh" @click="load(page)">{{ t('admin.requestTrace.list.refresh') }}</button>
      </header>
      <div class="rounded-xl border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800">
        <div class="grid gap-3 md:grid-cols-2 xl:grid-cols-4">
          <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.traceId') }}</span>
            <input v-model.trim="filters.trace_id" data-testid="request-trace-id-filter" class="input w-full font-mono" maxlength="32" autocomplete="off" />
          </label>
          <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.routeFamily') }}</span>
            <select v-model="filters.route_family" class="input w-full">
              <option value="">{{ t('admin.requestTrace.list.any') }}</option>
              <option value="messages">{{ t('admin.requestTrace.list.messages') }}</option>
              <option value="chat_completions">{{ t('admin.requestTrace.list.chat_completions') }}</option>
              <option value="responses">{{ t('admin.requestTrace.list.responses') }}</option>
            </select>
          </label>
          <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.status') }}</span>
            <input v-model="filters.client_status" class="input w-full" type="number" min="0" max="599" />
          </label>
          <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.usageLinked') }}</span>
            <select v-model="filters.usage_linked" class="input w-full">
              <option value="">{{ t('admin.requestTrace.list.any') }}</option>
              <option value="true">{{ t('admin.requestTrace.list.linked') }}</option>
              <option value="false">{{ t('admin.requestTrace.list.unlinked') }}</option>
            </select>
          </label>
          <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.usageLogId') }}</span>
            <input v-model.trim="filters.usage_log_id" data-testid="request-trace-usage-filter" class="input w-full font-mono" inputmode="numeric" autocomplete="off" />
          </label>
          <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.accountId') }}</span>
            <input v-model.trim="filters.account_id" data-testid="request-trace-account-filter" class="input w-full font-mono" inputmode="numeric" autocomplete="off" />
          </label>
          <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.from') }}</span>
            <input v-model="filters.created_from" class="input w-full" type="datetime-local" />
          </label>
          <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.to') }}</span>
            <input v-model="filters.created_to" class="input w-full" type="datetime-local" />
          </label>
        </div>
        <button type="button" class="btn btn-primary mt-3" data-testid="request-trace-search" @click="search">{{ t('admin.requestTrace.list.search') }}</button>
      </div>
      <p v-if="filterError" role="alert" class="rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-700 dark:border-amber-900 dark:bg-amber-950/20" data-testid="request-trace-filter-error">{{ t('admin.requestTrace.list.invalidFilter') }}</p>
      <p v-if="failed" role="alert" class="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700 dark:border-red-900 dark:bg-red-950/20" data-testid="request-trace-error">{{ t('admin.requestTrace.list.failed') }}</p>
      <p v-else-if="loading && !rows.length" role="status" class="py-8 text-center text-sm" data-testid="request-trace-loading">{{ t('admin.requestTrace.list.loading') }}</p>
      <p v-else-if="!rows.length" role="status" class="rounded-lg border border-gray-200 bg-gray-50 p-4 text-sm dark:border-dark-700 dark:bg-dark-900" data-testid="request-trace-empty">{{ t('admin.requestTrace.list.empty') }} {{ t('admin.requestTrace.list.captureOff') }}</p>
      <template v-else>
        <div class="overflow-x-auto rounded-xl border border-gray-200 dark:border-dark-700">
          <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
            <thead class="bg-gray-50 text-left text-xs text-gray-500 dark:bg-dark-900">
              <tr>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.createdAt') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.traceId') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.route') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.status') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.state') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.cleanup') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.usage') }}</th>
                <th class="px-3 py-3 text-right">{{ t('admin.requestTrace.list.view') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-200 bg-white dark:divide-dark-700 dark:bg-dark-800">
              <tr v-for="row in rows" :key="row.trace_id" data-testid="request-trace-row">
                <td class="whitespace-nowrap px-3 py-3 text-xs">{{ formatDate(row.created_at) }}</td>
                <td class="px-3 py-3 font-mono text-xs">{{ row.trace_id }}</td>
                <td class="px-3 py-3 text-xs">{{ row.inbound_endpoint }}</td>
                <td class="px-3 py-3 font-mono">{{ row.client_status || '—' }}</td>
                <td class="px-3 py-3 text-xs" data-testid="request-trace-capture-state">{{ captureStateLabel(t, row.capture_state) }}</td>
                <td class="max-w-xs px-3 py-3 text-xs" data-testid="request-trace-cleanup-rule">
                  {{ row.usage_log_id ? t('admin.requestTrace.list.followsUsage') : row.cleanup_after ? t('admin.requestTrace.list.plannedCleanup', { date: formatDate(row.cleanup_after) }) : '—' }}
                </td>
                <td class="px-3 py-3 font-mono text-xs">{{ row.usage_log_id ? `#${row.usage_log_id}` : t('admin.requestTrace.list.usageAbsent') }}</td>
                <td class="px-3 py-3 text-right"><button type="button" class="btn btn-secondary btn-sm" data-testid="request-trace-view" @click="openDetail(row.trace_id)">{{ t('admin.requestTrace.list.view') }}</button></td>
              </tr>
            </tbody>
          </table>
        </div>
        <Pagination :total="total" :page="page" :page-size="pageSize" @update:page="load" @update:page-size="changePageSize" />
      </template>
      <RequestTraceDetailDrawer :show="detailOpen" :trace-id="selectedID" @update:show="onDetailVisibility" />
      <RequestTraceOpsStatusPanel />
      <RequestTraceExportPanel />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import RequestTraceDetailDrawer from './RequestTraceDetailDrawer.vue'
import RequestTraceExportPanel from './RequestTraceExportPanel.vue'
import RequestTraceOpsStatusPanel from './RequestTraceOpsStatusPanel.vue'
import { listTraces } from './api'
import { captureStateLabel } from './labels'
import type { RequestTraceListParams, RequestTraceSummary } from './types'

const { t } = useI18n()
const route = useRoute()
const rows = ref<RequestTraceSummary[]>([])
const page = ref(1)
const pageSize = ref(20)
const total = ref(0)
const failed = ref(false)
const filterError = ref(false)
const loading = ref(false)
const detailOpen = ref(false)
const selectedID = ref<string | null>(null)
const filters = reactive({ trace_id: '', route_family: '', client_status: '', usage_linked: '', account_id: '', usage_log_id: '', created_from: '', created_to: '' })
let activeFilters: Partial<RequestTraceListParams> = {}
let revision = 0
let controller: AbortController | null = null

/** A lookup id is only a filter when it is a positive integer; anything else is not a filter. */
function parseLookupID(raw: string): number | null {
  if (!/^[0-9]+$/.test(raw)) return null
  const value = Number(raw)
  return Number.isSafeInteger(value) && value > 0 ? value : null
}

function routeQueryString(value: unknown): string {
  return typeof value === 'string' ? value.trim() : ''
}

/**
 * A usage row can jump here with `?usage_log_id=<id>`. Applying that lookup on
 * load keeps the navigation meaningful: without it the page would load every
 * Trace and the operator would think the row had been located. Only a positive
 * integer is accepted, so a hand-edited URL cannot turn the filter into "all".
 */
function routePrefillFilters(): Partial<RequestTraceListParams> {
  const next: Partial<RequestTraceListParams> = {}
  const traceID = routeQueryString(route.query.trace_id)
  if (/^[0-9a-f]{32}$/.test(traceID)) {
    filters.trace_id = traceID
    next.trace_id = traceID
  }
  const usageLogID = parseLookupID(routeQueryString(route.query.usage_log_id))
  if (usageLogID !== null) {
    filters.usage_log_id = String(usageLogID)
    next.usage_log_id = usageLogID
  }
  const accountID = parseLookupID(routeQueryString(route.query.account_id))
  if (accountID !== null) {
    filters.account_id = String(accountID)
    next.account_id = accountID
  }
  return next
}

function formatDate(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function onDetailVisibility(value: boolean) {
  detailOpen.value = value
  if (!value) selectedID.value = null
}

function openDetail(id: string) {
  selectedID.value = id
  detailOpen.value = true
}

function search() {
  filterError.value = false
  const next: Partial<RequestTraceListParams> = {}
  if (filters.trace_id) {
    if (!/^[0-9a-f]{32}$/.test(filters.trace_id)) { filterError.value = true; return }
    next.trace_id = filters.trace_id
  }
  if (filters.route_family === 'messages' || filters.route_family === 'chat_completions' || filters.route_family === 'responses') next.route_family = filters.route_family
  if (filters.client_status !== '') {
    const status = Number(filters.client_status)
    if (!Number.isSafeInteger(status) || status < 0 || status > 599) { filterError.value = true; return }
    next.client_status = status
  }
  if (filters.usage_linked !== '') next.usage_linked = filters.usage_linked === 'true'
  for (const [raw, key] of [[filters.usage_log_id, 'usage_log_id'], [filters.account_id, 'account_id']] as const) {
    if (raw === '') continue
    const id = parseLookupID(raw)
    if (id === null) { filterError.value = true; return }
    next[key] = id
  }
  if (filters.created_from) {
    const from = new Date(filters.created_from)
    if (Number.isNaN(from.getTime())) { filterError.value = true; return }
    next.created_from = from.toISOString()
  }
  if (filters.created_to) {
    const to = new Date(filters.created_to)
    if (Number.isNaN(to.getTime())) { filterError.value = true; return }
    next.created_to = to.toISOString()
  }
  activeFilters = next
  void load(1)
}

function changePageSize(size: number) {
  pageSize.value = size
  void load(1)
}

async function load(nextPage: number) {
  const current = ++revision
  controller?.abort()
  const requestController = new AbortController()
  controller = requestController
  loading.value = true
  failed.value = false
  try {
    const result = await listTraces({ page: nextPage, page_size: pageSize.value, ...activeFilters }, { signal: requestController.signal })
    if (current !== revision) return
    rows.value = result.items
    total.value = result.total
    page.value = result.page
    pageSize.value = result.page_size
    onDetailVisibility(false)
  } catch {
    if (current !== revision) return
    rows.value = []
    total.value = 0
    onDetailVisibility(false)
    failed.value = true
  } finally {
    if (current === revision) loading.value = false
  }
}

onMounted(() => {
  activeFilters = routePrefillFilters()
  void load(1)
})
onBeforeUnmount(() => {
  revision += 1
  controller?.abort()
  onDetailVisibility(false)
})
</script>
