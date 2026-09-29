import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  createTraceExport: vi.fn(),
  getTraceExport: vi.fn(),
  downloadTraceExport: vi.fn(),
}))

vi.mock('../api', () => mocks)
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import RequestTraceExportPanel from '../RequestTraceExportPanel.vue'

const POLL_INTERVAL_MS = 3000
const TASK_ID = 'b'.repeat(32)
const BEFORE_DEADLINE = '2026-10-05T00:05:00Z'
const AFTER_DEADLINE = '2026-09-27T00:05:00Z'

/** Mirrors the server task view: { id, status, filter, counts, timestamps, downloadable }. */
const task = (overrides: Record<string, unknown> = {}) => ({
  id: TASK_ID,
  status: 'pending',
  filter: {},
  rows_exported: 0,
  rows_skipped: 0,
  bytes_exported: 0,
  created_at: '2026-09-28T00:00:00Z',
  completed_at: null,
  download_until: null,
  downloadable: false,
  ...overrides,
})

const completed = (overrides: Record<string, unknown> = {}) =>
  task({ status: 'completed', completed_at: '2026-09-28T00:05:00Z', download_until: BEFORE_DEADLINE, downloadable: true, ...overrides })

function mountPanel() {
  return mount(RequestTraceExportPanel)
}

async function createExport(wrapper: ReturnType<typeof mountPanel>) {
  await wrapper.get('[data-testid="request-trace-export-create"]').trigger('click')
  await flushPromises()
}

async function clickDownload(wrapper: ReturnType<typeof mountPanel>) {
  await wrapper.get('[data-testid="request-trace-export-download"]').trigger('click')
  await flushPromises()
}

