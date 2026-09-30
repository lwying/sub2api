/**
 * The read-only hit list reads one page on demand and renders observable facts
 * only: what it did not record is shown as explicitly absent, a protocol outside
 * the closed set is labelled as unknown rather than echoed, and a read that could
 * not be completed never leaves the previous page standing as if it were current.
 */
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({ listEvents: vi.fn() }))
vi.mock('../api', () => mocks)
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import GatewayMockEventsPanel from '../GatewayMockEventsPanel.vue'
import { gatewayMockProtocols } from '../types'

type Raw = Record<string, unknown>

const event = (patch: Raw = {}): Raw => ({
  occurred_at: '2026-09-30T03:04:05Z',
  rule_id: 'gmr_0123456789abcdef',
  rule_version: '2026-09-30T03:00:00Z',
  protocol: 'messages',
  model: 'claude-sonnet-4-5',
  api_key_id: 7,
  user_id: 3,
  group_id: 2,
  account_id: 11,
  client_ip: '203.0.113.7',
  trace_id: '0123456789abcdef0123456789abcdef',
  cleanup_after: '2026-12-29T03:04:05Z',
  ...patch,
})

const page = (patch: Raw = {}): Raw => ({ items: [event()], total: 1, page: 1, page_size: 20, ...patch })

function mountPanel() {
  return mount(GatewayMockEventsPanel)
}

async function mountLoaded(payload: Raw = page()) {
  mocks.listEvents.mockResolvedValue(payload)
  const wrapper = mountPanel()
  await flushPromises()
  return wrapper
}

