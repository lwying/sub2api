/**
 * The admin list, with the export actions sitting beside the query they export.
 *
 * The rules these cases hold to: an export carries the last query that actually
 * ran — never a form edit that was not searched; "selected" and "all" are two
 * different scopes and never blur into one; and the checked set is refused when
 * it is too large rather than trimmed to fit.
 */
import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  listTraces: vi.fn(),
  getTrace: vi.fn(),
  createTraceExport: vi.fn(),
  getTraceExportRisk: vi.fn(),
  getTraceModelCandidates: vi.fn(),
  previewTraceDelete: vi.fn(),
  deleteSelectedTraces: vi.fn(),
  deleteTracesByFilter: vi.fn(),
  getGroups: vi.fn(),
  route: { query: {} as Record<string, string> },
  replace: vi.fn(() => Promise.resolve()),
}));

vi.mock("../api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api")>();
  return {
    ...actual,
    listTraces: mocks.listTraces,
    getTrace: mocks.getTrace,
    createTraceExport: mocks.createTraceExport,
    getTraceExportRisk: mocks.getTraceExportRisk,
    getOperatorSettings: vi.fn().mockResolvedValue(null),
    listTraceExports: vi
      .fn()
      .mockResolvedValue({ items: [], next_cursor: null }),
    getTraceModelCandidates: mocks.getTraceModelCandidates,
    previewTraceDelete: mocks.previewTraceDelete,
    deleteSelectedTraces: mocks.deleteSelectedTraces,
    deleteTracesByFilter: mocks.deleteTracesByFilter,
  };
});
vi.mock("@/api/admin/groups", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/admin/groups")>();
  return { ...actual, getAllIncludingInactive: mocks.getGroups };
});
// The list reads lookup prefills from the route; an empty query keeps these cases
// on the plain, unfiltered load path they assert.
vi.mock("vue-router", () => ({
  useRoute: () => mocks.route,
  useRouter: () => ({ replace: mocks.replace }),
}));
vi.mock("vue-i18n", async (importOriginal) => {
  const actual = await importOriginal<typeof import("vue-i18n")>();
  return {
    ...actual,
    // The key is the rendered message, so a raw wire value can never pass for a
    // label; parameters are appended so counts stay assertable.
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params
          ? `${key}|${Object.entries(params)
              .map(([name, value]) => `${name}=${String(value)}`)
              .join(",")}`
          : key,
    }),
  };
});

import RequestTraceView from "../RequestTraceView.vue";
import { requestTraceCaptureStates } from "../types";

const TRACE_ID = "a".repeat(32);
const TASK_ID = "b".repeat(32);

