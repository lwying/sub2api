import { beforeEach, describe, expect, it, vi } from "vitest";

const client = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  put: vi.fn(),
}));
vi.mock("@/api/client", () => ({ apiClient: client }));

import {
  TraceExportRefusedError,
  acknowledgeTraceExportRisk,
  createTraceExport,
  downloadTraceExport,
  downloadTraceExportPart,
  getTrace,
  getTraceExport,
  getTraceExportLimits,
  getTraceExportRisk,
  listTraces,
  updateTraceExportLimits,
} from "../api";

const row = {
  trace_id: "a".repeat(32),
  route_family: "messages",
  inbound_endpoint: "/v1/messages",
  capture_state: "partial",
  client_status: 401,
  usage_log_id: null,
  cleanup_after: "2026-10-28T00:00:00Z",
  created_at: "2026-09-28T00:00:00Z",
  completed_at: null,
};

describe("Trace list and detail API", () => {
  beforeEach(() => Object.values(client).forEach((mock) => mock.mockReset()));

  it("reads metadata-only list using only approved metadata filters", async () => {
    client.get.mockResolvedValue({
      data: {
        items: [
          {
            ...row,
            payload_text: "PRIVATE_BODY",
            extra: { authorization: "Bearer TOKEN" },
          },
        ],
        total: 1,
        page: 1,
        page_size: 20,
      },
    });
    const result = await listTraces({
      page: 1,
      page_size: 20,
      trace_id: row.trace_id,
    });
    expect(client.get).toHaveBeenCalledWith(
      "/admin/request-traces",
      expect.objectContaining({
        params: {
          page: 1,
          page_size: 20,
          trace_id: row.trace_id,
          include_stats: true,
        },
        headers: expect.objectContaining({ "Cache-Control": "no-store" }),
      }),
    );
    expect(result.items).toHaveLength(1);
    expect(JSON.stringify(result.items)).not.toContain("PRIVATE_BODY");
    expect(JSON.stringify(result.items)).not.toContain("Bearer TOKEN");
  });

  it("rejects malformed list rows instead of fabricating a trace", async () => {
    client.get.mockResolvedValue({
      data: {
        items: [{ trace_id: "../other" }],
        total: 1,
        page: 1,
        page_size: 20,
      },
    });
    await expect(listTraces({ page: 1, page_size: 20 })).rejects.toThrow();
  });

  it("refuses a contradictory not-observed stage carrying plaintext", async () => {
    client.get.mockResolvedValue({
      data: {
        ...row,
        stages: [
          {
            ordinal: 1,
            stage: "inbound_request",
            state: "not_observed",
            reason: "auth_rejected_before_body",
            attempt_index: 0,
            view_name: "",
            observed_bytes: 0,
            retained_bytes: 0,
            dropped_events: 0,
            redaction_unverified: false,
            payload_text: "LEAK",
          },
        ],
      },
    });
    await expect(getTrace(row.trace_id)).rejects.toThrow();
  });

  it("reads plaintext only on explicit detail, normalizes stages and encodes ID in URL", async () => {
    client.get.mockResolvedValue({
      data: {
        ...row,
        stages: [
          {
            ordinal: 1,
            stage: "inbound_request",
            state: "not_observed",
            reason: "auth_rejected_before_body",
            attempt_index: 0,
            view_name: "",
            observed_bytes: 0,
            retained_bytes: 0,
            dropped_events: 0,
            redaction_unverified: false,
            metadata: { credentials: "SECRET" },
          },
        ],
      },
    });
    const result = await getTrace(row.trace_id);
    expect(client.get).toHaveBeenCalledWith(
      `/admin/request-traces/${row.trace_id}`,
      expect.objectContaining({
        headers: expect.objectContaining({ Pragma: "no-cache" }),
      }),
    );
    expect(JSON.stringify(result)).not.toContain("SECRET");
    expect(result.stages[0].reason).toBe("auth_rejected_before_body");
  });
});

