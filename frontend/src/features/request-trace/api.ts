import { apiClient } from "@/api/client";
import {
  normalizeRequestTraceOperatorStatus,
  normalizeRequestTracePage,
  normalizeRequestTraceDetail,
  normalizeRequestTraceExportLimits,
  normalizeRequestTraceExportRisk,
  normalizeRequestTraceExportTask,
  normalizeRequestTraceOpsStatus,
  requestTraceExportIDPattern,
  requestTraceExportMaxSelectedTraces,
  requestTraceExportMaxShards,
  type RequestTraceOperatorStatus,
  type RequestTraceOperatorUpdateInput,
  type RequestTraceListParams,
  type RequestTracePage,
  type RequestTraceDetail,
  type RequestTraceExportFilter,
  type RequestTraceExportLimits,
  type RequestTraceExportRefusal,
  type RequestTraceExportRisk,
  type RequestTraceExportTask,
  type RequestTraceOpsStatus,
  type TraceAckLanguage,
  type RequestTraceDeletePreview,
  type RequestTraceDeleteResult,
} from "./types";

const path = "/admin/settings/request-trace";
const tracePath = "/admin/request-traces";
const exportPath = "/admin/request-traces/exports";
const exportRiskPath = "/admin/settings/request-trace/export-risk";
const exportLimitsPath = "/admin/settings/request-trace/export-limits";
const headers = { "Cache-Control": "no-store", Pragma: "no-cache" };

export async function listTraces(
  params: RequestTraceListParams,
  options?: { signal?: AbortSignal },
): Promise<RequestTracePage> {
  const { data } = await apiClient.get<unknown>(tracePath, {
    params: { ...params, include_stats: true },
    headers,
    signal: options?.signal,
  });
  return normalizeRequestTracePage(data);
}

export async function getTrace(
  traceID: string,
  options?: { signal?: AbortSignal },
): Promise<RequestTraceDetail> {
  if (!/^[0-9a-f]{32}$/.test(traceID))
    throw new Error("Invalid request Trace ID");
  const { data } = await apiClient.get<unknown>(
    `${tracePath}/${encodeURIComponent(traceID)}`,
    { headers, signal: options?.signal },
  );
  return normalizeRequestTraceDetail(data);
}

export async function getTraceModelCandidates(): Promise<string[]> {
  const { data } = await apiClient.get<{ models: unknown }>(
    `${path}/model-candidates`,
    { headers },
  );
  if (
    !Array.isArray(data.models) ||
    data.models.some(
      (value) =>
        typeof value !== "string" || !value.trim() || value.length > 128,
    )
  ) {
    throw new Error("Trace model candidates are unavailable");
  }
  return [...new Set(data.models as string[])];
}

export async function getOperatorSettings(): Promise<RequestTraceOperatorStatus> {
  const { data } = await apiClient.get<unknown>(path, { headers });
  return normalizeRequestTraceOperatorStatus(data);
}

/**
 * Reads the value-free operational status. The route is a static sibling of
 * `/request-traces/:trace_id`, so it takes no trace id, no filter and no query
 * string at all; the answer is counts and closed sets, and it never says
 * whether the capture gate is enabled.
 */
export async function getTraceOpsStatus(options?: {
  signal?: AbortSignal;
}): Promise<RequestTraceOpsStatus> {
  const { data } = await apiClient.get<unknown>(`${tracePath}/status`, {
    headers,
    signal: options?.signal,
  });
  return normalizeRequestTraceOpsStatus(data);
}

/**
 * Writes the switch and, when the operator actually changed it, the capture
 * scope. `scope_provided` is the server's signal to replace the stored scope
 * rather than keep it, so it is sent as `true` only with a **complete** scope
 * object; a switch-only update never carries scope fields at all. Enabling still
 * requires the written acknowledgement, exactly as before.
 */
