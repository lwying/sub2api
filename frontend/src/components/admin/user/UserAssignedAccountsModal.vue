<template>
  <BaseDialog
    :show="show"
    :title="t('assignedAccounts.admin.title')"
    width="wide"
    @close="$emit('close')"
  >
    <div v-if="user" class="space-y-6">
      <div
        class="rounded-2xl bg-gradient-to-r from-primary-50 to-primary-100 p-5 dark:from-primary-900/30 dark:to-primary-800/20"
      >
        <p class="text-lg font-semibold text-gray-900 dark:text-white">
          {{ user.email }}
        </p>
        <p class="mt-1 text-sm text-gray-600 dark:text-gray-400">
          {{ t("assignedAccounts.admin.hint", { email: user.email }) }}
        </p>
      </div>

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

      <div v-else class="space-y-6">
        <!-- 能力开关：默认关闭，关闭时不展示任何账号 -->
        <label
          class="flex cursor-pointer items-center gap-3 rounded-xl border border-gray-200 p-4 dark:border-dark-600"
        >
          <input
            type="checkbox"
            class="h-5 w-5 rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-500"
            :checked="enabled"
            data-test="grant-enabled"
            @change="enabled = ($event.target as HTMLInputElement).checked"
          />
          <span class="text-sm font-medium text-gray-800 dark:text-gray-200">
            {{ t("assignedAccounts.admin.enableLabel") }}
          </span>
        </label>

        <!-- 已分配账号：以后端返回的 account_ids 为准，缺少详情的账号保留为占位条目 -->
        <div>
          <div class="mb-3 flex items-center gap-2">
            <div class="h-1.5 w-1.5 rounded-full bg-primary-500"></div>
            <h4 class="text-sm font-semibold text-gray-700 dark:text-gray-300">
              {{ t("assignedAccounts.admin.assignedTitle") }}
            </h4>
            <span class="text-xs text-gray-400" data-test="assigned-count">
              {{
                t("assignedAccounts.admin.assignedCount", {
                  count: assigned.length,
                })
              }}
            </span>
          </div>

          <p
            v-if="assigned.length === 0"
            class="rounded-xl border border-dashed border-gray-200 px-4 py-6 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400"
            data-test="no-assigned"
          >
            {{ t("assignedAccounts.admin.noAssigned") }}
          </p>

          <ul v-else class="space-y-2">
            <li
              v-for="account in assigned"
              :key="account.id"
              class="flex items-center justify-between gap-3 rounded-xl border px-4 py-3"
              :class="
                invalidPendingIdSet.has(account.id)
                  ? 'border-red-300 bg-red-50/50 dark:border-red-900/50 dark:bg-red-900/10'
                  : 'border-gray-200 dark:border-dark-600'
              "
              :data-test="`assigned-${account.id}`"
            >
              <div class="min-w-0">
                <div class="flex items-center gap-2">
                  <p
                    class="truncate text-sm font-medium text-gray-900 dark:text-white"
                  >
                    {{ accountLabel(account) }}
                  </p>
                  <!-- 服务端上一次保存点名的失效项：只标出 id，不猜原因。 -->
                  <span
                    v-if="invalidPendingIdSet.has(account.id)"
                    class="shrink-0 rounded-full bg-red-100 px-2 py-0.5 text-[10px] font-medium text-red-700 dark:bg-red-900/40 dark:text-red-300"
                    :data-test="`assigned-invalid-${account.id}`"
                  >
                    {{ t("assignedAccounts.admin.invalidPending") }}
                  </span>
                </div>
                <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">
                  {{ accountMeta(account) }}
                </p>
              </div>
              <button
                type="button"
                class="btn btn-secondary px-3 py-1 text-xs"
                :data-test="`remove-${account.id}`"
                @click="removeAccount(account.id)"
              >
                {{ t("assignedAccounts.admin.remove") }}
              </button>
            </li>
          </ul>

          <!--
            保存被整批回滚时，服务端返回的失效 id 在此点名：草稿、勾选与开关都保持不动，
            管理员只需移除或替换被标出的账号再保存，不必重新挑选整批。
          -->
          <div
            v-if="pendingInvalidIds.length > 0"
            class="mt-3 rounded-xl border border-red-200 bg-red-50/50 px-4 py-3 dark:border-red-900/40 dark:bg-red-900/10"
            data-test="save-invalid-accounts"
          >
            <p class="text-sm text-red-700 dark:text-red-300">
              {{
                t("assignedAccounts.admin.saveUnknownAccounts", {
                  count: pendingInvalidIds.length,
                  ids: pendingInvalidIds.join(", "),
                })
              }}
            </p>
            <p class="mt-1 text-xs text-red-600/80 dark:text-red-400/80">
              {{ t("assignedAccounts.admin.saveUnknownAccountsHint") }}
            </p>
          </div>
        </div>

        <!-- 添加账号：搜索与分页都走后端，避免只能授权前若干账号 -->
        <div>
          <input
            v-model="search"
            type="text"
            class="input w-full"
            :placeholder="t('assignedAccounts.admin.searchPlaceholder')"
            data-test="account-search"
            @input="handleSearchInput"
          />

          <div class="mt-3 flex flex-wrap gap-2">
            <select
              data-test="candidate-platform"
              v-model="candidatePlatform"
              class="input w-36"
              @change="handleCandidateFilterChange"
            >
              <option value="">
                {{ t("assignedAccounts.filters.allPlatforms") }}
              </option>
              <option
                v-for="option in CONCRETE_PLATFORM_OPTIONS"
                :key="option.value"
                :value="option.value"
              >
                {{ option.label }}
              </option>
            </select>
            <select
              data-test="candidate-type"
              v-model="candidateType"
              class="input w-36"
              @change="handleCandidateFilterChange"
            >
              <option value="">
                {{ t("assignedAccounts.filters.allTypes") }}
              </option>
              <option
                v-for="option in ACCOUNT_TYPE_OPTIONS"
                :key="option.value"
                :value="option.value"
              >
                {{ t(option.labelKey) }}
              </option>
            </select>
            <select
              data-test="candidate-status"
              v-model="candidateStatus"
              class="input w-36"
              @change="handleCandidateFilterChange"
            >
              <option value="">{{ t("admin.accounts.allStatus") }}</option>
              <option value="active">
                {{ t("admin.accounts.status.active") }}
              </option>
              <option value="inactive">
                {{ t("admin.accounts.status.inactive") }}
              </option>
              <option value="error">
                {{ t("admin.accounts.status.error") }}
              </option>
              <option value="rate_limited">
                {{ t("admin.accounts.status.rateLimited") }}
              </option>
              <option value="temp_unschedulable">
                {{ t("admin.accounts.status.tempUnschedulable") }}
              </option>
              <option value="unschedulable">
                {{ t("admin.accounts.status.unschedulable") }}
              </option>
            </select>
            <select
              data-test="candidate-group"
              v-model="candidateGroup"
              class="input w-40"
              :disabled="groupsFailed"
              @change="handleCandidateFilterChange"
            >
              <option value="">{{ t("admin.accounts.allGroups") }}</option>
              <option v-if="!groupsFailed" value="ungrouped">
                {{ t("admin.accounts.ungroupedGroup") }}
              </option>
              <option
                v-for="group in candidateGroups"
                :key="group.id"
                :value="String(group.id)"
              >
                {{ group.name }}
              </option>
            </select>
          </div>

          <!-- 多选后再一键加入待授权列表；选择只在本弹窗会话内有效，跨筛选与翻页保留。 -->
          <div
            v-if="visibleCandidates.length > 0 || selectedCount > 0"
            class="mt-3 flex flex-wrap items-center justify-between gap-2 rounded-lg border border-gray-200 px-3 py-2 dark:border-dark-600"
            data-test="candidate-selection"
          >
            <label
              class="flex cursor-pointer items-center gap-2 text-xs text-gray-600 dark:text-gray-300"
            >
              <input
                type="checkbox"
                class="h-4 w-4 cursor-pointer rounded border-gray-300 text-primary-600 focus:ring-primary-500 disabled:cursor-not-allowed dark:border-dark-500"
                :checked="allVisibleSelected"
                :disabled="visibleCandidates.length === 0"
                data-test="select-visible-candidates"
                @change="handleToggleSelectVisible"
              />
              {{ t("assignedAccounts.admin.selectVisible") }}
            </label>
            <div class="flex items-center gap-3">
              <span
                class="text-xs text-gray-500 dark:text-gray-400"
                data-test="selection-count"
              >
                {{
                  t("assignedAccounts.admin.selectedCount", {
                    count: selectedCount,
                  })
                }}
              </span>
              <button
                v-if="selectedCount > 0"
                type="button"
                class="text-xs font-medium text-gray-500 hover:underline dark:text-gray-400"
                data-test="clear-selection"
                @click="clearCandidateSelection"
              >
                {{ t("assignedAccounts.admin.clearSelection") }}
              </button>
              <button
                type="button"
                class="btn btn-primary px-3 py-1 text-xs"
                :disabled="selectedCount === 0"
                data-test="batch-add-selected"
                @click="addSelectedAccounts"
              >
                {{ t("assignedAccounts.admin.batchAdd") }}
              </button>
            </div>
          </div>

          <!--
            一键加入只合并本地待授权列表，不发起写入；授权仍然只在「保存」时一次提交，
            因此这里只报告合并结果，不宣布授权已生效。
          -->
          <div
            v-if="lastBatchAddResult"
            class="mt-3 rounded-xl border border-gray-200 px-4 py-3 dark:border-dark-600"
            data-test="batch-add-summary"
          >
            <p class="text-sm text-gray-700 dark:text-gray-200">
              {{
                t("assignedAccounts.admin.batchAddSummary", {
                  added: lastBatchAddResult.added,
                  skipped: lastBatchAddResult.skipped,
                  failed: lastBatchAddResult.failed,
                })
              }}
            </p>
            <p
              class="mt-1 text-xs text-gray-500 dark:text-gray-400"
              data-test="batch-add-pending-notice"
            >
              {{ t("assignedAccounts.admin.batchAddPendingNotice") }}
            </p>
          </div>

          <ul
            v-if="visibleCandidates.length > 0"
            class="mt-2 space-y-1"
            data-test="candidate-list"
          >
            <li
              v-for="candidate in visibleCandidates"
              :key="candidate.id"
              class="flex items-center justify-between gap-3 rounded-lg px-3 py-2 hover:bg-gray-50 dark:hover:bg-dark-700"
              :data-test="`candidate-${candidate.id}`"
            >
              <div class="flex min-w-0 items-center gap-3">
                <input
                  type="checkbox"
                  class="h-4 w-4 shrink-0 cursor-pointer rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-500"
                  :checked="isCandidateSelected(candidate.id)"
                  :aria-label="
                    t('assignedAccounts.admin.selectCandidate', {
                      name: candidate.name || `#${candidate.id}`,
                    })
                  "
                  :data-test="`candidate-select-${candidate.id}`"
                  @change="toggleCandidateSelection(candidate.id)"
                />
                <div class="min-w-0">
                  <div class="flex items-center gap-2">
                    <p
                      class="truncate text-sm text-gray-800 dark:text-gray-200"
                    >
                      {{ candidate.name || `#${candidate.id}` }}
                    </p>
                    <!-- 停用或异常账号照常可选，这里只把状态显示得更醒目 -->
                    <span
                      v-if="candidate.status && candidate.status !== 'active'"
                      class="shrink-0 rounded-full bg-gray-100 px-2 py-0.5 text-[10px] font-medium text-gray-600 dark:bg-dark-700 dark:text-gray-300"
                      :data-test="`candidate-status-${candidate.id}`"
                    >
                      {{ statusLabel(candidate.status) }}
                    </span>
                  </div>
                  <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">
                    {{ candidateMeta(candidate) }}
                  </p>
                </div>
              </div>
              <button
                type="button"
                class="text-sm font-medium text-primary-600 hover:underline dark:text-primary-400"
                :data-test="`add-${candidate.id}`"
                @click="addAccount(candidate)"
              >
                {{ t("assignedAccounts.admin.add") }}
              </button>
            </li>
          </ul>

          <p
            v-else-if="candidatesLoading"
            class="mt-2 px-3 py-2 text-xs text-gray-400 dark:text-dark-500"
          >
            {{ t("common.loading") }}
          </p>
          <p
            v-else
            class="mt-2 px-3 py-2 text-xs text-gray-400 dark:text-dark-500"
            data-test="no-candidates"
          >
            {{
              candidatesFailed
                ? t("assignedAccounts.admin.loadCandidatesFailed")
                : t("assignedAccounts.admin.noCandidates")
            }}
          </p>

          <button
            v-if="hasMoreCandidates"
            type="button"
            class="mt-2 text-sm font-medium text-primary-600 hover:underline dark:text-primary-400"
            :disabled="candidatesLoading"
            data-test="load-more-candidates"
            @click="loadMoreCandidates"
          >
            {{ t("assignedAccounts.admin.loadMore") }}
          </button>
        </div>
      </div>
    </div>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button @click="$emit('close')" class="btn btn-secondary px-5">
          {{ t("common.cancel") }}
        </button>
        <button
          @click="handleSave"
          :disabled="!canSave"
          class="btn btn-primary px-6"
          data-test="save-grant"
        >
          {{ submitting ? t("common.saving") : t("common.save") }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { adminAPI } from "@/api/admin";
import type { AdminUser, AdminGroup, PaginatedResponse } from "@/types";
import type { AccountOptionItem } from "@/api/admin/accounts";
import BaseDialog from "@/components/common/BaseDialog.vue";
import LoadingSpinner from "@/components/common/LoadingSpinner.vue";
import { useAppStore } from "@/stores/app";
import { useKeyedDebouncedSearch } from "@/composables/useKeyedDebouncedSearch";
import { useTableSelection } from "@/composables/useTableSelection";
import {
  extractApiErrorMessage,
  extractApiErrorMetadata,
} from "@/utils/apiError";
import { CONCRETE_PLATFORM_OPTIONS } from "@/constants/platforms";
import { ACCOUNT_TYPE_OPTIONS } from "@/constants/accountTypes";

interface GrantAccount {
  id: number;
  name: string;
  platform: string;
  account_type: string;
  status: string;
}

type AccountOption = AccountOptionItem;

const CANDIDATE_KEY = "accounts";
const CANDIDATE_PAGE_SIZE = 20;

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
const assigned = ref<GrantAccount[]>([]);

const search = ref("");
const candidatePlatform = ref("");
const candidateType = ref("");
const candidateStatus = ref("");
const candidateGroup = ref("");
const candidateGroups = ref<AdminGroup[]>([]);
const groupsFailed = ref(false);
const candidates = ref<AccountOption[]>([]);
const candidatePage = ref(1);
const candidateTotal = ref(0);
const candidatesLoading = ref(false);
const candidatesFailed = ref(false);

/**
 * 当前打开的授权请求代次。切换用户或重新打开时递增，晚到的响应一律丢弃，
 * 避免 A 用户的响应覆盖 B 用户的界面状态。保存也据此判断回显是否属于当前会话。
 */
let requestGeneration = 0;
/**
 * 候选列表的搜索代次。搜索词变化或重新打开时递增，
 * 使旧搜索词的第 2 页响应无法追加到新搜索的候选上、也无法改写页数与总数。
 */
let candidateGeneration = 0;
/** 只作为详情缓存：account_ids 是权威集合，缺少详情的 id 仍要保留。 */
const accountDetails = new Map<number, GrantAccount>();
/**
 * 候选账号详情缓存：候选列表会随搜索与筛选整页替换，这里保留本次会话见过的账号资料，
 * 使跨筛选、跨分页仍被选中的账号在一键加入时还能还原名称与状态。
 */
const candidateDetails = new Map<number, GrantAccount>();
/** 最近一次「一键加入」的合并结果；只描述待授权列表，不代表授权已生效。 */
const lastBatchAddResult = ref<{
  added: number;
  skipped: number;
  failed: number;
} | null>(null);
let loadMoreController: AbortController | null = null;

/** 保存必须建立在「当前用户已成功读取授权」之上，读取失败或进行中一律禁用。 */
const canSave = computed(
  () =>
    props.user !== null &&
    !submitting.value &&
    !loading.value &&
    !loadFailed.value &&
    loadedUserId.value === props.user?.id,
);

const assignedIds = computed(
  () => new Set(assigned.value.map((account) => account.id)),
);

/**
 * 服务端上一次保存点名的失效账号 id。只用于就地标出失败项，不改变草稿内容：
 * 整批回滚后待授权列表与勾选原样保留，管理员据此移除或替换后再保存。
 */
const invalidPendingIds = ref<number[]>([]);
const invalidPendingIdSet = computed(() => new Set(invalidPendingIds.value));
/** 仍留在待授权列表里的失败项：已被移除的项不再提示。 */
const pendingInvalidIds = computed(() =>
  invalidPendingIds.value.filter((id) => assignedIds.value.has(id)),
);

const visibleCandidates = computed(() =>
  candidates.value.filter((candidate) => !assignedIds.value.has(candidate.id)),
);

/**
 * 候选账号的多选状态。列表是普通 <ul> 而不是 DataTable，但选择语义与表格批量操作完全相同，
 * 因此直接复用 useTableSelection（它只依赖行集合与取 id 的函数，不绑定表格组件）。
 * 选择集只属于当前弹窗会话：切换用户、关闭或重新打开都会清空。
 *
 * 「全选」作用于当前列表里已加载的候选账号：候选使用「加载更多」而非分页器，
 * 所以这里的“当前页”就是当前已列出的候选。
 */
const {
  selectedIds: selectedCandidateIds,
  selectedCount,
  allVisibleSelected,
  isSelected: isCandidateSelected,
  toggle: toggleCandidateSelection,
  clear: clearCandidateSelection,
  removeMany: removeCandidatesFromSelection,
  toggleVisible: toggleVisibleCandidates,
} = useTableSelection<AccountOption>({
  rows: visibleCandidates,
  getId: (candidate) => candidate.id,
});

const hasMoreCandidates = computed(
  () =>
    !candidatesLoading.value && candidates.value.length < candidateTotal.value,
);

/**
 * 账号状态沿用管理端账号表的状态词表。
 * 'disabled' 是历史取值，与当前编辑器的 'inactive' 同为「手动停用」，展示同一标签；
 * error／expired／临时不可调度等非手动禁用状态照常展示，授权列表不据此隐藏任何账号。
 */
const ACCOUNT_STATUS_LABEL_KEYS: Record<string, string> = {
  active: "active",
  disabled: "inactive",
  inactive: "inactive",
  error: "error",
  expired: "expired",
  cooldown: "cooldown",
  paused: "paused",
  limited: "limited",
  rate_limited: "rateLimited",
  overloaded: "overloaded",
  temp_unschedulable: "tempUnschedulable",
  quota_exceeded: "quotaExceeded",
  unschedulable: "unschedulable",
};

function statusLabel(status: string): string {
  const key = ACCOUNT_STATUS_LABEL_KEYS[status];
  return key ? t(`admin.accounts.status.${key}`) : status;
}

function accountLabel(account: GrantAccount): string {
  return account.name.trim() !== "" ? account.name : `#${account.id}`;
}

function accountMeta(account: GrantAccount): string {
  const parts = [
    account.platform,
    account.account_type,
    account.status ? statusLabel(account.status) : "",
  ].filter((part) => part !== "");
  return parts.length > 0
    ? parts.join(" · ")
    : t("assignedAccounts.admin.unknownAccount");
}

function candidateMeta(candidate: AccountOption): string {
  const parts = [
    candidate.platform,
    candidate.type,
    candidate.status ? statusLabel(candidate.status) : "",
  ].filter((part) => part !== "");
  return parts.length > 0
    ? parts.join(" · ")
    : t("assignedAccounts.admin.unknownAccount");
}

function toGrantAccount(account: {
  id: number;
  name: string;
  platform: string;
  account_type: string;
  status: string;
}): GrantAccount {
  return {
    id: account.id,
    name: account.name ?? "",
    platform: account.platform ?? "",
    account_type: account.account_type ?? "",
    status: account.status ?? "",
  };
}

function toAccountOption(account: AccountOptionItem): AccountOption {
  return {
    id: account.id,
    name: account.name ?? "",
    platform: account.platform ?? "",
    type: account.type ?? "",
    status: account.status ?? "",
  };
}

/** 候选列表每次只持有当前筛选的结果，这里累积详情供跨页、跨筛选的一键加入使用。 */
function cacheCandidateDetails(items: AccountOption[]): void {
  for (const item of items) {
    const account = toGrantAccount({
      id: item.id,
      name: item.name,
      platform: item.platform,
      account_type: item.type,
      status: item.status,
    });
    candidateDetails.set(account.id, account);
  }
}

/**
 * 还原被选中账号的资料：优先用本次会话的详情缓存，其次用当前候选列表。
 * 都取不到时返回 null（该账号无法加入待授权列表，会在结果里如实报告）。
 */
function resolveCandidateAccount(id: number): GrantAccount | null {
  const cached = candidateDetails.get(id);
  if (cached) {
    return cached;
  }

  const listed = candidates.value.find((candidate) => candidate.id === id);
  if (!listed) {
    return null;
  }

  return toGrantAccount({
    id: listed.id,
    name: listed.name,
    platform: listed.platform,
    account_type: listed.type,
    status: listed.status,
  });
}

/**
 * 以后端 account_ids 为权威集合解析已分配账号：
 * - 响应里带 account_ids（即使是空数组）→ 以它为准，因此撤销到零不会被旧状态复活；
 * - 只带 accounts → 用 accounts 的 id；
 * - 两者都缺失 → 沿用调用方提供的兜底 id（不会静默丢授权）。
 * 缺少详情的 id 保留为占位条目，保存时仍会带上。
 */
function resolveAssignedIds(
  grant: { account_ids?: number[]; accounts?: { id: number }[] },
  fallbackIds: number[],
): number[] {
  if (Array.isArray(grant.account_ids)) {
    return grant.account_ids;
  }
  if (Array.isArray(grant.accounts)) {
    return grant.accounts.map((account) => account.id);
  }
  return fallbackIds;
}

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
    accountDetails.set(account.id, toGrantAccount(account));
  }

  enabled.value = grant.enabled === true;
  assigned.value = resolveAssignedIds(grant, fallbackIds).map(
    (id) =>
      accountDetails.get(id) ??
      toGrantAccount({
        id,
        name: "",
        platform: "",
        account_type: "",
        status: "",
      }),
  );
}

