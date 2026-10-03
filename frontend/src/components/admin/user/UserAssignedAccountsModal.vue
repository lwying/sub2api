<template>
  <BaseDialog
    :show="show"
    :title="t('assignedAccounts.admin.title')"
    width="wide"
    @close="$emit('close')"
  >
    <div v-if="user" class="space-y-4">
      <!-- 紧凑头：用户身份 + 能力开关，一行完成；关闭只暂停资格，不清空草稿 -->
      <div
        class="flex flex-col gap-3 border-b border-gray-200 pb-4 dark:border-dark-600 sm:flex-row sm:items-center sm:justify-between"
      >
        <div class="min-w-0">
          <p
            class="truncate text-base font-semibold text-gray-900 dark:text-white"
          >
            {{ user.email }}
          </p>
          <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">
            {{ t("assignedAccounts.admin.hint", { email: user.email }) }}
          </p>
        </div>
        <label
          v-if="grantLoaded"
          class="flex shrink-0 cursor-pointer items-center gap-3"
        >
          <Toggle
            :model-value="enabled"
            :disabled="!grantLoaded"
            data-test="grant-enabled"
            @update:model-value="setEnabled"
          />
          <span class="text-sm font-medium text-gray-800 dark:text-gray-200">
            {{ t("assignedAccounts.admin.enableLabel") }}
          </span>
        </label>
      </div>

      <!-- 关闭能力但保留分配：明确说明，避免误以为撤权 -->
      <p
        v-if="grantLoaded && !enabled"
        class="text-xs text-amber-700 dark:text-amber-300"
        data-test="disabled-hint"
      >
        {{ t("assignedAccounts.admin.disabledHint") }}
      </p>

      <div v-if="loading" class="flex justify-center py-12">
        <LoadingSpinner />
      </div>

      <!-- 读取失败时不显示任何授权状态，也不允许保存（fail-closed）。 -->
      <div
        v-else-if="loadFailed"
        class="space-y-3 rounded-xl border border-red-200 px-4 py-6 text-center dark:border-red-900/40"
      >
        <p class="text-sm text-gray-600 dark:text-gray-300">
          {{ t("assignedAccounts.admin.loadFailed") }}
        </p>
        <button
          type="button"
          class="btn btn-secondary px-4"
          data-test="retry-load"
          @click="load()"
        >
          {{ t("assignedAccounts.retry") }}
        </button>
      </div>

      <div v-else class="space-y-4">
        <!-- 单表选项卡：勾选即草稿，不再有「加入」搬运步骤 -->
        <div
          class="flex items-center gap-1 border-b border-gray-200 dark:border-dark-600"
          role="tablist"
        >
          <button
            type="button"
            role="tab"
            :aria-selected="activeTab === 'all'"
            class="relative -mb-px border-b-2 px-4 py-2 text-sm font-medium transition-colors"
            :class="
              activeTab === 'all'
                ? 'border-primary-600 text-primary-700 dark:border-primary-400 dark:text-primary-300'
                : 'border-transparent text-gray-500 hover:text-gray-700 dark:text-dark-400 dark:hover:text-gray-200'
            "
            data-test="tab-all"
            @click="switchTab('all')"
          >
            {{ t("assignedAccounts.admin.tabAll") }}
          </button>
          <button
            type="button"
            role="tab"
            :aria-selected="activeTab === 'selected'"
            class="relative -mb-px border-b-2 px-4 py-2 text-sm font-medium transition-colors"
            :class="
              activeTab === 'selected'
                ? 'border-primary-600 text-primary-700 dark:border-primary-400 dark:text-primary-300'
                : 'border-transparent text-gray-500 hover:text-gray-700 dark:text-dark-400 dark:hover:text-gray-200'
            "
            data-test="tab-selected"
            @click="switchTab('selected')"
          >
            {{
              t("assignedAccounts.admin.tabSelected", { count: selectedCount })
            }}
          </button>
        </div>

        <!-- 共享筛选器：全部账号用服务端全量语义；已选页签隐藏本地不支持的分组并收窄状态 -->
        <AccountTableFilters
          :search-query="searchQuery"
          :filters="filters"
          :groups="groups"
          :visible-fields="filterVisibleFields"
          :type-options="typeOptions"
          :status-options="
            activeTab === 'selected' ? storedStatusOptions : undefined
          "
          @update:search-query="searchQuery = $event"
          @update:filters="onFiltersUpdate"
          @change="onFilterChange"
        />

        <!-- 全部账号：服务端分页 + 选择本页 / 选择全部匹配 -->
        <div
          v-if="activeTab === 'all'"
          class="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-gray-200 px-3 py-2 dark:border-dark-600"
        >
          <span
            class="text-xs text-gray-500 dark:text-gray-400"
            data-test="all-summary"
          >
            {{
              t("assignedAccounts.admin.allSummary", {
                count: allTotal,
                selected: currentPageSelectedCount,
              })
            }}
          </span>
          <div class="flex items-center gap-3">
            <button
              v-if="!selectAllRunning"
              type="button"
              class="text-xs font-medium text-primary-600 hover:underline disabled:cursor-not-allowed disabled:text-gray-400 dark:text-primary-400"
              :disabled="allTotal === 0 || listFailed"
              data-test="select-all-matching"
              @click="startSelectAllMatching"
            >
              {{
                t("assignedAccounts.admin.selectAllMatching", {
                  count: allTotal,
                })
              }}
            </button>
            <button
              v-else
              type="button"
              class="text-xs font-medium text-gray-500 hover:underline dark:text-gray-300"
              data-test="cancel-select-all"
              @click="cancelSelectAllMatching"
            >
              {{ t("assignedAccounts.admin.cancelSelectAll") }}
            </button>
          </div>
        </div>

        <!-- 已选账号：本地筛选 + 本地分页的草稿视图 -->
        <div
          v-else
          class="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-gray-200 px-3 py-2 dark:border-dark-600"
        >
          <span
            class="text-xs text-gray-500 dark:text-gray-400"
            data-test="draft-summary"
          >
            {{
              t("assignedAccounts.admin.draftSummary", {
                count: selectedCount,
              })
            }}
          </span>
          <button
            v-if="selectedCount > 0"
            type="button"
            class="text-xs font-medium text-gray-500 hover:underline dark:text-gray-400"
            data-test="clear-selection"
            @click="clearSelection"
          >
            {{ t("assignedAccounts.admin.clearSelection") }}
          </button>
        </div>

        <p
          v-if="selectAllNotice"
          class="rounded-lg border border-emerald-200 bg-emerald-50/60 px-3 py-2 text-xs text-emerald-800 dark:border-emerald-900/40 dark:bg-emerald-900/10 dark:text-emerald-200"
          data-test="select-all-notice"
        >
          {{ selectAllNotice }}
        </p>
        <p
          v-if="selectAllError"
          class="rounded-lg border border-red-200 bg-red-50/60 px-3 py-2 text-xs text-red-700 dark:border-red-900/40 dark:bg-red-900/10 dark:text-red-300"
          data-test="select-all-error"
        >
          {{ selectAllError }}
        </p>

        <AccountTable
          :columns="columns"
          :data="displayRows"
          :loading="activeTab === 'all' && listLoading"
          selectable
          :selected-keys="selectedIds"
          :selection-label="selectionLabel"
          @update:selected-keys="onSelectedKeysUpdate"
        >
          <!-- 草稿占位/失效项是选择器特有语义，需覆盖共享默认名称单元格 -->
          <template #cell-name="{ row }">
            <div class="flex items-center gap-2">
              <span class="font-medium text-gray-900 dark:text-white">
                {{ rowLabel(row) }}
              </span>
              <span
                v-if="invalidPendingIdSet.has(row.id)"
                class="shrink-0 rounded-full bg-red-100 px-2 py-0.5 text-[10px] font-medium text-red-700 dark:bg-red-900/40 dark:text-red-300"
                :data-test="`invalid-${row.id}`"
              >
                {{ t("assignedAccounts.admin.invalidPending") }}
              </span>
              <span
                v-if="!rowDetailsKnown(row)"
                class="shrink-0 rounded-full bg-gray-100 px-2 py-0.5 text-[10px] font-medium text-gray-600 dark:bg-dark-700 dark:text-gray-300"
                :data-test="`placeholder-${row.id}`"
              >
                {{ t("assignedAccounts.admin.unknownAccount") }}
              </span>
            </div>
          </template>

          <template #cell-actions="{ row }">
            <button
              v-if="activeTab === 'selected'"
              type="button"
              class="text-xs font-medium text-gray-500 hover:text-red-600 hover:underline dark:text-gray-400 dark:hover:text-red-400"
              :data-test="`remove-${row.id}`"
              @click="removeRow(row.id)"
            >
              {{ t("assignedAccounts.admin.remove") }}
            </button>
          </template>

          <template #empty>
            <div
              class="flex flex-col items-center py-8 text-center text-sm text-gray-500 dark:text-gray-400"
            >
              <p v-if="activeTab === 'selected'" data-test="no-selected">
                {{ t("assignedAccounts.admin.noSelected") }}
              </p>
              <p v-else-if="listFailed" data-test="list-failed">
                {{ t("assignedAccounts.admin.loadCandidatesFailed") }}
              </p>
              <p v-else data-test="no-accounts">
                {{ t("assignedAccounts.admin.noCandidates") }}
              </p>
            </div>
          </template>
        </AccountTable>

        <!-- 分页：全部账号为服务端分页，已选账号为本地分页 -->
        <Pagination
          v-if="activeTab === 'all'"
          :total="allTotal"
          :page="allPage"
          :page-size="allPageSize"
          @update:page="onAllPage"
          @update:page-size="onAllPageSize"
        />
        <Pagination
          v-else
          :total="selectedTotal"
          :page="selectedPage"
          :page-size="selectedPageSize"
          :show-page-size-selector="false"
          @update:page="onSelectedPage"
        />
      </div>
    </div>

    <template #footer>
      <div
        class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between"
      >
        <div class="flex items-center gap-3 text-sm">
          <span
            class="text-gray-600 dark:text-gray-300"
            data-test="selected-count"
          >
            {{
              t("assignedAccounts.admin.selectedCount", {
                count: selectedCount,
              })
            }}
          </span>
          <span
            v-if="dirty"
            class="rounded-full bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-700 dark:bg-amber-900/40 dark:text-amber-200"
            data-test="dirty-state"
          >
            {{ t("assignedAccounts.admin.unsaved") }}
          </span>
        </div>
        <div class="flex justify-end gap-3">
          <button
            type="button"
            class="btn btn-secondary px-5"
            data-test="cancel-grant"
            @click="$emit('close')"
          >
            {{ t("common.cancel") }}
          </button>
          <button
            type="button"
            class="btn btn-primary px-6"
            :disabled="!canSave"
            data-test="save-grant"
            @click="handleSave"
          >
            {{ submitting ? t("common.saving") : t("common.save") }}
          </button>
        </div>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { adminAPI } from "@/api/admin";
