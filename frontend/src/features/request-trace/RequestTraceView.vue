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
          <!--
            The three request-time facts below are each queried as one concrete
            value **or** as "not observed", never both: a request whose fact was
            never determined is not equal to any value, so the other option would
            silently change the question.
          -->
          <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.group') }}</span>
            <select v-model="filters.group_mode" data-testid="request-trace-group-filter-mode" class="input w-full">
              <option value="">{{ t('admin.requestTrace.list.any') }}</option>
              <option value="id">{{ t('admin.requestTrace.list.specificValue') }}</option>
              <option value="unknown">{{ t('admin.requestTrace.list.unknownValue') }}</option>
            </select>
          </label>
          <label v-if="filters.group_mode === 'id'" class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.groupId') }}</span>
            <input v-model.trim="filters.group_id" data-testid="request-trace-group-filter" class="input w-full font-mono" inputmode="numeric" autocomplete="off" />
          </label>
          <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.requestedModel') }}</span>
            <select v-model="filters.model_mode" data-testid="request-trace-model-filter-mode" class="input w-full">
              <option value="">{{ t('admin.requestTrace.list.any') }}</option>
              <option value="value">{{ t('admin.requestTrace.list.specificValue') }}</option>
              <option value="unknown">{{ t('admin.requestTrace.list.unknownValue') }}</option>
            </select>
          </label>
          <label v-if="filters.model_mode === 'value'" class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.modelName') }}</span>
            <input v-model.trim="filters.requested_model" data-testid="request-trace-model-filter" class="input w-full font-mono" maxlength="128" autocomplete="off" />
          </label>
          <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.platform') }}</span>
            <select v-model="filters.platform_mode" data-testid="request-trace-platform-filter-mode" class="input w-full">
              <option value="">{{ t('admin.requestTrace.list.any') }}</option>
              <option value="value">{{ t('admin.requestTrace.list.specificValue') }}</option>
              <option value="unknown">{{ t('admin.requestTrace.list.unknownValue') }}</option>
            </select>
          </label>
          <label v-if="filters.platform_mode === 'value'" class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
            <span>{{ t('admin.requestTrace.list.platformName') }}</span>
            <input v-model.trim="filters.platform_name" data-testid="request-trace-platform-filter" class="input w-full font-mono" maxlength="128" autocomplete="off" />
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
        <div class="mt-3 flex flex-wrap items-center gap-3">
          <button type="button" class="btn btn-primary" data-testid="request-trace-search" @click="search">{{ t('admin.requestTrace.list.search') }}</button>
          <!--
            The export actions sit beside the query, because they export the
            query: there is no second filter form to fill in, and no way to
            export a scope that was never executed.
          -->
          <button
            type="button"
            class="btn btn-secondary"
            data-testid="request-trace-export-selected"
            :disabled="creating || selectionOverBound || selectedCount === 0 || exportBlocked"
            @click="exportSelected"
          >
            {{ creating ? t('admin.requestTrace.export.action.creating') : t('admin.requestTrace.export.action.selected', { count: selectedCount }) }}
          </button>
          <button
            type="button"
            class="btn btn-secondary"
            data-testid="request-trace-export-all"
            :disabled="creating || exportBlocked"
            @click="exportAll"
          >
            {{ creating ? t('admin.requestTrace.export.action.creating') : t('admin.requestTrace.export.action.all') }}
          </button>
          <span class="text-xs text-gray-500 dark:text-dark-300" data-testid="request-trace-export-selection-count">
            {{ t('admin.requestTrace.export.action.selectionCount', { count: selectedCount }) }}
          </span>
          <button
            v-if="selectedCount > 0"
            type="button"
            class="btn btn-secondary btn-sm"
            data-testid="request-trace-export-clear-selection"
            @click="clearSelection"
          >
            {{ t('admin.requestTrace.export.action.clearSelection') }}
          </button>
        </div>
        <!--
          The scope line states the filter set the export will carry: the last one
          that actually ran. Edits made in the form since then are not a scope
          until a query succeeds, and the note below says so out loud.
        -->
        <p class="mt-3 text-xs text-gray-600 dark:text-dark-300" data-testid="request-trace-export-scope">
          {{ t('admin.requestTrace.export.scope.heading') }}: {{ executedScopeSummary }}
        </p>
        <p v-if="draftDiffers" class="mt-1 text-xs text-amber-700 dark:text-amber-300" data-testid="request-trace-export-draft-note">
          {{ t('admin.requestTrace.export.scope.draftPending') }}
        </p>
        <p v-if="selectionOverBound" class="mt-1 text-xs text-amber-700 dark:text-amber-300" data-testid="request-trace-export-selection-over-bound">
          {{ t('admin.requestTrace.export.action.overBound', { count: selectedCount, max: maxSelectedTraces }) }}
        </p>
        <p v-if="riskAcknowledged === false" class="mt-1 text-xs text-amber-700 dark:text-amber-300" data-testid="request-trace-export-risk-note">
          {{ t('admin.requestTrace.export.action.riskRequired') }}
          <RouterLink :to="settingsPath" class="underline" data-testid="request-trace-export-risk-link">{{ t('admin.requestTrace.export.action.riskLink') }}</RouterLink>
          <button type="button" class="ml-2 underline" data-testid="request-trace-export-risk-recheck" @click="loadExportRisk">{{ t('admin.requestTrace.export.action.riskRecheck') }}</button>
        </p>
        <p v-if="refusal" role="alert" class="mt-1 text-xs text-red-700 dark:text-red-300" data-testid="request-trace-export-refusal">
          {{ t(`admin.requestTrace.export.refusal.${refusal}`) }}
        </p>
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
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.select') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.createdAt') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.traceId') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.route') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.group') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.requestedModel') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.status') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.state') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.cleanup') }}</th>
                <th class="px-3 py-3">{{ t('admin.requestTrace.list.usage') }}</th>
                <th class="px-3 py-3 text-right">{{ t('admin.requestTrace.list.view') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-200 bg-white dark:divide-dark-700 dark:bg-dark-800">
              <tr v-for="row in rows" :key="row.trace_id" data-testid="request-trace-row">
                <!--
                  Selection is by Trace ID, not by row position, so a checked row
                  stays checked when the same query is paged through.
                -->
                <td class="px-3 py-3">
                  <input
                    type="checkbox"
                    :checked="isSelected(row.trace_id)"
                    :data-testid="`request-trace-select-${row.trace_id}`"
                    :aria-label="t('admin.requestTrace.list.select')"
                    @change="toggleSelected(row.trace_id)"
                  />
                </td>
                <td class="whitespace-nowrap px-3 py-3 text-xs">{{ formatDate(row.created_at) }}</td>
                <td class="px-3 py-3 font-mono text-xs">{{ row.trace_id }}</td>
                <td class="px-3 py-3 text-xs">{{ row.inbound_endpoint }}</td>
                <!-- Request-time facts. Absent means not observed; it is never a blank cell or a guess. -->
                <td class="px-3 py-3 font-mono text-xs" data-testid="request-trace-row-group">
                  {{ row.group_id == null ? t('admin.requestTrace.list.unknownValue') : `#${row.group_id}` }}
                </td>
                <td class="px-3 py-3 font-mono text-xs" data-testid="request-trace-row-model">
                  {{ row.requested_model ? row.requested_model : t('admin.requestTrace.list.unknownValue') }}
                </td>
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
      <RequestTraceExportDrawer :show="exportOpen" :task-id="exportID" @update:show="onExportVisibility" />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import RequestTraceDetailDrawer from './RequestTraceDetailDrawer.vue'
import RequestTraceExportDrawer from './RequestTraceExportDrawer.vue'
import RequestTraceOpsStatusPanel from './RequestTraceOpsStatusPanel.vue'
import { TraceExportRefusedError, createTraceExport, getTraceExportRisk, listTraces } from './api'
import { captureStateLabel, exportScopeSummary } from './labels'
import {
  requestTraceExportIDPattern,
  requestTraceExportMaxSelectedTraces,
  type RequestTraceExportFilter,
  type RequestTraceExportRefusal,
  type RequestTraceListParams,
  type RequestTraceSummary,
} from './types'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const rows = ref<RequestTraceSummary[]>([])
const page = ref(1)
const pageSize = ref(20)
const total = ref(0)
const failed = ref(false)
const filterError = ref(false)
const loading = ref(false)
const detailOpen = ref(false)
const selectedID = ref<string | null>(null)
const filters = reactive({
  trace_id: '', route_family: '', client_status: '', usage_linked: '', account_id: '', usage_log_id: '',
  // Each request-time fact is chosen as "any" (no condition), one concrete value,
  // or "not observed". One mode per fact keeps the two mutually exclusive
  // conditions the server refuses to be selected at the same time.
  group_mode: '', group_id: '',
  model_mode: '', requested_model: '',
  platform_mode: '', platform_name: '',
  created_from: '', created_to: '',
})
/**
 * `executedFilters` is the set that actually ran and produced what is on screen:
 * it is what an export carries, and what the scope line shows. `queryFilters` is
 * the set the table is currently asking for, which only becomes executed once a
 * load succeeds — a form edit, or a query that failed, is not a scope.
 */
const executedFilters = ref<Partial<RequestTraceListParams>>({})
let queryFilters: Partial<RequestTraceListParams> = {}
/** A newly searched query drops the old cross-page selection, but only once it succeeds. */
let pendingSelectionReset = false
let revision = 0
let controller: AbortController | null = null
let createController: AbortController | null = null

const settingsPath = '/admin/settings'
const maxSelectedTraces = requestTraceExportMaxSelectedTraces
/** Ordered by selection, so the exported set is what the operator checked, in order. */
const selectedIDs = ref<string[]>([])
const selectedCount = computed(() => selectedIDs.value.length)
const selectionOverBound = computed(() => selectedCount.value > maxSelectedTraces)
const exportOpen = ref(false)
const exportID = ref<string | null>(null)
const creating = ref(false)
const refusal = ref<RequestTraceExportRefusal | null>(null)
/**
 * `null` means the risk state could not be read. An unreadable state is not
 * "unacknowledged": it blocks nothing and claims nothing.
 */
const riskAcknowledged = ref<boolean | null>(null)
const executedScopeSummary = computed(() => exportScopeSummary(t, executedFilters.value as RequestTraceExportFilter))
const exportBlocked = computed(() => riskAcknowledged.value === false)

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
  if (filters.group_mode === 'id') {
    const id = parseLookupID(filters.group_id)
    if (id === null) { filterError.value = true; return }
    next.group_id = id
  } else if (filters.group_mode === 'unknown') {
    next.group_unknown = true
  }
  // A chosen concrete value that is blank is an incomplete filter, not "no
  // filter": sending nothing here would quietly answer a different question.
  if (filters.model_mode === 'value') {
    if (filters.requested_model.trim() === '') { filterError.value = true; return }
    next.requested_model = filters.requested_model.trim()
  } else if (filters.model_mode === 'unknown') {
    next.model_unknown = true
  }
  if (filters.platform_mode === 'value') {
    if (filters.platform_name.trim() === '') { filterError.value = true; return }
    next.platform = filters.platform_name.trim()
  } else if (filters.platform_mode === 'unknown') {
    next.platform_unknown = true
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
  queryFilters = next
  // The checked rows belong to the query that found them: a new one clears them,
  // but only after it has actually run, so a failed search does not lose them.
  pendingSelectionReset = true
  void load(1)
}

function changePageSize(size: number) {
  pageSize.value = size
  void load(1)
}

/**
 * Whether the form holds edits the executed query does not: those edits are not
 * a scope yet, and the export does not carry them.
 */
const draftDiffers = computed(() => JSON.stringify(sortKeys(filtersToQuery())) !== JSON.stringify(sortKeys(executedFilters.value)))

function sortKeys(filter: Partial<RequestTraceListParams>): [string, unknown][] {
  return Object.entries(filter).sort(([left], [right]) => left.localeCompare(right))
}

/** The draft form as the filter set the query button would submit, without validating it. */
function filtersToQuery(): Partial<RequestTraceListParams> {
  const next: Partial<RequestTraceListParams> = {}
  if (filters.trace_id) next.trace_id = filters.trace_id
  if (filters.route_family) next.route_family = filters.route_family as RequestTraceListParams['route_family']
  if (filters.client_status !== '') {
    const status = Number(filters.client_status)
    if (Number.isSafeInteger(status)) next.client_status = status
  }
  if (filters.usage_linked !== '') next.usage_linked = filters.usage_linked === 'true'
  for (const [raw, key] of [[filters.usage_log_id, 'usage_log_id'], [filters.account_id, 'account_id']] as const) {
    if (raw === '') continue
    const id = parseLookupID(raw)
    if (id !== null) next[key] = id
  }
  if (filters.group_mode === 'id') {
    const id = parseLookupID(filters.group_id)
    if (id !== null) next.group_id = id
  } else if (filters.group_mode === 'unknown') {
    next.group_unknown = true
  }
  if (filters.model_mode === 'value') {
    if (filters.requested_model.trim() !== '') next.requested_model = filters.requested_model.trim()
  } else if (filters.model_mode === 'unknown') {
    next.model_unknown = true
  }
  if (filters.platform_mode === 'value') {
    if (filters.platform_name.trim() !== '') next.platform = filters.platform_name.trim()
  } else if (filters.platform_mode === 'unknown') {
    next.platform_unknown = true
  }
  if (filters.created_from) {
    const from = new Date(filters.created_from)
    if (!Number.isNaN(from.getTime())) next.created_from = from.toISOString()
  }
  if (filters.created_to) {
    const to = new Date(filters.created_to)
    if (!Number.isNaN(to.getTime())) next.created_to = to.toISOString()
  }
  return next
}

function isSelected(traceID: string): boolean {
  return selectedIDs.value.includes(traceID)
}

/**
 * Checking is bounded by what the server will accept: past the bound the export
 * action refuses and says so rather than sending a trimmed set.
 */
function toggleSelected(traceID: string) {
  refusal.value = null
  const current = selectedIDs.value
  if (current.includes(traceID)) {
    selectedIDs.value = current.filter(id => id !== traceID)
    return
  }
  selectedIDs.value = [...current, traceID]
}

function clearSelection() {
  selectedIDs.value = []
  refusal.value = null
}

/** Reads whether this deployment has accepted the export risk statement. */
async function loadExportRisk() {
  try {
    const risk = await getTraceExportRisk()
    riskAcknowledged.value = risk.acknowledged
  } catch {
    riskAcknowledged.value = null
  }
}

function exportSelected() {
  if (creating.value || exportBlocked.value || selectedCount.value === 0 || selectionOverBound.value) return
  void startExport({ trace_ids: [...selectedIDs.value] })
}

/** "Everything the current query matched" ignores the checked rows entirely. */
function exportAll() {
  if (creating.value || exportBlocked.value) return
  void startExport({ ...executedFilters.value })
}

async function startExport(filter: RequestTraceExportFilter) {
  if (riskAcknowledged.value === false) {
    refusal.value = 'risk_ack_required'
    return
  }
  creating.value = true
  refusal.value = null
  createController?.abort()
  const next = new AbortController()
  createController = next
  try {
    const created = await createTraceExport(filter, { signal: next.signal })
    await openExportTask(created.id)
  } catch (error) {
    // Only a bounded refusal is rendered; anything else stays "unavailable".
    refusal.value = error instanceof TraceExportRefusedError ? error.refusal : 'unavailable'
    // The risk verdict may be stale (another tab acknowledged it), so re-read it
    // instead of blocking the next attempt on a guess.
    void loadExportRisk()
  } finally {
    createController = null
    creating.value = false
  }
}

/**
 * The task handle lives in the URL, so a refresh in the same admin session finds
 * the task again. It is only a handle: the task itself is read from the server,
 * which decides whether this session may see it at all.
 */
async function openExportTask(id: string) {
  exportID.value = id
  exportOpen.value = true
  await router.replace({ query: { ...route.query, export: id } }).catch(() => undefined)
}

function onExportVisibility(value: boolean) {
  exportOpen.value = value
  if (value) return
  exportID.value = null
  refusal.value = null
  void router.replace({ query: { ...route.query, export: undefined } }).catch(() => undefined)
}

/** A task id can only come back from the URL as a task handle. */
function routePrefillExport(): string | null {
  const id = routeQueryString(route.query.export)
  return requestTraceExportIDPattern.test(id) ? id : null
}

async function load(nextPage: number) {
  const current = ++revision
  controller?.abort()
  const requestController = new AbortController()
  controller = requestController
  loading.value = true
  failed.value = false
  try {
    const result = await listTraces({ page: nextPage, page_size: pageSize.value, ...queryFilters }, { signal: requestController.signal })
    if (current !== revision) return
    rows.value = result.items
    total.value = result.total
    page.value = result.page
    pageSize.value = result.page_size
    // Only a query that ran is a scope, and only then does it clear the old
    // selection: a failed search leaves both the rows and the checks alone.
    executedFilters.value = { ...queryFilters }
    if (pendingSelectionReset) {
      pendingSelectionReset = false
      selectedIDs.value = []
    }
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
  queryFilters = routePrefillFilters()
  const taskID = routePrefillExport()
  if (taskID) {
    exportID.value = taskID
    exportOpen.value = true
  }
  void loadExportRisk()
  void load(1)
})
onBeforeUnmount(() => {
  revision += 1
  controller?.abort()
  createController?.abort()
  onDetailVisibility(false)
})
</script>
