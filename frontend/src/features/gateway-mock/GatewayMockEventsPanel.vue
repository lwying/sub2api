<template>
  <section
    class="rounded-xl border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800"
    data-testid="gateway-mock-events"
  >
    <div class="flex flex-wrap items-start justify-between gap-3">
      <div>
        <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
          {{ t("admin.gatewayMock.events.title") }}
        </h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-dark-300">
          {{ t("admin.gatewayMock.events.description") }}
        </p>
      </div>
      <button
        type="button"
        class="btn btn-secondary btn-sm"
        data-testid="gateway-mock-events-refresh"
        @click="load(page)"
      >
        {{
          loading
            ? t("admin.gatewayMock.events.refreshing")
            : t("admin.gatewayMock.events.refresh")
        }}
      </button>
    </div>
    <p
      class="mt-2 text-xs text-gray-500 dark:text-dark-300"
      data-testid="gateway-mock-events-absent-note"
    >
      {{ t("admin.gatewayMock.events.absentNote") }}
    </p>
    <p
      class="mt-1 text-xs text-gray-500 dark:text-dark-300"
      data-testid="gateway-mock-events-retention-note"
    >
      {{ t("admin.gatewayMock.events.retentionNote") }}
    </p>

    <template v-if="rows.length">
      <div class="mt-4 overflow-x-auto">
        <table
          class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700"
        >
          <thead
            class="bg-gray-50 text-left text-xs text-gray-500 dark:bg-dark-900"
          >
            <tr>
              <th class="px-3 py-2">
                {{ t("admin.gatewayMock.events.columns.occurredAt") }}
              </th>
              <th class="px-3 py-2">
                {{ t("admin.gatewayMock.events.columns.rule") }}
              </th>
              <th class="px-3 py-2">
                {{ t("admin.gatewayMock.events.columns.protocol") }}
              </th>
              <th class="px-3 py-2">
                {{ t("admin.gatewayMock.events.columns.model") }}
              </th>
              <th class="px-3 py-2">
                {{ t("admin.gatewayMock.events.columns.apiKey") }}
              </th>
              <th class="px-3 py-2">
                {{ t("admin.gatewayMock.events.columns.user") }}
              </th>
              <th class="px-3 py-2">
                {{ t("admin.gatewayMock.events.columns.group") }}
              </th>
              <th class="px-3 py-2">
                {{ t("admin.gatewayMock.events.columns.account") }}
              </th>
              <th class="px-3 py-2">
                {{ t("admin.gatewayMock.events.columns.clientIp") }}
              </th>
              <th class="px-3 py-2">
                {{ t("admin.gatewayMock.events.columns.traceId") }}
              </th>
              <th class="px-3 py-2">
                {{ t("admin.gatewayMock.events.columns.cleanup") }}
              </th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-200 dark:divide-dark-700">
            <tr
              v-for="(row, index) in rows"
              :key="index"
              data-testid="gateway-mock-events-row"
            >
              <td
                class="whitespace-nowrap px-3 py-2 text-xs"
                data-testid="gateway-mock-events-time"
              >
                {{ formatTime(row.occurred_at) }}
              </td>
              <td
                class="px-3 py-2 font-mono text-xs"
                data-testid="gateway-mock-events-rule"
              >
                <span data-testid="gateway-mock-events-rule-id">{{
                  row.rule_id
                }}</span>
                <span
                  v-if="row.rule_version"
                  class="ml-1 text-gray-500 dark:text-dark-300"
                  data-testid="gateway-mock-events-rule-version"
                  >{{ row.rule_version }}</span
                >
                <span
                  v-else
                  class="ml-1 text-gray-500 dark:text-dark-300"
                  data-testid="gateway-mock-events-rule-version"
                  >{{ absent }}</span
                >
              </td>
              <td
                class="px-3 py-2 text-xs"
                data-testid="gateway-mock-events-protocol"
              >
                {{ t(gatewayMockProtocolKey(row.protocol)) }}
              </td>
              <td
                class="px-3 py-2 text-xs"
                data-testid="gateway-mock-events-model"
              >
                {{ observed(row.model) }}
              </td>
              <td
                class="px-3 py-2 font-mono text-xs"
                data-testid="gateway-mock-events-api-key"
              >
                {{ observedID(row.api_key_id) }}
              </td>
              <td
                class="px-3 py-2 font-mono text-xs"
                data-testid="gateway-mock-events-user"
              >
                {{ observedID(row.user_id) }}
              </td>
              <td
                class="px-3 py-2 font-mono text-xs"
                data-testid="gateway-mock-events-group"
              >
                {{ observedID(row.group_id) }}
              </td>
              <td
                class="px-3 py-2 font-mono text-xs"
                data-testid="gateway-mock-events-account"
              >
                {{ observedID(row.account_id) }}
              </td>
              <td
                class="px-3 py-2 font-mono text-xs"
                data-testid="gateway-mock-events-client-ip"
              >
                {{ observed(row.client_ip) }}
              </td>
              <td
                class="px-3 py-2 font-mono text-xs"
                data-testid="gateway-mock-events-trace"
              >
                {{ observed(row.trace_id) }}
              </td>
              <td
                class="whitespace-nowrap px-3 py-2 text-xs"
                data-testid="gateway-mock-events-cleanup"
              >
                {{ t("admin.gatewayMock.events.noDeadline") }}
              </td>
            </tr>
          </tbody>
        </table>
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
    <p
      v-else-if="failed"
      role="alert"
      class="mt-4 rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-900 dark:bg-red-950/20"
      data-testid="gateway-mock-events-failed"
    >
      {{ t("admin.gatewayMock.events.failed") }}
    </p>
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
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import Pagination from "@/components/common/Pagination.vue";
import { getConfiguredTablePageSizeOptions } from "@/utils/tablePreferences";
import { listEvents } from "./api";
import { gatewayMockProtocolKey } from "./labels";
import { gatewayMockEventMaxPageSize, type GatewayMockEvent } from "./types";

const { t } = useI18n();
const rows = ref<GatewayMockEvent[]>([]);
const total = ref(0);
const page = ref(1);
const pageSize = ref(20);
const failed = ref(false);
const loading = ref(false);
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
  const current = ++revision;
  controller?.abort();
  const requestController = new AbortController();
  controller = requestController;
  loading.value = true;
  try {
    const result = await listEvents(
      { page: nextPage, page_size: pageSize.value },
      { signal: requestController.signal },
    );
    if (current !== revision) return;
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
});
</script>
