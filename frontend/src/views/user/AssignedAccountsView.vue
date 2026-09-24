<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-col gap-3">
          <div class="flex flex-wrap items-center justify-between gap-3">
            <div>
              <h1 class="text-lg font-semibold text-gray-900 dark:text-white">
                {{ t('nav.assignedAccounts') }}
              </h1>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
                {{ t('assignedAccounts.description') }}
              </p>
            </div>
            <button
              @click="loadAccounts"
              :disabled="loading"
              class="btn btn-secondary"
              :title="t('common.refresh')"
              data-test="refresh-accounts"
            >
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>
          </div>
          <p class="rounded-lg bg-gray-50 px-3 py-2 text-xs text-gray-500 dark:bg-dark-800 dark:text-gray-400">
            {{ t('assignedAccounts.readOnlyNotice') }}
            {{ t('assignedAccounts.identityMaskedNotice') }}
          </p>
        </div>
      </template>

      <template #table>
        <DataTable :columns="columns" :data="accounts" :loading="loading">
          <template #cell-platform="{ value }">
            <span class="text-sm text-gray-900 dark:text-gray-100">{{ value }}</span>
          </template>

          <template #cell-account_type="{ value }">
            <span class="text-sm text-gray-900 dark:text-gray-100">{{ value }}</span>
          </template>

          <template #cell-email_masked="{ value }">
            <span class="text-sm text-gray-700 dark:text-gray-300" data-test="cell-email">{{
              displayIdentity(value)
            }}</span>
          </template>

          <template #cell-username_masked="{ value }">
            <span class="text-sm text-gray-700 dark:text-gray-300" data-test="cell-username">{{
              displayIdentity(value)
            }}</span>
          </template>

          <template #cell-upstream_account_id_masked="{ value }">
            <span class="text-sm text-gray-700 dark:text-gray-300" data-test="cell-upstream-id">{{
              displayIdentity(value)
            }}</span>
          </template>

          <template #cell-actions="{ row }">
            <button
              @click="openDetail(row)"
              class="flex items-center gap-1 rounded-lg px-2 py-1 text-sm text-primary-600 transition-colors hover:bg-primary-50 dark:text-primary-400 dark:hover:bg-primary-900/20"
              data-test="view-detail"
            >
              <Icon name="eye" size="sm" />
              <span>{{ t('assignedAccounts.viewDetail') }}</span>
            </button>
          </template>

          <template #empty>
            <EmptyState
              :title="t('assignedAccounts.empty')"
              :description="t('assignedAccounts.emptyHint')"
            />
          </template>
        </DataTable>
      </template>

      <template #pagination>
        <Pagination
          v-if="pagination.total > 0"
          :page="pagination.page"
          :total="pagination.total"
          :page-size="pagination.page_size"
          @update:page="handlePageChange"
          @update:pageSize="handlePageSizeChange"
        />
      </template>
    </TablePageLayout>

    <BaseDialog
      :show="showDetail"
      :title="t('assignedAccounts.detail.title')"
      @close="closeDetail"
    >
      <div v-if="detailLoading" class="flex justify-center py-10">
        <LoadingSpinner />
      </div>
      <p
        v-else-if="detailUnavailable"
        class="rounded-lg bg-gray-50 px-4 py-6 text-center text-sm text-gray-500 dark:bg-dark-800 dark:text-gray-400"
        data-test="detail-unavailable"
      >
        {{ t('assignedAccounts.detail.notVisible') }}
      </p>
      <!-- 网络/服务端故障不是「无权查看」，必须如实提示并提供重试。 -->
      <div
        v-else-if="detailFailed"
        class="space-y-3 rounded-lg bg-gray-50 px-4 py-6 text-center dark:bg-dark-800"
        data-test="detail-failed"
      >
        <p class="text-sm text-gray-600 dark:text-gray-300">
          {{ t('assignedAccounts.detail.loadFailed') }}
        </p>
        <button type="button" class="btn btn-secondary px-4" data-test="retry-detail" @click="retryDetail">
          {{ t('assignedAccounts.retry') }}
        </button>
      </div>
      <dl v-else-if="detail" class="space-y-3" data-test="detail-fields">
        <div class="flex items-start justify-between gap-4">
          <dt class="text-sm text-gray-500 dark:text-gray-400">{{ t('assignedAccounts.columns.platform') }}</dt>
          <dd class="text-sm text-gray-900 dark:text-gray-100">{{ detail.platform }}</dd>
        </div>
        <div class="flex items-start justify-between gap-4">
          <dt class="text-sm text-gray-500 dark:text-gray-400">{{ t('assignedAccounts.columns.accountType') }}</dt>
          <dd class="text-sm text-gray-900 dark:text-gray-100">{{ detail.account_type }}</dd>
        </div>
        <div class="flex items-start justify-between gap-4">
          <dt class="text-sm text-gray-500 dark:text-gray-400">{{ t('assignedAccounts.columns.email') }}</dt>
          <dd class="text-sm text-gray-900 dark:text-gray-100" data-test="detail-email">
            {{ displayIdentity(detail.email_masked) }}
          </dd>
        </div>
        <div class="flex items-start justify-between gap-4">
          <dt class="text-sm text-gray-500 dark:text-gray-400">{{ t('assignedAccounts.columns.username') }}</dt>
          <dd class="text-sm text-gray-900 dark:text-gray-100" data-test="detail-username">
            {{ displayIdentity(detail.username_masked) }}
          </dd>
        </div>
        <div class="flex items-start justify-between gap-4">
          <dt class="text-sm text-gray-500 dark:text-gray-400">{{ t('assignedAccounts.columns.upstreamAccountId') }}</dt>
          <dd class="text-sm text-gray-900 dark:text-gray-100" data-test="detail-upstream-id">
            {{ displayIdentity(detail.upstream_account_id_masked) }}
          </dd>
        </div>
        <p class="pt-2 text-xs text-gray-400 dark:text-dark-500">
          {{ t('assignedAccounts.identityMaskedNotice') }}
        </p>
      </dl>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import Pagination from '@/components/common/Pagination.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import Icon from '@/components/icons/Icon.vue'