describe('admin Request Trace batch export panel', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] })
    vi.setSystemTime(new Date('2026-09-28T00:00:00Z'))
    localStorage.clear()
    mocks.createTraceExport.mockReset()
    mocks.getTraceExport.mockReset()
    mocks.downloadTraceExport.mockReset()
    mocks.createTraceExport.mockResolvedValue(task())
    Object.assign(URL, {
      createObjectURL: vi.fn(() => 'blob:trace-export'),
      revokeObjectURL: vi.fn(),
    })
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('creates a bounded export from metadata filters only, with no body search', async () => {
    const wrapper = mountPanel()
    await wrapper.get('[data-testid="request-trace-export-id-filter"]').setValue('a'.repeat(32))
    await wrapper.get('[data-testid="request-trace-export-route-family"]').setValue('messages')
    await wrapper.get('[data-testid="request-trace-export-status"]').setValue('429')
    await createExport(wrapper)

    expect(mocks.createTraceExport).toHaveBeenCalledTimes(1)
    const filter = mocks.createTraceExport.mock.calls[0][0] as Record<string, unknown>
    expect(filter).toEqual({ trace_id: 'a'.repeat(32), route_family: 'messages', client_status: 429 })
    expect(Object.keys(filter).sort()).toEqual(['client_status', 'route_family', 'trace_id'])
    expect(JSON.stringify(filter)).not.toMatch(/body|prompt|search|text/i)
    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('pending')
  })

  it('rejects an invalid Trace ID instead of creating an unbounded export', async () => {
    const wrapper = mountPanel()
    await wrapper.get('[data-testid="request-trace-export-id-filter"]').setValue('not-a-trace-id')
    await createExport(wrapper)

    expect(wrapper.get('[data-testid="request-trace-export-filter-error"]').exists()).toBe(true)
    expect(mocks.createTraceExport).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="request-trace-export-task"]').exists()).toBe(false)
  })

  it('does not claim a task exists when creation is refused', async () => {
    mocks.createTraceExport.mockRejectedValue(new Error('503'))
    const wrapper = mountPanel()
    await createExport(wrapper)

    expect(wrapper.get('[data-testid="request-trace-export-create-error"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="request-trace-export-task"]').exists()).toBe(false)
  })

  it('polls the server task by id until it completes, then stops', async () => {
    mocks.getTraceExport
      .mockResolvedValueOnce(task({ status: 'running' }))
      .mockResolvedValueOnce(completed({ rows_exported: 42, rows_skipped: 3, bytes_exported: 2048 }))
    const wrapper = mountPanel()
    await createExport(wrapper)

    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS)
    expect(mocks.getTraceExport).toHaveBeenCalledWith(TASK_ID, expect.anything())
    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('running')

    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS)
    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('completed')
    expect(wrapper.get('[data-testid="request-trace-export-rows"]').text()).toContain('42')
    expect(wrapper.get('[data-testid="request-trace-export-skipped"]').text()).toContain('3')
    expect(wrapper.get('[data-testid="request-trace-export-bytes"]').text()).toContain('2048')

    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 5)
    expect(mocks.getTraceExport).toHaveBeenCalledTimes(2)
  })

  it('states plainly that completion and download deadline are not recorded yet', async () => {
    const wrapper = mountPanel()
    await createExport(wrapper)

    expect(wrapper.get('[data-testid="request-trace-export-completed"]').text()).toContain('notCompleted')
    expect(wrapper.get('[data-testid="request-trace-export-download-until"]').text()).toContain('unknownDeadline')
    expect(wrapper.get('[data-testid="request-trace-export-download"]').attributes('disabled')).toBeDefined()
  })

  it('offers download only while the server view reports the task as downloadable', async () => {
    mocks.createTraceExport.mockResolvedValue(completed())
    const wrapper = mountPanel()
    await createExport(wrapper)

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('completed')
    expect(wrapper.get('[data-testid="request-trace-export-download"]').attributes('disabled')).toBeUndefined()
  })

  it('refuses download once the server reports the window closed, without calling the API', async () => {
    mocks.createTraceExport.mockResolvedValue(completed({ download_until: AFTER_DEADLINE, downloadable: false }))
    const wrapper = mountPanel()
    await createExport(wrapper)

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('expired')
    expect(wrapper.get('[data-testid="request-trace-export-expired"]').exists()).toBe(true)
    await clickDownload(wrapper)
    expect(mocks.downloadTraceExport).not.toHaveBeenCalled()
  })

  it('reports a failed task without offering a download', async () => {
    mocks.createTraceExport.mockResolvedValue(task({ status: 'failed' }))
    const wrapper = mountPanel()
    await createExport(wrapper)

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('failed')
    expect(wrapper.get('[data-testid="request-trace-export-failure"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="request-trace-export-download"]').attributes('disabled')).toBeDefined()
  })

  it('downloads the temporary file once and persists no task handle', async () => {
    const blob = new Blob(['SYNTHETIC'])
    mocks.createTraceExport.mockResolvedValue(completed())
    mocks.downloadTraceExport.mockResolvedValue({ outcome: 'ready', blob, fileName: `sub2api-request-trace-export-${TASK_ID}.jsonl` })
    const wrapper = mountPanel()
    await createExport(wrapper)
    await clickDownload(wrapper)

    expect(mocks.downloadTraceExport).toHaveBeenCalledWith(TASK_ID, expect.anything())
    expect(URL.createObjectURL).toHaveBeenCalledTimes(1)
    expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:trace-export')
    expect(localStorage.length).toBe(0)
    expect(wrapper.find('[data-testid="request-trace-export-download-error"]').exists()).toBe(false)
  })

  it('reports a lost temporary file only from the server file-lost outcome', async () => {
    mocks.createTraceExport.mockResolvedValue(completed())
    mocks.downloadTraceExport.mockResolvedValue({ outcome: 'file_lost' })
    const wrapper = mountPanel()
    await createExport(wrapper)
    await clickDownload(wrapper)

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('file_lost')
    expect(wrapper.get('[data-testid="request-trace-export-file-lost"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="request-trace-export-download"]').attributes('disabled')).toBeDefined()
  })

  it('does not claim a lost file when the download was merely refused', async () => {
    mocks.createTraceExport.mockResolvedValue(completed())
    mocks.downloadTraceExport.mockResolvedValue({ outcome: 'unavailable' })
    const wrapper = mountPanel()
    await createExport(wrapper)
    await clickDownload(wrapper)

    expect(wrapper.get('[data-testid="request-trace-export-download-error"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="request-trace-export-file-lost"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('completed')
    expect(URL.createObjectURL).not.toHaveBeenCalled()
  })

  it('shows the closed window when the server refuses with the expiry reason', async () => {
    mocks.createTraceExport.mockResolvedValue(completed())
    mocks.downloadTraceExport.mockResolvedValue({ outcome: 'expired' })
    const wrapper = mountPanel()
    await createExport(wrapper)
    await clickDownload(wrapper)

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('expired')
    expect(wrapper.find('[data-testid="request-trace-export-file-lost"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="request-trace-export-download"]').attributes('disabled')).toBeDefined()
  })

  it('keeps an unreadable task status unknown instead of reporting failure', async () => {
    mocks.getTraceExport.mockRejectedValue(new Error('network down'))
    const wrapper = mountPanel()
    await createExport(wrapper)

    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS)
    expect(wrapper.get('[data-testid="request-trace-export-status-unknown"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('pending')
  })

  it('renders no unexpected body or credential fields and stops polling on unmount', async () => {
    mocks.createTraceExport.mockResolvedValue(task({
      payload_text: 'BODY_CANARY',
      authorization: 'Bearer CANARY',
      download_url: 'https://public.example/leak',
    }))
    const wrapper = mountPanel()
    await createExport(wrapper)

    expect(wrapper.text()).not.toContain('BODY_CANARY')
    expect(wrapper.text()).not.toContain('Bearer CANARY')
    expect(wrapper.text()).not.toContain('public.example')

    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS)
    const calls = mocks.getTraceExport.mock.calls.length
    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 5)
    expect(mocks.getTraceExport).toHaveBeenCalledTimes(calls)
  })

  it('registers no interval when the panel unmounts while the create POST is in flight', async () => {
    let resolveCreate!: (value: unknown) => void
    mocks.createTraceExport.mockImplementationOnce(() => new Promise(resolve => { resolveCreate = resolve }))
    const wrapper = mountPanel()
    await wrapper.get('[data-testid="request-trace-export-create"]').trigger('click')

    // The POST is still in flight when the panel goes away; the create call itself
    // must not leave a 3s poller alive for the life of the tab.
    wrapper.unmount()
    resolveCreate(task())
    await flushPromises()

    expect(vi.getTimerCount()).toBe(0)
    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 5)
    expect(mocks.getTraceExport).not.toHaveBeenCalled()
  })
})
