import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({ listTraces: vi.fn(), getTrace: vi.fn() }))
vi.mock('../api', () => ({ listTraces: mocks.listTraces, getTrace: mocks.getTrace }))
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import RequestTraceView from '../RequestTraceView.vue'

const trace = (overrides: Record<string, unknown> = {}) => ({
  trace_id: 'a'.repeat(32), route_family: 'messages', inbound_endpoint: '/v1/messages',
  created_at: '2026-09-28T00:00:00Z', completed_at: '2026-09-28T00:00:01Z',
  client_status: 401, capture_state: 'partial', usage_log_id: null,
  cleanup_after: '2026-10-28T00:00:00Z', ...overrides,
})
const page = (items: unknown[]) => ({ items, total: items.length, page: 1, page_size: 20 })
const DetailStub = {
  props: ['show', 'traceId'], emits: ['update:show'],
  template: '<div data-testid="trace-detail-stub" :data-show="String(show)" :data-id="traceId || \'\'" />',
}
function mountView() {
  return mount(RequestTraceView, {
    global: { stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      Pagination: { template: '<div data-testid="trace-pagination" />' },
      RequestTraceDetailDrawer: DetailStub,
      RequestTraceExportPanel: { template: '<div data-testid="request-trace-export-panel-stub" />' },
    } },
  })
}

describe('admin Request Trace list', () => {
  beforeEach(() => {
    mocks.listTraces.mockReset()
    mocks.getTrace.mockReset()
    mocks.listTraces.mockResolvedValue(page([trace()]))
  })

  it('shows one logical request without rendering unexpected body or credential fields', async () => {
    mocks.listTraces.mockResolvedValue(page([trace({ payload_text: 'BODY_CANARY', authorization: 'Bearer CANARY' })]))
    const wrapper = mountView()
    await flushPromises()
    expect(mocks.listTraces).toHaveBeenCalledWith({ page: 1, page_size: 20 }, expect.anything())
    expect(wrapper.findAll('[data-testid="request-trace-row"]')).toHaveLength(1)
    expect(wrapper.get('[data-testid="request-trace-row"]').text()).toContain('/v1/messages')
    expect(wrapper.text()).not.toContain('BODY_CANARY')
    expect(wrapper.text()).not.toContain('Bearer CANARY')
    expect(mocks.getTrace).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="trace-detail-stub"]').attributes('data-show')).toBe('false')
  })

  it('marks the 30-day unlinked date as planned cleanup, not a strict read expiry', async () => {
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.get('[data-testid="request-trace-cleanup-rule"]').text()).toContain('plannedCleanup')
    expect(wrapper.get('[data-testid="request-trace-cleanup-rule"]').text()).not.toContain('expires')
  })

  it('uses only metadata filters and resets page for a changed trace id', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-testid="request-trace-id-filter"]').setValue('a'.repeat(32))
    await wrapper.get('[data-testid="request-trace-search"]').trigger('click')
    await flushPromises()
    expect(mocks.listTraces).toHaveBeenLastCalledWith({ page: 1, page_size: 20, trace_id: 'a'.repeat(32) }, expect.anything())
  })

  it('rejects an invalid Trace ID instead of silently returning unfiltered rows', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-testid="request-trace-id-filter"]').setValue('not-a-trace-id')
    await wrapper.get('[data-testid="request-trace-search"]').trigger('click')
    expect(wrapper.get('[data-testid="request-trace-filter-error"]').exists()).toBe(true)
    expect(mocks.listTraces).toHaveBeenCalledTimes(1)
  })

  it('opens the selected trace without fetching another body automatically', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-testid="request-trace-view"]').trigger('click')
    expect(wrapper.get('[data-testid="trace-detail-stub"]').attributes('data-id')).toBe('a'.repeat(32))
    expect(mocks.getTrace).not.toHaveBeenCalled()
  })

  it('renders the batch export panel alongside the list without starting an export', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-testid="request-trace-export-panel-stub"]').exists()).toBe(true)
    expect(mocks.listTraces).toHaveBeenCalledTimes(1)
  })
})
