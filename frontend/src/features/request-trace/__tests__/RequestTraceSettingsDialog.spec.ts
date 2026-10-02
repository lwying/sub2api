import { mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";
import RequestTraceSettingsDialog from "../RequestTraceSettingsDialog.vue";

vi.mock("vue-i18n", async (original) => {
  const actual = await original<typeof import("vue-i18n")>();
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) };
});

describe("Trace export settings dialog", () => {
  it("shows only the independent export capability, with no capture controls", async () => {
    const wrapper = mount(RequestTraceSettingsDialog, {
      props: { show: false },
      global: {
        stubs: {
          BaseDialog: {
            props: ["show"],
            template: "<div v-if='show'><slot /></div>",
          },
          RequestTraceExportSettings: {
            template: "<section data-testid='export-settings'>export</section>",
          },
        },
      },
    });
    expect(wrapper.find('[data-testid="export-settings"]').exists()).toBe(
      false,
    );
    await wrapper.setProps({ show: true });
    expect(wrapper.get('[data-testid="export-settings"]').exists()).toBe(true);
    // Capture configuration lives in its own page tab; the dialog must not
    // render it, so a capture change can never ride along with an export one.
    expect(
      wrapper.find('[data-testid="request-trace-operator"]').exists(),
    ).toBe(false);
  });
});
