import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";
import UsageDetailModal from "../UsageDetailModal.vue";

const api = vi.hoisted(() => ({ getForcedRequestAudit: vi.fn() }));
vi.mock("@/api/admin/usage", () => ({
  getForcedRequestAudit: api.getForcedRequestAudit,
}));
vi.mock("vue-i18n", async (importOriginal) => {
  const actual = await importOriginal<typeof import("vue-i18n")>();
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) };
});

const usage = {
  id: 42,
  created_at: "2026-09-30T00:00:00Z",
  request_id: "client-reusable",
  model: "claude-sonnet-4",
  input_tokens: 12,
  output_tokens: 8,
  cache_read_tokens: 1,
  cache_creation_tokens: 2,
  total_cost: 0.025,
  actual_cost: 0.02,
  user_id: 3,
  api_key_id: 4,
  account_id: 5,
  stream: true,
  duration_ms: 231,
  first_token_ms: 64,
  request_trace_id: "a".repeat(32),
  request_trace_available: true,
  request_audit_forced_available: true,
};
const DialogStub = {
  props: ["show", "title"],
  template: '<section v-if="show" :data-title="title"><slot /></section>',
};

function mountModal(overrides = {}) {
  return mount(UsageDetailModal, {
    props: { show: true, usage: { ...usage, ...overrides } },
    global: {
      stubs: {
        BaseDialog: DialogStub,
        RequestTraceDetailContent: {
          props: ["show", "traceId"],
          template: '<div data-testid="trace-content-stub" />',
        },
      },
    },
  });
}

describe("admin usage detail", () => {
  beforeEach(() => api.getForcedRequestAudit.mockReset());

  it("shows metering facts and switches to a readable Trace without routing", async () => {
    const wrapper = mountModal();
    expect(wrapper.text()).toContain("claude-sonnet-4");
    expect(wrapper.text()).toContain("12");
    expect(wrapper.text()).toContain("0.02");
    await wrapper.get('[data-testid="usage-view-trace"]').trigger("click");
    expect(wrapper.get('[data-testid="usage-trace-panel"]').exists()).toBe(
      true,
    );
    expect(wrapper.find('[data-testid="usage-detail-summary"]').exists()).toBe(
      false,
    );
    expect(wrapper.findAll('[data-testid="usage-trace-panel"]')).toHaveLength(
      1,
    );
    await wrapper.get('[data-testid="usage-trace-back"]').trigger("click");
    expect(wrapper.find('[data-testid="usage-detail-summary"]').exists()).toBe(
      true,
    );
  });

  it("does not infer Trace from client request ID or expose unknown-origin audit", () => {
    const wrapper = mountModal({
      request_trace_available: false,
      request_audit_forced_available: false,
    });
    expect(wrapper.find('[data-testid="usage-view-trace"]').exists()).toBe(
      false,
    );
    expect(wrapper.find('[data-testid="usage-forced-audit"]').exists()).toBe(
      false,
    );
    expect(
      wrapper.get('[data-testid="usage-trace-unavailable"]').text(),
    ).toContain("traceUnavailable");
    expect(api.getForcedRequestAudit).not.toHaveBeenCalled();
  });

  it("loads verified forced metadata only after the advanced action", async () => {
    api.getForcedRequestAudit.mockResolvedValue({
      usage_log_id: 42,
      attempts: [],
      events: [],
      capture_completeness: "incomplete",
      capture_reason: "missing_reservation",
    });
    const wrapper = mountModal();
    expect(api.getForcedRequestAudit).not.toHaveBeenCalled();
    await wrapper.get('[data-testid="usage-forced-audit"]').trigger("click");
    await flushPromises();
    expect(api.getForcedRequestAudit).toHaveBeenCalledWith(42);
    expect(
      wrapper.get('[data-testid="usage-forced-audit-detail"]').text(),
    ).toContain("captureStatuses.incomplete");
    expect(
      wrapper.get('[data-testid="usage-forced-audit-reason"]').text(),
    ).toContain("forcedReasons.missing_reservation");
  });
});