function stopCandidateRequests(): void {
  loadMoreController?.abort();
  loadMoreController = null;
  candidateSearch.clearKey(CANDIDATE_KEY);
  candidatesLoading.value = false;
}

/** 每个用户/每次打开都从干净状态开始：GET 成功前不展示、也不允许沿用任何旧授权。 */
function resetForNewUser(): void {
  // 代次同时作废在途的读取、保存回显与候选分页。
  requestGeneration += 1;
  candidateGeneration += 1;
  loadedUserId.value = null;
  loadFailed.value = false;
  loadMoreController?.abort();
  loadMoreController = null;
  enabled.value = false;
  assigned.value = [];
  accountDetails.clear();
  candidateDetails.clear();
  lastBatchAddResult.value = null;
  invalidPendingIds.value = [];
  clearCandidateSelection();
  search.value = "";
  candidatePlatform.value = "";
  candidateType.value = "";
  candidateStatus.value = "";
  candidateGroup.value = "";
  candidateGroups.value = [];
  groupsFailed.value = false;
  candidateSearch.clearKey(CANDIDATE_KEY);
  candidates.value = [];
  candidatePage.value = 1;
  candidateTotal.value = 0;
  candidatesLoading.value = false;
  candidatesFailed.value = false;
  // 上一次会话的保存属于旧代次，不能让它继续禁用新会话的保存按钮。
  submitting.value = false;
}

