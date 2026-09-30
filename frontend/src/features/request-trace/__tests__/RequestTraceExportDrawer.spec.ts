/**
 * The export task drawer.
 *
 * Three honesty rules drive these cases. A truncated task is never rendered as
 * success — it has its own state and its own reason code. A refused file is
 * refused as that file, not as the whole task. And the drawer only offers a
 * download while the server's own verdict for this session says it may: the
 * local clock is never asked.
 */
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  getTraceExport: vi.fn(),
  downloadTraceExport: vi.fn(),
  downloadTraceExportPart: vi.fn(),
}))

vi.mock('../api', () => mocks)
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import RequestTraceExportDrawer from '../RequestTraceExportDrawer.vue'

const POLL_INTERVAL_MS = 3000
const TASK_ID = 'b'.repeat(32)

/** Mirrors the server task view, including the completeness fields. */
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
  shard_count: 0,
  truncated: false,
  ...overrides,
})

const completed = (overrides: Record<string, unknown> = {}) => task({
  status: 'completed', completed_at: '2026-09-28T00:05:00Z',
  download_until: '2026-10-05T00:05:00Z', downloadable: true, shard_count: 2, ...overrides,
})

const BaseDialogStub = {
  props: ['show', 'title'], emits: ['close'],
  template: '<div v-if="show" data-testid="export-dialog"><slot /></div>',
}

function mountDrawer(taskID: string | null = TASK_ID) {
  return mount(RequestTraceExportDrawer, {
    props: { show: true, taskId: taskID },
    global: { stubs: { BaseDialog: BaseDialogStub } },
  })
}

async function mountLoaded(payload: unknown = completed()) {
  mocks.getTraceExport.mockResolvedValue(payload)
  const wrapper = mountDrawer()
  await flushPromises()
  return wrapper
}

