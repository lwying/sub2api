import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ getTrace: vi.fn() }));
vi.mock("../api", () => ({ getTrace: api.getTrace }));
vi.mock("vue-i18n", async (importOriginal) => {
  const actual = await importOriginal<typeof import("vue-i18n")>();
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) };
});

import RequestTraceDetailDrawer from "../RequestTraceDetailDrawer.vue";
import {
  requestTraceDecisionKinds,
  requestTraceDecisionOutcomes,
  requestTraceDecisionSources,
  requestTraceStageNames,
  requestTraceStageReasons,
  requestTraceStageStates,
  requestTraceStageViews,
} from "../types";

const detail = (id: string, overrides: Record<string, unknown> = {}) => ({
  trace_id: id,
  route_family: "messages",
  inbound_endpoint: "/v1/messages",
  created_at: "2026-09-28T00:00:00Z",
  completed_at: "2026-09-28T00:00:01Z",
  client_status: 401,
  capture_state: "partial",
  usage_log_id: null,
  cleanup_after: "2026-10-28T00:00:00Z",
  stages: [
    {
      ordinal: 1,
      stage: "inbound_request",
      attempt_index: 0,
      view_name: "",
      state: "not_observed",
      reason: "auth_rejected_body_not_observed",
      observed_bytes: 0,
      retained_bytes: 0,
      dropped_events: 0,
      redaction_unverified: false,
    },
    {
      ordinal: 2,
      stage: "downstream_response",
      attempt_index: 0,
      view_name: "",
      state: "stored",
      reason: "recorded",
      observed_bytes: 0,
      retained_bytes: 0,
      dropped_events: 0,
      redaction_unverified: false,
    },
  ],
  ...overrides,
});
const BaseDialogStub = {
  props: ["show"],
  emits: ["close"],
  template: '<div v-if="show" data-testid="trace-dialog"><slot /></div>',
};
// The usage-record id renders as a link to the admin usage page; RouterLink is
// stubbed so a mount without a real router still resolves it.
const RouterLinkStub = {
  props: { to: { type: Object, required: true } },
  template: "<a><slot /></a>",
};
function mountDrawer(id: string) {
  return mount(RequestTraceDetailDrawer, {
    props: { show: true, traceId: id },
    global: {
      stubs: { BaseDialog: BaseDialogStub, RouterLink: RouterLinkStub },
    },
  });
}

/** A distinct valid Trace id per case, so no case can borrow another's render. */
function caseTraceId(seed: number): string {
  return seed.toString(16).padStart(2, "0").repeat(16);
}

/**
 * Every value of every closed decision set, mapped to the field it travels in
 * and the label map the drawer must address it through. A tier that stops at
 * one sampled value cannot prove the rest of the contract renders at all.
 */
const decisionLabelCases: [string, string, string, string, string][] = [];
requestTraceDecisionKinds.forEach((value, index) => {
  decisionLabelCases.push([
    "decision",
    value,
    "trace-decision-kind",
    "kindLabel",
    caseTraceId(index + 1),
  ]);
});
requestTraceDecisionOutcomes.forEach((value, index) => {
  decisionLabelCases.push([
    "outcome",
    value,
    "trace-decision-outcome",
    "outcomeLabel",
    caseTraceId(index + 8),
  ]);
});
requestTraceDecisionSources.forEach((value, index) => {
  decisionLabelCases.push([
    "source",
    value,
    "trace-decision-source",
    "sourceLabel",
    caseTraceId(index + 16),
  ]);
});