function candidateFilters(keyword: string): {
  platform?: string;
  type?: string;
  status?: string;
  group?: string;
  search?: string;
} {
  const filters: {
    platform?: string;
    type?: string;
    status?: string;
    group?: string;
    search?: string;
  } = {};
  if (keyword.trim()) filters.search = keyword.trim();
  if (candidatePlatform.value) filters.platform = candidatePlatform.value;
  if (candidateType.value) filters.type = candidateType.value;
  if (candidateStatus.value) filters.status = candidateStatus.value;
  if (!groupsFailed.value && candidateGroup.value)
    filters.group = candidateGroup.value;
  return filters;
}

const candidateSearch = useKeyedDebouncedSearch<
  PaginatedResponse<AccountOptionItem>
>({
  delay: 300,
  search: (keyword, context) =>
    adminAPI.accounts.listOptions(
      1,
      CANDIDATE_PAGE_SIZE,
      candidateFilters(keyword),
      { signal: context.signal },
    ),
  onSuccess: (_key, page) => {
    const items = page.items.map(toAccountOption);
    cacheCandidateDetails(items);
    candidates.value = items;
    candidatePage.value = 1;
    candidateTotal.value = page.total;
    candidatesFailed.value = false;
    candidatesLoading.value = false;
  },
  onError: () => {
    candidates.value = [];
    candidateTotal.value = 0;
    candidatesFailed.value = true;
    candidatesLoading.value = false;
  },
});