export async function updateOperatorSettings(
  input: RequestTraceOperatorUpdateInput,
): Promise<RequestTraceOperatorStatus> {
  const body: Record<string, unknown> = {
    enabled: input.enabled,
    language: input.language,
    phrase: input.phrase,
    scope_provided: input.scope_provided === true,
  };
  for (const key of [
    "capture_body",
    "capture_http_200",
    "sample_rate_http_200",
    "sample_rate_other",
    "body_max_bytes",
    "capture_duration_seconds",
    "renew_capture_window",
  ] as const) {
    if (input[key] !== undefined) body[key] = input[key];
  }
  const payload =
    input.scope_provided === true
      ? {
          ...body,
          all_groups: input.all_groups,
          group_ids: input.group_ids,
          model_scope: input.model_scope,
          models: input.models,
          platform_scope: input.platform_scope,
          platforms: input.platforms,
        }
      : body;
  const { data } = await apiClient.put<unknown>(path, payload, { headers });
  return normalizeRequestTraceOperatorStatus(data);
}

/**
 * The only filter keys an export may carry in the query string: the same
 * metadata question the list answers. An allowlist — not a spread — so a draft
 * field, a body field or a future key on the caller's object can never reach the
 * query string. The "export selected" id set is deliberately absent: it is a
 * bounded structured body, never a query parameter.
 */
const exportFilterKeys = [
  "trace_id",
  "route_family",
  "client_status",
  "usage_linked",
  "created_from",
  "created_to",
  "usage_log_id",
  "account_id",
  "group_id",
  "group_unknown",
  "requested_model",
  "model_unknown",
  "platform",
  "platform_unknown",
  "user_id",
  "user_unknown",
  "api_key_id",
  "api_key_unknown",
  "q",
] as const;

export async function previewTraceDelete(
  filter: RequestTraceExportFilter,
): Promise<RequestTraceDeletePreview> {
  const normalizedFilter = exportQueryParams(filter);
  if (Object.keys(normalizedFilter).length === 0)
    throw new Error("Trace cleanup requires an executed filter");
  const { data } = await apiClient.post<RequestTraceDeletePreview>(
    `${tracePath}/delete-preview`,
    { filter: normalizedFilter },
    { headers },
  );
  if (
    !Number.isSafeInteger(data.matched_count) ||
    data.matched_count < 0 ||
    !Number.isSafeInteger(data.snapshot_max_id) ||
    data.snapshot_max_id < 0 ||
    typeof data.filter_hash !== "string" ||
    !/^[a-f0-9]{64}$/.test(data.filter_hash) ||
    typeof data.confirmation_token !== "string" ||
    !data.confirmation_token ||
    !Number.isFinite(Date.parse(data.expires_at))
  ) {
    throw new Error("Trace cleanup preview is unavailable");
  }
  return data;
}

function deleteResult(
  data: RequestTraceDeleteResult,
): RequestTraceDeleteResult {
  if (
    !Number.isSafeInteger(data.deleted_count) ||
    data.deleted_count < 0 ||
    typeof data.completed !== "boolean"
  ) {
    throw new Error("Trace cleanup result is unavailable");
  }
  return { deleted_count: data.deleted_count, completed: data.completed };
}

export async function deleteSelectedTraces(
  traceIDs: string[],
): Promise<RequestTraceDeleteResult> {
  const { data } = await apiClient.post<RequestTraceDeleteResult>(
    `${tracePath}/batch-delete`,
    {
      trace_ids: boundedTraceSelection(traceIDs),
      confirm: true,
    },
    { headers },
  );
  return deleteResult(data);
}

export async function deleteTracesByFilter(
  filter: RequestTraceExportFilter,
  preview: RequestTraceDeletePreview,
): Promise<RequestTraceDeleteResult> {
  const normalizedFilter = exportQueryParams(filter);
  if (Object.keys(normalizedFilter).length === 0)
    throw new Error("Trace cleanup requires an executed filter");
  const { data } = await apiClient.post<RequestTraceDeleteResult>(
    `${tracePath}/delete-by-filter`,
    {
      filter: normalizedFilter,
      snapshot_max_id: preview.snapshot_max_id,
      filter_hash: preview.filter_hash,
      confirmation_token: preview.confirmation_token,
      confirm: true,
    },
    { headers },
  );
  return deleteResult(data);
}