describe("admin Request Trace detail", () => {
  beforeEach(() => api.getTrace.mockReset());

  it("labels rejected request body as unobserved without inventing an upstream attempt", async () => {
    const id = "b".repeat(32);
    api.getTrace.mockResolvedValue(detail(id));
    const wrapper = mountDrawer(id);
    await flushPromises();
    expect(api.getTrace).toHaveBeenCalledWith(id, expect.anything());
    // The closed-set wire values render through their labels, never raw.
    expect(wrapper.get('[data-testid="trace-stage-state"]').text()).toBe(
      "admin.requestTrace.detail.stateLabel.not_observed",
    );
    expect(wrapper.get('[data-testid="trace-stage-reason"]').text()).toBe(
      "admin.requestTrace.detail.reasonLabel.auth_rejected_body_not_observed",
    );
    expect(
      wrapper.find('[data-testid="trace-upstream-attempt-1"]').exists(),
    ).toBe(false);
    expect(
      wrapper.get('[data-testid="trace-detail-cleanup-rule"]').text(),
    ).toContain("plannedCleanup");
  });

  it("never renders unapproved metadata or HTML, but shows high-risk retained text as text", async () => {
    const id = "c".repeat(32);
    api.getTrace.mockResolvedValue(
      detail(id, {
        stages: [
          {
            ordinal: 1,
            stage: "inbound_request",
            attempt_index: 0,
            view_name: "decoded",
            state: "redaction_unverified",
            reason: "redaction_unverified",
            redaction_unverified: true,
            observed_bytes: 13,
            retained_bytes: 13,
            dropped_events: 0,
            payload_text: "<img src=x onerror=alert(1)>",
            metadata: { authorization: "Bearer SECRET", credential: "SECRET" },
          },
        ],
      }),
    );
    const wrapper = mountDrawer(id);
    await flushPromises();
    expect(wrapper.text()).toContain("<img src=x onerror=alert(1)>");
    expect(wrapper.find("img").exists()).toBe(false);
    expect(wrapper.text()).not.toContain("Bearer SECRET");
    expect(wrapper.get('[data-testid="trace-risk-warning"]').exists()).toBe(
      true,
    );
  });

  it("renders typed stage facts as text and keeps credential placeholders", async () => {
    const id = "f".repeat(32);
    api.getTrace.mockResolvedValue(
      detail(id, {
        stages: [
          {
            ordinal: 1,
            stage: "wire_attempt",
            attempt_index: 1,
            view_name: "",
            state: "not_observed",
            reason: "wire_observed",
            observed_bytes: 0,
            retained_bytes: 0,
            dropped_events: 0,
            redaction_unverified: false,
            facts: {
              method: "POST",
              url: "https://api.example.com/v1/messages?api_key=%5BREDACTED%5D",
              url_omitted: false,
              request_headers: {
                Authorization: ["[REDACTED]"],
                "X-Echo": ["<img src=x onerror=alert(1)>"],
              },
              request_headers_omitted: 3,
              response_headers: { "Content-Type": ["application/json"] },
              response_headers_omitted: 0,
              account_id: 42,
              model: "claude-sonnet-4-5",
              protocol: "messages",
              value_protocol: "anthropic",
              status: 200,
              started_at: "2026-09-28T00:00:00Z",
              ended_at: "2026-09-28T00:00:01Z",
            },
          },
        ],
      }),
    );
    const wrapper = mountDrawer(id);
    await flushPromises();
    const facts = wrapper.get('[data-testid="trace-facts"]');
    expect(facts.get('[data-testid="trace-fact-method"]').text()).toContain(
      "POST",
    );
    expect(facts.get('[data-testid="trace-fact-url"]').text()).toContain(
      "api_key=%5BREDACTED%5D",
    );
    expect(
      facts.get('[data-testid="trace-fact-request-headers"]').text(),
    ).toContain("Authorization");
    expect(
      facts.get('[data-testid="trace-fact-request-headers"]').text(),
    ).toContain("[REDACTED]");
    expect(
      facts.get('[data-testid="trace-fact-request-headers"]').text(),
    ).toContain("<img src=x onerror=alert(1)>");
    expect(
      facts.get('[data-testid="trace-fact-request-headers-omitted"]').text(),
    ).toContain("3");
    expect(
      facts.get('[data-testid="trace-fact-response-headers"]').text(),
    ).toContain("Content-Type");
    expect(facts.get('[data-testid="trace-fact-account"]').text()).toContain(
      "42",
    );
    expect(facts.get('[data-testid="trace-fact-model"]').text()).toContain(
      "claude-sonnet-4-5",
    );
    expect(facts.get('[data-testid="trace-fact-protocol"]').text()).toContain(
      "messages",
    );
    expect(
      facts.get('[data-testid="trace-fact-value-protocol"]').text(),
    ).toContain("anthropic");
    expect(facts.get('[data-testid="trace-fact-status"]').text()).toContain(
      "200",
    );
    expect(facts.get('[data-testid="trace-fact-started-at"]').exists()).toBe(
      true,
    );
    expect(facts.get('[data-testid="trace-fact-ended-at"]').exists()).toBe(
      true,
    );
    expect(wrapper.find("img").exists()).toBe(false);
  });

  it("shows an omitted URL and absent response facts instead of empty observed values", async () => {
    const id = "a1".repeat(16);
    api.getTrace.mockResolvedValue(
      detail(id, {
        stages: [
          {
            ordinal: 1,
            stage: "client_metadata",
            attempt_index: 0,
            view_name: "",
            state: "not_observed",
            reason: "metadata_observed",
            observed_bytes: 0,
            retained_bytes: 0,
            dropped_events: 0,
            redaction_unverified: false,
            facts: {
              method: "POST",
              url: null,
              url_omitted: true,
              request_headers: { "X-Client": ["cli/1.0"] },
              request_headers_omitted: 0,
              response_headers: {},
              response_headers_omitted: 0,
              account_id: null,
              model: null,
              protocol: null,
              value_protocol: null,
              status: null,
              started_at: null,
              ended_at: null,
            },
          },
        ],
      }),
    );
    const wrapper = mountDrawer(id);
    await flushPromises();
    expect(wrapper.get('[data-testid="trace-fact-url-omitted"]').text()).toBe(
      "admin.requestTrace.detail.urlOmitted",
    );
    expect(wrapper.find('[data-testid="trace-fact-url"]').exists()).toBe(false);
    expect(
      wrapper.get('[data-testid="trace-fact-request-headers"]').text(),
    ).toContain("X-Client");
    for (const key of [
      "response-headers",
      "account",
      "model",
      "protocol",
      "value-protocol",
      "status",
      "started-at",
      "ended-at",
    ]) {
      expect(wrapper.find(`[data-testid="trace-fact-${key}"]`).exists()).toBe(
        false,
      );
    }
  });

  it("renders no facts section for a stage the server sent without facts", async () => {
    const id = "a2".repeat(16);
    api.getTrace.mockResolvedValue(detail(id));
    const wrapper = mountDrawer(id);
    await flushPromises();
    expect(wrapper.find('[data-testid="trace-facts"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="trace-stage-1"]').exists()).toBe(true);
  });

  it("drops a previous plaintext payload on close and ignores late responses", async () => {
    const first = "d".repeat(32);
    const second = "e".repeat(32);
    api.getTrace.mockResolvedValueOnce(
      detail(first, {
        stages: [
          {
            ordinal: 1,
            stage: "inbound_request",
            attempt_index: 0,
            view_name: "",
            state: "stored",
            reason: "recorded",
            observed_bytes: 6,
            retained_bytes: 6,
            dropped_events: 0,
            redaction_unverified: false,
            payload_text: "CANARY",
          },
        ],
      }),
    );
    let resolveSecond!: (value: unknown) => void;
    api.getTrace.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveSecond = resolve;
        }),
    );
    const wrapper = mountDrawer(first);
    await flushPromises();
    expect(wrapper.text()).toContain("CANARY");
    await wrapper.setProps({ traceId: second });
    expect(wrapper.text()).not.toContain("CANARY");
    await wrapper.setProps({ show: false });
    resolveSecond(
      detail(second, {
        stages: [
          {
            ordinal: 1,
            stage: "inbound_request",
            attempt_index: 0,
            view_name: "",
            state: "stored",
            reason: "recorded",
            observed_bytes: 4,
            retained_bytes: 4,
            dropped_events: 0,
            redaction_unverified: false,
            payload_text: "LATE",
          },
        ],
      }),
    );
    await flushPromises();
    expect(wrapper.text()).not.toContain("LATE");
    api.getTrace.mockResolvedValueOnce(detail(first));
    await wrapper.setProps({ show: true, traceId: first });
    await flushPromises();
    expect(wrapper.text()).not.toContain("LATE");
  });

  it("renders a gateway decision with its localized source and outcome and the attempt it belongs to", async () => {
    const id = "11".repeat(16);
    api.getTrace.mockResolvedValue(
      detail(id, {
        stages: [
          {
            ordinal: 3,
            stage: "gateway_decision",
            attempt_index: 2,
            view_name: "",
            state: "not_observed",
            reason: "model_mapping_rewritten",
            observed_bytes: 0,
            retained_bytes: 0,
            dropped_events: 0,
            redaction_unverified: false,
            decision: {
              decision: "model_mapping",
              outcome: "rewritten",
              source: "group",
              sequence: 3,
              model_from: "gpt-5.3-codex",
              model_to: "gpt-5.3-codex-spark",
              protocol_from: "messages",
              protocol_to: "chat_completions",
              account_id: 42,
              decided_at: "2026-09-28T00:00:00.123456789Z",
            },
          },
        ],
      }),
    );
    const wrapper = mountDrawer(id);
    await flushPromises();
    const decision = wrapper.get('[data-testid="trace-decision"]');
    expect(decision.get('[data-testid="trace-decision-kind"]').text()).toBe(
      "admin.requestTrace.detail.decision.kindLabel.model_mapping",
    );
    expect(decision.get('[data-testid="trace-decision-outcome"]').text()).toBe(
      "admin.requestTrace.detail.decision.outcomeLabel.rewritten",
    );
    expect(decision.get('[data-testid="trace-decision-source"]').text()).toBe(
      "admin.requestTrace.detail.decision.sourceLabel.group",
    );
    expect(decision.get('[data-testid="trace-decision-sequence"]').text()).toBe(
      "3",
    );
    expect(
      decision.get('[data-testid="trace-decision-model-from"]').text(),
    ).toContain("gpt-5.3-codex");
    expect(
      decision.get('[data-testid="trace-decision-model-to"]').text(),
    ).toContain("gpt-5.3-codex-spark");
    expect(
      decision.get('[data-testid="trace-decision-protocol-from"]').text(),
    ).toContain("messages");
    expect(
      decision.get('[data-testid="trace-decision-protocol-to"]').text(),
    ).toContain("chat_completions");
    expect(
      decision.get('[data-testid="trace-decision-account"]').text(),
    ).toContain("42");
    expect(
      decision.get('[data-testid="trace-decision-decided-at"]').exists(),
    ).toBe(true);
    expect(
      wrapper.get('[data-testid="trace-upstream-attempt-2"]').text(),
    ).toContain("2");
    expect(wrapper.find('[data-testid="trace-facts"]').exists()).toBe(false);
  });

  it("shows only the decision facts the server recorded, never free-form metadata", async () => {
    const id = "12".repeat(16);
    api.getTrace.mockResolvedValue(
      detail(id, {
        stages: [
          {
            ordinal: 1,
            stage: "gateway_decision",
            attempt_index: 0,
            view_name: "",
            state: "not_observed",
            reason: "route_selected",
            observed_bytes: 0,
            retained_bytes: 0,
            dropped_events: 0,
            redaction_unverified: false,
            decision: {
              decision: "route",
              outcome: "selected",
              source: "api_key",
              sequence: 1,
              model_from: null,
              model_to: "",
              protocol_from: null,
              protocol_to: null,
              account_id: null,
              decided_at: null,
              metadata: { authorization: "Bearer SECRET" },
              headers: { Authorization: ["Bearer SECRET"] },
            },
          },
        ],
      }),
    );
    const wrapper = mountDrawer(id);
    await flushPromises();
    for (const key of [
      "model-from",
      "model-to",
      "protocol-from",
      "protocol-to",
      "account",
      "decided-at",
    ]) {
      expect(
        wrapper.find(`[data-testid="trace-decision-${key}"]`).exists(),
      ).toBe(false);
    }
    expect(wrapper.get('[data-testid="trace-decision-source"]').text()).toBe(
      "admin.requestTrace.detail.decision.sourceLabel.api_key",
    );
    expect(wrapper.text()).not.toContain("Bearer SECRET");
    // A decision is a gateway-internal fact, so it carries no unobserved body to disclose.
    expect(wrapper.text()).not.toContain(
      "admin.requestTrace.detail.notObserved",
    );
    expect(wrapper.get('[data-testid="trace-decision"]').text()).toContain(
      "admin.requestTrace.detail.decision.note",
    );
  });

  it("never claims a body for a body-less decision stage, even without a decision record", async () => {
    const id = "14".repeat(16);
    api.getTrace.mockResolvedValue(
      detail(id, {
        stages: [
          {
            ordinal: 1,
            stage: "gateway_decision",
            attempt_index: 0,
            view_name: "",
            state: "not_observed",
            reason: "decision_not_recorded",
            observed_bytes: 0,
            retained_bytes: 0,
            dropped_events: 0,
            redaction_unverified: false,
          },
        ],
      }),
    );
    const wrapper = mountDrawer(id);
    await flushPromises();
    expect(wrapper.get('[data-testid="trace-stage-1"]').exists()).toBe(true);
    expect(
      wrapper.find('[data-testid="trace-stage-not-observed"]').exists(),
    ).toBe(false);
    expect(wrapper.find('[data-testid="trace-decision"]').exists()).toBe(false);
  });

  it("renders decision text as text and never as markup", async () => {
    const id = "13".repeat(16);
    api.getTrace.mockResolvedValue(
      detail(id, {
        stages: [
          {
            ordinal: 2,
            stage: "gateway_decision",
            attempt_index: 1,
            view_name: "",
            state: "not_observed",
            reason: "identity_rewritten",
            observed_bytes: 0,
            retained_bytes: 0,
            dropped_events: 0,
            redaction_unverified: false,
            decision: {
              decision: "identity",
              outcome: "rewritten",
              source: "identity",
              sequence: 2,
              model_from: "<img src=x onerror=alert(1)>",
              model_to: "<script>alert(1)</script>",
              protocol_from: "messages",
              protocol_to: "responses",
              account_id: 7,
              decided_at: null,
            },
          },
        ],
      }),
    );
    const wrapper = mountDrawer(id);
    await flushPromises();
    expect(
      wrapper.get('[data-testid="trace-decision-model-from"]').text(),
    ).toContain("<img src=x onerror=alert(1)>");
    expect(wrapper.find("img").exists()).toBe(false);
    expect(wrapper.find("script").exists()).toBe(false);
    expect(wrapper.get('[data-testid="trace-decision-outcome"]').text()).toBe(
      "admin.requestTrace.detail.decision.outcomeLabel.rewritten",
    );
  });

  it.each(decisionLabelCases)(
    "renders the localized %s label for %s",
    async (field, value, testid, group, id) => {
      api.getTrace.mockResolvedValue(
        detail(id, {
          stages: [
            {
              ordinal: 1,
              stage: "gateway_decision",
              attempt_index: 0,
              view_name: "",
              state: "not_observed",
              reason: "decision_recorded",
              observed_bytes: 0,
              retained_bytes: 0,
              dropped_events: 0,
              redaction_unverified: false,
              decision: {
                decision: "route",
                outcome: "selected",
                source: "inbound",
                sequence: 1,
                model_from: null,
                model_to: null,
                protocol_from: null,
                protocol_to: null,
                account_id: null,
                decided_at: null,
                [field]: value,
              },
            },
          ],
        }),
      );
      const wrapper = mountDrawer(id);
      await flushPromises();
      expect(wrapper.get(`[data-testid="${testid}"]`).text()).toBe(
        `admin.requestTrace.detail.decision.${group}.${value}`,
      );
      wrapper.unmount();
    },
  );

  /**
   * Every value of every closed stage set the drawer renders, mapped to the field
   * it travels in, the label map it must address and the element it must reach. A
   * tier that stops at one sampled value cannot prove the rest of the contract
   * renders through a label at all.
   */
  const stageLabelCases: [string, string, string, string, string][] = [];
  requestTraceStageNames.forEach((value, index) => {
    stageLabelCases.push([
      "stage",
      value,
      "trace-stage-name",
      "stageLabel",
      caseTraceId(index + 32),
    ]);
  });
  requestTraceStageStates.forEach((value, index) => {
    stageLabelCases.push([
      "state",
      value,
      "trace-stage-state",
      "stateLabel",
      caseTraceId(index + 48),
    ]);
  });
  requestTraceStageViews.forEach((value, index) => {
    stageLabelCases.push([
      "view_name",
      value,
      "trace-stage-view",
      "viewLabel",
      caseTraceId(index + 64),
    ]);
  });
  requestTraceStageReasons.forEach((value, index) => {
    stageLabelCases.push([
      "reason",
      value,
      "trace-stage-reason",
      "reasonLabel",
      caseTraceId(index + 80),
    ]);
  });

  it.each(stageLabelCases)(
    "renders the localized %s label for %s instead of the raw token",
    async (field, value, testid, group, id) => {
      api.getTrace.mockResolvedValue(
        detail(id, {
          stages: [
            {
              ordinal: 1,
              stage: "wire_attempt",
              attempt_index: 0,
              view_name: "decoded",
              state: "stored",
              reason: "retained",
              observed_bytes: 0,
              retained_bytes: 0,
              dropped_events: 0,
              redaction_unverified: false,
              [field]: value,
            },
          ],
        }),
      );
      const wrapper = mountDrawer(id);
      await flushPromises();
      const rendered = wrapper.get(`[data-testid="${testid}"]`).text();
      expect(rendered).toBe(`admin.requestTrace.detail.${group}.${value}`);
      expect(rendered).not.toBe(value);
      wrapper.unmount();
    },
  );

  const unknownStageCases: [string, string, string, string][] = [
    ["stage", "some_future_stage", "trace-stage-name", "stageLabel"],
    ["view_name", "some_future_view", "trace-stage-view", "viewLabel"],
  ];

  it.each(unknownStageCases)(
    "labels an unknown %s generically instead of echoing it",
    async (field, value, testid, group) => {
      const id = caseTraceId(112);
      api.getTrace.mockResolvedValue(
        detail(id, {
          stages: [
            {
              ordinal: 1,
              stage: "wire_attempt",
              attempt_index: 0,
              view_name: "decoded",
              state: "stored",
              reason: "retained",
              observed_bytes: 0,
              retained_bytes: 0,
              dropped_events: 0,
              redaction_unverified: false,
              [field]: value,
            },
          ],
        }),
      );
      const wrapper = mountDrawer(id);
      await flushPromises();
      const rendered = wrapper.get(`[data-testid="${testid}"]`).text();
      expect(rendered).toBe(`admin.requestTrace.detail.${group}.unknown`);
      expect(rendered).not.toContain(value);
      wrapper.unmount();
    },
  );

  it("labels an unrecognized reason code generically and keeps the code out of the text", async () => {
    const id = caseTraceId(113);
    const reason = "some_future_reason_code";
    api.getTrace.mockResolvedValue(
      detail(id, {
        stages: [
          {
            ordinal: 1,
            stage: "wire_attempt",
            attempt_index: 0,
            view_name: "decoded",
            state: "stored",
            reason,
            observed_bytes: 0,
            retained_bytes: 0,
            dropped_events: 0,
            redaction_unverified: false,
          },
        ],
      }),
    );
    const wrapper = mountDrawer(id);
    await flushPromises();
    const rendered = wrapper.get('[data-testid="trace-stage-reason"]');
    expect(rendered.text()).toBe("admin.requestTrace.detail.reasonLabel.other");
    expect(rendered.text()).not.toContain(reason);
    // The bounded code stays available for debugging, but only as a non-display attribute.
    expect(rendered.attributes("title")).toBe(reason);
    wrapper.unmount();
  });
});