function handleCandidateFilterChange(): void {
  // All filters invalidate page-two requests, not just the keyword.
  candidateGeneration += 1;
  loadMoreController?.abort();
  loadMoreController = null;
  candidates.value = [];
  candidatePage.value = 1;
  candidateTotal.value = 0;
  candidatesLoading.value = true;
  candidatesFailed.value = false;
  candidateSearch.trigger(CANDIDATE_KEY, search.value);
}

function handleSearchInput(): void {
  handleCandidateFilterChange();
}

/** 追加下一页候选账号（服务端分页），使授权不再受首屏数量限制。 */
async function loadMoreCandidates(): Promise<void> {
  const filters = candidateFilters(search.value);

  loadMoreController?.abort();
  const controller = new AbortController();
  loadMoreController = controller;
  const nextPage = candidatePage.value + 1;
  // 响应只有仍属于当前搜索代次时才允许写入，避免旧搜索词的分页污染新结果。
  const generation = candidateGeneration;
  const isCurrent = () =>
    !controller.signal.aborted && generation === candidateGeneration;
  candidatesLoading.value = true;

  try {
    const page = await adminAPI.accounts.listOptions(
      nextPage,
      CANDIDATE_PAGE_SIZE,
      filters,
      {
        signal: controller.signal,
      },
    );
    if (!isCurrent()) return;

    const known = new Set(candidates.value.map((candidate) => candidate.id));
    const items = page.items.map(toAccountOption);
    cacheCandidateDetails(items);
    candidates.value = [
      ...candidates.value,
      ...items.filter((candidate) => !known.has(candidate.id)),
    ];
    candidatePage.value = nextPage;
    candidateTotal.value = page.total;
    candidatesFailed.value = false;
  } catch (error) {
    if (!isCurrent()) return;
    candidatesFailed.value = true;
  } finally {
    // 已被新搜索作废的请求不得清除新搜索的加载状态。
    if (isCurrent()) {
      candidatesLoading.value = false;
    }
  }
}