import type { AdminUser, AdminGroup, PaginatedResponse } from "@/types";
import type { AccountOptionItem } from "@/api/admin/accounts";
import BaseDialog from "@/components/common/BaseDialog.vue";
import LoadingSpinner from "@/components/common/LoadingSpinner.vue";
import AccountTable from "@/components/account/AccountTable.vue";
import Pagination from "@/components/common/Pagination.vue";
import Toggle from "@/components/common/Toggle.vue";
import AccountTableFilters from "@/components/admin/account/AccountTableFilters.vue";
import type { SelectOption } from "@/components/common/Select.vue";
import type { Column } from "@/components/common/types";
import { ACCOUNT_TYPE_OPTIONS } from "@/constants/accountTypes";
import { useAppStore } from "@/stores/app";
import { useTableSelection } from "@/composables/useTableSelection";
import {
  extractApiErrorMessage,
  extractApiErrorMetadata,
} from "@/utils/apiError";

/**
 * 选择器里的账号行只需安全展示字段：id、名称、平台、类型、状态。
 * 不把用户 DTO 强转为完整 Account，也不伪造 credentials/extra。
 */
interface AccountRow {
  id: number;
  name: string;
  platform: string;
  /** 与共享 AccountTable 的最小行字段保持一致 */
  type: string;
  status: string;
}