/**
 * The server's bounded reasons, mapped onto the outcomes the UI can explain.
 * Anything unknown, and any transport failure, is "unavailable" — the server's
 * message text is never read, carried or rendered.
 */
const refusalByReason: Record<string, RequestTraceExportRefusal> = {
  REQUEST_TRACE_EXPORT_RISK_ACK_REQUIRED: "risk_ack_required",
  REQUEST_TRACE_EXPORT_ADMIN_SESSION_REQUIRED: "session_required",
  REQUEST_TRACE_EXPORT_FORBIDDEN: "session_required",
  REQUEST_TRACE_EXPORT_DISABLED: "disabled",
  REQUEST_TRACE_EXPORT_CAPACITY_LIMIT: "capacity",
  REQUEST_TRACE_EXPORT_INVALID_FILTER: "invalid_filter",
  // A cursor this client could not have sent (it only ever echoes the server's
  // own token) is still an unreadable request, not "no more tasks".
  REQUEST_TRACE_EXPORT_INVALID_CURSOR: "invalid_filter",
};

/** A refused export action, carrying only a bounded outcome — never server text. */
export class TraceExportRefusedError extends Error {
  readonly refusal: RequestTraceExportRefusal;

  constructor(refusal: RequestTraceExportRefusal) {
    super(refusal);
    this.name = "TraceExportRefusedError";
    this.refusal = refusal;
  }
}

/** Reads the bounded reason of a refused call; an unreadable one is not a verdict. */
function refusalOf(error: unknown): RequestTraceExportRefusal {
  const reason = (error as { reason?: unknown } | null)?.reason;
  if (typeof reason !== "string") return "unavailable";
  return refusalByReason[reason] ?? "unavailable";
}

/** The query-string scope: every list condition the query used, and nothing else. */
function exportQueryParams(
  filter: RequestTraceExportFilter,
): Record<string, unknown> {
  const params: Record<string, unknown> = {};
  for (const key of exportFilterKeys) {
    const value = filter[key];
    if (value === undefined || value === null || value === "") continue;
    params[key] = value;
  }
  return params;
}

/**
 * The bounded "export selected" set. It is an explicit id set or nothing: an
 * empty one is not "export everything", and a malformed or repeated id means the
 * checked set cannot be described honestly, so the call is refused with the
 * server's own invalid-filter outcome instead of starting a task over a set the
 * operator did not choose. The bound mirrors `service.RequestTraceExportMaxSelectedIDs`.
 */
function boundedTraceSelection(traceIDs: string[]): string[] {
  if (traceIDs.length === 0)
    throw new TraceExportRefusedError("invalid_filter");
  if (traceIDs.length > requestTraceExportMaxSelectedTraces)
    throw new TraceExportRefusedError("selection_too_large");
  const seen = new Set<string>();
  for (const traceID of traceIDs) {
    if (!requestTraceExportIDPattern.test(traceID) || seen.has(traceID))
      throw new TraceExportRefusedError("invalid_filter");
    seen.add(traceID);
  }
  return traceIDs;
}

/**
 * Creates a bounded background export. The scope is exactly one of two mutually
 * exclusive shapes; a request carrying both is refused by the server:
 *
 * - "Export the query": the bounded metadata filter rides in the query string and
 *   the request carries no body at all, so a body or a full-text search cannot be
 *   expressed here.
 * - "Export selected": the explicitly checked Trace IDs ride in a bounded
 *   structured JSON body. They never go into the URL — a checked set is not
 *   truncated by a query-length limit, a proxy or a log — and no metadata filter
 *   is sent beside them, so a checked record is checked against the query the
 *   operator actually ran and the two scopes never blur into one.
 *
 * Only the originating admin session may later read or download the task; the
 * server enforces that.
 */