function addAccount(option: AccountOption): void {
  if (assigned.value.some((account) => account.id === option.id)) {
    return;
  }

  const grantAccount = toGrantAccount({
    id: option.id,
    name: option.name,
    platform: option.platform,
    account_type: option.type,
    status: option.status,
  });
  accountDetails.set(grantAccount.id, grantAccount);
  assigned.value = [...assigned.value, grantAccount];
  // 逐行添加改动了待授权列表，上一次「一键添加」的计数不再描述当前草稿。
  lastBatchAddResult.value = null;
}

function removeAccount(id: number): void {
  assigned.value = assigned.value.filter((account) => account.id !== id);
  lastBatchAddResult.value = null;
}

/**
 * 从保存失败响应里取出服务端点名的失效账号 id。
 *
 * 只认服务端约定的 `invalid_account_ids`（逗号分隔的十进制 id，有界）：字段缺失或
 * 形状不符时返回空数组，界面退回通用失败提示，绝不凭客户端猜测标记任何账号。
 * 名字与状态只有服务端返回详情时才有，失败项一律只用 id 表达。
 */
function invalidAccountIdsFromError(error: unknown): number[] {
  const raw = extractApiErrorMetadata(error)?.invalid_account_ids;
  if (typeof raw !== "string") {
    return [];
  }
  const ids = raw
    .split(",")
    .map((part) => Number(part.trim()))
    .filter((id) => Number.isSafeInteger(id) && id > 0);
  return Array.from(new Set(ids));
}