import type { Column } from '@/components/common/types'
import assignedAccountsAPI, {
  isAssignedAccountNotFound,
  isAssignedAccountsAccessDenied,
  type AssignedAccount
} from '@/api/assignedAccounts'
import { getPersistedPageSize, setPersistedPageSize } from '@/composables/usePersistedPageSize'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import { extractApiErrorMessage } from '@/utils/apiError'
import { ASSIGNED_ACCOUNTS_FALLBACK_PATH } from '@/router/assignedAccountsAccess'

const { t } = useI18n()
const router = useRouter()
const appStore = useAppStore()
const authStore = useAuthStore()

const accounts = ref<AssignedAccount[]>([])
const loading = ref(false)
const pagination = ref({
  page: 1,
  page_size: getPersistedPageSize(),
  total: 0
})

const showDetail = ref(false)
const detailLoading = ref(false)
const detailUnavailable = ref(false)
const detailFailed = ref(false)
const detail = ref<AssignedAccount | null>(null)
const detailAccountId = ref<number | null>(null)

// 请求代次：重叠的列表/详情响应一律以最新一次为准，避免旧响应覆盖新状态。
let listGeneration = 0
let detailGeneration = 0

const columns = computed((): Column[] => [
  { key: 'platform', label: t('assignedAccounts.columns.platform') },
  { key: 'account_type', label: t('assignedAccounts.columns.accountType') },
  { key: 'email_masked', label: t('assignedAccounts.columns.email') },
  { key: 'username_masked', label: t('assignedAccounts.columns.username') },
  { key: 'upstream_account_id_masked', label: t('assignedAccounts.columns.upstreamAccountId') },
  { key: 'actions', label: t('assignedAccounts.columns.actions') }
])

