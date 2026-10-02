<template>
  <section
    :aria-labelledby="`${panelUid}-title`"
    class="border-t border-gray-200 pt-6 dark:border-dark-700"
    :aria-busy="loading ? 'true' : 'false'"
    data-testid="gateway-mock-events"
  >
    <div class="flex flex-wrap items-start justify-between gap-3">
      <div>
        <h2
          :id="`${panelUid}-title`"
          class="text-base font-semibold text-gray-950 dark:text-white"
        >
          {{ t("admin.gatewayMock.events.title") }}
        </h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-dark-300">
          {{ t("admin.gatewayMock.events.description") }}
        </p>
      </div>
      <button
        type="button"
        class="btn btn-secondary btn-sm"
        :disabled="loading"
        data-testid="gateway-mock-events-refresh"
        @click="load(page)"
      >
        <Icon
          name="refresh"
          size="sm"
          class="mr-1"
          :class="loading ? 'animate-spin' : ''"
        />
        {{
          loading
            ? t("admin.gatewayMock.events.refreshing")
            : t("admin.gatewayMock.events.refresh")
        }}
      </button>
    </div>

    <template v-if="rows.length">
      <div
        class="mt-4 overflow-hidden rounded-xl border border-gray-200 dark:border-dark-700/60"
      >
        <div class="overflow-x-auto">
          <!-- 桌面为 6 列密集表；窄屏由下方 media query 折叠为纵向记录，单一 DOM。 -->
          <table
            class="gateway-mock-events-table w-full min-w-full text-left text-sm"
          >
            <thead
              class="bg-gray-50 text-xs text-gray-500 dark:bg-dark-900/70 dark:text-dark-400"
            >
              <tr class="border-b border-gray-200 dark:border-dark-700">
                <th class="px-3 py-2 font-medium">
                  {{ t("admin.gatewayMock.events.columns.occurredAt") }}
                </th>
                <th class="px-3 py-2 font-medium">
                  {{ t("admin.gatewayMock.events.columns.rule") }}
                </th>
                <th class="px-3 py-2 font-medium">
                  {{ t("admin.gatewayMock.events.columns.protocol") }} /
                  {{ t("admin.gatewayMock.events.columns.model") }}
                </th>
                <th class="px-3 py-2 font-medium">
                  {{ t("admin.gatewayMock.events.columns.requestSource") }}
                </th>
                <th class="px-3 py-2 font-medium">
                  {{ t("admin.gatewayMock.events.columns.contentAudit") }}
                </th>
                <th class="px-3 py-2 text-right font-medium">
                  {{ t("admin.gatewayMock.events.columns.details") }}
                </th>
              </tr>
            </thead>
            <tbody class="bg-white dark:bg-transparent">
              <template v-for="(row, index) in rows" :key="index">
                <tr
                  :class="
                    isExpanded(index)
                      ? ''
                      : 'border-b border-gray-200 dark:border-dark-700'
                  "
                  data-testid="gateway-mock-events-row"
                >
                  <td
                    class="whitespace-nowrap px-3 py-2 text-xs text-gray-600 dark:text-dark-300"
                    :data-label="
                      t('admin.gatewayMock.events.columns.occurredAt')
                    "
                    data-testid="gateway-mock-events-time"
                  >
                    {{ formatTime(row.occurred_at) }}
                  </td>
                  <td
                    class="px-3 py-2 font-mono text-xs"
                    :data-label="t('admin.gatewayMock.events.columns.rule')"
                    data-testid="gateway-mock-events-rule"
                  >
                    <span
                      class="break-all text-gray-900 dark:text-dark-100"
                      data-testid="gateway-mock-events-rule-id"
                      >{{ row.rule_id }}</span
                    >
                  </td>
                  <td
                    class="px-3 py-2 text-xs"
                    :data-label="`${t(
                      'admin.gatewayMock.events.columns.protocol',
                    )} / ${t('admin.gatewayMock.events.columns.model')}`"
                    data-testid="gateway-mock-events-protocol-model"
                  >
                    <div class="min-w-0">
                      <span
                        class="text-gray-900 dark:text-dark-100"
                        data-testid="gateway-mock-events-protocol"
                        >{{ t(gatewayMockProtocolKey(row.protocol)) }}</span
                      >
                      <span
                        class="mt-0.5 block break-words font-mono text-gray-500 dark:text-dark-300"
                        data-testid="gateway-mock-events-model"
                        >{{ observed(row.model) }}</span
                      >
                    </div>
                  </td>
                  <td
                    class="px-3 py-2 text-xs"
                    :data-label="
                      t('admin.gatewayMock.events.columns.requestSource')
                    "
                    data-testid="gateway-mock-events-source"
                  >
                    <dl class="flex flex-col gap-0.5">
                      <div class="flex items-center gap-1.5">
                        <dt class="text-gray-500 dark:text-dark-400">
                          {{ t("admin.gatewayMock.events.columns.apiKey") }}
                        </dt>
                        <dd
                          class="font-mono text-gray-800 dark:text-dark-100"
                          data-testid="gateway-mock-events-api-key"
                        >
                          {{ observedID(row.api_key_id) }}
                        </dd>
                      </div>
                      <div class="flex items-center gap-1.5">
                        <dt class="text-gray-500 dark:text-dark-400">
                          {{ t("admin.gatewayMock.events.columns.user") }}
                        </dt>
                        <dd
                          class="font-mono text-gray-800 dark:text-dark-100"
                          data-testid="gateway-mock-events-user"
                        >
                          {{ observedID(row.user_id) }}
                        </dd>
                      </div>
                      <div class="flex items-center gap-1.5">
                        <dt class="text-gray-500 dark:text-dark-400">
                          {{ t("admin.gatewayMock.events.columns.group") }}
                        </dt>
                        <dd
                          class="font-mono text-gray-800 dark:text-dark-100"
                          data-testid="gateway-mock-events-group"
                        >
                          {{ observedID(row.group_id) }}
                        </dd>
                      </div>
                    </dl>
                  </td>
                  <td
                    class="px-3 py-2 text-xs"
                    :data-label="
                      t('admin.gatewayMock.events.columns.contentAudit')
                    "
                    data-testid="gateway-mock-events-content-audit"
                  >
                    <span
                      class="inline-flex rounded-full px-2 py-0.5 text-xs font-medium"
                      :class="contentAuditClass(row.content_audit_state)"
                    >
                      {{
                        t(gatewayMockContentAuditKey(row.content_audit_state))
                      }}
                    </span>
                  </td>
                  <td
                    class="gateway-mock-events-toggle-cell whitespace-nowrap px-3 py-2 text-right"
                  >
                    <button
                      type="button"
                      class="btn btn-ghost btn-sm inline-flex items-center gap-1"
                      :aria-expanded="isExpanded(index) ? 'true' : 'false'"
                      :aria-controls="
                        isExpanded(index) ? detailId(index) : undefined
                      "
                      :disabled="loading"
                      data-testid="gateway-mock-events-detail-toggle"
                      @click="toggleDetail(index)"
                    >
                      <Icon
                        :name="isExpanded(index) ? 'chevronUp' : 'chevronDown'"
                        size="sm"
                      />
                      <span>{{
                        t(
                          isExpanded(index)
                            ? "admin.gatewayMock.events.details.hide"
                            : "admin.gatewayMock.events.details.show",
                        )
                      }}</span>
                    </button>
                  </td>
                </tr>
                <tr
                  v-if="isExpanded(index)"
                  class="gateway-mock-events-detail-row border-b border-gray-200 bg-gray-50/60 dark:border-dark-700 dark:bg-dark-900/40"
                  data-testid="gateway-mock-events-detail-row"
                >
                  <td
                    :id="detailId(index)"
                    colspan="6"
                    class="gateway-mock-events-detail-cell px-3 py-3"
                  >
                    <p
                      class="text-xs font-medium text-gray-500 dark:text-dark-400"
                    >
                      {{ t("admin.gatewayMock.events.details.title") }}
                    </p>
                    <!-- 只读元数据：只用该行已记录的值，不查询历史、不回拼当前规则。 -->
                    <dl
                      class="mt-2 grid gap-x-6 gap-y-2 text-xs sm:grid-cols-2 lg:grid-cols-3"
                    >
                      <div class="min-w-0">
                        <dt class="text-gray-500 dark:text-dark-400">
                          {{ t("admin.gatewayMock.events.columns.rule") }}
                        </dt>
                        <dd
                          class="mt-0.5 min-w-0 font-mono text-gray-800 dark:text-dark-100"
                        >
                          <span class="break-all">{{ row.rule_id }}</span>
                          <span
                            class="ml-1 break-all text-gray-500 dark:text-dark-300"
                            data-testid="gateway-mock-events-rule-version"
                            >{{ observed(row.rule_version) }}</span
                          >
                        </dd>
                      </div>
                      <div class="min-w-0">
                        <dt class="text-gray-500 dark:text-dark-400">
                          {{ t("admin.gatewayMock.events.columns.account") }}
                        </dt>
                        <dd
                          class="mt-0.5 font-mono text-gray-800 dark:text-dark-100"
                          data-testid="gateway-mock-events-account"
                        >
                          {{ observedID(row.account_id) }}
                        </dd>
                      </div>
                      <div class="min-w-0">
                        <dt class="text-gray-500 dark:text-dark-400">
                          {{ t("admin.gatewayMock.events.columns.clientIp") }}
                        </dt>
                        <dd
                          class="mt-0.5 break-all font-mono text-gray-800 dark:text-dark-100"
                          data-testid="gateway-mock-events-client-ip"
                        >
                          {{ observed(row.client_ip) }}
                        </dd>
                      </div>
                      <div class="min-w-0">
                        <dt class="text-gray-500 dark:text-dark-400">
                          {{ t("admin.gatewayMock.events.columns.traceId") }}
                        </dt>
                        <dd
                          class="mt-0.5 select-all break-all font-mono text-gray-800 dark:text-dark-100"
                          data-testid="gateway-mock-events-trace"
                        >
                          {{ observed(row.trace_id) }}
                        </dd>
                      </div>
                      <div class="min-w-0">
                        <dt class="text-gray-500 dark:text-dark-400">
                          {{ t("admin.gatewayMock.events.columns.cleanup") }}
                        </dt>
                        <dd
                          class="mt-0.5 text-gray-800 dark:text-dark-100"
                          data-testid="gateway-mock-events-cleanup"
                        >
                          {{ t("admin.gatewayMock.events.noDeadline") }}
                        </dd>
                      </div>
                    </dl>
                    <p
                      class="mt-2 text-xs text-gray-500 dark:text-dark-400"
                      data-testid="gateway-mock-events-details-metadata-note"
                    >
                      {{ t("admin.gatewayMock.events.details.metadataNote") }}
                    </p>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
      </div>
      <Pagination
        class="mt-3"
        :total="total"
        :page="page"
        :page-size="pageSize"
        :page-size-options="eventPageSizeOptions"
        @update:page="load($event)"
        @update:page-size="changePageSize"
      />
    </template>
    <div
      v-else-if="failed"
      class="mt-4 rounded-lg border border-red-200 bg-red-50 p-3 dark:border-red-900 dark:bg-red-950/20"
    >
      <p
        role="alert"
        class="text-sm text-red-700 dark:text-red-300"
        data-testid="gateway-mock-events-failed"
      >
        {{ t("admin.gatewayMock.events.failed") }}
      </p>
      <button
        type="button"
        class="btn btn-secondary btn-sm mt-2"
        data-testid="gateway-mock-events-retry"
        :disabled="loading"
        @click="load(requestedPage)"
      >
        <Icon name="refresh" size="sm" class="mr-1" />
        {{
          loading
            ? t("admin.gatewayMock.events.loading")
            : t("admin.gatewayMock.events.retry")
        }}
      </button>
    </div>
    <p
      v-else-if="loading"
      role="status"
      class="mt-4 py-4 text-center text-sm text-gray-500 dark:text-dark-300"
      data-testid="gateway-mock-events-loading"
    >
      {{ t("admin.gatewayMock.events.loading") }}
    </p>
    <p
      v-else
      role="status"
      class="mt-4 rounded-lg border border-gray-200 bg-gray-50 p-3 text-sm dark:border-dark-700 dark:bg-dark-900"
      data-testid="gateway-mock-events-empty"
    >
      {{ t("admin.gatewayMock.events.empty") }}
    </p>

    <!-- 两段长说明下沉到可展开帮助，避免表头前的文字墙；保留项始终留在 DOM 中。 -->
    <details
      class="mt-4 text-xs text-gray-500 dark:text-dark-300"
      data-testid="gateway-mock-events-recording-note"
    >
      <summary class="cursor-pointer select-none">
        {{ t("admin.gatewayMock.events.recordingNote") }}
      </summary>
      <div class="mt-2 space-y-1">
        <p data-testid="gateway-mock-events-absent-note">
          {{ t("admin.gatewayMock.events.absentNote") }}
        </p>
        <p data-testid="gateway-mock-events-retention-note">
          {{ t("admin.gatewayMock.events.retentionNote") }}
        </p>
      </div>
    </details>
  </section>
</template>

<script setup lang="ts">
import {
  computed,
  getCurrentInstance,
  onBeforeUnmount,
  onMounted,
  ref,
} from "vue";
import { useI18n } from "vue-i18n";
import Icon from "@/components/icons/Icon.vue";
import Pagination from "@/components/common/Pagination.vue";
import { getConfiguredTablePageSizeOptions } from "@/utils/tablePreferences";
import { listEvents } from "./api";
import { gatewayMockContentAuditKey, gatewayMockProtocolKey } from "./labels";
import { gatewayMockEventMaxPageSize, type GatewayMockEvent } from "./types";

const { t } = useI18n();
const rows = ref<GatewayMockEvent[]>([]);
const total = ref(0);
const page = ref(1);
const requestedPage = ref(1);
const pageSize = ref(20);
const failed = ref(false);
const loading = ref(false);
// 命中事件没有稳定 id，展开状态只按当前页的行下标记；每次读取都清空，
// 否则刷新或翻页后旧下标会把详情展开到新内容上。
const expandedIndex = ref<number | null>(null);
const panelUid = `gateway-mock-events-${getCurrentInstance()?.uid ?? 0}`;
let revision = 0;
let controller: AbortController | null = null;

const absent = computed(() => t("admin.gatewayMock.events.absent"));

// 部署配置可能声明超过网关命中列表上限（100）的页大小；下拉只保留合法档位，
// 若配置里没有一个合法值则回退到 20/100，避免空下拉并把非法值写进全局持久化。
const eventPageSizeOptions = computed(() => {
  const withinLimit = getConfiguredTablePageSizeOptions().filter(
    (size) => size <= gatewayMockEventMaxPageSize,
  );
  return withinLimit.length > 0
    ? withinLimit
    : [20, gatewayMockEventMaxPageSize];
});

// 命中列表与审计页共用同一分页组件；页大小超过后端上限（100）时收敛回该上限。
function changePageSize(size: number) {
  const clamped = Math.min(
    Math.max(1, Math.trunc(size)),
    gatewayMockEventMaxPageSize,
  );
  pageSize.value = clamped;
  void load(1);
}

function isExpanded(index: number): boolean {
  return expandedIndex.value === index;
}

function toggleDetail(index: number): void {
  if (loading.value) return;
  expandedIndex.value = isExpanded(index) ? null : index;
}

function detailId(index: number): string {
  return `${panelUid}-detail-${index}`;
}

/**
 * 内容审计状态用克制的徽标表达：本地直答（未执行审计）为琥珀色，
 * 其余一律中性灰，绝不伪装成审计通过。
 */
function contentAuditClass(state: string): string {
  return state === "skipped_local_mock"
    ? "bg-amber-50 text-amber-700 dark:bg-amber-950/30 dark:text-amber-300"
    : "bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-dark-200";
}

/**
 * A value the gateway did not record renders as explicitly absent. The events
 * carry `''` and `0` for "not observed", so a blank cell would claim the panel
 * knows something it does not.
 */
function observed(value: string): string {
  return value === "" ? absent.value : value;
}

function observedID(value: number): string {
  return value > 0 ? `#${value}` : absent.value;
}