type AccountOption = AccountOptionItem;

const ALL_PAGE_SIZE = 20;
const SELECT_ALL_PAGE_SIZE = 100;
/**
 * 「选择全部匹配」的失控保护：仅用于防止分页循环无限进行（例如服务端 total
 * 与实际数据不一致而不收敛）。仓库当前没有账号集合硬上限，因此这不是策略性截断；
 * 一旦触发会明确提示数量与原因，且不合并任何半截结果。
 */
const SELECT_ALL_MAX_PAGES = 200;
const VISIBLE_FIELDS: ("platform" | "type" | "status" | "group")[] = [
  "platform",
  "type",
  "status",
  "group",
];
/** 已选页签是本地派生视图：不支持分组，且只认库内存储状态。 */
const SELECTED_VISIBLE_FIELDS: ("platform" | "type" | "status")[] = [
  "platform",
  "type",
  "status",
];
const STORED_STATUS_VALUES = new Set([
  "active",
  "inactive",
  "error",
  "expired",
  "",
]);

const props = defineProps<{ show: boolean; user: AdminUser | null }>();
const emit = defineEmits(["close", "success"]);
const { t } = useI18n();
const appStore = useAppStore();

const loading = ref(false);
const loadFailed = ref(false);
const submitting = ref(false);
/** 已成功加载授权的用户；只有它与当前用户一致时才允许保存。 */
const loadedUserId = ref<number | null>(null);