// 服务端对无法安全展示的身份返回空串，界面统一显示占位符，不回退到任何原始名称。
function displayIdentity(value: string | null | undefined): string {
  const text = typeof value === 'string' ? value.trim() : ''
  return text === '' ? '—' : text
}

/**
 * 能力被撤销（或从未授权）时，后端返回 403：立刻刷新本地资料让菜单消失，
 * 并离开这个页面。缓存状态不构成访问依据。
 */
async function handleAccessRevoked(): Promise<void> {
  authStore.refreshUser().catch((error) => {
    console.warn('Failed to refresh user after account view revocation:', error)
  })
  await router.replace(ASSIGNED_ACCOUNTS_FALLBACK_PATH)
}

async function loadAccounts(): Promise<void> {
  // 翻页/刷新可能重叠：只有最新一次请求可以写入列表与计数。
  const generation = ++listGeneration
  loading.value = true
  try {
    const page = await assignedAccountsAPI.list(pagination.value.page, pagination.value.page_size)
    if (generation !== listGeneration) return
    accounts.value = page.items
    pagination.value.total = page.total
    pagination.value.page_size = page.page_size
  } catch (error) {
    // 过期响应不得据此判定权限：更新的请求若已成功，说明能力仍在。
    // 当前代次仍以服务端为准，403 照常按撤销处理（fail-closed）。
    if (generation !== listGeneration) return
    if (isAssignedAccountsAccessDenied(error)) {
      await handleAccessRevoked()
      return
    }
    appStore.showError(extractApiErrorMessage(error, t('assignedAccounts.loadFailed')))
  } finally {
    if (generation === listGeneration) {
      loading.value = false
    }
  }
}

async function openDetail(row: AssignedAccount): Promise<void> {
  showDetail.value = true
  await loadDetail(row.id)
}

async function loadDetail(id: number): Promise<void> {
  detailAccountId.value = id
  detailLoading.value = true
  detailUnavailable.value = false
  detailFailed.value = false
  detail.value = null

  // 连续点击不同账号时，先发出的响应不得覆盖后打开的账号。
  const generation = ++detailGeneration
  try {
    const account = await assignedAccountsAPI.getById(id)
    if (generation !== detailGeneration) return
    detail.value = account
  } catch (error) {
    // 过期响应不得关闭当前详情或把用户踢出页面；当前代次的 403 仍按撤销处理。
    if (generation !== detailGeneration) return
    if (isAssignedAccountsAccessDenied(error)) {
      closeDetail()
      await handleAccessRevoked()
      return
    }
    // 未分配、已禁用、已删除与不存在都返回同一个 404：统一提示，不区分存在性。
    // 只有 404 表示「不可见」；网络或服务端故障如实报错，不能谎称无权查看。
    if (isAssignedAccountNotFound(error)) {
      detailUnavailable.value = true
    } else {
      detailFailed.value = true
    }
  } finally {
    if (generation === detailGeneration) {
      detailLoading.value = false
    }
  }
}

function retryDetail(): void {
  if (detailAccountId.value === null) {
    return
  }
  void loadDetail(detailAccountId.value)
}

function closeDetail(): void {
  // 关闭即让在途的详情响应过期。
  detailGeneration += 1
  showDetail.value = false
  detailLoading.value = false
  detail.value = null
  detailUnavailable.value = false
  detailFailed.value = false
  detailAccountId.value = null
}

function handlePageChange(page: number): void {
  pagination.value.page = page
  loadAccounts()
}

function handlePageSizeChange(pageSize: number): void {
  pagination.value.page_size = pageSize
  pagination.value.page = 1
  setPersistedPageSize(pageSize)
  loadAccounts()
}

onMounted(loadAccounts)
</script>
