import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  listTraces: vi.fn(),
  getTrace: vi.fn(),
  route: { query: {} as Record<string, string> },
}))

vi.mock('../api', () => ({ listTraces: mocks.listTraces, getTrace: mocks.getTrace }))
vi.mock('vue-router', () => ({ useRoute: () => mocks.route }))
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import RequestTraceView from '../RequestTraceView.vue'

const trace = (overrides: Record<string, unknown> = {}) => ({
  trace_id: 'a'.repeat(32), route_family: 'messages', inbound_endpoint: '/v1/messages',
  created_at: '2026-09-28T00:00:00Z', completed_at: '2026-09-28T00:00:01Z',
  client_status: 200, capture_state: 'partial', usage_log_id: 4242,
  cleanup_after: null, ...overrides,
})
const page = (items: unknown[]) => ({ items, total: items.length, page: 1, page_size: 20 })

function mountView() {
  return mount(RequestTraceView, {
    global: { stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      Pagination: { template: '<div data-testid="trace-pagination" />' },
      RequestTraceDetailDrawer: { template: '<div data-testid="trace-detail-stub" />' },
      RequestTraceExportPanel: { template: '<div data-testid="request-trace-export-panel-stub" />' },
    } },
  })
}

/**
 * Navigation from an admin usage row arrives as `?usage_log_id=<id>`. The list must
 * apply that lookup immediately instead of loading every Trace and letting the
 * operator believe the row was located.
 */
describe('admin Request Trace lookup navigation', () => {
  beforeEach(() => {
    mocks.listTraces.mockReset()
    mocks.getTrace.mockReset()
    mocks.listTraces.mockResolvedValue(page([trace()]))
    mocks.route = { query: {} }
  })

  it('applies a usage_log_id from the route on load and shows it in the filter', async () => {
    mocks.route = { query: { usage_log_id: '4242' } }
    const wrapper = mountView()
    await flushPromises()
    expect(mocks.listTraces).toHaveBeenLastCalledWith(
      { page: 1, page_size: 20, usage_log_id: 4242 },
      expect.anything(),
    )
    expect((wrapper.get('[data-testid="request-trace-usage-filter"]').element as HTMLInputElement).value).toBe('4242')
  })

  it('applies an account_id from the route on load', async () => {
    mocks.route = { query: { account_id: '73' } }
    mountView()
    await flushPromises()
    expect(mocks.listTraces).toHaveBeenLastCalledWith(
      { page: 1, page_size: 20, account_id: 73 },
      expect.anything(),
    )
  })

  it('refuses a non-positive or non-numeric route lookup instead of sending it', async () => {
    mocks.route = { query: { usage_log_id: '0', account_id: 'not-a-number' } }
    mountView()
    await flushPromises()
    expect(mocks.listTraces).toHaveBeenLastCalledWith({ page: 1, page_size: 20 }, expect.anything())
  })

  it('sends a hand-typed account id only when it is a positive integer', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-testid="request-trace-account-filter"]').setValue('73')
    await wrapper.get('[data-testid="request-trace-search"]').trigger('click')
    await flushPromises()
    expect(mocks.listTraces).toHaveBeenLastCalledWith(
      { page: 1, page_size: 20, account_id: 73 },
      expect.anything(),
    )
  })

  it('rejects a hand-typed usage id that is not a positive integer', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-testid="request-trace-usage-filter"]').setValue('0')
    await wrapper.get('[data-testid="request-trace-search"]').trigger('click')
    expect(wrapper.get('[data-testid="request-trace-filter-error"]').exists()).toBe(true)
    expect(mocks.listTraces).toHaveBeenCalledTimes(1)
  })
})