function handleToggleSelectVisible(event: Event): void {
  toggleVisibleCandidates((event.target as HTMLInputElement).checked);
}

/**
 * 一键加入待授权列表：只把选中的候选合并进本地待授权集合，不发起任何写入。
 * 授权仍然只在「保存」时通过一次全量替换提交，因此这里只报告合并结果。
 * 选择只在本弹窗会话内有效：已加入与已跳过的从选择中移除，取不到资料的保留选中以便修正重试。
 */
function addSelectedAccounts(): void {
  if (selectedCount.value === 0) {
    return;
  }

  const added: GrantAccount[] = [];
  const addedIds: number[] = [];
  const skippedIds: number[] = [];
  const failedIds: number[] = [];

  for (const id of selectedCandidateIds.value) {
    if (assignedIds.value.has(id)) {
      skippedIds.push(id);
      continue;
    }

    const account = resolveCandidateAccount(id);
    if (!account) {
      failedIds.push(id);
      continue;
    }

    accountDetails.set(account.id, account);
    added.push(account);
    addedIds.push(account.id);
  }

  if (added.length > 0) {
    assigned.value = [...assigned.value, ...added];
  }

  lastBatchAddResult.value = {
    added: added.length,
    skipped: skippedIds.length,
    failed: failedIds.length,
  };
  removeCandidatesFromSelection([...addedIds, ...skippedIds]);
}