const enabled = ref(false);
const originalEnabled = ref(false);
const originalIds = ref<Set<number>>(new Set());

/** 详情缓存：选择集合以 id 为权威，缺少详情的 id 仍保留为占位。 */
const accountDetails = new Map<number, AccountRow>();

const activeTab = ref<"all" | "selected">("all");
const searchQuery = ref("");
const filters = reactive({
  platform: "",
  type: "",
  status: "",
  group: "",
});

const groups = ref<AdminGroup[]>([]);

// 全部账号：服务端分页
const allRows = ref<AccountRow[]>([]);
const allPage = ref(1);
const allPageSize = ref(ALL_PAGE_SIZE);
const allTotal = ref(0);
const listLoading = ref(false);
const listFailed = ref(false);
let listController: AbortController | null = null;

// 已选账号：本地分页
const selectedPage = ref(1);
const selectedPageSize = ref(ALL_PAGE_SIZE);

// 选择全部匹配：可取消的有限分页收集
const selectAllRunning = ref(false);
const selectAllNotice = ref("");
const selectAllError = ref("");
let selectAllController: AbortController | null = null;

const invalidPendingIds = ref<number[]>([]);
const invalidPendingIdSet = computed(() => new Set(invalidPendingIds.value));

/**
 * 授权草稿 ID 集合就是表格选中集合，直接复用 useTableSelection；
 * 不再有候选集合与已分配集合之间的「加入」搬运。
 */
const {
  selectedSet,
  selectedIds,
  selectedCount,
  setSelectedIds,
  clear: clearSelectionSet,
  batchUpdate,
} = useTableSelection<AccountRow>({
  rows: allRows,
  getId: (row) => row.id,
});

/** 当前草稿行的详情，按选择顺序排列；缺详情保留占位。 */
const draftRows = computed<AccountRow[]>(() =>
  selectedIds.value.map((id) => accountDetails.get(id) ?? placeholderRow(id)),
);

/** 状态归一化：去空白 + 小写，历史 `disabled` 与 `inactive` 同为手动停用。 */
function normalizeStatus(status: string): string {
  const value = String(status ?? "")
    .trim()
    .toLowerCase();
  return value === "disabled" ? "inactive" : value;
}

/** 已选页签的本地筛选（不改变草稿集合，只影响展示）。 */
const filteredDraftRows = computed(() =>
  draftRows.value.filter((row) => {
    if (filters.platform && row.platform !== filters.platform) return false;
    if (filters.type && row.type !== filters.type) return false;
    if (
      filters.status &&
      normalizeStatus(row.status) !== normalizeStatus(filters.status)
    )
      return false;
    if (searchQuery.value.trim()) {
      const q = searchQuery.value.trim().toLowerCase();
      const haystack = `${row.name} ${row.platform} ${row.type} ${row.id}`;
      if (!haystack.toLowerCase().includes(q)) return false;
    }
    return true;
  }),
);

