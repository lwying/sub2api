import { beforeEach, describe, expect, it, vi } from 'vitest'
const client = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: client }))
import { getOperatorSettings, updateOperatorSettings } from '../api'

const status = {
  enabled: false,
  capture_allowed: false,
  risk_acknowledgement_current: false,
  risk_version: 'v2026.09.28',
  risk_phrase_en: 'New Trace risk',
  risk_phrase_zh: '新明文风险',
  plaintext_capture_supported: true,
  plaintext_capture_support_reason: 'supported',
}

describe('Trace operator settings API', () => {
  beforeEach(() => Object.values(client).forEach(mock => mock.mockReset()))

  it('reads only the new Trace gate without returning unexpected credentials', async () => {
    client.get.mockResolvedValue({ data: { ...status, body: 'CANARY_BODY', admin_api_key: 'sk-CANARY' } })
    const result = await getOperatorSettings()
    expect(client.get).toHaveBeenCalledWith('/admin/settings/request-trace', expect.objectContaining({ headers: expect.objectContaining({ 'Cache-Control': 'no-store' }) }))
    expect(JSON.stringify(result)).not.toContain('CANARY_BODY')
    expect(JSON.stringify(result)).not.toContain('sk-CANARY')
    expect(result.capture_allowed).toBe(false)
  })

  it('sends only the new gate and typed acknowledgement, never a legacy setting', async () => {
    client.put.mockResolvedValue({ data: status })
    await updateOperatorSettings({ enabled: true, language: 'en', phrase: 'New Trace risk' })
    expect(client.put).toHaveBeenCalledWith('/admin/settings/request-trace', {
      enabled: true,
      language: 'en',
      phrase: 'New Trace risk',
    }, expect.objectContaining({ headers: expect.objectContaining({ Pragma: 'no-cache' }) }))
  })
})
