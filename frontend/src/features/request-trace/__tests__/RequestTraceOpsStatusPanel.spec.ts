/**
 * The operational status panel reads `GET /admin/request-traces/status` on
 * demand and renders counts, booleans and closed sets only.
 *
 * Two honesty rules drive these cases: a state the endpoint cannot report (the
 * capture gate) is never inferred from the counters, and a state it could not
 * read (an unavailable probe, an unmeasured backlog, a failed read) is never
 * rendered as a number or as "off".
 */
import { flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({ getTraceOpsStatus: vi.fn() }));
vi.mock("../api", () => mocks);
vi.mock("vue-i18n", async (importOriginal) => {
  const actual = await importOriginal<typeof import("vue-i18n")>();
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) };
});

import RequestTraceOpsStatusPanel from "../RequestTraceOpsStatusPanel.vue";
import {
  requestTraceBacklogStates,
  requestTraceStorageProbeStates,
  requestTraceStorageStates,
} from "../types";

type Raw = Record<string, unknown>;

const capture = (patch: Raw = {}): Raw => ({
  storage: "ok",
  repository_available: true,
  stopped: false,
  queue_depth: 3,
  queue_capacity: 8,
  accepted: 41,
  stored: 38,
  write_failed: 0,
  dropped: 0,
  rejected: 2,
  ...patch,
});

const cleanup = (patch: Raw = {}): Raw => ({
  runs: 6,
  deleted: 12,
  failures: 0,
  last_deleted: 4,
  unlinked_backlog: 40,
  backlog_limit: 1000,
  backlog_state: "measured",
  ...patch,
});

const status = (patch: Raw = {}): Raw => ({
  storage_probe: "reachable",
  capture: capture(),
  export: {
    worker_started: true,
    ticks: 9,
    tasks_run: 5,
    tasks_completed: 4,
    tasks_failed: 1,
    failures: 2,
    disabled_ticks: 0,
    cleanups: 1,
    cleaned_files: 2,
  },
  cleanup: cleanup(),
  ...patch,
});

function mountPanel() {
  return mount(RequestTraceOpsStatusPanel);
}

async function mountLoaded(payload: Raw = status()) {
  mocks.getTraceOpsStatus.mockResolvedValue(payload);
  const wrapper = mountPanel();
  await flushPromises();
  return wrapper;
}

