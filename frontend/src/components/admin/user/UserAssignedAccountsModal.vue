<template>
  <BaseDialog :show="show" :title="t('assignedAccounts.admin.title')" width="wide" @close="$emit('close')">
    <div v-if="user" class="space-y-6">
      <div class="rounded-2xl bg-gradient-to-r from-primary-50 to-primary-100 p-5 dark:from-primary-900/30 dark:to-primary-800/20">
        <p class="text-lg font-semibold text-gray-900 dark:text-white">{{ user.email }}</p>
        <p class="mt-1 text-sm text-gray-600 dark:text-gray-400">
          {{ t('assignedAccounts.admin.hint', { email: user.email }) }}
        </p>
      </div>

      <div v-if="loading" class="flex justify-center py-12">
        <LoadingSpinner />
      </div>

      <!-- 读取失败时不显示任何授权状态，也不允许保存（fail-closed）。 -->
      <div v-else-if="loadFailed" class="space-y-3 rounded-xl border border-red-200 px-4 py-6 text-center dark:border-red-900/40">
        <p class="text-sm text-gray-600 dark:text-gray-300">{{ t('assignedAccounts.admin.loadFailed') }}</p>
        <button type="button" class="btn btn-secondary px-4" data-test="retry-load" @click="load()">
          {{ t('assignedAccounts.retry') }}
        </button>
      </div>

      <div v-else class="space-y-6">
        <!-- 能力开关：默认关闭，关闭时不展示任何账号 -->
        <label class="flex cursor-pointer items-center gap-3 rounded-xl border border-gray-200 p-4 dark:border-dark-600">
          <input
            type="checkbox"
            class="h-5 w-5 rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-500"
            :checked="enabled"
            data-test="grant-enabled"
            @change="enabled = ($event.target as HTMLInputElement).checked"
          />
          <span class="text-sm font-medium text-gray-800 dark:text-gray-200">
            {{ t('assignedAccounts.admin.enableLabel') }}
          </span>
        </label>

        <!-- 已分配账号：以后端返回的 account_ids 为准，缺少详情的账号保留为占位条目 -->
        <div>
          <div class="mb-3 flex items-center gap-2">
            <div class="h-1.5 w-1.5 rounded-full bg-primary-500"></div>
            <h4 class="text-sm font-semibold text-gray-700 dark:text-gray-300">
              {{ t('assignedAccounts.admin.assignedTitle') }}
            </h4>
            <span class="text-xs text-gray-400" data-test="assigned-count">
              {{ t('assignedAccounts.admin.assignedCount', { count: assigned.length }) }}
            </span>
          </div>

          <p v-if="assigned.length === 0" class="rounded-xl border border-dashed border-gray-200 px-4 py-6 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400" data-test="no-assigned">
            {{ t('assignedAccounts.admin.noAssigned') }}
          </p>

          <ul v-else class="space-y-2">
            <li
              v-for="account in assigned"
              :key="account.id"
              class="flex items-center justify-between gap-3 rounded-xl border border-gray-200 px-4 py-3 dark:border-dark-600"
              :data-test="`assigned-${account.id}`"
            >
              <div class="min-w-0">
                <p class="truncate text-sm font-medium text-gray-900 dark:text-white">{{ accountLabel(account) }}</p>
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
                {{ t('assignedAccounts.admin.remove') }}
              </button>
            </li>
          </ul>
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

          <ul v-if="visibleCandidates.length > 0" class="mt-2 space-y-1" data-test="candidate-list">
            <li
              v-for="candidate in visibleCandidates"
              :key="candidate.id"
              class="flex items-center justify-between gap-3 rounded-lg px-3 py-2 hover:bg-gray-50 dark:hover:bg-dark-700"
              :data-test="`candidate-${candidate.id}`"
            >
              <div class="min-w-0">
                <p class="truncate text-sm text-gray-800 dark:text-gray-200">{{ candidate.name || `#${candidate.id}` }}</p>
                <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">
                  {{ candidateMeta(candidate) }}
                </p>
              </div>
              <button
                type="button"
                class="text-sm font-medium text-primary-600 hover:underline dark:text-primary-400"
                :data-test="`add-${candidate.id}`"
                @click="addAccount(candidate)"
              >
                {{ t('assignedAccounts.admin.add') }}
              </button>
            </li>
          </ul>

          <p v-else-if="candidatesLoading" class="mt-2 px-3 py-2 text-xs text-gray-400 dark:text-dark-500">
            {{ t('common.loading') }}
          </p>
          <p v-else class="mt-2 px-3 py-2 text-xs text-gray-400 dark:text-dark-500" data-test="no-candidates">
            {{ candidatesFailed ? t('assignedAccounts.admin.loadCandidatesFailed') : t('assignedAccounts.admin.noCandidates') }}
          </p>

          <button
            v-if="hasMoreCandidates"
            type="button"
            class="mt-2 text-sm font-medium text-primary-600 hover:underline dark:text-primary-400"
            :disabled="candidatesLoading"
            data-test="load-more-candidates"
            @click="loadMoreCandidates"
          >
            {{ t('assignedAccounts.admin.loadMore') }}
          </button>
        </div>
      </div>
    </div>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button @click="$emit('close')" class="btn btn-secondary px-5">{{ t('common.cancel') }}</button>
        <button
          @click="handleSave"
          :disabled="!canSave"
          class="btn btn-primary px-6"
          data-test="save-grant"
        >
          {{ submitting ? t('common.saving') : t('common.save') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { AdminUser, AccountListItem, PaginatedResponse } from '@/types'
import BaseDialog from '@/components/common/BaseDialog.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import { useAppStore } from '@/stores/app'
import { useKeyedDebouncedSearch } from '@/composables/useKeyedDebouncedSearch'
import { extractApiErrorMessage } from '@/utils/apiError'

interface GrantAccount {
  id: number
  name: string
  platform: string
  account_type: string
  status: string
}

interface AccountOption {
  id: number
  name: string
  platform: string
  type: string
  status: string
}

const CANDIDATE_KEY = 'accounts'
const CANDIDATE_PAGE_SIZE = 20

const props = defineProps<{ show: boolean; user: AdminUser | null }>()
const emit = defineEmits(['close', 'success'])
const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const loadFailed = ref(false)
const submitting = ref(false)
/** 已成功加载授权的用户；只有它与当前用户一致时才允许保存。 */
const loadedUserId = ref<number | null>(null)
const enabled = ref(false)
const assigned = ref<GrantAccount[]>([])

const search = ref('')
const candidates = ref<AccountOption[]>([])
const candidatePage = ref(1)
const candidateTotal = ref(0)
const candidatesLoading = ref(false)
const candidatesFailed = ref(false)

/**
 * 当前打开的授权请求代次。切换用户或重新打开时递增，晚到的响应一律丢弃，
 * 避免 A 用户的响应覆盖 B 用户的界面状态。
 */
let requestGeneration = 0
/** 只作为详情缓存：account_ids 是权威集合，缺少详情的 id 仍要保留。 */
const accountDetails = new Map<number, GrantAccount>()
let loadMoreController: AbortController | null = null

/** 保存必须建立在「当前用户已成功读取授权」之上，读取失败或进行中一律禁用。 */
const canSave = computed(
  () =>
    props.user !== null &&
    !submitting.value &&
    !loading.value &&
    !loadFailed.value &&
    loadedUserId.value === props.user?.id
)

const assignedIds = computed(() => new Set(assigned.value.map((account) => account.id)))

const visibleCandidates = computed(() =>
  candidates.value.filter((candidate) => !assignedIds.value.has(candidate.id))
)

const hasMoreCandidates = computed(
  () => !candidatesLoading.value && candidates.value.length < candidateTotal.value
)

/**
 * 账号状态沿用管理端账号表的状态词表。
 * 'disabled' 是历史取值，与当前编辑器的 'inactive' 同为「手动停用」，展示同一标签；
 * error／expired／临时不可调度等非手动禁用状态照常展示，授权列表不据此隐藏任何账号。
 */
const ACCOUNT_STATUS_LABEL_KEYS: Record<string, string> = {
  active: 'active',
  disabled: 'inactive',
  inactive: 'inactive',
  error: 'error',
  expired: 'expired',
  cooldown: 'cooldown',
  paused: 'paused',
  limited: 'limited',
  rate_limited: 'rateLimited',
  overloaded: 'overloaded',
  temp_unschedulable: 'tempUnschedulable',
  quota_exceeded: 'quotaExceeded',
  unschedulable: 'unschedulable'
}

function statusLabel(status: string): string {
  const key = ACCOUNT_STATUS_LABEL_KEYS[status]
  return key ? t(`admin.accounts.status.${key}`) : status
}

function accountLabel(account: GrantAccount): string {
  return account.name.trim() !== '' ? account.name : `#${account.id}`
}

function accountMeta(account: GrantAccount): string {
  const parts = [account.platform, account.account_type, account.status ? statusLabel(account.status) : '']
    .filter((part) => part !== '')
  return parts.length > 0 ? parts.join(' · ') : t('assignedAccounts.admin.unknownAccount')
}

function candidateMeta(candidate: AccountOption): string {
  const parts = [candidate.platform, candidate.type, candidate.status ? statusLabel(candidate.status) : '']
    .filter((part) => part !== '')
  return parts.length > 0 ? parts.join(' · ') : t('assignedAccounts.admin.unknownAccount')
}

function toGrantAccount(account: {
  id: number
  name: string
  platform: string
  account_type: string
  status: string
}): GrantAccount {
  return {
    id: account.id,
    name: account.name ?? '',
    platform: account.platform ?? '',
    account_type: account.account_type ?? '',
    status: account.status ?? ''
  }
}

function toAccountOption(account: AccountListItem): AccountOption {
  return {
    id: account.id,
    name: account.name ?? '',
    platform: account.platform ?? '',
    type: account.type ?? '',
    status: account.status ?? ''
  }
}

/**
 * 以后端 account_ids 为权威集合解析已分配账号：
 * - 响应里带 account_ids（即使是空数组）→ 以它为准，因此撤销到零不会被旧状态复活；
 * - 只带 accounts → 用 accounts 的 id；
 * - 两者都缺失 → 沿用调用方提供的兜底 id（不会静默丢授权）。
 * 缺少详情的 id 保留为占位条目，保存时仍会带上。
 */
function resolveAssignedIds(grant: { account_ids?: number[]; accounts?: { id: number }[] }, fallbackIds: number[]): number[] {
  if (Array.isArray(grant.account_ids)) {
    return grant.account_ids
  }
  if (Array.isArray(grant.accounts)) {
    return grant.accounts.map((account) => account.id)
  }
  return fallbackIds
}

function applyGrant(
  grant: { enabled?: boolean; account_ids?: number[]; accounts?: { id: number; name: string; platform: string; account_type: string; status: string }[] },
  fallbackIds: number[] = []
): void {
  for (const account of grant.accounts ?? []) {
    accountDetails.set(account.id, toGrantAccount(account))
  }

  enabled.value = grant.enabled === true
  assigned.value = resolveAssignedIds(grant, fallbackIds).map(
    (id) => accountDetails.get(id) ?? toGrantAccount({ id, name: '', platform: '', account_type: '', status: '' })
  )
}

function stopCandidateRequests(): void {
  loadMoreController?.abort()
  loadMoreController = null
  candidateSearch.clearKey(CANDIDATE_KEY)
  candidatesLoading.value = false
}

/** 每个用户/每次打开都从干净状态开始：GET 成功前不展示、也不允许沿用任何旧授权。 */
function resetForNewUser(): void {
  requestGeneration += 1
  loadedUserId.value = null
  loadFailed.value = false
  loadMoreController?.abort()
  loadMoreController = null
  enabled.value = false
  assigned.value = []
  accountDetails.clear()
  search.value = ''
  candidateSearch.clearKey(CANDIDATE_KEY)
  candidates.value = []
  candidatePage.value = 1
  candidateTotal.value = 0
  candidatesLoading.value = false
  candidatesFailed.value = false
}

const candidateSearch = useKeyedDebouncedSearch<PaginatedResponse<AccountListItem>>({
  delay: 300,
  search: (keyword, context) => {
    const filters: { lite: string; search?: string } = { lite: '1' }
    if (keyword.trim() !== '') {
      filters.search = keyword.trim()
    }
    return adminAPI.accounts.list(1, CANDIDATE_PAGE_SIZE, filters, { signal: context.signal })
  },
  onSuccess: (_key, page) => {
    candidates.value = page.items.map(toAccountOption)
    candidatePage.value = 1
    candidateTotal.value = page.total
    candidatesFailed.value = false
    candidatesLoading.value = false
  },
  onError: () => {
    candidates.value = []
    candidateTotal.value = 0
    candidatesFailed.value = true
    candidatesLoading.value = false
  }
})

function handleSearchInput(): void {
  candidatesLoading.value = true
  candidatesFailed.value = false
  candidateSearch.trigger(CANDIDATE_KEY, search.value)
}

/** 追加下一页候选账号（服务端分页），使授权不再受首屏数量限制。 */
async function loadMoreCandidates(): Promise<void> {
  const keyword = search.value.trim()
  const filters: { lite: string; search?: string } = { lite: '1' }
  if (keyword !== '') {
    filters.search = keyword
  }

  loadMoreController?.abort()
  const controller = new AbortController()
  loadMoreController = controller
  const nextPage = candidatePage.value + 1
  candidatesLoading.value = true

  try {
    const page = await adminAPI.accounts.list(nextPage, CANDIDATE_PAGE_SIZE, filters, {
      signal: controller.signal
    })
    if (controller.signal.aborted) return

    const known = new Set(candidates.value.map((candidate) => candidate.id))
    candidates.value = [
      ...candidates.value,
      ...page.items.map(toAccountOption).filter((candidate) => !known.has(candidate.id))
    ]
    candidatePage.value = nextPage
    candidateTotal.value = page.total
    candidatesFailed.value = false
  } catch (error) {
    if (controller.signal.aborted) return
    candidatesFailed.value = true
  } finally {
    if (!controller.signal.aborted) {
      candidatesLoading.value = false
    }
  }
}

function addAccount(option: AccountOption): void {
  if (assigned.value.some((account) => account.id === option.id)) {
    return
  }

  const grantAccount = toGrantAccount({
    id: option.id,
    name: option.name,
    platform: option.platform,
    account_type: option.type,
    status: option.status
  })
  accountDetails.set(grantAccount.id, grantAccount)
  assigned.value = [...assigned.value, grantAccount]
}

function removeAccount(id: number): void {
  assigned.value = assigned.value.filter((account) => account.id !== id)
}

watch(
  () => [props.show, props.user?.id] as const,
  ([visible]) => {
    if (visible && props.user) {
      void load()
    }
  },
  { immediate: true }
)

async function load(): Promise<void> {
  const user = props.user
  if (!user) {
    return
  }

  resetForNewUser()
  const generation = requestGeneration
  loading.value = true

  try {
    const grant = await adminAPI.users.getAccountView(user.id)
    if (generation !== requestGeneration) return
    applyGrant(grant)
    loadedUserId.value = user.id
  } catch (error) {
    if (generation !== requestGeneration) return
    loadFailed.value = true
    stopCandidateRequests()
    appStore.showError(extractApiErrorMessage(error, t('assignedAccounts.admin.loadFailed')))
    return
  } finally {
    if (generation === requestGeneration) {
      loading.value = false
    }
  }

  if (generation !== requestGeneration) return
  candidatesLoading.value = true
  candidateSearch.trigger(CANDIDATE_KEY, '')
}

async function handleSave(): Promise<void> {
  if (!props.user || !canSave.value) {
    return
  }

  const userId = props.user.id
  const submittedIds = assigned.value.map((account) => account.id)
  submitting.value = true

  try {
    // 全量替换：提交的列表就是完整授权集合，撤销立即生效。
    const grant = await adminAPI.users.updateAccountView(userId, {
      enabled: enabled.value,
      account_ids: submittedIds
    })
    if (userId !== props.user?.id) {
      return
    }
    // 响应缺少账号详情时沿用已有详情与提交的 id，避免界面把授权显示成空。
    applyGrant(grant, submittedIds)
    appStore.showSuccess(t('assignedAccounts.admin.saveSuccess'))
    emit('success')
    emit('close')
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('assignedAccounts.admin.saveFailed')))
  } finally {
    submitting.value = false
  }
}
</script>
