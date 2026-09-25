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

import { getDiagnostic, listDiagnostics, revealDiagnosticBody, revealDiagnosticHeaders } from '../api'

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

  // -------------------------------------------------------------------------
  // 429 header values (Claude Messages only)
  // -------------------------------------------------------------------------

  it('carries the header retention facts on a row, independently of the body', async () => {
    client.get.mockResolvedValue({
      data: attempt({
        // The headers were retained even though no body ever was: two facts.
        body_state: 'not_observed',
        reason: 'not_observed',
        body_expires_at: undefined,
        header_state: 'stored',
        header_reason: 'retained',
        header_entry_count: 4,
        header_expires_at: '2026-10-01T00:00:00Z',
      }),
    })

    const detail = await getDiagnostic('diag_01HXYZ')

    expect(detail.body_state).toBe('not_observed')
    expect(detail.header_state).toBe('stored')
    expect(detail.header_reason).toBe('retained')
    expect(detail.header_entry_count).toBe(4)
    expect(detail.header_expires_at).toBe('2026-10-01T00:00:00Z')
  })

  it('reads a row written before header values existed as not observed', async () => {
    client.get.mockResolvedValue({ data: attempt() })

    const detail = await getDiagnostic('diag_01HXYZ')

    expect(detail.header_state).toBeUndefined()
    expect(detail.header_entry_count).toBeUndefined()
  })

  it('rejects an unknown header state and drops an unknown header reason', async () => {
    client.get.mockResolvedValue({ data: attempt({ header_state: 'leaked' }) })
    await expect(getDiagnostic('diag_01HXYZ')).rejects.toThrow()

    client.get.mockResolvedValue({
      data: attempt({ header_state: 'skipped', header_reason: '<script>alert(1)</script>' }),
    })
    const detail = await getDiagnostic('diag_01HXYZ')
    expect(detail.header_state).toBe('skipped')
    expect(detail.header_reason).toBeUndefined()
  })

  it('reveals the 429 header values only through an explicit no-store POST', async () => {
    client.post.mockResolvedValue({
      data: {
        request_headers: { Host: 'api.anthropic.com', 'User-Agent': 'claude-cli/2.0.0 (external, cli)' },
        response_headers: { 'Retry-After': '30' },
        header_entry_count: 3,
        header_bytes: 128,
        header_expires_at: '2026-10-01T00:00:00Z',
      },
    })

    const reveal = await revealDiagnosticHeaders('diag_01HXYZ')

    expect(client.get).not.toHaveBeenCalled()
    const [url, payload, config] = client.post.mock.calls[0]
    expect(url).toBe('/admin/error-diagnostics/diag_01HXYZ/headers')
    expect(payload).toBeUndefined()
    expect(config.headers).toMatchObject({ 'Cache-Control': 'no-store' })
    expect(reveal).toEqual({
      request_headers: { Host: 'api.anthropic.com', 'User-Agent': 'claude-cli/2.0.0 (external, cli)' },
      response_headers: { 'Retry-After': '30' },
      header_entry_count: 3,
      header_expires_at: '2026-10-01T00:00:00Z',
    })
  })

  it('encodes the opaque id on the header reveal path too', async () => {
    client.post.mockResolvedValue({
      data: { request_headers: {}, response_headers: {}, header_entry_count: 0 },
    })

    await revealDiagnosticHeaders('diag/../evil?x=1')

    expect(client.post.mock.calls[0][0]).toBe(
      '/admin/error-diagnostics/diag%2F..%2Fevil%3Fx%3D1/headers',
    )
  })

  it('never lets a credential, cookie or unknown header reach a header consumer', async () => {
    client.post.mockResolvedValue({
      data: {
        request_headers: {
          Authorization: 'Bearer sk-canary',
          Cookie: 'session=canary',
          'X-Api-Key': 'sk-canary',
          'X-Unknown-Header': 'canary',
          // An allowlisted name carrying a credential-shaped value.
          Host: 'sk-canary',
          'Accept-Language': 'en-US, zh-CN',
        },
        response_headers: {
          'Set-Cookie': 'session=canary',
          'WWW-Authenticate': 'Basic canary',
          'Retry-After': '30',
        },
        header_entry_count: 9,
        header_expires_at: '2026-10-01T00:00:00Z',
      },
    })

    const reveal = await revealDiagnosticHeaders('diag_01HXYZ')
    const serialized = JSON.stringify(reveal)

    expect(serialized).not.toContain('sk-canary')
    expect(serialized).not.toContain('canary')
    expect(serialized).not.toContain('Bearer')
    expect(serialized).not.toContain('Basic')
    expect(serialized).not.toContain('session=')
    expect(serialized).not.toContain('Authorization')
    expect(serialized).not.toContain('Cookie')
    expect(serialized).not.toContain('X-Api-Key')
    expect(serialized).not.toContain('X-Unknown-Header')
    expect(serialized).not.toContain('WWW-Authenticate')
    expect(reveal.request_headers).toEqual({ 'Accept-Language': 'en-US, zh-CN' })
    expect(reveal.response_headers).toEqual({ 'Retry-After': '30' })
    // The server's count is reported as-is: this view holds fewer entries.
    expect(reveal.header_entry_count).toBe(9)
  })

  /**
   * The closed set here mirrors the persisted one name for name, and the default
   * Claude Code OAuth client sends `anthropic-dangerous-direct-browser-access:
   * true` on every `/v1/messages` request. A view that dropped that one would list
   * fewer entries than the `header_entry_count` it reports (the count is the
   * server's, and it includes what the client actually sent), so a name the
   * backend retains must survive this boundary too.
   */
  it('keeps every request header name the backend retains, including the browser-access flag', async () => {
    client.post.mockResolvedValue({
      data: {
        request_headers: {
          Host: 'api.anthropic.com',
          'User-Agent': 'claude-cli/2.0.0 (external, cli)',
          'Anthropic-Version': '2023-06-01',
          'Anthropic-Beta': 'oauth-2025-04-20',
          // The default OAuth client flag: retained server-side, so it must render here.
          'Anthropic-Dangerous-Direct-Browser-Access': 'true',
          Accept: 'application/json',
          'Accept-Encoding': 'gzip, deflate',
          'Accept-Language': 'en-US',
          'Content-Type': 'application/json',
          'X-App': 'cli',
          'X-Request-Id': 'req_01HXYZ',
          'X-Client-Request-Id': 'req_01HXYZ',
          'X-Claude-Code-Session-Id': '11111111-2222-3333-4444-555555555555',
          'X-Stainless-Retry-Count': '0',
          'X-Stainless-Timeout': '600',
          'X-Stainless-Lang': 'js',
          'X-Stainless-Package-Version': '0.60.0',
          'X-Stainless-OS': 'Linux',
          'X-Stainless-Arch': 'arm64',
          'X-Stainless-Runtime': 'node',
          'X-Stainless-Runtime-Version': '20.0.0',
          'X-Stainless-Helper-Method': 'stream',
        },
        response_headers: {
          'Content-Type': 'application/json',
          'Cache-Control': 'no-store',
          'Retry-After': '30',
          'Request-Id': 'req_01HXYZ',
          'Anthropic-Ratelimit-Requests-Limit': '1000',
          'Anthropic-Ratelimit-Requests-Remaining': '0',
          'Anthropic-Ratelimit-Requests-Reset': '2026-09-24T00:05:00Z',
          'Anthropic-Ratelimit-Input-Tokens-Limit': '2000000',
          'Anthropic-Ratelimit-Input-Tokens-Remaining': '0',
          'Anthropic-Ratelimit-Input-Tokens-Reset': '2026-09-24T00:05:00Z',
          'Anthropic-Ratelimit-Output-Tokens-Limit': '400000',
          'Anthropic-Ratelimit-Output-Tokens-Remaining': '0',
          'Anthropic-Ratelimit-Output-Tokens-Reset': '2026-09-24T00:05:00Z',
        } as Record<string, string>,
        header_entry_count: 35,
      },
    })

    const reveal = await revealDiagnosticHeaders('diag_01HXYZ')

    expect(reveal.request_headers['Anthropic-Dangerous-Direct-Browser-Access']).toBe('true')
    // Nothing the backend retained may be silently missing: the reported count is
    // the server's, so a dropped name would make the list disagree with it.
    expect(Object.keys(reveal.request_headers).sort()).toEqual(
      [
        'Accept',
        'Accept-Encoding',
        'Accept-Language',
        'Anthropic-Beta',
        'Anthropic-Dangerous-Direct-Browser-Access',
        'Anthropic-Version',
        'Content-Type',
        'Host',
        'User-Agent',
        'X-App',
        'X-Claude-Code-Session-Id',
        'X-Client-Request-Id',
        'X-Request-Id',
        'X-Stainless-Arch',
        'X-Stainless-Helper-Method',
        'X-Stainless-Lang',
        'X-Stainless-OS',
        'X-Stainless-Package-Version',
        'X-Stainless-Retry-Count',
        'X-Stainless-Runtime',
        'X-Stainless-Runtime-Version',
        'X-Stainless-Timeout',
      ].sort(),
    )
    expect(reveal.header_entry_count).toBe(35)
  })

  it('drops a malformed header value, and reports the server count when there is one', async () => {
    // A non-string value, an empty value and a non-numeric count are all dropped
    // or ignored; the count of what is actually shown is reported instead.
    client.post.mockResolvedValue({
      data: {
        request_headers: { Host: 'api.anthropic.com', 'User-Agent': ['claude-cli/2.0.0'] },
        response_headers: { 'Retry-After': 30, 'Request-Id': '' },
        header_entry_count: 'not-a-count',
      },
    })

    const reveal = await revealDiagnosticHeaders('diag_01HXYZ')

    expect(reveal.request_headers).toEqual({ Host: 'api.anthropic.com' })
    expect(reveal.response_headers).toEqual({})
    expect(reveal.header_entry_count).toBe(1)

    // A trustworthy server count is the total that was retained, so it is kept
    // even when this view holds fewer entries after filtering.
    client.post.mockResolvedValue({
      data: {
        request_headers: { Host: 'api.anthropic.com' },
        response_headers: {},
        header_entry_count: 4,
      },
    })
    const withServerCount = await revealDiagnosticHeaders('diag_01HXYZ')
    expect(withServerCount.header_entry_count).toBe(4)
  })

  it('rejects a reveal payload that is not a header map', async () => {
    client.post.mockResolvedValue({ data: { request_headers: 'raw', response_headers: {} } })
    await expect(revealDiagnosticHeaders('diag_01HXYZ')).rejects.toThrow()

    client.post.mockResolvedValue({ data: 'raw upstream headers' })
    await expect(revealDiagnosticHeaders('diag_01HXYZ')).rejects.toThrow()
  })
})
