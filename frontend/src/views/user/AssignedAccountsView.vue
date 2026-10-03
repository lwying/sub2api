<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="space-y-4">
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div>
              <h1 class="text-lg font-semibold text-gray-900 dark:text-white">
                {{ t("nav.assignedAccounts") }}
              </h1>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
                {{ t("assignedAccounts.description") }}
              </p>
            </div>
            <button
              type="button"
              class="btn btn-secondary"
              :disabled="loading"
              data-test="refresh-accounts"
              @click="loadAccounts"
            >
              <Icon
                name="refresh"
                size="sm"
                :class="loading ? 'animate-spin' : ''"
              />{{ t("common.refresh") }}
            </button>
          </div>
          <AccountTableFilters
            :search-query="searchQuery"
            :filters="filters"
            :groups="groups"
            :visible-fields="['platform', 'type', 'status', 'group']"
            :type-options="typeOptions"
            :status-options="statusOptions"
            :show-ungrouped="false"
            @update:search-query="updateSearch"
            @update:filters="filters = $event"
            @change="refreshFilteredAccounts"
          />
          <p class="text-xs text-gray-500 dark:text-gray-400">
            {{ t("assignedAccounts.readOnlyNotice") }}
            {{ t("assignedAccounts.identityMaskedNotice") }}
          </p>
          <p
            v-if="groupsError"
            data-test="groups-error"
            role="alert"
            class="text-xs text-amber-700 dark:text-amber-300"
          >
            {{ t("admin.groups.failedToLoad") }}
            <button
              type="button"
              data-test="retry-groups"
              class="underline"
              @click="loadGroups"
            >
              {{ t("assignedAccounts.retry") }}
            </button>
          </p>
          <p
            v-if="runtimeError"
            role="alert"
            class="text-xs text-amber-700 dark:text-amber-300"
          >
            {{ t("assignedAccounts.loadFailed") }}
            <button type="button" class="underline" @click="loadAccounts">
              {{ t("assignedAccounts.retry") }}
            </button>
          </p>
        </div>
      </template>
      <template #table>
        <AccountTable
          :columns="columns"
          :data="displayAccounts"
          :loading="loading"
          :runtime-by-id="runtime"
          :runtime-loading="runtimeLoading"
          :runtime-error="runtimeError"
        >
          <template #cell-actions="{ row }"
            ><div class="flex flex-wrap items-center gap-2">
              <button
                type="button"
                class="btn btn-secondary btn-sm"
                data-test="view-detail"
                @click="openDetail(row)"
              >
                {{ t("assignedAccounts.viewDetail") }}</button
              ><button
                type="button"
                class="btn btn-secondary btn-sm"
                data-test="view-stats"
                @click="openStats(row)"
              >
                {{ t("admin.accounts.viewStats") }}
              </button>
            </div></template
          >
          <template #empty
            ><EmptyState
              :title="t('assignedAccounts.empty')"
              :description="t('assignedAccounts.emptyHint')"
          /></template>
        </AccountTable>
      </template>
      <template #pagination
        ><Pagination
          v-if="pagination.total > 0"
          :page="pagination.page"
          :total="pagination.total"
          :page-size="pagination.page_size"
          @update:page="handlePageChange"
          @update:page-size="handlePageSizeChange"
      /></template>
    </TablePageLayout>
    <BaseDialog
      :show="showDetail"
      :title="t('assignedAccounts.detail.title')"
      width="extra-wide"
      @close="closeDetail"
    >
      <div v-if="detailLoading" class="flex justify-center py-10">
        <LoadingSpinner />
      </div>
      <p
        v-else-if="detailUnavailable"
        data-test="detail-unavailable"
        class="py-6 text-sm text-gray-500"
      >
        {{ t("assignedAccounts.detail.notVisible") }}
      </p>
      <div
        v-else-if="detailFailed"
        data-test="detail-failed"
        class="space-y-3 py-6 text-sm text-gray-600 dark:text-gray-300"
      >
        <p>{{ t("assignedAccounts.detail.loadFailed") }}</p>
        <button
          type="button"
          class="btn btn-secondary"
          data-test="retry-detail"
          @click="retryDetail"
        >
          {{ t("assignedAccounts.retry") }}
        </button>
      </div>
      <div v-else-if="detail" data-test="detail-fields">
        <AccountTable
          :data="[toDisplay(detail)]"
          :columns="detailColumns"
          :runtime-by-id="runtime"
          :runtime-loading="runtimeLoading"
          :runtime-error="runtimeError"
        />
      </div>
    </BaseDialog>
    <AccountStatsModal
      :show="showStats"
      :account="statsAccount"
      read-only
      :stats-fetcher="fetchStats"
      @close="showStats = false"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from "vue";