watch(
  () => [props.show, props.user?.id] as const,
  ([visible]) => {
    if (visible && props.user) {
      void load();
      return;
    }
    // 关闭即结束本次选择会话：重新打开时不沿用上一次的选择与合并结果。
    clearCandidateSelection();
    lastBatchAddResult.value = null;
  },
  { immediate: true },
);

async function load(): Promise<void> {
  const user = props.user;
  if (!user) {
    return;
  }

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
    stopCandidateRequests();
    appStore.showError(
      extractApiErrorMessage(error, t("assignedAccounts.admin.loadFailed")),
    );
    return;
  } finally {
    if (generation === requestGeneration) {
      loading.value = false;
    }
  }

  if (generation !== requestGeneration) return;
  candidatesLoading.value = true;
  candidateSearch.trigger(CANDIDATE_KEY, "");
  try {
    const groups = await adminAPI.groups.getAll();
    if (generation !== requestGeneration) return;
    candidateGroups.value = groups;
  } catch {
    if (generation !== requestGeneration) return;
    groupsFailed.value = true;
    candidateGroup.value = "";
  }
}

/**
 * 保存回显只在「发起保存的那次会话仍然有效」时生效：
 * 弹窗仍打开、仍是同一个用户、该用户的授权已成功读取，且期间没有被关闭重开。
 * 仅比较 userId 无法区分「关闭后又为同一用户重新打开」的情形。
 */