/** 筛选控件可见字段：已选页签隐藏本地不支持的分组筛选。 */
const filterVisibleFields = computed<
  ("platform" | "type" | "status" | "group")[]
>(() => (activeTab.value === "all" ? VISIBLE_FIELDS : SELECTED_VISIBLE_FIELDS));

/** 账号类型候选复用完整目录（含 upstream/service_account）。 */
const typeOptions = computed<SelectOption[]>(() => [
  { value: "", label: t("admin.accounts.allTypes") },
  ...ACCOUNT_TYPE_OPTIONS.map((option) => ({
    value: option.value,
    label: t(option.labelKey),
  })),
]);

/** 已选页签只暴露库内存储状态，避免派生的运行态筛选永远匹配不到。 */
const storedStatusOptions = computed<SelectOption[]>(() => [
  { value: "", label: t("admin.accounts.allStatus") },
  { value: "active", label: t("admin.accounts.status.active") },
  { value: "inactive", label: t("admin.accounts.status.inactive") },
  { value: "error", label: t("admin.accounts.status.error") },
  { value: "expired", label: t("admin.accounts.status.expired") },
]);

const selectedTotal = computed(() => filteredDraftRows.value.length);
const selectedPageRows = computed(() => {
  const start = (selectedPage.value - 1) * selectedPageSize.value;
  return filteredDraftRows.value.slice(start, start + selectedPageSize.value);
});

const displayRows = computed<AccountRow[]>(() =>
  activeTab.value === "all" ? allRows.value : selectedPageRows.value,
);

const columns = computed<Column[]>(() => {
  const base: Column[] = [
    { key: "name", label: t("assignedAccounts.columns.name") },
    { key: "platform_type", label: t("assignedAccounts.columns.platform") },
    { key: "status", label: t("assignedAccounts.columns.status") },
  ];
  if (activeTab.value === "selected") {
    base.push({ key: "actions", label: t("assignedAccounts.columns.actions") });
  }
  return base;
});

const grantLoaded = computed(
  () =>
    props.user !== null &&
    loadedUserId.value === props.user?.id &&
    !loading.value &&
    !loadFailed.value,
);

/** 脏状态：能力开关或集合相对服务端已加载状态发生变化。 */
const dirty = computed(() => {
  if (!grantLoaded.value) return false;
  if (enabled.value !== originalEnabled.value) return true;
  if (selectedSet.value.size !== originalIds.value.size) return true;
  for (const id of selectedSet.value) {
    if (!originalIds.value.has(id)) return true;
  }
  return false;
});

/** 保存必须建立在「当前用户已成功读取授权」且有改动之上。 */
const canSave = computed(
  () =>
    props.user !== null &&
    !submitting.value &&
    grantLoaded.value &&
    dirty.value,
);

const currentPageSelectedCount = computed(
  () => allRows.value.filter((row) => selectedSet.value.has(row.id)).length,
);

// --- 请求代次：切换用户或重新打开时递增，晚到响应一律丢弃 ---

let requestGeneration = 0;
let listGeneration = 0;
let selectAllGeneration = 0;

function placeholderRow(id: number): AccountRow {
  return { id, name: "", platform: "", type: "", status: "" };
}

function rowDetailsKnown(row: AccountRow): boolean {
  return (
    row.name.trim() !== "" ||
    row.platform !== "" ||
    row.type !== "" ||
    row.status !== ""
  );
}

function rowLabel(row: AccountRow): string {
  return row.name.trim() !== "" ? row.name : `#${row.id}`;
}

function selectionLabel(row: AccountRow): string {
  return t("assignedAccounts.admin.selectAccount", { name: rowLabel(row) });
}

function toAccountRow(account: {
  id: number;
  name?: string;
  platform?: string;
  account_type?: string;
  type?: string;
  status?: string;
}): AccountRow {
  return {
    id: account.id,
    name: account.name ?? "",
    platform: account.platform ?? "",
    type: account.account_type ?? account.type ?? "",
    status: account.status ?? "",
  };
}

function cacheDetails(items: AccountOption[]): void {
  for (const item of items) {
    accountDetails.set(
      item.id,
      toAccountRow({
        id: item.id,
        name: item.name,
        platform: item.platform,
        type: item.type,
        status: item.status,
      }),
    );
  }
}