import { useRouter } from "vue-router";
import { useI18n } from "vue-i18n";
import AppLayout from "@/components/layout/AppLayout.vue";
import TablePageLayout from "@/components/layout/TablePageLayout.vue";
import AccountTable from "@/components/account/AccountTable.vue";
import AccountTableFilters from "@/components/admin/account/AccountTableFilters.vue";
import AccountStatsModal from "@/components/account/AccountStatsModal.vue";
import EmptyState from "@/components/common/EmptyState.vue";
import Pagination from "@/components/common/Pagination.vue";
import BaseDialog from "@/components/common/BaseDialog.vue";
import LoadingSpinner from "@/components/common/LoadingSpinner.vue";
import Icon from "@/components/icons/Icon.vue";
import type { Column } from "@/components/common/types";
import {
  accountReadColumns,
  normalizeAccountStatus,
  type AccountDisplayRow,
  type AccountRuntimeSnapshot,
} from "@/components/account/accountDisplay";
import type {
  AccountCellGroup,
  ReadonlyStatsAccount,
} from "@/components/account/accountCellTypes";
import assignedAccountsAPI, {
  isAssignedAccountNotFound,
  isAssignedAccountsAccessDenied,
  type AssignedAccount,
} from "@/api/assignedAccounts";
import {
  getPersistedPageSize,
  setPersistedPageSize,
} from "@/composables/usePersistedPageSize";
import { useAppStore } from "@/stores/app";
import { useAuthStore } from "@/stores/auth";
import { extractApiErrorMessage } from "@/utils/apiError";
import { ASSIGNED_ACCOUNTS_FALLBACK_PATH } from "@/router/assignedAccountsAccess";
import { ACCOUNT_TYPE_OPTIONS } from "@/constants/accountTypes";

const { t } = useI18n();
const router = useRouter();
const appStore = useAppStore();
const authStore = useAuthStore();
const accounts = ref<AssignedAccount[]>([]);
const loading = ref(false);
const filters = ref({ platform: "", type: "", status: "", group: "" });
const searchQuery = ref("");
const groups = ref<AccountCellGroup[]>([]);
const groupsError = ref(false);
const runtime = ref<Record<string, AccountRuntimeSnapshot>>({});
const runtimeLoading = ref(false);
const runtimeError = ref(false);
const pagination = reactive({
  page: 1,
  page_size: getPersistedPageSize(),
  total: 0,
});
let listGeneration = 0;
let listController: AbortController | null = null;
let searchTimer: ReturnType<typeof setTimeout> | undefined;
const typeOptions = computed(() => [
  { value: "", label: t("admin.accounts.allTypes") },
  ...ACCOUNT_TYPE_OPTIONS.map((option) => ({
    value: option.value,
    label: t(option.labelKey),
  })),
]);

const statusOptions = computed(() => [
  { value: "", label: t("admin.accounts.allStatus") },
  ...["active", "inactive", "error", "expired"].map((value) => ({
    value,
    label: t(`admin.accounts.status.${value}`),
  })),
]);

const identityColumns = computed<Column[]>(() => [
  { key: "email_masked", label: t("assignedAccounts.columns.email") },
  { key: "username_masked", label: t("assignedAccounts.columns.username") },
  {
    key: "upstream_account_id_masked",
    label: t("assignedAccounts.columns.upstreamAccountId"),
  },
]);
const columns = computed(() => [
  ...accountReadColumns(t).filter((column) => column.key !== "actions"),
  ...identityColumns.value,
  { key: "actions", label: t("assignedAccounts.columns.actions") },
]);
const detailColumns = computed(() =>
  columns.value.filter((column) => column.key !== "actions"),
);