const trace = (overrides: Record<string, unknown> = {}) => ({
  trace_id: TRACE_ID,
  route_family: "messages",
  inbound_endpoint: "/v1/messages",
  created_at: "2026-09-28T00:00:00Z",
  completed_at: "2026-09-28T00:00:01Z",
  client_status: 401,
  capture_state: "partial",
  usage_log_id: null,
  cleanup_after: "2026-10-28T00:00:00Z",
  ...overrides,
});
const page = (items: unknown[]) => ({
  items,
  total: items.length,
  page: 1,
  page_size: 20,
});
const task = (overrides: Record<string, unknown> = {}) => ({
  id: TASK_ID,
  status: "pending",
  filter: {},
  rows_exported: 0,
  rows_skipped: 0,
  bytes_exported: 0,
  created_at: "2026-09-28T00:00:00Z",
  completed_at: null,
  download_until: null,
  downloadable: false,
  shard_count: 0,
  truncated: false,
  ...overrides,
});
const DetailStub = {
  props: ["show", "traceId"],
  emits: ["update:show"],
  template:
    '<div data-testid="trace-detail-stub" :data-show="String(show)" :data-id="traceId || \'\'" />',
};
const ExportDrawerStub = {
  props: ["show", "taskId"],
  emits: ["update:show"],
  template:
    '<div data-testid="request-trace-export-drawer-stub" :data-show="String(show)" :data-id="taskId || \'\'" />',
};
const PaginationStub = {
  emits: ["update:page", "update:page-size"],
  template:
    '<button data-testid="trace-pagination" @click="$emit(\'update:page\', 2)" />',
};
function mountView() {
  return mount(RequestTraceView, {
    global: {
      stubs: {
        AppLayout: { template: "<div><slot /></div>" },
        RequestTraceSettingsDialog: true,
        RequestTraceOpsStatusPanel: true,
        Pagination: PaginationStub,
        ConfirmDialog: {
          props: ["show", "message"],
          emits: ["confirm", "cancel"],
          template:
            '<div v-if="show" data-testid="request-trace-cleanup-confirm"><p>{{ message }}</p><slot /><button data-testid="confirm-delete" @click="$emit(\'confirm\')" /></div>',
        },
        RequestTraceOperatorSettings: {
          props: ["status"],
          template: '<div data-testid="operator-stub" />',
        },
        Select: {
          props: ["modelValue", "options"],
          emits: ["update:modelValue"],
          template:
            '<select :value="modelValue" @change="$emit(\'update:modelValue\', $event.target.value)"><option value=""></option><option v-for="option in options" :key="option.value" :value="option.value">{{ option.label }}</option></select>',
        },
        RequestTraceDetailDrawer: DetailStub,
        RequestTraceExportDrawer: ExportDrawerStub,
        RouterLink: { template: "<a><slot /></a>" },
      },
    },
  });
}

async function mountLoaded() {
  const wrapper = mountView();
  await flushPromises();
  return wrapper;
}

/** The filter set the last export POST carried. */
function exportedParams(): Record<string, unknown> {
  const [filter] = mocks.createTraceExport.mock.calls[0] as [
    Record<string, unknown>,
  ];
  return filter;
}