// The real handler reads the metadata filter from the query string and returns the
// task view { id, status, filter, rows_exported, rows_skipped, bytes_exported,
// created_at, completed_at?, download_until?, downloadable }. Nothing else is decoded.
const TASK_ID = "b".repeat(32);
const exportTask = (overrides: Record<string, unknown> = {}) => ({
  id: TASK_ID,
  status: "pending",
  filter: { trace_id: "a".repeat(32), route_family: "messages" },
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

function withReason(reason: string, status = 410) {
  return {
    status,
    data: new Blob([JSON.stringify({ code: status, reason })]),
    headers: {},
  };
}

describe("Trace batch export API", () => {
  beforeEach(() => Object.values(client).forEach((mock) => mock.mockReset()));

  it("creates a bounded task from query metadata and sends no request body", async () => {
    client.post.mockResolvedValue({ data: exportTask() });
    const created = await createTraceExport({
      trace_id: "a".repeat(32),
      route_family: "messages",
      client_status: 429,
      usage_linked: false,
    });

    const [url, body, config] = client.post.mock.calls[0] as [
      string,
      unknown,
      { params?: Record<string, unknown>; headers?: Record<string, string> },
    ];
    expect(url).toBe("/admin/request-traces/exports");
    expect(body ?? null).toBeNull();
    expect(config.params).toEqual({
      trace_id: "a".repeat(32),
      route_family: "messages",
      client_status: 429,
      usage_linked: false,
    });
    expect(Object.keys(config.params ?? {}).sort()).toEqual([
      "client_status",
      "route_family",
      "trace_id",
      "usage_linked",
    ]);
    expect(JSON.stringify(config.params)).not.toMatch(
      /body|prompt|search|text/i,
    );
    expect(config.headers).toEqual(
      expect.objectContaining({
        "Cache-Control": "no-store",
        Pragma: "no-cache",
      }),
    );
    expect(created.id).toBe(TASK_ID);
    expect(created.filter).toEqual({
      trace_id: "a".repeat(32),
      route_family: "messages",
    });
  });

  it("keeps omitted completion timestamps null instead of inventing a deadline", async () => {
    client.post.mockResolvedValue({ data: exportTask() });
    const created = await createTraceExport({});

    expect(exportParams()).toEqual({});
    expect(created.completed_at).toBeNull();
    expect(created.download_until).toBeNull();
    expect(created.downloadable).toBe(false);
    expect(created.truncated).toBe(false);
    expect(created.shard_count).toBe(0);
    expect(created.incomplete_reason).toBeNull();
  });

  it("sends every list condition the query used, so the export answers the same question", async () => {
    client.post.mockResolvedValue({ data: exportTask() });
    await createTraceExport({
      route_family: "responses",
      client_status: 429,
      usage_linked: false,
      created_from: "2026-09-01T00:00:00Z",
      usage_log_id: 12,
      account_id: 34,
      group_id: 5,
      requested_model: "claude-sonnet-4-5",
      platform: "antigravity",
    });

    expect(exportParams()).toEqual({
      route_family: "responses",
      client_status: 429,
      usage_linked: false,
      created_from: "2026-09-01T00:00:00Z",
      usage_log_id: 12,
      account_id: 34,
      group_id: 5,
      requested_model: "claude-sonnet-4-5",
      platform: "antigravity",
    });
  });

  it("sends an explicit not-observed condition as its own flag, never as a value", async () => {
    client.post.mockResolvedValue({ data: exportTask() });
    await createTraceExport({
      group_unknown: true,
      model_unknown: true,
      platform_unknown: true,
    });

    expect(exportParams()).toEqual({
      group_unknown: true,
      model_unknown: true,
      platform_unknown: true,
    });
  });

  it("sends the checked set as a bounded structured body, never as a URL query", async () => {
    client.post.mockResolvedValue({ data: exportTask() });
    const first = "a".repeat(32);
    const second = "c".repeat(32);
    await createTraceExport({
      trace_ids: [first, second],
      route_family: "messages",
      client_status: 500,
    });

    // The checked set is one bounded JSON body: the id set never rides in the URL,
    // and no metadata filter is sent beside it (the two scopes are exclusive).
    const [url, body, config] = client.post.mock.calls[0] as [
      string,
      unknown,
      { params?: Record<string, unknown> },
    ];
    expect(url).toBe("/admin/request-traces/exports");
    expect(body).toEqual({ trace_ids: [first, second] });
    expect(config.params ?? {}).toEqual({});
    expect(JSON.stringify(config.params ?? {})).not.toContain(first);
  });

  it("refuses a checked set above the server bound instead of exporting part of it", async () => {
    const ids = Array.from({ length: 2001 }, (_, index) =>
      index.toString(16).padStart(32, "0"),
    );
    await expect(createTraceExport({ trace_ids: ids })).rejects.toMatchObject({
      refusal: "selection_too_large",
    });
    expect(client.post).not.toHaveBeenCalled();
  });

  it("refuses a checked set that is empty, malformed or repeated before calling the server", async () => {
    // An explicitly empty selection is an invalid scope, never a silent "export all".
    await expect(createTraceExport({ trace_ids: [] })).rejects.toMatchObject({
      refusal: "invalid_filter",
    });
    await expect(
      createTraceExport({ trace_ids: ["not-a-trace-id"] }),
    ).rejects.toMatchObject({ refusal: "invalid_filter" });
    const repeated = "a".repeat(32);
    await expect(
      createTraceExport({ trace_ids: [repeated, repeated] }),
    ).rejects.toMatchObject({ refusal: "invalid_filter" });
    expect(client.post).not.toHaveBeenCalled();
  });

  it("maps a refused creation onto a bounded outcome instead of the server message", async () => {
    const refusal = async (reason: string) => {
      client.post.mockRejectedValueOnce({
        status: 429,
        reason,
        message: "SECRET_SERVER_TEXT",
      });
      const error = await createTraceExport({}).catch(
        (thrown: unknown) => thrown,
      );
      expect(error).toBeInstanceOf(TraceExportRefusedError);
      expect(JSON.stringify(error)).not.toContain("SECRET_SERVER_TEXT");
      return (error as InstanceType<typeof TraceExportRefusedError>).refusal;
    };

    expect(await refusal("REQUEST_TRACE_EXPORT_RISK_ACK_REQUIRED")).toBe(
      "risk_ack_required",
    );
    expect(await refusal("REQUEST_TRACE_EXPORT_ADMIN_SESSION_REQUIRED")).toBe(
      "session_required",
    );
    expect(await refusal("REQUEST_TRACE_EXPORT_FORBIDDEN")).toBe(
      "session_required",
    );
    expect(await refusal("REQUEST_TRACE_EXPORT_DISABLED")).toBe("disabled");
    expect(await refusal("REQUEST_TRACE_EXPORT_CAPACITY_LIMIT")).toBe(
      "capacity",
    );
    expect(await refusal("REQUEST_TRACE_EXPORT_INVALID_FILTER")).toBe(
      "invalid_filter",
    );
    // Anything unrecognized, including a transport failure, is not a verdict.
    expect(await refusal("REQUEST_TRACE_EXPORT_SOMETHING_ELSE")).toBe(
      "unavailable",
    );
  });

  it("drops unknown task fields instead of carrying captured content or a download URL", async () => {
    client.post.mockResolvedValue({
      data: exportTask({
        payload_text: "BODY_CANARY",
        authorization: "Bearer CANARY",
        download_url: "https://public.example/leak",
      }),
    });
    const created = await createTraceExport({});

    expect(JSON.stringify(created)).not.toContain("BODY_CANARY");
    expect(JSON.stringify(created)).not.toContain("CANARY");
    expect(JSON.stringify(created)).not.toContain("public.example");
  });

  it("rejects a malformed task view rather than fabricating export progress", async () => {
    client.post.mockResolvedValue({ data: exportTask({ rows_exported: -1 }) });
    await expect(createTraceExport({})).rejects.toThrow();

    client.post.mockResolvedValue({ data: exportTask({ id: "../other" }) });
    await expect(createTraceExport({})).rejects.toThrow();

    client.post.mockResolvedValue({
      data: exportTask({ status: "almost_done" }),
    });
    await expect(createTraceExport({})).rejects.toThrow();
  });

  it("reads a truncated task as truncated, with its reason and shard count", async () => {
    client.post.mockResolvedValue({
      data: exportTask({
        status: "completed",
        truncated: true,
        incomplete_reason: "limit_rows",
        shard_count: 3,
      }),
    });
    const created = await createTraceExport({});

    expect(created.truncated).toBe(true);
    expect(created.incomplete_reason).toBe("limit_rows");
    expect(created.shard_count).toBe(3);
  });

  it("refuses to call a task complete when the completeness flag is missing", async () => {
    client.post.mockResolvedValue({
      data: exportTask({ truncated: undefined }),
    });
    await expect(createTraceExport({})).rejects.toThrow();
  });

  it("reads the same task view by id from the server-owned route", async () => {
    client.get.mockResolvedValue({
      data: exportTask({
        status: "completed",
        completed_at: "2026-09-28T00:10:00Z",
        download_until: "2026-10-05T00:10:00Z",
        rows_exported: 42,
        rows_skipped: 3,
        bytes_exported: 2048,
        downloadable: true,
      }),
    });
    const task = await getTraceExport(TASK_ID);

    expect(client.get).toHaveBeenCalledWith(
      `/admin/request-traces/exports/${TASK_ID}`,
      expect.objectContaining({
        headers: expect.objectContaining({ Pragma: "no-cache" }),
      }),
    );
    expect(task).toMatchObject({
      status: "completed",
      rows_exported: 42,
      rows_skipped: 3,
      bytes_exported: 2048,
      downloadable: true,
    });
  });

  it("refuses an id that the server route cannot own", async () => {
    await expect(getTraceExport("../exports")).rejects.toThrow();
    expect(client.get).not.toHaveBeenCalled();
  });

  it("downloads the manifest as a blob under the server filename, without a part", async () => {
    const blob = new Blob(["SYNTHETIC"]);
    client.get.mockResolvedValue({
      status: 200,
      data: blob,
      headers: {
        "content-disposition": `attachment; filename="sub2api-request-trace-export-${TASK_ID}.manifest.json"`,
      },
    });
    const result = await downloadTraceExport(TASK_ID);

    const [url, config] = client.get.mock.calls[0] as [
      string,
      {
        params?: unknown;
        responseType?: string;
        headers?: Record<string, string>;
      },
    ];
    expect(url).toBe(`/admin/request-traces/exports/${TASK_ID}/download`);
    expect(config.params).toBeUndefined();
    expect(config.responseType).toBe("blob");
    expect(config.headers).toEqual(
      expect.objectContaining({ "Cache-Control": "no-store" }),
    );
    expect(result).toEqual({
      outcome: "ready",
      blob,
      fileName: `sub2api-request-trace-export-${TASK_ID}.manifest.json`,
    });
  });

  it("downloads shard N with ?part=N from the same session-scoped route", async () => {
    const blob = new Blob(["SYNTHETIC"]);
    client.get.mockResolvedValue({
      status: 200,
      data: blob,
      headers: {
        "content-disposition": `attachment; filename="sub2api-request-trace-export-${TASK_ID}-part0002.jsonl"`,
      },
    });
    const result = await downloadTraceExportPart(TASK_ID, 2);

    const [url, config] = client.get.mock.calls[0] as [
      string,
      { params?: Record<string, unknown> },
    ];
    expect(url).toBe(`/admin/request-traces/exports/${TASK_ID}/download`);
    expect(config.params).toEqual({ part: 2 });
    expect(result).toEqual({
      outcome: "ready",
      blob,
      fileName: `sub2api-request-trace-export-${TASK_ID}-part0002.jsonl`,
    });
  });

  it("refuses a shard index the server route cannot own", async () => {
    await expect(downloadTraceExportPart(TASK_ID, 0)).rejects.toThrow();
    await expect(downloadTraceExportPart(TASK_ID, 1001)).rejects.toThrow();
    await expect(downloadTraceExportPart("../exports", 1)).rejects.toThrow();
    expect(client.get).not.toHaveBeenCalled();
  });

  it("never follows a hostile filename out of the server header", async () => {
    const blob = new Blob(["SYNTHETIC"]);
    client.get.mockResolvedValue({
      status: 200,
      data: blob,
      headers: {
        "content-disposition":
          'attachment; filename="../../etc/passwd"\r\nX-Injected: 1',
      },
    });
    const result = await downloadTraceExport(TASK_ID);

    expect(result).toEqual({
      outcome: "ready",
      blob,
      fileName: ".._.._etc_passwd",
    });
  });

  it("falls back to a server-shaped name when the header carries nothing usable", async () => {
    const blob = new Blob(["SYNTHETIC"]);
    client.get.mockResolvedValue({ status: 200, data: blob, headers: {} });

    await expect(downloadTraceExport(TASK_ID)).resolves.toMatchObject({
      fileName: `sub2api-request-trace-export-${TASK_ID}.manifest.json`,
    });
    await expect(downloadTraceExportPart(TASK_ID, 2)).resolves.toMatchObject({
      fileName: `sub2api-request-trace-export-${TASK_ID}-part0002.jsonl`,
    });
  });

  it("reports a lost temporary file only from the server 410 file-lost reason", async () => {
    client.get.mockResolvedValue(withReason("REQUEST_TRACE_EXPORT_FILE_LOST"));
    await expect(downloadTraceExport(TASK_ID)).resolves.toEqual({
      outcome: "file_lost",
    });
    await expect(downloadTraceExportPart(TASK_ID, 1)).resolves.toEqual({
      outcome: "file_lost",
    });
  });

  it("does not claim a lost file when the 410 means the download window closed", async () => {
    client.get.mockResolvedValue(
      withReason("REQUEST_TRACE_EXPORT_DOWNLOAD_EXPIRED"),
    );
    await expect(downloadTraceExport(TASK_ID)).resolves.toEqual({
      outcome: "expired",
    });
    await expect(downloadTraceExportPart(TASK_ID, 1)).resolves.toEqual({
      outcome: "expired",
    });
  });

  it("does not claim a lost file from an unrecognized 410 reason", async () => {
    client.get.mockResolvedValue(
      withReason("REQUEST_TRACE_EXPORT_SOMETHING_ELSE"),
    );
    await expect(downloadTraceExport(TASK_ID)).resolves.toEqual({
      outcome: "unavailable",
    });
  });

  it("distinguishes a task that is not ready yet from one that expired", async () => {
    // 409 是"任务还没产出文件"，重试即可；报成 expired 会让轮询中的任务像是已失败。
    client.get.mockResolvedValue({
      status: 409,
      data: JSON.stringify({ reason: "REQUEST_TRACE_EXPORT_NOT_READY" }),
      headers: {},
    });
    await expect(downloadTraceExport(TASK_ID)).resolves.toEqual({
      outcome: "not_ready",
    });
    await expect(downloadTraceExportPart(TASK_ID, 1)).resolves.toEqual({
      outcome: "not_ready",
    });
  });

  it("reports a refused or failed download without pretending the file arrived", async () => {
    client.get.mockRejectedValue({ status: 403, message: "forbidden" });
    await expect(downloadTraceExport(TASK_ID)).resolves.toEqual({
      outcome: "unavailable",
    });
  });
});

describe("Trace export settings API", () => {
  beforeEach(() => Object.values(client).forEach((mock) => mock.mockReset()));

  const risk = {
    acknowledged: false,
    version: "v2026.09.30",
    phrase_en: "EN statement",
    phrase_zh: "中文声明",
  };

  it("reads the risk statement without carrying any credential or operator address", async () => {
    client.get.mockResolvedValue({
      data: {
        ...risk,
        ip_address: "203.0.113.9",
        user_agent: "CANARY_UA",
        admin_api_key: "sk-CANARY",
      },
    });
    const status = await getTraceExportRisk();

    const [url, config] = client.get.mock.calls[0] as [
      string,
      { headers?: Record<string, string> },
    ];
    expect(url).toBe("/admin/settings/request-trace/export-risk");
    expect(config.headers).toEqual(
      expect.objectContaining({
        "Cache-Control": "no-store",
        Pragma: "no-cache",
      }),
    );
    expect(JSON.stringify(status)).not.toContain("203.0.113.9");
    expect(JSON.stringify(status)).not.toContain("CANARY_UA");
    expect(JSON.stringify(status)).not.toContain("sk-CANARY");
    expect(status.acknowledged).toBe(false);
    expect(status.phrase).toBeNull();
    expect(status.accepted_at).toBeNull();
  });

  it("keeps an acknowledgement record only as bounded facts", async () => {
    client.get.mockResolvedValue({
      data: {
        ...risk,
        acknowledged: true,
        phrase: risk.phrase_en,
        admin_user_id: 3,
        accepted_at: "2026-09-30T00:00:00Z",
      },
    });
    const status = await getTraceExportRisk();

    expect(status).toMatchObject({
      acknowledged: true,
      admin_user_id: 3,
      accepted_at: "2026-09-30T00:00:00Z",
    });
  });

  it("submits the typed statement and nothing else", async () => {
    client.post.mockResolvedValue({ data: { ...risk, acknowledged: true } });
    await acknowledgeTraceExportRisk({
      language: "zh",
      phrase: risk.phrase_zh,
    });

    expect(client.post).toHaveBeenCalledWith(
      "/admin/settings/request-trace/export-risk",
      { language: "zh", phrase: risk.phrase_zh },
      expect.objectContaining({
        headers: expect.objectContaining({ "Cache-Control": "no-store" }),
      }),
    );
  });

  it("refuses a risk state that is not a state rather than rendering it", async () => {
    client.get.mockResolvedValue({ data: { ...risk, acknowledged: "yes" } });
    await expect(getTraceExportRisk()).rejects.toThrow();
  });

  it("reads and writes the five caps, always as one complete set", async () => {
    const stored = {
      max_rows: 500,
      max_bytes: 2_000_000,
      max_runtime_seconds: 60,
      max_shard_rows: 100,
      max_shard_bytes: 1_500_000,
      configured: true,
    };
    client.get.mockResolvedValue({ data: stored });
    await expect(getTraceExportLimits()).resolves.toEqual(stored);
    expect(client.get).toHaveBeenCalledWith(
      "/admin/settings/request-trace/export-limits",
      expect.objectContaining({
        headers: expect.objectContaining({ Pragma: "no-cache" }),
      }),
    );

    client.put.mockResolvedValue({ data: { ...stored, max_rows: 5000 } });
    await expect(updateTraceExportLimits(stored)).resolves.toEqual({
      ...stored,
      max_rows: 5000,
    });
    // `configured` is the server's own provenance flag, not something a client asserts.
    expect(client.put).toHaveBeenCalledWith(
      "/admin/settings/request-trace/export-limits",
      {
        max_rows: 500,
        max_bytes: 2_000_000,
        max_runtime_seconds: 60,
        max_shard_rows: 100,
        max_shard_bytes: 1_500_000,
      },
      expect.objectContaining({
        headers: expect.objectContaining({ "Cache-Control": "no-store" }),
      }),
    );
  });

  it("refuses caps outside the range the server clamps to instead of passing them on", async () => {
    client.get.mockResolvedValue({
      data: {
        max_rows: 10,
        max_bytes: 2_000_000,
        max_runtime_seconds: 60,
        max_shard_rows: 100,
        max_shard_bytes: 1_500_000,
        configured: true,
      },
    });
    await expect(getTraceExportLimits()).rejects.toThrow();

    client.get.mockResolvedValue({
      data: {
        max_rows: 500,
        max_bytes: 2_000_000,
        max_runtime_seconds: 60,
        max_shard_rows: 100,
        max_shard_bytes: 1_500_000,
        configured: "yes",
      },
    });
    await expect(getTraceExportLimits()).rejects.toThrow();
  });
});

function exportParams(): Record<string, unknown> {
  const config = client.post.mock.calls[0]?.[2] as
    { params?: Record<string, unknown> } | undefined;
  return config?.params ?? {};
}
