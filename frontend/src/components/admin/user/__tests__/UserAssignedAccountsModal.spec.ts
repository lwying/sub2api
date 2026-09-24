import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import type { AdminUser } from '@/types'
import UserAssignedAccountsModal from '../UserAssignedAccountsModal.vue'

const { getAccountView, updateAccountView, listAccounts, showError, showSuccess } = vi.hoisted(
  () => ({
    getAccountView: vi.fn(),
    updateAccountView: vi.fn(),
    listAccounts: vi.fn(),
    showError: vi.fn(),
    showSuccess: vi.fn()
  })
)

vi.mock('@/api/admin', () => ({
  adminAPI: {
    users: {
      getAccountView,
      updateAccountView
    },
    accounts: {
      list: listAccounts
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess })
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

const BaseDialogStub = {
  props: ['show', 'title', 'width'],
  emits: ['close'],
  template: `
    <div v-if="show" data-test="grant-dialog">
      <span data-test="dialog-title">{{ title }}</span>
      <slot />
      <div data-test="dialog-footer"><slot name="footer" /></div>
    </div>
  `
}

function createUser(id: number, email: string): AdminUser {
  return { id, email, role: 'user' } as unknown as AdminUser
}

const firstUser = createUser(42, 'customer@example.com')
const secondUser = createUser(43, 'other@example.com')

const mountModal = (user: AdminUser = firstUser) =>
  mount(UserAssignedAccountsModal, {
    props: { show: true, user },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        LoadingSpinner: true
      }
    }
  })

function emptyCandidates() {
  return { items: [], total: 0, page: 1, page_size: 20, pages: 0 }
}

function createDeferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

function candidatePage(ids: number[], options: { total?: number; page?: number } = {}) {
  const items = ids.map((id) => ({
    id,
    name: `account-${id}`,
    platform: 'openai',
    type: 'oauth',
    status: 'active'
  }))
  return {
    items,
    total: options.total ?? items.length,
    page: options.page ?? 1,
    page_size: 20,
    pages: 1
  }
}

describe('UserAssignedAccountsModal', () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    getAccountView.mockReset()
    updateAccountView.mockReset()
    listAccounts.mockReset()
    showError.mockReset()
    showSuccess.mockReset()

    getAccountView.mockResolvedValue({
      user_id: 42,
      enabled: true,
      account_ids: [11],
      accounts: [
        { id: 11, name: 'assigned-one', platform: 'anthropic', account_type: 'oauth', status: 'active' }
      ]
    })
    listAccounts.mockResolvedValue({
      items: [
        { id: 11, name: 'assigned-one', platform: 'anthropic', type: 'oauth', status: 'active' },
        { id: 12, name: 'candidate-two', platform: 'openai', type: 'apikey', status: 'active' }
      ],
      total: 2,
      page: 1,
      page_size: 20,
      pages: 1
    })
    updateAccountView.mockImplementation(async (_id: number, grant: Record<string, unknown>) => ({
      user_id: 42,
      enabled: grant.enabled,
      account_ids: grant.account_ids,
      accounts: []
    }))
  })

  it('loads the current grant and hides already assigned accounts from the picker', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    expect(getAccountView).toHaveBeenCalledWith(42)
    expect(wrapper.get('[data-test="grant-enabled"]').element).toHaveProperty('checked', true)
    expect(wrapper.get('[data-test="assigned-11"]').text()).toContain('assigned-one')
    expect(wrapper.find('[data-test="candidate-11"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="candidate-12"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('saves the complete assignment set so revoking is immediate', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    await wrapper.get('[data-test="remove-11"]').trigger('click')
    await wrapper.get('[data-test="add-12"]').trigger('click')
    await wrapper.get('[data-test="save-grant"]').trigger('click')
    await flushPromises()

    expect(updateAccountView).toHaveBeenCalledWith(42, { enabled: true, account_ids: [12] })
    expect(showSuccess).toHaveBeenCalledWith('assignedAccounts.admin.saveSuccess')
    wrapper.unmount()
  })

  it('can revoke everything by disabling the capability and clearing the set', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    await wrapper.get('[data-test="grant-enabled"]').setValue(false)
    await wrapper.get('[data-test="remove-11"]').trigger('click')
    await wrapper.get('[data-test="save-grant"]').trigger('click')
    await flushPromises()

    expect(updateAccountView).toHaveBeenCalledWith(42, { enabled: false, account_ids: [] })
    wrapper.unmount()
  })

  it('surfaces a save failure without closing the dialog', async () => {
    updateAccountView.mockRejectedValue({ status: 500, message: 'boom' })

    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    await wrapper.get('[data-test="save-grant"]').trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[data-test="grant-dialog"]').exists()).toBe(true)
    wrapper.unmount()
  })

  // --- fail-closed 读取：不得沿用上一个用户的授权，也不得允许盲目保存 ---

  it('refuses to show or save another user’s state when the grant read fails', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()
    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true)

    // 打开第二个用户时读取失败：清空状态、禁用保存。
    getAccountView.mockRejectedValue({ status: 500, message: 'boom' })
    await wrapper.setProps({ user: secondUser })
    await flushPromises()

    expect(getAccountView).toHaveBeenLastCalledWith(43)
    // 读取失败时不呈现任何授权状态（既不显示上一个用户的状态，也不显示空集合）。
    expect(wrapper.find('[data-test="grant-enabled"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="assigned-11"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="no-assigned"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="retry-load"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="save-grant"]').attributes('disabled')).toBeDefined()

    await wrapper.get('[data-test="save-grant"]').trigger('click')
    await flushPromises()
    expect(updateAccountView).not.toHaveBeenCalled()

    // 允许重试，重试成功后保存恢复可用。
    getAccountView.mockResolvedValue({
      user_id: 43,
      enabled: true,
      account_ids: [21],
      accounts: [
        { id: 21, name: 'second-user-account', platform: 'openai', account_type: 'oauth', status: 'active' }
      ]
    })
    await wrapper.get('[data-test="retry-load"]').trigger('click')
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    expect(wrapper.get('[data-test="assigned-21"]').text()).toContain('second-user-account')
    expect(wrapper.get('[data-test="save-grant"]').attributes('disabled')).toBeUndefined()

    await wrapper.get('[data-test="save-grant"]').trigger('click')
    await flushPromises()
    expect(updateAccountView).toHaveBeenCalledWith(43, { enabled: true, account_ids: [21] })
    wrapper.unmount()
  })

  it('drops a slow response of the previous user instead of overwriting the current one', async () => {
    let resolveFirst!: (grant: unknown) => void
    getAccountView.mockImplementationOnce(
      () => new Promise((resolve) => { resolveFirst = resolve })
    )

    const wrapper = mountModal()
    await flushPromises()

    // 立刻切到第二个用户，其读取先返回。
    getAccountView.mockResolvedValueOnce({
      user_id: 43,
      enabled: true,
      account_ids: [21],
      accounts: [
        { id: 21, name: 'second-user-account', platform: 'openai', account_type: 'oauth', status: 'active' }
      ]
    })
    await wrapper.setProps({ user: secondUser })
    await flushPromises()

    expect(wrapper.get('[data-test="assigned-21"]').exists()).toBe(true)

    // 第一个用户的响应姗姗来迟，必须被丢弃。
    resolveFirst({
      user_id: 42,
      enabled: true,
      account_ids: [11],
      accounts: [
        { id: 11, name: 'assigned-one', platform: 'anthropic', account_type: 'oauth', status: 'active' }
      ]
    })
    await flushPromises()

    expect(wrapper.find('[data-test="assigned-11"]').exists()).toBe(false)
    expect(wrapper.get('[data-test="assigned-21"]').exists()).toBe(true)
    wrapper.unmount()
  })

  // --- 授权集合以 account_ids 为准，缺详情也要保留 ---

  it('keeps assigned ids whose details are missing from the response', async () => {
    getAccountView.mockResolvedValue({
      user_id: 42,
      enabled: true,
      account_ids: [11, 99],
      accounts: [
        { id: 11, name: 'assigned-one', platform: 'anthropic', account_type: 'oauth', status: 'active' }
      ]
    })

    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    expect(wrapper.get('[data-test="assigned-99"]').text()).toContain('#99')

    await wrapper.get('[data-test="save-grant"]').trigger('click')
    await flushPromises()

    expect(updateAccountView).toHaveBeenCalledWith(42, { enabled: true, account_ids: [11, 99] })
    wrapper.unmount()
  })

  it('does not blank the assigned list when the save echo omits account details', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    await wrapper.get('[data-test="save-grant"]').trigger('click')
    await flushPromises()

    expect(updateAccountView).toHaveBeenCalledWith(42, { enabled: true, account_ids: [11] })
    expect(wrapper.get('[data-test="assigned-11"]').text()).toContain('assigned-one')
    wrapper.unmount()
  })

  // --- 同一用户关闭再打开：上一次会话的保存回显不得作用到新会话 ---

  it('drops a save echo that lands after the dialog was closed and reopened for the same user', async () => {
    const slowSave = createDeferred<Record<string, unknown>>()
    updateAccountView.mockImplementationOnce(() => slowSave.promise)

    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    // 旧会话提交了 [11, 12] 的保存，响应挂起。
    await wrapper.get('[data-test="add-12"]').trigger('click')
    await wrapper.get('[data-test="save-grant"]').trigger('click')
    await flushPromises()
    expect(updateAccountView).toHaveBeenCalledWith(42, { enabled: true, account_ids: [11, 12] })

    // 关闭再打开同一个用户：新会话的 GET 先返回，仍是服务端的 [11]。
    await wrapper.setProps({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true })
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    expect(getAccountView).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="assigned-12"]').exists()).toBe(false)

    // 旧保存的回显姗姗来迟：不得改写新会话，也不得替新会话关闭弹窗。
    slowSave.resolve({ user_id: 42, enabled: true, account_ids: [11, 12], accounts: [] })
    await flushPromises()

    expect(wrapper.find('[data-test="assigned-12"]').exists()).toBe(false)
    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="grant-dialog"]').exists()).toBe(true)
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(showSuccess).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('does not apply a save echo after the dialog was closed', async () => {
    const slowSave = createDeferred<Record<string, unknown>>()
    updateAccountView.mockImplementationOnce(() => slowSave.promise)

    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    await wrapper.get('[data-test="add-12"]').trigger('click')
    await wrapper.get('[data-test="save-grant"]').trigger('click')
    await flushPromises()

    await wrapper.setProps({ show: false })
    await flushPromises()

    slowSave.resolve({ user_id: 42, enabled: true, account_ids: [11, 12], accounts: [] })
    await flushPromises()

    expect(wrapper.emitted('close')).toBeUndefined()
    expect(wrapper.emitted('success')).toBeUndefined()
    expect(showSuccess).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('lets a save of the reopened session win over the previous session’s late save', async () => {
    const slowOldSave = createDeferred<Record<string, unknown>>()
    updateAccountView.mockImplementationOnce(() => slowOldSave.promise)

    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    // 旧会话的保存挂起（[11, 12]）。
    await wrapper.get('[data-test="add-12"]').trigger('click')
    await wrapper.get('[data-test="save-grant"]').trigger('click')
    await flushPromises()

    // 关闭再打开同一用户，新会话重新保存并先返回。
    await wrapper.setProps({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true })
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    updateAccountView.mockResolvedValueOnce({
      user_id: 42,
      enabled: true,
      account_ids: [11, 12],
      accounts: []
    })
    await wrapper.get('[data-test="add-12"]').trigger('click')
    await wrapper.get('[data-test="save-grant"]').trigger('click')
    await flushPromises()

    expect(updateAccountView).toHaveBeenLastCalledWith(42, { enabled: true, account_ids: [11, 12] })
    expect(wrapper.get('[data-test="assigned-12"]').exists()).toBe(true)

    // 旧会话的保存姗姗来迟，带着与新会话不同的集合：必须被丢弃。
    slowOldSave.resolve({ user_id: 42, enabled: true, account_ids: [99], accounts: [] })
    await flushPromises()

    expect(wrapper.find('[data-test="assigned-99"]').exists()).toBe(false)
    expect(wrapper.get('[data-test="assigned-12"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true)
    // 只有新会话那次保存产生了成功提示与关闭事件。
    expect(showSuccess).toHaveBeenCalledTimes(1)
    expect(wrapper.emitted('close')).toHaveLength(1)
    wrapper.unmount()
  })

  // --- 候选账号：远端搜索 + 服务端分页 ---

  it('searches candidates on the server instead of filtering a first page locally', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    expect(listAccounts).toHaveBeenLastCalledWith(1, 20, { lite: '1' }, expect.anything())

    listAccounts.mockResolvedValue({
      items: [{ id: 201, name: 'account-201', platform: 'openai', type: 'oauth', status: 'active' }],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })
    await wrapper.get('[data-test="account-search"]').setValue('201')
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    expect(listAccounts).toHaveBeenLastCalledWith(
      1,
      20,
      { lite: '1', search: '201' },
      expect.anything()
    )
    expect(wrapper.get('[data-test="candidate-201"]').text()).toContain('account-201')
    wrapper.unmount()
  })

  it('pages through candidates so accounts beyond the first page stay assignable', async () => {
    listAccounts.mockResolvedValueOnce({
      items: Array.from({ length: 20 }, (_, index) => ({
        id: index + 1,
        name: `page-one-${index + 1}`,
        platform: 'openai',
        type: 'oauth',
        status: 'active'
      })),
      total: 21,
      page: 1,
      page_size: 20,
      pages: 2
    })

    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    expect(wrapper.find('[data-test="load-more-candidates"]').exists()).toBe(true)

    listAccounts.mockResolvedValueOnce({
      items: [{ id: 21, name: 'twenty-first', platform: 'gemini', type: 'oauth', status: 'active' }],
      total: 21,
      page: 2,
      page_size: 20,
      pages: 2
    })
    await wrapper.get('[data-test="load-more-candidates"]').trigger('click')
    await flushPromises()

    expect(listAccounts).toHaveBeenLastCalledWith(2, 20, { lite: '1' }, expect.anything())
    expect(wrapper.get('[data-test="candidate-21"]').text()).toContain('twenty-first')
    expect(wrapper.get('[data-test="candidate-1"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="load-more-candidates"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('drops a page-two response of the previous keyword when the search changes mid-flight', async () => {
    // 旧关键词：首页 20 条、总数 40，因此还有第 2 页。
    listAccounts.mockResolvedValueOnce({
      items: Array.from({ length: 20 }, (_, index) => ({
        id: index + 1,
        name: `old-page-one-${index + 1}`,
        platform: 'openai',
        type: 'oauth',
        status: 'active'
      })),
      total: 40,
      page: 1,
      page_size: 20,
      pages: 2
    })

    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    // 旧关键词的第 2 页挂起。
    const slowOldPageTwo = createDeferred<ReturnType<typeof candidatePage>>()
    listAccounts.mockImplementationOnce(() => slowOldPageTwo.promise)
    await wrapper.get('[data-test="load-more-candidates"]').trigger('click')
    await flushPromises()

    // 用户改搜 'B'：新关键词的第 1 页先返回。
    listAccounts.mockResolvedValueOnce(candidatePage([201], { total: 21 }))
    await wrapper.get('[data-test="account-search"]').setValue('B')
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    expect(listAccounts).toHaveBeenLastCalledWith(
      1,
      20,
      { lite: '1', search: 'B' },
      expect.anything()
    )
    expect(wrapper.get('[data-test="candidate-201"]').exists()).toBe(true)

    // 旧关键词的第 2 页姗姗来迟：既不能追加进候选，也不能改写页数与总数。
    slowOldPageTwo.resolve(candidatePage([999], { total: 40, page: 2 }))
    await flushPromises()

    expect(wrapper.find('[data-test="candidate-999"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="candidate-1"]').exists()).toBe(false)

    // 新关键词的第 2 页必须是它自己的第 2 页，而不是被旧响应顶到第 3 页。
    listAccounts.mockResolvedValueOnce(candidatePage([202], { total: 21, page: 2 }))
    await wrapper.get('[data-test="load-more-candidates"]').trigger('click')
    await flushPromises()

    expect(listAccounts).toHaveBeenLastCalledWith(
      2,
      20,
      { lite: '1', search: 'B' },
      expect.anything()
    )
    expect(wrapper.get('[data-test="candidate-202"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="candidate-201"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="candidate-999"]').exists()).toBe(false)
    wrapper.unmount()
  })

  // --- 账号状态词表：手动停用（含历史 disabled）同一标签，非手动状态照常展示 ---

  it('labels legacy and current manual-disable statuses identically', async () => {
    getAccountView.mockResolvedValue({
      user_id: 42,
      enabled: true,
      account_ids: [11, 12, 13],
      accounts: [
        { id: 11, name: 'legacy-disabled', platform: 'anthropic', account_type: 'oauth', status: 'disabled' },
        { id: 12, name: 'editor-inactive', platform: 'anthropic', account_type: 'oauth', status: 'inactive' },
        { id: 13, name: 'temp-unschedulable', platform: 'openai', account_type: 'oauth', status: 'temp_unschedulable' }
      ]
    })

    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    expect(wrapper.get('[data-test="assigned-11"]').text()).toContain('admin.accounts.status.inactive')
    expect(wrapper.get('[data-test="assigned-12"]').text()).toContain('admin.accounts.status.inactive')
    // 临时不可调度不是手动禁用：照常展示，且用其自身标签。
    expect(wrapper.get('[data-test="assigned-13"]').text()).toContain(
      'admin.accounts.status.tempUnschedulable'
    )
    wrapper.unmount()
  })

  it('keeps assigned ids when the picker pages are unavailable', async () => {
    listAccounts.mockResolvedValue(emptyCandidates())

    const wrapper = mountModal()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="save-grant"]').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })
})