// 明确投影允许字段；即使响应夹带额外数据，也不把凭据送入共享展示模块。
function toDisplay(account: AssignedAccount): AccountDisplayRow {
  const snapshot = runtime.value[String(account.id)];
  return {
    id: account.id,
    name: account.name,
    platform: account.platform,
    type: account.account_type,
    status: normalizeAccountStatus(account.status),
    schedulable: account.schedulable,
    concurrency: account.concurrency,
    current_concurrency: snapshot?.current_concurrency ?? null,
    groups: account.groups,
    email_masked: account.email_masked,
    username_masked: account.username_masked,
    upstream_account_id_masked: account.upstream_account_id_masked,
    rate_limit_reset_at: account.rate_limit_reset_at,
    overload_until: account.overload_until,
    temp_unschedulable_until: account.temp_unschedulable_until,
    window_cost_limit: account.window_cost_limit,
    window_cost_sticky_reserve: account.window_cost_sticky_reserve,
    max_sessions: account.max_sessions,
    session_idle_timeout_minutes: account.session_idle_timeout_minutes,
    base_rpm: account.base_rpm,
    rpm_strategy: account.rpm_strategy,
    quota_limit: account.quota_limit,
    quota_used: account.quota_used,
    quota_daily_limit: account.quota_daily_limit,
    quota_daily_used: account.quota_daily_used,
    quota_weekly_limit: account.quota_weekly_limit,
    quota_weekly_used: account.quota_weekly_used,
  };
}
const displayAccounts = computed(() => accounts.value.map(toDisplay));
const showDetail = ref(false);
const detailLoading = ref(false);
const detailUnavailable = ref(false);
const detailFailed = ref(false);
const detail = ref<AssignedAccount | null>(null);
const detailAccountId = ref<number | null>(null);
let detailGeneration = 0;
let groupsGeneration = 0;
let groupsController: AbortController | null = null;
let accessRevoked = false;
const showStats = ref(false);
const statsAccount = ref<ReadonlyStatsAccount | null>(null);

async function handleAccessRevoked() {
  if (accessRevoked) return;
  accessRevoked = true;
  listGeneration++;
  groupsGeneration++;
  listController?.abort();
  groupsController?.abort();
  loading.value = false;
  runtimeLoading.value = false;
  pagination.total = 0;
  accounts.value = [];
  runtime.value = {};
  groups.value = [];
  closeDetail();
  showStats.value = false;
  void authStore.refreshUser().catch(() => undefined);
  await router.replace(ASSIGNED_ACCOUNTS_FALLBACK_PATH);
}

async function loadAccounts() {
  if (accessRevoked) return;
  const generation = ++listGeneration;
  listController?.abort();
  const controller = new AbortController();
  listController = controller;
  loading.value = true;
  runtimeError.value = false;
  try {
    const page = await assignedAccountsAPI.list(
      pagination.page,
      pagination.page_size,
      {
        platform: filters.value.platform,
        account_type: filters.value.type,
        status: filters.value.status,
        group: filters.value.group,
        search: searchQuery.value.trim(),
      },
      { signal: controller.signal },
    );
    if (generation !== listGeneration) return;
    accounts.value = page.items;
    pagination.total = page.total;
    pagination.page_size = page.page_size;
    runtime.value = {};
    runtimeLoading.value = page.items.length > 0;
    try {
      const snapshots = page.items.length
        ? await assignedAccountsAPI.getRuntime(
            page.items.map((account) => account.id),
            { signal: controller.signal },
          )
        : {};
      if (generation !== listGeneration) return;
      runtime.value = snapshots;
      // 批量接口省略失去授权的账号；快照为空与未获授权是两种不同状态。
      const revoked = page.items.filter(
        (account) =>
          !Object.prototype.hasOwnProperty.call(snapshots, String(account.id)),
      );
      if (revoked.length) {
        const revokedIDs = new Set(revoked.map((account) => account.id));
        accounts.value = accounts.value.filter(
          (account) => !revokedIDs.has(account.id),
        );
        pagination.total = Math.max(0, pagination.total - revoked.length);
        if (
          detailAccountId.value !== null &&
          revokedIDs.has(detailAccountId.value)
        )
          closeDetail();
        if (statsAccount.value && revokedIDs.has(statsAccount.value.id)) {
          showStats.value = false;
          statsAccount.value = null;
        }
        void loadGroups();
      }
    } catch (error) {
      if (generation !== listGeneration) return;
      if (isAssignedAccountsAccessDenied(error)) {
        await handleAccessRevoked();
        return;
      }
      runtimeError.value = true;
    } finally {
      if (generation === listGeneration) runtimeLoading.value = false;
    }
  } catch (error) {
    if (generation !== listGeneration) return;
    accounts.value = [];
    runtime.value = {};
    runtimeLoading.value = false;
    if (isAssignedAccountsAccessDenied(error)) {
      await handleAccessRevoked();
      return;
    }
    appStore.showError(
      extractApiErrorMessage(error, t("assignedAccounts.loadFailed")),
    );
  } finally {
    if (generation === listGeneration) loading.value = false;
  }
}

