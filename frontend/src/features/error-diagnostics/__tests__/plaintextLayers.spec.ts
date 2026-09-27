/**
 * Readability rules for the new plaintext layers (tickets 08/09).
 *
 * The two formats do not share a window:
 *
 *   - `encrypted` rows keep their own seven-day window and need the old key;
 *   - `plaintext` rows follow their usage record when linked (no window of their
 *     own) and stop being readable exactly at `metadata_expires_at` when unlinked.
 *
 * These are pure rules, so they are pinned here without a component: the drawer
 * and the "leave no stale plaintext on screen" behaviour both build on them.
 */
import { describe, expect, it } from 'vitest'
import {
  isBodyReadable,
  isHeaderValuesReadable,
  plaintextReadDeadline,
  retentionFormatLabel,
  retentionRuleLabel,
} from '../labels'

const t = (key: string) => key

describe('plaintext retention rules', () => {
  const NOW = Date.parse('2026-09-27T12:00:00Z')

  it('gives a linked plaintext row no deadline of its own', () => {
    expect(plaintextReadDeadline(true, '2020-01-01T00:00:00Z')).toBeUndefined()
    expect(isBodyReadable(
      {
        body_format: 'plaintext',
        body_state: 'stored',
        usage_linked: true,
        metadata_expires_at: '2020-01-01T00:00:00Z',
      },
      NOW,
    )).toBe(true)
  })

  it('stops an unlinked plaintext row exactly at its metadata deadline', () => {
    const detail = {
      body_format: 'plaintext' as const,
      body_state: 'stored' as const,
      usage_linked: false,
      metadata_expires_at: '2026-09-27T12:00:00Z',
    }
    // 到期时刻本身就不算可读：与服务端的「整点拒绝」逐字一致。
    expect(isBodyReadable(detail, NOW)).toBe(false)
    expect(isBodyReadable({ ...detail, metadata_expires_at: '2026-09-27T12:00:01Z' }, NOW)).toBe(true)
  })

  it('never offers a reveal for a plaintext row that is not stored', () => {
    expect(
      isBodyReadable(
        { body_format: 'plaintext', body_state: 'purged', usage_linked: true },
        NOW,
      ),
    ).toBe(false)
    expect(
      isHeaderValuesReadable(
        { header_format: 'plaintext', header_state: 'purged', usage_linked: true },
        NOW,
      ),
    ).toBe(false)
  })

  it('keeps the encrypted layer on its own seven-day window', () => {
    const detail = {
      body_format: 'encrypted' as const,
      body_state: 'stored' as const,
      body_expires_at: '2026-09-27T11:59:59Z',
      metadata_expires_at: '2026-10-27T12:00:00Z',
    }
    expect(isBodyReadable(detail, NOW)).toBe(false)
    expect(isBodyReadable({ ...detail, body_expires_at: '2026-09-27T12:00:01Z' }, NOW)).toBe(true)
  })

  it('names the rule and the format for the admin', () => {
    expect(retentionFormatLabel(t, 'plaintext')).toBe('admin.errorDiagnostics.formats.plaintext')
    // An unknown or missing format is read as the legacy layer: never claim plaintext.
    expect(retentionFormatLabel(t, undefined)).toBe('admin.errorDiagnostics.formats.encrypted')
    expect(retentionRuleLabel(t, 'plaintext', true)).toBe('admin.errorDiagnostics.rules.plaintextLinked')
    expect(retentionRuleLabel(t, 'plaintext', false)).toBe(
      'admin.errorDiagnostics.rules.plaintextUnlinked',
    )
    expect(retentionRuleLabel(t, 'encrypted', false)).toBe('admin.errorDiagnostics.rules.encrypted')
  })

  it('reads a missing format as the legacy layer, window included', () => {
    // 旧后端不带格式字段：按密文读，绝不声称有明文可读。
    expect(
      isBodyReadable({ body_state: 'stored', body_expires_at: '2026-09-27T12:00:01Z' }, NOW),
    ).toBe(true)
    expect(
      isBodyReadable({ body_state: 'stored', body_expires_at: '2026-09-27T11:00:00Z' }, NOW),
    ).toBe(false)
  })
})
