/**
 * The export settings section: the written risk acknowledgement and the task caps.
 *
 * The acknowledgement is a separate statement from the capture one, and it is
 * compared word for word — a submit is only possible once the operator has
 * reproduced the server's own text, and the text is never replayed for them. The
 * caps are finite by construction, and the panel says when a value it sent was
 * clamped rather than echoing back what was typed.
 */
import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  getTraceExportRisk: vi.fn(),
  acknowledgeTraceExportRisk: vi.fn(),
  getTraceExportLimits: vi.fn(),
  updateTraceExportLimits: vi.fn(),
}));

vi.mock("../api", () => api);
vi.mock("vue-i18n", async (importOriginal) => {
  const actual = await importOriginal<typeof import("vue-i18n")>();
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) };
});

import RequestTraceExportSettings from "../RequestTraceExportSettings.vue";

const PHRASE_EN =
  "Export risk: the manifest and every shard are plaintext on this machine.";
const PHRASE_ZH = "导出风险：清单与各分片均为本机明文。";

const risk = (overrides: Record<string, unknown> = {}) => ({
  acknowledged: false,
  version: "v2026.09.30",
  phrase_en: PHRASE_EN,
  phrase_zh: PHRASE_ZH,
  ...overrides,
});

const limits = (overrides: Record<string, unknown> = {}) => ({
  max_rows: 10_000,
  max_bytes: 134_217_728,
  max_runtime_seconds: 600,
  max_shard_rows: 2_000,
  max_shard_bytes: 33_554_432,
  configured: false,
  ...overrides,
});

async function mountPanel(
  riskPayload: unknown = risk(),
  limitsPayload: unknown = limits(),
) {
  api.getTraceExportRisk.mockResolvedValue(riskPayload);
  api.getTraceExportLimits.mockResolvedValue(limitsPayload);
  const wrapper = mount(RequestTraceExportSettings);
  await flushPromises();
  return wrapper;
}