export async function createTraceExport(
  filter: RequestTraceExportFilter,
  options?: { signal?: AbortSignal },
): Promise<RequestTraceExportTask> {
  // The checked set is settled before the request, so a local refusal can never
  // be re-labelled as a transport failure by the mapping below.
  const selected =
    filter.trace_ids == null ? null : boundedTraceSelection(filter.trace_ids);
  let data: unknown;
  try {
    if (selected !== null) {
      ({ data } = await apiClient.post<unknown>(
        exportPath,
        { trace_ids: selected },
        { headers, signal: options?.signal },
      ));
    } else {
      ({ data } = await apiClient.post<unknown>(exportPath, null, {
        params: exportQueryParams(filter),
        headers,
        signal: options?.signal,
      }));
    }
  } catch (error) {
    // A cancellation is not a refusal: it must stay recognisable to the caller.
    // A transport or protocol failure is one, and says nothing about why.
    if (options?.signal?.aborted) throw error;
    throw new TraceExportRefusedError(refusalOf(error));
  }
  // A malformed task view is a protocol failure, not a refusal: it must not be
  // reported as "the server said no".
  return normalizeRequestTraceExportTask(data);
}

export async function getTraceExport(
  exportID: string,
  options?: { signal?: AbortSignal },
): Promise<RequestTraceExportTask> {
  if (!requestTraceExportIDPattern.test(exportID))
    throw new Error("Invalid request Trace export ID");
  const { data } = await apiClient.get<unknown>(`${exportPath}/${exportID}`, {
    headers,
    signal: options?.signal,
  });
  return normalizeRequestTraceExportTask(data);
}

/**
 * One recall page is at most this many tasks. The server owns the bound and
 * refuses more rather than truncating; this mirror only keeps a hostile or
 * broken payload from being rendered as a longer list than one page can hold.
 */
export const requestTraceExportRecallLimit = 100;

/**
 * Mirrors the server's opaque-cursor length bound. A longer string is not a
 * token this server minted, so it is treated as an unreadable page rather than
 * followed.
 */
export const requestTraceExportRecallCursorLimit = 128;

/**
 * One page of the session's export tasks, plus the token for the page after it.
 *
 * `nextCursor` is `null` when the server says there is no further page: that is
 * the "no more" answer itself, not an absent field. Keeping the two apart is what
 * stops "the list ends here" from being confused with "this response could not be
 * read" — the second is an error, and must never be shown as a shorter list.
 */
export type TraceExportRecallPage = {
  items: RequestTraceExportTask[];
  nextCursor: string | null;
};

/**
 * Normalizes the bounded recall page. The shape is one `items` array plus one
 * `next_cursor`. An unreadable, over-long, or cursor-less payload is a protocol
 * failure, not an empty list: it must not be rendered as "you have no tasks",
 * and it must not silently end the walk either.
 */
function normalizeTraceExportRecallPage(value: unknown): TraceExportRecallPage {
  if (value === null || typeof value !== "object" || Array.isArray(value))
    throw new Error("Invalid request Trace export list");
  const payload = value as { items?: unknown; next_cursor?: unknown };
  if (!("next_cursor" in payload))
    throw new Error("Invalid request Trace export list");
  const { items, next_cursor: nextCursor } = payload;
  if (!Array.isArray(items) || items.length > requestTraceExportRecallLimit)
    throw new Error("Invalid request Trace export list");
  if (
    nextCursor !== null &&
    (typeof nextCursor !== "string" ||
      nextCursor.length === 0 ||
      nextCursor.length > requestTraceExportRecallCursorLimit)
  ) {
    throw new Error("Invalid request Trace export list");
  }
  return { items: items.map(normalizeRequestTraceExportTask), nextCursor };
}

/**
 * Reads back one page of the export tasks of **this admin login session**,
 * newest first. `cursor` is the opaque token the server handed out for the
 * previous page; omitting it asks for the first page, and the token is only ever
 * passed back, never parsed or stored.
 *
 * The session is the credential: the server filters on the authenticated admin
 * plus the session it derived from the session itself, so this call takes no
 * task id, no filter and no session handle. Leaving or refreshing the page does
 * not lose the tasks — a different session cannot list them, and an admin API
 * key is refused outright. A refused call says why through the same bounded
 * outcomes as the other export calls.
 */
