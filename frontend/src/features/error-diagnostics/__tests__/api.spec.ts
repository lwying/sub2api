/**
 * API seam for the admin error-request diagnostics (tickets 01-04).
 *
 * The v1 contract is one row per real upstream attempt, metadata only, opaque
 * string id, and a stored body that never appears in list/detail but only in an
 * explicit no-store reveal POST.
 *
 * `body_state` and `reason` literals below are the authoritative persisted
 * enums from the diagnostics storage layer (not_observed / stored / skipped /
 * expired / purged and the skipped_* reason codes). They are closed sets: an
 * unknown literal must be rejected or dropped, never rendered.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest'

const client = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: client }))

import { getDiagnostic, listDiagnostics, revealDiagnosticBody } from '../api'

const attempt = (overrides: Record<string, unknown> = {}) => ({
  id: 'diag_01HXYZ',
  created_at: '2026-09-24T00:00:00Z',
  protocol: 'messages',
  attempt_index: 1,
  upstream_status: 400,
  usage_log_id: 77,
  body_state: 'stored',
  reason: 'retained',
  body_expires_at: '2026-10-01T00:00:00Z',
  metadata_expires_at: '2026-10-24T00:00:00Z',
  ...overrides,
})

const page_ = (items: unknown[]) => ({ items, total: items.length, page: 1, page_size: 20, pages: 1 })

describe('error diagnostics API', () => {
  beforeEach(() => Object.values(client).forEach((mock) => mock.mockReset()))

  it('lists per-attempt rows from the dedicated admin namespace', async () => {
    client.get.mockResolvedValue({ data: page_([attempt()]) })

    const result = await listDiagnostics({ page: 1, page_size: 20 })

    expect(client.get).toHaveBeenCalledWith(
      '/admin/error-diagnostics',
      expect.objectContaining({ params: expect.objectContaining({ page: 1, page_size: 20 }) }),
    )
    expect(result.items).toHaveLength(1)
    expect(result.items[0]).toMatchObject({
      id: 'diag_01HXYZ',
      protocol: 'messages',
      attempt_index: 1,
      upstream_status: 400,
      body_state: 'stored',
      reason: 'retained',
    })
    expect(result.total).toBe(1)
  })

  it('accepts every authoritative body_state and reason literal', async () => {
    client.get.mockResolvedValue({
      data: page_([
        attempt({ id: 'a', body_state: 'not_observed', reason: 'not_observed', body_expires_at: undefined }),
        attempt({ id: 'b', body_state: 'stored', reason: 'retained' }),
        attempt({ id: 'c', body_state: 'skipped', reason: 'skipped_not_text_json', body_expires_at: undefined }),
        attempt({ id: 'd', body_state: 'expired', reason: 'retained' }),
        attempt({ id: 'e', body_state: 'purged', reason: 'retained' }),
        attempt({ id: 'f', body_state: 'skipped', reason: 'skipped_too_large', body_expires_at: undefined }),
        attempt({ id: 'g', body_state: 'skipped', reason: 'skipped_attachment', body_expires_at: undefined }),
        attempt({ id: 'h', body_state: 'skipped', reason: 'skipped_known_credential', body_expires_at: undefined }),
        attempt({ id: 'i', body_state: 'skipped', reason: 'skipped_incomplete_read', body_expires_at: undefined }),
        attempt({ id: 'j', body_state: 'skipped', reason: 'skipped_encryption_unavailable', body_expires_at: undefined }),
        attempt({ id: 'k', body_state: 'skipped', reason: 'skipped_body_retention_disabled', body_expires_at: undefined }),
      ]),
    })

    const result = await listDiagnostics({ page: 1, page_size: 20 })

    expect(result.items.map((item) => item.body_state)).toEqual([
      'not_observed', 'stored', 'skipped', 'expired', 'purged',
      'skipped', 'skipped', 'skipped', 'skipped', 'skipped', 'skipped',
    ])
  })

  it('never lets a request body, credential or model reach a list consumer', async () => {
    client.get.mockResolvedValue({
      data: page_([
        attempt({
          body_text: 'BODY_CANARY_DO_NOT_RENDER',
          model: 'model-canary',
          account_id: 42,
          api_key: 'sk-canary',
          authorization: 'Bearer canary',
        }),
      ]),
    })

    const serialized = JSON.stringify(await listDiagnostics({ page: 1, page_size: 20 }))

    expect(serialized).not.toContain('BODY_CANARY_DO_NOT_RENDER')
    expect(serialized).not.toContain('model-canary')
    expect(serialized).not.toContain('sk-canary')
    expect(serialized).not.toContain('Bearer canary')
    expect(serialized).not.toContain('body_text')
    expect(serialized).not.toContain('account_id')
  })

  it('rejects a row whose closed-set field carries an unknown value', async () => {
    client.get.mockResolvedValue({ data: page_([attempt({ protocol: 'raw_url_leak' })]) })
    await expect(listDiagnostics({ page: 1, page_size: 20 })).rejects.toThrow()

    client.get.mockResolvedValue({ data: page_([attempt({ body_state: 'retained' })]) })
    await expect(listDiagnostics({ page: 1, page_size: 20 })).rejects.toThrow()
  })

  it('drops an unknown reason instead of rendering untrusted text', async () => {
    client.get.mockResolvedValue({
      data: page_([attempt({ body_state: 'skipped', reason: '<script>alert(1)</script>', body_expires_at: undefined })]),
    })

    const result = await listDiagnostics({ page: 1, page_size: 20 })

    expect(result.items[0].reason).toBeUndefined()
  })

  it('drops a body expiry that is not a usable timestamp', async () => {
    client.get.mockResolvedValue({ data: page_([attempt({ body_expires_at: 'not-a-date' })]) })

    const result = await listDiagnostics({ page: 1, page_size: 20 })

    expect(result.items[0].body_expires_at).toBeUndefined()
  })

  it('fetches one attempt by opaque id and still carries no body', async () => {
    client.get.mockResolvedValue({ data: attempt() })

    const detail = await getDiagnostic('diag_01HXYZ')

    expect(client.get).toHaveBeenCalledWith('/admin/error-diagnostics/diag_01HXYZ', expect.anything())
    expect(detail).not.toHaveProperty('body_text')
    expect(detail.body_state).toBe('stored')
  })

  it('encodes the opaque id so it cannot escape the diagnostics path', async () => {
    client.get.mockResolvedValue({ data: attempt() })

    await getDiagnostic('diag/../evil?x=1')

    expect(client.get).toHaveBeenCalledWith(
      '/admin/error-diagnostics/diag%2F..%2Fevil%3Fx%3D1',
      expect.anything(),
    )
  })

  it('reveals the body only through an explicit no-store POST', async () => {
    client.post.mockResolvedValue({ data: { body_text: '{"messages":[]}', body_bytes: 15 } })

    const reveal = await revealDiagnosticBody('diag_01HXYZ')

    expect(client.get).not.toHaveBeenCalled()
    const [url, payload, config] = client.post.mock.calls[0]
    expect(url).toBe('/admin/error-diagnostics/diag_01HXYZ/body')
    expect(payload).toBeUndefined()
    expect(config.headers).toMatchObject({ 'Cache-Control': 'no-store' })
    expect(reveal).toEqual({ body_text: '{"messages":[]}', body_bytes: 15 })
  })

  it('drops unexpected fields from a reveal payload', async () => {
    client.post.mockResolvedValue({
      data: { body_text: '{"a":1}', body_bytes: 7, authorization: 'Bearer canary', model: 'model-canary' },
    })

    const reveal = await revealDiagnosticBody('diag_01HXYZ')

    expect(Object.keys(reveal).sort()).toEqual(['body_bytes', 'body_text'])
    expect(JSON.stringify(reveal)).not.toContain('Bearer canary')
  })

  it('rejects a reveal payload that does not match the agreed shape', async () => {
    client.post.mockResolvedValue({ data: { detail: 'raw upstream body' } })
    await expect(revealDiagnosticBody('diag_01HXYZ')).rejects.toThrow()

    client.post.mockResolvedValue({ data: { body_text: 12, body_bytes: 3 } })
    await expect(revealDiagnosticBody('diag_01HXYZ')).rejects.toThrow()
  })
})