function listFilters(): {
  search?: string;
  platform?: string;
  type?: string;
  status?: string;
  group?: string;
} {
  const out: {
    search?: string;
    platform?: string;
    type?: string;
    status?: string;
    group?: string;
  } = {};
  const keyword = searchQuery.value.trim();
  if (keyword) out.search = keyword;
  if (filters.platform) out.platform = filters.platform;
  if (filters.type) out.type = filters.type;
  if (filters.status) out.status = filters.status;
  if (filters.group) out.group = filters.group;
  return out;
}

// --- 全部账号：服务端分页读取 ---

async function loadAllPage(page: number): Promise<void> {
  listController?.abort();
  const controller = new AbortController();
  listController = controller;
  const generation = ++listGeneration;
  const isCurrent = () =>
    !controller.signal.aborted && generation === listGeneration;

  listLoading.value = true;
  listFailed.value = false;
  try {
    const response: PaginatedResponse<AccountOptionItem> =
      await adminAPI.accounts.listOptions(
        page,
        allPageSize.value,
        listFilters(),
        { signal: controller.signal },
      );
    if (!isCurrent()) return;
    const rows = response.items.map((item) =>
      toAccountRow({
        id: item.id,
        name: item.name,
        platform: item.platform,
        type: item.type,
        status: item.status,
      }),
    );
    cacheDetails(response.items);
    allRows.value = rows;
    allPage.value = page;
    allTotal.value = response.total;
  } catch {
    if (!isCurrent()) return;
    allRows.value = [];
    allTotal.value = 0;
    listFailed.value = true;
  } finally {
    if (isCurrent()) {
      listLoading.value = false;
    }
  }
}

function onFiltersUpdate(next: Record<string, unknown>): void {
  filters.platform = String(next.platform ?? "");
  filters.type = String(next.type ?? "");
  filters.status = String(next.status ?? "");
  filters.group = String(next.group ?? "");
  selectedPage.value = 1;
}

/** 筛选或搜索变化：服务端列表与本地已选视图都从第 1 页重新开始。 */
function onFilterChange(): void {
  selectedPage.value = 1;
  // 筛选变化作废进行中的「选择全部匹配」，避免旧快照结果落到新筛选下的草稿。
  if (selectAllRunning.value) cancelSelectAllMatching();
  void loadAllPage(1);
}

function onAllPage(page: number): void {
  void loadAllPage(page);
}

function onAllPageSize(size: number): void {
  allPageSize.value = size;
  void loadAllPage(1);
}

function onSelectedPage(page: number): void {
  if (page >= 1) selectedPage.value = page;
}

// 选中页变短（移除/筛选）时把本地页码夹回有效范围
watch(selectedTotal, (total) => {
  const maxPage = Math.max(1, Math.ceil(total / selectedPageSize.value));
  if (selectedPage.value > maxPage) selectedPage.value = maxPage;
});

// --- 选择集合 ---

function onSelectedKeysUpdate(keys: Array<string | number>): void {
  const ids = keys
    .map((key) => Number(key))
    .filter((id) => Number.isSafeInteger(id) && id > 0);
  setSelectedIds(Array.from(new Set(ids)));
}

function removeRow(id: number): void {
  const next = new Set(selectedSet.value);
  next.delete(id);
  setSelectedIds(Array.from(next));
  invalidPendingIds.value = invalidPendingIds.value.filter(
    (invalidId) => invalidId !== id,
  );
}

function clearSelection(): void {
  clearSelectionSet();
}

function switchTab(tab: "all" | "selected"): void {
  activeTab.value = tab;
  if (tab === "selected") {
    selectedPage.value = 1;
    // 已选页签本地不支持分组；派生的运行态状态也匹配不到库内状态，清除以免隐形筛选。
    filters.group = "";
    if (!STORED_STATUS_VALUES.has(normalizeStatus(filters.status))) {
      filters.status = "";
    }
  } else {
    void loadAllPage(1);
  }
}

// --- 选择全部匹配：可取消的有限分页收集，成功才一次性合并 ---

function resetSelectAllState(): void {
  selectAllController?.abort();
  selectAllController = null;
  selectAllRunning.value = false;
  selectAllNotice.value = "";
  selectAllError.value = "";
}

