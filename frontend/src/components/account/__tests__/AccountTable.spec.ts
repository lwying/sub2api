import { describe, expect, it, vi } from "vitest";
import { mount } from "@vue/test-utils";
import { defineComponent } from "vue";
import AccountTable from "../AccountTable.vue";
import type { AccountRuntimeSnapshot } from "../accountDisplay";

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  };
});

// DataTable 桩：把共享默认单元格插槽按行展开，便于断言默认分支真实取数。
const DataTableStub = defineComponent({
  name: "DataTable",
  props: ["data", "columns", "loading"],
  template: `
    <div>
      <slot name="cell-capacity" :row="data[0]" :value="data[0]?.id" />
      <slot name="cell-status" :row="data[0]" :value="data[0]?.status" />
      <slot name="cell-groups" :row="data[0]" :value="data[0]?.groups" />
      <slot name="cell-today_stats" :row="data[0]" :value="data[0]?.id" />
      <slot name="cell-usage" :row="data[0]" :value="data[0]?.id" />
      <slot name="cell-name" :row="data[0]" :value="data[0]?.name" />
    </div>`,
});

const row = {
  id: 7,
  name: "acct",
  platform: "anthropic",
  type: "oauth",
  status: "active",
  concurrency: 5,
  current_concurrency: 2,
  schedulable: true,
  groups: [{ id: 1, name: "g1" }],
};

function mountTable(props: Record<string, unknown> = {}) {
  return mount(AccountTable, {
    props: {
      data: [row],
      columns: [],
      ...props,
    },
    global: {
      stubs: {
        DataTable: DataTableStub,
        AccountCapacityCell: {
          name: "AccountCapacityCell",
          props: ["account", "readOnly"],
          template: "<span data-test='capacity' />",
        },
        AccountStatusIndicator: {
          name: "AccountStatusIndicator",
          props: ["account", "readOnly"],
          template: "<span data-test='status' />",
        },
        AccountGroupsCell: {
          name: "AccountGroupsCell",
          props: ["groups", "maxDisplay"],
          template: "<span data-test='groups' />",
        },
        AccountTodayStatsCell: {
          name: "AccountTodayStatsCell",
          props: ["stats", "loading", "error"],
          template: "<span data-test='today' />",
        },
        AccountUsageCell: {
          name: "AccountUsageCell",
          props: ["account", "readOnly", "batchedUsage"],
          template: "<span data-test='usage' />",
        },
      },
    },
  });
}

const runtime: Record<string, AccountRuntimeSnapshot> = {
  "7": {
    usage: null,
    today_stats: { requests: 3, tokens: 4, cost: 1 },
    current_concurrency: null,
  },
};

describe("AccountTable shared default cells", () => {
  it("默认只读：capacity/status/usage 收到 readOnly=true", () => {
    const wrapper = mountTable({ runtimeById: runtime });
    expect(
      wrapper.findComponent({ name: "AccountCapacityCell" }).props("readOnly"),
    ).toBe(true);
    expect(
      wrapper
        .findComponent({ name: "AccountStatusIndicator" })
        .props("readOnly"),
    ).toBe(true);
    expect(
      wrapper.findComponent({ name: "AccountUsageCell" }).props("readOnly"),
    ).toBe(true);
  });

  it("管理页 readOnly=false 时默认单元格保留真实管理语义", () => {
    const wrapper = mountTable({ runtimeById: runtime, readOnly: false });
    const capacity = wrapper.findComponent({ name: "AccountCapacityCell" });
    expect(capacity.props("readOnly")).toBe(false);
    expect(capacity.props("account")).toMatchObject({
      current_concurrency: 2,
      concurrency: 5,
    });
    expect(
      wrapper.findComponent({ name: "AccountUsageCell" }).props("readOnly"),
    ).toBe(false);
  });

  it("摘要行状态继续使用完整的本地化标签", () => {
    const wrapper = mountTable({
      data: [
        {
          id: 7,
          name: "acct",
          platform: "anthropic",
          type: "oauth",
          status: " RATE_LIMITED ",
        },
      ],
    });
    expect(wrapper.text()).toContain("admin.accounts.status.rateLimited");
    expect(wrapper.text()).not.toContain("RATE_LIMITED");
  });

  it("默认 groups 单元格消费行上的 groups", () => {
    const wrapper = mountTable({ runtimeById: runtime });
    expect(
      wrapper.findComponent({ name: "AccountGroupsCell" }).props("groups"),
    ).toEqual([{ id: 1, name: "g1" }]);
  });

  it("默认 today_stats 单元格消费 runtimeById + loading/error", () => {
    const wrapper = mountTable({
      runtimeById: runtime,
      runtimeLoading: true,
      runtimeError: true,
    });
    const today = wrapper.findComponent({ name: "AccountTodayStatsCell" });
    expect(today.props("stats")).toMatchObject({ requests: 3 });
    expect(today.props("loading")).toBe(true);
    expect(today.props("error")).toBe("common.error");
  });

  it("默认 usage 单元格消费 runtimeById 的被动快照", () => {
    const runtimeWithUsage: Record<string, AccountRuntimeSnapshot> = {
      "7": {
        usage: {
          updated_at: null,
          five_hour: null,
          seven_day: null,
          seven_day_sonnet: null,
        },
        today_stats: null,
        current_concurrency: null,
      },
    };
    const wrapper = mountTable({ runtimeById: runtimeWithUsage });
    expect(
      wrapper.findComponent({ name: "AccountUsageCell" }).props("batchedUsage"),
    ).toMatchObject({ updated_at: null });
  });
});
