/**
 * Component seam for the admin diagnostics detail drawer.
 *
 * Pins the interaction contract: metadata may load when the admin opens a row,
 * but the stored request body is only fetched by an explicit click, is rendered
 * as text (never HTML), and is dropped from memory when the drawer closes.
 *
 * Body availability is never inferred from the reveal error: on refusal the
 * drawer re-reads the metadata and renders the authoritative `body_state`.
 */
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { DiagnosticAttempt } from '../types'

const mocks = vi.hoisted(() => ({
  getDiagnostic: vi.fn(),
  revealDiagnosticBody: vi.fn(),
}))

vi.mock('../api', () => ({
  getDiagnostic: mocks.getDiagnostic,
  revealDiagnosticBody: mocks.revealDiagnosticBody,
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const labels: Record<string, string> = {
    'admin.errorDiagnostics.detail.title': 'Error diagnostic',
    'admin.errorDiagnostics.detail.loadFailed': 'Could not load the diagnostic metadata',
    'admin.errorDiagnostics.detail.metadata': 'Metadata',
    'admin.errorDiagnostics.detail.body': 'Stored request body',
    'admin.errorDiagnostics.detail.reveal': 'Reveal request body',
    'admin.errorDiagnostics.detail.revealing': 'Revealing…',
    'admin.errorDiagnostics.detail.revealFailed': 'The request body could not be revealed',
    'admin.errorDiagnostics.detail.attempt': 'Attempt #{index}',
    'admin.errorDiagnostics.detail.upstreamStatus': 'Upstream status',
    'admin.errorDiagnostics.detail.protocol': 'Protocol',
    'admin.errorDiagnostics.detail.createdAt': 'Created',
    'admin.errorDiagnostics.detail.bodyExpiresAt': 'Body expires',
    'admin.errorDiagnostics.detail.metadataExpiresAt': 'Metadata expires',
    'admin.errorDiagnostics.detail.bodyBytes': '{bytes} bytes',
    'admin.errorDiagnostics.detail.usageLink': 'Related usage record',
    'admin.errorDiagnostics.detail.usageAbsent': 'No usage record',
    'admin.errorDiagnostics.detail.notice': 'The body is decrypted only on request and is never cached.',
    'admin.errorDiagnostics.protocols.messages': 'Messages',
    'admin.errorDiagnostics.protocols.chat_completions': 'Chat Completions',
    'admin.errorDiagnostics.protocols.responses': 'Responses',
    'admin.errorDiagnostics.protocols.unknown': 'Unknown protocol',
    'admin.errorDiagnostics.bodyStates.notObserved': 'No request body was observed',
    'admin.errorDiagnostics.bodyStates.stored': 'Body stored',
    'admin.errorDiagnostics.bodyStates.skipped': 'Body not retained',
    'admin.errorDiagnostics.bodyStates.expired': 'Body expired',
    'admin.errorDiagnostics.bodyStates.purged': 'Body cleared',
    'admin.errorDiagnostics.bodyStates.unknown': 'Unknown state',
    'admin.errorDiagnostics.reasons.not_observed': 'No request body was observed for this attempt',
    'admin.errorDiagnostics.reasons.retained': 'Request body retained',
    'admin.errorDiagnostics.reasons.skipped_not_text_json': 'Request was not a text JSON body',
    'admin.errorDiagnostics.reasons.skipped_too_large': 'Request exceeded the retention size limit',
    'admin.errorDiagnostics.reasons.skipped_attachment': 'Request contained an attachment',
    'admin.errorDiagnostics.reasons.skipped_known_credential': 'Request contained a known credential',
    'admin.errorDiagnostics.reasons.skipped_incomplete_read': 'The outbound body was not fully read',
    'admin.errorDiagnostics.reasons.skipped_encryption_unavailable': 'Encryption was unavailable',
    'admin.errorDiagnostics.reasons.skipped_body_retention_disabled': 'Request body retention is disabled',
    'admin.errorDiagnostics.reasons.unknown': 'Unknown retention outcome',
  }
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        (labels[key] ?? key).replace(/\{(\w+)\}/g, (_, token) => String(params?.[token] ?? '')),
    }),
  }
})

