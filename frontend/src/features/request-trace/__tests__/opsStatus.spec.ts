/**
 * Value-free operational status: the wire contract, its parser and the labels
 * for its closed sets.
 *
 * `GET /api/v1/admin/request-traces/status` answers with counts, booleans and
 * closed-set enums only. There is no enablement flag in the payload, so nothing
 * here may be read as "capture is on"; and no database message, body, header
 * value, raw query or filename may survive the parser.
 */
import { beforeEach, describe, expect, it, vi } from "vitest";

const client = vi.hoisted(() => ({ get: vi.fn() }));
vi.mock("@/api/client", () => ({ apiClient: client }));

import { getTraceOpsStatus } from "../api";
import { requestTraceEn, requestTraceZh } from "../locale";
import {
  normalizeRequestTraceOpsStatus,
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

const exportCounters = (patch: Raw = {}): Raw => ({
  worker_started: true,
  ticks: 9,
  tasks_run: 5,
  tasks_completed: 4,
  tasks_failed: 1,
  failures: 2,
  disabled_ticks: 0,
  cleanups: 1,
  cleaned_files: 2,
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

const raw = (patch: Raw = {}): Raw => ({
  storage_probe: "reachable",
  capture: capture(),
  export: exportCounters(),
  cleanup: cleanup(),
  ...patch,
});

describe("request Trace operational status API", () => {
  beforeEach(() => client.get.mockReset());

  it("reads the static sibling route with no filter, no query and no cached copy", async () => {
    client.get.mockResolvedValue({ data: raw() });
    const status = await getTraceOpsStatus();

    const [url, config] = client.get.mock.calls[0] as [
      string,
      { headers?: Record<string, string>; params?: unknown },
    ];
    expect(url).toBe("/admin/request-traces/status");
    // The status route is a static sibling of /:trace_id: it never addresses a trace.
    expect(url).not.toMatch(/[0-9a-f]{32}/);
    expect(config.params).toBeUndefined();
    expect(config.headers).toEqual(
      expect.objectContaining({
        "Cache-Control": "no-store",
        Pragma: "no-cache",
      }),
    );
    expect(status.capture.accepted).toBe(41);
    expect(status.cleanup.backlog_state).toBe("measured");
  });

  it("keeps only the allowlisted counts and closed sets, never a stored message or a file name", async () => {
    client.get.mockResolvedValue({
      data: raw({
        error: 'pq: password authentication failed for user "sub2api"',
        message: 'relation "request_traces" does not exist',
        body: "BODY_CANARY",
        request_headers: { authorization: "Bearer CANARY" },
        query: "SELECT count(*) FROM request_traces",
        filename: "sub2api-request-trace-export-abc.jsonl",
        download_url: "https://public.example/leak",
      }),
    });
    const status = await getTraceOpsStatus();

    const serialized = JSON.stringify(status);
    for (const canary of [
      "password authentication",
      "does not exist",
      "BODY_CANARY",
      "Bearer CANARY",
      "SELECT count(*)",
      "request-traces-export",
      "public.example",
    ]) {
      expect(
        serialized,
        `the status view must not carry ${canary}`,
      ).not.toContain(canary);
    }
    expect(Object.keys(status).sort()).toEqual([
      "capture",
      "cleanup",
      "export",
      "storage_probe",
    ]);
  });

  it("rejects an unknown enum instead of rendering it as an observed state", async () => {
    const cases: Raw[] = [
      raw({ storage_probe: "probably_fine" }),
      raw({ capture: capture({ storage: "healthy" }) }),
      raw({ cleanup: cleanup({ backlog_state: "capped" }) }),
    ];
    for (const payload of cases) {
      client.get.mockResolvedValue({ data: payload });
      await expect(getTraceOpsStatus()).rejects.toThrow();
    }
  });

  it("rejects malformed, negative and unsafe counts instead of claiming a number", async () => {
    const cases: Raw[] = [
      raw({ capture: capture({ queue_depth: -1 }) }),
      raw({ capture: capture({ accepted: 1.5 }) }),
      raw({ capture: capture({ dropped: "3" }) }),
      raw({ capture: capture({ write_failed: Number.MAX_SAFE_INTEGER + 1 }) }),
      raw({ export: exportCounters({ ticks: -9 }) }),
      raw({ cleanup: cleanup({ unlinked_backlog: null }) }),
      raw({ cleanup: cleanup({ backlog_limit: -1000 }) }),
    ];
    for (const payload of cases) {
      client.get.mockResolvedValue({ data: payload });
      await expect(getTraceOpsStatus()).rejects.toThrow();
    }
  });

  it("rejects a non-boolean flag and a truncated record rather than filling in a default", async () => {
    const cases: Raw[] = [
      raw({ capture: capture({ repository_available: "true" }) }),
      raw({ capture: capture({ stopped: 0 }) }),
      raw({ export: exportCounters({ worker_started: "yes" }) }),
      { storage_probe: "reachable" },
      raw({ capture: null }),
      [],
    ];
    for (const payload of cases) {
      client.get.mockResolvedValue({ data: payload });
      await expect(getTraceOpsStatus()).rejects.toThrow();
    }
  });

  it("reads the reachable, unavailable and not-probed probe states as themselves", () => {
    for (const probe of requestTraceStorageProbeStates) {
      expect(
        normalizeRequestTraceOpsStatus(raw({ storage_probe: probe }))
          .storage_probe,
      ).toBe(probe);
    }
  });
});

type LocaleValue = Record<string, unknown>;

function flattenLeafKeys(value: unknown, prefix = ""): string[] {
  if (value === null || typeof value !== "object" || Array.isArray(value))
    return prefix ? [prefix] : [];
  return Object.entries(value as LocaleValue).flatMap(([key, child]) => {
    const path = prefix ? `${prefix}.${key}` : key;
    return flattenLeafKeys(child, path);
  });
}

function readLeaf(messages: unknown, key: string): unknown {
  return key.split(".").reduce<unknown>((current, segment) => {
    if (current === null || typeof current !== "object") return undefined;
    return (current as LocaleValue)[segment];
  }, messages);
}

/**
 * Every closed set the operational status renders through: the map path, the
 * wire values it must cover, and the fallback for a value outside the set.
 */
const opsLabelGroups: [string, readonly string[], string][] = [
  ["requestTrace.ops.storageProbe", requestTraceStorageProbeStates, "unknown"],
  ["requestTrace.ops.capture.storage", requestTraceStorageStates, "unknown"],
  [
    "requestTrace.ops.cleanup.backlogState",
    requestTraceBacklogStates,
    "unknown",
  ],
];

describe("request Trace operational status locale", () => {
  it("keeps English and Chinese schemas identical", () => {
    const enKeys = flattenLeafKeys(requestTraceEn).sort();
    const zhKeys = flattenLeafKeys(requestTraceZh).sort();
    expect(enKeys.filter((key) => !zhKeys.includes(key))).toEqual([]);
    expect(zhKeys.filter((key) => !enKeys.includes(key))).toEqual([]);
  });

  it.each(opsLabelGroups)(
    "labels every value of %s the backend can send",
    (path, values, fallback) => {
      for (const [locale, messages] of Object.entries({
        en: requestTraceEn,
        zh: requestTraceZh,
      })) {
        expect(
          flattenLeafKeys(readLeaf(messages, path) ?? {}).sort(),
          `${locale} ${path}`,
        ).toEqual([...values, fallback].sort());
      }
    },
  );

  it.each(opsLabelGroups)(
    "labels each %s value as a distinct message rather than a raw token",
    (path, values, fallback) => {
      for (const [locale, messages] of Object.entries({
        en: requestTraceEn,
        zh: requestTraceZh,
      })) {
        const keys = [...values, fallback];
        const labels = keys.map((key) => readLeaf(messages, `${path}.${key}`));
        for (const [index, label] of labels.entries()) {
          expect(typeof label, `${locale} ${path}.${keys[index]}`).toBe(
            "string",
          );
          expect(
            (label as string).trim(),
            `${locale} ${path}.${keys[index]}`,
          ).not.toBe("");
          expect(
            label,
            `${locale} ${path}.${keys[index]} must not echo the wire value`,
          ).not.toBe(keys[index]);
        }
        expect(
          new Set(labels).size,
          `${locale} ${path} has repeated labels`,
        ).toBe(keys.length);
      }
    },
  );
});
