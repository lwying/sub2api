import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount } from "@vue/test-utils";

import type { AdminUser } from "@/types";
import UserAssignedAccountsModal from "../UserAssignedAccountsModal.vue";

const {
  getAccountView,
  updateAccountView,
  listAccounts,
  getGroups,
  showError,
  showSuccess,
} = vi.hoisted(() => ({
  getAccountView: vi.fn(),
  updateAccountView: vi.fn(),
  listAccounts: vi.fn(),
  getGroups: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}));

vi.mock("@/api/admin", () => ({
  adminAPI: {
    users: {
      getAccountView,
      updateAccountView,
    },
    accounts: {
      listOptions: listAccounts,
    },
    groups: {
      getAll: getGroups,
    },
  },
}));

vi.mock("@/stores/app", () => ({
  useAppStore: () => ({ showError, showSuccess }),
}));

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params === undefined ? key : `${key}:${JSON.stringify(params)}`,
    }),
  };
});

const BaseDialogStub = {
  props: ["show", "title", "width"],
  emits: ["close"],
  template: `
    <div v-if="show" data-test="grant-dialog">
      <span data-test="dialog-title">{{ title }}</span>
      <slot />
      <div data-test="dialog-footer"><slot name="footer" /></div>
    </div>
  `,
};

function createUser(id: number, email: string): AdminUser {
  return { id, email, role: "user" } as unknown as AdminUser;
}

const firstUser = createUser(42, "customer@example.com");
const secondUser = createUser(43, "other@example.com");

const mountModal = (user: AdminUser = firstUser) =>
  mount(UserAssignedAccountsModal, {
    props: { show: true, user },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        LoadingSpinner: true,
      },
    },
  });

function emptyCandidates() {
  return { items: [], total: 0, page: 1, page_size: 20, pages: 0 };
}

function createDeferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function candidatePage(
  ids: number[],
  options: { total?: number; page?: number } = {},
) {
  const items = ids.map((id) => ({
    id,
    name: `account-${id}`,
    platform: "openai",
    type: "oauth",
    status: "active",
  }));
  return {
    items,
    total: options.total ?? items.length,
    page: options.page ?? 1,
    page_size: 20,
    pages: 1,
  };
}

/** 候选列表的另一种构造：名称可预测，并能指定部分账号的状态（停用账号仍应可选）。 */
function candidateList(
  ids: number[],
  options: {
    statuses?: Record<number, string>;
    total?: number;
    page?: number;
  } = {},
) {
  const items = ids.map((id) => ({
    id,
    name: `candidate-${id}`,
    platform: "openai",
    type: "oauth",
    status: options.statuses?.[id] ?? "active",
  }));
  return {
    items,
    total: options.total ?? items.length,
    page: options.page ?? 1,
    page_size: 20,
    pages: 1,
  };
}

/** 等待弹窗完成一次授权读取 + 候选首页加载。 */
async function settleModal(): Promise<void> {
  await flushPromises();
  await vi.advanceTimersByTimeAsync(300);
  await flushPromises();
}

