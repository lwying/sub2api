/**
 * The wire contract of the downstream-test-request mock.
 *
 * The settings call is a whole-set replacement, so the payload a form sends has
 * to be exactly `{ enabled, rules }` — never a partial patch. The events call is
 * read-only metadata; a keyword or a reply text must not survive its parser,
 * because the route does not store either of them.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest'

const client = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: client }))

import { getOperatorSettings, listEvents, seedPresets, updateOperatorSettings } from '../api'

const status = {
  enabled: false,
  rules: [
    { id: 'gmr_1', keyword: 'hi', normalized_keyword: 'hi', reply: 'Hello!', enabled: true, updated_at: '2026-09-30T03:00:00Z' },
  ],
  preset_available: true,
  preset_created: 0,
}

const event = (patch: Record<string, unknown> = {}): Record<string, unknown> => ({
  occurred_at: '2026-09-30T03:04:05Z',
  rule_id: 'gmr_1',
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

describe('gateway mock API', () => {
  beforeEach(() => Object.values(client).forEach(mock => mock.mockReset()))

  it('reads the rule set without trusting anything the contract does not name', async () => {
    client.get.mockResolvedValue({ data: { ...status, body: 'BODY_CANARY', admin_api_key: 'sk-CANARY' } })
    const result = await getOperatorSettings()

    expect(client.get).toHaveBeenCalledWith('/admin/settings/gateway-mock', {
      headers: { 'Cache-Control': 'no-store', Pragma: 'no-cache' },
    })
    expect(result.enabled).toBe(false)
    expect(result.rules).toHaveLength(1)
    expect(JSON.stringify(result)).not.toContain('BODY_CANARY')
    expect(JSON.stringify(result)).not.toContain('sk-CANARY')
  })

  it('sends the whole rule set, not a patch, and nothing the API does not accept', async () => {
    client.put.mockResolvedValue({ data: { ...status, enabled: true } })
    const next = await updateOperatorSettings({
      enabled: true,
      rules: [{ id: 'gmr_1', keyword: 'hi', reply: 'Hello!', enabled: true }],
    })

    expect(client.put).toHaveBeenCalledWith('/admin/settings/gateway-mock', {
      enabled: true,
      rules: [{ id: 'gmr_1', keyword: 'hi', reply: 'Hello!', enabled: true }],
    }, { headers: { 'Cache-Control': 'no-store', Pragma: 'no-cache' } })
    // The extra wire-only fields of a rule are never echoed back into a request.
    expect(JSON.stringify(client.put.mock.calls[0][1])).not.toContain('normalized_keyword')
    expect(next.enabled).toBe(true)
  })

  it('reads the presets as seeded-but-disabled and reports how many were written', async () => {
    client.post.mockResolvedValue({ data: { status: { ...status, rules: [{ ...status.rules[0], enabled: false }] }, created: 7 } })
    const seed = await seedPresets()

    expect(client.post).toHaveBeenCalledWith('/admin/settings/gateway-mock/presets', null, {
      headers: { 'Cache-Control': 'no-store', Pragma: 'no-cache' },
    })
    expect(seed.created).toBe(7)
    expect(seed.status.rules.every(rule => !rule.enabled)).toBe(true)
  })

  it('reads one page of minimal events with no content filter at all', async () => {
    client.get.mockResolvedValue({ data: { items: [event()], total: 1, page: 2, page_size: 20 } })
    const controller = new AbortController()
    const page = await listEvents({ page: 2, page_size: 20 }, { signal: controller.signal })

    const [url, config] = client.get.mock.calls[0] as [string, { params?: unknown; headers?: unknown; signal?: AbortSignal }]
    expect(url).toBe('/admin/settings/gateway-mock/events')
    expect(config.params).toEqual({ page: 2, page_size: 20 })
    expect(config.headers).toEqual({ 'Cache-Control': 'no-store', Pragma: 'no-cache' })
    expect(config.signal).toBe(controller.signal)
    expect(page.total).toBe(1)
    expect(page.items[0].rule_id).toBe('gmr_1')
    expect(page.items[0].client_ip).toBe('203.0.113.7')
  })

  it('keeps no configured keyword and no reply text even when the answer carries them', async () => {
    client.get.mockResolvedValue({
      data: {
        items: [event({ keyword: 'CANARY_KEYWORD', reply: 'CANARY_REPLY', normalized_keyword: 'canary_keyword' })],
        total: 1,
        page: 1,
        page_size: 20,
      },
    })
    const page = await listEvents({ page: 1, page_size: 20 })

    const serialized = JSON.stringify(page)
    for (const canary of ['CANARY_KEYWORD', 'CANARY_REPLY', 'canary_keyword', 'keyword', 'reply']) {
      expect(serialized, `the event projection must not carry ${canary}`).not.toContain(canary)
    }
    expect(Object.keys(page.items[0]).sort()).toEqual([
      'account_id', 'api_key_id', 'cleanup_after', 'client_ip', 'group_id', 'model',
      'occurred_at', 'protocol', 'rule_id', 'rule_version', 'trace_id', 'user_id',
    ])
  })

  it('keeps an unrecorded cleanup deadline as null instead of guessing one', async () => {
    client.get.mockResolvedValue({
      data: { items: [event({ cleanup_after: null })], total: 1, page: 1, page_size: 20 },
    })
    const page = await listEvents({ page: 1, page_size: 20 })

    expect(page.items[0].cleanup_after).toBeNull()
    expect('cleanup_after' in page.items[0]).toBe(true)
  })

  it('refuses an unreadable rule set instead of reporting it as an empty, disabled one', async () => {
    for (const payload of [
      { ...status, enabled: 'false' },
      { ...status, rules: null },
      { ...status, rules: [{ ...status.rules[0], enabled: 1 }] },
      { ...status, rules: [{ ...status.rules[0], keyword: 'x'.repeat(201) }] },
      { enabled: false, rules: [] },
      [],
    ]) {
      client.get.mockResolvedValue({ data: payload })
      await expect(getOperatorSettings()).rejects.toThrow()
    }
  })

  it('refuses a page that is not a bounded, readable page of events', async () => {
    for (const payload of [
      { items: [{ ...event(), api_key_id: -1 }], total: 1, page: 1, page_size: 20 },
      { items: [{ ...event(), occurred_at: 'not a time' }], total: 1, page: 1, page_size: 20 },
      { items: [{ ...event(), cleanup_after: 'not a time' }], total: 1, page: 1, page_size: 20 },
      { items: [], total: -1, page: 1, page_size: 20 },
      { items: [], total: 0, page: 0, page_size: 20 },
      { items: [], total: 0, page: 1 },
      { items: Array.from({ length: 101 }, () => event()), total: 101, page: 1, page_size: 100 },
    ]) {
      client.get.mockResolvedValue({ data: payload })
      await expect(listEvents({ page: 1, page_size: 20 })).rejects.toThrow()
    }
  })
})