describe("admin Request Trace list", () => {
  beforeEach(() => {
    mocks.listTraces.mockReset();
    mocks.getTrace.mockReset();
    mocks.createTraceExport.mockReset();
    mocks.getTraceExportRisk.mockReset();
    mocks.getTraceModelCandidates
      .mockReset()
      .mockResolvedValue(["claude-sonnet-4-5", "gateway-alias"]);
    mocks.getGroups
      .mockReset()
      .mockResolvedValue([{ id: 7, name: "开发组", platform: "anthropic" }]);
    mocks.previewTraceDelete.mockReset();
    mocks.deleteSelectedTraces
      .mockReset()
      .mockResolvedValue({ deleted_count: 1, completed: true });
    mocks.deleteTracesByFilter
      .mockReset()
      .mockResolvedValue({ deleted_count: 1, completed: true });
    mocks.replace.mockClear();
    mocks.route = { query: {} };
    mocks.listTraces.mockResolvedValue(page([trace()]));
    mocks.getTraceExportRisk.mockResolvedValue({
      acknowledged: true,
      version: "v1",
      phrase_en: "EN statement",
      phrase_zh: "ZH 声明",
    });
    mocks.createTraceExport.mockResolvedValue(task());
  });

  it("shows one logical request without rendering unexpected body or credential fields", async () => {
    mocks.listTraces.mockResolvedValue(
      page([
        trace({ payload_text: "BODY_CANARY", authorization: "Bearer CANARY" }),
      ]),
    );
    const wrapper = await mountLoaded();
    expect(mocks.listTraces).toHaveBeenCalledWith(
      { page: 1, page_size: 20 },
      expect.anything(),
    );
    expect(wrapper.findAll('[data-testid="request-trace-row"]')).toHaveLength(
      1,
    );
    expect(wrapper.get('[data-testid="request-trace-row"]').text()).toContain(
      "/v1/messages",
    );
    // The closed-set capture state renders through its label, never raw.
    expect(
      wrapper.get('[data-testid="request-trace-capture-state"]').text(),
    ).toBe("admin.requestTrace.list.captureStateLabel.partial");
    expect(wrapper.text()).not.toContain("BODY_CANARY");
    expect(wrapper.text()).not.toContain("Bearer CANARY");
    expect(mocks.getTrace).not.toHaveBeenCalled();
    expect(
      wrapper.get('[data-testid="trace-detail-stub"]').attributes("data-show"),
    ).toBe("false");
  });

  it("marks the 30-day unlinked date as planned cleanup, not a strict read expiry", async () => {
    const wrapper = await mountLoaded();
    expect(
      wrapper.get('[data-testid="request-trace-cleanup-rule"]').text(),
    ).toContain("plannedCleanup");
    expect(
      wrapper.get('[data-testid="request-trace-cleanup-rule"]').text(),
    ).not.toContain("expires");
  });

  it("uses only metadata filters and resets page for a changed trace id", async () => {
    const wrapper = await mountLoaded();
    await wrapper
      .get('[data-testid="request-trace-id-filter"]')
      .setValue(TRACE_ID);
    await wrapper.get('[data-testid="request-trace-search"]').trigger("click");
    await flushPromises();
    expect(mocks.listTraces).toHaveBeenLastCalledWith(
      { page: 1, page_size: 20, trace_id: TRACE_ID },
      expect.anything(),
    );
  });

  it("rejects an invalid Trace ID instead of silently returning unfiltered rows", async () => {
    const wrapper = await mountLoaded();
    await wrapper
      .get('[data-testid="request-trace-id-filter"]')
      .setValue("not-a-trace-id");
    await wrapper.get('[data-testid="request-trace-search"]').trigger("click");
    expect(
      wrapper.get('[data-testid="request-trace-filter-error"]').exists(),
    ).toBe(true);
    expect(mocks.listTraces).toHaveBeenCalledTimes(1);
  });

  it("opens the selected trace on row click or keyboard, but not on checkbox", async () => {
    const wrapper = await mountLoaded();
    await wrapper
      .get(`[data-testid="request-trace-select-${TRACE_ID}"]`)
      .trigger("click");
    expect(
      wrapper.get('[data-testid="trace-detail-stub"]').attributes("data-show"),
    ).toBe("false");
    await wrapper.get('[data-testid="request-trace-row"]').trigger("click");
    expect(
      wrapper.get('[data-testid="trace-detail-stub"]').attributes("data-id"),
    ).toBe(TRACE_ID);
    expect(mocks.getTrace).not.toHaveBeenCalled();
  });

  it("shows current-query statistics without changing scope when the draft is edited", async () => {
    mocks.listTraces.mockResolvedValue({
      ...page([trace()]),
      total: 2,
      stats: {
        matched_total: 2,
        status: { "2xx": 1, "3xx": 0, "4xx": 1, "5xx": 0, other: 0 },
        capture: { stored: 1, partial: 1, not_observed: 0, write_failed: 0 },
        usage: { linked: 0, unlinked: 2 },
      },
    });
    const wrapper = await mountLoaded();
    expect(
      wrapper.get('[data-testid="request-trace-query-stats"]').text(),
    ).toContain("2");
    await wrapper
      .get('[data-testid="request-trace-keyword"]')
      .setValue("claude");
    expect(mocks.listTraces).toHaveBeenCalledTimes(1);
    expect(
      wrapper.get('[data-testid="request-trace-query-stats"]').text(),
    ).toContain("2");
    await wrapper.get('[data-testid="request-trace-search"]').trigger("click");
    await flushPromises();
    expect(mocks.listTraces).toHaveBeenLastCalledWith(
      { page: 1, page_size: 20, q: "claude" },
      expect.anything(),
    );
  });

  it("renders the export task drawer closed and starts no export on load", async () => {
    const wrapper = await mountLoaded();

    expect(
      wrapper
        .get('[data-testid="request-trace-export-drawer-stub"]')
        .attributes("data-show"),
    ).toBe("false");
    expect(mocks.createTraceExport).not.toHaveBeenCalled();
    expect(mocks.listTraces).toHaveBeenCalledTimes(1);
  });

  it("keeps records and capture configuration in separate tabs", async () => {
    const wrapper = await mountLoaded();
    expect(
      wrapper.get('[data-testid="request-trace-records-panel"]').isVisible(),
    ).toBe(true);
    await wrapper
      .get('[data-testid="request-trace-tab-config"]')
      .trigger("click");
    expect(
      wrapper
        .get('[data-testid="request-trace-records-panel"]')
        .attributes("style"),
    ).toContain("display: none");
    expect(wrapper.get('[data-testid="operator-stub"]').isVisible()).toBe(true);
    await wrapper
      .get('[data-testid="request-trace-tab-records"]')
      .trigger("click");
    expect(
      wrapper.get('[data-testid="request-trace-records-panel"]').isVisible(),
    ).toBe(true);
    expect(mocks.listTraces).toHaveBeenCalledTimes(1);
  });

  it("filters by one concrete request-time group, model and platform value", async () => {
    const wrapper = await mountLoaded();
    await wrapper
      .get('[data-testid="request-trace-group-filter-mode"]')
      .setValue("id");
    await wrapper
      .get('[data-testid="request-trace-group-filter"]')
      .setValue("7");
    await wrapper
      .get('[data-testid="request-trace-model-filter-mode"]')
      .setValue("value");
    await wrapper
      .get('[data-testid="request-trace-model-filter"]')
      .setValue("claude-sonnet-4-5");
    await wrapper
      .get('[data-testid="request-trace-platform-filter-mode"]')
      .setValue("value");
    await wrapper
      .get('[data-testid="request-trace-platform-filter"]')
      .setValue("antigravity");
    await wrapper.get('[data-testid="request-trace-search"]').trigger("click");
    await flushPromises();
    expect(mocks.listTraces).toHaveBeenLastCalledWith(
      {
        page: 1,
        page_size: 20,
        group_id: 7,
        requested_model: "claude-sonnet-4-5",
        platform: "antigravity",
      },
      expect.anything(),
    );
  });

  it("cleans only confirmed selected trace IDs", async () => {
    const wrapper = await mountLoaded();
    await wrapper
      .get(`[data-testid="request-trace-select-${TRACE_ID}"]`)
      .setValue(true);
    await wrapper
      .get('[data-testid="request-trace-delete-selected"]')
      .trigger("click");
    expect(mocks.deleteSelectedTraces).not.toHaveBeenCalled();
    await wrapper
      .get(
        '[data-testid="request-trace-cleanup-confirm"] button[data-testid="confirm-delete"]',
      )
      .trigger("click");
    await flushPromises();
    expect(mocks.deleteSelectedTraces).toHaveBeenCalledWith([TRACE_ID]);
  });

  it("previews cleanup against executed filters rather than unsaved edits", async () => {
    mocks.previewTraceDelete.mockResolvedValue({
      matched_count: 1,
      snapshot_max_id: 9,
      filter_hash: "f".repeat(64),
      confirmation_token: "token",
      expires_at: new Date(Date.now() + 300000).toISOString(),
    });
    const wrapper = await mountLoaded();
    expect(
      wrapper
        .get('[data-testid="request-trace-delete-filtered"]')
        .attributes("disabled"),
    ).toBeDefined();
    await wrapper
      .get('[data-testid="request-trace-keyword"]')
      .setValue("original");
    await wrapper.get('[data-testid="request-trace-search"]').trigger("click");
    await flushPromises();
    await wrapper
      .get('[data-testid="request-trace-keyword"]')
      .setValue("unsaved");
    await wrapper
      .get('[data-testid="request-trace-delete-filtered"]')
      .trigger("click");
    await flushPromises();
    expect(mocks.previewTraceDelete).toHaveBeenCalledWith({ q: "original" });
    expect(mocks.deleteTracesByFilter).not.toHaveBeenCalled();
  });

  it("queries a fact that was never observed instead of any concrete value", async () => {
    const wrapper = await mountLoaded();
    await wrapper
      .get('[data-testid="request-trace-group-filter-mode"]')
      .setValue("unknown");
    await wrapper
      .get('[data-testid="request-trace-model-filter-mode"]')
      .setValue("unknown");
    await wrapper
      .get('[data-testid="request-trace-platform-filter-mode"]')
      .setValue("unknown");
    await wrapper.get('[data-testid="request-trace-search"]').trigger("click");
    await flushPromises();
    // "Unknown" is its own condition, never a value and never both at once.
    expect(mocks.listTraces).toHaveBeenLastCalledWith(
      {
        page: 1,
        page_size: 20,
        group_unknown: true,
        model_unknown: true,
        platform_unknown: true,
      },
      expect.anything(),
    );
  });

  it("refuses an incomplete fact filter instead of quietly querying something else", async () => {
    const wrapper = await mountLoaded();
    await wrapper
      .get('[data-testid="request-trace-group-filter-mode"]')
      .setValue("id");
    await wrapper
      .get('[data-testid="request-trace-group-filter"]')
      .setValue("0");
    await wrapper.get('[data-testid="request-trace-search"]').trigger("click");
    expect(
      wrapper.get('[data-testid="request-trace-filter-error"]').exists(),
    ).toBe(true);
    expect(mocks.listTraces).toHaveBeenCalledTimes(1);

    await wrapper
      .get('[data-testid="request-trace-group-filter-mode"]')
      .setValue("");
    await wrapper
      .get('[data-testid="request-trace-model-filter-mode"]')
      .setValue("value");
    await wrapper.get('[data-testid="request-trace-search"]').trigger("click");
    expect(
      wrapper.get('[data-testid="request-trace-filter-error"]').exists(),
    ).toBe(true);
    expect(mocks.listTraces).toHaveBeenCalledTimes(1);
  });

  it("shows an unobserved group and model as unknown rather than leaving the cell empty", async () => {
    mocks.listTraces.mockResolvedValue(
      page([trace({ group_id: 7, requested_model: "claude-sonnet-4-5" })]),
    );
    const observed = await mountLoaded();
    expect(observed.get('[data-testid="request-trace-row-group"]').text()).toBe(
      "#7",
    );
    expect(observed.get('[data-testid="request-trace-row-model"]').text()).toBe(
      "claude-sonnet-4-5",
    );

    mocks.listTraces.mockResolvedValue(
      page([trace({ group_id: null, requested_model: null })]),
    );
    const unobserved = await mountLoaded();
    expect(
      unobserved.get('[data-testid="request-trace-row-group"]').text(),
    ).toBe("admin.requestTrace.list.unknownValue");
    expect(
      unobserved.get('[data-testid="request-trace-row-model"]').text(),
    ).toBe("admin.requestTrace.list.unknownValue");
  });

  it.each([...requestTraceCaptureStates])(
    "renders the localized capture state label for %s instead of the raw token",
    async (state) => {
      mocks.listTraces.mockResolvedValue(
        page([trace({ capture_state: state })]),
      );
      const wrapper = await mountLoaded();
      const rendered = wrapper
        .get('[data-testid="request-trace-capture-state"]')
        .text();
      expect(rendered).toBe(
        `admin.requestTrace.list.captureStateLabel.${state}`,
      );
      expect(rendered).not.toBe(state);
    },
  );
});