async function loadGroups() {
  if (accessRevoked) return;
  const generation = ++groupsGeneration;
  groupsController?.abort();
  const controller = new AbortController();
  groupsController = controller;
  groupsError.value = false;
  try {
    const result = await assignedAccountsAPI.getGroups({
      signal: controller.signal,
    });
    if (generation === groupsGeneration) groups.value = result;
  } catch (error) {
    if (generation !== groupsGeneration) return;
    if (isAssignedAccountsAccessDenied(error)) await handleAccessRevoked();
    else groupsError.value = true;
  }
}

async function openDetail(row: AccountDisplayRow) {
  showDetail.value = true;
  await loadDetail(row.id);
}
async function loadDetail(id: number) {
  detailAccountId.value = id;
  detailLoading.value = true;
  detailUnavailable.value = false;
  detailFailed.value = false;
  detail.value = null;
  const generation = ++detailGeneration;
  try {
    const account = await assignedAccountsAPI.getById(id);
    if (generation === detailGeneration) detail.value = account;
  } catch (error) {
    if (generation !== detailGeneration) return;
    if (isAssignedAccountsAccessDenied(error)) {
      await handleAccessRevoked();
      return;
    }
    if (isAssignedAccountNotFound(error)) {
      detailUnavailable.value = true;
      const previousCount = accounts.value.length;
      accounts.value = accounts.value.filter((account) => account.id !== id);
      pagination.total = Math.max(
        0,
        pagination.total - (previousCount - accounts.value.length),
      );
      delete runtime.value[String(id)];
      void loadGroups();
    } else detailFailed.value = true;
  } finally {
    if (generation === detailGeneration) detailLoading.value = false;
  }
}
function retryDetail() {
  if (detailAccountId.value !== null) void loadDetail(detailAccountId.value);
}
function closeDetail() {
  detailGeneration++;
  showDetail.value = false;
  detailLoading.value = false;
  detail.value = null;
  detailUnavailable.value = false;
  detailFailed.value = false;
  detailAccountId.value = null;
}
function openStats(row: AccountDisplayRow) {
  statsAccount.value = { id: row.id, name: row.name, status: row.status };
  showStats.value = true;
}
async function fetchStats(account: ReadonlyStatsAccount) {
  try {
    return await assignedAccountsAPI.getStats(account.id);
  } catch (error) {
    if (
      showStats.value &&
      statsAccount.value?.id === account.id &&
      isAssignedAccountsAccessDenied(error)
    )
      await handleAccessRevoked();
    throw error;
  }
}
function refreshFilteredAccounts() {
  if (searchTimer) clearTimeout(searchTimer);
  searchTimer = undefined;
  pagination.page = 1;
  void loadAccounts();
}
function updateSearch(value: string) {
  searchQuery.value = value;
  listGeneration++;
  listController?.abort();
  if (searchTimer) clearTimeout(searchTimer);
  searchTimer = setTimeout(refreshFilteredAccounts, 300);
}
function handlePageChange(page: number) {
  pagination.page = page;
  void loadAccounts();
}
function handlePageSizeChange(size: number) {
  pagination.page_size = size;
  pagination.page = 1;
  setPersistedPageSize(size);
  void loadAccounts();
}
onMounted(() => {
  void loadAccounts();
  void loadGroups();
});
onUnmounted(() => {
  if (searchTimer) clearTimeout(searchTimer);
  listController?.abort();
  listGeneration++;
  detailGeneration++;
  groupsGeneration++;
  groupsController?.abort();
  accessRevoked = true;
});
</script>
