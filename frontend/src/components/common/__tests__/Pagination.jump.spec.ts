import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { enableAutoUnmount, mount } from "@vue/test-utils";
import Pagination from "../Pagination.vue";
import Select from "../Select.vue";

// 用注入的全局表格配置替换默认配置，验证组件确实读取配置作为兜底。
const preferences = vi.hoisted(() => ({
  getConfiguredTablePageSizeOptions: vi.fn(() => [10, 20, 50, 100]),
}));

vi.mock("@/utils/tablePreferences", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/utils/tablePreferences")>();
  return {
    ...actual,
    getConfiguredTablePageSizeOptions:
      preferences.getConfiguredTablePageSizeOptions,
  };
});

vi.mock("vue-i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
enableAutoUnmount(afterEach);

function mountPagination() {
  return mount(Pagination, {
    props: {
      total: 200,
      page: 1,
      pageSize: 20,
      showJump: true,
      showPageSizeSelector: false,
    },
    global: { stubs: { Icon: true } },
  });
}

describe("pagination jump input", () => {
  it.each(["click", "enter"])(
    "jumps to the entered numeric page using %s",
    async (action) => {
      const wrapper = mountPagination();
      const input = wrapper.get('input[type="number"]');
      await input.setValue("3");
      if (action === "click") await wrapper.get(".btn").trigger("click");
      else await input.trigger("keyup", { key: "Enter" });
      expect(wrapper.emitted("update:page")).toEqual([[3]]);
      expect((input.element as HTMLInputElement).value).toBe("");
    },
  );

  it("clamps an entered page to the last page", async () => {
    const wrapper = mountPagination();
    await wrapper.get("input").setValue("99");
    await wrapper.get(".btn").trigger("click");
    expect(wrapper.emitted("update:page")).toEqual([[10]]);
  });

  it("ignores an empty jump input", async () => {
    const wrapper = mountPagination();
    await wrapper.get(".btn").trigger("click");
    expect(wrapper.emitted("update:page")).toBeUndefined();
  });
});

describe("pagination page-size options", () => {
  beforeEach(() => {
    // 与真实部署一样，全局配置与调用方传入的受限选项不同。
    preferences.getConfiguredTablePageSizeOptions.mockReturnValue([10, 25, 50]);
  });

  it("uses the pageSizeOptions prop instead of the global table configuration", () => {
    const wrapper = mount(Pagination, {
      props: {
        total: 200,
        page: 1,
        pageSize: 20,
        pageSizeOptions: [20, 100],
      },
      global: { stubs: { Icon: true } },
    });

    const options = wrapper
      .getComponent(Select)
      .props("options")
      .map((option: { value: number }) => option.value);
    // 调用方明确给出了可选项时，不得再混入全局配置里的 10/25/50。
    expect(options).toEqual([20, 100]);
  });

  it("emits and persists the selected restricted option without global remapping", async () => {
    window.__APP_CONFIG__ = { table_page_size_options: [200] } as NonNullable<
      typeof window.__APP_CONFIG__
    >;
    const wrapper = mount(Pagination, {
      props: { total: 200, page: 1, pageSize: 20, pageSizeOptions: [20, 100] },
      global: { stubs: { Icon: true } },
    });
    try {
      wrapper.getComponent(Select).vm.$emit("update:modelValue", 100);
      await wrapper.vm.$nextTick();
      expect(wrapper.emitted("update:pageSize")).toEqual([[100]]);
      expect(window.localStorage.getItem("table-page-size")).toBe("100");
    } finally {
      delete window.__APP_CONFIG__;
      window.localStorage.removeItem("table-page-size");
    }
  });

  it("falls back to the configured options when the prop is omitted", () => {
    const wrapper = mount(Pagination, {
      props: {
        total: 200,
        page: 1,
        pageSize: 25,
        pageSizeOptions: undefined,
      },
      global: { stubs: { Icon: true } },
    });

    const options = wrapper
      .getComponent(Select)
      .props("options")
      .map((option: { value: number }) => option.value);
    expect(options).toEqual([10, 25, 50]);
  });
});
