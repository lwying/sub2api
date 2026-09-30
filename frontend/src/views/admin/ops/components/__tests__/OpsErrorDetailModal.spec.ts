import { flushPromises, shallowMount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import OpsErrorDetailModal from '../OpsErrorDetailModal.vue'

const mocks = vi.hoisted(() => ({
  getRequestErrorDetail: vi.fn(),
  listRequestErrorUpstreamErrors: vi.fn()
}))

vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    getRequestErrorDetail: mocks.getRequestErrorDetail,
    getUpstreamErrorDetail: vi.fn(),
    listRequestErrorUpstreamErrors: mocks.listRequestErrorUpstreamErrors
  }
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({ showError: vi.fn() })
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

// 组件传的 to 一定是 { path, query }；stub 把它渲染成可断言的 href，
// 不依赖真实路由挂载。
const RouterLinkStub = {
  props: ['to'],
  computed: {
    href(this: { to: { path: string; query?: Record<string, string> } }) {
      const query = new URLSearchParams(Object.entries(this.to.query || {}).map(([key, value]) => [key, String(value)]))
      return query.size > 0 ? `${this.to.path}?${query.toString()}` : this.to.path
    }
  },
  template: '<a :href="href"><slot /></a>'
}

describe('OpsErrorDetailModal', () => {
  beforeEach(() => {
    mocks.getRequestErrorDetail.mockReset()
    mocks.listRequestErrorUpstreamErrors.mockReset()
    mocks.listRequestErrorUpstreamErrors.mockResolvedValue({ items: [] })
  })

  it('links to the request Trace only when the server says it is still readable', async () => {
    mocks.getRequestErrorDetail.mockResolvedValue({
      id: 1,
      created_at: '2026-08-19T00:00:00Z',
      phase: 'request',
      type: 'upstream_error',
      error_owner: 'provider',
      error_source: 'gateway',
      severity: 'P1',
      status_code: 502,
      platform: 'openai',
      model: 'gpt-5.6',
      resolved: false,
      request_id: 'rid-1',
      request_trace_id: 'a'.repeat(32),
      request_trace_available: true,
      message: 'boom',
      account_name: 'account',
      group_name: 'group',
      is_business_limited: false
    })

    const wrapper = shallowMount(OpsErrorDetailModal, {
      props: { show: true, errorId: 1, errorType: 'request' },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' }, Icon: true, RouterLink: RouterLinkStub } }
    })
    await flushPromises()

    await wrapper.get('[data-testid="ops-error-trace-link"]').trigger('click')
    expect(wrapper.get('[data-testid="ops-error-trace-panel"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="ops-error-trace-link"]').exists()).toBe(false)
    await wrapper.get('[data-testid="ops-error-trace-back"]').trigger('click')
    expect(wrapper.find('[data-testid="ops-error-trace-panel"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="ops-error-trace-link"]').exists()).toBe(true)
  })

  it('never links to a Trace that is gone, and never falls back to the client request id', async () => {
    mocks.getRequestErrorDetail.mockResolvedValue({
      id: 2,
      created_at: '2026-08-19T00:00:00Z',
      phase: 'request',
      type: 'upstream_error',
      error_owner: 'provider',
      error_source: 'gateway',
      severity: 'P1',
      status_code: 502,
      platform: 'openai',
      model: 'gpt-5.6',
      resolved: false,
      request_id: 'rid-can-be-reused',
      request_trace_id: 'b'.repeat(32),
      request_trace_available: false,
      message: 'boom',
      account_name: 'account',
      group_name: 'group',
      is_business_limited: false
    })

    const wrapper = shallowMount(OpsErrorDetailModal, {
      props: { show: true, errorId: 2, errorType: 'request' },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' }, Icon: true, RouterLink: RouterLinkStub } }
    })
    await flushPromises()

    expect(wrapper.find('[data-testid="ops-error-trace-link"]').exists()).toBe(false)
    expect(wrapper.html()).not.toContain('rid-can-be-reused'.repeat(1) + '&')
  })

  it('prioritizes upstream root cause and deduplicates diagnostic payloads', async () => {
    mocks.getRequestErrorDetail.mockResolvedValue({
      id: 1,
      created_at: '2026-08-19T00:00:00Z',
      phase: 'request',
      type: 'upstream_error',
      error_owner: 'provider',
      error_source: 'gateway',
      severity: 'P1',
      status_code: 502,
      upstream_status_code: 429,
      platform: 'openai',
      model: 'gpt-5.6',
      resolved: false,
      request_id: 'rid-1',
      message: 'All available accounts exhausted',
      error_body: '{"error":"same"}',
      upstream_error_message: 'provider rate limit exhausted',
      upstream_error_detail: '{"error":"same"}',
      upstream_errors: '[]',
      account_name: 'account',
      group_name: 'group',
      is_business_limited: false
    })

    const wrapper = shallowMount(OpsErrorDetailModal, {
      props: { show: true, errorId: 1, errorType: 'request' },
      global: {
        stubs: {
          BaseDialog: { template: '<div><slot /></div>' },
          Icon: true
        }
      }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('provider rate limit exhausted')
    expect(wrapper.text()).toContain('admin.ops.errorDetail.upstreamStatus')
    expect(wrapper.text()).toContain('429')
    expect(wrapper.findAll('pre')).toHaveLength(2)
    expect(wrapper.text()).not.toContain('admin.ops.errorDetail.payloads.upstream_detail')
  })
})
