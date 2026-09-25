import { describe, expect, it, vi } from 'vitest'

vi.mock('@/api/client', () => ({ apiClient: { get: vi.fn(), post: vi.fn() } }))

import {
  isRequestAuditValueDetailNotFound,
  normalizeRequestAuditValueDetailReveal,
} from '@/api/admin/usage'

/** Mirrors the backend's own attempt-latency ceiling (24 hours). */
const MAX_ATTEMPT_LATENCY_MS = 24 * 60 * 60 * 1000

/**
 * Reads one attempt back through the real reveal boundary. These are the only
 * tests of the decoder itself: the drawer spec goes through the same normalizer,
 * but the bounds below are the decoder's contract, not the drawer's rendering.
 */
function decodedAttempt(attempt: Record<string, unknown>) {
  return normalizeRequestAuditValueDetailReveal({
    usage_log_id: 51,
    values: { inbound: { request_headers: {} }, attempts: [attempt] },
  }).values.attempts[0]
}

describe('request audit value detail attempt scalars', () => {
  it('keeps a measured attempt latency and a positive proxy id', () => {
    const attempt = decodedAttempt({ index: 1, latency_ms: 1234, proxy_id: 7 })

    expect(attempt.latency_ms).toBe(1234)
    expect(attempt.proxy_id).toBe(7)
  })

  it('keeps a measured zero latency and the latency ceiling as facts', () => {
    expect(decodedAttempt({ index: 1, latency_ms: 0 }).latency_ms).toBe(0)
    expect(decodedAttempt({ index: 2, latency_ms: MAX_ATTEMPT_LATENCY_MS }).latency_ms).toBe(
      MAX_ATTEMPT_LATENCY_MS,
    )
  })

  it.each([
    ['beyond the 24-hour ceiling', MAX_ATTEMPT_LATENCY_MS + 1],
    ['negative', -1],
    ['fractional', 1.5],
    ['inexact', 1e300],
    ['a numeric string', '1234'],
    ['null', null],
    ['a boolean', true],
    ['an object', { measured: 12 }],
  ])('drops a latency that is %s', (_description, latency) => {
    expect(decodedAttempt({ index: 1, latency_ms: latency })).not.toHaveProperty('latency_ms')
  })

  it.each([
    ['zero', 0],
    ['negative', -3],
    ['fractional', 1.5],
    ['beyond the exact-integer range', Number.MAX_SAFE_INTEGER + 2],
    ['a numeric string', '7'],
    ['null', null],
    ['an object', { id: 7 }],
  ])('drops a proxy id that is %s', (_description, proxyId) => {
    expect(decodedAttempt({ index: 1, proxy_id: proxyId })).not.toHaveProperty('proxy_id')
  })

  it('never carries an unknown attempt key into the decoded attempt', () => {
    const attempt = decodedAttempt({
      index: 1,
      latency_ms: 12,
      proxy_id: 3,
      body_text: 'BODY_CANARY',
      proxy_url: 'http://proxy-canary.internal',
    })

    expect(Object.keys(attempt).sort()).toEqual([
      'index',
      'latency_ms',
      'proxy_id',
      'request_headers',
      'response_headers',
    ])
  })
})

/**
 * The decrypted payload crosses a trust boundary, so it is re-validated here — but
 * against the backend's own rules, not stricter ones. The persistence boundary
 * refuses the model name and the parsed `metadata.user_id` components only for the
 * unambiguous credential *prefixes* (see `requestAuditValueDetailCredentialPrefixes`),
 * and it deliberately keeps closed-set header names out of any generic word scan.
 * A decoder that dropped `secret`- or `key`-containing values instead would refuse
 * legitimate values the backend retained, and would then silently show fewer entries
 * than the payload carried.
 */
