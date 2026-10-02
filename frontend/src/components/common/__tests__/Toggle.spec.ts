import { afterEach, describe, expect, it } from "vitest";
import { enableAutoUnmount, mount } from "@vue/test-utils";
import Toggle from "../Toggle.vue";

enableAutoUnmount(afterEach);

describe("shared toggle", () => {
  it("exposes the checked state and emits a change on click", async () => {
    const wrapper = mount(Toggle, {
      props: { modelValue: false },
      attrs: { "aria-label": "启用测试回复" },
    });
    expect(wrapper.attributes("role")).toBe("switch");
    expect(wrapper.attributes("aria-checked")).toBe("false");
    expect(wrapper.attributes("aria-label")).toBe("启用测试回复");
    await wrapper.trigger("click");
    expect(wrapper.emitted("update:modelValue")).toEqual([[true]]);
  });

  it("does not emit changes while disabled", async () => {
    const wrapper = mount(Toggle, {
      props: { modelValue: true, disabled: true },
    });
    expect((wrapper.element as HTMLButtonElement).disabled).toBe(true);
    expect(wrapper.attributes("aria-checked")).toBe("true");
    await wrapper.trigger("click");
    expect(wrapper.emitted("update:modelValue")).toBeUndefined();
  });
});
