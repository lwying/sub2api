/**
 * The locale and label surface of the mock feature: both languages carry the
 * same leaves, every closed set the views render through has a label for each
 * value the server can send plus a fallback, and no label is the raw wire token.
 */
import { describe, expect, it } from "vitest";

import {
  gatewayMockContentAuditKey,
  gatewayMockFailureKey,
  gatewayMockFailureReasons,
  gatewayMockProtocolKey,
} from "../labels";
import { gatewayMockEn, gatewayMockZh } from "../locale";
import { gatewayMockContentAuditStates, gatewayMockProtocols } from "../types";

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

describe("gateway mock locale", () => {
  it("keeps English and Chinese schemas identical", () => {
    const enKeys = flattenLeafKeys(gatewayMockEn).sort();
    const zhKeys = flattenLeafKeys(gatewayMockZh).sort();
    expect(enKeys.filter((key) => !zhKeys.includes(key))).toEqual([]);
    expect(zhKeys.filter((key) => !enKeys.includes(key))).toEqual([]);
  });

  it("contains a non-empty message for every leaf", () => {
    for (const [locale, messages] of Object.entries({
      en: gatewayMockEn,
      zh: gatewayMockZh,
    })) {
      const empty = flattenLeafKeys(messages).filter((key) => {
        const value = readLeaf(messages, key);
        return typeof value !== "string" || value.trim() === "";
      });
      expect(empty, `${locale} has empty or non-string messages`).toEqual([]);
    }
  });

  it("labels every protocol the backend can send, plus a fallback for anything else", () => {
    const values = [...gatewayMockProtocols, "unknown"];
    for (const [locale, messages] of Object.entries({
      en: gatewayMockEn,
      zh: gatewayMockZh,
    })) {
      const labels = values.map((value) =>
        readLeaf(messages, `gatewayMock.events.protocol.${value}`),
      );
      for (const [index, label] of labels.entries()) {
        expect(typeof label, `${locale} protocol.${values[index]}`).toBe(
          "string",
        );
        expect(
          (label as string).trim(),
          `${locale} protocol.${values[index]}`,
        ).not.toBe("");
        expect(
          label,
          `${locale} protocol.${values[index]} must not echo the wire value`,
        ).not.toBe(values[index]);
      }
      expect(
        new Set(labels).size,
        `${locale} has repeated protocol labels`,
      ).toBe(values.length);
    }
  });

  it("maps a protocol outside the closed set to the fallback label key", () => {
    for (const protocol of gatewayMockProtocols) {
      expect(gatewayMockProtocolKey(protocol)).toBe(
        `admin.gatewayMock.events.protocol.${protocol}`,
      );
    }
    expect(gatewayMockProtocolKey("invented_protocol")).toBe(
      "admin.gatewayMock.events.protocol.unknown",
    );
    expect(gatewayMockProtocolKey("")).toBe(
      "admin.gatewayMock.events.protocol.unknown",
    );
  });

  it("labels every content-audit state plus a fallback, and never echoes the raw token", () => {
    // `unknown` 既是闭集成员又是兜底键，去重后仍要求每个值各有独立文案。
    const values = [...new Set([...gatewayMockContentAuditStates, "unknown"])];
    for (const [locale, messages] of Object.entries({
      en: gatewayMockEn,
      zh: gatewayMockZh,
    })) {
      const labels = values.map((value) =>
        readLeaf(messages, `gatewayMock.events.contentAudit.${value}`),
      );
      for (const [index, label] of labels.entries()) {
        expect(typeof label, `${locale} contentAudit.${values[index]}`).toBe(
          "string",
        );
        expect(
          (label as string).trim(),
          `${locale} contentAudit.${values[index]}`,
        ).not.toBe("");
        expect(
          label,
          `${locale} contentAudit.${values[index]} must not echo the wire value`,
        ).not.toBe(values[index]);
      }
      expect(
        new Set(labels).size,
        `${locale} has repeated content-audit labels`,
      ).toBe(values.length);
    }
    // 用户要求的具体文案。
    expect(
      readLeaf(
        gatewayMockZh,
        "gatewayMock.events.contentAudit.skipped_local_mock",
      ),
    ).toBe("本地 Mock，内容审计未执行");
    expect(
      readLeaf(gatewayMockZh, "gatewayMock.events.contentAudit.unknown"),
    ).toBe("未记录");
  });

  it("maps a content-audit state outside the closed set to the fallback label key", () => {
    for (const state of gatewayMockContentAuditStates) {
      expect(gatewayMockContentAuditKey(state)).toBe(
        `admin.gatewayMock.events.contentAudit.${state}`,
      );
    }
    expect(gatewayMockContentAuditKey("invented_audit_state")).toBe(
      "admin.gatewayMock.events.contentAudit.unknown",
    );
    expect(gatewayMockContentAuditKey("")).toBe(
      "admin.gatewayMock.events.contentAudit.unknown",
    );
  });

  it("gives every bounded failure reason its own label, and a generic one otherwise", () => {
    for (const [locale, messages] of Object.entries({
      en: gatewayMockEn,
      zh: gatewayMockZh,
    })) {
      const labels = gatewayMockFailureReasons.map((reason) => {
        const key = gatewayMockFailureKey({ reason });
        expect(
          key,
          `${locale} ${reason} must not be the generic fallback`,
        ).not.toBe("admin.gatewayMock.failures.generic");
        return readLeaf(messages, key.replace("admin.", ""));
      });
      for (const [index, label] of labels.entries()) {
        expect(
          typeof label,
          `${locale} ${gatewayMockFailureReasons[index]}`,
        ).toBe("string");
        expect(
          (label as string).trim(),
          `${locale} ${gatewayMockFailureReasons[index]}`,
        ).not.toBe("");
        expect(
          label,
          `${locale} ${gatewayMockFailureReasons[index]} must not echo the server token`,
        ).not.toBe(gatewayMockFailureReasons[index]);
      }
      expect(
        new Set(labels).size,
        `${locale} has repeated failure labels`,
      ).toBe(labels.length);
    }
  });

  it("falls back to the generic message for an unknown or missing reason", () => {
    expect(
      gatewayMockFailureKey({ reason: "GATEWAY_MOCK_SOMETHING_NEW" }),
    ).toBe("admin.gatewayMock.failures.generic");
    expect(
      gatewayMockFailureKey({ code: "GATEWAY_MOCK_RULE_KEYWORD_EMPTY" }),
    ).toBe("admin.gatewayMock.failures.keywordEmpty");
    expect(
      gatewayMockFailureKey({ reason: 'pq: relation "x" does not exist' }),
    ).toBe("admin.gatewayMock.failures.generic");
    expect(gatewayMockFailureKey(null)).toBe(
      "admin.gatewayMock.failures.generic",
    );
    expect(gatewayMockFailureKey(undefined)).toBe(
      "admin.gatewayMock.failures.generic",
    );
  });
});
