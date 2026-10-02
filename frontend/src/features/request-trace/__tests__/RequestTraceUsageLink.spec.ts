import { flushPromises, mount } from "@vue/test-utils";
import { defineComponent } from "vue";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ getTrace: vi.fn() }));
vi.mock("../api", () => ({ getTrace: api.getTrace }));
vi.mock("vue-i18n", async (importOriginal) => {
  const actual = await importOriginal<typeof import("vue-i18n")>();
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) };
});

import RequestTraceDetailDrawer from "../RequestTraceDetailDrawer.vue";

/** Follows the repo convention: RouterLink is stubbed and asserted through its `to` prop. */
const RouterLinkStub = defineComponent({
  name: "RouterLink",
  props: { to: { type: Object, required: true } },
  template: "<a><slot /></a>",
});
const BaseDialogStub = {
  props: ["show"],
  emits: ["close"],
  template: '<div v-if="show" data-testid="trace-dialog"><slot /></div>',
};

const detail = (usageLogId: number | null) => ({
  trace_id: "a".repeat(32),
  route_family: "messages",
  inbound_endpoint: "/v1/messages",
  created_at: "2026-09-28T00:00:00Z",
  completed_at: "2026-09-28T00:00:01Z",
  client_status: 200,
  capture_state: "stored",
  usage_log_id: usageLogId,
  cleanup_after: null,
  stages: [],
});

function mountDrawer(usageLogId: number | null) {
  api.getTrace.mockResolvedValue(detail(usageLogId));
  return mount(RequestTraceDetailDrawer, {
    props: { show: true, traceId: "a".repeat(32) },
    global: {
      stubs: { BaseDialog: BaseDialogStub, RouterLink: RouterLinkStub },
    },
  });
}

/**
 * The Trace detail only knows a usage_log_id. It must point at the existing admin
 * usage page, which owns the metering facts, instead of inventing token or cost
 * figures from the request payload. The link carries no Trace body and no plaintext.
 */
describe("Trace detail usage record link", () => {
  beforeEach(() => {
    api.getTrace.mockReset();
  });

  it("links the usage record to that exact row on the admin usage page", async () => {
    const wrapper = mountDrawer(4242);
    await flushPromises();
    const link = wrapper.getComponent(RouterLinkStub);
    // The usage page owns the metering facts, so the link must open on that exact
    // record: a bare /admin/usage list would leave the operator searching for the
    // record the Trace pointed at.
    expect(link.props("to")).toEqual({
      path: "/admin/usage",
      query: { usage_log_id: "4242" },
    });
    expect(
      wrapper.get('[data-testid="trace-detail-usage-link"]').text(),
    ).toContain("#4242");
  });

  it("shows no link when the Trace has no usage record", async () => {
    const wrapper = mountDrawer(null);
    await flushPromises();
    expect(
      wrapper.find('[data-testid="trace-detail-usage-link"]').exists(),
    ).toBe(false);
    expect(wrapper.text()).toContain("admin.requestTrace.list.usageAbsent");
  });

  it("does not fabricate token or cost facts from the request payload", async () => {
    api.getTrace.mockResolvedValue({
      ...detail(4242),
      stages: [
        {
          ordinal: 1,
          stage: "wire_attempt",
          attempt_index: 1,
          view_name: "wire",
          state: "stored",
          reason: "retained",
          observed_bytes: 12,
          retained_bytes: 12,
          dropped_events: 0,
          redaction_unverified: false,
          payload_text: '{"usage":{"input_tokens":9999}}',
          facts: {
            method: "POST",
            url: null,
            url_omitted: false,
            request_headers: {},
            request_headers_omitted: 0,
            response_headers: {},
            response_headers_omitted: 0,
            account_id: 73,
            model: null,
            protocol: null,
            value_protocol: null,
            status: 200,
            started_at: null,
            ended_at: null,
          },
        },
      ],
    });
    const wrapper = mountDrawer(4242);
    await flushPromises();
    // The payload text is a retained body, not a metering fact: no token/cost label may be added.
    expect(
      wrapper.find('[data-testid="trace-detail-usage-tokens"]').exists(),
    ).toBe(false);
    expect(
      wrapper.find('[data-testid="trace-detail-usage-cost"]').exists(),
    ).toBe(false);
  });
});