describe("Request Trace export settings", () => {
  beforeEach(() => Object.values(api).forEach((mock) => mock.mockReset()));

  it("shows the server statement and keeps the acknowledgement disabled until it matches", async () => {
    const wrapper = await mountPanel();

    expect(api.getTraceExportRisk).toHaveBeenCalledTimes(1);
    expect(
      wrapper.get('[data-testid="request-trace-export-risk-phrase"]').text(),
    ).toBe(PHRASE_EN);
    expect(
      wrapper
        .get('[data-testid="request-trace-export-risk-state"]')
        .attributes("data-state"),
    ).toBe("pending");

    const submit = wrapper.get(
      '[data-testid="request-trace-export-risk-submit"]',
    );
    expect((submit.element as HTMLButtonElement).disabled).toBe(true);
    await wrapper
      .get('[data-testid="request-trace-export-risk-input"]')
      .setValue(PHRASE_EN + " changed");
    expect((submit.element as HTMLButtonElement).disabled).toBe(true);
    expect(api.acknowledgeTraceExportRisk).not.toHaveBeenCalled();
  });

  it("submits the statement verbatim in the chosen language, then drops the typed copy", async () => {
    api.acknowledgeTraceExportRisk.mockResolvedValue(
      risk({ acknowledged: true, accepted_at: "2026-09-30T00:00:00Z" }),
    );
    const wrapper = await mountPanel();
    await wrapper
      .get('[data-testid="request-trace-export-risk-language"]')
      .setValue("zh");
    expect(
      wrapper.get('[data-testid="request-trace-export-risk-phrase"]').text(),
    ).toBe(PHRASE_ZH);

    await wrapper
      .get('[data-testid="request-trace-export-risk-input"]')
      .setValue(PHRASE_ZH);
    const submit = wrapper.get(
      '[data-testid="request-trace-export-risk-submit"]',
    );
    expect((submit.element as HTMLButtonElement).disabled).toBe(false);
    await submit.trigger("click");
    await flushPromises();

    expect(api.acknowledgeTraceExportRisk).toHaveBeenCalledWith({
      language: "zh",
      phrase: PHRASE_ZH,
    });
    expect(
      (
        wrapper.get('[data-testid="request-trace-export-risk-input"]')
          .element as HTMLTextAreaElement
      ).value,
    ).toBe("");
    expect(
      wrapper
        .get('[data-testid="request-trace-export-risk-state"]')
        .attributes("data-state"),
    ).toBe("acknowledged");
    expect(
      wrapper.get('[data-testid="request-trace-export-risk-saved"]').exists(),
    ).toBe(true);
  });

  it("reports a refused acknowledgement without echoing any server text", async () => {
    api.acknowledgeTraceExportRisk.mockRejectedValue({
      reason: "REQUEST_TRACE_EXPORT_RISK_ACK_INVALID",
      message: "SECRET_SERVER_TEXT",
    });
    const wrapper = await mountPanel();
    await wrapper
      .get('[data-testid="request-trace-export-risk-input"]')
      .setValue(PHRASE_EN);
    await wrapper
      .get('[data-testid="request-trace-export-risk-submit"]')
      .trigger("click");
    await flushPromises();

    expect(
      wrapper.get('[data-testid="request-trace-export-risk-error"]').exists(),
    ).toBe(true);
    expect(wrapper.text()).not.toContain("SECRET_SERVER_TEXT");
    expect(wrapper.text()).not.toContain(
      "REQUEST_TRACE_EXPORT_RISK_ACK_INVALID",
    );
  });

  it("never claims the statement is unacknowledged when the state cannot be read", async () => {
    api.getTraceExportRisk.mockRejectedValue(new Error("503"));
    const wrapper = mount(RequestTraceExportSettings);
    await flushPromises();

    expect(
      wrapper
        .get('[data-testid="request-trace-export-risk-unavailable"]')
        .exists(),
    ).toBe(true);
    expect(
      wrapper.find('[data-testid="request-trace-export-risk-state"]').exists(),
    ).toBe(false);
    expect(wrapper.text()).not.toContain("503");
  });

  it("shows the stored caps with their allowed ranges and says when they are still defaults", async () => {
    const wrapper = await mountPanel();

    expect(
      (
        wrapper.get('[data-testid="request-trace-export-limit-max_rows"]')
          .element as HTMLInputElement
      ).value,
    ).toBe("10000");
    expect(
      wrapper
        .get('[data-testid="request-trace-export-limits-provenance"]')
        .text(),
    ).toBe("admin.requestTrace.export.settings.limits.defaults");
    // Every field states the range the server clamps to, so "how large may this be" is answerable.
    for (const key of ["max_rows", "max_bytes", "max_runtime_seconds"]) {
      expect(
        wrapper
          .get(`[data-testid="request-trace-export-limit-${key}-hint"]`)
          .text(),
      ).toBe("admin.requestTrace.export.settings.limits.range");
    }
    // A shard cap is additionally bounded by the task cap it belongs to.
    for (const key of ["max_shard_rows", "max_shard_bytes"]) {
      expect(
        wrapper
          .get(`[data-testid="request-trace-export-limit-${key}-hint"]`)
          .text(),
      ).toBe("admin.requestTrace.export.settings.limits.shardRange");
    }
  });

  it("saves all five caps in one payload, never a partial one", async () => {
    api.updateTraceExportLimits.mockResolvedValue(
      limits({
        max_rows: 500,
        max_bytes: 2_000_000,
        max_runtime_seconds: 60,
        max_shard_rows: 100,
        max_shard_bytes: 1_500_000,
        configured: true,
      }),
    );
    const wrapper = await mountPanel();
    await wrapper
      .get('[data-testid="request-trace-export-limit-max_rows"]')
      .setValue("500");
    await wrapper
      .get('[data-testid="request-trace-export-limit-max_bytes"]')
      .setValue("2000000");
    await wrapper
      .get('[data-testid="request-trace-export-limit-max_runtime_seconds"]')
      .setValue("60");
    await wrapper
      .get('[data-testid="request-trace-export-limit-max_shard_rows"]')
      .setValue("100");
    await wrapper
      .get('[data-testid="request-trace-export-limit-max_shard_bytes"]')
      .setValue("1500000");
    await wrapper
      .get('[data-testid="request-trace-export-limits-save"]')
      .trigger("click");
    await flushPromises();

    expect(api.updateTraceExportLimits).toHaveBeenCalledWith({
      max_rows: 500,
      max_bytes: 2_000_000,
      max_runtime_seconds: 60,
      max_shard_rows: 100,
      max_shard_bytes: 1_500_000,
      configured: true,
    });
    expect(
      wrapper.get('[data-testid="request-trace-export-limits-saved"]').exists(),
    ).toBe(true);
    expect(
      wrapper
        .get('[data-testid="request-trace-export-limits-provenance"]')
        .text(),
    ).toBe("admin.requestTrace.export.settings.limits.configured");
  });

  it("shows the values the server accepted, not the ones that were typed", async () => {
    // The server clamps; what it stored is what it answers with, and that is what
    // a new task will snapshot. The form must not keep showing the typed value.
    api.updateTraceExportLimits.mockResolvedValue(
      limits({ max_shard_rows: 1000, configured: true }),
    );
    const wrapper = await mountPanel();
    await wrapper
      .get('[data-testid="request-trace-export-limit-max_shard_rows"]')
      .setValue("2000");
    await wrapper
      .get('[data-testid="request-trace-export-limits-save"]')
      .trigger("click");
    await flushPromises();

    expect(api.updateTraceExportLimits).toHaveBeenCalledWith(
      expect.objectContaining({ max_shard_rows: 2000 }),
    );
    expect(
      (
        wrapper.get('[data-testid="request-trace-export-limit-max_shard_rows"]')
          .element as HTMLInputElement
      ).value,
    ).toBe("1000");
    expect(
      wrapper
        .find('[data-testid="request-trace-export-limits-error"]')
        .exists(),
    ).toBe(false);
  });

  it("refuses a cap outside the allowed range rather than sending it hopefully", async () => {
    const wrapper = await mountPanel();
    await wrapper
      .get('[data-testid="request-trace-export-limit-max_rows"]')
      .setValue("0");
    await wrapper
      .get('[data-testid="request-trace-export-limits-save"]')
      .trigger("click");
    expect(
      wrapper.get('[data-testid="request-trace-export-limits-error"]').exists(),
    ).toBe(true);
    expect(api.updateTraceExportLimits).not.toHaveBeenCalled();

    // A shard cap above the task cap is not a cap the server could keep.
    await wrapper
      .get('[data-testid="request-trace-export-limit-max_rows"]')
      .setValue("1000");
    await wrapper
      .get('[data-testid="request-trace-export-limit-max_shard_rows"]')
      .setValue("2000");
    await wrapper
      .get('[data-testid="request-trace-export-limits-save"]')
      .trigger("click");
    expect(
      wrapper.get('[data-testid="request-trace-export-limits-error"]').exists(),
    ).toBe(true);
    expect(api.updateTraceExportLimits).not.toHaveBeenCalled();
  });

  it("clears the saved confirmation as soon as the draft is edited again", async () => {
    api.updateTraceExportLimits.mockResolvedValue(limits({ configured: true }));
    const wrapper = await mountPanel();
    await wrapper
      .get('[data-testid="request-trace-export-limits-save"]')
      .trigger("click");
    await flushPromises();
    expect(
      wrapper.get('[data-testid="request-trace-export-limits-saved"]').exists(),
    ).toBe(true);

    await wrapper
      .get('[data-testid="request-trace-export-limit-max_rows"]')
      .setValue("10001");
    expect(
      wrapper
        .find('[data-testid="request-trace-export-limits-saved"]')
        .exists(),
    ).toBe(false);
  });

  it("reports unreadable caps as unknown rather than showing zero limits", async () => {
    api.getTraceExportLimits.mockRejectedValue(new Error("503"));
    const wrapper = mount(RequestTraceExportSettings);
    await flushPromises();

    expect(
      wrapper
        .get('[data-testid="request-trace-export-limits-unavailable"]')
        .exists(),
    ).toBe(true);
    expect(
      wrapper
        .find('[data-testid="request-trace-export-limit-max_rows"]')
        .exists(),
    ).toBe(false);
  });
});
