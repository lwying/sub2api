import { beforeEach, describe, expect, it, vi } from 'vitest'

const client = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: client }))

import { createTraceExport, downloadTraceExport, getTrace, getTraceExport, listTraces } from '../api'

const row = { trace_id: 'a'.repeat(32), route_family: 'messages', inbound_endpoint: '/v1/messages', capture_state: 'partial', client_status: 401, usage_log_id: null, cleanup_after: '2026-10-28T00:00:00Z', created_at: '2026-09-28T00:00:00Z', completed_at: null }

describe('Trace list and detail API', () => {
  beforeEach(() => Object.values(client).forEach(mock => mock.mockReset()))

  it('reads metadata-only list using only approved metadata filters', async () => {
    client.get.mockResolvedValue({ data: { items: [{ ...row, payload_text: 'PRIVATE_BODY', extra: { authorization: 'Bearer TOKEN' } }], total: 1, page: 1, page_size: 20 } })
    const result = await listTraces({ page: 1, page_size: 20, trace_id: row.trace_id })
    expect(client.get).toHaveBeenCalledWith('/admin/request-traces', expect.objectContaining({
      params: { page: 1, page_size: 20, trace_id: row.trace_id },
      headers: expect.objectContaining({ 'Cache-Control': 'no-store' }),
    }))
    expect(result.items).toHaveLength(1)
    expect(JSON.stringify(result.items)).not.toContain('PRIVATE_BODY')
    expect(JSON.stringify(result.items)).not.toContain('Bearer TOKEN')
  })

  it('rejects malformed list rows instead of fabricating a trace', async () => {
    client.get.mockResolvedValue({ data: { items: [{ trace_id: '../other' }], total: 1, page: 1, page_size: 20 } })
    await expect(listTraces({ page: 1, page_size: 20 })).rejects.toThrow()
  })

  it('refuses a contradictory not-observed stage carrying plaintext', async () => {
    client.get.mockResolvedValue({ data: { ...row, stages: [{ ordinal: 1, stage: 'inbound_request', state: 'not_observed', reason: 'auth_rejected_before_body', attempt_index: 0, view_name: '', observed_bytes: 0, retained_bytes: 0, dropped_events: 0, redaction_unverified: false, payload_text: 'LEAK' }] } })
    await expect(getTrace(row.trace_id)).rejects.toThrow()
  })

  it('reads plaintext only on explicit detail, normalizes stages and encodes ID in URL', async () => {
    client.get.mockResolvedValue({ data: { ...row, stages: [{ ordinal: 1, stage: 'inbound_request', state: 'not_observed', reason: 'auth_rejected_before_body', attempt_index: 0, view_name: '', observed_bytes: 0, retained_bytes: 0, dropped_events: 0, redaction_unverified: false, metadata: { credentials: 'SECRET' } }] } })
    const result = await getTrace(row.trace_id)
    expect(client.get).toHaveBeenCalledWith(`/admin/request-traces/${row.trace_id}`, expect.objectContaining({ headers: expect.objectContaining({ Pragma: 'no-cache' }) }))
    expect(JSON.stringify(result)).not.toContain('SECRET')
    expect(result.stages[0].reason).toBe('auth_rejected_before_body')
  })
})

// The real handler reads the metadata filter from the query string and returns the
// task view { id, status, filter, rows_exported, rows_skipped, bytes_exported,
// created_at, completed_at?, download_until?, downloadable }. Nothing else is decoded.
const TASK_ID = 'b'.repeat(32)
const exportTask = (overrides: Record<string, unknown> = {}) => ({
  id: TASK_ID,
  status: 'pending',
  filter: { trace_id: 'a'.repeat(32), route_family: 'messages' },
  rows_exported: 0,
  rows_skipped: 0,
  bytes_exported: 0,
  created_at: '2026-09-28T00:00:00Z',
  completed_at: null,
  download_until: null,
  downloadable: false,
  ...overrides,
})

function withReason(reason: string, status = 410) {
  return { status, data: new Blob([JSON.stringify({ code: status, reason })]), headers: {} }
}

