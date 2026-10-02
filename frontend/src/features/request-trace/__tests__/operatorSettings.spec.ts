import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { nextTick } from "vue";

const api = vi.hoisted(() => ({ updateOperatorSettings: vi.fn() }));
vi.mock("../api", () => ({
  updateOperatorSettings: api.updateOperatorSettings,
}));
vi.mock("vue-i18n", async (importOriginal) => {
  const actual = await importOriginal<typeof import("vue-i18n")>();
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => {
        const values = params?.values;
        if (key.endsWith("operator.scope.onlyValues")) return `Only: ${values}`;
        if (key.endsWith("operator.scope.exceptValues"))
          return `Except: ${values}`;
        if (key.endsWith("operator.scope.emptyValues"))
          return "No entry listed";
        if (key.endsWith("operator.scope.allValues")) return "All";
        return key;
      },
      locale: { value: "en" },
    }),
  };
});

import RequestTraceOperatorSettings from "../RequestTraceOperatorSettings.vue";
import type { RequestTraceOperatorStatus } from "../types";

const phrase =
  "New Trace risk: raw fragments may contain credentials, plaintext exports survive usage deletion.";

const status = (
  overrides: Partial<RequestTraceOperatorStatus> = {},
): RequestTraceOperatorStatus => ({
  enabled: false,
  capture_allowed: false,
  risk_acknowledged: false,
  risk_acknowledgement_current: false,
  risk_version: "v2026.09.28",
  risk_phrase_en: phrase,
  risk_phrase_zh: "新版请求跟踪明文风险确认。",
  plaintext_capture_supported: true,
  plaintext_capture_support_reason: "supported",
  all_groups: true,
  group_ids: [],
  model_scope: "all",
  models: [],
  platform_scope: "all",
  platforms: [],
  capture_body: true,
  capture_http_200: true,
  sample_rate_http_200: 100,
  sample_rate_other: 100,
  body_max_bytes: 1048576,
  capture_duration_seconds: 0,
  capture_until: null,
  capture_expired: false,
  ...overrides,
});

/** Lightweight stand-ins so the spec exercises the operator form itself. */
const ToggleStub = {
  name: "Toggle",
  props: ["modelValue", "disabled"],
  emits: ["update:modelValue"],
  template: `<button v-bind="$attrs" @click="$emit('update:modelValue', !modelValue)"></button>`,
};
const SelectStub = {
  name: "Select",
  props: ["modelValue", "options", "id"],
  emits: ["update:modelValue"],
  template: `<select :id="id" v-bind="$attrs" @change="$emit('update:modelValue', $event.target.value)"><option v-for="option in options" :key="option.value" :value="option.value">{{ option.label }}</option></select>`,
};
const GroupSelectorStub = {
  name: "GroupSelector",
  props: ["modelValue", "groups", "preserveAllGroups", "searchable"],
  template: `<div data-testid="group-selector-stub"></div>`,
};
const ModelSelectorStub = {
  name: "ModelWhitelistSelector",
  props: ["modelValue", "extraOptions", "allowCustom", "platforms"],
  template: `<div data-testid="model-selector-stub"></div>`,
};

function mountSettings(
  current: RequestTraceOperatorStatus | null = status(),
  extra: Record<string, unknown> = {},
) {
  return mount(RequestTraceOperatorSettings, {
    props: { status: current, loading: false, ...extra },
    global: {
      stubs: {
        Toggle: ToggleStub,
        Select: SelectStub,
        GroupSelector: GroupSelectorStub,
        ModelWhitelistSelector: ModelSelectorStub,
      },
    },
  });
}

async function openAdvanced(wrapper: ReturnType<typeof mountSettings>) {
  await wrapper
    .get('[data-testid="request-trace-advanced-toggle"]')
    .trigger("click");
}

beforeEach(() => {
  api.updateOperatorSettings.mockReset();
  sessionStorage.clear();
  localStorage.clear();
});