export async function listTraceExports(options?: {
  signal?: AbortSignal;
  cursor?: string | null;
  limit?: number;
}): Promise<TraceExportRecallPage> {
  const params: Record<string, string | number> = {};
  if (options?.cursor) params.cursor = options.cursor;
  if (options?.limit !== undefined) params.limit = options.limit;
  // No scope of its own: an empty query string means "the first page", exactly
  // as an empty parameter object did before.
  const query = Object.keys(params).length > 0 ? { params } : {};
  let data: unknown;
  try {
    ({ data } = await apiClient.get<unknown>(exportPath, {
      ...query,
      headers,
      signal: options?.signal,
    }));
  } catch (error) {
    // Cancellation stays recognisable; anything else is a bounded refusal.
    if (options?.signal?.aborted) throw error;
    throw new TraceExportRefusedError(refusalOf(error));
  }
  return normalizeTraceExportRecallPage(data);
}

/** The server refuses a download with 410 and one of these two bounded reasons. */
const exportFileLostReason = "REQUEST_TRACE_EXPORT_FILE_LOST";
const exportWindowClosedReason = "REQUEST_TRACE_EXPORT_DOWNLOAD_EXPIRED";
// 409：任务还没产出文件。轮询重试即可，不能说成"已过期"或"文件丢失"。
const exportNotReadyReason = "REQUEST_TRACE_EXPORT_NOT_READY";
const unsafeFileNamePattern = /[^A-Za-z0-9._-]/g;

export type TraceExportDownloadResult =
  | { outcome: "ready"; blob: Blob; fileName: string }
  // not_ready：任务尚未产出文件（服务端 409）。它与 expired/file_lost 不同，重试即可。
  | { outcome: "file_lost" | "expired" | "not_ready" | "unavailable" };

/** Uses only the server-provided basename; anything hostile degrades to the server's own naming. */
function exportFileName(disposition: unknown, fallback: string): string {
  const match =
    typeof disposition === "string"
      ? /filename\*?=(?:UTF-8'')?"?([^";]+)"?/i.exec(disposition)
      : null;
  const candidate =
    match?.[1]?.trim().replace(unsafeFileNamePattern, "_") ?? "";
  return candidate && candidate !== "." && candidate !== ".."
    ? candidate
    : fallback;
}

/** Reads a refused download body as text, whatever the transport handed back. */
async function exportRefusalBody(data: unknown): Promise<string> {
  if (typeof data === "string") return data;
  if (data === null || typeof data !== "object") return "";
  const blob = data as { text?: () => Promise<string> };
  if (typeof blob.text === "function") {
    try {
      return await blob.text();
    } catch {
      return "";
    }
  }
  // Older engines (and the test DOM) expose Blob without text().
  return await new Promise<string>((resolve) => {
    try {
      const reader = new FileReader();
      reader.onload = () =>
        resolve(typeof reader.result === "string" ? reader.result : "");
      reader.onerror = () => resolve("");
      reader.readAsText(data as Blob);
    } catch {
      resolve("");
    }
  });
}

/** Reads the bounded reason of a refused download; a blob or JSON body is parsed defensively. */
async function exportRefusalReason(data: unknown): Promise<string> {
  try {
    const parsed: unknown = JSON.parse(await exportRefusalBody(data));
    const reason = (parsed as { reason?: unknown } | null)?.reason;
    return typeof reason === "string" ? reason : "";
  } catch {
    return "";
  }
}

/**
 * Streams one of a task's own files. Refusals are results, not exceptions:
 * "file_lost" is only ever reported from the server's file-lost reason, never
 * inferred from a status code, a clock or a missing field.
 */
async function downloadExportFile(
  url: string,
  fallbackName: string,
  params: Record<string, unknown> | undefined,
  options?: { signal?: AbortSignal },
): Promise<TraceExportDownloadResult> {
  const response = await apiClient
    .get<Blob>(url, {
      params,
      headers,
      responseType: "blob",
      signal: options?.signal,
      // 410/409 都是关于这个文件的封闭结论，不是传输失败：读它们的 reason。
      validateStatus: (status) =>
        status === 200 || status === 409 || status === 410,
    })
    .catch(() => null);
  if (response === null) return { outcome: "unavailable" };
  if (response.status !== 200) {
    const reason = await exportRefusalReason(response.data);
    if (reason === exportFileLostReason) return { outcome: "file_lost" };
    if (reason === exportWindowClosedReason) return { outcome: "expired" };
    if (reason === exportNotReadyReason) return { outcome: "not_ready" };
    return { outcome: "unavailable" };
  }
  const disposition = response.headers?.["content-disposition"];
  return {
    outcome: "ready",
    blob: response.data,
    fileName: exportFileName(disposition, fallbackName),
  };
}

