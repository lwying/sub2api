import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => {
  vi.stubGlobal('localStorage', {
    getItem: vi.fn(() => null),
    setItem: vi.fn(),
    removeItem: vi.fn(),
  })

  return {
    list: vi.fn(),
    exportList: vi.fn(),
    getStats: vi.fn(),
    getSnapshotV2: vi.fn(),
    getModelStats: vi.fn(),
    getById: vi.fn(),
    // Replaced with a reactive route by the vue-router mock below, so route
    // changes made after mount are observed like they are in the app.
    route: { query: {} } as { query: Record<string, string> },
  }
})

const messages: Record<string, string> = {
  'admin.usage.exactRecordFilter': 'Showing only usage record #{id}',
  'common.clear': 'Clear',
}

function translate(key: string, params?: Record<string, unknown>): string {
  const message = messages[key]
  if (message === undefined) return key
  if (params === undefined) return message
  return message.replace(/\{(\w+)\}/g, (match, name: string) => (
    params[name] === undefined ? match : String(params[name])
  ))
}

vi.mock('@/api/admin', () => ({
  adminAPI: {
    usage: {
      list: mocks.list,
      getStats: mocks.getStats,
    },
    dashboard: {
      getSnapshotV2: mocks.getSnapshotV2,
      getModelStats: mocks.getModelStats,
    },
    users: {
      getById: mocks.getById,
    },
  },
}))

vi.mock('@/api/admin/usage', () => ({
  adminUsageAPI: {
    list: mocks.exportList,
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showWarning: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn(),
  }),
}))

vi.mock('@/utils/format', () => ({
  formatReasoningEffort: (value: string | null | undefined) => value ?? '-',
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: translate }),
  }
})

vi.mock('vue-router', async () => {
  const { reactive } = await import('vue')
  mocks.route = reactive({ query: {} as Record<string, string> })
  return {
    useRoute: () => mocks.route,
    useRouter: () => ({ push: vi.fn() }),
  }
})

import UsageView from '../UsageView.vue'

const AppLayoutStub = { template: '<div><slot /></div>' }
const UsageFiltersStub = defineComponent({
  setup(_, { expose }) {
    expose({ getUserSearchRevision: () => 0, setUserKeyword: () => {} })
    return {}
  },
  template: '<div><slot name="after-reset" /></div>',
})

function mountUsageView() {
  return mount(UsageView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub, UsageStatsCards: true, UsageFilters: UsageFiltersStub,
        UsageTable: true, UsageExportProgress: true, UsageCleanupDialog: true,
        UserBalanceHistoryModal: true, Pagination: true, Select: true,
        DateRangePicker: true, Icon: true, TokenUsageTrend: true,
        ModelDistributionChart: true, GroupDistributionChart: true,
        EndpointDistributionChart: true, UserTokenRanking: true,
        OpsErrorLogTable: true, OpsErrorDetailModal: true,
      },
    },
  })
}

/**
 * The request Trace detail links a usage record as `/admin/usage?usage_log_id=<id>`.
 * The list must open that exact record — the metering facts the Trace pointed at —
 * instead of the unfiltered list the operator already saw. Only a positive integer
 * is an exact lookup; anything else must not be forwarded as a filter.
 */
describe('admin UsageView exact usage record lookup', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    Object.keys(mocks.route.query).forEach((key) => delete mocks.route.query[key])
    mocks.list.mockReset().mockResolvedValue({ items: [], total: 0, pages: 0 })
    mocks.exportList.mockReset()
    mocks.getStats.mockReset().mockResolvedValue({
      total_requests: 0, total_input_tokens: 0, total_output_tokens: 0,
      total_cache_tokens: 0, total_tokens: 0, total_cost: 0, total_actual_cost: 0, average_duration_ms: 0,
    })
    mocks.getSnapshotV2.mockReset().mockResolvedValue({ trend: [], models: [], groups: [] })
    mocks.getModelStats.mockReset().mockResolvedValue({ models: [] })
    mocks.getById.mockReset()
  })

  afterEach(() => {
    Object.keys(mocks.route.query).forEach((key) => delete mocks.route.query[key])
    vi.useRealTimers()
  })

  it('opens the routed usage record instead of the unfiltered list', async () => {
    mocks.route.query.usage_log_id = '4242'

    const wrapper = mountUsageView()
    vi.advanceTimersByTime(120)
    await flushPromises()

    // The record is located by id, so the default 24h range must not hide an older
    // record: the id is the exact locator the Trace handed over.
    expect(mocks.list).toHaveBeenCalledWith(
      expect.objectContaining({ usage_log_id: 4242, start_date: undefined, end_date: undefined }),
      expect.anything(),
    )
    expect(wrapper.get('[data-testid="usage-exact-record"]').text()).toContain('#4242')
  })

  it('refuses a routed usage id that is not a positive integer', async () => {
    for (const raw of ['0', '-3', 'abc', '1.5']) {
      mocks.list.mockClear()
      Object.keys(mocks.route.query).forEach((key) => delete mocks.route.query[key])
      mocks.route.query.usage_log_id = raw

      const wrapper = mountUsageView()
      vi.advanceTimersByTime(120)
      await flushPromises()

      expect(mocks.list, `usage_log_id=${raw} must not be sent as a filter`).toHaveBeenCalledWith(
        expect.not.objectContaining({ usage_log_id: expect.anything() }),
        expect.anything(),
      )
      expect(mocks.list.mock.calls[0][0].start_date).toEqual(expect.any(String))
      expect(wrapper.find('[data-testid="usage-exact-record"]').exists()).toBe(false)
      wrapper.unmount()
    }
  })

  it('clears the routed record and restores the list scope when the operator clears it', async () => {
    mocks.route.query.usage_log_id = '4242'

    const wrapper = mountUsageView()
    vi.advanceTimersByTime(120)
    await flushPromises()

    mocks.list.mockClear()
    await wrapper.get('[data-testid="usage-exact-record-clear"]').trigger('click')
    await flushPromises()

    expect(mocks.list).toHaveBeenCalledWith(
      expect.not.objectContaining({ usage_log_id: expect.anything() }),
      expect.anything(),
    )
    expect(mocks.list.mock.calls[0][0].start_date).toEqual(expect.any(String))
    expect(wrapper.find('[data-testid="usage-exact-record"]').exists()).toBe(false)
  })

  it('follows a record lookup that arrives while the page is already open', async () => {
    const wrapper = mountUsageView()
    vi.advanceTimersByTime(120)
    await flushPromises()

    mocks.list.mockClear()
    mocks.route.query.usage_log_id = '9'
    await flushPromises()

    expect(mocks.list).toHaveBeenLastCalledWith(
      expect.objectContaining({ usage_log_id: 9 }),
      expect.anything(),
    )
    expect(wrapper.get('[data-testid="usage-exact-record"]').text()).toContain('#9')
  })

  it('drops the routed record when the operator resets filters', async () => {
    mocks.route.query.usage_log_id = '4242'

    const wrapper = mountUsageView()
    vi.advanceTimersByTime(120)
    await flushPromises()

    mocks.list.mockClear()
    ;(wrapper.vm as any).resetFilters()
    await flushPromises()

    expect(mocks.list).toHaveBeenCalledWith(
      expect.not.objectContaining({ usage_log_id: expect.anything() }),
      expect.anything(),
    )
    expect(wrapper.find('[data-testid="usage-exact-record"]').exists()).toBe(false)
  })
})