describe("Trace capture status", () => {
  it("offers retry after the capture status could not be loaded", async () => {
    const wrapper = mountSettings(null);
    await wrapper
      .get('[data-testid="request-trace-status-retry"]')
      .trigger("click");
    expect(wrapper.emitted("retry-status")).toHaveLength(1);
    expect(api.updateOperatorSettings).not.toHaveBeenCalled();
  });

  it("shows the stored switch, effective verdict and deployment separately", () => {
    const wrapper = mountSettings(
      status({
        enabled: true,
        capture_allowed: false,
        risk_acknowledgement_current: false,
        plaintext_capture_supported: false,
        plaintext_capture_support_reason: "probe_failed",
      }),
    );
    expect(
      wrapper
        .get('[data-testid="request-trace-stored"]')
        .attributes("data-state"),
    ).toBe("on");
    expect(
      wrapper
        .get('[data-testid="request-trace-capture-state"]')
        .attributes("data-state"),
    ).toBe("off");
    expect(
      wrapper.find('[data-testid="request-trace-deployment-blocked"]').exists(),
    ).toBe(true);
    expect(
      wrapper.get('[data-testid="request-trace-deployment"]').text(),
    ).toContain("probe_failed");
    expect(api.updateOperatorSettings).not.toHaveBeenCalled();
  });

  it("marks an expired window as stopped rather than capturing", () => {
    const wrapper = mountSettings(
      status({
        enabled: true,
        capture_allowed: false,
        capture_until: new Date(Date.now() - 60_000).toISOString(),
        capture_expired: true,
      }),
    );
    expect(
      wrapper
        .get('[data-testid="request-trace-remaining"]')
        .attributes("data-expired"),
    ).toBe("true");
    expect(wrapper.get('[data-testid="request-trace-remaining"]').text()).toBe(
      "admin.requestTrace.operator.status.expired",
    );
    expect(wrapper.find('[data-testid="request-trace-expired"]').exists()).toBe(
      true,
    );
  });

  it("renders the applied scope from the server, never from the draft", () => {
    const wrapper = mountSettings(
      status({
        all_groups: false,
        group_ids: [4, 7],
        model_scope: "include",
        models: ["claude-sonnet-4-5"],
        platform_scope: "exclude",
        platforms: ["antigravity"],
      }),
    );
    // Missing catalog entries fall back to #ID; names are covered below.
    expect(
      wrapper.get('[data-testid="request-trace-scope-stored-groups"]').text(),
    ).toBe("Only: #4, #7");
    expect(
      wrapper
        .get('[data-testid="request-trace-scope-stored-platforms"]')
        .text(),
    ).toBe("Except: antigravity");
  });

  it("shows known group names in the stored scope and #ID only when missing", () => {
    const wrapper = mountSettings(
      status({ all_groups: false, group_ids: [4, 9] }),
      {
        groups: [
          {
            id: 4,
            name: "Production",
            platform: "anthropic",
            status: "active",
          },
        ] as never,
      },
    );
    const text = wrapper
      .get('[data-testid="request-trace-scope-stored-groups"]')
      .text();
    expect(text).toContain("Production");
    expect(text).not.toContain("#4");
    expect(text).toContain("#9");
  });

  it("never renders unexpected credential or body fields supplied with status", () => {
    const wrapper = mountSettings({
      ...status(),
      body: "BODY_CANARY",
      authorization: "Bearer secret",
    } as RequestTraceOperatorStatus);
    expect(wrapper.text()).not.toContain("BODY_CANARY");
    expect(wrapper.text()).not.toContain("Bearer secret");
  });
});

