import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount } from "@vue/test-utils";

import type { AdminUser } from "@/types";
import UserAssignedAccountsModal from "../UserAssignedAccountsModal.vue";

const {
  getAccountView,
  updateAccountView,
  listOptions,
  getGroups,
  showError,
  showSuccess,
} = vi.hoisted(() => ({
  getAccountView: vi.fn(),
  updateAccountView: vi.fn(),
  listOptions: vi.fn(),
  getGroups: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}));

vi.mock("@/api/admin", () => ({
  adminAPI: {
    users: { getAccountView, updateAccountView },
    accounts: { listOptions },
    groups: { getAll: getGroups },
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

/**
 * 用一个轻量 harness 代替真实的 AccountTableFilters：只负责按契约回传筛选事件，
 * 让行为测试聚焦在弹窗的筛选/选择/保存状态机上，不绑定父组件内部实现。
 */
const FiltersStub = {
  name: "AccountTableFilters",
  props: [
    "searchQuery",
    "filters",
    "groups",
    "visibleFields",
    "typeOptions",
    "statusOptions",
  ],
  emits: ["update:searchQuery", "update:filters", "change"],
  template: `
    <div data-test="filters-stub">
      <span data-test="stub-visible-fields">{{ visibleFields.join(',') }}</span>
      <span data-test="stub-group-value">{{ filters.group }}</span>
      <span data-test="stub-status-value">{{ filters.status }}</span>
      <span data-test="stub-has-status-options">{{ statusOptions ? 'yes' : 'no' }}</span>
      <span data-test="stub-type-values">{{ (typeOptions || []).map(o => o.value).join(',') }}</span>
      <button type="button" data-test="stub-platform" @click="pick('platform','openai')" />
      <button type="button" data-test="stub-status" @click="pick('status','inactive')" />
      <button type="button" data-test="stub-status-derived" @click="pick('status','rate_limited')" />
      <button type="button" data-test="stub-group" @click="pick('group','21')" />
      <button type="button" data-test="stub-search" @click="emitSearch" />
    </div>
  `,
  methods: {
    pick(key: string, value: string) {
      this.$emit("update:filters", { ...this.filters, [key]: value });
      this.$emit("change");
    },
    emitSearch() {
      this.$emit("update:searchQuery", "match");
      this.$emit("change");
    },
  },
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
        AccountTableFilters: FiltersStub,
      },
    },
  });

interface OptionItem {
  id: number;
  name: string;
  platform: string;
  type: string;
  status: string;
}

function option(id: number, overrides: Partial<OptionItem> = {}): OptionItem {
  return {
    id,
    name: `account-${id}`,
    platform: "openai",
    type: "oauth",
    status: "active",
    ...overrides,
  };
}

function pageOf(
  items: OptionItem[],
  options: { total?: number; page?: number; pageSize?: number } = {},
) {
  const pageSize = options.pageSize ?? 20;
  const total = options.total ?? items.length;
  return {
    items,
    total,
    page: options.page ?? 1,
    page_size: pageSize,
    pages: Math.max(1, Math.ceil(total / pageSize)),
  };
}

function grantOf(ids: number[], enabled = true) {
  return {
    user_id: 42,
    enabled,
    account_ids: ids,
    accounts: ids.map((id) => ({
      id,
      name: `assigned-${id}`,
      platform: "anthropic",
      account_type: "oauth",
      status: "active",
    })),
  };
}

async function settle(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await flushPromises();
}

function rowCheckbox(wrapper: ReturnType<typeof mountModal>, id: number) {
  return wrapper.get(`[data-row-id="${id}"] [data-test="select-row"]`);
}

describe("UserAssignedAccountsModal", () => {
  beforeEach(() => {
    getAccountView.mockReset();
    updateAccountView.mockReset();
    listOptions.mockReset();
    getGroups.mockReset();
    showError.mockReset();
    showSuccess.mockReset();

    getAccountView.mockResolvedValue(grantOf([11]));
    listOptions.mockResolvedValue(
      pageOf([
        option(11, { name: "assigned-one", platform: "anthropic" }),
        option(12, { name: "candidate-two", type: "apikey" }),
      ]),
    );
    getGroups.mockResolvedValue([{ id: 21, name: "test-group" }]);
    updateAccountView.mockImplementation(
      async (
        _id: number,
        grant: { enabled: boolean; account_ids: number[] },
      ) => ({
        user_id: 42,
        enabled: grant.enabled,
        account_ids: grant.account_ids,
        accounts: [],
      }),
    );
  });

  it("loads the grant into the selection and lists it on the selected tab", async () => {
    const wrapper = mountModal();
    await settle();

    expect(getAccountView).toHaveBeenCalledWith(42);
    expect(
      wrapper.get('[data-test="grant-enabled"]').attributes("aria-checked"),
    ).toBe("true");
    // 无改动：不显示脏状态，保存按钮禁用
    expect(wrapper.find('[data-test="dirty-state"]').exists()).toBe(false);
    expect(
      wrapper.get('[data-test="save-grant"]').attributes("disabled"),
    ).toBeDefined();

    await wrapper.get('[data-test="tab-selected"]').trigger("click");
    await settle();
    expect(wrapper.get('[data-test="draft-summary"]').text()).toContain(
      '"count":1',
    );
    expect(wrapper.text()).toContain("assigned-11");
    expect(wrapper.find('[data-test="no-selected"]').exists()).toBe(false);
    wrapper.unmount();
  });

  it("edits the draft with row checkboxes and saves one unified {enabled, account_ids}", async () => {
    getAccountView.mockResolvedValue(grantOf([], false));
    const wrapper = mountModal();
    await settle();

    await wrapper.get('[data-test="grant-enabled"]').trigger("click");
    await rowCheckbox(wrapper, 11).setValue(true);
    await settle();

    expect(wrapper.get('[data-test="selected-count"]').text()).toContain(
      '"count":1',
    );
    expect(wrapper.get('[data-test="dirty-state"]').exists()).toBe(true);
    expect(
      wrapper.get('[data-test="save-grant"]').attributes("disabled"),
    ).toBeUndefined();

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await settle();

    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [11],
    });
    expect(showSuccess).toHaveBeenCalledWith(
      "assignedAccounts.admin.saveSuccess",
    );
    wrapper.unmount();
  });

  it("keeps selection across server pages and saves ids from both pages", async () => {
    getAccountView.mockResolvedValue(grantOf([], true));
    listOptions.mockImplementation(async (page: number) => {
      if (page === 1) {
        return pageOf(
          Array.from({ length: 20 }, (_, i) => option(i + 1)),
          { total: 21, page: 1 },
        );
      }
      return pageOf([option(21)], { total: 21, page: 2 });
    });

    const wrapper = mountModal();
    await settle();

    await rowCheckbox(wrapper, 1).setValue(true);
    await wrapper.get('button[aria-label="pagination.next"]').trigger("click");
    await settle();
    expect(listOptions).toHaveBeenLastCalledWith(2, 20, {}, expect.anything());

    await rowCheckbox(wrapper, 21).setValue(true);
    await settle();
    expect(wrapper.get('[data-test="selected-count"]').text()).toContain(
      '"count":2',
    );

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await settle();
    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [1, 21],
    });
    wrapper.unmount();
  });

  it("derives the selected tab from the draft and keeps a placeholder for missing details", async () => {
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
    await settle();

    await wrapper.get('[data-test="tab-selected"]').trigger("click");
    await settle();

    expect(wrapper.get('[data-test="draft-summary"]').text()).toContain(
      '"count":2',
    );
    // 缺详情的 99 保留为占位，绝不静默丢授权
    expect(wrapper.get('[data-test="placeholder-99"]').exists()).toBe(true);
    expect(wrapper.text()).toContain("#99");

    // 显式移除占位项
    await wrapper.get('[data-test="remove-99"]').trigger("click");
    await settle();
    expect(wrapper.find('[data-test="placeholder-99"]').exists()).toBe(false);
    expect(wrapper.get('[data-test="draft-summary"]').text()).toContain(
      '"count":1',
    );

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await settle();
    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [11],
    });
    wrapper.unmount();
  });

  it("disabling the capability keeps the assigned ids", async () => {
    const wrapper = mountModal();
    await settle();

    await wrapper.get('[data-test="grant-enabled"]').trigger("click");
    await settle();
    expect(wrapper.get('[data-test="disabled-hint"]').exists()).toBe(true);

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await settle();
    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: false,
      account_ids: [11],
    });
    wrapper.unmount();
  });

  it("cancel writes nothing", async () => {
    const wrapper = mountModal();
    await settle();

    await rowCheckbox(wrapper, 12).setValue(true);
    await settle();
    await wrapper.get('[data-test="cancel-grant"]').trigger("click");
    await settle();

    expect(updateAccountView).not.toHaveBeenCalled();
    expect(wrapper.emitted("close")).toHaveLength(1);
    wrapper.unmount();
  });

  it("all accounts list stays within the picker filter subset", async () => {
    const wrapper = mountModal();
    await settle();

    expect(wrapper.get('[data-test="stub-visible-fields"]').text()).toBe(
      "platform,type,status,group",
    );

    await wrapper.get('[data-test="stub-platform"]').trigger("click");
    await settle();
    expect(listOptions).toHaveBeenLastCalledWith(
      1,
      20,
      { platform: "openai" },
      expect.anything(),
    );

    await wrapper.get('[data-test="stub-status"]').trigger("click");
    await settle();
    expect(listOptions).toHaveBeenLastCalledWith(
      1,
      20,
      { platform: "openai", status: "inactive" },
      expect.anything(),
    );

    // 选择跨筛选保留
    await rowCheckbox(wrapper, 12).setValue(true);
    await settle();
    await wrapper.get('[data-test="stub-search"]').trigger("click");
    await settle();
    expect(wrapper.get('[data-test="selected-count"]').text()).toContain(
      '"count":2',
    );
    wrapper.unmount();
  });

  it("narrows the selected-tab filters and matches legacy disabled as inactive", async () => {
    getAccountView.mockResolvedValue({
      user_id: 42,
      enabled: true,
      account_ids: [11, 12],
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
          name: "still-active",
          platform: "openai",
          account_type: "oauth",
          status: "active",
        },
      ],
    });

    const wrapper = mountModal();
    await settle();

    // 全部账号：完整服务端语义，含分组与全部类型目录，不注入收窄的 status options
    expect(wrapper.get('[data-test="stub-visible-fields"]').text()).toBe(
      "platform,type,status,group",
    );
    expect(wrapper.get('[data-test="stub-has-status-options"]').text()).toBe(
      "no",
    );
    const typeValues = wrapper.get('[data-test="stub-type-values"]').text();
    expect(typeValues).toContain("upstream");
    expect(typeValues).toContain("service_account");

    // 在全部账号先选分组与派生状态，切到已选页签时应被清除且不参与匹配
    await wrapper.get('[data-test="stub-group"]').trigger("click");
    await wrapper.get('[data-test="stub-status-derived"]').trigger("click");
    await settle();
    expect(wrapper.get('[data-test="stub-group-value"]').text()).toBe("21");
    expect(wrapper.get('[data-test="stub-status-value"]').text()).toBe(
      "rate_limited",
    );

    await wrapper.get('[data-test="tab-selected"]').trigger("click");
    await settle();

    // 已选页签：隐藏不支持的分组，注入库内状态候选，并清掉不可匹配的残留
    expect(wrapper.get('[data-test="stub-visible-fields"]').text()).toBe(
      "platform,type,status",
    );
    expect(wrapper.get('[data-test="stub-has-status-options"]').text()).toBe(
      "yes",
    );
    expect(wrapper.get('[data-test="stub-group-value"]').text()).toBe("");
    expect(wrapper.get('[data-test="stub-status-value"]').text()).toBe("");

    // 按「已停用」筛选：存储为 disabled 的行仍命中 inactive，正常 active 行被过滤
    await wrapper.get('[data-test="stub-status"]').trigger("click");
    await settle();
    expect(wrapper.find('[data-row-id="11"]').exists()).toBe(true);
    expect(wrapper.find('[data-row-id="12"]').exists()).toBe(false);
    // 草稿集合不受本地展示筛选影响
    expect(wrapper.get('[data-test="selected-count"]').text()).toContain(
      '"count":2',
    );
    listOptions.mockClear();
    await wrapper.get('[data-test="tab-all"]').trigger("click");
    await settle();
    expect(listOptions).toHaveBeenCalledWith(
      1,
      expect.any(Number),
      expect.objectContaining({ status: "inactive" }),
      expect.anything(),
    );
    wrapper.unmount();
  });

  it("select all matching collects every page then merges once", async () => {
    getAccountView.mockResolvedValue(grantOf([], true));
    listOptions.mockImplementation(async (page: number) => {
      if (page === 1) {
        return pageOf([option(1), option(2)], { total: 3, page: 1 });
      }
      return pageOf([option(3)], { total: 3, page: 2 });
    });

    const wrapper = mountModal();
    await settle();

    await wrapper.get('[data-test="select-all-matching"]').trigger("click");
    await settle();

    expect(wrapper.get('[data-test="select-all-notice"]').text()).toContain(
      "assignedAccounts.admin.selectAllDone",
    );
    expect(wrapper.get('[data-test="selected-count"]').text()).toContain(
      '"count":3',
    );

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await settle();
    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [1, 2, 3],
    });
    wrapper.unmount();
  });

  it("cancelling select-all matching merges nothing", async () => {
    getAccountView.mockResolvedValue(grantOf([], true));
    let resolveSecond!: (value: ReturnType<typeof pageOf>) => void;
    const secondPage = new Promise<ReturnType<typeof pageOf>>((resolve) => {
      resolveSecond = resolve;
    });
    listOptions.mockImplementation(async (page: number) => {
      if (page === 1) return pageOf([option(1), option(2)], { total: 3 });
      return secondPage;
    });

    const wrapper = mountModal();
    await settle();

    await wrapper.get('[data-test="select-all-matching"]').trigger("click");
    await flushPromises();
    await wrapper.get('[data-test="cancel-select-all"]').trigger("click");
    resolveSecond(pageOf([option(3)], { total: 3, page: 2 }));
    await settle();

    expect(wrapper.get('[data-test="selected-count"]').text()).toContain(
      '"count":0',
    );
    expect(wrapper.find('[data-test="select-all-notice"]').exists()).toBe(
      false,
    );
    wrapper.unmount();
  });

  it("changing a filter aborts a running select-all matching instead of mixing sets", async () => {
    getAccountView.mockResolvedValue(grantOf([], true));
    let resolveSecond!: (value: ReturnType<typeof pageOf>) => void;
    const secondPage = new Promise<ReturnType<typeof pageOf>>((resolve) => {
      resolveSecond = resolve;
    });
    listOptions.mockImplementation(async (page: number) => {
      if (page === 1) return pageOf([option(1), option(2)], { total: 3 });
      return secondPage;
    });

    const wrapper = mountModal();
    await settle();

    await wrapper.get('[data-test="select-all-matching"]').trigger("click");
    await flushPromises();
    // 第 2 页请求发出时用的是本次启动的快照筛选（空）
    const pageTwoCall = listOptions.mock.calls.find((call) => call[0] === 2);
    expect(pageTwoCall?.[2]).toEqual({});

    // 运行期间切换筛选：必须 abort + 作废，旧结果不得落入新筛选下的草稿
    await wrapper.get('[data-test="stub-platform"]').trigger("click");
    await flushPromises();
    resolveSecond(pageOf([option(3)], { total: 3, page: 2 }));
    await settle();

    expect(wrapper.get('[data-test="selected-count"]').text()).toContain(
      '"count":0',
    );
    expect(wrapper.find('[data-test="select-all-notice"]').exists()).toBe(
      false,
    );
    wrapper.unmount();
  });

  it("a failed select-all matching does not partially merge", async () => {
    getAccountView.mockResolvedValue(grantOf([], true));
    listOptions.mockImplementation(async (page: number) => {
      if (page === 1) return pageOf([option(1), option(2)], { total: 3 });
      throw new Error("boom");
    });

    const wrapper = mountModal();
    await settle();

    await wrapper.get('[data-test="select-all-matching"]').trigger("click");
    await settle();

    expect(wrapper.get('[data-test="select-all-error"]').text()).toContain(
      "assignedAccounts.admin.selectAllFailed",
    );
    expect(wrapper.get('[data-test="selected-count"]').text()).toContain(
      '"count":0',
    );
    wrapper.unmount();
  });

  it("refuses to show or save another user's state when the grant read fails", async () => {
    const wrapper = mountModal();
    await settle();

    getAccountView.mockRejectedValue({ status: 500, message: "boom" });
    await wrapper.setProps({ user: secondUser });
    await settle();

    expect(getAccountView).toHaveBeenLastCalledWith(43);
    expect(wrapper.find('[data-test="grant-enabled"]').exists()).toBe(false);
    expect(wrapper.find('[data-test="retry-load"]').exists()).toBe(true);
    expect(
      wrapper.get('[data-test="save-grant"]').attributes("disabled"),
    ).toBeDefined();

    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await settle();
    expect(updateAccountView).not.toHaveBeenCalled();
    wrapper.unmount();
  });

  it("drops a slow response of the previous user instead of overwriting the current one", async () => {
    let resolveFirst!: (grant: unknown) => void;
    getAccountView.mockImplementationOnce(
      () => new Promise((resolve) => (resolveFirst = resolve)),
    );

    const wrapper = mountModal();
    await flushPromises();

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
    await settle();

    resolveFirst(grantOf([11]));
    await settle();

    await wrapper.get('[data-test="tab-selected"]').trigger("click");
    await settle();
    expect(wrapper.text()).toContain("second-user-account");
    expect(wrapper.text()).not.toContain("assigned-11");
    wrapper.unmount();
  });

  it("keeps the draft and marks server-rejected ids on save failure", async () => {
    updateAccountView.mockRejectedValue({
      status: 400,
      code: 400,
      message: "One or more account ids do not exist",
      metadata: { invalid_account_ids: "12", invalid_account_count: "1" },
    });

    const wrapper = mountModal();
    await settle();

    await rowCheckbox(wrapper, 12).setValue(true);
    await settle();
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await settle();

    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [11, 12],
    });
    expect(showSuccess).not.toHaveBeenCalled();
    expect(wrapper.emitted("close")).toBeUndefined();
    // 草稿保留，仅点名服务端返回的失效项
    expect(wrapper.get('[data-test="selected-count"]').text()).toContain(
      '"count":2',
    );
    expect(wrapper.get('[data-test="invalid-12"]').exists()).toBe(true);
    expect(wrapper.find('[data-test="invalid-11"]').exists()).toBe(false);

    // 移除入口在已选页签
    await wrapper.get('[data-test="tab-selected"]').trigger("click");
    await settle();
    await wrapper.get('[data-test="remove-12"]').trigger("click");
    await settle();
    expect(wrapper.find('[data-test="invalid-12"]').exists()).toBe(false);

    // 移除后已回到服务端原始集合（不算脏，保存禁用）；再做一处改动以便重试
    await wrapper.get('[data-test="grant-enabled"]').trigger("click");
    await settle();
    updateAccountView.mockResolvedValue({
      user_id: 42,
      enabled: false,
      account_ids: [11],
      accounts: [],
    });
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await settle();
    expect(updateAccountView).toHaveBeenLastCalledWith(42, {
      enabled: false,
      account_ids: [11],
    });
    expect(showSuccess).toHaveBeenCalledWith(
      "assignedAccounts.admin.saveSuccess",
    );
    wrapper.unmount();
  });

  it("mounts with the real shared AccountTableFilters component", async () => {
    const wrapper = mount(UserAssignedAccountsModal, {
      props: { show: true, user: firstUser },
      global: {
        stubs: { BaseDialog: BaseDialogStub, LoadingSpinner: true },
      },
    });
    await settle();

    expect(wrapper.find('[data-row-id="11"]').exists()).toBe(true);
    wrapper.unmount();
  });

  it("respects the server echo on save instead of forcing the submitted ids", async () => {
    updateAccountView.mockResolvedValue({
      user_id: 42,
      enabled: true,
      account_ids: [11],
      accounts: [],
    });

    const wrapper = mountModal();
    await settle();

    await rowCheckbox(wrapper, 12).setValue(true);
    await settle();
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await settle();

    // 提交 [11,12]，服务端只保留 [11]：界面以服务端为准
    expect(updateAccountView).toHaveBeenCalledWith(42, {
      enabled: true,
      account_ids: [11, 12],
    });
    expect(wrapper.get('[data-test="selected-count"]').text()).toContain(
      '"count":1',
    );
    expect(wrapper.find('[data-test="dirty-state"]').exists()).toBe(false);
    expect(
      wrapper.get('[data-test="save-grant"]').attributes("disabled"),
    ).toBeDefined();
    expect(showSuccess).toHaveBeenCalledWith(
      "assignedAccounts.admin.saveSuccess",
    );
    wrapper.unmount();
  });

  it("drops a save echo that lands after the dialog was closed", async () => {
    let resolveSave!: (value: unknown) => void;
    updateAccountView.mockImplementationOnce(
      () => new Promise((resolve) => (resolveSave = resolve)),
    );

    const wrapper = mountModal();
    await settle();

    await rowCheckbox(wrapper, 12).setValue(true);
    await settle();
    await wrapper.get('[data-test="save-grant"]').trigger("click");
    await flushPromises();

    await wrapper.setProps({ show: false });
    await flushPromises();

    resolveSave({
      user_id: 42,
      enabled: true,
      account_ids: [11, 12],
      accounts: [],
    });
    await settle();

    expect(wrapper.emitted("close")).toBeUndefined();
    expect(wrapper.emitted("success")).toBeUndefined();
    expect(showSuccess).not.toHaveBeenCalled();
    wrapper.unmount();
  });
});
