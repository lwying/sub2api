/**
 * API and normalization seam for the error-diagnostic operator gate
 * (GET/PUT /api/v1/admin/settings/error-diagnostic).
 *
 * The gate is the only place where production capture and body retention can be
 * turned on, and ADR 0005 requires that to be an explicit, written operator
 * action. Two consequences are pinned here:
 *
 *   1. The response is a closed shape. Every gate boolean must be a real boolean:
 *      a missing or malformed field must NOT be normalized into `false` (which the
 *      UI would render as "off") — it is a payload error, and "cannot be asked"
 *      must never look like "disabled".
 *   2. The response never carries a key, a body, an operator IP or a User-Agent.
 *      An explicit allowlist drops anything a build or a proxy might add, so no
 *      component can leak it by rendering the object.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest'

const client = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  put: vi.fn(),
}))
vi.mock('@/api/client', () => ({ apiClient: client }))

import { getOperatorSettings, updateOperatorSettings } from '../api'
import { normalizeErrorDiagnosticOperatorStatus } from '../types'

const PHRASE_EN = 'Statement EN: bodies 7 days, metadata 30 days, not an erasure tool.'
const PHRASE_ZH = '确认语句：正文 7 天，元数据 30 天，不是擦除手段。'

const status = (overrides: Record<string, unknown> = {}) => ({
  enabled: false,
  risk_acknowledged: false,
  body_retention_enabled: false,
  header_values_enabled: false,
  capture_allowed: false,
  body_retention_allowed: false,
  header_values_allowed: false,
  body_encryption_key_available: true,
  risk_version: 'v2026.09.24.1',
  risk_phrase_en: PHRASE_EN,
  risk_phrase_zh: PHRASE_ZH,
  risk_acknowledgement_current: false,
  ...overrides,
})

describe('error diagnostic operator settings API', () => {
  beforeEach(() => {
    client.get.mockReset()
    client.post.mockReset()
    client.put.mockReset()
  })

  it('reads the gate from the dedicated settings path without caching', async () => {
    client.get.mockResolvedValue({ data: status() })

    const result = await getOperatorSettings()

    expect(client.get).toHaveBeenCalledWith(
      '/admin/settings/error-diagnostic',
      expect.objectContaining({ headers: expect.objectContaining({ 'Cache-Control': 'no-store' }) }),
    )
    expect(client.put).not.toHaveBeenCalled()
    expect(result).toMatchObject({
      enabled: false,
      risk_acknowledged: false,
      body_retention_enabled: false,
      header_values_enabled: false,
      capture_allowed: false,
      body_retention_allowed: false,
      header_values_allowed: false,
      body_encryption_key_available: true,
      risk_version: 'v2026.09.24.1',
      risk_acknowledgement_current: false,
    })
    expect(result.risk_phrase_en).toBe(PHRASE_EN)
    expect(result.risk_phrase_zh).toBe(PHRASE_ZH)
    expect(result.risk_acknowledgement).toBeUndefined()
  })

  it('sends exactly the contract fields, stating both retention layers', async () => {
    client.put.mockResolvedValue({ data: status({ enabled: true, capture_allowed: true }) })

    const updated = await updateOperatorSettings({
      enabled: true,
      body_retention_enabled: false,
      header_values_enabled: false,
      language: 'en',
      phrase: PHRASE_EN,
    })

    expect(client.put).toHaveBeenCalledTimes(1)
    const [url, payload, config] = client.put.mock.calls[0]
    expect(url).toBe('/admin/settings/error-diagnostic')
    // Whole-state update: the server treats an omitted field as off, so the client
    // must always state both retention layers and the acknowledgement it is making.
    expect(Object.keys(payload).sort()).toEqual([
      'body_retention_enabled',
      'enabled',
      'header_values_enabled',
      'language',
      'phrase',
    ])
    expect(payload).toEqual({
      enabled: true,
      body_retention_enabled: false,
      header_values_enabled: false,
      language: 'en',
      phrase: PHRASE_EN,
    })
    expect(config.headers).toMatchObject({ 'Cache-Control': 'no-store' })
    expect(updated).toMatchObject({ enabled: true, capture_allowed: true })
  })

  it('states the header value layer on its own, independently of body retention', async () => {
    client.put.mockResolvedValue({
      data: status({ enabled: true, capture_allowed: true, header_values_enabled: true, header_values_allowed: true }),
    })

    // Body retention stays off while the 429 header value layer is turned on: the
    // two switches are orthogonal, and neither may be derived from the other.
    const updated = await updateOperatorSettings({
      enabled: true,
      body_retention_enabled: false,
      header_values_enabled: true,
      language: 'en',
      phrase: PHRASE_EN,
    })

    const [, payload] = client.put.mock.calls[0]
    expect(payload.body_retention_enabled).toBe(false)
    expect(payload.header_values_enabled).toBe(true)
    expect(updated).toMatchObject({
      body_retention_allowed: false,
      header_values_enabled: true,
      header_values_allowed: true,
    })
  })

  it('never lets a key, a body or an operator identity reach a consumer', async () => {
    const response = status({
      enabled: true,
      capture_allowed: true,
      body_retention_enabled: true,
      body_retention_allowed: true,
      risk_acknowledgement: {
        version: 'v2026.09.24',
        phrase: PHRASE_EN,
        admin_user_id: 7,
        accepted_at: '2026-09-24T10:00:00Z',
        ip_address: '203.0.113.9',
        user_agent: 'CANARY_UA',
      },
      body_text: 'CANARY_BODY',
      body_ciphertext: 'CANARY_CIPHERTEXT',
      encryption_key: 'CANARY_KEY',
      admin_api_key: 'sk-CANARY',
      ip_address: '203.0.113.9',
      user_agent: 'CANARY_UA',
    })
    client.get.mockResolvedValue({ data: response })

    const serialized = JSON.stringify(await getOperatorSettings())

    expect(serialized).not.toContain('CANARY_BODY')
    expect(serialized).not.toContain('CANARY_CIPHERTEXT')
    expect(serialized).not.toContain('CANARY_KEY')
    expect(serialized).not.toContain('sk-CANARY')
    expect(serialized).not.toContain('203.0.113.9')
    expect(serialized).not.toContain('CANARY_UA')
    expect(serialized).not.toContain('ip_address')
    expect(serialized).not.toContain('user_agent')
  })

  it('keeps only the four acknowledgement fields that may be shown', async () => {
    const result = normalizeErrorDiagnosticOperatorStatus(
      status({
        risk_acknowledgement_current: true,
        risk_acknowledgement: {
          version: 'v2026.09.24',
          phrase: PHRASE_EN,
          admin_user_id: 7,
          accepted_at: '2026-09-24T10:00:00Z',
          ip_address: '203.0.113.9',
          user_agent: 'CANARY_UA',
        },
      }),
    )

    expect(result.risk_acknowledgement).toEqual({
      version: 'v2026.09.24',
      phrase: PHRASE_EN,
      admin_user_id: 7,
      accepted_at: '2026-09-24T10:00:00Z',
    })
    expect(result.risk_acknowledgement_current).toBe(true)
  })

  it('carries the stored flags and the verified conclusions as the server sent them', async () => {
    // Turned on out of band without a current acknowledgement: the server reports a
    // stored-on / not-capturing state on purpose, so both sides must survive here
    // instead of being reconciled into one local guess.
    client.get.mockResolvedValue({
      data: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: false,
        risk_acknowledgement_current: false,
      }),
    })

    const result = await getOperatorSettings()

    expect(result.enabled).toBe(true)
    expect(result.risk_acknowledged).toBe(true)
    expect(result.capture_allowed).toBe(false)
    expect(result.risk_acknowledgement_current).toBe(false)
  })

  it('keeps the two retention layers apart, stored flags and conclusions alike', async () => {
    // Stored on but not effective (no usable key): the stored intent and the verified
    // conclusion are different facts for each layer, and a layer's conclusion must
    // not be read off the other layer's flags.
    client.get.mockResolvedValue({
      data: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: true,
        risk_acknowledgement_current: true,
        body_retention_enabled: true,
        body_retention_allowed: false,
        header_values_enabled: true,
        header_values_allowed: false,
        body_encryption_key_available: false,
      }),
    })

    const result = await getOperatorSettings()

    expect(result.body_retention_enabled).toBe(true)
    expect(result.body_retention_allowed).toBe(false)
    expect(result.header_values_enabled).toBe(true)
    expect(result.header_values_allowed).toBe(false)
    expect(result.body_encryption_key_available).toBe(false)
  })

  it('treats a malformed acknowledgement as no acknowledgement at all', async () => {
    const withoutAcceptedAt = normalizeErrorDiagnosticOperatorStatus(
      status({ risk_acknowledgement: { version: 'v1', phrase: PHRASE_EN, admin_user_id: 7 } }),
    )
    expect(withoutAcceptedAt.risk_acknowledgement).toBeUndefined()

    const withoutOperator = normalizeErrorDiagnosticOperatorStatus(
      status({
        risk_acknowledgement: {
          version: 'v1',
          phrase: PHRASE_EN,
          admin_user_id: 0,
          accepted_at: '2026-09-24T10:00:00Z',
        },
      }),
    )
    expect(withoutOperator.risk_acknowledgement).toBeUndefined()
  })

  it('rejects a payload whose gate flags are not real booleans', () => {
    // A missing flag must not be read as "off": that is exactly the confusion
    // between "the server cannot be asked" and "capture is disabled".
    for (const key of [
      'enabled',
      'risk_acknowledged',
      'body_retention_enabled',
      'header_values_enabled',
      'capture_allowed',
      'body_retention_allowed',
      'header_values_allowed',
      'body_encryption_key_available',
    ]) {
      expect(() => normalizeErrorDiagnosticOperatorStatus(status({ [key]: undefined })), key).toThrow()
      expect(() => normalizeErrorDiagnosticOperatorStatus(status({ [key]: 'false' })), key).toThrow()
    }
  })

  it('rejects a payload without the current acknowledgement statement', () => {
    expect(() => normalizeErrorDiagnosticOperatorStatus(status({ risk_version: '' }))).toThrow()
    expect(() => normalizeErrorDiagnosticOperatorStatus(status({ risk_phrase_en: '' }))).toThrow()
    expect(() => normalizeErrorDiagnosticOperatorStatus(status({ risk_phrase_zh: undefined }))).toThrow()
    expect(() => normalizeErrorDiagnosticOperatorStatus(null)).toThrow()
  })

  it('never invents an acknowledgement currency the server did not report', () => {
    const result = normalizeErrorDiagnosticOperatorStatus(status({ risk_acknowledgement_current: undefined }))
    expect(result.risk_acknowledgement_current).toBe(false)
  })
})