/**
 * Downloads the manifest: what this export covered, which shards it wrote and
 * whether it is complete. The manifest is the only place the shard list is
 * stated, so it stays downloadable exactly as long as the shards do.
 */
export async function downloadTraceExport(
  exportID: string,
  options?: { signal?: AbortSignal },
): Promise<TraceExportDownloadResult> {
  if (!requestTraceExportIDPattern.test(exportID))
    throw new Error("Invalid request Trace export ID");
  return downloadExportFile(
    `${exportPath}/${exportID}/download`,
    `sub2api-request-trace-export-${exportID}.manifest.json`,
    undefined,
    options,
  );
}

/**
 * Downloads shard `part` (1-based) of the same task. The server refuses an
 * out-of-range index, so the index is checked against the server's own bound
 * before the request rather than sent hopefully.
 */
export async function downloadTraceExportPart(
  exportID: string,
  part: number,
  options?: { signal?: AbortSignal },
): Promise<TraceExportDownloadResult> {
  if (!requestTraceExportIDPattern.test(exportID))
    throw new Error("Invalid request Trace export ID");
  if (
    !Number.isSafeInteger(part) ||
    part < 1 ||
    part > requestTraceExportMaxShards
  )
    throw new Error("Invalid request Trace export part");
  return downloadExportFile(
    `${exportPath}/${exportID}/download`,
    `sub2api-request-trace-export-${exportID}-part${String(part).padStart(4, "0")}.jsonl`,
    { part },
    options,
  );
}

/**
 * Reads whether this deployment's export risk statement has been accepted. The
 * statement text itself comes from here: the form shows the server's own words,
 * never a copy maintained in the frontend.
 */
export async function getTraceExportRisk(options?: {
  signal?: AbortSignal;
}): Promise<RequestTraceExportRisk> {
  const { data } = await apiClient.get<unknown>(exportRiskPath, {
    headers,
    signal: options?.signal,
  });
  return normalizeRequestTraceExportRisk(data);
}

/**
 * Records the written acknowledgement. It is a separate statement from the
 * capture one, with its own version, so accepting it here never turns capture on.
 */
export async function acknowledgeTraceExportRisk(input: {
  language: TraceAckLanguage;
  phrase: string;
}): Promise<RequestTraceExportRisk> {
  const { data } = await apiClient.post<unknown>(
    exportRiskPath,
    { language: input.language, phrase: input.phrase },
    { headers },
  );
  return normalizeRequestTraceExportRisk(data);
}

/** Reads the caps a NEW task will snapshot: total rows/bytes/runtime and per shard. */
export async function getTraceExportLimits(options?: {
  signal?: AbortSignal;
}): Promise<RequestTraceExportLimits> {
  const { data } = await apiClient.get<unknown>(exportLimitsPath, {
    headers,
    signal: options?.signal,
  });
  return normalizeRequestTraceExportLimits(data);
}

/**
 * Saves the caps. Every field is sent, always: the server clamps a missing field
 * to the minimum, so a partial save would silently lower the other caps instead
 * of leaving them alone. The saved values are the ones tasks snapshot, so a
 * change never retro-applies to a task that already started.
 */
export async function updateTraceExportLimits(
  input: RequestTraceExportLimits,
): Promise<RequestTraceExportLimits> {
  const { data } = await apiClient.put<unknown>(
    exportLimitsPath,
    {
      max_rows: input.max_rows,
      max_bytes: input.max_bytes,
      max_runtime_seconds: input.max_runtime_seconds,
      max_shard_rows: input.max_shard_rows,
      max_shard_bytes: input.max_shard_bytes,
    },
    { headers },
  );
  return normalizeRequestTraceExportLimits(data);
}
