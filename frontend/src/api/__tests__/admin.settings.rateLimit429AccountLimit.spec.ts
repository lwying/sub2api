/**
 * Decoding seam for the per-request 429 account limit and its optional
 * cross-request Claude cooldown
 * (GET/PUT /api/v1/admin/settings/rate-limit-429-account-limit).
 *
 * The N control and the cooldown belong to one setting group: one path, one
 * payload. Two consequences are pinned here:
 *
 *   1. The settings card sends the whole group. The server also accepts older
 *      clients that send only N, preserving the saved cooldown fields.
 *   2. The stored shape is read back as a closed set: `scope` is one of two
 *      scopes, `cooldown_seconds` stays inside 1–7200 and `max_accounts` inside
 *      1–100. A missing or unusable field falls back to the documented default
 *      (off / session / 60 / 2), the same way the backend falls back when no
 *      value was ever stored — a legacy payload must not render as an
 *      out-of-contract control.
 */
import { beforeEach, describe, expect, it, vi } from "vitest";

const { get, put } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  apiClient: { get, put },
}));

import {
  getRateLimit429AccountLimit,
  normalizeRateLimit429AccountLimit,
  updateRateLimit429AccountLimit,
} from "@/api/admin/settings";

const SETTINGS_PATH = "/admin/settings/rate-limit-429-account-limit";

const STORED = {
  max_accounts: 4,
  enabled: true,
  scope: "device",
  cooldown_seconds: 900,
};

describe("429 account limit settings group", () => {
  beforeEach(() => {
    get.mockReset();
    put.mockReset();
  });

  it("reads back the account limit and the cross-request cooldown from one path", async () => {
    get.mockResolvedValueOnce({ data: STORED });

    await expect(getRateLimit429AccountLimit()).resolves.toEqual(STORED);
    expect(get).toHaveBeenCalledWith(SETTINGS_PATH);
  });

  it("writes N, the switch, the scope and the seconds in one payload", async () => {
    put.mockResolvedValueOnce({ data: STORED });

    await expect(updateRateLimit429AccountLimit(STORED)).resolves.toEqual(STORED);
    expect(put).toHaveBeenCalledWith(SETTINGS_PATH, STORED);
  });

  it("normalizes an incomplete save response before returning it to the settings card", async () => {
    put.mockResolvedValueOnce({ data: { max_accounts: 7 } });
    await expect(updateRateLimit429AccountLimit(STORED)).resolves.toEqual({
      max_accounts: 7,
      enabled: false,
      scope: "session",
      cooldown_seconds: 60,
    });
  });

  it("keeps every field of the group on the payload it hands to the server", () => {
    // Guards against a caller building the body by hand and dropping a field.
    const payload = normalizeRateLimit429AccountLimit(STORED);
    expect(Object.keys(payload).sort()).toEqual([
      "cooldown_seconds",
      "enabled",
      "max_accounts",
      "scope",
    ]);
  });
});

describe("429 account limit stored-shape normalization", () => {
  it("fills the documented defaults for a payload stored before the cooldown existed", () => {
    expect(normalizeRateLimit429AccountLimit({ max_accounts: 7 })).toEqual({
      max_accounts: 7,
      enabled: false,
      scope: "session",
      cooldown_seconds: 60,
    });
  });

  it("keeps a valid group unchanged and keeps N readable", () => {
    expect(normalizeRateLimit429AccountLimit(STORED)).toEqual(STORED);
  });

  it("falls back to the session scope for an unknown scope", () => {
    expect(normalizeRateLimit429AccountLimit({ ...STORED, scope: "bucket" }).scope).toBe("session");
    expect(normalizeRateLimit429AccountLimit({ ...STORED, scope: "device" }).scope).toBe("device");
    expect(normalizeRateLimit429AccountLimit({ ...STORED, scope: null }).scope).toBe("session");
  });

  it("keeps the seconds inside 1-7200 and falls back to 60 outside", () => {
    expect(normalizeRateLimit429AccountLimit({ ...STORED, cooldown_seconds: 1 }).cooldown_seconds).toBe(1);
    expect(normalizeRateLimit429AccountLimit({ ...STORED, cooldown_seconds: 7200 }).cooldown_seconds).toBe(7200);
    expect(normalizeRateLimit429AccountLimit({ ...STORED, cooldown_seconds: 0 }).cooldown_seconds).toBe(60);
    expect(normalizeRateLimit429AccountLimit({ ...STORED, cooldown_seconds: 7201 }).cooldown_seconds).toBe(60);
    expect(normalizeRateLimit429AccountLimit({ ...STORED, cooldown_seconds: "60" }).cooldown_seconds).toBe(60);
    expect(normalizeRateLimit429AccountLimit({ ...STORED, cooldown_seconds: Number.NaN }).cooldown_seconds).toBe(60);
  });

  it("keeps the account limit inside 1-100 and falls back to 2 outside", () => {
    expect(normalizeRateLimit429AccountLimit({ max_accounts: 1 }).max_accounts).toBe(1);
    expect(normalizeRateLimit429AccountLimit({ max_accounts: 100 }).max_accounts).toBe(100);
    expect(normalizeRateLimit429AccountLimit({ max_accounts: 0 }).max_accounts).toBe(2);
    expect(normalizeRateLimit429AccountLimit({ max_accounts: 101 }).max_accounts).toBe(2);
    expect(normalizeRateLimit429AccountLimit({ max_accounts: "3" }).max_accounts).toBe(2);
  });

  it("treats the switch as a real boolean", () => {
    expect(normalizeRateLimit429AccountLimit({ ...STORED, enabled: true }).enabled).toBe(true);
    expect(normalizeRateLimit429AccountLimit({ ...STORED, enabled: "true" }).enabled).toBe(false);
    expect(normalizeRateLimit429AccountLimit({ ...STORED, enabled: 1 }).enabled).toBe(false);
  });
});
