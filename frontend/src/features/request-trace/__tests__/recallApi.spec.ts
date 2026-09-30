import { beforeEach, describe, expect, it, vi } from 'vitest'

const client = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: client }))

import {
  TraceExportRefusedError,
  listTraceExports,
  requestTraceExportRecallCursorLimit,
  requestTraceExportRecallLimit,
} from '../api'

const TASK_ID = 'b'.repeat(32)
const OTHER_TASK_ID = 'c'.repeat(32)
const exportTask = (overrides: Record<string, unknown> = {}) => ({
  id: TASK_ID,
  status: 'completed',
  filter: { route_family: 'messages' },
  rows_exported: 12,
  rows_skipped: 0,
  bytes_exported: 4096,
  created_at: '2026-09-28T00:00:00Z',
  completed_at: '2026-09-28T00:05:00Z',
  download_until: '2026-10-05T00:05:00Z',
  downloadable: true,
  shard_count: 2,
  truncated: false,
  ...overrides,
})
/** The server always states whether a next page exists; null is that answer. */
const page = (items: unknown[], nextCursor: unknown = null) => ({ data: { items, next_cursor: nextCursor } })

describe('Trace export recall API', () => {
  beforeEach(() => Object.values(client).forEach(mock => mock.mockReset()))

  it('reads the session task list with no id, filter or session parameter', async () => {
    client.get.mockResolvedValue(page([exportTask(), exportTask({ id: OTHER_TASK_ID, status: 'failed' })]))

    const result = await listTraceExports()

    expect(client.get).toHaveBeenCalledWith('/admin/request-traces/exports', expect.objectContaining({
      headers: expect.objectContaining({ 'Cache-Control': 'no-store', Pragma: 'no-cache' }),
    }))
    // The session is the credential: no handle is ever sent as a parameter, and
    // the first page needs no cursor either.
    const config = client.get.mock.calls[0][1] as { params?: unknown }
    expect(config.params).toBeUndefined()
    expect(result.items.map(task => task.id)).toEqual([TASK_ID, OTHER_TASK_ID])
    expect(result.items[0].downloadable).toBe(true)
    expect(result.items[1].status).toBe('failed')
    // null is "no further page", and it is a value, not a missing field.
    expect(result.nextCursor).toBeNull()
  })

  it('passes the page token straight back, and only that token', async () => {
    client.get.mockResolvedValue(page([exportTask()], 'v1.opaque-token'))

    const first = await listTraceExports({ cursor: null })
    expect((client.get.mock.calls[0][1] as { params?: unknown }).params).toBeUndefined()

    const second = await listTraceExports({ cursor: first.nextCursor, limit: 20 })
    expect((client.get.mock.calls[1][1] as { params: Record<string, unknown> }).params).toEqual({
      cursor: 'v1.opaque-token',
      limit: 20,
    })
    expect(second.nextCursor).toBe('v1.opaque-token')
  })

  it('treats a payload that cannot be continued as a protocol failure, not as "no more"', async () => {
    // A missing key would silently end the walk one page early.
    client.get.mockResolvedValue({ data: { items: [exportTask()] } })
    await expect(listTraceExports()).rejects.toThrow()

    // An empty or over-long token is not a page the caller could follow.
    client.get.mockResolvedValue(page([exportTask()], ''))
    await expect(listTraceExports()).rejects.toThrow()

    client.get.mockResolvedValue(page([exportTask()], 'x'.repeat(requestTraceExportRecallCursorLimit + 1)))
    await expect(listTraceExports()).rejects.toThrow()

    client.get.mockResolvedValue(page([exportTask()], 42))
    await expect(listTraceExports()).rejects.toThrow()

    client.get.mockResolvedValue({ data: { items: 'not-an-array', next_cursor: null } })
    await expect(listTraceExports()).rejects.toThrow()

    client.get.mockResolvedValue(page(Array.from({ length: requestTraceExportRecallLimit + 1 }, () => exportTask())))
    await expect(listTraceExports()).rejects.toThrow()

    client.get.mockResolvedValue(page([exportTask({ id: 'not-a-task-id' })]))
    await expect(listTraceExports()).rejects.toThrow()

    client.get.mockResolvedValue({ data: null })
    await expect(listTraceExports()).rejects.toThrow()
  })

  it('never surfaces a filename or a session handle even if the server sends one', async () => {
    client.get.mockResolvedValue(page([{
      ...exportTask(),
      filename: 'sub2api-request-trace-export-secret.jsonl',
      session_digest: 'd'.repeat(64),
      instance_id: 'node-one',
    }]))

    const result = await listTraceExports()

    const serialized = JSON.stringify(result)
    expect(serialized).not.toContain('secret.jsonl')
    expect(serialized).not.toContain('d'.repeat(64))
    expect(serialized).not.toContain('node-one')
  })

  it('reports a refused recall as a bounded outcome, not as an empty list', async () => {
    client.get.mockRejectedValue({ status: 403, reason: 'REQUEST_TRACE_EXPORT_ADMIN_SESSION_REQUIRED' })

    await expect(listTraceExports()).rejects.toBeInstanceOf(TraceExportRefusedError)
    await expect(listTraceExports()).rejects.toMatchObject({ refusal: 'session_required' })
  })

  it('keeps a cancellation recognisable instead of relabelling it a refusal', async () => {
    const cancelled = Object.assign(new Error('canceled'), { code: 'ERR_CANCELED' })
    client.get.mockRejectedValue(cancelled)
    const controller = new AbortController()
    controller.abort()

    await expect(listTraceExports({ signal: controller.signal })).rejects.toBe(cancelled)
  })
})
