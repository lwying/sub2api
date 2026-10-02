/**
 * Feature-local locale guard.
 *
 * Mirrors the repo-wide `check:i18n` rules for this feature so the payload can be
 * handed to the shared locale bundles without surprises: both languages must
 * carry exactly the same leaf keys, every leaf must be a non-empty string, and
 * every literal `admin.requestTrace.*` key used by the feature components must
 * exist and be reachable in the shared bundles.
 *
 * The payload in ../locale is spread into the shared `admin` namespace, so its
 * own keys are `requestTrace.*` while components address them as
 * `admin.requestTrace.*`.
 *
 * The `gateway_decision` stage labels are addressed through dynamic keys
 * (`...decision.kindLabel.${...}`), which no source scan can prove. They are
 * instead checked directly against the closed sets the backend contract defines,
 * so a kind, outcome or source can never reach the drawer without a label in
 * either language.
 */
import { describe, expect, it } from "vitest";
import sharedEn from "@/i18n/locales/en";
import sharedZh from "@/i18n/locales/zh";
import { requestTraceEn, requestTraceZh } from "../locale";
import {
  requestTraceCaptureStates,
  requestTraceDecisionKinds,
  requestTraceDecisionOutcomes,
  requestTraceDecisionSources,
  requestTraceDecisionStage,
  requestTraceExportDisplayStates,
  requestTraceExportIncompleteReasons,
  requestTraceExportRefusals,
  requestTraceExportSkipReasons,
  requestTraceStageNames,
  requestTraceStageReasons,
  requestTraceStageStates,
  requestTraceStageViews,
} from "../types";

const ADMIN_PREFIX = "admin.";

type LocaleValue = Record<string, unknown>;

function flattenLeafKeys(value: unknown, prefix = ""): string[] {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    return prefix ? [prefix] : [];
  }

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

