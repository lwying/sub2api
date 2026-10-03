import { describe, expect, it, vi } from "vitest";
import { mount } from "@vue/test-utils";
import AccountStatusIndicator from "../AccountStatusIndicator.vue";
import type { ReadonlyStatusAccount } from "../accountCellTypes";

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  };
});

vi.mock("@/utils/format", async () => {
  const actual =
    await vi.importActual<typeof import("@/utils/format")>("@/utils/format");
  return { ...actual, formatCountdown: () => "1h" };
});

function makeSafe(
  overrides: Partial<ReadonlyStatusAccount> = {},
): ReadonlyStatusAccount {
  return {
    platform: "anthropic",
    type: "oauth",
    status: "active",
    schedulable: true,
    ...overrides,
  };
}

describe("AccountStatusIndicator read-only", () => {
  it("does not emit show-temp-unsched and renders no action button", async () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeSafe({
          temp_unschedulable_until: "2099-07-28T00:00:00Z",
        }),
        readOnly: true,
      },
    });
    await wrapper.vm.$nextTick();

    expect(wrapper.find("button").exists()).toBe(false);
    expect(wrapper.emitted("show-temp-unsched")).toBeUndefined();
  });

  it("never renders the raw error tooltip in read-only mode", async () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeSafe({ status: "error" }),
        readOnly: true,
      },
    });
    await wrapper.vm.$nextTick();

    expect(wrapper.find(".group\\/error").exists()).toBe(false);
    expect(wrapper.text()).not.toContain("boom");
  });

  it("keeps admin behavior: error account with message renders the indicator", async () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: {
          ...makeSafe({ status: "error" }),
          error_message: "boom",
        },
      },
    });
    await wrapper.vm.$nextTick();

    expect(wrapper.find(".group\\/error").exists()).toBe(true);
  });
});
