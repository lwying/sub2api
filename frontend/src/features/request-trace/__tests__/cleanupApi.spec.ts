import { beforeEach, describe, expect, it, vi } from "vitest";
const client = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
  post: vi.fn(),
}));
vi.mock("@/api/client", () => ({ apiClient: client }));
import {
  previewTraceDelete,
  deleteSelectedTraces,
  deleteTracesByFilter,
} from "../api";

const preview = {
  matched_count: 42,
  snapshot_max_id: 900,
  filter_hash: "f".repeat(64),
  confirmation_token: "opaque-server-confirmation",
  expires_at: "2026-10-03T01:05:00Z",
};

describe("Trace cleanup API", () => {
  beforeEach(() => Object.values(client).forEach((mock) => mock.mockReset()));

  it("previews only explicit metadata filters and refuses an empty cleanup", async () => {
    await expect(previewTraceDelete({})).rejects.toThrow();
    expect(client.post).not.toHaveBeenCalled();
    client.post.mockResolvedValue({ data: preview });
    expect(await previewTraceDelete({ q: "debug", page: 2 } as never)).toEqual(
      preview,
    );
    expect(client.post).toHaveBeenCalledWith(
      "/admin/request-traces/delete-preview",
      { filter: { q: "debug" } },
      expect.anything(),
    );
  });

  it("binds filtered deletion to the server preview and reports partial counts", async () => {
    client.post.mockResolvedValue({
      data: { deleted_count: 20, completed: false },
    });
    expect(await deleteTracesByFilter({ client_status: 500 }, preview)).toEqual(
      { deleted_count: 20, completed: false },
    );
    expect(client.post).toHaveBeenCalledWith(
      "/admin/request-traces/delete-by-filter",
      {
        filter: { client_status: 500 },
        snapshot_max_id: 900,
        filter_hash: preview.filter_hash,
        confirmation_token: preview.confirmation_token,
        confirm: true,
      },
      expect.anything(),
    );
  });

  it("sends only the selected trace IDs after confirmation", async () => {
    client.post.mockResolvedValue({
      data: { deleted_count: 1, completed: true },
    });
    await deleteSelectedTraces(["a".repeat(32)]);
    expect(client.post).toHaveBeenCalledWith(
      "/admin/request-traces/batch-delete",
      { trace_ids: ["a".repeat(32)], confirm: true },
      expect.anything(),
    );
    client.post.mockClear();
    await expect(deleteSelectedTraces([])).rejects.toThrow();
    expect(client.post).not.toHaveBeenCalled();
  });
});