describe('admin Request Trace export task drawer', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] })
    vi.setSystemTime(new Date('2026-09-28T00:00:00Z'))
    mocks.getTraceExport.mockReset()
    mocks.downloadTraceExport.mockReset()
    mocks.downloadTraceExportPart.mockReset()
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

  it('reads the task by id and states that only the creating session may download it', async () => {
    const wrapper = await mountLoaded()

    expect(mocks.getTraceExport).toHaveBeenCalledWith(TASK_ID, expect.anything())
    expect(wrapper.get('[data-testid="request-trace-export-task-session-note"]').text())
      .toBe('admin.requestTrace.export.task.sessionOnly')
  })

  it('renders a truncated task as incomplete rather than as a completed export', async () => {
    const wrapper = await mountLoaded(completed({
      truncated: true, incomplete_reason: 'limit_rows', rows_exported: 12, shard_count: 1,
    }))

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('incomplete')
    expect(wrapper.get('[data-testid="request-trace-export-state"]').text()).toBe('admin.requestTrace.export.state.incomplete')
    expect(wrapper.get('[data-testid="request-trace-export-state"]').text()).not.toBe('admin.requestTrace.export.state.completed')
    expect(wrapper.get('[data-testid="request-trace-export-incomplete"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="request-trace-export-incomplete-reason"]').text())
      .toBe('admin.requestTrace.export.reason.limit_rows')
  })

  it('shows a completed task whose manifest is gone as file lost, not as incomplete', async () => {
    // The server could not read the manifest, so nothing is known about what this
    // export delivered — and the server also refuses the download. The truncation
    // sentence is about which part of the *source* was never seen, which is a
    // different claim, so it must not stand in for the missing file.
    const wrapper = await mountLoaded(completed({
      truncated: true, incomplete_reason: 'manifest_lost', downloadable: false, shard_count: 0,
    }))

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('file_lost')
    expect(wrapper.get('[data-testid="request-trace-export-state"]').text()).toBe('admin.requestTrace.export.state.file_lost')
    expect(wrapper.get('[data-testid="request-trace-export-file-lost"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="request-trace-export-incomplete"]').exists()).toBe(false)
    // Nothing to download: the shard list lives in the manifest that is gone.
    expect(wrapper.get('[data-testid="request-trace-export-manifest"]').attributes('disabled')).toBeDefined()
  })

  it('breaks the skipped total down by reason instead of hiding the two facts behind one number', async () => {
    const wrapper = await mountLoaded(completed({
      truncated: true, incomplete_reason: 'read_failed', rows_skipped: 5, shard_count: 1,
      skipped_by_reason: { read_failed: 2, source_gone: 3 },
    }))

    // The aggregate stays the authoritative total; the breakdown is its parts.
    expect(wrapper.get('[data-testid="request-trace-export-skipped"]').text()).toBe('5')
    const readFailed = wrapper.get('[data-testid="request-trace-export-skipped-reason-read_failed"]')
    const sourceGone = wrapper.get('[data-testid="request-trace-export-skipped-reason-source_gone"]')
    expect(readFailed.attributes('data-reason')).toBe('read_failed')
    expect(readFailed.text()).toBe('admin.requestTrace.export.reason.read_failed: 2')
    expect(sourceGone.text()).toBe('admin.requestTrace.export.reason.source_gone: 3')
    // A typed breakdown is not a smaller success: the task is still incomplete.
    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('incomplete')
  })

  it('shows no per-reason breakdown when the task skipped nothing', async () => {
    const wrapper = await mountLoaded(completed({ rows_skipped: 0, skipped_by_reason: { read_failed: 0, source_gone: 0 } }))

    expect(wrapper.find('[data-testid="request-trace-export-skipped-by-reason"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="request-trace-export-skipped"]').text()).toBe('0')
  })

  it('labels an incomplete task whose reason this version does not know, and still calls it incomplete', async () => {
    const wrapper = await mountLoaded(completed({ truncated: true, incomplete_reason: 'some_new_reason' }))

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('incomplete')
    expect(wrapper.get('[data-testid="request-trace-export-incomplete-reason"]').text())
      .toBe('admin.requestTrace.export.reason.unknown')
    expect(wrapper.text()).not.toContain('some_new_reason')
  })

  it('renders a complete task as complete and states the non-snapshot caveat', async () => {
    const wrapper = await mountLoaded()

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('completed')
    expect(wrapper.find('[data-testid="request-trace-export-incomplete"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="request-trace-export-not-snapshot"]').text())
      .toBe('admin.requestTrace.export.progress.notSnapshot')
    expect(wrapper.get('[data-testid="request-trace-export-shard-count"]').text()).toContain('2')
  })

  it('downloads the manifest without a part and shard N with part=N', async () => {
    mocks.downloadTraceExport.mockResolvedValue({ outcome: 'ready', blob: new Blob(['M']), fileName: 'manifest.json' })
    mocks.downloadTraceExportPart.mockResolvedValue({ outcome: 'ready', blob: new Blob(['S']), fileName: 'part0002.jsonl' })
    const wrapper = await mountLoaded()

    await wrapper.get('[data-testid="request-trace-export-manifest"]').trigger('click')
    await flushPromises()
    expect(mocks.downloadTraceExport).toHaveBeenCalledWith(TASK_ID, expect.anything())
    expect(mocks.downloadTraceExportPart).not.toHaveBeenCalled()

    await wrapper.get('[data-testid="request-trace-export-shard-2"]').trigger('click')
    await flushPromises()
    expect(mocks.downloadTraceExportPart).toHaveBeenCalledWith(TASK_ID, 2, expect.anything())
    expect(URL.createObjectURL).toHaveBeenCalledTimes(2)
  })

  it('offers exactly the shards the task says it wrote', async () => {
    const wrapper = await mountLoaded(completed({ shard_count: 3 }))

    expect(wrapper.find('[data-testid="request-trace-export-shard-1"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="request-trace-export-shard-2"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="request-trace-export-shard-3"]').exists()).toBe(true)
    // An index the server never wrote is not offered: it is refused, not guessed.
    expect(wrapper.find('[data-testid="request-trace-export-shard-4"]').exists()).toBe(false)
  })

  it('does not offer any download while the server refuses this session', async () => {
    const wrapper = await mountLoaded(completed({ downloadable: false }))

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('expired')
    expect(wrapper.get('[data-testid="request-trace-export-expired"]').exists()).toBe(true)
    expect((wrapper.get('[data-testid="request-trace-export-manifest"]').element as HTMLButtonElement).disabled).toBe(true)
    expect((wrapper.get('[data-testid="request-trace-export-shard-1"]').element as HTMLButtonElement).disabled).toBe(true)
    await wrapper.get('[data-testid="request-trace-export-manifest"]').trigger('click')
    await wrapper.get('[data-testid="request-trace-export-shard-1"]').trigger('click')
    expect(mocks.downloadTraceExport).not.toHaveBeenCalled()
    expect(mocks.downloadTraceExportPart).not.toHaveBeenCalled()
  })

  it('reports a lost shard as that shard being gone, not as the whole task failing', async () => {
    mocks.downloadTraceExportPart.mockResolvedValue({ outcome: 'file_lost' })
    const wrapper = await mountLoaded()

    await wrapper.get('[data-testid="request-trace-export-shard-2"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('completed')
    expect(wrapper.get('[data-testid="request-trace-export-lost-part-2"]').exists()).toBe(true)
    expect((wrapper.get('[data-testid="request-trace-export-shard-2"]').element as HTMLButtonElement).disabled).toBe(true)
    // The manifest and the other shard are unaffected by one lost file.
    expect(wrapper.find('[data-testid="request-trace-export-lost-manifest"]').exists()).toBe(false)
    expect((wrapper.get('[data-testid="request-trace-export-shard-1"]').element as HTMLButtonElement).disabled).toBe(false)
  })

  it('reports a lost manifest without claiming a shard is gone', async () => {
    mocks.downloadTraceExport.mockResolvedValue({ outcome: 'file_lost' })
    const wrapper = await mountLoaded()

    await wrapper.get('[data-testid="request-trace-export-manifest"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="request-trace-export-file-lost"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="request-trace-export-lost-part-1"]').exists()).toBe(false)
  })

  it('stops every download and says so when the server reports the window closed', async () => {
    mocks.downloadTraceExportPart.mockResolvedValue({ outcome: 'expired' })
    const wrapper = await mountLoaded()

    await wrapper.get('[data-testid="request-trace-export-shard-1"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="request-trace-export-file-expired"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="request-trace-export-lost-files"]').exists()).toBe(false)
    for (const testid of ['request-trace-export-manifest', 'request-trace-export-shard-1']) {
      expect((wrapper.get(`[data-testid="${testid}"]`).element as HTMLButtonElement).disabled).toBe(true)
    }
  })

  it('treats a not-yet-ready task as retryable, not as expired or lost', async () => {
    mocks.downloadTraceExportPart.mockResolvedValue({ outcome: 'not_ready' })
    const wrapper = await mountLoaded()

    await wrapper.get('[data-testid="request-trace-export-shard-1"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="request-trace-export-not-ready"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="request-trace-export-file-expired"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="request-trace-export-lost-files"]').exists()).toBe(false)
    // 尚未就绪只是"还没好"：下载入口不被永久禁用。
    expect((wrapper.get('[data-testid="request-trace-export-shard-1"]').element as HTMLButtonElement).disabled).toBe(false)
  })

  it('reports a refused download without claiming any file was lost', async () => {
    mocks.downloadTraceExport.mockResolvedValue({ outcome: 'unavailable' })
    const wrapper = await mountLoaded()

    await wrapper.get('[data-testid="request-trace-export-manifest"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="request-trace-export-download-error"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="request-trace-export-file-lost"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="request-trace-export-lost-files"]').exists()).toBe(false)
    expect(URL.createObjectURL).not.toHaveBeenCalled()
  })

  it('polls a running task until it completes, then stops', async () => {
    mocks.getTraceExport
      .mockResolvedValueOnce(task({ status: 'running' }))
      .mockResolvedValueOnce(completed({ rows_exported: 42, rows_skipped: 3, bytes_exported: 2048 }))
    const wrapper = mountDrawer()
    await flushPromises()

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('running')
    // 排队/进行中不是"过期"：窗口只是还没开始计。把两者说成一回事会让管理员
    // 以为任务白跑了，而这正是他们盯着抽屉的时刻。
    expect(wrapper.find('[data-testid="request-trace-export-expired"]').exists()).toBe(false)

    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS)
    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('completed')
    expect(wrapper.get('[data-testid="request-trace-export-rows"]').text()).toContain('42')
    expect(wrapper.get('[data-testid="request-trace-export-skipped"]').text()).toContain('3')
    expect(wrapper.get('[data-testid="request-trace-export-bytes"]').text()).toContain('2048')

    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 5)
    expect(mocks.getTraceExport).toHaveBeenCalledTimes(2)
  })

  it('keeps an unreadable task unknown instead of reporting failure', async () => {
    mocks.getTraceExport.mockRejectedValue(new Error('network down'))
    const wrapper = mountDrawer()
    await flushPromises()

    expect(wrapper.get('[data-testid="request-trace-export-task-unreadable"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="request-trace-export-failure"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('network down')
  })

  it('reports a failed task without offering any download', async () => {
    const wrapper = await mountLoaded(task({ status: 'failed' }))
    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS)

    expect(wrapper.get('[data-testid="request-trace-export-task"]').attributes('data-state')).toBe('failed')
    expect(wrapper.get('[data-testid="request-trace-export-failure"]').exists()).toBe(true)
    expect((wrapper.get('[data-testid="request-trace-export-manifest"]').element as HTMLButtonElement).disabled).toBe(true)
    await wrapper.get('[data-testid="request-trace-export-manifest"]').trigger('click')
    expect(mocks.downloadTraceExport).not.toHaveBeenCalled()
  })

  it('says nothing about a task when there is no task handle', async () => {
    const wrapper = mountDrawer(null)
    await flushPromises()

    expect(wrapper.get('[data-testid="request-trace-export-task-absent"]').exists()).toBe(true)
    expect(mocks.getTraceExport).not.toHaveBeenCalled()
  })

  it('renders no unexpected body, credential or path field from the task view', async () => {
    const wrapper = await mountLoaded(completed({
      payload_text: 'BODY_CANARY',
      authorization: 'Bearer CANARY',
      file_path: '/var/tmp/secret',
      session_digest: 'deadbeef',
    }))

    expect(wrapper.text()).not.toContain('BODY_CANARY')
    expect(wrapper.text()).not.toContain('Bearer CANARY')
    expect(wrapper.text()).not.toContain('/var/tmp/secret')
    expect(wrapper.text()).not.toContain('deadbeef')
  })

  it('stops polling when it unmounts', async () => {
    mocks.getTraceExport.mockResolvedValue(task({ status: 'running' }))
    const wrapper = mountDrawer()
    await flushPromises()
    expect(vi.getTimerCount()).toBe(1)

    wrapper.unmount()
    expect(vi.getTimerCount()).toBe(0)
    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 5)
    expect(mocks.getTraceExport).toHaveBeenCalledTimes(1)
  })
})
