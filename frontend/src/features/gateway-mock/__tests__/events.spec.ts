/**
 * The read-only hit list reads one page on demand and renders observable facts
 * only: what it did not record is shown as explicitly absent, a protocol outside
 * the closed set is labelled as unknown rather than echoed, and a read that could
 * not be completed never leaves the previous page standing as if it were current.
 */
import { flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({ listEvents: vi.fn() }));
vi.mock("../api", () => mocks);
vi.mock("vue-i18n", async (importOriginal) => {
  const actual = await importOriginal<typeof import("vue-i18n")>();
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) };
});

// 部署配置可能声明超过网关命中列表上限的页大小，用注入配置验证面板会过滤。
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

import GatewayMockEventsPanel from "../GatewayMockEventsPanel.vue";
import Pagination from "@/components/common/Pagination.vue";
import { gatewayMockEventMaxPageSize, gatewayMockProtocols } from "../types";

type Raw = Record<string, unknown>;

const event = (patch: Raw = {}): Raw => ({
  occurred_at: "2026-09-30T03:04:05Z",
  rule_id: "gmr_0123456789abcdef",
  rule_version: "2026-09-30T03:00:00Z",
  protocol: "messages",
  model: "claude-sonnet-4-5",
  api_key_id: 7,
  user_id: 3,
  group_id: 2,
  account_id: 11,
  client_ip: "203.0.113.7",
  trace_id: "0123456789abcdef0123456789abcdef",
  cleanup_after: "2026-12-29T03:04:05Z",
  ...patch,
});

const page = (patch: Raw = {}): Raw => ({
  items: [event()],
  total: 1,
  page: 1,
  page_size: 20,
  ...patch,
});

function mountPanel() {
  return mount(GatewayMockEventsPanel);
}

async function mountLoaded(payload: Raw = page()) {
  mocks.listEvents.mockResolvedValue(payload);
  const wrapper = mountPanel();
  await flushPromises();
  return wrapper;
}

// 账号 / IP / Trace / 规则版本 / 清理说明在展开的只读元数据区，先展开再断言。
async function expandDetail(wrapper: ReturnType<typeof mountPanel>) {
  await wrapper
    .get('[data-testid="gateway-mock-events-detail-toggle"]')
    .trigger("click");
}