async function startSelectAllMatching(): Promise<void> {
  if (!grantLoaded.value || selectAllRunning.value) return;

  selectAllController?.abort();
  const controller = new AbortController();
  selectAllController = controller;
  const generation = ++selectAllGeneration;
  const isCurrent = () =>
    !controller.signal.aborted && generation === selectAllGeneration;
  selectAllRunning.value = true;
  selectAllNotice.value = "";
  selectAllError.value = "";
  // 启动时固定筛选快照，运行期间不得逐页重读，否则中途改筛选会把别的集合混进本次全选。
  const snapshotFilters = listFilters();

  const collected = new Map<number, AccountRow>();
  try {
    let page = 1;
    let total = Number.POSITIVE_INFINITY;
    while (collected.size < total) {
      if (page > SELECT_ALL_MAX_PAGES) {
        throw Object.assign(new Error("select-all-page-guard"), {
          guardTripped: true,
          collected: collected.size,
        });
      }
      const response = await adminAPI.accounts.listOptions(
        page,
        SELECT_ALL_PAGE_SIZE,
        snapshotFilters,
        { signal: controller.signal },
      );
      if (!isCurrent()) return;
      total = response.total;
      if (response.items.length === 0) break;
      let addedOnPage = 0;
      for (const item of response.items) {
        if (!collected.has(item.id)) {
          collected.set(
            item.id,
            toAccountRow({
              id: item.id,
              name: item.name,
              platform: item.platform,
              type: item.type,
              status: item.status,
            }),
          );
          addedOnPage += 1;
        }
      }
      // 无新进展说明服务端分页不再收敛，停止而不是无限循环
      if (addedOnPage === 0) break;
      page += 1;
    }

    if (!isCurrent()) return;
    for (const row of collected.values()) accountDetails.set(row.id, row);
    batchUpdate((draft) => {
      for (const id of collected.keys()) draft.add(id);
    });
    selectAllNotice.value = t("assignedAccounts.admin.selectAllDone", {
      count: collected.size,
    });
  } catch (error) {
    if (!isCurrent()) return;
    if ((error as { guardTripped?: boolean })?.guardTripped) {
      selectAllError.value = t("assignedAccounts.admin.selectAllLimit", {
        count: (error as { collected?: number }).collected ?? collected.size,
        pages: SELECT_ALL_MAX_PAGES,
      });
    } else {
      selectAllError.value = t("assignedAccounts.admin.selectAllFailed");
    }
    // 失败不合并任何半截结果
  } finally {
    if (isCurrent()) {
      selectAllRunning.value = false;
      selectAllController = null;
    }
  }
}

function cancelSelectAllMatching(): void {
  // 取消作废本次收集：不合并、不伪称全选成功。
  selectAllGeneration += 1;
  selectAllController?.abort();
  selectAllController = null;
  selectAllRunning.value = false;
  selectAllNotice.value = "";
  selectAllError.value = "";
}

// --- 授权读取与保存 ---

function applyGrant(
  grant: {
    enabled?: boolean;
    account_ids?: number[];
    accounts?: {
      id: number;
      name: string;
      platform: string;
      account_type: string;
      status: string;
    }[];
  },
  fallbackIds: number[] = [],
): void {
  for (const account of grant.accounts ?? []) {
    accountDetails.set(account.id, toAccountRow(account));
  }

  const ids = Array.isArray(grant.account_ids)
    ? grant.account_ids
    : Array.isArray(grant.accounts)
      ? grant.accounts.map((account) => account.id)
      : fallbackIds;

  enabled.value = grant.enabled === true;
  originalEnabled.value = enabled.value;
  originalIds.value = new Set(ids);
  invalidPendingIds.value = [];
  setSelectedIds(ids);
}

