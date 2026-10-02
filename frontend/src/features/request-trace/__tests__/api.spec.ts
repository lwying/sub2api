import { beforeEach, describe, expect, it, vi } from "vitest";
const client = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn() }));
vi.mock("@/api/client", () => ({ apiClient: client }));
import { getOperatorSettings, updateOperatorSettings } from "../api";

const scope = {
  all_groups: false,
  group_ids: [4, 7],
  model_scope: "include",
  models: ["claude-sonnet-4-5"],
  platform_scope: "exclude",
  platforms: ["antigravity"],
};

const status = {
  enabled: false,
  capture_allowed: false,
  risk_acknowledgement_current: false,
  risk_version: "v2026.09.28",
  risk_phrase_en: "New Trace risk",
  risk_phrase_zh: "新明文风险",
  plaintext_capture_supported: true,
  plaintext_capture_support_reason: "supported",
  ...scope,
};

describe("Trace operator settings API", () => {
  beforeEach(() => Object.values(client).forEach((mock) => mock.mockReset()));

  it("reads only the new Trace gate without returning unexpected credentials", async () => {
    client.get.mockResolvedValue({
      data: { ...status, body: "CANARY_BODY", admin_api_key: "sk-CANARY" },
    });
    const result = await getOperatorSettings();
    expect(client.get).toHaveBeenCalledWith(
      "/admin/settings/request-trace",
      expect.objectContaining({
        headers: expect.objectContaining({ "Cache-Control": "no-store" }),
      }),
    );
    expect(JSON.stringify(result)).not.toContain("CANARY_BODY");
    expect(JSON.stringify(result)).not.toContain("sk-CANARY");
    expect(result.capture_allowed).toBe(false);
  });

  it("sends the gate and typed acknowledgement without any scope, and says so", async () => {
    client.put.mockResolvedValue({ data: status });
    await updateOperatorSettings({
      enabled: true,
      language: "en",
      phrase: "New Trace risk",
    });
    expect(client.put).toHaveBeenCalledWith(
      "/admin/settings/request-trace",
      {
        enabled: true,
        language: "en",
        phrase: "New Trace risk",
        // Explicitly not a scope edit: the server keeps the stored scope.
        scope_provided: false,
      },
      expect.objectContaining({
        headers: expect.objectContaining({ Pragma: "no-cache" }),
      }),
    );
  });

  it("sends the whole scope, and only when the operator actually provided one", async () => {
    client.put.mockResolvedValue({ data: status });
    await updateOperatorSettings({
      enabled: false,
      language: "en",
      phrase: "",
      scope_provided: true,
      ...scope,
    });
    expect(client.put).toHaveBeenCalledWith(
      "/admin/settings/request-trace",
      {
        enabled: false,
        language: "en",
        phrase: "",
        scope_provided: true,
        ...scope,
      },
      expect.objectContaining({
        headers: expect.objectContaining({ "Cache-Control": "no-store" }),
      }),
    );
  });
});