function isSameSaveSession(userId: number, generation: number): boolean {
  return (
    props.show &&
    props.user?.id === userId &&
    loadedUserId.value === userId &&
    generation === requestGeneration
  );
}

async function handleSave(): Promise<void> {
  if (!props.user || !canSave.value) {
    return;
  }

  const userId = props.user.id;
  const submittedIds = assigned.value.map((account) => account.id);
  const generation = requestGeneration;
  submitting.value = true;

  try {
    // 全量替换：提交的列表就是完整授权集合，撤销立即生效。
    const grant = await adminAPI.users.updateAccountView(userId, {
      enabled: enabled.value,
      account_ids: submittedIds,
    });
    if (!isSameSaveSession(userId, generation)) {
      return;
    }
    invalidPendingIds.value = [];
    // 响应缺少账号详情时沿用已有详情与提交的 id，避免界面把授权显示成空。
    applyGrant(grant, submittedIds);
    appStore.showSuccess(t("assignedAccounts.admin.saveSuccess"));
    emit("success");
    emit("close");
  } catch (error) {
    if (!isSameSaveSession(userId, generation)) {
      return;
    }
    // 整批回滚：草稿、勾选与开关都保持原样，只把服务端点名的失效项标出来供修正。
    invalidPendingIds.value = invalidAccountIdsFromError(error);
    appStore.showError(
      extractApiErrorMessage(error, t("assignedAccounts.admin.saveFailed")),
    );
  } finally {
    // 旧会话的保存不得解除新会话的保存中状态。
    if (generation === requestGeneration) {
      submitting.value = false;
    }
  }
}
</script>
