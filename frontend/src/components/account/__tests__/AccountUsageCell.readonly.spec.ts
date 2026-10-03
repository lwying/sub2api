import { describe, expect, it, vi, beforeEach } from "vitest";
import { flushPromises, mount } from "@vue/test-utils";
import AccountUsageCell from "../AccountUsageCell.vue";
import type { AccountUsageInfo } from "@/types";
import type { ReadonlyUsageAccount } from "../accountCellTypes";

const { getUsage } = vi.hoisted(() => ({ getUsage: vi.fn() }));

vi.mock("@/api/admin", () => ({
  adminAPI: {
    accounts: {
      getUsage,
      getStats: vi.fn(),
    },
  },
}));

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  };
});

function makeSafeAccount(
  overrides: Partial<ReadonlyUsageAccount> = {},
): ReadonlyUsageAccount {
  return {
    id: 916,
    platform: "anthropic",
    type: "oauth",
    ...overrides,
  };
}

function makeUsage(partial: Partial<AccountUsageInfo> = {}): AccountUsageInfo {
  return {
    updated_at: "2026-10-03T00:00:00Z",
    five_hour: {
      utilization: 41,
      resets_at: "2026-10-03T05:00:00Z",
      remaining_seconds: 3600,
    },
    seven_day: {
      utilization: 56,
      resets_at: "2026-10-07T00:00:00Z",
      remaining_seconds: 300000,
    },
    seven_day_sonnet: null,
    ...partial,
  } as AccountUsageInfo;
}

const UsageProgressBarStub = {
  name: "UsageProgressBar",
  props: ["label", "utilization", "resetsAt", "color"],
  template: '<div class="usage-bar">{{ label }}|{{ utilization }}</div>',
};

describe("AccountUsageCell read-only", () => {
  beforeEach(() => {
    getUsage.mockReset();
  });

  it("consumes injected batchedUsage and never calls the admin API", async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeSafeAccount(),
        readOnly: true,
        batchedUsage: makeUsage(),
      },
      global: { stubs: { UsageProgressBar: UsageProgressBarStub } },
    });
    await flushPromises();

    expect(wrapper.text()).toContain("5h|41");
    expect(wrapper.text()).toContain("7d|56");
    expect(getUsage).not.toHaveBeenCalled();
  });

  it("uses the injected passive usageFetcher only", async () => {
    const usageFetcher = vi.fn().mockResolvedValue(makeUsage());
    const account = makeSafeAccount({ id: 917 });
    const wrapper = mount(AccountUsageCell, {
      props: { account, readOnly: true, usageFetcher },
      global: { stubs: { UsageProgressBar: UsageProgressBarStub } },
    });
    await flushPromises();

    expect(usageFetcher).toHaveBeenCalledTimes(1);
    expect(usageFetcher.mock.calls[0][0]).toMatchObject({ id: 917 });
    expect(wrapper.text()).toContain("5h|41");
    expect(getUsage).not.toHaveBeenCalled();
  });

  it("shows the no-data placeholder and no action DOM when nothing is injected", async () => {
    const wrapper = mount(AccountUsageCell, {
      props: { account: makeSafeAccount(), readOnly: true },
      global: { stubs: { UsageProgressBar: UsageProgressBarStub } },
    });
    await flushPromises();

    expect(wrapper.text()).toContain("admin.accounts.stats.noData");
    expect(wrapper.find("button").exists()).toBe(false);
    expect(getUsage).not.toHaveBeenCalled();
  });

  it("renders safe API-key quota bars from the narrow projection", async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeSafeAccount({
          platform: "anthropic",
          type: "apikey",
          quota_daily_limit: 100,
          quota_daily_used: 25,
        }),
        readOnly: true,
      },
      global: { stubs: { UsageProgressBar: UsageProgressBarStub } },
    });
    await flushPromises();

    expect(wrapper.text()).toContain("1d|25");
    expect(getUsage).not.toHaveBeenCalled();
  });

  it("keeps the admin path calling the admin API when not read-only", async () => {
    getUsage.mockResolvedValue(makeUsage());
    mount(AccountUsageCell, {
      props: {
        account: {
          id: 918,
          name: "a",
          platform: "anthropic",
          type: "oauth",
          proxy_id: null,
          concurrency: 1,
          priority: 1,
          status: "active",
          error_message: null,
          last_used_at: null,
          expires_at: null,
          auto_pause_on_expired: true,
          created_at: "2026-03-15T00:00:00Z",
          updated_at: "2026-03-15T00:00:00Z",
          schedulable: true,
          rate_limited_at: null,
          rate_limit_reset_at: null,
          overload_until: null,
          temp_unschedulable_until: null,
          temp_unschedulable_reason: null,
          session_window_start: null,
          session_window_end: null,
          session_window_status: null,
          extra: {},
        },
      },
      global: { stubs: { UsageProgressBar: UsageProgressBarStub } },
    });
    await flushPromises();

    expect(getUsage).toHaveBeenCalled();
  });

  it("renders a sonnet-only window instead of no-data", async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeSafeAccount(),
        readOnly: true,
        batchedUsage: makeUsage({
          five_hour: null,
          seven_day: null,
          seven_day_sonnet: {
            utilization: 33,
            resets_at: "2026-10-06T00:00:00Z",
            remaining_seconds: 100,
          },
        }),
      },
      global: { stubs: { UsageProgressBar: UsageProgressBarStub } },
    });
    await flushPromises();

    expect(wrapper.text()).toContain("7d S|33");
    expect(wrapper.text()).not.toContain("admin.accounts.stats.noData");
    expect(getUsage).not.toHaveBeenCalled();
  });

  it("renders a fable-only window instead of no-data", async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeSafeAccount(),
        readOnly: true,
        batchedUsage: makeUsage({
          five_hour: null,
          seven_day: null,
          seven_day_fable: {
            utilization: 44,
            resets_at: "2026-10-06T00:00:00Z",
            remaining_seconds: 100,
          },
        }),
      },
      global: { stubs: { UsageProgressBar: UsageProgressBarStub } },
    });
    await flushPromises();

    expect(wrapper.text()).toContain("7d F|44");
    expect(wrapper.text()).not.toContain("admin.accounts.stats.noData");
    expect(getUsage).not.toHaveBeenCalled();
  });
});
