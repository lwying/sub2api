import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import type { AssignedAccount } from '@/api/assignedAccounts'
import AssignedAccountsView from '../AssignedAccountsView.vue'

const { listAccounts, getAccount, showError, refreshUser, routerReplace } = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  getAccount: vi.fn(),
  showError: vi.fn(),
  refreshUser: vi.fn(),
  routerReplace: vi.fn()
}))

// 保留真实的 403 判定（isAssignedAccountsAccessDenied），只替换网络调用。
vi.mock('@/api/assignedAccounts', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/assignedAccounts')>('@/api/assignedAccounts')
  return {
    ...actual,
    default: {
      list: listAccounts,
      getById: getAccount
    }
  }
})

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ refreshUser })
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({ replace: routerReplace })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params === undefined ? key : `${key}:${JSON.stringify(params)}`
    })
  }
})

const DataTableStub = {
  props: ['columns', 'data', 'loading'],
  template: `
    <div data-test="accounts-table">
      <div v-for="row in data" :key="row.id" :data-test="'row-' + row.id">
        <template v-for="col in columns" :key="col.key">
          <slot :name="'cell-' + col.key" :value="row[col.key]" :row="row" />
        </template>
      </div>
      <div v-if="data.length === 0" data-test="table-empty">
        <slot name="empty" />
      </div>
    </div>
  `
}

const BaseDialogStub = {
  props: ['show', 'title'],
  emits: ['close'],
  template: `
    <div v-if="show" data-test="detail-dialog">
      <span data-test="detail-title">{{ title }}</span>
      <slot />
    </div>
  `
}

const PaginationStub = {
  props: ['page', 'total', 'pageSize'],
  emits: ['update:page', 'update:pageSize'],
  // 每次点击跳到下一页：与真实分页一致，允许在上一次请求未返回时继续翻页。
  template: '<button data-test="next-page" @click="$emit(\'update:page\', (page || 1) + 1)">next</button>'
}

const mountView = () =>
  mount(AssignedAccountsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template:
            '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
        },
        DataTable: DataTableStub,
        BaseDialog: BaseDialogStub,
        Pagination: PaginationStub,
        EmptyState: { props: ['title'], template: '<div data-test="empty-state">{{ title }}</div>' },
        LoadingSpinner: true,
        Icon: true
      }
    }
  })

function createDeferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

function accountPage(items: AssignedAccount[], page = 1) {
  return { items, total: items.length, page, page_size: 20, pages: 1 }
}

/**
 * 模拟一个「超范围」的后端响应：除了允许字段之外还夹带原始身份与凭据哨兵，
 * 前端必须完全不呈现它们。
 */
function hostileAccount(overrides: Partial<AssignedAccount> = {}): AssignedAccount & {
  name: string
  credentials: Record<string, string>
} {
  return {
    id: 7,
    platform: 'anthropic',
    account_type: 'oauth',
    email_masked: 'a***@example.com',
    username_masked: 'u***r',
    upstream_account_id_masked: 'a***7',
    name: 'RAW-SENTINEL-NAME',
    credentials: { access_token: 'SECRET-SENTINEL' },
    ...overrides
  }
}