describe("Trace capture deadline", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  function mountWithDeadline(untilMsFromNow: number) {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-10-03T12:00:00.000Z"));
    return mountSettings(
      status({
        enabled: true,
        capture_allowed: true,
        capture_expired: false,
        capture_until: new Date(Date.now() + untilMsFromNow).toISOString(),
      }),
    );
  }

  it("ticks the remaining label down without a fresh server response", async () => {
    const wrapper = mountWithDeadline(2 * 60_000);
    expect(wrapper.get('[data-testid="request-trace-remaining"]').text()).toBe(
      "2m",
    );
    await vi.advanceTimersByTimeAsync(60_000);
    await nextTick();
    expect(wrapper.get('[data-testid="request-trace-remaining"]').text()).toBe(
      "1m",
    );
    wrapper.unmount();
  });

  it("switches the effective verdict to stopped at the deadline and asks for a fresh status once", async () => {
    const wrapper = mountWithDeadline(30_000);
    // Before the deadline the server verdict stands.
    expect(
      wrapper
        .get('[data-testid="request-trace-capture-state"]')
        .attributes("data-state"),
    ).toBe("on");
    expect(
      wrapper
        .get('[data-testid="request-trace-remaining"]')
        .attributes("data-expired"),
    ).toBe("false");
    expect(wrapper.find('[data-testid="request-trace-expired"]').exists()).toBe(
      false,
    );

    await vi.advanceTimersByTimeAsync(31_000);
    await nextTick();

    // The local clock wins: capture_allowed is no longer shown as capturing.
    expect(
      wrapper
        .get('[data-testid="request-trace-capture-state"]')
        .attributes("data-state"),
    ).toBe("off");
    expect(
      wrapper.get('[data-testid="request-trace-capture-state"]').text(),
    ).toBe("admin.requestTrace.operator.status.expired");
    expect(
      wrapper
        .get('[data-testid="request-trace-remaining"]')
        .attributes("data-expired"),
    ).toBe("true");
    expect(wrapper.get('[data-testid="request-trace-remaining"]').text()).toBe(
      "admin.requestTrace.operator.status.expired",
    );
    const note = wrapper.get('[data-testid="request-trace-expired"]');
    expect(note.attributes("data-source")).toBe("derived");
    expect(note.text()).toBe(
      "admin.requestTrace.operator.status.expiredPendingNote",
    );
    // Exactly one refresh request for this deadline, even as time keeps moving.
    expect(wrapper.emitted("retry-status")).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(60_000);
    await nextTick();
    expect(wrapper.emitted("retry-status")).toHaveLength(1);
    wrapper.unmount();
  });

  it("does not claim a derived expiry as a server-confirmed one", () => {
    const wrapper = mountSettings(
      status({
        enabled: true,
        capture_allowed: false,
        capture_until: new Date(Date.now() - 60_000).toISOString(),
        capture_expired: true,
      }),
    );
    const note = wrapper.get('[data-testid="request-trace-expired"]');
    expect(note.attributes("data-source")).toBe("server");
    expect(note.text()).toBe("admin.requestTrace.operator.status.expiredNote");
  });

  it("clears its deadline ticker when unmounted", () => {
    const wrapper = mountWithDeadline(60_000);
    expect(vi.getTimerCount()).toBeGreaterThan(0);
    wrapper.unmount();
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe("Trace capture presets", () => {
  it("changes only the two content switches and never saves on its own", async () => {
    const wrapper = mountSettings();
    await wrapper
      .get('[data-testid="request-trace-preset-lite"]')
      .trigger("click");
    expect(
      wrapper
        .get('[data-testid="request-trace-summary-body"]')
        .attributes("data-diff"),
    ).toBe("diff");
    expect(
      wrapper
        .get('[data-testid="request-trace-summary-http200"]')
        .attributes("data-diff"),
    ).toBe("diff");
    // Scope, sampling, size and duration are untouched by the preset.
    for (const key of [
      "groups",
      "models",
      "platforms",
      "sample-http200",
      "body-limit",
      "duration",
    ]) {
      expect(
        wrapper
          .get(`[data-testid="request-trace-summary-${key}"]`)
          .attributes("data-diff"),
      ).toBe("same");
    }
    expect(api.updateOperatorSettings).not.toHaveBeenCalled();
  });

  it("labels an unmatched combination as custom", async () => {
    const wrapper = mountSettings();
    await wrapper
      .get('[data-testid="request-trace-http200-toggle"]')
      .trigger("click");
    expect(
      wrapper.get('[data-testid="request-trace-preset-current"]').text(),
    ).toBe("admin.requestTrace.operator.presets.custom");
  });

  it("saves the preset through the full payload without re-timing the window", async () => {
    api.updateOperatorSettings.mockResolvedValue(status());
    const wrapper = mountSettings();
    await wrapper
      .get('[data-testid="request-trace-preset-lite"]')
      .trigger("click");
    await wrapper.get('[data-testid="request-trace-save"]').trigger("click");
    await flushPromises();
    const [payload] = api.updateOperatorSettings.mock.calls[0];
    expect(payload).toMatchObject({
      enabled: false,
      capture_body: false,
      capture_http_200: false,
      sample_rate_http_200: 100,
      body_max_bytes: 1048576,
      capture_duration_seconds: 0,
      scope_provided: true,
      all_groups: true,
    });
    expect(payload).not.toHaveProperty("renew_capture_window");
  });
});

describe("Trace capture content switches", () => {
  it("explains metadata-only capture and keeps the body limit value while off", async () => {
    api.updateOperatorSettings.mockResolvedValue(status());
    const wrapper = mountSettings();
    await wrapper
      .get('[data-testid="request-trace-body-toggle"]')
      .trigger("click");
    expect(
      wrapper.find('[data-testid="request-trace-body-metadata-only"]').exists(),
    ).toBe(true);
    await openAdvanced(wrapper);
    const limit = wrapper.get('[data-testid="request-trace-body-limit"]');
    expect((limit.element as HTMLSelectElement).disabled).toBe(true);
    expect(
      wrapper
        .find('[data-testid="request-trace-body-limit-disabled"]')
        .exists(),
    ).toBe(true);
    await wrapper.get('[data-testid="request-trace-save"]').trigger("click");
    await flushPromises();
    const [payload] = api.updateOperatorSettings.mock.calls[0];
    expect(payload).toMatchObject({
      capture_body: false,
      body_max_bytes: 1048576,
    });
  });

  it("disables the HTTP 200 sample rate while off but keeps its value", async () => {
    api.updateOperatorSettings.mockResolvedValue(status());
    const wrapper = mountSettings();
    await wrapper
      .get('[data-testid="request-trace-http200-toggle"]')
      .trigger("click");
    await openAdvanced(wrapper);
    const sample = wrapper.get('[data-testid="request-trace-sample-http200"]');
    expect((sample.element as HTMLInputElement).disabled).toBe(true);
    expect((sample.element as HTMLInputElement).value).toBe("100");
    expect(
      wrapper
        .find('[data-testid="request-trace-sample-http200-disabled"]')
        .exists(),
    ).toBe(true);
    await wrapper.get('[data-testid="request-trace-save"]').trigger("click");
    await flushPromises();
    const [payload] = api.updateOperatorSettings.mock.calls[0];
    expect(payload).toMatchObject({
      capture_http_200: false,
      sample_rate_http_200: 100,
    });
  });
});

describe("Trace capture scope", () => {
  it("keeps saved entries that are missing from the current catalogs", () => {
    const wrapper = mountSettings(
      status({
        all_groups: false,
        group_ids: [99],
        model_scope: "include",
        models: ["ghost-model"],
        platform_scope: "include",
        platforms: ["ghost-platform"],
      }),
      { groups: [], modelCandidates: ["real-model"] },
    );
    expect(
      wrapper.find('[data-testid="request-trace-scope-stale"]').exists(),
    ).toBe(true);
    const groupProps = wrapper.findComponent({ name: "GroupSelector" }).props();
    expect(
      (groupProps.groups as { id: number }[]).some((g) => g.id === 99),
    ).toBe(true);
    const modelProps = wrapper
      .findComponent({ name: "ModelWhitelistSelector" })
      .props();
    expect(modelProps.allowCustom).toBe(false);
    expect(modelProps.extraOptions as string[]).toContain("ghost-model");
    expect(
      wrapper
        .find('[data-testid="request-trace-scope-platform-ghost-platform"]')
        .exists(),
    ).toBe(true);
  });

  it("shows a retry for a failed option load and re-emits", async () => {
    const wrapper = mountSettings(status(), {
      optionsError: "candidates unavailable",
    });
    expect(
      wrapper.find('[data-testid="request-trace-options-error"]').exists(),
    ).toBe(true);
    await wrapper
      .get('[data-testid="request-trace-options-retry"]')
      .trigger("click");
    expect(wrapper.emitted("retry-options")).toEqual([[]]);
  });

  it("refuses an empty include scope instead of storing one that matches nothing", async () => {
    api.updateOperatorSettings.mockResolvedValue(status());
    const wrapper = mountSettings();
    await wrapper
      .get('[data-testid="request-trace-scope-model-mode"]')
      .setValue("include");
    expect(
      (
        wrapper.get('[data-testid="request-trace-save"]')
          .element as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    await wrapper.get('[data-testid="request-trace-save"]').trigger("click");
    expect(api.updateOperatorSettings).not.toHaveBeenCalled();
  });
});

describe("Trace capture enable, save and emergency disable", () => {
  it("enables only after the verbatim statement and applies the whole draft", async () => {
    api.updateOperatorSettings.mockResolvedValue(
      status({ enabled: true, capture_allowed: true }),
    );
    const wrapper = mountSettings();
    const enable = wrapper.get('[data-testid="request-trace-enable"]');
    expect((enable.element as HTMLButtonElement).disabled).toBe(true);
    await wrapper
      .get('[data-testid="request-trace-phrase-input"]')
      .setValue(`${phrase} changed`);
    expect((enable.element as HTMLButtonElement).disabled).toBe(true);
    await wrapper
      .get('[data-testid="request-trace-phrase-input"]')
      .setValue(phrase);
    expect((enable.element as HTMLButtonElement).disabled).toBe(false);
    await enable.trigger("click");
    await flushPromises();
    expect(api.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      language: "en",
      phrase,
      scope_provided: true,
      all_groups: true,
      group_ids: [],
      model_scope: "all",
      models: [],
      platform_scope: "all",
      platforms: [],
      capture_body: true,
      capture_http_200: true,
      sample_rate_http_200: 100,
      sample_rate_other: 100,
      body_max_bytes: 1048576,
      capture_duration_seconds: 0,
    });
    expect(wrapper.emitted("updated")).toBeTruthy();
  });

  it("saves a disabled gate without a phrase", async () => {
    api.updateOperatorSettings.mockResolvedValue(status());
    const wrapper = mountSettings();
    await wrapper
      .get('[data-testid="request-trace-preset-lite"]')
      .trigger("click");
    await wrapper.get('[data-testid="request-trace-save"]').trigger("click");
    await flushPromises();
    expect(api.updateOperatorSettings).toHaveBeenCalledWith(
      expect.objectContaining({ enabled: false, phrase: "" }),
    );
  });

  it("requires a freshly typed statement to save while capture is on", async () => {
    api.updateOperatorSettings.mockResolvedValue(
      status({ enabled: true, capture_allowed: true }),
    );
    const wrapper = mountSettings(
      status({
        enabled: true,
        capture_allowed: true,
        risk_acknowledged: true,
        risk_acknowledgement_current: true,
      }),
    );
    const save = wrapper.get('[data-testid="request-trace-save"]');
    expect((save.element as HTMLButtonElement).disabled).toBe(true);
    await wrapper
      .get('[data-testid="request-trace-phrase-input"]')
      .setValue(phrase);
    expect((save.element as HTMLButtonElement).disabled).toBe(false);
    await save.trigger("click");
    await flushPromises();
    expect(api.updateOperatorSettings).toHaveBeenCalledWith(
      expect.objectContaining({ enabled: true, phrase }),
    );
  });

  it("keeps the emergency stop available on an unsupported deployment", async () => {
    api.updateOperatorSettings.mockResolvedValue(status());
    const wrapper = mountSettings(
      status({
        enabled: true,
        capture_allowed: false,
        plaintext_capture_supported: false,
        plaintext_capture_support_reason: "unsupported_partitioned_usage_logs",
      }),
    );
    expect(
      wrapper.find('[data-testid="request-trace-deployment-blocked"]').exists(),
    ).toBe(true);
    expect(wrapper.find('[data-testid="request-trace-enable"]').exists()).toBe(
      false,
    );
    await wrapper.get('[data-testid="request-trace-disable"]').trigger("click");
    await flushPromises();
    expect(api.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: false,
      language: "en",
      phrase: "",
    });
  });

  it("stops capture without a phrase even when the draft is invalid", async () => {
    api.updateOperatorSettings.mockResolvedValue(status());
    const wrapper = mountSettings(
      status({ enabled: true, capture_allowed: true }),
    );
    // Make the draft unusable: an include scope with no models selected.
    await wrapper
      .get('[data-testid="request-trace-scope-model-mode"]')
      .setValue("include");
    await wrapper.get('[data-testid="request-trace-disable"]').trigger("click");
    await flushPromises();
    const [payload] = api.updateOperatorSettings.mock.calls[0];
    expect(Object.keys(payload as object).sort()).toEqual([
      "enabled",
      "language",
      "phrase",
    ]);
    expect(payload).toEqual({ enabled: false, language: "en", phrase: "" });
  });
});

describe("Trace capture advanced and draft bar", () => {
  it("resets the draft back to the applied values", async () => {
    const wrapper = mountSettings();
    expect(
      wrapper.get('[data-testid="request-trace-draft-state"]').text(),
    ).toBe("admin.requestTrace.operator.actions.synced");
    await wrapper
      .get('[data-testid="request-trace-http200-toggle"]')
      .trigger("click");
    expect(
      wrapper.get('[data-testid="request-trace-draft-state"]').text(),
    ).toBe("admin.requestTrace.operator.actions.dirty");
    await wrapper.get('[data-testid="request-trace-reset"]').trigger("click");
    expect(
      wrapper.get('[data-testid="request-trace-draft-state"]').text(),
    ).toBe("admin.requestTrace.operator.actions.synced");
  });

  it("sends the changed stop window on save but only renews explicitly", async () => {
    api.updateOperatorSettings.mockImplementation(async (input: object) =>
      status(input as Partial<RequestTraceOperatorStatus>),
    );
    const wrapper = mountSettings(
      status({ enabled: true, capture_allowed: true }),
    );
    await wrapper
      .get('[data-testid="request-trace-phrase-input"]')
      .setValue(phrase);
    await openAdvanced(wrapper);
    await wrapper.get("#request-trace-duration").setValue("900");
    await wrapper.get('[data-testid="request-trace-save"]').trigger("click");
    await flushPromises();
    const [savePayload] = api.updateOperatorSettings.mock.calls[0];
    expect(savePayload).toMatchObject({
      enabled: true,
      capture_duration_seconds: 900,
    });
    expect(savePayload).not.toHaveProperty("renew_capture_window");

    // A save while capture is on consumes the statement; a restart needs it again.
    await wrapper
      .get('[data-testid="request-trace-phrase-input"]')
      .setValue(phrase);
    await wrapper.get('[data-testid="request-trace-renew"]').trigger("click");
    await flushPromises();
    const renewPayload = api.updateOperatorSettings.mock.calls[1][0];
    expect(renewPayload).toMatchObject({
      enabled: true,
      capture_duration_seconds: 900,
      renew_capture_window: true,
    });
  });

  it("does not offer a window restart while the master switch is off", async () => {
    const wrapper = mountSettings(status({ enabled: false }));
    await openAdvanced(wrapper);
    await wrapper.get("#request-trace-duration").setValue("900");
    expect(
      (
        wrapper.get('[data-testid="request-trace-renew"]')
          .element as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    expect(
      wrapper
        .find('[data-testid="request-trace-renew-needs-enabled"]')
        .exists(),
    ).toBe(true);
    await wrapper.get('[data-testid="request-trace-renew"]').trigger("click");
    expect(api.updateOperatorSettings).not.toHaveBeenCalled();
  });

  it("does not offer a restart when there is no timed window to restart", async () => {
    const wrapper = mountSettings(
      status({ enabled: true, capture_allowed: true }),
    );
    await openAdvanced(wrapper);
    // Duration is still "no time limit" (0): nothing to restart.
    expect(
      (
        wrapper.get('[data-testid="request-trace-renew"]')
          .element as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    expect(
      wrapper
        .find('[data-testid="request-trace-renew-needs-duration"]')
        .exists(),
    ).toBe(true);
  });

  it("keeps the risk field typeable on an unsupported deployment while capture is on", async () => {
    const wrapper = mountSettings(
      status({
        enabled: true,
        capture_allowed: false,
        plaintext_capture_supported: false,
        plaintext_capture_support_reason:
          "unsupported_missing_ownership_foreign_key",
      }),
    );
    // The field is not hidden: the operator can type and attempt the write even
    // though the deployment gate blocks enable/renew.
    const phraseField = wrapper.get(
      '[data-testid="request-trace-phrase-input"]',
    );
    await phraseField.setValue(phrase);
    expect((phraseField.element as HTMLTextAreaElement).value).toBe(phrase);
    expect(wrapper.find('[data-testid="request-trace-enable"]').exists()).toBe(
      false,
    );
    const renew = wrapper.get('[data-testid="request-trace-renew"]');
    expect((renew.element as HTMLButtonElement).disabled).toBe(true);
  });
});
