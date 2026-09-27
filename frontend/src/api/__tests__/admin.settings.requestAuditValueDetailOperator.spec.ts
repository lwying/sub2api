/**
 * Decoding seam for the request-audit value-detail operator gate
 * (GET/PUT /api/v1/admin/usage/request-audit-value-detail-settings).
 *
 * New value details are plaintext and usage-owned, so besides the stored switch and
 * the written acknowledgement the server reports a **deployment premise**: whether
 * this database can guarantee that plaintext rows disappear with the usage record
 * they belong to (ADR 0007). Three consequences are pinned here:
 *
 *   1. The premise flag is a real boolean and the reason code is a non-empty string.
 *      A payload missing either is a payload error — rendering it would show "off"
 *      with no explanation, which is exactly the state this panel has to avoid.
 *   2. The reason code is a closed enum. An unknown code is mapped to the "unknown"
 *      copy, never echoed onto the panel.
 *   3. A refusal to enable is a 409 with its own stable code; it must not fall back
 *      to the generic rejection copy.
 */
import { describe, expect, it } from "vitest";

import {
  normalizeRequestAuditValueDetailOperatorStatus,
  requestAuditValueDetailErrorKey,
  requestAuditValueDetailSupportReasonKey,
} from "@/api/admin/settings";

const PHRASE_EN = "Statement EN: plaintext, follows usage, not an erasure tool.";

function statusPayload(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    enabled: true,
    risk_acknowledged: true,
    capture_allowed: false,
    encryption_key_available: true,
    risk_version: "v2026.09.27",
    risk_phrase_en: PHRASE_EN,
    risk_phrase_zh: "确认语句",
    risk_acknowledgement_current: true,
    plaintext_capture_supported: false,
    plaintext_capture_support_reason: "unsupported_partitioned_usage_logs",
    ...overrides,
  };
}

describe("request audit value detail operator status decoding", () => {
  it("carries the deployment premise alongside the gate flags", () => {
    const status = normalizeRequestAuditValueDetailOperatorStatus(statusPayload());

    expect(status.plaintext_capture_supported).toBe(false);
    expect(status.plaintext_capture_support_reason).toBe("unsupported_partitioned_usage_logs");
    // The stored flags and the verified conclusion stay separate facts.
    expect(status.enabled).toBe(true);
    expect(status.capture_allowed).toBe(false);
    expect(status.risk_acknowledgement_current).toBe(true);
  });

  it("keeps an unknown reason code verbatim for the closed-set mapper", () => {
    const status = normalizeRequestAuditValueDetailOperatorStatus(
      statusPayload({ plaintext_capture_support_reason: "brand_new_code" }),
    );

    expect(status.plaintext_capture_support_reason).toBe("brand_new_code");
    expect(requestAuditValueDetailSupportReasonKey(status.plaintext_capture_support_reason)).toBe(
      "deploymentUnknown",
    );
  });

  it("rejects a payload whose deployment premise is not a real boolean", () => {
    // "Could not be read" must not be normalized into "off": the client refuses the
    // payload instead of inventing a conclusion the server never stated.
    expect(() =>
      normalizeRequestAuditValueDetailOperatorStatus(
        statusPayload({ plaintext_capture_supported: "false" }),
      ),
    ).toThrow();
  });

  it("rejects a payload with no deployment reason to explain the conclusion", () => {
    expect(() =>
      normalizeRequestAuditValueDetailOperatorStatus(
        statusPayload({ plaintext_capture_support_reason: "" }),
      ),
    ).toThrow();
    expect(() => {
      const payload = statusPayload();
      delete payload.plaintext_capture_support_reason;
      return normalizeRequestAuditValueDetailOperatorStatus(payload);
    }).toThrow();
  });
});

describe("request audit value detail deployment reason mapping", () => {
  it("maps every closed reason code to its own copy", () => {
    expect(requestAuditValueDetailSupportReasonKey("supported")).toBe("deploymentSupported");
    expect(requestAuditValueDetailSupportReasonKey("unsupported_partitioned_usage_logs")).toBe(
      "deploymentPartitioned",
    );
    expect(requestAuditValueDetailSupportReasonKey("unsupported_missing_ownership_foreign_key")).toBe(
      "deploymentMissingOwnership",
    );
    expect(requestAuditValueDetailSupportReasonKey("probe_failed")).toBe("deploymentProbeFailed");
    expect(requestAuditValueDetailSupportReasonKey("probe_unavailable")).toBe(
      "deploymentProbeUnavailable",
    );
    // 未知码与已知的「未知形态」同文案：都不回显原字符串。
    expect(requestAuditValueDetailSupportReasonKey("something_else")).toBe("deploymentUnknown");
    expect(requestAuditValueDetailSupportReasonKey("")).toBe("deploymentUnknown");
  });
});

describe("request audit value detail refusal mapping", () => {
  it("gives the deployment refusal its own copy instead of the generic rejection", () => {
    expect(
      requestAuditValueDetailErrorKey({
        status: 409,
        reason: "REQUEST_AUDIT_VALUE_DETAIL_DEPLOYMENT_UNSUPPORTED",
      }),
    ).toBe("deploymentUnsupported");
  });

  it("keeps the existing mappings and the fallbacks", () => {
    expect(
      requestAuditValueDetailErrorKey({ status: 400, reason: "REQUEST_AUDIT_VALUE_DETAIL_RISK_ACK_INVALID" }),
    ).toBe("phraseInvalid");
    expect(requestAuditValueDetailErrorKey({ status: 400 })).toBe("rejected");
    expect(requestAuditValueDetailErrorKey({ status: 403 })).toBe("forbidden");
    expect(requestAuditValueDetailErrorKey({ status: 503 })).toBe("unavailable");
    // 未知 reason 不猜语义，按状态码回落。
    expect(requestAuditValueDetailErrorKey({ status: 409, reason: "SOMETHING_NEW" })).toBe("rejected");
  });
});