describe('AssignedAccountsView', () => {
  beforeEach(() => {
    localStorage.clear()
    listAccounts.mockReset()
    getAccount.mockReset()
    showError.mockReset()
    refreshUser.mockReset()
    routerReplace.mockReset()

    refreshUser.mockResolvedValue({})
    listAccounts.mockResolvedValue({
      items: [hostileAccount()],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('renders only the masked identity fields returned by the dedicated API', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(listAccounts).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-test="cell-email"]').text()).toBe('a***@example.com')
    expect(wrapper.get('[data-test="cell-username"]').text()).toBe('u***r')
    expect(wrapper.get('[data-test="cell-upstream-id"]').text()).toBe('a***7')

    const html = wrapper.html()
    expect(html).not.toContain('RAW-SENTINEL-NAME')
    expect(html).not.toContain('SECRET-SENTINEL')
    wrapper.unmount()
  })

  it('renders an identity value as inert text instead of markup', async () => {
    listAccounts.mockResolvedValue({
      items: [hostileAccount({ email_masked: '<img src=x onerror="alert(1)">' })],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })

    const wrapper = mountView()
    await flushPromises()

    const cell = wrapper.get('[data-test="cell-email"]')
    expect(cell.text()).toBe('<img src=x onerror="alert(1)">')
    expect(cell.find('img').exists()).toBe(false)
    wrapper.unmount()
  })

  it('shows the empty state when nothing is assigned', async () => {
    listAccounts.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 1 })

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-test="table-empty"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('assignedAccounts.empty')
    wrapper.unmount()
  })

  it('leaves the page and drops the cached capability when the API denies access', async () => {
    listAccounts.mockRejectedValue({ status: 403, code: 'ACCOUNT_VIEW_DISABLED' })

    const wrapper = mountView()
    await flushPromises()

    expect(refreshUser).toHaveBeenCalledTimes(1)
    expect(routerReplace).toHaveBeenCalledWith('/dashboard')
    expect(showError).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('reports non-authorization failures without leaving the page', async () => {
    listAccounts.mockRejectedValue({ status: 500, message: 'boom' })

    const wrapper = mountView()
    await flushPromises()

    expect(showError).toHaveBeenCalledTimes(1)
    expect(routerReplace).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('loads the detail from the dedicated endpoint and renders it as text', async () => {
    getAccount.mockResolvedValue(hostileAccount({ id: 7, email_masked: 'b***@example.com' }))

    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="view-detail"]').trigger('click')
    await flushPromises()

    expect(getAccount).toHaveBeenCalledWith(7)
    expect(wrapper.find('[data-test="detail-dialog"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="detail-email"]').text()).toBe('b***@example.com')
    expect(wrapper.html()).not.toContain('RAW-SENTINEL-NAME')
    wrapper.unmount()
  })

  it('collapses an unassigned or disabled account into one indistinguishable notice', async () => {
    getAccount.mockRejectedValue({ status: 404, message: 'not found' })

    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="view-detail"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-test="detail-unavailable"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="detail-fields"]').exists()).toBe(false)
    expect(routerReplace).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('reports a server failure as a load error rather than “not visible”', async () => {
    getAccount.mockRejectedValue({ status: 500, message: 'boom' })

    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="view-detail"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-test="detail-failed"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="detail-failed"]').text()).toContain(
      'assignedAccounts.detail.loadFailed'
    )
    expect(wrapper.find('[data-test="detail-unavailable"]').exists()).toBe(false)
    expect(routerReplace).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('reports a network failure and lets the reader retry the same account', async () => {
    getAccount.mockRejectedValueOnce(new Error('network down'))

    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="view-detail"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-test="detail-failed"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="detail-unavailable"]').exists()).toBe(false)

    getAccount.mockResolvedValueOnce(hostileAccount({ id: 7, email_masked: 'c***d@example.com' }))
    await wrapper.get('[data-test="retry-detail"]').trigger('click')
    await flushPromises()

    expect(getAccount).toHaveBeenLastCalledWith(7)
    expect(wrapper.get('[data-test="detail-email"]').text()).toBe('c***d@example.com')
    expect(wrapper.find('[data-test="detail-failed"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('leaves the page when the detail read is denied by the server', async () => {
    getAccount.mockRejectedValue({ status: 403, code: 'ACCOUNT_VIEW_DISABLED' })

    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="view-detail"]').trigger('click')
    await flushPromises()

    expect(routerReplace).toHaveBeenCalledWith('/dashboard')
    wrapper.unmount()
  })

  // --- 重叠请求：旧响应不得覆盖新状态 ---

  it('keeps the newest list response when an earlier page request resolves late', async () => {
    // 初次加载：总数让分页入口出现。
    listAccounts.mockResolvedValueOnce({
      items: [hostileAccount({ id: 7 })],
      total: 40,
      page: 1,
      page_size: 20,
      pages: 2
    })

    const wrapper = mountView()
    await flushPromises()

    // 第 2 页请求挂起，用户继续翻到第 3 页并先拿到结果。
    const slowPageTwo = createDeferred<ReturnType<typeof accountPage>>()
    listAccounts.mockImplementationOnce(() => slowPageTwo.promise)
    await wrapper.get('[data-test="next-page"]').trigger('click')
    await flushPromises()

    listAccounts.mockResolvedValueOnce(accountPage([hostileAccount({ id: 9 })], 3))
    await wrapper.get('[data-test="next-page"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-test="row-9"]').exists()).toBe(true)

    // 第 2 页的响应姗姗来迟，必须被丢弃。
    slowPageTwo.resolve(accountPage([hostileAccount({ id: 8 })], 2))
    await flushPromises()

    expect(wrapper.find('[data-test="row-8"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="row-9"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('keeps the last opened account when an earlier detail request resolves late', async () => {
    const slowDetail = createDeferred<ReturnType<typeof hostileAccount>>()
    listAccounts.mockResolvedValue(accountPage([hostileAccount({ id: 7 }), hostileAccount({ id: 8 })]))

    const wrapper = mountView()
    await flushPromises()

    getAccount.mockImplementationOnce(() => slowDetail.promise)
    await wrapper.findAll('[data-test="view-detail"]')[0].trigger('click')
    await flushPromises()

    getAccount.mockResolvedValueOnce(hostileAccount({ id: 8, email_masked: 'o***r@example.com' }))
    await wrapper.findAll('[data-test="view-detail"]')[1].trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="detail-email"]').text()).toBe('o***r@example.com')

    slowDetail.resolve(hostileAccount({ id: 7, email_masked: 's***t@example.com' }))
    await flushPromises()

    expect(wrapper.get('[data-test="detail-email"]').text()).toBe('o***r@example.com')
    wrapper.unmount()
  })

  // --- 过期的 403：能力已被更新的成功响应证明仍在，不得据此把用户踢出页面 ---

  it('keeps the reader on the page when a superseded list request answers 403', async () => {
    listAccounts.mockResolvedValueOnce({
      items: [hostileAccount({ id: 7 })],
      total: 40,
      page: 1,
      page_size: 20,
      pages: 2
    })

    const wrapper = mountView()
    await flushPromises()

    // 第 2 页挂起，用户继续翻到第 3 页并先拿到成功结果。
    const slowPageTwo = createDeferred<ReturnType<typeof accountPage>>()
    listAccounts.mockImplementationOnce(() => slowPageTwo.promise)
    await wrapper.get('[data-test="next-page"]').trigger('click')
    await flushPromises()

    listAccounts.mockResolvedValueOnce(accountPage([hostileAccount({ id: 9 })], 3))
    await wrapper.get('[data-test="next-page"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-test="row-9"]').exists()).toBe(true)

    // 过期的第 2 页返回 403：更新的请求已经成功，说明能力仍在。
    slowPageTwo.reject({ status: 403, code: 'ACCOUNT_VIEW_DISABLED' })
    await flushPromises()

    expect(routerReplace).not.toHaveBeenCalled()
    expect(refreshUser).not.toHaveBeenCalled()
    expect(showError).not.toHaveBeenCalled()
    expect(wrapper.find('[data-test="row-9"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('keeps the opened detail when a superseded detail request answers 403', async () => {
    listAccounts.mockResolvedValue(accountPage([hostileAccount({ id: 7 }), hostileAccount({ id: 8 })]))

    const wrapper = mountView()
    await flushPromises()

    // 账号 7 的详情挂起，用户改看账号 8 并先拿到成功结果。
    const slowDetail = createDeferred<ReturnType<typeof hostileAccount>>()
    getAccount.mockImplementationOnce(() => slowDetail.promise)
    await wrapper.findAll('[data-test="view-detail"]')[0].trigger('click')
    await flushPromises()

    getAccount.mockResolvedValueOnce(hostileAccount({ id: 8, email_masked: 'o***r@example.com' }))
    await wrapper.findAll('[data-test="view-detail"]')[1].trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="detail-email"]').text()).toBe('o***r@example.com')

    slowDetail.reject({ status: 403, code: 'ACCOUNT_VIEW_DISABLED' })
    await flushPromises()

    expect(routerReplace).not.toHaveBeenCalled()
    expect(refreshUser).not.toHaveBeenCalled()
    expect(wrapper.find('[data-test="detail-dialog"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="detail-email"]').text()).toBe('o***r@example.com')
    wrapper.unmount()
  })
})
