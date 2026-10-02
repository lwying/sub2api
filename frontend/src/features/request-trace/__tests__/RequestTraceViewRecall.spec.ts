/**
 * ticket09: after leaving or refreshing the Trace page, the same admin login
 * session must find its export tasks again — and find **all** of them, even when
 * a session has more tasks than one page holds.
 *
 * The rules these cases hold to: the list is answered by the server per session
 * and never cached in the browser; a refused recall is a bounded outcome, not an
 * empty list; a failed next page never discards the tasks already on screen; and
 * reopening a task is just a handle in the URL, so the server still decides
 * whether this session may read it.
 */
import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  listTraces: vi.fn(),
  getTrace: vi.fn(),
  createTraceExport: vi.fn(),
  getTraceExportRisk: vi.fn(),
  listTraceExports: vi.fn(),
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
    listTraceExports: mocks.listTraceExports,
  };
});
vi.mock("vue-router", () => ({
  useRoute: () => mocks.route,
  useRouter: () => ({ replace: mocks.replace }),
}));
vi.mock("vue-i18n", async (importOriginal) => {
  const actual = await importOriginal<typeof import("vue-i18n")>();
  return {
    ...actual,
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

const FIRST_TASK = "b".repeat(32);
const SECOND_TASK = "c".repeat(32);

const task = (id: string, overrides: Record<string, unknown> = {}) => ({
  id,
  status: "completed",
  filter: { route_family: "messages" },
  rows_exported: 3,
  rows_skipped: 0,
  bytes_exported: 512,
  created_at: "2026-09-28T00:00:00Z",
  completed_at: "2026-09-28T00:00:01Z",
  download_until: "2026-10-05T00:00:01Z",
  downloadable: true,
  shard_count: 1,
  truncated: false,
  ...overrides,
});

/** One server page: a bounded set of tasks plus the token for the next one. */
const page = (items: unknown[], nextCursor: string | null = null) => ({
  items,
  nextCursor,
});

/** `count` distinct task ids, so a page can be told apart from its neighbours. */
const taskIDs = (from: number, count: number) =>
  Array.from({ length: count }, (_, index) =>
    (from + index).toString(16).padStart(32, "0"),
  );

beforeEach(() => {
  Object.values(mocks).forEach((mock) => {
    if (typeof mock === "function" && "mockReset" in mock)
      (mock as { mockReset: () => void }).mockReset();
  });
  mocks.replace.mockImplementation(() => Promise.resolve());
  mocks.route.query = {};
  mocks.listTraces.mockResolvedValue({
    items: [],
    total: 0,
    page: 1,
    page_size: 20,
  });
  mocks.getTraceExportRisk.mockResolvedValue({
    acknowledged: true,
    version: "v1",
    phrase_en: "x",
    phrase_zh: "x",
    admin_user_id: 1,
    accepted_at: null,
  });
});

async function mountView() {
  const wrapper = mount(RequestTraceView, {
    global: { stubs: { AppLayout: { template: "<div><slot /></div>" } } },
  });
  await flushPromises();
  return wrapper;
}

/** Opens the recall panel; the list and its "load more" control live inside it. */
async function openRecall(wrapper: Awaited<ReturnType<typeof mountView>>) {
  await wrapper
    .find('[data-testid="request-trace-export-recall-toggle"]')
    .trigger("click");
  await flushPromises();
}

describe("Request Trace export task recall", () => {
  it("finds this session's tasks on arrival without the operator asking", async () => {
    mocks.listTraceExports.mockResolvedValue(
      page([task(FIRST_TASK), task(SECOND_TASK)]),
    );
    const wrapper = await mountView();

    expect(mocks.listTraceExports).toHaveBeenCalledTimes(1);
    expect(
      wrapper.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("2");
    // No token from the server means there is no further page to offer.
    expect(
      wrapper.find('[data-testid="request-trace-export-recall-more"]').exists(),
    ).toBe(false);
  });

  it("reopens a recalled task by its id, leaving the reading to the server", async () => {
    mocks.listTraceExports.mockResolvedValue(page([task(FIRST_TASK)]));
    const wrapper = await mountView();

    await wrapper
      .find('[data-testid="request-trace-export-recall-toggle"]')
      .trigger("click");
    await flushPromises();
    await wrapper
      .find(`[data-testid="request-trace-export-recall-task-${FIRST_TASK}"]`)
      .trigger("click");
    await flushPromises();

    expect(mocks.replace).toHaveBeenCalled();
    const lastQuery = mocks.replace.mock.calls.at(-1)?.[0] as {
      query: Record<string, string>;
    };
    expect(lastQuery.query.export).toBe(FIRST_TASK);
  });

  it("finds the task again after leaving the page, and keeps nothing in the browser", async () => {
    mocks.listTraceExports.mockResolvedValue(page([task(FIRST_TASK)]));
    const setItem = vi.spyOn(window.localStorage, "setItem");

    const first = await mountView();
    expect(
      first.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("1");
    first.unmount();

    // A second visit in the same admin session asks the server again rather than
    // reading a cached handle.
    mocks.listTraceExports.mockClear();
    const second = await mountView();
    expect(mocks.listTraceExports).toHaveBeenCalledTimes(1);
    expect(
      second.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("1");
    expect(setItem).not.toHaveBeenCalled();
    setItem.mockRestore();
  });

  it("keeps the oldest task reachable when a session has more tasks than one page", async () => {
    // 21 tasks: the server answers with 20 and a token for the rest. Without the
    // next page the twenty-first task would be invisible for good.
    const ids = taskIDs(1, 21);
    mocks.listTraceExports.mockImplementation(
      (options?: { cursor?: string | null }) => {
        if (options?.cursor) return Promise.resolve(page([task(ids[20])]));
        return Promise.resolve(
          page(
            ids.slice(0, 20).map((id) => task(id)),
            "v1.page-2",
          ),
        );
      },
    );
    const wrapper = await mountView();
    await openRecall(wrapper);

    expect(
      wrapper.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("20+");
    const more = wrapper.find(
      '[data-testid="request-trace-export-recall-more"]',
    );
    expect(more.exists()).toBe(true);

    await more.trigger("click");
    await flushPromises();

    expect(mocks.listTraceExports).toHaveBeenLastCalledWith(
      expect.objectContaining({ cursor: "v1.page-2" }),
    );
    // The next page is appended, not substituted: nothing already read is lost.
    expect(
      wrapper.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("21");
    expect(
      wrapper
        .find(`[data-testid="request-trace-export-recall-task-${ids[0]}"]`)
        .exists(),
    ).toBe(true);
    expect(
      wrapper
        .find(`[data-testid="request-trace-export-recall-task-${ids[20]}"]`)
        .exists(),
    ).toBe(true);
    // The server said the second page is the last one.
    expect(
      wrapper.find('[data-testid="request-trace-export-recall-more"]').exists(),
    ).toBe(false);
  });

  it("keeps the loaded tasks when the next page fails, and can ask for it again", async () => {
    const ids = taskIDs(1, 21);
    let nextPageCalls = 0;
    mocks.listTraceExports.mockImplementation(
      (options?: { cursor?: string | null }) => {
        if (!options?.cursor)
          return Promise.resolve(
            page(
              ids.slice(0, 20).map((id) => task(id)),
              "v1.page-2",
            ),
          );
        nextPageCalls += 1;
        if (nextPageCalls === 1)
          return Promise.reject(new Error("synthetic next page failure"));
        return Promise.resolve(page([task(ids[20])]));
      },
    );
    const wrapper = await mountView();
    await openRecall(wrapper);

    await wrapper
      .find('[data-testid="request-trace-export-recall-more"]')
      .trigger("click");
    await flushPromises();

    // A failed next page is not "no tasks": the twenty already read stay, the
    // failure is shown as itself, and the same page can be asked for again.
    expect(
      wrapper
        .find('[data-testid="request-trace-export-recall-more-failed"]')
        .exists(),
    ).toBe(true);
    expect(
      wrapper
        .find('[data-testid="request-trace-export-recall-empty"]')
        .exists(),
    ).toBe(false);
    expect(
      wrapper.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("20+");
    expect(
      wrapper
        .find(`[data-testid="request-trace-export-recall-task-${ids[0]}"]`)
        .exists(),
    ).toBe(true);

    await wrapper
      .find('[data-testid="request-trace-export-recall-more"]')
      .trigger("click");
    await flushPromises();

    expect(
      wrapper.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("21");
    expect(
      wrapper
        .find('[data-testid="request-trace-export-recall-more-failed"]')
        .exists(),
    ).toBe(false);
    expect(
      wrapper.find('[data-testid="request-trace-export-recall-more"]').exists(),
    ).toBe(false);
  });

  it("renders a task once even if a page repeats one that is already listed", async () => {
    // The server's key-set walk is disjoint, so a repeat should not happen; if it
    // ever does, one task id must still be one row.
    const ids = taskIDs(1, 21);
    mocks.listTraceExports.mockImplementation(
      (options?: { cursor?: string | null }) => {
        if (options?.cursor)
          return Promise.resolve(page([task(ids[19]), task(ids[20])]));
        return Promise.resolve(
          page(
            ids.slice(0, 20).map((id) => task(id)),
            "v1.page-2",
          ),
        );
      },
    );
    const wrapper = await mountView();
    await openRecall(wrapper);

    await wrapper
      .find('[data-testid="request-trace-export-recall-more"]')
      .trigger("click");
    await flushPromises();

    expect(
      wrapper.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("21");
    expect(
      wrapper.findAll(
        `[data-testid="request-trace-export-recall-task-${ids[19]}"]`,
      ),
    ).toHaveLength(1);
  });

  it("ignores a next page that arrives after a refresh replaced the list", async () => {
    const ids = taskIDs(1, 21);
    const refreshed = taskIDs(101, 20);
    let resolveStale:
      | ((value: { items: unknown[]; nextCursor: string | null }) => void)
      | null = null;
    let firstReads = 0;
    mocks.listTraceExports.mockImplementation(
      (options?: { cursor?: string | null }) => {
        if (options?.cursor) {
          return new Promise((resolve) => {
            resolveStale = resolve;
          });
        }
        firstReads += 1;
        const rows = firstReads === 1 ? ids.slice(0, 20) : refreshed;
        return Promise.resolve(
          page(
            rows.map((id) => task(id)),
            "v1.page-2",
          ),
        );
      },
    );
    const wrapper = await mountView();
    await openRecall(wrapper);

    // The next page is still in flight when the operator refreshes the panel.
    await wrapper
      .find('[data-testid="request-trace-export-recall-more"]')
      .trigger("click");
    await flushPromises();
    await wrapper
      .find('[data-testid="request-trace-export-recall-toggle"]')
      .trigger("click");
    await flushPromises();
    await wrapper
      .find('[data-testid="request-trace-export-recall-toggle"]')
      .trigger("click");
    await flushPromises();
    expect(
      wrapper.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("20+");

    // The late page belongs to the list that is no longer on screen: appending it
    // would mix two reads, and its stale rows are not part of this one.
    expect(resolveStale).not.toBeNull();
    resolveStale?.({ items: [task(ids[0]), task(ids[20])], nextCursor: null });
    await flushPromises();

    expect(
      wrapper.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("20+");
    expect(
      wrapper
        .find(`[data-testid="request-trace-export-recall-task-${ids[0]}"]`)
        .exists(),
    ).toBe(false);
    expect(
      wrapper
        .find(
          `[data-testid="request-trace-export-recall-task-${refreshed[0]}"]`,
        )
        .exists(),
    ).toBe(true);
    // The refresh left a token of its own, so the walk can continue from here.
    expect(
      wrapper.find('[data-testid="request-trace-export-recall-more"]').exists(),
    ).toBe(true);
    expect(
      wrapper
        .find('[data-testid="request-trace-export-recall-more-failed"]')
        .exists(),
    ).toBe(false);
  });

  it("stops instead of re-reading the same page when the token does not advance", async () => {
    // A server that keeps handing back the token it was given is not paging:
    // following it forever would append the same rows on every click.
    mocks.listTraceExports.mockImplementation(
      (options?: { cursor?: string | null }) =>
        Promise.resolve(
          options?.cursor
            ? page([task(SECOND_TASK)], "v1.stuck")
            : page([task(FIRST_TASK)], "v1.stuck"),
        ),
    );
    const wrapper = await mountView();
    await openRecall(wrapper);

    await wrapper
      .find('[data-testid="request-trace-export-recall-more"]')
      .trigger("click");
    await flushPromises();

    // The page was real, so its task is kept; the token behind it is not, so the
    // walk ends instead of asking for the same page again.
    expect(
      wrapper.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("2");
    expect(
      wrapper.find('[data-testid="request-trace-export-recall-more"]').exists(),
    ).toBe(false);
  });

  it("tells a refused session why, instead of claiming there are no tasks", async () => {
    const refused = Object.assign(new Error("refused"), {
      refusal: "session_required",
    });
    mocks.listTraceExports.mockRejectedValue(refused);
    const wrapper = await mountView();

    await wrapper
      .find('[data-testid="request-trace-export-recall-toggle"]')
      .trigger("click");
    await flushPromises();

    expect(
      wrapper.find('[data-testid="request-trace-export-recall-failed"]').text(),
    ).toBe("admin.requestTrace.export.refusal.session_required");
    expect(
      wrapper
        .find('[data-testid="request-trace-export-recall-empty"]')
        .exists(),
    ).toBe(false);
  });

  it("shows an empty recall as empty, not as a refusal", async () => {
    mocks.listTraceExports.mockResolvedValue(page([]));
    const wrapper = await mountView();

    await wrapper
      .find('[data-testid="request-trace-export-recall-toggle"]')
      .trigger("click");
    await flushPromises();

    expect(
      wrapper.find('[data-testid="request-trace-export-recall-empty"]').text(),
    ).toBe("admin.requestTrace.export.task.absent");
    expect(
      wrapper
        .find('[data-testid="request-trace-export-recall-failed"]')
        .exists(),
    ).toBe(false);
    expect(
      wrapper
        .find('[data-testid="request-trace-export-recall-count"]')
        .exists(),
    ).toBe(false);
  });

  it("marks an incomplete task as incomplete in the recalled list", async () => {
    mocks.listTraceExports.mockResolvedValue(
      page([task(FIRST_TASK, { truncated: true, downloadable: true })]),
    );
    const wrapper = await mountView();

    await wrapper
      .find('[data-testid="request-trace-export-recall-toggle"]')
      .trigger("click");
    await flushPromises();

    const entry = wrapper.find(
      `[data-testid="request-trace-export-recall-task-${FIRST_TASK}"]`,
    );
    expect(entry.text()).toContain(
      "admin.requestTrace.export.state.incomplete",
    );
  });

  it("shows a completed export whose own record is gone as file lost, not as done", async () => {
    // The server says the manifest could not be read, so it knows neither the
    // shard list nor whether the delivery was cut short. That is not "done".
    mocks.listTraceExports.mockResolvedValue(
      page([
        task(FIRST_TASK, {
          truncated: true,
          incomplete_reason: "manifest_lost",
        }),
        task(SECOND_TASK, { truncated: true, incomplete_reason: "limit_rows" }),
      ]),
    );
    const wrapper = await mountView();
    await openRecall(wrapper);

    expect(
      wrapper
        .find(`[data-testid="request-trace-export-recall-task-${FIRST_TASK}"]`)
        .text(),
    ).toContain("admin.requestTrace.export.state.file_lost");
    // A truncation with a known reason keeps its own state: "we know what was cut"
    // and "we cannot tell" are different facts.
    expect(
      wrapper
        .find(`[data-testid="request-trace-export-recall-task-${SECOND_TASK}"]`)
        .text(),
    ).toContain("admin.requestTrace.export.state.incomplete");
  });

  it("counts the tasks loaded, not the tasks that exist, while a next page remains", async () => {
    const ids = taskIDs(1, 21);
    mocks.listTraceExports.mockImplementation(
      (options?: { cursor?: string | null }) =>
        options?.cursor
          ? Promise.resolve(page([task(ids[20])]))
          : Promise.resolve(
              page(
                ids.slice(0, 20).map((id) => task(id)),
                "v1.page-2",
              ),
            ),
    );
    const wrapper = await mountView();

    // A bare "20" would claim the operator has twenty tasks and hide the rest.
    expect(
      wrapper.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("20+");

    await openRecall(wrapper);
    await wrapper
      .find('[data-testid="request-trace-export-recall-more"]')
      .trigger("click");
    await flushPromises();

    // Once the server says there is nothing behind it, the count is a total.
    expect(
      wrapper.find('[data-testid="request-trace-export-recall-count"]').text(),
    ).toBe("21");
  });
});