/**
 * The parser already refuses an unreadable time; this only formats what it kept.
 * The cleanup cell never calls this: the server does not disclose a deadline at
 * all (the stored one is an internal estimate), so that cell says so in words
 * instead of rendering a date that would read as the real cleanup time.
 */
function formatTime(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? absent.value : date.toLocaleString();
}

/**
 * One manual read of one page. The events are a growing log, so there is no
 * poller here; a read that cannot be completed leaves the panel saying it is
 * unknown instead of showing the previous page as if it were current.
 */
async function load(nextPage: number) {
  requestedPage.value = nextPage;
  const current = ++revision;
  controller?.abort();
  const requestController = new AbortController();
  controller = requestController;
  // 先收起详情：即使这次读取被更晚的请求作废，也不会残留上一页的展开态。
  expandedIndex.value = null;
  loading.value = true;
  try {
    const result = await listEvents(
      { page: nextPage, page_size: pageSize.value },
      { signal: requestController.signal },
    );
    if (current !== revision) return;
    expandedIndex.value = null;
    rows.value = result.items;
    total.value = result.total;
    page.value = result.page;
    pageSize.value = result.page_size;
    failed.value = false;
  } catch {
    if (current !== revision) return;
    rows.value = [];
    total.value = 0;
    failed.value = true;
  } finally {
    if (current === revision) loading.value = false;
  }
}