describe("admin Request Trace operational status panel", () => {
  beforeEach(() => mocks.getTraceOpsStatus.mockReset());
  afterEach(() => vi.restoreAllMocks());

  it("renders the queue counters, the store probe and the cleanup backlog as counts", async () => {
    const wrapper = await mountLoaded();

    expect(mocks.getTraceOpsStatus).toHaveBeenCalledTimes(1);
    expect(
      wrapper.get('[data-testid="request-trace-ops-accepted"]').text(),
    ).toContain("41");
    expect(
      wrapper.get('[data-testid="request-trace-ops-stored"]').text(),
    ).toContain("38");
    expect(
      wrapper.get('[data-testid="request-trace-ops-write-failed"]').text(),
    ).toContain("0");
    expect(
      wrapper.get('[data-testid="request-trace-ops-dropped"]').text(),
    ).toContain("0");
    expect(
      wrapper.get('[data-testid="request-trace-ops-rejected"]').text(),
    ).toContain("2");
    expect(
      wrapper.get('[data-testid="request-trace-ops-queue"]').text(),
    ).toContain("3");
    expect(
      wrapper.get('[data-testid="request-trace-ops-queue"]').text(),
    ).toContain("8");
    expect(wrapper.get('[data-testid="request-trace-ops-probe"]').text()).toBe(
      "admin.requestTrace.ops.storageProbe.reachable",
    );
    expect(
      wrapper.get('[data-testid="request-trace-ops-repository"]').text(),
    ).toContain("admin.requestTrace.ops.yes");
    expect(
      wrapper.get('[data-testid="request-trace-ops-stopped"]').text(),
    ).toContain("admin.requestTrace.ops.no");
    expect(
      wrapper.get('[data-testid="request-trace-ops-export"]').text(),
    ).toContain("admin.requestTrace.ops.export.cleanedFiles");
    expect(
      wrapper.get('[data-testid="request-trace-ops-export"]').text(),
    ).toContain("4");
    expect(
      wrapper.get('[data-testid="request-trace-ops-cleanup-runs"]').text(),
    ).toContain("6");
    expect(
      wrapper.get('[data-testid="request-trace-ops-backlog-value"]').text(),
    ).toBe("40");
    expect(
      wrapper
        .get('[data-testid="request-trace-ops-backlog"]')
        .attributes("data-state"),
    ).toBe("measured");
    expect(
      wrapper.get('[data-testid="request-trace-ops-backlog-state"]').text(),
    ).toBe("admin.requestTrace.ops.cleanup.backlogState.measured");
  });

  it("states plainly that the endpoint never reports whether capture is enabled", async () => {
    const wrapper = await mountLoaded();

    const note = wrapper
      .get('[data-testid="request-trace-ops-gate-note"]')
      .text();
    expect(note).toBe("admin.requestTrace.ops.unknownGateNote");
    // No counter, state or label here may be presented as an enablement verdict.
    expect(wrapper.text()).not.toMatch(/enable|enabled|active/i);
  });

  it.each([...requestTraceStorageProbeStates])(
    "renders the %s probe state as its own label",
    async (probe) => {
      const wrapper = await mountLoaded(status({ storage_probe: probe }));
      const rendered = wrapper
        .get('[data-testid="request-trace-ops-probe"]')
        .text();
      expect(rendered).toBe(`admin.requestTrace.ops.storageProbe.${probe}`);
      expect(rendered).not.toBe(probe);
    },
  );

  it.each([...requestTraceStorageStates])(
    "renders the %s storage state as its own label",
    async (storage) => {
      const wrapper = await mountLoaded(
        status({ capture: capture({ storage }) }),
      );
      const rendered = wrapper
        .get('[data-testid="request-trace-ops-storage"]')
        .text();
      expect(rendered).toBe(
        `admin.requestTrace.ops.capture.storage.${storage}`,
      );
      expect(rendered).not.toBe(storage);
    },
  );

  it('does not turn "nothing accepted yet" into a health or enablement claim', async () => {
    const wrapper = await mountLoaded(
      status({
        capture: capture({ storage: "no_traffic", accepted: 0, stored: 0 }),
      }),
    );

    expect(
      wrapper.get('[data-testid="request-trace-ops-storage"]').text(),
    ).toBe("admin.requestTrace.ops.capture.storage.no_traffic");
    expect(
      wrapper.get('[data-testid="request-trace-ops-storage"]').text(),
    ).not.toBe("admin.requestTrace.ops.capture.storage.ok");
  });

  it("reads a capped backlog as a lower bound and not as an exact total", async () => {
    const wrapper = await mountLoaded(
      status({
        cleanup: cleanup({
          unlinked_backlog: 1000,
          backlog_limit: 1000,
          backlog_state: "at_least",
        }),
      }),
    );

    expect(
      wrapper
        .get('[data-testid="request-trace-ops-backlog"]')
        .attributes("data-state"),
    ).toBe("at_least");
    expect(
      wrapper.get('[data-testid="request-trace-ops-backlog-value"]').text(),
    ).toBe("1000");
    expect(
      wrapper.get('[data-testid="request-trace-ops-backlog-state"]').text(),
    ).toBe("admin.requestTrace.ops.cleanup.backlogState.at_least");
    expect(
      wrapper.get('[data-testid="request-trace-ops-backlog-note"]').text(),
    ).toBe("admin.requestTrace.ops.cleanup.atLeastNote");
  });

  it("shows an unmeasured backlog as unknown rather than as a zero", async () => {
    const wrapper = await mountLoaded(
      status({
        cleanup: cleanup({ unlinked_backlog: 0, backlog_state: "unavailable" }),
      }),
    );

    expect(
      wrapper
        .get('[data-testid="request-trace-ops-backlog"]')
        .attributes("data-state"),
    ).toBe("unavailable");
    expect(
      wrapper.get('[data-testid="request-trace-ops-backlog-value"]').text(),
    ).toBe("—");
    expect(
      wrapper.get('[data-testid="request-trace-ops-backlog"]').text(),
    ).not.toContain("0");
    expect(
      wrapper.get('[data-testid="request-trace-ops-backlog-state"]').text(),
    ).toBe("admin.requestTrace.ops.cleanup.backlogState.unavailable");
    expect(
      wrapper.get('[data-testid="request-trace-ops-backlog-note"]').text(),
    ).toBe("admin.requestTrace.ops.cleanup.unavailableNote");
  });

  it("labels every backlog state the backend can send", async () => {
    // The parser refuses an unknown state, so the panel only has to label the set.
    for (const state of requestTraceBacklogStates) {
      const wrapper = await mountLoaded(
        status({ cleanup: cleanup({ backlog_state: state }) }),
      );
      expect(
        wrapper.get('[data-testid="request-trace-ops-backlog-state"]').text(),
      ).toBe(`admin.requestTrace.ops.cleanup.backlogState.${state}`);
      wrapper.unmount();
    }
  });

  it("never shows the raw wire value when a value outside the closed set reaches the panel", async () => {
    const wrapper = await mountLoaded(
      status({
        storage_probe: "definitely_broken",
        capture: capture({ storage: "invented" }),
      }),
    );

    expect(wrapper.get('[data-testid="request-trace-ops-probe"]').text()).toBe(
      "admin.requestTrace.ops.storageProbe.unknown",
    );
    expect(
      wrapper.get('[data-testid="request-trace-ops-storage"]').text(),
    ).toBe("admin.requestTrace.ops.capture.storage.unknown");
    expect(wrapper.text()).not.toContain("definitely_broken");
    expect(wrapper.text()).not.toContain("invented");
  });

  it("reports an unreadable status as unknown instead of rendering zeros", async () => {
    mocks.getTraceOpsStatus.mockRejectedValue({
      status: 503,
      reason: "REQUEST_TRACE_STATUS_UNAVAILABLE",
    });
    const wrapper = mountPanel();
    await flushPromises();

    expect(
      wrapper.get('[data-testid="request-trace-ops-failed"]').exists(),
    ).toBe(true);
    expect(
      wrapper.find('[data-testid="request-trace-ops-accepted"]').exists(),
    ).toBe(false);
    expect(
      wrapper.find('[data-testid="request-trace-ops-backlog"]').exists(),
    ).toBe(false);
    // The gate note survives a failed read: it is a property of the endpoint.
    expect(
      wrapper.get('[data-testid="request-trace-ops-gate-note"]').exists(),
    ).toBe(true);
  });

  it("renders no database message, body, header value, raw query or filename", async () => {
    const wrapper = await mountLoaded(
      status({
        error: 'pq: password authentication failed for user "sub2api"',
        message: 'relation "request_traces" does not exist',
        statement: "SELECT count(*) FROM request_traces",
        body: "BODY_CANARY",
        request_headers: { authorization: "Bearer CANARY" },
        filename: "sub2api-request-trace-export-abc.jsonl",
        download_url: "https://public.example/leak",
      }),
    );

    for (const canary of [
      "password authentication",
      "does not exist",
      "SELECT count(*)",
      "BODY_CANARY",
      "Bearer CANARY",
      "request-trace-export",
      "public.example",
    ]) {
      expect(
        wrapper.text(),
        `the panel must not render ${canary}`,
      ).not.toContain(canary);
    }
    // The allowlisted counters are still there, so the check above is not vacuous.
    expect(
      wrapper.get('[data-testid="request-trace-ops-accepted"]').text(),
    ).toContain("41");
  });

  it("refreshes only on demand and never starts a poller", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    try {
      const wrapper = await mountLoaded();
      expect(mocks.getTraceOpsStatus).toHaveBeenCalledTimes(1);

      await vi.advanceTimersByTimeAsync(60_000);
      expect(mocks.getTraceOpsStatus).toHaveBeenCalledTimes(1);
      expect(vi.getTimerCount()).toBe(0);

      await wrapper
        .get('[data-testid="request-trace-ops-refresh"]')
        .trigger("click");
      await flushPromises();
      expect(mocks.getTraceOpsStatus).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("aborts an in-flight read on unmount and adopts no stale answer", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    try {
      let resolveStatus!: (value: unknown) => void;
      mocks.getTraceOpsStatus.mockImplementationOnce(
        (options?: { signal?: AbortSignal }) =>
          new Promise((resolve) => {
            options?.signal?.addEventListener("abort", () => resolve(status()));
            resolveStatus = resolve;
          }),
      );
      const wrapper = mountPanel();
      const options = mocks.getTraceOpsStatus.mock.calls[0]?.[0] as
        { signal?: AbortSignal } | undefined;
      expect(options?.signal).toBeInstanceOf(AbortSignal);

      wrapper.unmount();
      expect(options?.signal?.aborted).toBe(true);

      // The read answers after the panel is gone; it must not register a timer or
      // render into a detached component.
      resolveStatus(status());
      await flushPromises();
      expect(vi.getTimerCount()).toBe(0);
      expect(
        wrapper.find('[data-testid="request-trace-ops-accepted"]').exists(),
      ).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps an older answer from overwriting a newer refresh", async () => {
    let resolveFirst!: (value: unknown) => void;
    mocks.getTraceOpsStatus
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveFirst = resolve;
          }),
      )
      .mockResolvedValueOnce(status({ capture: capture({ accepted: 7 }) }));

    const wrapper = mountPanel();
    await wrapper
      .get('[data-testid="request-trace-ops-refresh"]')
      .trigger("click");
    await flushPromises();
    resolveFirst(status({ capture: capture({ accepted: 999 }) }));
    await flushPromises();

    expect(
      wrapper.get('[data-testid="request-trace-ops-accepted"]').text(),
    ).toContain("7");
    expect(
      wrapper.get('[data-testid="request-trace-ops-accepted"]').text(),
    ).not.toContain("999");
  });
});
