import { mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

vi.mock("vue-i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
import AccountTableFilters from "../AccountTableFilters.vue";

const controls = {
  SearchInput: {
    props: ["modelValue"],
    template: '<input data-testid="search" :value="modelValue" />',
  },
  Select: {
    props: ["modelValue", "options"],
    template:
      '<select><option v-for="option in options" :key="option.value">{{ option.label }}</option></select>',
  },
};

describe("共享账号筛选", () => {
  it("allows read-only consumers to omit administrator-only privacy filtering", () => {
    const wrapper = mount(AccountTableFilters, {
      props: {
        searchQuery: "",
        filters: {},
        visibleFields: ["platform", "type", "status", "group"],
      },
      global: { stubs: controls },
    });
    expect(wrapper.findAll("select")).toHaveLength(4);
    expect(wrapper.text()).not.toContain("admin.accounts.allPrivacyModes");
  });

  it("keeps the administrator default filter set", () => {
    const wrapper = mount(AccountTableFilters, {
      props: { searchQuery: "", filters: {} },
      global: { stubs: controls },
    });
    expect(wrapper.findAll("select")).toHaveLength(5);
  });
});