function resetForNewUser(): void {
  requestGeneration += 1;
  listGeneration += 1;
  selectAllGeneration += 1;
  loadedUserId.value = null;
  loadFailed.value = false;
  listController?.abort();
  listController = null;
  enabled.value = false;
  originalEnabled.value = false;
  originalIds.value = new Set();
  accountDetails.clear();
  setSelectedIds([]);
  activeTab.value = "all";
  searchQuery.value = "";
  filters.platform = "";
  filters.type = "";
  filters.status = "";
  filters.group = "";
  groups.value = [];
  allRows.value = [];
  allPage.value = 1;
  allTotal.value = 0;
  listLoading.value = false;
  listFailed.value = false;
  selectedPage.value = 1;
  invalidPendingIds.value = [];
  resetSelectAllState();
  // 上一次会话的保存属于旧代次，不能继续禁用新会话的保存按钮。
  submitting.value = false;
}

async function load(): Promise<void> {
  const user = props.user;
  if (!user) return;

  resetForNewUser();
  const generation = requestGeneration;
  loading.value = true;

  try {
    const grant = await adminAPI.users.getAccountView(user.id);
    if (generation !== requestGeneration) return;
    applyGrant(grant);
    loadedUserId.value = user.id;
  } catch (error) {
    if (generation !== requestGeneration) return;
    loadFailed.value = true;
    appStore.showError(
      extractApiErrorMessage(error, t("assignedAccounts.admin.loadFailed")),
    );
    return;
  } finally {
    if (generation === requestGeneration) loading.value = false;
  }

  if (generation !== requestGeneration) return;
  void loadAllPage(1);
  try {
    const list = await adminAPI.groups.getAll();
    if (generation !== requestGeneration) return;
    groups.value = list;
  } catch {
    // 分组目录加载失败不阻塞授权编辑，分组筛选留空即可。
    if (generation !== requestGeneration) return;
    groups.value = [];
  }
}

function setEnabled(value: boolean): void {
  enabled.value = value;
}

/**
 * 保存回显只在「发起保存的那次会话仍然有效」时生效：
 * 弹窗仍打开、仍是同一个用户、该用户的授权已成功读取，且期间没有被关闭重开。
 */
function isSameSaveSession(userId: number, generation: number): boolean {
  return (
    props.show &&
    props.user?.id === userId &&
    loadedUserId.value === userId &&
    generation === requestGeneration
  );
}

/**
 * 从保存失败响应里取出服务端点名的失效账号 id。
 * 只认服务端约定的 `invalid_account_ids`（逗号分隔的十进制 id，有界）。
 */
function invalidAccountIdsFromError(error: unknown): number[] {
  const raw = extractApiErrorMetadata(error)?.invalid_account_ids;
  if (typeof raw !== "string") return [];
  const ids = raw
    .split(",")
    .map((part) => Number(part.trim()))
    .filter((id) => Number.isSafeInteger(id) && id > 0);
  return Array.from(new Set(ids));
}

async function handleSave(): Promise<void> {
  if (!props.user || !canSave.value) return;

  const user = props.user;
  const userId = user.id;
  const generation = requestGeneration;
  const submittedIds = [...selectedIds.value];
  const submittedEnabled = enabled.value;
  submitting.value = true;

  try {
    // 全量替换：提交的集合就是完整授权，撤销立即生效。
    const grant = await adminAPI.users.updateAccountView(userId, {
      enabled: submittedEnabled,
      account_ids: submittedIds,
    });
    if (!isSameSaveSession(userId, generation)) return;
    // 以服务端回显为准：草稿与原始基线都落到返回的 account_ids。
    // 只有回显完全缺少 account_ids/accounts 时才退回本次提交的 id，避免界面把授权显示成空。
    applyGrant(grant, submittedIds);
    appStore.showSuccess(t("assignedAccounts.admin.saveSuccess"));
    emit("success");
    emit("close");
  } catch (error) {
    if (!isSameSaveSession(userId, generation)) return;
    // 整批回滚：草稿、勾选与开关保持原样，只标出服务端点名的失效项。
    invalidPendingIds.value = invalidAccountIdsFromError(error);
    appStore.showError(
      extractApiErrorMessage(error, t("assignedAccounts.admin.saveFailed")),
    );
  } finally {
    if (generation === requestGeneration) submitting.value = false;
  }
}

watch(
  () => [props.show, props.user?.id] as const,
  ([visible]) => {
    if (visible && props.user) {
      void load();
      return;
    }
    // 关闭即结束本次选择会话。
    resetSelectAllState();
  },
  { immediate: true },
);
</script>