describe('Trace batch export API', () => {
  beforeEach(() => Object.values(client).forEach(mock => mock.mockReset()))

  it('creates a bounded task from query metadata and sends no request body', async () => {
    client.post.mockResolvedValue({ data: exportTask() })
    const created = await createTraceExport({ trace_id: 'a'.repeat(32), route_family: 'messages', client_status: 429, usage_linked: false })

    const [url, body, config] = client.post.mock.calls[0] as [string, unknown, { params?: Record<string, unknown>; headers?: Record<string, string> }]
    expect(url).toBe('/admin/request-traces/exports')
    expect(body ?? null).toBeNull()
    expect(config.params).toEqual({ trace_id: 'a'.repeat(32), route_family: 'messages', client_status: 429, usage_linked: false })
    expect(Object.keys(config.params ?? {}).sort()).toEqual(['client_status', 'route_family', 'trace_id', 'usage_linked'])
    expect(JSON.stringify(config.params)).not.toMatch(/body|prompt|search|text/i)
    expect(config.headers).toEqual(expect.objectContaining({ 'Cache-Control': 'no-store', Pragma: 'no-cache' }))
    expect(created.id).toBe(TASK_ID)
    expect(created.filter).toEqual({ trace_id: 'a'.repeat(32), route_family: 'messages' })
  })

  it('keeps omitted completion timestamps null instead of inventing a deadline', async () => {
    client.post.mockResolvedValue({ data: exportTask() })
    const created = await createTraceExport({})

    expect(exportParams()).toEqual({})
    expect(created.completed_at).toBeNull()
    expect(created.download_until).toBeNull()
    expect(created.downloadable).toBe(false)
  })

  it('drops unknown task fields instead of carrying captured content or a download URL', async () => {
    client.post.mockResolvedValue({ data: exportTask({ payload_text: 'BODY_CANARY', authorization: 'Bearer CANARY', download_url: 'https://public.example/leak' }) })
    const created = await createTraceExport({})

    expect(JSON.stringify(created)).not.toContain('BODY_CANARY')
    expect(JSON.stringify(created)).not.toContain('CANARY')
    expect(JSON.stringify(created)).not.toContain('public.example')
  })

  it('rejects a malformed task view rather than fabricating export progress', async () => {
    client.post.mockResolvedValue({ data: exportTask({ rows_exported: -1 }) })
    await expect(createTraceExport({})).rejects.toThrow()

    client.post.mockResolvedValue({ data: exportTask({ id: '../other' }) })
    await expect(createTraceExport({})).rejects.toThrow()

    client.post.mockResolvedValue({ data: exportTask({ status: 'almost_done' }) })
    await expect(createTraceExport({})).rejects.toThrow()
  })

  it('reads the same task view by id from the server-owned route', async () => {
    client.get.mockResolvedValue({ data: exportTask({ status: 'completed', completed_at: '2026-09-28T00:10:00Z', download_until: '2026-10-05T00:10:00Z', rows_exported: 42, rows_skipped: 3, bytes_exported: 2048, downloadable: true }) })
    const task = await getTraceExport(TASK_ID)

    expect(client.get).toHaveBeenCalledWith(`/admin/request-traces/exports/${TASK_ID}`, expect.objectContaining({ headers: expect.objectContaining({ Pragma: 'no-cache' }) }))
    expect(task).toMatchObject({ status: 'completed', rows_exported: 42, rows_skipped: 3, bytes_exported: 2048, downloadable: true })
  })

  it('refuses an id that the server route cannot own', async () => {
    await expect(getTraceExport('../exports')).rejects.toThrow()
    expect(client.get).not.toHaveBeenCalled()
  })

  it('downloads the temporary file as a blob under the server filename', async () => {
    const blob = new Blob(['SYNTHETIC'])
    client.get.mockResolvedValue({ status: 200, data: blob, headers: { 'content-disposition': `attachment; filename="sub2api-request-trace-export-${TASK_ID}.jsonl"` } })
    const result = await downloadTraceExport(TASK_ID)

    expect(client.get).toHaveBeenCalledWith(`/admin/request-traces/exports/${TASK_ID}/download`, expect.objectContaining({ responseType: 'blob', headers: expect.objectContaining({ 'Cache-Control': 'no-store' }) }))
    expect(result).toEqual({ outcome: 'ready', blob, fileName: `sub2api-request-trace-export-${TASK_ID}.jsonl` })
  })

  it('never follows a hostile filename out of the server header', async () => {
    const blob = new Blob(['SYNTHETIC'])
    client.get.mockResolvedValue({ status: 200, data: blob, headers: { 'content-disposition': 'attachment; filename="../../etc/passwd"\r\nX-Injected: 1' } })
    const result = await downloadTraceExport(TASK_ID)

    expect(result).toEqual({ outcome: 'ready', blob, fileName: '.._.._etc_passwd' })
  })

  it('reports a lost temporary file only from the server 410 file-lost reason', async () => {
    client.get.mockResolvedValue(withReason('REQUEST_TRACE_EXPORT_FILE_LOST'))
    await expect(downloadTraceExport(TASK_ID)).resolves.toEqual({ outcome: 'file_lost' })
  })

  it('does not claim a lost file when the 410 means the download window closed', async () => {
    client.get.mockResolvedValue(withReason('REQUEST_TRACE_EXPORT_DOWNLOAD_EXPIRED'))
    await expect(downloadTraceExport(TASK_ID)).resolves.toEqual({ outcome: 'expired' })
  })

  it('does not claim a lost file from an unrecognized 410 reason', async () => {
    client.get.mockResolvedValue(withReason('REQUEST_TRACE_EXPORT_SOMETHING_ELSE'))
    await expect(downloadTraceExport(TASK_ID)).resolves.toEqual({ outcome: 'unavailable' })
  })

  it('reports a refused or failed download without pretending the file arrived', async () => {
    client.get.mockRejectedValue({ status: 403, message: 'forbidden' })
    await expect(downloadTraceExport(TASK_ID)).resolves.toEqual({ outcome: 'unavailable' })
  })
})

function exportParams(): Record<string, unknown> {
  const config = client.post.mock.calls[0]?.[2] as { params?: Record<string, unknown> } | undefined
  return config?.params ?? {}
}