describe("admin gateway mock hit list", () => {
  beforeEach(() => {
    mocks.listEvents.mockReset();
    preferences.getConfiguredTablePageSizeOptions.mockReturnValue([
      10, 20, 50, 100,
    ]);
  });
  afterEach(() => vi.restoreAllMocks());

  it("keeps secondary metadata in an accessible expandable detail region", async () => {
    const wrapper = await mountLoaded();
    const toggle = wrapper.get(
      '[data-testid="gateway-mock-events-detail-toggle"]',
    );
    expect(toggle.attributes("aria-expanded")).toBe("false");
    expect(
      wrapper.find('[data-testid="gateway-mock-events-trace"]').exists(),
    ).toBe(false);
    await toggle.trigger("click");
    expect(toggle.attributes("aria-expanded")).toBe("true");
    expect(
      wrapper.get('[data-testid="gateway-mock-events-trace"]').text(),
    ).toBe("0123456789abcdef0123456789abcdef");
    expect(
      wrapper.get('[data-testid="gateway-mock-events-account"]').text(),
    ).toBe("#11");
    await toggle.trigger("click");
    expect(
      wrapper.find('[data-testid="gateway-mock-events-trace"]').exists(),
    ).toBe(false);
  });

  it("retries a failed read without treating it as an empty history", async () => {
    mocks.listEvents.mockRejectedValueOnce(new Error("unavailable"));
    const wrapper = mountPanel();
    await flushPromises();
    expect(
      wrapper.find('[data-testid="gateway-mock-events-failed"]').exists(),
    ).toBe(true);
    mocks.listEvents.mockResolvedValueOnce(page());
    await wrapper
      .get('[data-testid="gateway-mock-events-retry"]')
      .trigger("click");
    await flushPromises();
    expect(
      wrapper.find('[data-testid="gateway-mock-events-row"]').exists(),
    ).toBe(true);
    expect(
      wrapper.find('[data-testid="gateway-mock-events-failed"]').exists(),
    ).toBe(false);
  });

  it("opens and closes the detail from the keyboard, not only a pointer", async () => {
    const wrapper = await mountLoaded();
    const toggle = wrapper.get(
      '[data-testid="gateway-mock-events-detail-toggle"]',
    );
    // 原生 button 才具备键盘可达性，配合 aria-expanded / aria-controls 暴露展开态。
    expect(toggle.element.tagName).toBe("BUTTON");

    // jsdom 不合成原生键盘点击；用 click 验证同一激活事件，真实键盘行为由浏览器验收。
    await toggle.trigger("click");
    expect(toggle.attributes("aria-expanded")).toBe("true");
    expect(
      wrapper.find(`#${toggle.attributes("aria-controls")}`).exists(),
    ).toBe(true);
    await toggle.trigger("click");
    expect(toggle.attributes("aria-expanded")).toBe("false");
    expect(toggle.attributes("aria-controls")).toBeUndefined();
  });

  it("collapses the open detail when a refresh replaces the rows", async () => {
    const wrapper = await mountLoaded();
    await expandDetail(wrapper);
    expect(
      wrapper.find('[data-testid="gateway-mock-events-trace"]').exists(),
    ).toBe(true);

    mocks.listEvents.mockResolvedValueOnce(
      page({ items: [event({ rule_id: "gmr_second" })] }),
    );
    await wrapper
      .get('[data-testid="gateway-mock-events-refresh"]')
      .trigger("click");
    await flushPromises();

    expect(
      wrapper.get('[data-testid="gateway-mock-events-rule-id"]').text(),
    ).toBe("gmr_second");
    // 事件没有稳定 id：旧行下标不能把详情展开到新内容上。
    expect(
      wrapper.find('[data-testid="gateway-mock-events-trace"]').exists(),
    ).toBe(false);
    expect(
      wrapper
        .get('[data-testid="gateway-mock-events-detail-toggle"]')
        .attributes("aria-expanded"),
    ).toBe("false");
  });

  it("does not reopen a stale row when details are clicked during a refresh", async () => {
    const wrapper = await mountLoaded();
    let resolveRead!: (value: unknown) => void;
    mocks.listEvents.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveRead = resolve;
        }),
    );
    await wrapper
      .get('[data-testid="gateway-mock-events-refresh"]')
      .trigger("click");
    const toggle = wrapper.get(
      '[data-testid="gateway-mock-events-detail-toggle"]',
    );
    expect((toggle.element as HTMLButtonElement).disabled).toBe(true);
    await toggle.trigger("click");
    resolveRead(page({ items: [event({ rule_id: "gmr_replacement" })] }));
    await flushPromises();
    expect(
      wrapper.find('[data-testid="gateway-mock-events-detail-row"]').exists(),
    ).toBe(false);
  });

  it("retries the requested page and disables retry while a read is in flight", async () => {
    const wrapper = await mountLoaded(page({ total: 60 }));
    mocks.listEvents.mockRejectedValueOnce(new Error("unavailable"));
    wrapper.getComponent(Pagination).vm.$emit("update:page", 2);
    await flushPromises();
    let resolveRead!: (value: unknown) => void;
    mocks.listEvents.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveRead = resolve;
        }),
    );
    await wrapper
      .get('[data-testid="gateway-mock-events-retry"]')
      .trigger("click");
    expect(mocks.listEvents).toHaveBeenLastCalledWith(
      { page: 2, page_size: 20 },
      expect.anything(),
    );
    const retry = wrapper.get('[data-testid="gateway-mock-events-retry"]');
    expect((retry.element as HTMLButtonElement).disabled).toBe(true);
    resolveRead(page({ page: 2, total: 60 }));
    await flushPromises();
  });

  it("announces its phase: busy while reading, a status role when empty", async () => {
    let resolveRead!: (value: unknown) => void;
    mocks.listEvents.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveRead = resolve;
        }),
    );
    const wrapper = mountPanel();
    await wrapper.vm.$nextTick();

    expect(
      wrapper
        .get('[data-testid="gateway-mock-events"]')
        .attributes("aria-busy"),
    ).toBe("true");
    expect(
      wrapper
        .get('[data-testid="gateway-mock-events-loading"]')
        .attributes("role"),
    ).toBe("status");

    resolveRead({ items: [], total: 0, page: 1, page_size: 20 });
    await flushPromises();

    expect(
      wrapper
        .get('[data-testid="gateway-mock-events-empty"]')
        .attributes("role"),
    ).toBe("status");
    expect(
      wrapper
        .get('[data-testid="gateway-mock-events"]')
        .attributes("aria-busy"),
    ).toBe("false");
  });

  it("keeps one table whose cells label themselves for the narrow layout", async () => {
    const wrapper = await mountLoaded();

    // 单一 DOM：桌面与窄屏共用同一张 6 列表格，不再渲染第二套带重复 testid 的行。
    const table = wrapper.get("table.gateway-mock-events-table");
    expect(table.element.tagName).toBe("TABLE");
    expect(table.findAll("thead th")).toHaveLength(6);
    expect(
      wrapper.findAll('[data-testid="gateway-mock-events-row"]'),
    ).toHaveLength(1);
    expect(
      wrapper.findAll('[data-testid="gateway-mock-events-time"]'),
    ).toHaveLength(1);

    // 除展开按钮外每个数据单元格都带行内标签，窄屏折叠后仍能读出字段名。
    const labelled = table.findAll("tbody tr:first-child td[data-label]");
    expect(labelled).toHaveLength(5);
    for (const cell of labelled) {
      expect(cell.attributes("data-label")).toBeTruthy();
    }

    // 详情独占整行，用 aria-controls 指向的 id 关联。
    await expandDetail(wrapper);
    const toggle = wrapper.get(
      '[data-testid="gateway-mock-events-detail-toggle"]',
    );
    const detailCell = wrapper.get(
      '[data-testid="gateway-mock-events-detail-row"] td',
    );
    expect(detailCell.attributes("colspan")).toBe("6");
    expect(detailCell.attributes("id")).toBe(
      toggle.attributes("aria-controls"),
    );
  });

  it("reads the first page on mount and renders the stored facts", async () => {
    const wrapper = await mountLoaded(page({ total: 1 }));

    expect(mocks.listEvents).toHaveBeenCalledTimes(1);
    expect(mocks.listEvents).toHaveBeenCalledWith(
      { page: 1, page_size: 20 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    // 规则版本 / 账号 / IP / Trace 在详情里，展开后再检查（不删除原有断言）。
    await expandDetail(wrapper);
    expect(
      wrapper.get('[data-testid="gateway-mock-events-rule-id"]').text(),
    ).toBe("gmr_0123456789abcdef");
    expect(
      wrapper.get('[data-testid="gateway-mock-events-rule-version"]').text(),
    ).toBe("2026-09-30T03:00:00Z");
    expect(
      wrapper.get('[data-testid="gateway-mock-events-api-key"]').text(),
    ).toBe("#7");
    expect(wrapper.get('[data-testid="gateway-mock-events-user"]').text()).toBe(
      "#3",
    );
    expect(
      wrapper.get('[data-testid="gateway-mock-events-group"]').text(),
    ).toBe("#2");
    expect(
      wrapper.get('[data-testid="gateway-mock-events-account"]').text(),
    ).toBe("#11");
    expect(
      wrapper.get('[data-testid="gateway-mock-events-client-ip"]').text(),
    ).toBe("203.0.113.7");
    expect(
      wrapper.get('[data-testid="gateway-mock-events-trace"]').text(),
    ).toBe("0123456789abcdef0123456789abcdef");
    expect(
      wrapper.get('[data-testid="gateway-mock-events-model"]').text(),
    ).toBe("claude-sonnet-4-5");
    // 与审计页统一使用同一分页组件。
    expect(wrapper.findComponent(Pagination).exists()).toBe(true);
  });

  it("discloses that hits follow the usage-retention policy and are kept while it is off", async () => {
    const wrapper = await mountLoaded();
    expect(
      wrapper.get('[data-testid="gateway-mock-events-retention-note"]').text(),
    ).toBe("admin.gatewayMock.events.retentionNote");
  });

  it("says a hit has no recorded cleanup deadline instead of inventing a date", async () => {
    const wrapper = await mountLoaded(
      page({ items: [event({ cleanup_after: null })] }),
    );
    await expandDetail(wrapper);

    const cell = wrapper.get('[data-testid="gateway-mock-events-cleanup"]');
    expect(cell.text()).toBe("admin.gatewayMock.events.noDeadline");
    // 那不是"未观察到该字段"的占位，也不是被猜成某个日期的空白。
    expect(cell.text()).not.toBe("admin.gatewayMock.events.absent");
    expect(Number.isNaN(Date.parse(cell.text()))).toBe(true);
  });

  it("never renders the stored deadline, even when the answer still carries one", async () => {
    // 服务端不再披露该列（落库值只是内部估算）。旧实例仍可能回一个日期：
    // 面板必须继续按"未记录清理期限"呈现，而不是把它当成实际清理时间。
    const wrapper = await mountLoaded(
      page({ items: [event({ cleanup_after: "2026-12-29T03:04:05Z" })] }),
    );
    await expandDetail(wrapper);

    const cell = wrapper.get('[data-testid="gateway-mock-events-cleanup"]');
    expect(cell.text()).toBe("admin.gatewayMock.events.noDeadline");
    expect(wrapper.text()).not.toContain(
      new Date("2026-12-29T03:04:05Z").toLocaleDateString(),
    );
  });

  it("shows the retention disclosure even when nothing has been recorded", async () => {
    const wrapper = await mountLoaded({
      items: [],
      total: 0,
      page: 1,
      page_size: 20,
    });
    expect(
      wrapper.get('[data-testid="gateway-mock-events-retention-note"]').text(),
    ).toBe("admin.gatewayMock.events.retentionNote");
  });

  it("renders every value the gateway did not record as explicitly absent, never blank", async () => {
    const wrapper = await mountLoaded(
      page({
        items: [
          event({
            rule_version: "",
            model: "",
            api_key_id: 0,
            user_id: 0,
            group_id: 0,
            account_id: 0,
            client_ip: "",
            trace_id: "",
          }),
        ],
      }),
    );

    const absent = "admin.gatewayMock.events.absent";
    await expandDetail(wrapper);
    for (const testid of [
      "gateway-mock-events-rule-version",
      "gateway-mock-events-model",
      "gateway-mock-events-api-key",
      "gateway-mock-events-user",
      "gateway-mock-events-group",
      "gateway-mock-events-account",
      "gateway-mock-events-client-ip",
      "gateway-mock-events-trace",
    ]) {
      expect(wrapper.get(`[data-testid="${testid}"]`).text(), testid).toBe(
        absent,
      );
    }
    // 没有记录的编号不会渲染成 0 号。
    expect(wrapper.text()).not.toContain("#0");
    expect(
      wrapper.get('[data-testid="gateway-mock-events-absent-note"]').text(),
    ).toBe("admin.gatewayMock.events.absentNote");
  });

  it.each([...gatewayMockProtocols])(
    "renders the %s protocol as its own label",
    async (protocol) => {
      const wrapper = await mountLoaded(page({ items: [event({ protocol })] }));
      const rendered = wrapper
        .get('[data-testid="gateway-mock-events-protocol"]')
        .text();
      expect(rendered).toBe(`admin.gatewayMock.events.protocol.${protocol}`);
      expect(rendered).not.toBe(protocol);
    },
  );

  it("never shows a raw protocol token when a value outside the closed set arrives", async () => {
    const wrapper = await mountLoaded(
      page({ items: [event({ protocol: "invented_protocol" })] }),
    );
    expect(
      wrapper.get('[data-testid="gateway-mock-events-protocol"]').text(),
    ).toBe("admin.gatewayMock.events.protocol.unknown");
    expect(wrapper.text()).not.toContain("invented_protocol");
  });

  it("distinguishes a hit that skipped the content audit from one that did not record it", async () => {
    const wrapper = await mountLoaded(
      page({
        items: [
          event({ content_audit_state: "skipped_local_mock" }),
          event({ content_audit_state: "unknown" }),
        ],
      }),
    );
    const cells = wrapper.findAll(
      '[data-testid="gateway-mock-events-content-audit"]',
    );
    expect(cells).toHaveLength(2);
    expect(cells[0].text()).toBe(
      "admin.gatewayMock.events.contentAudit.skipped_local_mock",
    );
    expect(cells[1].text()).toBe(
      "admin.gatewayMock.events.contentAudit.unknown",
    );
    expect(cells[0].text()).not.toBe(cells[1].text());
  });

  it("treats a missing or unrecognised content-audit state as not recorded, never as skipped", async () => {
    // 旧服务响应缺该字段；未来枚举/损坏值也不回显，一律安全降级为 unknown。
    const wrapper = await mountLoaded(
      page({
        items: [
          event(),
          event({ content_audit_state: "invented_audit_state" }),
        ],
      }),
    );
    const cells = wrapper.findAll(
      '[data-testid="gateway-mock-events-content-audit"]',
    );
    expect(cells[0].text()).toBe(
      "admin.gatewayMock.events.contentAudit.unknown",
    );
    expect(cells[1].text()).toBe(
      "admin.gatewayMock.events.contentAudit.unknown",
    );
    expect(wrapper.text()).not.toContain("invented_audit_state");
    expect(wrapper.text()).not.toContain("skipped_local_mock");
  });

  it("renders no configured keyword and no reply text even if the answer carries them", async () => {
    const wrapper = await mountLoaded(
      page({
        items: [
          event({
            keyword: "CANARY_KEYWORD",
            reply: "CANARY_REPLY",
            normalized_keyword: "canary_keyword",
          }),
        ],
      }),
    );

    expect(
      wrapper.get('[data-testid="gateway-mock-events-rule-id"]').text(),
    ).toBe("gmr_0123456789abcdef");
    expect(wrapper.text()).not.toContain("CANARY_KEYWORD");
    expect(wrapper.text()).not.toContain("CANARY_REPLY");
  });

  it("reports an unreadable list as unknown instead of an empty history", async () => {
    mocks.listEvents.mockRejectedValue({
      status: 503,
      reason: "GATEWAY_MOCK_EVENTS_UNAVAILABLE",
    });
    const wrapper = mountPanel();
    await flushPromises();

    expect(
      wrapper.get('[data-testid="gateway-mock-events-failed"]').text(),
    ).toBe("admin.gatewayMock.events.failed");
    expect(
      wrapper.find('[data-testid="gateway-mock-events-row"]').exists(),
    ).toBe(false);
    expect(
      wrapper.find('[data-testid="gateway-mock-events-empty"]').exists(),
    ).toBe(false);
  });

  it('renders an empty list as "nothing recorded", not as a failure', async () => {
    const wrapper = await mountLoaded({
      items: [],
      total: 0,
      page: 1,
      page_size: 20,
    });
    expect(
      wrapper.get('[data-testid="gateway-mock-events-empty"]').text(),
    ).toBe("admin.gatewayMock.events.empty");
    expect(
      wrapper.find('[data-testid="gateway-mock-events-failed"]').exists(),
    ).toBe(false);
  });

  it("pages forward and back through the shared pagination, and disables the ends", async () => {
    mocks.listEvents
      .mockResolvedValueOnce({
        items: [event()],
        total: 25,
        page: 1,
        page_size: 20,
      })
      .mockResolvedValueOnce({
        items: [event()],
        total: 25,
        page: 2,
        page_size: 20,
      })
      .mockResolvedValueOnce({
        items: [event()],
        total: 25,
        page: 1,
        page_size: 20,
      });
    const wrapper = mountPanel();
    await flushPromises();

    // 桌面分页导航：首尾按钮即上一页 / 下一页。
    const desktopButtons = () =>
      wrapper.get('nav[aria-label="Pagination"]').findAll("button");
    expect((desktopButtons()[0].element as HTMLButtonElement).disabled).toBe(
      true,
    );
    expect(
      (desktopButtons().at(-1)!.element as HTMLButtonElement).disabled,
    ).toBe(false);

    await desktopButtons().at(-1)!.trigger("click");
    await flushPromises();
    expect(mocks.listEvents).toHaveBeenLastCalledWith(
      { page: 2, page_size: 20 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect((desktopButtons()[0].element as HTMLButtonElement).disabled).toBe(
      false,
    );
    expect(
      (desktopButtons().at(-1)!.element as HTMLButtonElement).disabled,
    ).toBe(true);

    await desktopButtons()[0].trigger("click");
    await flushPromises();
    expect(mocks.listEvents).toHaveBeenLastCalledWith(
      { page: 1, page_size: 20 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
  });

  it("applies a new page size and clamps it to the API maximum", async () => {
    mocks.listEvents.mockResolvedValue({
      items: [event()],
      total: 25,
      page: 1,
      page_size: 50,
    });
    const wrapper = mountPanel();
    await flushPromises();

    wrapper.getComponent(Pagination).vm.$emit("update:page-size", 50);
    await flushPromises();
    expect(mocks.listEvents).toHaveBeenLastCalledWith(
      { page: 1, page_size: 50 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );

    wrapper.getComponent(Pagination).vm.$emit("update:page-size", 500);
    await flushPromises();
    expect(mocks.listEvents).toHaveBeenLastCalledWith(
      { page: 1, page_size: 100 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
  });

  it("refreshes only on demand and never starts a poller", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    try {
      const wrapper = await mountLoaded();
      expect(mocks.listEvents).toHaveBeenCalledTimes(1);

      await vi.advanceTimersByTimeAsync(60_000);
      expect(mocks.listEvents).toHaveBeenCalledTimes(1);
      expect(vi.getTimerCount()).toBe(0);

      await wrapper
        .get('[data-testid="gateway-mock-events-refresh"]')
        .trigger("click");
      await flushPromises();
      expect(mocks.listEvents).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps an older answer from overwriting a newer refresh", async () => {
    let resolveFirst!: (value: unknown) => void;
    mocks.listEvents
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveFirst = resolve;
          }),
      )
      .mockResolvedValueOnce({
        items: [event({ rule_id: "gmr_newer" })],
        total: 1,
        page: 1,
        page_size: 20,
      });

    const wrapper = mountPanel();
    await wrapper
      .get('[data-testid="gateway-mock-events-refresh"]')
      .trigger("click");
    await flushPromises();
    resolveFirst({
      items: [event({ rule_id: "gmr_older" })],
      total: 1,
      page: 1,
      page_size: 20,
    });
    await flushPromises();

    expect(
      wrapper.get('[data-testid="gateway-mock-events-rule-id"]').text(),
    ).toBe("gmr_newer");
    expect(wrapper.text()).not.toContain("gmr_older");
  });

  it("aborts an in-flight read on unmount and adopts no stale answer", async () => {
    let resolveRead!: (value: unknown) => void;
    mocks.listEvents.mockImplementationOnce(
      (params: unknown, options?: { signal?: AbortSignal }) =>
        new Promise((resolve) => {
          options?.signal?.addEventListener("abort", () =>
            resolve(page({ items: [] })),
          );
          resolveRead = resolve;
        }),
    );
    const wrapper = mountPanel();
    const options = mocks.listEvents.mock.calls[0]?.[1] as
      { signal?: AbortSignal } | undefined;
    expect(options?.signal).toBeInstanceOf(AbortSignal);

    wrapper.unmount();
    expect(options?.signal?.aborted).toBe(true);

    resolveRead(page());
    await flushPromises();
    expect(
      wrapper.find('[data-testid="gateway-mock-events-row"]').exists(),
    ).toBe(false);
  });

  it("offers only page sizes within the API maximum, dropping a larger deployment option", async () => {
    preferences.getConfiguredTablePageSizeOptions.mockReturnValue([
      20, 100, 200,
    ]);
    const wrapper = await mountLoaded();

    const options = wrapper.getComponent(Pagination).props("pageSizeOptions");
    // 200 超出后端上限，下拉不得提供，否则选到它会把非法值写进全局持久化。
    expect(options).toEqual([20, 100]);
  });

  it("falls back to legal page sizes when configuration offers none within the maximum", async () => {
    preferences.getConfiguredTablePageSizeOptions.mockReturnValue([200]);
    const wrapper = await mountLoaded();

    const options = wrapper.getComponent(Pagination).props("pageSizeOptions");
    expect(options).toEqual([20, gatewayMockEventMaxPageSize]);
  });
});