describe('admin gateway mock hit list', () => {
  beforeEach(() => mocks.listEvents.mockReset())
  afterEach(() => vi.restoreAllMocks())

  it('reads the first page on mount and renders the stored facts', async () => {
    const wrapper = await mountLoaded(page({ total: 1 }))

    expect(mocks.listEvents).toHaveBeenCalledTimes(1)
    expect(mocks.listEvents).toHaveBeenCalledWith(
      { page: 1, page_size: 20 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    )
    expect(wrapper.get('[data-testid="gateway-mock-events-rule-id"]').text()).toBe('gmr_0123456789abcdef')
    expect(wrapper.get('[data-testid="gateway-mock-events-rule-version"]').text()).toBe('2026-09-30T03:00:00Z')
    expect(wrapper.get('[data-testid="gateway-mock-events-api-key"]').text()).toBe('#7')
    expect(wrapper.get('[data-testid="gateway-mock-events-user"]').text()).toBe('#3')
    expect(wrapper.get('[data-testid="gateway-mock-events-group"]').text()).toBe('#2')
    expect(wrapper.get('[data-testid="gateway-mock-events-account"]').text()).toBe('#11')
    expect(wrapper.get('[data-testid="gateway-mock-events-client-ip"]').text()).toBe('203.0.113.7')
    expect(wrapper.get('[data-testid="gateway-mock-events-trace"]').text()).toBe('0123456789abcdef0123456789abcdef')
    expect(wrapper.get('[data-testid="gateway-mock-events-model"]').text()).toBe('claude-sonnet-4-5')
    expect(wrapper.get('[data-testid="gateway-mock-events-total"]').text()).toBe('admin.gatewayMock.events.total')
    expect(wrapper.get('[data-testid="gateway-mock-events-page"]').text()).toBe('admin.gatewayMock.events.page')
  })

  it('renders every value the gateway did not record as explicitly absent, never blank', async () => {
    const wrapper = await mountLoaded(page({
      items: [event({
        rule_version: '', model: '', api_key_id: 0, user_id: 0, group_id: 0,
        account_id: 0, client_ip: '', trace_id: '',
      })],
    }))

    const absent = 'admin.gatewayMock.events.absent'
    for (const testid of [
      'gateway-mock-events-rule-version', 'gateway-mock-events-model', 'gateway-mock-events-api-key',
      'gateway-mock-events-user', 'gateway-mock-events-group', 'gateway-mock-events-account',
      'gateway-mock-events-client-ip', 'gateway-mock-events-trace',
    ]) {
      expect(wrapper.get(`[data-testid="${testid}"]`).text(), testid).toBe(absent)
    }
    // 没有记录的编号不会渲染成 0 号。
    expect(wrapper.text()).not.toContain('#0')
    expect(wrapper.get('[data-testid="gateway-mock-events-absent-note"]').text()).toBe('admin.gatewayMock.events.absentNote')
  })

  it.each([...gatewayMockProtocols])('renders the %s protocol as its own label', async (protocol) => {
    const wrapper = await mountLoaded(page({ items: [event({ protocol })] }))
    const rendered = wrapper.get('[data-testid="gateway-mock-events-protocol"]').text()
    expect(rendered).toBe(`admin.gatewayMock.events.protocol.${protocol}`)
    expect(rendered).not.toBe(protocol)
  })

  it('never shows a raw protocol token when a value outside the closed set arrives', async () => {
    const wrapper = await mountLoaded(page({ items: [event({ protocol: 'invented_protocol' })] }))
    expect(wrapper.get('[data-testid="gateway-mock-events-protocol"]').text()).toBe('admin.gatewayMock.events.protocol.unknown')
    expect(wrapper.text()).not.toContain('invented_protocol')
  })

  it('renders no configured keyword and no reply text even if the answer carries them', async () => {
    const wrapper = await mountLoaded(page({
      items: [event({ keyword: 'CANARY_KEYWORD', reply: 'CANARY_REPLY', normalized_keyword: 'canary_keyword' })],
    }))

    expect(wrapper.get('[data-testid="gateway-mock-events-rule-id"]').text()).toBe('gmr_0123456789abcdef')
    expect(wrapper.text()).not.toContain('CANARY_KEYWORD')
    expect(wrapper.text()).not.toContain('CANARY_REPLY')
  })

  it('reports an unreadable list as unknown instead of an empty history', async () => {
    mocks.listEvents.mockRejectedValue({ status: 503, reason: 'GATEWAY_MOCK_EVENTS_UNAVAILABLE' })
    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.get('[data-testid="gateway-mock-events-failed"]').text()).toBe('admin.gatewayMock.events.failed')
    expect(wrapper.find('[data-testid="gateway-mock-events-row"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="gateway-mock-events-empty"]').exists()).toBe(false)
  })

  it('renders an empty list as "nothing recorded", not as a failure', async () => {
    const wrapper = await mountLoaded({ items: [], total: 0, page: 1, page_size: 20 })
    expect(wrapper.get('[data-testid="gateway-mock-events-empty"]').text()).toBe('admin.gatewayMock.events.empty')
    expect(wrapper.find('[data-testid="gateway-mock-events-failed"]').exists()).toBe(false)
  })

  it('pages forward and back on demand, and disables the ends', async () => {
    mocks.listEvents
      .mockResolvedValueOnce({ items: [event()], total: 25, page: 1, page_size: 20 })
      .mockResolvedValueOnce({ items: [event()], total: 25, page: 2, page_size: 20 })
      .mockResolvedValueOnce({ items: [event()], total: 25, page: 1, page_size: 20 })
    const wrapper = mountPanel()
    await flushPromises()

    const previous = wrapper.get('[data-testid="gateway-mock-events-prev"]')
    const next = wrapper.get('[data-testid="gateway-mock-events-next"]')
    expect((previous.element as HTMLButtonElement).disabled).toBe(true)
    expect((next.element as HTMLButtonElement).disabled).toBe(false)

    await next.trigger('click')
    await flushPromises()
    expect(mocks.listEvents).toHaveBeenLastCalledWith({ page: 2, page_size: 20 }, expect.objectContaining({ signal: expect.any(AbortSignal) }))
    expect((wrapper.get('[data-testid="gateway-mock-events-prev"]').element as HTMLButtonElement).disabled).toBe(false)
    expect((wrapper.get('[data-testid="gateway-mock-events-next"]').element as HTMLButtonElement).disabled).toBe(true)

    await wrapper.get('[data-testid="gateway-mock-events-prev"]').trigger('click')
    await flushPromises()
    expect(mocks.listEvents).toHaveBeenLastCalledWith({ page: 1, page_size: 20 }, expect.objectContaining({ signal: expect.any(AbortSignal) }))
  })

  it('refreshes only on demand and never starts a poller', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    try {
      const wrapper = await mountLoaded()
      expect(mocks.listEvents).toHaveBeenCalledTimes(1)

      await vi.advanceTimersByTimeAsync(60_000)
      expect(mocks.listEvents).toHaveBeenCalledTimes(1)
      expect(vi.getTimerCount()).toBe(0)

      await wrapper.get('[data-testid="gateway-mock-events-refresh"]').trigger('click')
      await flushPromises()
      expect(mocks.listEvents).toHaveBeenCalledTimes(2)
    } finally {
      vi.useRealTimers()
    }
  })

  it('keeps an older answer from overwriting a newer refresh', async () => {
    let resolveFirst!: (value: unknown) => void
    mocks.listEvents
      .mockImplementationOnce(() => new Promise((resolve) => { resolveFirst = resolve }))
      .mockResolvedValueOnce({ items: [event({ rule_id: 'gmr_newer' })], total: 1, page: 1, page_size: 20 })

    const wrapper = mountPanel()
    await wrapper.get('[data-testid="gateway-mock-events-refresh"]').trigger('click')
    await flushPromises()
    resolveFirst({ items: [event({ rule_id: 'gmr_older' })], total: 1, page: 1, page_size: 20 })
    await flushPromises()

    expect(wrapper.get('[data-testid="gateway-mock-events-rule-id"]').text()).toBe('gmr_newer')
    expect(wrapper.text()).not.toContain('gmr_older')
  })

  it('aborts an in-flight read on unmount and adopts no stale answer', async () => {
    let resolveRead!: (value: unknown) => void
    mocks.listEvents.mockImplementationOnce(
      (params: unknown, options?: { signal?: AbortSignal }) => new Promise((resolve) => {
        options?.signal?.addEventListener('abort', () => resolve(page({ items: [] })))
        resolveRead = resolve
      }),
    )
    const wrapper = mountPanel()
    const options = mocks.listEvents.mock.calls[0]?.[1] as { signal?: AbortSignal } | undefined
    expect(options?.signal).toBeInstanceOf(AbortSignal)

    wrapper.unmount()
    expect(options?.signal?.aborted).toBe(true)

    resolveRead(page())
    await flushPromises()
    expect(wrapper.find('[data-testid="gateway-mock-events-row"]').exists()).toBe(false)
  })
})