import ErrorDiagnosticDetailDrawer from '../components/ErrorDiagnosticDetailDrawer.vue'

const attempt = (overrides: Partial<DiagnosticAttempt> = {}): DiagnosticAttempt => ({
  id: 'diag_1',
  created_at: '2026-09-24T00:00:00Z',
  protocol: 'messages',
  attempt_index: 2,
  upstream_status: 400,
  usage_log_id: 77,
  body_state: 'stored',
  reason: 'retained',
  body_expires_at: '2999-01-01T00:00:00Z',
  metadata_expires_at: '2999-02-01T00:00:00Z',
  ...overrides,
})

function mountDrawer(props: Record<string, unknown> = {}) {
  return mount(ErrorDiagnosticDetailDrawer, {
    props: { show: true, diagnosticId: 'diag_1', ...props },
    global: {
      stubs: {
        BaseDialog: { template: '<div><slot /></div>' },
        Icon: true,
      },
    },
  })
}

describe('ErrorDiagnosticDetailDrawer', () => {
  beforeEach(() => {
    mocks.getDiagnostic.mockReset()
    mocks.revealDiagnosticBody.mockReset()
    mocks.getDiagnostic.mockResolvedValue(attempt())
  })

  it('loads metadata on open but never fetches the body by itself', async () => {
    const wrapper = mountDrawer()
    await flushPromises()

    expect(mocks.getDiagnostic).toHaveBeenCalledWith('diag_1')
    expect(mocks.revealDiagnosticBody).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('Messages')
    expect(wrapper.text()).toContain('400')
    expect(wrapper.text()).not.toContain('{"messages":[]}')
  })

  it('reveals the body only after an explicit click and renders it as text', async () => {
    mocks.revealDiagnosticBody.mockResolvedValue({
      body_text: '<img src=x onerror="alert(1)">{"messages":[]}',
      body_bytes: 40,
    })

    const wrapper = mountDrawer()
    await flushPromises()

    const revealButton = wrapper.find('[data-testid="error-diagnostic-reveal"]')
    expect(revealButton.exists()).toBe(true)
    expect(mocks.revealDiagnosticBody).not.toHaveBeenCalled()

    await revealButton.trigger('click')
    await flushPromises()

    expect(mocks.revealDiagnosticBody).toHaveBeenCalledTimes(1)
    expect(mocks.revealDiagnosticBody).toHaveBeenCalledWith('diag_1')
    expect(wrapper.find('[data-testid="error-diagnostic-body"]').text()).toContain('{"messages":[]}')
    // The stored payload is untrusted text: it must never become DOM.
    expect(wrapper.find('img').exists()).toBe(false)
  })

  it('offers no reveal action for a skipped body and shows the authoritative reason', async () => {
    mocks.getDiagnostic.mockResolvedValue(
      attempt({ body_state: 'skipped', reason: 'skipped_too_large', body_expires_at: undefined }),
    )

    const wrapper = mountDrawer()
    await flushPromises()

    expect(wrapper.find('[data-testid="error-diagnostic-reveal"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('Request exceeded the retention size limit')
    expect(mocks.revealDiagnosticBody).not.toHaveBeenCalled()
  })

  it('does not distinguish a missing key from a failed encryption', async () => {
    mocks.getDiagnostic.mockResolvedValue(
      attempt({ body_state: 'skipped', reason: 'skipped_encryption_unavailable', body_expires_at: undefined }),
    )

    const wrapper = mountDrawer()
    await flushPromises()

    expect(wrapper.text()).toContain('Encryption was unavailable')
    expect(wrapper.find('[data-testid="error-diagnostic-reveal"]').exists()).toBe(false)
  })

  it('treats expired and purged bodies as unreadable and never offers a reveal', async () => {
    mocks.getDiagnostic.mockResolvedValue(attempt({ body_state: 'expired' }))
    const expired = mountDrawer()
    await flushPromises()
    expect(expired.find('[data-testid="error-diagnostic-reveal"]').exists()).toBe(false)
    expect(expired.text()).toContain('Body expired')

    mocks.getDiagnostic.mockResolvedValue(attempt({ body_state: 'purged' }))
    const purged = mountDrawer()
    await flushPromises()
    expect(purged.find('[data-testid="error-diagnostic-reveal"]').exists()).toBe(false)
    expect(purged.text()).toContain('Body cleared')
  })

  it('treats a stored body past its expiry as expired even if the server still calls it stored', async () => {
    mocks.getDiagnostic.mockResolvedValue(
      attempt({ body_state: 'stored', body_expires_at: '2020-01-01T00:00:00Z' }),
    )

    const wrapper = mountDrawer()
    await flushPromises()

    expect(wrapper.find('[data-testid="error-diagnostic-reveal"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('Body expired')
    expect(mocks.revealDiagnosticBody).not.toHaveBeenCalled()
  })

  it('shows a not-observed body without any reveal action', async () => {
    mocks.getDiagnostic.mockResolvedValue(
      attempt({ body_state: 'not_observed', reason: 'not_observed', body_expires_at: undefined }),
    )

    const wrapper = mountDrawer()
    await flushPromises()

    expect(wrapper.text()).toContain('No request body was observed')
    expect(wrapper.find('[data-testid="error-diagnostic-reveal"]').exists()).toBe(false)
  })

  it('re-reads the metadata after a refused reveal instead of guessing from the error code', async () => {
    mocks.getDiagnostic
      .mockResolvedValueOnce(attempt({ body_state: 'stored' }))
      .mockResolvedValueOnce(attempt({ body_state: 'expired' }))
    mocks.revealDiagnosticBody.mockRejectedValue({ status: 410, message: 'body gone' })

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.find('[data-testid="error-diagnostic-reveal"]').trigger('click')
    await flushPromises()

    expect(mocks.getDiagnostic).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('Body expired')
    expect(wrapper.find('[data-testid="error-diagnostic-body"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="error-diagnostic-reveal"]').exists()).toBe(false)
  })

  it('never echoes a server error message that could carry body or credentials', async () => {
    mocks.revealDiagnosticBody.mockRejectedValue({
      status: 500,
      message: 'upstream said: {"authorization":"Bearer CANARY_SECRET"}',
    })

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.find('[data-testid="error-diagnostic-reveal"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('The request body could not be revealed')
    expect(wrapper.text()).not.toContain('CANARY_SECRET')
    expect(wrapper.text()).not.toContain('Bearer')
  })

  it('drops the revealed body from the DOM once the drawer closes', async () => {
    mocks.revealDiagnosticBody.mockResolvedValue({ body_text: '{"messages":[1]}', body_bytes: 16 })

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.find('[data-testid="error-diagnostic-reveal"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('{"messages":[1]}')

    await wrapper.setProps({ show: false })
    await flushPromises()

    expect(wrapper.text()).not.toContain('{"messages":[1]}')
    expect(wrapper.find('[data-testid="error-diagnostic-body"]').exists()).toBe(false)
  })

  it('drops a reveal that resolves after the drawer moved on to another attempt', async () => {
    let resolveReveal: (value: { body_text: string; body_bytes: number }) => void = () => {}
    mocks.revealDiagnosticBody.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveReveal = resolve
        }),
    )

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.find('[data-testid="error-diagnostic-reveal"]').trigger('click')

    // The POST is still in flight when the admin closes the drawer and opens a
    // different attempt.
    await wrapper.setProps({ show: false })
    mocks.getDiagnostic.mockResolvedValue(attempt({ id: 'diag_2' }))
    await wrapper.setProps({ show: true, diagnosticId: 'diag_2' })
    await flushPromises()

    resolveReveal({ body_text: 'STALE_PLAINTEXT_CANARY', body_bytes: 21 })
    await flushPromises()

    // Metadata for the newly opened attempt is shown, but the plaintext of the
    // abandoned one must never be rendered under it.
    expect(mocks.getDiagnostic).toHaveBeenLastCalledWith('diag_2')
    expect(wrapper.text()).toContain('Upstream status')
    expect(wrapper.text()).not.toContain('STALE_PLAINTEXT_CANARY')
    expect(wrapper.find('[data-testid="error-diagnostic-body"]').exists()).toBe(false)
  })

  it('drops a reveal that resolves after the same attempt was closed and reopened', async () => {
    let resolveReveal: (value: { body_text: string; body_bytes: number }) => void = () => {}
    mocks.revealDiagnosticBody.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveReveal = resolve
        }),
    )

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.find('[data-testid="error-diagnostic-reveal"]').trigger('click')

    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()

    resolveReveal({ body_text: 'REOPENED_PLAINTEXT_CANARY', body_bytes: 25 })
    await flushPromises()

    // Closing the drawer dropped the plaintext from memory; a response that
    // arrives afterwards must not resurrect it.
    expect(wrapper.text()).not.toContain('REOPENED_PLAINTEXT_CANARY')
    expect(wrapper.find('[data-testid="error-diagnostic-body"]').exists()).toBe(false)
    // The abandoned reveal must not leave the action stuck in its pending state.
    expect(wrapper.find('[data-testid="error-diagnostic-reveal"]').attributes('disabled')).toBeUndefined()
  })

  it('keeps the plaintext of a newer reveal when an older response lands late', async () => {
    const resolveReveals: Array<(value: { body_text: string; body_bytes: number }) => void> = []
    mocks.revealDiagnosticBody.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveReveals.push(resolve)
        }),
    )

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.find('[data-testid="error-diagnostic-reveal"]').trigger('click')

    await wrapper.setProps({ show: false })
    mocks.getDiagnostic.mockResolvedValue(attempt({ id: 'diag_2' }))
    await wrapper.setProps({ show: true, diagnosticId: 'diag_2' })
    await flushPromises()
    await wrapper.find('[data-testid="error-diagnostic-reveal"]').trigger('click')
    expect(resolveReveals).toHaveLength(2)

    // The attempt the admin is actually looking at answers first…
    resolveReveals[1]({ body_text: 'CURRENT_BODY', body_bytes: 12 })
    await flushPromises()
    expect(wrapper.text()).toContain('CURRENT_BODY')

    // …and the abandoned one must not wipe it out when it arrives afterwards.
    resolveReveals[0]({ body_text: 'STALE_PLAINTEXT_CANARY', body_bytes: 21 })
    await flushPromises()

    expect(wrapper.text()).toContain('CURRENT_BODY')
    expect(wrapper.text()).not.toContain('STALE_PLAINTEXT_CANARY')
  })

  it('never persists a revealed body into browser storage', async () => {
    mocks.revealDiagnosticBody.mockResolvedValue({ body_text: 'STORAGE_CANARY_BODY', body_bytes: 18 })

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.find('[data-testid="error-diagnostic-reveal"]').trigger('click')
    await flushPromises()

    const stored = [
      ...Object.keys(localStorage).map((key) => `${key}=${localStorage.getItem(key)}`),
      ...Object.keys(sessionStorage).map((key) => `${key}=${sessionStorage.getItem(key)}`),
    ].join('|')
    expect(stored).not.toContain('STORAGE_CANARY_BODY')
  })

  it('renders the usage id as plain text, and nothing when there is no usage record', async () => {
    const withUsage = mountDrawer()
    await flushPromises()
    expect(withUsage.find('[data-testid="error-diagnostic-usage-id"]').text()).toContain('#77')
    expect(withUsage.findAll('a')).toHaveLength(0)

    mocks.getDiagnostic.mockResolvedValue(attempt({ usage_log_id: undefined }))
    const withoutUsage = mountDrawer()
    await flushPromises()

    expect(withoutUsage.find('[data-testid="error-diagnostic-usage-absent"]').exists()).toBe(true)
    expect(withoutUsage.find('[data-testid="error-diagnostic-usage-id"]').exists()).toBe(false)
  })

  it('surfaces a metadata load failure without exposing raw error text', async () => {
    mocks.getDiagnostic.mockRejectedValue({ status: 500, message: 'Bearer CANARY_SECRET' })

    const wrapper = mountDrawer()
    await flushPromises()

    expect(wrapper.text()).toContain('Could not load the diagnostic metadata')
    expect(wrapper.text()).not.toContain('CANARY_SECRET')
    expect(wrapper.find('[data-testid="error-diagnostic-reveal"]').exists()).toBe(false)
  })
})
