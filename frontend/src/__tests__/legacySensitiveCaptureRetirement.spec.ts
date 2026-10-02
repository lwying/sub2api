/**
 * Ticket 10 — the retired legacy sensitive-capture surfaces must be gone.
 *
 * The new request Trace is the only body-bearing admin capability. The legacy
 * value-detail / error-diagnostic capture, its operator switches, its reveal
 * endpoints and its pages are removed outright (not hidden), while the
 * metadata-only request audit that the forced-audit gate still needs stays
 * reachable.
 *
 * These assertions are deliberately about the shipped wiring — routes, navigation,
 * the imported API objects and the locale bundles — so a re-added button, route or
 * endpoint fails here instead of only in review.
 */
import { describe, expect, it } from "vitest";

import en from "@/i18n/locales/en";
import zh from "@/i18n/locales/zh";
import { adminUsageAPI } from "@/api/admin/usage";
import { settingsAPI } from "@/api/admin/settings";

const RETIRED_FEATURE_DIR = "features/error-diagnostics";
const LEGACY_ROUTE_PATH = "/admin/error-diagnostics";
const TRACE_ROUTE_PATH = "/admin/request-traces";

/** Raw sources of the whole frontend tree, keyed by their path relative to `src`. */
const sources: Record<string, string> = Object.fromEntries(
  Object.entries(
    import.meta.glob("../**/*.{ts,vue}", {
      query: "?raw",
      import: "default",
      eager: true,
    }) as Record<string, string>,
  ).map(([path, source]) => [path.replace(/^\.\.\//, ""), source]),
);

/** Production sources only: a retired-surface assertion must not be satisfied by a test file. */
const productionSources = Object.entries(sources).filter(
  ([path]) => !path.includes("__tests__") && !/\.(spec|test)\.ts$/.test(path),
);

function source(path: string): string {
  const value = sources[path];
  if (value === undefined) {
    throw new Error(`missing source for ${path}`);
  }
  return value;
}

function flattenKeys(value: unknown, prefix = ""): string[] {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    return prefix ? [prefix] : [];
  }
  return Object.entries(value as Record<string, unknown>).flatMap(
    ([key, child]) => flattenKeys(child, prefix ? `${prefix}.${key}` : key),
  );
}

describe("retired legacy capture surfaces", () => {
  it("no longer ships the error-diagnostics feature module", () => {
    const referencing = productionSources
      .filter(([, source]) => source.includes(RETIRED_FEATURE_DIR))
      .map(([path]) => path);
    expect(referencing).toEqual([]);
  });

  it("registers the Trace route and no legacy error-diagnostic route", () => {
    const router = source("router/index.ts");
    expect(router).toContain(TRACE_ROUTE_PATH);
    expect(router).not.toContain(LEGACY_ROUTE_PATH);
    expect(router).not.toContain("AdminErrorDiagnostics");
  });

  it("keeps only the Trace administration entry in the sidebar", () => {
    const sidebar = source("components/layout/AppSidebar.vue");
    expect(sidebar).toContain(TRACE_ROUTE_PATH);
    expect(sidebar).not.toContain(LEGACY_ROUTE_PATH);
  });

  it("keeps only the session-gated forced audit metadata read", () => {
    expect(typeof adminUsageAPI.getForcedRequestAudit).toBe("function");
    expect(adminUsageAPI).not.toHaveProperty("getRequestAudit");
    const detail = source("components/admin/usage/UsageDetailModal.vue");
    expect(detail).toContain("request_audit_forced_available");
  });

  it("removes the value-detail reads and reveals from the usage API", () => {
    expect(adminUsageAPI).not.toHaveProperty("getRequestAuditValueDetail");
    expect(adminUsageAPI).not.toHaveProperty("revealRequestAuditValueDetail");
  });

  it("removes the value-detail operator gate from the settings API", () => {
    expect(settingsAPI).not.toHaveProperty(
      "getRequestAuditValueDetailOperatorSettings",
    );
    expect(settingsAPI).not.toHaveProperty(
      "updateRequestAuditValueDetailOperatorSettings",
    );
  });

  it("removes the value-detail reveal control from the new usage detail", () => {
    const detail = source("components/admin/usage/UsageDetailModal.vue");
    expect(detail).not.toContain("request-audit-value-detail");
    expect(detail).not.toContain("revealRequestAuditValueDetail");
  });

  it("removes the value-detail panel from settings while keeping the Trace gate", () => {
    const settingsView = source("views/admin/SettingsView.vue");
    expect(settingsView).not.toContain("requestAuditValueDetail");
    expect(settingsView).not.toContain("RequestTraceOperatorSettings");
    expect(
      source("features/request-trace/RequestTraceSettingsDialog.vue"),
    ).toContain("RequestTraceOperatorSettings");
  });
});

describe("retired legacy capture locale keys", () => {
  for (const [name, locale] of Object.entries({ en, zh })) {
    it(`${name} no longer exposes the retired copy`, () => {
      const keys = flattenKeys(locale);
      expect(
        keys.filter((key) => key.startsWith("admin.errorDiagnostics")),
      ).toEqual([]);
      expect(
        keys.filter((key) =>
          key.startsWith("admin.usage.requestAudit.valueDetail"),
        ),
      ).toEqual([]);
      expect(
        keys.filter((key) =>
          key.startsWith("admin.settings.requestAuditValueDetail"),
        ),
      ).toEqual([]);
    });

    it(`${name} keeps the metadata-only audit and the Trace copy`, () => {
      const keys = flattenKeys(locale);
      expect(keys).toContain("admin.usage.requestAudit.title");
      expect(keys).toContain("admin.requestTrace.title");
    });
  }
});