onMounted(() => {
  void load(1);
});
onBeforeUnmount(() => {
  revision += 1;
  controller?.abort();
  rows.value = [];
  expandedIndex.value = null;
});
</script>

<style scoped>
/* 窄屏把同一张 6 列表格折叠成纵向记录：隐藏表头，单元格用 data-label 作行内标签。 */
.gateway-mock-events-table tbody tr:last-child {
  border-bottom: 0;
}

@media (max-width: 767px) {
  .gateway-mock-events-table {
    min-width: 0;
  }

  .gateway-mock-events-table thead {
    display: none;
  }

  .gateway-mock-events-table,
  .gateway-mock-events-table tbody,
  .gateway-mock-events-table tr,
  .gateway-mock-events-table td {
    display: block;
    width: 100%;
  }

  .gateway-mock-events-table tbody tr {
    padding: 0.75rem 0.875rem;
    border-bottom: 1px solid #e5e7eb;
  }

  .dark .gateway-mock-events-table tbody tr {
    border-bottom-color: #334155;
  }

  .gateway-mock-events-table td[data-label] {
    display: grid;
    grid-template-columns: 6.5rem minmax(0, 1fr);
    gap: 0.5rem;
    align-items: start;
    padding: 0.2rem 0;
    white-space: normal;
  }

  .gateway-mock-events-table td[data-label]::before {
    content: attr(data-label);
    font-size: 0.75rem;
    color: #6b7280;
  }

  .dark .gateway-mock-events-table td[data-label]::before {
    color: #94a3b8;
  }

  /* 详情行整格铺开，不带行内标签；展开按钮占满一行便于点按。 */
  .gateway-mock-events-table .gateway-mock-events-detail-cell {
    padding: 0;
  }

  .gateway-mock-events-table .gateway-mock-events-toggle-cell {
    padding: 0.4rem 0 0;
    text-align: left;
  }
}
</style>