describe("admin Request Trace export actions", () => {
  beforeEach(() => {
    mocks.listTraces.mockReset();
    mocks.createTraceExport.mockReset();
    mocks.getTraceExportRisk.mockReset();
    mocks.getTraceModelCandidates
      .mockReset()
      .mockResolvedValue(["claude-sonnet-4-5", "gateway-alias"]);
    mocks.getGroups
      .mockReset()
      .mockResolvedValue([{ id: 7, name: "开发组", platform: "anthropic" }]);
    mocks.previewTraceDelete.mockReset();
    mocks.deleteSelectedTraces
      .mockReset()
      .mockResolvedValue({ deleted_count: 1, completed: true });
    mocks.deleteTracesByFilter
      .mockReset()
      .mockResolvedValue({ deleted_count: 1, completed: true });
    mocks.replace.mockClear();
    mocks.route = { query: {} };
    mocks.listTraces.mockResolvedValue(page([trace()]));
    mocks.getTraceExportRisk.mockResolvedValue({
      acknowledged: true,
      version: "v1",
      phrase_en: "EN statement",
      phrase_zh: "ZH 声明",
    });
    mocks.createTraceExport.mockResolvedValue(task());
  });

  async function checkRow(
    wrapper: Awaited<ReturnType<typeof mountView>>,
    traceID = TRACE_ID,
  ) {
    await wrapper
      .get(`[data-testid="request-trace-select-${traceID}"]`)
      .setValue(true);
  }

  /**
   * Checking a whole page at once must not await per row: every check re-renders
   * the table, and looking each row up by id rescans the whole document, so the
   * naive loop is quadratic in the row count. This collects the boxes once and
   * lets Vue flush a single time — the bound is what is under test here, not the
   * speed of DOM updates.
   */
  function checkManyRows(wrapper: Awaited<ReturnType<typeof mountView>>) {
    for (const box of wrapper.findAll(
      '[data-testid^="request-trace-select-"]',
    )) {
      const input = box.element as HTMLInputElement;
      input.checked = true;
      input.dispatchEvent(new Event("change"));
    }
  }

  it("shows the executed filter set as the export scope", async () => {
    const wrapper = await mountLoaded();

    expect(
      wrapper.get('[data-testid="request-trace-export-scope"]').text(),
    ).toContain("admin.requestTrace.export.scope.none");

    await wrapper
      .get('[data-testid="request-trace-group-filter-mode"]')
      .setValue("id");
    await wrapper
      .get('[data-testid="request-trace-group-filter"]')
      .setValue("7");
    await wrapper.get('[data-testid="request-trace-search"]').trigger("click");
    await flushPromises();

    expect(
      wrapper.get('[data-testid="request-trace-export-scope"]').text(),
    ).toContain("admin.requestTrace.list.group");
    expect(
      wrapper.get('[data-testid="request-trace-export-scope"]').text(),
    ).toContain("#7");
  });

  it("exports the last executed query, never a form edit that was not searched", async () => {
    const wrapper = await mountLoaded();

    await wrapper
      .get('[data-testid="request-trace-account-filter"]')
      .setValue("91");
    await wrapper.get('[data-testid="request-trace-search"]').trigger("click");
    await flushPromises();

    // The edit after the query is a draft: it has not been executed, so it is not
    // part of the scope the export carries.
    await wrapper
      .get('[data-testid="request-trace-account-filter"]')
      .setValue("92");
    expect(
      wrapper.get('[data-testid="request-trace-export-draft-note"]').exists(),
    ).toBe(true);

    await wrapper
      .get('[data-testid="request-trace-export-all"]')
      .trigger("click");
    await flushPromises();

    expect(mocks.createTraceExport).toHaveBeenCalledTimes(1);
    expect(exportedParams()).toEqual({ account_id: 91 });
  });

  it('exports every result of the query and ignores the checked rows for "all"', async () => {
    const wrapper = await mountLoaded();
    await checkRow(wrapper);

    await wrapper
      .get('[data-testid="request-trace-export-all"]')
      .trigger("click");
    await flushPromises();

    expect(exportedParams()).toEqual({});
    expect(exportedParams()).not.toHaveProperty("trace_ids");
  });

  it('exports exactly the checked rows for "selected", and nothing else', async () => {
    const second = "c".repeat(32);
    mocks.listTraces.mockResolvedValue(
      page([trace(), trace({ trace_id: second })]),
    );
    const wrapper = await mountLoaded();
    await checkRow(wrapper);
    await checkRow(wrapper, second);

    expect(
      wrapper
        .get('[data-testid="request-trace-export-selection-count"]')
        .text(),
    ).toContain("count=2");

    await wrapper
      .get('[data-testid="request-trace-export-selected"]')
      .trigger("click");
    await flushPromises();

    expect(exportedParams()).toEqual({ trace_ids: [TRACE_ID, second] });
  });

  it("keeps a checked row across pages and clears it after a new query succeeds", async () => {
    const second = "c".repeat(32);
    mocks.listTraces.mockResolvedValue(page([trace()]));
    const wrapper = await mountLoaded();
    await checkRow(wrapper);

    mocks.listTraces.mockResolvedValue(page([trace({ trace_id: second })]));
    await wrapper.get('[data-testid="trace-pagination"]').trigger("click");
    await flushPromises();

    expect(
      wrapper
        .get('[data-testid="request-trace-export-selection-count"]')
        .text(),
    ).toContain("count=1");
    await checkRow(wrapper, second);
    expect(
      wrapper
        .get('[data-testid="request-trace-export-selection-count"]')
        .text(),
    ).toContain("count=2");

    await wrapper.get('[data-testid="request-trace-search"]').trigger("click");
    await flushPromises();
    expect(
      wrapper
        .get('[data-testid="request-trace-export-selection-count"]')
        .text(),
    ).toContain("count=0");
  });

  it("keeps the checks when a query fails instead of losing what was selected", async () => {
    const wrapper = await mountLoaded();
    await checkRow(wrapper);

    mocks.listTraces.mockRejectedValueOnce(new Error("503"));
    await wrapper.get('[data-testid="request-trace-search"]').trigger("click");
    await flushPromises();

    expect(wrapper.get('[data-testid="request-trace-error"]').exists()).toBe(
      true,
    );
    expect(
      wrapper
        .get('[data-testid="request-trace-export-selection-count"]')
        .text(),
    ).toContain("count=1");
    // A query that never ran is not a scope either: the export still carries the
    // last one that did.
    await wrapper
      .get('[data-testid="request-trace-export-all"]')
      .trigger("click");
    await flushPromises();
    expect(exportedParams()).toEqual({});
  });

  it("refuses a checked set above the sendable bound instead of exporting a subset", async () => {
    // 上限是 2000，因此至少要勾满 2001 行才会触发。
    // 注意：新查询会清空勾选（这是产品行为），所以只能在同一页里凑够总数。
    const many = Array.from({ length: 2001 }, (_, index) =>
      index.toString(16).padStart(32, "0"),
    );
    mocks.listTraces.mockResolvedValue(
      page(many.map((trace_id) => trace({ trace_id }))),
    );
    const wrapper = await mountLoaded();
    checkManyRows(wrapper);
    await flushPromises();

    expect(
      wrapper
        .get('[data-testid="request-trace-export-selection-over-bound"]')
        .exists(),
    ).toBe(true);
    expect(
      wrapper
        .get('[data-testid="request-trace-export-selection-over-bound"]')
        .text(),
    ).toContain("max=2000");
    expect(
      (
        wrapper.get('[data-testid="request-trace-export-selected"]')
          .element as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    await wrapper
      .get('[data-testid="request-trace-export-selected"]')
      .trigger("click");
    expect(mocks.createTraceExport).not.toHaveBeenCalled();

    // Clearing the checks is the offered way out, and it does not touch the query.
    await wrapper
      .get('[data-testid="request-trace-export-clear-selection"]')
      .trigger("click");
    expect(
      wrapper
        .get('[data-testid="request-trace-export-selection-count"]')
        .text(),
    ).toContain("count=0");

    // 这一条渲染了 2001 行：显式卸载，别把整棵大树留给后面的用例。
    wrapper.unmount();
  });

  it("opens the task drawer on the created task and keeps its handle in the URL", async () => {
    const wrapper = await mountLoaded();
    await wrapper
      .get('[data-testid="request-trace-export-all"]')
      .trigger("click");
    await flushPromises();

    expect(
      wrapper
        .get('[data-testid="request-trace-export-drawer-stub"]')
        .attributes("data-id"),
    ).toBe(TASK_ID);
    expect(
      wrapper
        .get('[data-testid="request-trace-export-drawer-stub"]')
        .attributes("data-show"),
    ).toBe("true");
    expect(mocks.replace).toHaveBeenCalledWith(
      expect.objectContaining({
        query: expect.objectContaining({ export: TASK_ID }),
      }),
    );
  });

  it("drops the task handle from the URL when the task area is closed", async () => {
    mocks.route = { query: { export: TASK_ID } };
    const wrapper = await mountLoaded();
    mocks.replace.mockClear();

    wrapper.findComponent(ExportDrawerStub).vm.$emit("update:show", false);
    await flushPromises();

    expect(
      wrapper
        .get('[data-testid="request-trace-export-drawer-stub"]')
        .attributes("data-show"),
    ).toBe("false");
    expect(mocks.replace).toHaveBeenCalledWith(
      expect.objectContaining({
        query: expect.objectContaining({ export: undefined }),
      }),
    );
  });

  it("restores the task handle from the URL after a refresh", async () => {
    mocks.route = { query: { export: TASK_ID } };
    const wrapper = await mountLoaded();

    const drawer = wrapper.get(
      '[data-testid="request-trace-export-drawer-stub"]',
    );
    expect(drawer.attributes("data-id")).toBe(TASK_ID);
    expect(drawer.attributes("data-show")).toBe("true");
    expect(mocks.createTraceExport).not.toHaveBeenCalled();
  });

  it("ignores a task handle in the URL that is not a task id", async () => {
    mocks.route = { query: { export: "../exports" } };
    const wrapper = await mountLoaded();

    expect(
      wrapper
        .get('[data-testid="request-trace-export-drawer-stub"]')
        .attributes("data-id"),
    ).toBe("");
    expect(
      wrapper
        .get('[data-testid="request-trace-export-drawer-stub"]')
        .attributes("data-show"),
    ).toBe("false");
  });

  it("renders a bounded refusal instead of the server message when creation is refused", async () => {
    const { TraceExportRefusedError } = await import("../api");
    mocks.createTraceExport.mockRejectedValue(
      new TraceExportRefusedError("risk_ack_required"),
    );
    const wrapper = await mountLoaded();
    await wrapper
      .get('[data-testid="request-trace-export-all"]')
      .trigger("click");
    await flushPromises();

    expect(
      wrapper.get('[data-testid="request-trace-export-refusal"]').text(),
    ).toBe("admin.requestTrace.export.refusal.risk_ack_required");
    expect(
      wrapper
        .get('[data-testid="request-trace-export-drawer-stub"]')
        .attributes("data-show"),
    ).toBe("false");
    // The refusal re-reads the risk state rather than trusting a stale verdict.
    expect(mocks.getTraceExportRisk).toHaveBeenCalledTimes(2);
  });

  it("does not start an export before the written risk statement is acknowledged", async () => {
    mocks.getTraceExportRisk.mockResolvedValue({
      acknowledged: false,
      version: "v1",
      phrase_en: "EN statement",
      phrase_zh: "ZH 声明",
    });
    const wrapper = await mountLoaded();

    expect(
      wrapper.get('[data-testid="request-trace-export-risk-note"]').exists(),
    ).toBe(true);
    expect(
      (
        wrapper.get('[data-testid="request-trace-export-all"]')
          .element as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    await wrapper
      .get('[data-testid="request-trace-export-all"]')
      .trigger("click");
    expect(mocks.createTraceExport).not.toHaveBeenCalled();
  });

  it("does not claim the statement is unacknowledged when its state cannot be read", async () => {
    mocks.getTraceExportRisk.mockRejectedValue(new Error("503"));
    const wrapper = await mountLoaded();

    expect(
      wrapper.find('[data-testid="request-trace-export-risk-note"]').exists(),
    ).toBe(false);
    expect(
      (
        wrapper.get('[data-testid="request-trace-export-all"]')
          .element as HTMLButtonElement
      ).disabled,
    ).toBe(false);
    await wrapper
      .get('[data-testid="request-trace-export-all"]')
      .trigger("click");
    await flushPromises();
    expect(mocks.createTraceExport).toHaveBeenCalledTimes(1);
  });
});
