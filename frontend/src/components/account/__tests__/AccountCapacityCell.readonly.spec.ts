import { describe, expect, it, vi } from "vitest";
import { mount } from "@vue/test-utils";
import AccountCapacityCell from "../AccountCapacityCell.vue";
import type { ReadonlyCapacityAccount } from "../accountCellTypes";

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  };
});

function makeSafe(
  overrides: Partial<ReadonlyCapacityAccount> = {},
): ReadonlyCapacityAccount {
  return {
    platform: "anthropic",
    type: "oauth",
    concurrency: 5,
    ...overrides,
  };
}

describe("AccountCapacityCell read-only", () => {
  it("marks an unknown current concurrency explicitly instead of faking 0", () => {
    const wrapper = mount(AccountCapacityCell, {
      props: {
        account: makeSafe({ current_concurrency: null }),
        readOnly: true,
      },
    });

    expect(wrapper.text()).toContain("-");
    expect(wrapper.text()).toContain("5");
  });

  it("keeps admin behavior when current concurrency is present", () => {
    const wrapper = mount(AccountCapacityCell, {
      props: { account: makeSafe({ current_concurrency: 2 }) },
    });

    expect(wrapper.text()).toContain("2");
  });

  it("shows unknown window cost / sessions / rpm as neutral '-' rather than 0", () => {
    const wrapper = mount(AccountCapacityCell, {
      props: {
        account: makeSafe({
          // 三个限额已配置，但只读投影不含实时读数（current_* 缺失）
          window_cost_limit: 100,
          max_sessions: 10,
          base_rpm: 60,
        }),
        readOnly: true,
      },
    });

    const text = wrapper.text();
    const html = wrapper.html();

    // 缺失的实时读数显示 -，绝不用 $0 / 0 冒充已知
    expect(text).not.toContain("$0.00");
    expect(text).toContain("-/$100.00");
    expect(text).toContain("-/10");
    expect(text).toContain("-/60");
    // 三处都用中性灰，而不是任何利用率配色
    expect(html).not.toContain("bg-red-100");
    expect(html).not.toContain("bg-orange-100");
    expect(html).not.toContain("bg-yellow-100");
    expect(html).not.toContain("bg-emerald-100");
    expect((html.match(/bg-gray-100/g) || []).length).toBeGreaterThanOrEqual(3);
  });
});