describe("UserAssignedAccountsModal", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    getAccountView.mockReset();
    updateAccountView.mockReset();
    listAccounts.mockReset();
    getGroups.mockReset();
    getGroups.mockResolvedValue([{ id: 21, name: "test-group" }]);
    showError.mockReset();
    showSuccess.mockReset();

    getAccountView.mockResolvedValue({
      user_id: 42,
      enabled: true,
      account_ids: [11],
      accounts: [
        {
          id: 11,
          name: "assigned-one",
          platform: "anthropic",
          account_type: "oauth",
          status: "active",
        },
      ],
    });
    listAccounts.mockResolvedValue({
      items: [
        {
          id: 11,
          name: "assigned-one",
          platform: "anthropic",
          type: "oauth",
          status: "active",
        },
        {
          id: 12,
          name: "candidate-two",
          platform: "openai",
          type: "apikey",
          status: "active",
        },
      ],
      total: 2,
      page: 1,
      page_size: 20,
      pages: 1,
    });
    updateAccountView.mockImplementation(
      async (_id: number, grant: Record<string, unknown>) => ({
        user_id: 42,
        enabled: grant.enabled,
        account_ids: grant.account_ids,
        accounts: [],
      }),
    );
  });

  it("loads the current grant and hides already assigned accounts from the picker", async () => {
    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(getAccountView).toHaveBeenCalledWith(42);
    expect(wrapper.get('[data-test="grant-enabled"]').element).toHaveProperty(
      "checked",
      true,
    );
    expect(wrapper.get('[data-test="assigned-11"]').text()).toContain(
      "assigned-one",
    );
    expect(wrapper.find('[data-test="candidate-11"]').exists()).toBe(false);
    expect(wrapper.find('[data-test="candidate-12"]').exists()).toBe(true);
    wrapper.unmount();
  });

  it("saves the complete assignment set so revoking is immediate", async () => {
    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    await wrapper.get('[data-test="remove-11"]').trigger("click");
    await wrapper.get('[data-test="add-12"]').trigger("click");
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [12],
    });
    expect(showSuccess).toHaveBeenCalledWith(
      "assignedAccounts.admin.saveSuccess",
    );
    wrapper.unmount();
  });

  it("can revoke everything by disabling the capability and clearing the set", async () => {
    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    await wrapper.get('[data-test="grant-enabled"]').setValue(false);
    await wrapper.get('[data-test="remove-11"]').trigger("click");
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: false,
      account_ids: [],
    });
    wrapper.unmount();
  });

  it("surfaces a save failure without closing the dialog", async () => {
    updateAccountView.mockRejectedValue({ status: 500, message: "boom" });

    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    expect(showError).toHaveBeenCalledTimes(1);
    expect(wrapper.find('[data-test="grant-dialog"]').exists()).toBe(true);
    wrapper.unmount();
  });

  // --- fail-closed 读取：不得沿用上一个用户的授权，也不得允许盲目保存 ---

  it("refuses to show or save another user’s state when the grant read fails", async () => {
    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();
    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true);

    // 打开第二个用户时读取失败：清空状态、禁用保存。
    getAccountView.mockRejectedValue({ status: 500, message: "boom" });
    await wrapper.setProps({ user: secondUser });
    await flushPromises();

    expect(getAccountView).toHaveBeenLastCalledWith(43);
    // 读取失败时不呈现任何授权状态（既不显示上一个用户的状态，也不显示空集合）。
    expect(wrapper.find('[data-test="grant-enabled"]').exists()).toBe(false);
    expect(wrapper.find('[data-test="assigned-11"]').exists()).toBe(false);
    expect(wrapper.find('[data-test="no-assigned"]').exists()).toBe(false);
    expect(wrapper.find('[data-test="retry-load"]').exists()).toBe(true);
    expect(
      wrapper.get('[data-test="save-grant"]').attributes("disabled"),
    ).toBeDefined();

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();
    expect(updateAccountView).not.toHaveBeenCalled();

    // 允许重试，重试成功后保存恢复可用。
    getAccountView.mockResolvedValue({
      user_id: 43,
      enabled: true,
      account_ids: [21],
      accounts: [
        {
          id: 21,
          name: "second-user-account",
          platform: "openai",
          account_type: "oauth",
          status: "active",
        },
      ],
    });
    await wrapper.get('[data-test="retry-load"]').trigger("click");
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(wrapper.get('[data-test="assigned-21"]').text()).toContain(
      "second-user-account",
    );
    expect(
      wrapper.get('[data-test="save-grant"]').attributes("disabled"),
    ).toBeUndefined();

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();
    expect(updateAccountView).toHaveBeenCalledWith(43, {
      enabled: true,
      account_ids: [21],
    });
    wrapper.unmount();
  });

  it("drops a slow response of the previous user instead of overwriting the current one", async () => {
    let resolveFirst!: (grant: unknown) => void;
    getAccountView.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveFirst = resolve;
        }),
    );

    const wrapper = mountModal();
    await flushPromises();

    // 立刻切到第二个用户，其读取先返回。
    getAccountView.mockResolvedValueOnce({
      user_id: 43,
      enabled: true,
      account_ids: [21],
      accounts: [
        {
          id: 21,
          name: "second-user-account",
          platform: "openai",
          account_type: "oauth",
          status: "active",
        },
      ],
    });
    await wrapper.setProps({ user: secondUser });
    await flushPromises();

    expect(wrapper.get('[data-test="assigned-21"]').exists()).toBe(true);

    // 第一个用户的响应姗姗来迟，必须被丢弃。
    resolveFirst({
      user_id: 42,
      enabled: true,
      account_ids: [11],
      accounts: [
        {
          id: 11,
          name: "assigned-one",
          platform: "anthropic",
          account_type: "oauth",
          status: "active",
        },
      ],
    });
    await flushPromises();

    expect(wrapper.find('[data-test="assigned-11"]').exists()).toBe(false);
    expect(wrapper.get('[data-test="assigned-21"]').exists()).toBe(true);
    wrapper.unmount();
  });

  // --- 授权集合以 account_ids 为准，缺详情也要保留 ---

  it("keeps assigned ids whose details are missing from the response", async () => {
    getAccountView.mockResolvedValue({
      user_id: 42,
      enabled: true,
      account_ids: [11, 99],
      accounts: [
        {
          id: 11,
          name: "assigned-one",
          platform: "anthropic",
          account_type: "oauth",
          status: "active",
        },
      ],
    });

    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(wrapper.get('[data-test="assigned-99"]').text()).toContain("#99");

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [11, 99],
    });
    wrapper.unmount();
  });

  it("does not blank the assigned list when the save echo omits account details", async () => {
    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [11],
    });
    expect(wrapper.get('[data-test="assigned-11"]').text()).toContain(
      "assigned-one",
    );
    wrapper.unmount();
  });

  // --- 同一用户关闭再打开：上一次会话的保存回显不得作用到新会话 ---

  it("drops a save echo that lands after the dialog was closed and reopened for the same user", async () => {
    const slowSave = createDeferred<Record<string, unknown>>();
    updateAccountView.mockImplementationOnce(() => slowSave.promise);

    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    // 旧会话提交了 [11, 12] 的保存，响应挂起。
    await wrapper.get('[data-test="add-12"]').trigger("click");
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();
    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [11, 12],
    });

    // 关闭再打开同一个用户：新会话的 GET 先返回，仍是服务端的 [11]。
    await wrapper.setProps({ show: false });
    await flushPromises();
    await wrapper.setProps({ show: true });
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(getAccountView).toHaveBeenCalledTimes(2);
    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true);
    expect(wrapper.find('[data-test="assigned-12"]').exists()).toBe(false);

    // 旧保存的回显姗姗来迟：不得改写新会话，也不得替新会话关闭弹窗。
    slowSave.resolve({
      user_id: 42,
      enabled: true,
      account_ids: [11, 12],
      accounts: [],
    });
    await flushPromises();

    expect(wrapper.find('[data-test="assigned-12"]').exists()).toBe(false);
    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true);
    expect(wrapper.find('[data-test="grant-dialog"]').exists()).toBe(true);
    expect(wrapper.emitted("close")).toBeUndefined();
    expect(showSuccess).not.toHaveBeenCalled();
    wrapper.unmount();
  });

  it("does not apply a save echo after the dialog was closed", async () => {
    const slowSave = createDeferred<Record<string, unknown>>();
    updateAccountView.mockImplementationOnce(() => slowSave.promise);

    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    await wrapper.get('[data-test="add-12"]').trigger("click");
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    await wrapper.setProps({ show: false });
    await flushPromises();

    slowSave.resolve({
      user_id: 42,
      enabled: true,
      account_ids: [11, 12],
      accounts: [],
    });
    await flushPromises();

    expect(wrapper.emitted("close")).toBeUndefined();
    expect(wrapper.emitted("success")).toBeUndefined();
    expect(showSuccess).not.toHaveBeenCalled();
    wrapper.unmount();
  });

  it("lets a save of the reopened session win over the previous session’s late save", async () => {
    const slowOldSave = createDeferred<Record<string, unknown>>();
    updateAccountView.mockImplementationOnce(() => slowOldSave.promise);

    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    // 旧会话的保存挂起（[11, 12]）。
    await wrapper.get('[data-test="add-12"]').trigger("click");
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    // 关闭再打开同一用户，新会话重新保存并先返回。
    await wrapper.setProps({ show: false });
    await flushPromises();
    await wrapper.setProps({ show: true });
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    updateAccountView.mockResolvedValueOnce({
      user_id: 42,
      enabled: true,
      account_ids: [11, 12],
      accounts: [],
    });
    await wrapper.get('[data-test="add-12"]').trigger("click");
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    expect(updateAccountView).toHaveBeenLastCalledWith(42, {
      enabled: true,
      account_ids: [11, 12],
    });
    expect(wrapper.get('[data-test="assigned-12"]').exists()).toBe(true);

    // 旧会话的保存姗姗来迟，带着与新会话不同的集合：必须被丢弃。
    slowOldSave.resolve({
      user_id: 42,
      enabled: true,
      account_ids: [99],
      accounts: [],
    });
    await flushPromises();

    expect(wrapper.find('[data-test="assigned-99"]').exists()).toBe(false);
    expect(wrapper.get('[data-test="assigned-12"]').exists()).toBe(true);
    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true);
    // 只有新会话那次保存产生了成功提示与关闭事件。
    expect(showSuccess).toHaveBeenCalledTimes(1);
    expect(wrapper.emitted("close")).toHaveLength(1);
    wrapper.unmount();
  });

  // --- 候选账号：远端搜索 + 服务端分页 ---

  it("applies platform, type, status and group filters without losing assigned IDs", async () => {
    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();
    expect(wrapper.find('[data-test="candidate-platform"]').exists()).toBe(
      true,
    );
    expect(wrapper.find('[data-test="candidate-type"]').exists()).toBe(true);
    expect(wrapper.find('[data-test="candidate-status"]').exists()).toBe(true);
    expect(wrapper.find('[data-test="candidate-group"]').exists()).toBe(true);

    await wrapper.get('[data-test="candidate-platform"]').setValue("openai");
    await wrapper.get('[data-test="candidate-type"]').setValue("upstream");
    await wrapper.get('[data-test="candidate-status"]').setValue("inactive");
    await wrapper.get('[data-test="candidate-group"]').setValue("21");
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();
    expect(listAccounts).toHaveBeenLastCalledWith(
      1,
      20,
      {
        platform: "openai",
        type: "upstream",
        status: "inactive",
        group: "21",
      },
      expect.anything(),
    );
    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true);
    wrapper.unmount();
  });

  it("ignores the previous filter page when switching platform mid-request", async () => {
    listAccounts.mockResolvedValueOnce(
      candidatePage(
        Array.from({ length: 20 }, (_, i) => i + 1),
        { total: 40 },
      ),
    );
    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    const slowOldPage = createDeferred<ReturnType<typeof candidatePage>>();
    listAccounts.mockImplementationOnce(() => slowOldPage.promise);
    await wrapper.get('[data-test="load-more-candidates"]').trigger("click");
    await flushPromises();

    listAccounts.mockResolvedValueOnce(candidatePage([201], { total: 21 }));
    await wrapper.get('[data-test="candidate-platform"]').setValue("openai");
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();
    expect(listAccounts).toHaveBeenLastCalledWith(
      1,
      20,
      { platform: "openai" },
      expect.anything(),
    );
    slowOldPage.resolve(candidatePage([999], { total: 40, page: 2 }));
    await flushPromises();
    expect(wrapper.find('[data-test="candidate-999"]').exists()).toBe(false);
    expect(wrapper.find('[data-test="candidate-201"]').exists()).toBe(true);
    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true);
    wrapper.unmount();
  });

  it("keeps existing assignments when group lookup fails", async () => {
    getGroups.mockRejectedValueOnce(new Error("groups offline"));
    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();
    expect(
      wrapper.get('[data-test="candidate-group"]').attributes("disabled"),
    ).toBeDefined();
    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true);
    expect(wrapper.get('[data-test="candidate-12"]').exists()).toBe(true);
    wrapper.unmount();
  });

  it("searches candidates on the server instead of filtering a first page locally", async () => {
    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(listAccounts).toHaveBeenLastCalledWith(1, 20, {}, expect.anything());

    listAccounts.mockResolvedValue({
      items: [
        {
          id: 201,
          name: "account-201",
          platform: "openai",
          type: "oauth",
          status: "active",
        },
      ],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1,
    });
    await wrapper.get('[data-test="account-search"]').setValue("201");
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(listAccounts).toHaveBeenLastCalledWith(
      1,
      20,
      { search: "201" },
      expect.anything(),
    );
    expect(wrapper.get('[data-test="candidate-201"]').text()).toContain(
      "account-201",
    );
    wrapper.unmount();
  });

  it("pages through candidates so accounts beyond the first page stay assignable", async () => {
    listAccounts.mockResolvedValueOnce({
      items: Array.from({ length: 20 }, (_, index) => ({
        id: index + 1,
        name: `page-one-${index + 1}`,
        platform: "openai",
        type: "oauth",
        status: "active",
      })),
      total: 21,
      page: 1,
      page_size: 20,
      pages: 2,
    });

    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(wrapper.find('[data-test="load-more-candidates"]').exists()).toBe(
      true,
    );

    listAccounts.mockResolvedValueOnce({
      items: [
        {
          id: 21,
          name: "twenty-first",
          platform: "gemini",
          type: "oauth",
          status: "active",
        },
      ],
      total: 21,
      page: 2,
      page_size: 20,
      pages: 2,
    });
    await wrapper.get('[data-test="load-more-candidates"]').trigger("click");
    await flushPromises();

    expect(listAccounts).toHaveBeenLastCalledWith(2, 20, {}, expect.anything());
    expect(wrapper.get('[data-test="candidate-21"]').text()).toContain(
      "twenty-first",
    );
    expect(wrapper.get('[data-test="candidate-1"]').exists()).toBe(true);
    expect(wrapper.find('[data-test="load-more-candidates"]').exists()).toBe(
      false,
    );
    wrapper.unmount();
  });

  it("drops a page-two response of the previous keyword when the search changes mid-flight", async () => {
    // 旧关键词：首页 20 条、总数 40，因此还有第 2 页。
    listAccounts.mockResolvedValueOnce({
      items: Array.from({ length: 20 }, (_, index) => ({
        id: index + 1,
        name: `old-page-one-${index + 1}`,
        platform: "openai",
        type: "oauth",
        status: "active",
      })),
      total: 40,
      page: 1,
      page_size: 20,
      pages: 2,
    });

    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    // 旧关键词的第 2 页挂起。
    const slowOldPageTwo = createDeferred<ReturnType<typeof candidatePage>>();
    listAccounts.mockImplementationOnce(() => slowOldPageTwo.promise);
    await wrapper.get('[data-test="load-more-candidates"]').trigger("click");
    await flushPromises();

    // 用户改搜 'B'：新关键词的第 1 页先返回。
    listAccounts.mockResolvedValueOnce(candidatePage([201], { total: 21 }));
    await wrapper.get('[data-test="account-search"]').setValue("B");
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(listAccounts).toHaveBeenLastCalledWith(
      1,
      20,
      { search: "B" },
      expect.anything(),
    );
    expect(wrapper.get('[data-test="candidate-201"]').exists()).toBe(true);

    // 旧关键词的第 2 页姗姗来迟：既不能追加进候选，也不能改写页数与总数。
    slowOldPageTwo.resolve(candidatePage([999], { total: 40, page: 2 }));
    await flushPromises();

    expect(wrapper.find('[data-test="candidate-999"]').exists()).toBe(false);
    expect(wrapper.find('[data-test="candidate-1"]').exists()).toBe(false);

    // 新关键词的第 2 页必须是它自己的第 2 页，而不是被旧响应顶到第 3 页。
    listAccounts.mockResolvedValueOnce(
      candidatePage([202], { total: 21, page: 2 }),
    );
    await wrapper.get('[data-test="load-more-candidates"]').trigger("click");
    await flushPromises();

    expect(listAccounts).toHaveBeenLastCalledWith(
      2,
      20,
      { search: "B" },
      expect.anything(),
    );
    expect(wrapper.get('[data-test="candidate-202"]').exists()).toBe(true);
    expect(wrapper.get('[data-test="candidate-201"]').exists()).toBe(true);
    expect(wrapper.find('[data-test="candidate-999"]').exists()).toBe(false);
    wrapper.unmount();
  });

  // --- 账号状态词表：手动停用（含历史 disabled）同一标签，非手动状态照常展示 ---

  it("labels legacy and current manual-disable statuses identically", async () => {
    getAccountView.mockResolvedValue({
      user_id: 42,
      enabled: true,
      account_ids: [11, 12, 13],
      accounts: [
        {
          id: 11,
          name: "legacy-disabled",
          platform: "anthropic",
          account_type: "oauth",
          status: "disabled",
        },
        {
          id: 12,
          name: "editor-inactive",
          platform: "anthropic",
          account_type: "oauth",
          status: "inactive",
        },
        {
          id: 13,
          name: "temp-unschedulable",
          platform: "openai",
          account_type: "oauth",
          status: "temp_unschedulable",
        },
      ],
    });

    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(wrapper.get('[data-test="assigned-11"]').text()).toContain(
      "admin.accounts.status.inactive",
    );
    expect(wrapper.get('[data-test="assigned-12"]').text()).toContain(
      "admin.accounts.status.inactive",
    );
    // 临时不可调度不是手动禁用：照常展示，且用其自身标签。
    expect(wrapper.get('[data-test="assigned-13"]').text()).toContain(
      "admin.accounts.status.tempUnschedulable",
    );
    wrapper.unmount();
  });

  it("keeps assigned ids when the picker pages are unavailable", async () => {
    listAccounts.mockResolvedValue(emptyCandidates());

    const wrapper = mountModal();
    await flushPromises();
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true);
    expect(
      wrapper.get('[data-test="save-grant"]').attributes("disabled"),
    ).toBeUndefined();
    wrapper.unmount();
  });

  // --- 候选多选：一键加入待授权列表（只合并本地草稿，保存时才写入） ---

  it("selects every listed candidate with the select-all control and clears the selection", async () => {
    listAccounts.mockResolvedValue(candidateList([11, 12, 13, 14]));

    const wrapper = mountModal();
    await settleModal();

    // 全选只作用于当前列出的候选：已分配的 11 不在候选里，因此不会被选中。
    await wrapper.get('[data-test="select-visible-candidates"]').setValue(true);
    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":3',
    );
    expect(
      wrapper.get('[data-test="candidate-select-12"]').element,
    ).toHaveProperty("checked", true);

    await wrapper.get('[data-test="clear-selection"]').trigger("click");
    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":0',
    );
    expect(
      wrapper.get('[data-test="candidate-select-12"]').element,
    ).toHaveProperty("checked", false);
    expect(wrapper.find('[data-test="clear-selection"]').exists()).toBe(false);
    expect(
      wrapper.get('[data-test="batch-add-selected"]').attributes("disabled"),
    ).toBeDefined();
    expect(updateAccountView).not.toHaveBeenCalled();
    wrapper.unmount();
  });

  it("merges multi-selected candidates into the pending list without writing to the server", async () => {
    listAccounts.mockResolvedValue(
      candidateList([11, 12, 13, 14], { statuses: { 13: "inactive" } }),
    );

    const wrapper = mountModal();
    await settleModal();

    // 停用账号照常可选，并明确标注状态；正常账号不额外加标记。
    expect(wrapper.get('[data-test="candidate-status-13"]').text()).toContain(
      "admin.accounts.status.inactive",
    );
    expect(wrapper.find('[data-test="candidate-status-12"]').exists()).toBe(
      false,
    );

    await wrapper.get('[data-test="candidate-select-12"]').setValue(true);
    await wrapper.get('[data-test="candidate-select-13"]').setValue(true);
    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":2',
    );

    await wrapper.get('[data-test="batch-add-selected"]').trigger("click");
    await flushPromises();

    // 一键加入只合并本地待授权列表，不发起任何写入，也不宣布授权已生效。
    expect(updateAccountView).not.toHaveBeenCalled();
    expect(showSuccess).not.toHaveBeenCalled();
    expect(showError).not.toHaveBeenCalled();
    expect(wrapper.get('[data-test="assigned-12"]').exists()).toBe(true);
    expect(wrapper.get('[data-test="assigned-13"]').exists()).toBe(true);
    expect(wrapper.get('[data-test="assigned-count"]').text()).toContain(
      '"count":3',
    );

    const summary = wrapper.get('[data-test="batch-add-summary"]').text();
    expect(summary).toContain("assignedAccounts.admin.batchAddSummary");
    expect(summary).toContain('"added":2');
    expect(summary).toContain('"skipped":0');
    expect(summary).toContain('"failed":0');
    expect(
      wrapper.get('[data-test="batch-add-pending-notice"]').text(),
    ).toContain("assignedAccounts.admin.batchAddPendingNotice");

    // 加入后它们不再出现在候选里，选择也随之清空。
    expect(wrapper.find('[data-test="candidate-12"]').exists()).toBe(false);
    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":0',
    );
    expect(
      wrapper.get('[data-test="batch-add-selected"]').attributes("disabled"),
    ).toBeDefined();

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();
    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [11, 12, 13],
    });
    wrapper.unmount();
  });

  it("reports selected candidates that are already pending instead of duplicating them", async () => {
    listAccounts.mockResolvedValue(candidateList([11, 12, 13]));

    const wrapper = mountModal();
    await settleModal();

    // 先选中 12，再用行内「添加」把它放进待授权列表：它仍留在选择里。
    await wrapper.get('[data-test="candidate-select-12"]').setValue(true);
    await wrapper.get('[data-test="add-12"]').trigger("click");
    await flushPromises();
    expect(wrapper.find('[data-test="candidate-12"]').exists()).toBe(false);
    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":1',
    );

    await wrapper.get('[data-test="batch-add-selected"]').trigger("click");
    await flushPromises();

    const summary = wrapper.get('[data-test="batch-add-summary"]').text();
    expect(summary).toContain('"added":0');
    expect(summary).toContain('"skipped":1');
    expect(summary).toContain('"failed":0');
    expect(wrapper.findAll('[data-test="assigned-12"]')).toHaveLength(1);
    expect(wrapper.get('[data-test="assigned-count"]').text()).toContain(
      '"count":2',
    );
    expect(updateAccountView).not.toHaveBeenCalled();
    wrapper.unmount();
  });

  it("keeps the selection across filter changes and load-more pages", async () => {
    listAccounts.mockResolvedValueOnce(
      candidateList(
        Array.from({ length: 20 }, (_, index) => index + 1),
        { total: 21 },
      ),
    );

    const wrapper = mountModal();
    await settleModal();

    await wrapper.get('[data-test="candidate-select-1"]').setValue(true);

    listAccounts.mockResolvedValueOnce(
      candidateList([21], { page: 2, total: 21 }),
    );
    await wrapper.get('[data-test="load-more-candidates"]').trigger("click");
    await flushPromises();
    await wrapper.get('[data-test="candidate-select-21"]').setValue(true);
    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":2',
    );

    // 切换筛选会整页替换候选，选择必须保留。
    listAccounts.mockResolvedValueOnce(candidateList([201]));
    await wrapper.get('[data-test="candidate-platform"]').setValue("openai");
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(listAccounts).toHaveBeenLastCalledWith(
      1,
      20,
      { platform: "openai" },
      expect.anything(),
    );
    expect(wrapper.find('[data-test="candidate-1"]').exists()).toBe(false);
    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":2',
    );

    // 筛选结果为空时，选择依旧可见（可清除、可一键加入）。
    listAccounts.mockResolvedValueOnce(emptyCandidates());
    await wrapper.get('[data-test="candidate-status"]').setValue("inactive");
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":2',
    );
    expect(
      wrapper
        .get('[data-test="select-visible-candidates"]')
        .attributes("disabled"),
    ).toBeDefined();

    await wrapper.get('[data-test="batch-add-selected"]').trigger("click");
    await flushPromises();

    // 跨筛选选中的账号仍能取到资料，而不是退化成占位 id。
    expect(wrapper.get('[data-test="assigned-1"]').text()).toContain(
      "candidate-1",
    );
    expect(wrapper.get('[data-test="assigned-21"]').text()).toContain(
      "candidate-21",
    );
    expect(wrapper.get('[data-test="batch-add-summary"]').text()).toContain(
      '"added":2',
    );
    expect(updateAccountView).not.toHaveBeenCalled();

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();
    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [11, 1, 21],
    });
    wrapper.unmount();
  });

  it("clears the selection when the target user changes or the dialog is reopened", async () => {
    listAccounts.mockResolvedValue(candidateList([11, 12, 22, 23]));

    const wrapper = mountModal();
    await settleModal();

    await wrapper.get('[data-test="candidate-select-12"]').setValue(true);
    await wrapper.get('[data-test="batch-add-selected"]').trigger("click");
    await flushPromises();
    await wrapper.get('[data-test="candidate-select-22"]').setValue(true);
    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":1',
    );

    getAccountView.mockResolvedValue({
      user_id: 43,
      enabled: true,
      account_ids: [21],
      accounts: [
        {
          id: 21,
          name: "second-user-account",
          platform: "openai",
          account_type: "oauth",
          status: "active",
        },
      ],
    });
    await wrapper.setProps({ user: secondUser });
    await settleModal();

    // 新用户从干净状态开始：没有选择，也没有上一个用户的合并结果。
    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":0',
    );
    expect(
      wrapper.get('[data-test="candidate-select-22"]').element,
    ).toHaveProperty("checked", false);
    expect(wrapper.find('[data-test="batch-add-summary"]').exists()).toBe(
      false,
    );

    await wrapper.get('[data-test="candidate-select-22"]').setValue(true);
    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":1',
    );

    // 关闭再打开同一个用户同样清空选择。
    await wrapper.setProps({ show: false });
    await flushPromises();
    await wrapper.setProps({ show: true });
    await settleModal();

    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":0',
    );
    expect(
      wrapper.get('[data-test="candidate-select-22"]').element,
    ).toHaveProperty("checked", false);
    wrapper.unmount();
  });

  it("keeps the pending list and the selection when the server rejects the save", async () => {
    listAccounts.mockResolvedValue(candidateList([11, 12, 13]));
    updateAccountView.mockRejectedValue({
      status: 400,
      message: "account 12 was deleted",
    });

    const wrapper = mountModal();
    await settleModal();

    await wrapper.get('[data-test="candidate-select-12"]').setValue(true);
    await wrapper.get('[data-test="batch-add-selected"]').trigger("click");
    await flushPromises();

    // 再选中一个尚未加入的候选账号，用来验证保存失败不会连带清空选择。
    await wrapper.get('[data-test="candidate-select-13"]').setValue(true);

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [11, 12],
    });
    // 整批都没有落地：不报告成功、不关闭弹窗，草稿与选择原样保留。
    expect(showError).toHaveBeenCalledTimes(1);
    expect(showSuccess).not.toHaveBeenCalled();
    expect(wrapper.emitted("close")).toBeUndefined();
    expect(wrapper.emitted("success")).toBeUndefined();
    expect(wrapper.get('[data-test="grant-dialog"]').exists()).toBe(true);
    expect(wrapper.get('[data-test="assigned-12"]').exists()).toBe(true);
    expect(wrapper.get('[data-test="assigned-count"]').text()).toContain(
      '"count":2',
    );
    expect(wrapper.get('[data-test="selection-count"]').text()).toContain(
      '"count":1',
    );
    expect(
      wrapper.get('[data-test="candidate-select-13"]').element,
    ).toHaveProperty("checked", true);
    expect(
      wrapper.get('[data-test="save-grant"]').attributes("disabled"),
    ).toBeUndefined();

    // 管理员就地修正后可以原样重试。
    updateAccountView.mockResolvedValue({
      user_id: 42,
      enabled: true,
      account_ids: [11, 12],
      accounts: [],
    });
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    expect(showSuccess).toHaveBeenCalledWith(
      "assignedAccounts.admin.saveSuccess",
    );
    wrapper.unmount();
  });

  // --- 保存被整批拒绝：必须点名具体失败项，草稿与勾选保留 ---

  it("marks the exact pending accounts the server rejected and keeps the draft", async () => {
    listAccounts.mockResolvedValue(candidateList([11, 12, 13]));
    // 12 在保存前被删除：服务端整批回滚，并在错误元数据里点名具体的失效 id。
    updateAccountView.mockRejectedValue({
      status: 400,
      code: 400,
      reason: "UNKNOWN_ACCOUNT",
      message: "One or more account ids do not exist",
      metadata: { invalid_account_ids: "12,999", invalid_account_count: "2" },
    });

    const wrapper = mountModal();
    await settleModal();

    await wrapper.get('[data-test="candidate-select-12"]').setValue(true);
    await wrapper.get('[data-test="candidate-select-13"]').setValue(true);
    await wrapper.get('[data-test="batch-add-selected"]').trigger("click");
    await flushPromises();
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [11, 12, 13],
    });
    // 整批都没有落地：不报成功、不关闭弹窗。
    expect(showSuccess).not.toHaveBeenCalled();
    expect(showError).toHaveBeenCalledTimes(1);
    expect(wrapper.emitted("close")).toBeUndefined();
    expect(wrapper.get('[data-test="grant-dialog"]').exists()).toBe(true);

    // 只有服务端点名的 12 被标出：13 与既有 11 不受牵连。
    expect(wrapper.get('[data-test="assigned-invalid-12"]').exists()).toBe(
      true,
    );
    expect(wrapper.find('[data-test="assigned-invalid-13"]').exists()).toBe(
      false,
    );
    expect(wrapper.find('[data-test="assigned-invalid-11"]').exists()).toBe(
      false,
    );
    const notice = wrapper.get('[data-test="save-invalid-accounts"]').text();
    expect(notice).toContain("assignedAccounts.admin.saveUnknownAccounts");
    expect(notice).toContain('"ids":"12"');

    // 草稿原样保留（包含被点名的 12），管理员可以就地移除它后重试。
    expect(wrapper.get('[data-test="assigned-12"]').exists()).toBe(true);
    expect(wrapper.get('[data-test="assigned-13"]').exists()).toBe(true);
    expect(wrapper.get('[data-test="assigned-count"]').text()).toContain(
      '"count":3',
    );
    expect(
      wrapper.get('[data-test="save-grant"]').attributes("disabled"),
    ).toBeUndefined();

    // 移除被点名的账号后提示随之消失，重试只提交剩下的内容。
    await wrapper.get('[data-test="remove-12"]').trigger("click");
    await flushPromises();
    expect(wrapper.find('[data-test="save-invalid-accounts"]').exists()).toBe(
      false,
    );

    updateAccountView.mockResolvedValue({
      user_id: 42,
      enabled: true,
      account_ids: [11, 13],
      accounts: [],
    });
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();
    expect(updateAccountView).toHaveBeenLastCalledWith(42, {
      enabled: true,
      account_ids: [11, 13],
    });
    expect(showSuccess).toHaveBeenCalledWith(
      "assignedAccounts.admin.saveSuccess",
    );
    wrapper.unmount();
  });

  it("falls back to the generic failure notice when the server names no account", async () => {
    updateAccountView.mockRejectedValue({ status: 500, message: "boom" });

    const wrapper = mountModal();
    await settleModal();

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    expect(showError).toHaveBeenCalledTimes(1);
    expect(wrapper.find('[data-test="save-invalid-accounts"]').exists()).toBe(
      false,
    );
    expect(wrapper.get('[data-test="assigned-11"]').exists()).toBe(true);
    wrapper.unmount();
  });

  it("clears a stale batch-add summary once the pending list is edited by hand", async () => {
    listAccounts.mockResolvedValue(candidateList([11, 12, 13]));

    const wrapper = mountModal();
    await settleModal();

    await wrapper.get('[data-test="candidate-select-12"]').setValue(true);
    await wrapper.get('[data-test="batch-add-selected"]').trigger("click");
    await flushPromises();
    expect(wrapper.find('[data-test="batch-add-summary"]').exists()).toBe(true);

    // 逐行添加改动了待授权列表：上一次合并的计数不再描述当前草稿。
    await wrapper.get('[data-test="add-13"]').trigger("click");
    await flushPromises();
    expect(wrapper.find('[data-test="batch-add-summary"]').exists()).toBe(
      false,
    );
    expect(wrapper.get('[data-test="assigned-13"]').exists()).toBe(true);
    expect(updateAccountView).not.toHaveBeenCalled();

    wrapper.unmount();
  });

  it("never drops server-side assignments that are outside the current candidate filter", async () => {
    getAccountView.mockResolvedValue({
      user_id: 42,
      enabled: true,
      account_ids: [11, 99],
      accounts: [
        {
          id: 11,
          name: "assigned-one",
          platform: "anthropic",
          account_type: "oauth",
          status: "active",
        },
      ],
    });
    listAccounts.mockResolvedValue(candidateList([12, 13]));

    const wrapper = mountModal();
    await settleModal();

    await wrapper.get('[data-test="candidate-platform"]').setValue("openai");
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    await wrapper.get('[data-test="candidate-select-12"]').setValue(true);
    await wrapper.get('[data-test="batch-add-selected"]').trigger("click");
    await flushPromises();
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    // 带详情的 11 与只有 id 的 99 都在提交集合里：筛选与多选都不会丢掉既有授权。
    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [11, 99, 12],
    });
    wrapper.unmount();
  });
});