/** Literal `t('...')` keys used by the feature components, without the namespace. */
function usedKeys(): string[] {
  const sources = import.meta.glob("../../request-trace/**/*.{ts,vue}", {
    query: "?raw",
    import: "default",
    eager: true,
  }) as Record<string, string>;

  const keys = new Set<string>();
  for (const [path, source] of Object.entries(sources)) {
    if (path.includes("/__tests__/")) continue;
    // Dynamic template keys are covered by the closed-set checks below.
    for (const match of source.matchAll(/\bt\s*\(\s*(['"])([^'"\r\n]+)\1/g)) {
      if (match[2].startsWith(ADMIN_PREFIX)) {
        keys.add(match[2].slice(ADMIN_PREFIX.length));
      }
    }
  }
  return [...keys].sort();
}

/** The label map a decision enum renders through, as the drawer addresses it. */
function decisionLabels(messages: unknown, group: string): string[] {
  return flattenLeafKeys(
    readLeaf(messages, `requestTrace.detail.decision.${group}`) ?? {},
  ).sort();
}

/** The leaf keys of a label map, as the component addresses it. */
function labelKeys(messages: unknown, path: string): string[] {
  return flattenLeafKeys(readLeaf(messages, path) ?? {}).sort();
}

/**
 * Every closed-set label map the UI renders through: the map path, the wire values
 * it must cover, and the fallback key for a value outside the set. Each list is
 * the same list the parser and the components use, so a value the contract gains
 * or drops cannot silently keep or lose a label in either language.
 */
const closedSetLabelGroups: [string, readonly string[], string][] = [
  ["requestTrace.list.captureStateLabel", requestTraceCaptureStates, "unknown"],
  ["requestTrace.detail.stateLabel", requestTraceStageStates, "unknown"],
  ["requestTrace.detail.stageLabel", requestTraceStageNames, "unknown"],
  ["requestTrace.detail.viewLabel", requestTraceStageViews, "unknown"],
  ["requestTrace.detail.reasonLabel", requestTraceStageReasons, "other"],
  // A task that stopped early, a refusal, and the reason it stopped are all
  // closed sets: no state and no reason may render without its own wording.
  ["requestTrace.export.state", requestTraceExportDisplayStates, "unknown"],
  ["requestTrace.export.refusal", requestTraceExportRefusals, "unknown"],
  [
    "requestTrace.export.reason",
    requestTraceExportIncompleteReasons,
    "unknown",
  ],
];

describe("request Trace locale", () => {
  const enKeys = flattenLeafKeys(requestTraceEn).sort();
  const zhKeys = flattenLeafKeys(requestTraceZh).sort();
  const literalKeys = usedKeys();

  it("keeps English and Chinese schemas identical", () => {
    expect(enKeys.filter((key) => !zhKeys.includes(key))).toEqual([]);
    expect(zhKeys.filter((key) => !enKeys.includes(key))).toEqual([]);
  });

  it("contains a non-empty message for every leaf", () => {
    for (const [locale, messages] of Object.entries({
      en: requestTraceEn,
      zh: requestTraceZh,
    })) {
      const empty = flattenLeafKeys(messages).filter((key) => {
        const value = readLeaf(messages, key);
        return typeof value !== "string" || value.trim() === "";
      });
      expect(empty, `${locale} has empty leaves`).toEqual([]);
    }
  });

  it("defines every literal key the feature components use", () => {
    expect(literalKeys.filter((key) => !enKeys.includes(key))).toEqual([]);
    expect(literalKeys.filter((key) => !zhKeys.includes(key))).toEqual([]);
  });

  it("is reachable from the shared locale bundles", () => {
    // Wire-up guard: the payload is only served once
    // i18n/locales/{en,zh}/admin/index.ts imports and spreads it into `admin`.
    const missingEn = enKeys.filter(
      (key) => readLeaf(sharedEn, `${ADMIN_PREFIX}${key}`) === undefined,
    );
    const missingZh = zhKeys.filter(
      (key) => readLeaf(sharedZh, `${ADMIN_PREFIX}${key}`) === undefined,
    );
    expect(missingEn, "en bundle is missing request Trace keys").toEqual([]);
    expect(missingZh, "zh bundle is missing request Trace keys").toEqual([]);
  });

  it("labels every skip reason an export task can report a count for", () => {
    // The breakdown renders each counted reason through the same closed-set label
    // map the task's own reason code uses, so a reason without wording would show
    // as a bare token to the operator.
    for (const [locale, messages] of Object.entries({
      en: requestTraceEn,
      zh: requestTraceZh,
    })) {
      for (const reason of requestTraceExportSkipReasons) {
        const label = readLeaf(
          messages,
          `requestTrace.export.reason.${reason}`,
        );
        expect(typeof label, `${locale} skip reason ${reason}`).toBe("string");
        expect(
          (label as string).trim(),
          `${locale} skip reason ${reason}`,
        ).not.toBe("");
      }
    }
  });

  it("labels every decision kind the backend contract can send", () => {
    for (const [locale, messages] of Object.entries({
      en: requestTraceEn,
      zh: requestTraceZh,
    })) {
      expect(
        decisionLabels(messages, "kindLabel"),
        `${locale} decision kinds`,
      ).toEqual([...requestTraceDecisionKinds].sort());
    }
  });

  it("labels every decision outcome the backend contract can send", () => {
    for (const [locale, messages] of Object.entries({
      en: requestTraceEn,
      zh: requestTraceZh,
    })) {
      expect(
        decisionLabels(messages, "outcomeLabel"),
        `${locale} decision outcomes`,
      ).toEqual([...requestTraceDecisionOutcomes].sort());
    }
  });

  it("labels every decision source the backend contract can send", () => {
    for (const [locale, messages] of Object.entries({
      en: requestTraceEn,
      zh: requestTraceZh,
    })) {
      expect(
        decisionLabels(messages, "sourceLabel"),
        `${locale} decision sources`,
      ).toEqual([...requestTraceDecisionSources].sort());
    }
  });

  it("renders each decision label as a distinct message rather than a raw enum value", () => {
    // A label that merely echoes the enum (or another label) would pass the
    // coverage checks above while disclosing nothing to an operator.
    for (const [locale, messages] of Object.entries({
      en: requestTraceEn,
      zh: requestTraceZh,
    })) {
      for (const group of [
        "kindLabel",
        "outcomeLabel",
        "sourceLabel",
      ] as const) {
        const labels = decisionLabels(messages, group);
        const values = labels.map((key) =>
          readLeaf(messages, `requestTrace.detail.decision.${group}.${key}`),
        );
        for (const [index, label] of values.entries()) {
          expect(typeof label, `${locale} ${group}.${labels[index]}`).toBe(
            "string",
          );
          expect(
            (label as string).trim(),
            `${locale} ${group}.${labels[index]}`,
          ).not.toBe("");
          expect(
            label,
            `${locale} ${group}.${labels[index]} must not echo the enum`,
          ).not.toBe(labels[index]);
        }
        expect(
          new Set(values).size,
          `${locale} ${group} has repeated labels`,
        ).toBe(labels.length);
      }
    }
  });

  it.each(closedSetLabelGroups)(
    "labels every value of %s the backend can send",
    (path, values, fallback) => {
      for (const [locale, messages] of Object.entries({
        en: requestTraceEn,
        zh: requestTraceZh,
      })) {
        // Exactly the closed set plus the explicit fallback: no missing and no stray label.
        expect(labelKeys(messages, path), `${locale} ${path}`).toEqual(
          [...values, fallback].sort(),
        );
      }
    },
  );

  it.each(closedSetLabelGroups)(
    "labels each %s value as a distinct message rather than a raw token",
    (path, values, fallback) => {
      // A label that merely echoes the wire value (or another label) would pass the
      // coverage check above while disclosing nothing to an operator.
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

  it("states the unknown-platform rule instead of shipping a toggle the backend does not have", () => {
    // The Go contract has no `platform_exclude_unknown`, so there is no toggle
    // and no key for one. The rule it used to imply — an unknown platform is
    // never captured under "only"/"except" — is stated in the platform note.
    for (const [locale, messages] of Object.entries({
      en: requestTraceEn,
      zh: requestTraceZh,
    })) {
      for (const key of [
        "excludeUnknown",
        "excludeUnknownNote",
        "platformUnknownPolicy",
      ]) {
        expect(
          readLeaf(messages, `requestTrace.operator.scope.${key}`),
          `${locale} ${key}`,
        ).toBeUndefined();
      }
    }
    expect(requestTraceEn.requestTrace.operator.scope.platformsNote).toContain(
      "never captured",
    );
    expect(requestTraceEn.requestTrace.operator.scope.platformsNote).toContain(
      "all platforms",
    );
    expect(requestTraceZh.requestTrace.operator.scope.platformsNote).toContain(
      "不采集",
    );
    expect(requestTraceZh.requestTrace.operator.scope.platformsNote).toContain(
      "所有平台",
    );
  });

  it("names the local mock decision as a served reply, not as a missing attempt", () => {
    // The mock kind and its reason say the gateway chose to answer locally: a
    // label that read like a gap or a failed attempt would misstate a successful
    // local mock as an incomplete Trace.
    const en = requestTraceEn.requestTrace.detail;
    const zh = requestTraceZh.requestTrace.detail;
    expect(en.decision.kindLabel.mock).toBe("Local mock reply");
    expect(zh.decision.kindLabel.mock).toBe("本地 mock 应答");
    expect(en.reasonLabel.mock_served).toContain("no upstream attempt");
    expect(zh.reasonLabel.mock_served).toContain("未发出上游尝试");
  });

  it("adds the without-content-audit mock reason without changing the old mock wording", () => {
    // 新早期严格命中在内容审计之前本地应答：新 reason 明确说出"未执行内容审计、未发上游"，
    // 旧 mock_served 的文案保持不变，两者不能合并成一句话。
    const en = requestTraceEn.requestTrace.detail;
    const zh = requestTraceZh.requestTrace.detail;
    expect(en.reasonLabel.mock_served_without_content_audit).toContain(
      "without a content audit",
    );
    expect(en.reasonLabel.mock_served_without_content_audit).toContain(
      "no upstream attempt",
    );
    expect(zh.reasonLabel.mock_served_without_content_audit).toContain(
      "未执行内容审计",
    );
    expect(zh.reasonLabel.mock_served_without_content_audit).toContain(
      "未发上游",
    );
    expect(en.reasonLabel.mock_served_without_content_audit).not.toBe(
      en.reasonLabel.mock_served,
    );
    expect(zh.reasonLabel.mock_served_without_content_audit).not.toBe(
      zh.reasonLabel.mock_served,
    );
  });

  it("labels the shard cap as an incomplete export, not a failed task", () => {
    const en = requestTraceEn.requestTrace.export.reason;
    const zh = requestTraceZh.requestTrace.export.reason;
    expect(en.limit_shards).toContain("shard limit");
    expect(zh.limit_shards).toContain("分片数上限");
  });

  it("names a failed read as its own export outcome, never as a vanished record", () => {
    // `read_failed` and `source_gone` are different facts: one is a read error on
    // a record that was found, the other is a record that is no longer there.
    // One label standing in for the other would misstate what happened.
    const en = requestTraceEn.requestTrace.export.reason;
    const zh = requestTraceZh.requestTrace.export.reason;
    expect(en.read_failed).not.toBe(en.source_gone);
    expect(zh.read_failed).not.toBe(zh.source_gone);
    expect(en.read_failed).toContain("read");
    expect(zh.read_failed).toContain("读取");
  });

  it("counts a skipped row as deleted or unreadable, not as deleted only", () => {
    // `rows_skipped` covers both a vanished record and one that could not be
    // read, so the label names both causes instead of implying deletion.
    const en = requestTraceEn.requestTrace.export.progress.skippedCount;
    const zh = requestTraceZh.requestTrace.export.progress.skippedCount;
    expect(en).toBe("Skipped (deleted or unreadable)");
    expect(en).toContain("deleted");
    expect(en).toContain("unreadable");
    expect(zh).toContain("已删除");
    expect(zh).toContain("无法读取");
  });

  it("labels the gateway decision stage with the other stage names", () => {
    for (const [locale, messages] of Object.entries({
      en: requestTraceEn,
      zh: requestTraceZh,
    })) {
      expect(
        labelKeys(messages, "requestTrace.detail.stageLabel"),
        locale,
      ).toContain(requestTraceDecisionStage);
    }
  });
});
