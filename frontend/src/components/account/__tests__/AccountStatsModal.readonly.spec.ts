import { describe, expect, it, vi, beforeEach } from "vitest";
import { flushPromises, mount } from "@vue/test-utils";
import AccountStatsModal from "../AccountStatsModal.vue";
import type { AccountUsageStatsResponse } from "@/types";
import type { ReadonlyStatsAccount } from "../accountCellTypes";

const { getStats } = vi.hoisted(() => ({ getStats: vi.fn() }));

vi.mock("@/api/admin", () => ({
  adminAPI: {
    accounts: { getStats },
  },
}));

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  };
});

function makeStatsWithCost(cost: number): AccountUsageStatsResponse {
  const stats = makeStats();
  stats.summary.total_cost = cost;
  return stats;
}

function makeStats(): AccountUsageStatsResponse {
  return {
    history: [],
    summary: {
      days: 30,
      actual_days_used: 1,
      total_cost: 1,
      total_user_cost: 1,
      total_standard_cost: 1,
      total_requests: 2,
      total_tokens: 3,
      avg_daily_cost: 1,
      avg_daily_user_cost: 1,
      avg_daily_requests: 2,
      avg_daily_tokens: 3,
      avg_duration_ms: 100,
      today: {
        date: "2026-10-03",
        cost: 1,
        user_cost: 1,
        requests: 2,
        tokens: 3,
      },
      highest_cost_day: {
        date: "2026-10-03",
        label: "10-03",
        cost: 1,
        user_cost: 1,
        requests: 2,
      },
      highest_request_day: {
        date: "2026-10-03",
        label: "10-03",
        requests: 2,
        cost: 1,
        user_cost: 1,
      },
    },
    models: [],
    endpoints: [],
    upstream_endpoints: [
      { key: "https://secret-upstream.example", requests: 1, cost: 1 },
    ],
  } as unknown as AccountUsageStatsResponse;
}

const account: ReadonlyStatsAccount = {
  id: 12,
  name: "safe-account",
  status: "active",
};

const globalStubs = {
  BaseDialog: {
    props: ["show", "title", "width"],
    template: '<div v-if="show"><slot /><slot name="footer" /></div>',
  },
  LoadingSpinner: true,
  Line: true,
  ModelDistributionChart: true,
  EndpointDistributionChart: {
    props: ["title"],
    template: '<div class="endpoint-chart">{{ title }}</div>',
  },
  Icon: true,
};

describe("AccountStatsModal read-only", () => {
  beforeEach(() => {
    getStats.mockReset();
  });

  it("uses the injected statsFetcher and never the admin API", async () => {
    const statsFetcher = vi.fn().mockResolvedValue(makeStats());
    const wrapper = mount(AccountStatsModal, {
      props: { show: false, account, readOnly: true, statsFetcher },
      global: { stubs: globalStubs },
    });
    await wrapper.setProps({ show: true });
    await flushPromises();

    expect(statsFetcher).toHaveBeenCalledTimes(1);
    expect(getStats).not.toHaveBeenCalled();
    expect(wrapper.text()).toContain("$1.00");
  });

  it("hides the raw upstream endpoints chart in read-only mode", async () => {
    const statsFetcher = vi.fn().mockResolvedValue(makeStats());
    const wrapper = mount(AccountStatsModal, {
      props: { show: false, account, readOnly: true, statsFetcher },
      global: { stubs: globalStubs },
    });
    await wrapper.setProps({ show: true });
    await flushPromises();

    expect(wrapper.text()).not.toContain("usage.upstreamEndpoint");
    expect(wrapper.text()).toContain("usage.inboundEndpoint");
  });

  it("fails closed without a statsFetcher: no admin API, no data", async () => {
    const wrapper = mount(AccountStatsModal, {
      props: { show: false, account, readOnly: true },
      global: { stubs: globalStubs },
    });
    await wrapper.setProps({ show: true });
    await flushPromises();

    expect(getStats).not.toHaveBeenCalled();
    expect(wrapper.text()).toContain("admin.accounts.stats.noData");
  });

  it("keeps the admin path using the admin API", async () => {
    getStats.mockResolvedValue(makeStats());
    const wrapper = mount(AccountStatsModal, {
      props: {
        show: false,
        account: { id: 13, name: "a", status: "active" },
      },
      global: { stubs: globalStubs },
    });
    await wrapper.setProps({ show: true });
    await flushPromises();

    expect(getStats).toHaveBeenCalled();
  });

  it("ignores a late response from an account closed before reopen", async () => {
    let resolveA!: (value: AccountUsageStatsResponse) => void;
    const pendingA = new Promise<AccountUsageStatsResponse>((resolve) => {
      resolveA = resolve;
    });
    const statsFetcher = vi.fn((acct: ReadonlyStatsAccount) =>
      acct.id === 1 ? pendingA : Promise.resolve(makeStatsWithCost(42)),
    );
    const wrapper = mount(AccountStatsModal, {
      props: {
        show: false,
        account: { id: 1, name: "A", status: "active" },
        readOnly: true,
        statsFetcher,
      },
      global: { stubs: globalStubs },
    });

    await wrapper.setProps({ show: true });
    await wrapper.setProps({ show: false });
    await wrapper.setProps({
      show: true,
      account: { id: 2, name: "B", status: "active" },
    });
    await flushPromises();
    expect(wrapper.text()).toContain("$42.00");

    resolveA(makeStatsWithCost(7));
    await flushPromises();

    expect(wrapper.text()).toContain("$42.00");
    expect(wrapper.text()).not.toContain("$7.00");
  });

  it("ignores a late response after switching account while open", async () => {
    let resolveA!: (value: AccountUsageStatsResponse) => void;
    const pendingA = new Promise<AccountUsageStatsResponse>((resolve) => {
      resolveA = resolve;
    });
    const statsFetcher = vi.fn((acct: ReadonlyStatsAccount) =>
      acct.id === 1 ? pendingA : Promise.resolve(makeStatsWithCost(42)),
    );
    const wrapper = mount(AccountStatsModal, {
      props: {
        show: false,
        account: { id: 1, name: "A", status: "active" },
        readOnly: true,
        statsFetcher,
      },
      global: { stubs: globalStubs },
    });

    await wrapper.setProps({ show: true });
    await wrapper.setProps({
      account: { id: 2, name: "B", status: "active" },
    });
    await flushPromises();
    expect(wrapper.text()).toContain("$42.00");

    resolveA(makeStatsWithCost(7));
    await flushPromises();

    expect(wrapper.text()).toContain("$42.00");
    expect(wrapper.text()).not.toContain("$7.00");
  });
});