describe('request audit value detail value boundaries', () => {
  const reveal = (values: Record<string, unknown>) =>
    normalizeRequestAuditValueDetailReveal({ usage_log_id: 51, values })

  it('keeps a model name and parsed identifiers the backend retained around a credential word', () => {
    const decoded = reveal({
      model: 'vendor-secret-key-model',
      inbound: {
        request_headers: {},
        device_id: 'device-secret-9f2c',
        account_uuid: 'account-apikey-7',
        session_id: 'session-password-1',
      },
      attempts: [],
    })

    expect(decoded.values.model).toBe('vendor-secret-key-model')
    expect(decoded.values.inbound.device_id).toBe('device-secret-9f2c')
    expect(decoded.values.inbound.account_uuid).toBe('account-apikey-7')
    expect(decoded.values.inbound.session_id).toBe('session-password-1')
    expect(decoded.values.validation_dropped).toBe(false)
  })

  it('keeps an allowlisted header value that merely contains a credential word', () => {
    const decoded = reveal({
      inbound: {
        request_headers: {
          Host: ['secret.internal'],
          'Anthropic-Beta': ['cookie-feature-20241101'],
        },
      },
      attempts: [],
    })

    expect(decoded.values.inbound.request_headers).toEqual({
      Host: ['secret.internal'],
      'Anthropic-Beta': ['cookie-feature-20241101'],
    })
    expect(decoded.values.validation_dropped).toBe(false)
  })

  it('keeps the default browser-access header value the backend retains', () => {
    const decoded = reveal({
      inbound: {
        request_headers: { 'anthropic-dangerous-direct-browser-access': ['true'] },
      },
      attempts: [],
    })

    expect(decoded.values.inbound.request_headers).toEqual({
      'Anthropic-Dangerous-Direct-Browser-Access': ['true'],
    })
  })

  it('renders the canonical header names the backend persists', () => {
    const decoded = reveal({
      inbound: {
        request_headers: { 'x-stainless-helper-method': ['stream'] },
      },
      attempts: [],
    })

    expect(decoded.values.inbound.request_headers).toEqual({ 'X-Stainless-Helper-Method': ['stream'] })
  })

  it('still refuses every field that starts with an unambiguous credential prefix', () => {
    const decoded = reveal({
      model: 'sk-model-name',
      inbound: {
        request_headers: { 'User-Agent': ['ghp_canary'] },
        device_id: 'sk-canary-device',
        session_id: '-----begin-canary',
      },
      attempts: [
        { index: 1, request_headers: { Accept: ['xoxb-canary'] }, response_headers: {} },
      ],
    })

    expect(decoded.values).not.toHaveProperty('model')
    expect(decoded.values.inbound).not.toHaveProperty('device_id')
    expect(decoded.values.inbound).not.toHaveProperty('session_id')
    expect(decoded.values.inbound.request_headers).toEqual({})
    expect(decoded.values.attempts[0].request_headers).toEqual({})
    expect(decoded.values.validation_dropped).toBe(true)
  })

  it('never counts a credential header as a dropped entry, because it is never a value', () => {
    const decoded = reveal({
      inbound: {
        request_headers: { Authorization: ['Bearer sk-canary'], Cookie: ['session=canary'] },
      },
      attempts: [
        {
          index: 1,
          request_headers: { 'X-Api-Key': ['sk-canary'] },
          response_headers: { 'Set-Cookie': ['session=canary'] },
        },
      ],
    })

    expect(decoded.values.inbound.request_headers).toEqual({})
    expect(decoded.values.attempts[0].request_headers).toEqual({})
    expect(decoded.values.attempts[0].response_headers).toEqual({})
    expect(decoded.values.validation_dropped).toBe(false)
  })

  it('reports an entry it had to drop instead of presenting the view as complete', () => {
    const decoded = reveal({
      inbound: { request_headers: { 'X-Unknown-Header': ['canary'] } },
      attempts: [
        {
          index: 1,
          latency_ms: MAX_ATTEMPT_LATENCY_MS + 1,
          proxy_id: 0,
          request_headers: {},
          response_headers: {},
        },
      ],
    })

    expect(decoded.values.validation_dropped).toBe(true)
    expect(decoded.values.inbound.request_headers).toEqual({})
    expect(decoded.values.attempts[0]).not.toHaveProperty('latency_ms')
    expect(decoded.values.attempts[0]).not.toHaveProperty('proxy_id')
  })

  it('derives the validation flag from what it dropped, never from the payload', () => {
    const decoded = reveal({
      validation_dropped: false,
      inbound: { request_headers: { Host: ['sk-canary'] } },
      attempts: [],
    })

    expect(decoded.values.validation_dropped).toBe(true)
  })

  it('leaves the flag off a payload that carried nothing this view refused', () => {
    const decoded = reveal({
      truncated: true,
      inbound: { request_headers: { Accept: ['application/json'] }, device_id: 'dev-1' },
      attempts: [{ index: 1, request_headers: {}, response_headers: {} }],
    })

    expect(decoded.values.validation_dropped).toBe(false)
    // The server's own statement stays a separate fact.
    expect(decoded.values.truncated).toBe(true)
  })
})

describe('request audit value detail envelope absence', () => {
  it('treats only a 404 as "no row for this usage log"', () => {
    expect(isRequestAuditValueDetailNotFound({ status: 404, message: 'not found' })).toBe(true)
  })

  it.each([
    ['storage failed', 500],
    ['temporarily unavailable', 503],
    ['a conflict', 409],
    ['gone', 410],
    ['a successful status that still failed', 200],
    ['a request that never reached the server', 0],
  ])('does not treat %s (%i) as absence', (_description, status) => {
    expect(isRequestAuditValueDetailNotFound({ status, message: 'failure' })).toBe(false)
  })

  it.each([
    ['a bare error', new Error('Network Error')],
    ['undefined', undefined],
    ['null', null],
    ['a string', '404'],
    ['a numeric status', 404],
    ['a string status', { status: '404' }],
  ])('refuses to read a status out of %s', (_description, error) => {
    expect(isRequestAuditValueDetailNotFound(error)).toBe(false)
  })
})
