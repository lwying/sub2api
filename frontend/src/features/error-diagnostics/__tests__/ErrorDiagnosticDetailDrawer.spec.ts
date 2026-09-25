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
  revealDiagnosticHeaders: vi.fn(),
}))

/**
 * The header reveal returns a RAW payload that the real normalizer then filters,
 * so the allowlist stays under test: a canary the real boundary drops proves the
 * guard, not the stub.
 */
vi.mock('../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api')>()
  const { normalizeDiagnosticHeaderReveal } = await import('../types')
  return {
    ...actual,
    getDiagnostic: mocks.getDiagnostic,
    revealDiagnosticBody: mocks.revealDiagnosticBody,
    revealDiagnosticHeaders: async (id: string) =>
      normalizeDiagnosticHeaderReveal(await mocks.revealDiagnosticHeaders(id)),
  }
})

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
    'admin.errorDiagnostics.headerStates.notObserved': 'No 429 header values were observed',
    'admin.errorDiagnostics.headerStates.stored': 'Header values stored',
    'admin.errorDiagnostics.headerStates.skipped': 'Header values not retained',
    'admin.errorDiagnostics.headerStates.expired': 'Header values expired',
    'admin.errorDiagnostics.headerStates.purged': 'Header values cleared',
    'admin.errorDiagnostics.headerStates.unknown': 'Unknown state',
    'admin.errorDiagnostics.headerReasons.not_observed': 'No 429 header values were observed for this attempt',
    'admin.errorDiagnostics.headerReasons.retained': '429 header values retained',
    'admin.errorDiagnostics.headerReasons.skipped_out_of_scope':
      'This attempt was not an upstream 429 on the Messages path',
    'admin.errorDiagnostics.headerReasons.skipped_header_retention_disabled':
      '429 header value retention is disabled',
    'admin.errorDiagnostics.headerReasons.skipped_encryption_unavailable': 'Encryption was unavailable',
    'admin.errorDiagnostics.headerReasons.skipped_invalid_values':
      'The observed header values did not pass validation',
    'admin.errorDiagnostics.headerReasons.unknown': 'Unknown retention outcome',
    'admin.errorDiagnostics.detail.headers': 'Upstream 429 header values',
    'admin.errorDiagnostics.detail.headerExpiresAt': 'Header values expire',
    'admin.errorDiagnostics.detail.headerNotice':
      'Header values are decrypted only on request, are never cached, and never include credentials, cookies or the request body. They are revealed separately from the body.',
    'admin.errorDiagnostics.detail.revealHeaders': 'Reveal 429 header values',
    'admin.errorDiagnostics.detail.revealingHeaders': 'Revealing…',
    'admin.errorDiagnostics.detail.headerRevealFailed': 'The 429 header values could not be revealed',
    'admin.errorDiagnostics.detail.headerEntryCount': '{count} header values stored',
    'admin.errorDiagnostics.detail.requestHeaders': 'Request header values',
    'admin.errorDiagnostics.detail.responseHeaders': 'Response header values',
    'stepUp.notEnabled': 'Enable two-factor authentication on your profile first',
    'stepUp.adminApiKeyForbidden': 'Admin API keys cannot perform this operation',
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

/**
 * Stand-in for the real TOTP dialog, which needs a Pinia store and the API
 * client. It renders only while the controller says the prompt is open and keeps
 * the controller reachable through its props, so these tests drive the real
 * `useStepUp` flow (prompt, verify, cancel, blocked) the drawer actually wires.
 */
const TotpStepUpDialogStub = {
  name: 'TotpStepUpDialog',
  props: { controller: { type: Object, required: true } },
  template: '<div v-if="controller.visible.value" data-testid="totp-step-up-dialog" />',
}

/** The controller the drawer handed to the (stubbed) TOTP dialog. */
function stepUpController(wrapper: any) {
  return wrapper.findComponent(TotpStepUpDialogStub).props('controller')
}

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
        TotpStepUpDialog: TotpStepUpDialogStub,
      },
    },
  })
}

describe('ErrorDiagnosticDetailDrawer', () => {
  beforeEach(() => {
    mocks.getDiagnostic.mockReset()
    mocks.revealDiagnosticBody.mockReset()
    mocks.revealDiagnosticHeaders.mockReset()
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

/**
 * 429 header values (Claude Messages only) are a second short-lived secret with
 * their own window and their own reveal. These tests pin the parts that keep them
 * from leaking or from being confused with the body.
 */
describe('ErrorDiagnosticDetailDrawer 429 header values', () => {
  const HEADER_REVEAL_SELECTOR = '[data-testid="error-diagnostic-header-reveal"]'
  const HEADER_VALUES_SELECTOR = '[data-testid="error-diagnostic-header-values"]'

  beforeEach(() => {
    mocks.getDiagnostic.mockReset()
    mocks.revealDiagnosticBody.mockReset()
    mocks.revealDiagnosticHeaders.mockReset()
    mocks.getDiagnostic.mockResolvedValue(attempt())
  })

  const withHeaders = (overrides: Record<string, unknown> = {}) =>
    attempt({
      header_state: 'stored',
      header_reason: 'retained',
      header_entry_count: 3,
      header_expires_at: '2999-01-01T00:00:00Z',
      ...overrides,
    })

  it('hides the section entirely when no header values were observed', async () => {
    const wrapper = mountDrawer()
    await flushPromises()

    expect(wrapper.find('[data-testid="error-diagnostic-headers"]').exists()).toBe(false)
    expect(mocks.revealDiagnosticHeaders).not.toHaveBeenCalled()
  })

  it('offers the header reveal even when no request body was retained', async () => {
    mocks.getDiagnostic.mockResolvedValue(
      withHeaders({
        body_state: 'not_observed',
        reason: 'not_observed',
        body_expires_at: undefined,
      }),
    )
    mocks.revealDiagnosticHeaders.mockResolvedValue({
      request_headers: { Host: 'api.anthropic.com' },
      response_headers: {},
      header_entry_count: 1,
      header_expires_at: '2999-01-01T00:00:00Z',
    })

    const wrapper = mountDrawer()
    await flushPromises()

    // The body is not available at all, and that does not gate the headers.
    expect(wrapper.find('[data-testid="error-diagnostic-reveal"]').exists()).toBe(false)
    expect(wrapper.find(HEADER_REVEAL_SELECTOR).exists()).toBe(true)

    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    expect(wrapper.get(HEADER_VALUES_SELECTOR).text()).toContain('api.anthropic.com')
    expect(mocks.revealDiagnosticBody).not.toHaveBeenCalled()
  })

  it('shows the state and deadline but fetches nothing until the admin clicks', async () => {
    mocks.getDiagnostic.mockResolvedValue(withHeaders())

    const wrapper = mountDrawer()
    await flushPromises()

    expect(mocks.revealDiagnosticHeaders).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="error-diagnostic-header-state"]').text()).toBe('Header values stored')
    expect(wrapper.get('[data-testid="error-diagnostic-header-expires"]').text()).toContain('Header values expire')
    expect(wrapper.find(HEADER_VALUES_SELECTOR).exists()).toBe(false)

    mocks.revealDiagnosticHeaders.mockResolvedValue({
      request_headers: { 'User-Agent': 'claude-cli/2.0.0 (external, cli)', Host: 'api.anthropic.com' },
      response_headers: { 'Retry-After': '30', 'Anthropic-Ratelimit-Requests-Remaining': '0' },
      header_entry_count: 4,
      header_expires_at: '2999-01-01T00:00:00Z',
    })

    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    expect(mocks.revealDiagnosticHeaders).toHaveBeenCalledWith('diag_1')
    const values = wrapper.get(HEADER_VALUES_SELECTOR)
    expect(values.text()).toContain('claude-cli/2.0.0 (external, cli)')
    expect(values.text()).toContain('api.anthropic.com')
    expect(values.text()).toContain('Retry-After')
    expect(values.text()).toContain('30')
    expect(values.text()).toContain('4 header values stored')
  })

  it('reveals the headers without touching the body, and the body without touching the headers', async () => {
    mocks.getDiagnostic.mockResolvedValue(withHeaders())
    mocks.revealDiagnosticHeaders.mockResolvedValue({
      request_headers: { Host: 'api.anthropic.com' },
      response_headers: {},
      header_entry_count: 1,
      header_expires_at: '2999-01-01T00:00:00Z',
    })

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    expect(mocks.revealDiagnosticBody).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="error-diagnostic-body"]').exists()).toBe(false)

    mocks.revealDiagnosticBody.mockResolvedValue({ body_text: '{"messages":[]}', body_bytes: 15 })
    await wrapper.get('[data-testid="error-diagnostic-reveal"]').trigger('click')
    await flushPromises()

    // Revealing the body leaves the header payload on screen: they are separate.
    expect(mocks.revealDiagnosticHeaders).toHaveBeenCalledTimes(1)
    expect(wrapper.get(HEADER_VALUES_SELECTOR).text()).toContain('api.anthropic.com')
    expect(wrapper.get('[data-testid="error-diagnostic-body"]').text()).toContain('{"messages":[]}')
  })

  it('never renders a credential, a cookie, an unknown header or a credential-shaped value', async () => {
    mocks.getDiagnostic.mockResolvedValue(withHeaders())
    mocks.revealDiagnosticHeaders.mockResolvedValue({
      request_headers: {
        Authorization: 'Bearer sk-canary',
        Cookie: 'session=canary',
        'X-Api-Key': 'sk-canary',
        'X-Unknown-Header': 'canary',
        // An allowlisted name carrying a credential-shaped value.
        Host: 'sk-canary',
        'User-Agent': 'Bearer canary',
      },
      response_headers: {
        'Set-Cookie': 'session=canary',
        'WWW-Authenticate': 'Basic canary',
        'Retry-After': '30',
        'Anthropic-Ratelimit-Requests-Reset': '2026-09-24T00:00:00Z',
      },
      header_entry_count: 9,
      header_expires_at: '2999-01-01T00:00:00Z',
    })

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    const html = wrapper.html()
    expect(html).not.toContain('sk-canary')
    expect(html).not.toContain('canary')
    expect(html).not.toContain('Bearer')
    expect(html).not.toContain('Basic')
    expect(html).not.toContain('session=')
    expect(html).not.toContain('Authorization')
    expect(html).not.toContain('Cookie')
    expect(html).not.toContain('X-Api-Key')
    expect(html).not.toContain('X-Unknown-Header')
    expect(html).not.toContain('WWW-Authenticate')
    // The allowlisted, innocuous values still come through.
    expect(wrapper.get(HEADER_VALUES_SELECTOR).text()).toContain('30')
    expect(wrapper.get(HEADER_VALUES_SELECTOR).text()).toContain('2026-09-24T00:00:00Z')
  })

  it('offers no header reveal for skipped, expired, purged or already-expired states', async () => {
    const states = [
      { header_state: 'skipped', header_reason: 'skipped_header_retention_disabled', header_expires_at: undefined },
      { header_state: 'expired', header_reason: 'retained' },
      { header_state: 'purged', header_reason: 'retained' },
      // Still called `stored` by the server, but past its own window.
      { header_state: 'stored', header_reason: 'retained', header_expires_at: '2020-01-01T00:00:00Z' },
    ]

    for (const state of states) {
      mocks.getDiagnostic.mockResolvedValue(withHeaders(state))

      const wrapper = mountDrawer()
      await flushPromises()

      expect(wrapper.find(HEADER_REVEAL_SELECTOR).exists()).toBe(false)
      expect(wrapper.find(HEADER_VALUES_SELECTOR).exists()).toBe(false)
      wrapper.unmount()
    }

    mocks.getDiagnostic.mockResolvedValue(withHeaders({ header_expires_at: '2020-01-01T00:00:00Z' }))
    const expired = mountDrawer()
    await flushPromises()
    expect(expired.get('[data-testid="error-diagnostic-header-state"]').text()).toBe('Header values expired')
  })

  it('re-reads the metadata after a refused reveal instead of guessing from the error code', async () => {
    mocks.getDiagnostic
      .mockResolvedValueOnce(withHeaders())
      .mockResolvedValueOnce(withHeaders({ header_state: 'expired' }))
    mocks.revealDiagnosticHeaders.mockRejectedValue({ status: 410, message: 'headers gone' })

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    expect(mocks.getDiagnostic).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('Header values expired')
    expect(wrapper.find(HEADER_VALUES_SELECTOR).exists()).toBe(false)
    expect(wrapper.find('[data-testid="error-diagnostic-header-reveal-failed"]').exists()).toBe(false)
  })

  it('keeps a genuine reveal failure visible while the values are still retained', async () => {
    mocks.getDiagnostic.mockResolvedValue(withHeaders())
    mocks.revealDiagnosticHeaders.mockRejectedValue({ status: 500, message: 'Bearer CANARY_SECRET' })

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="error-diagnostic-header-reveal-failed"]').text()).toContain(
      'The 429 header values could not be revealed',
    )
    expect(wrapper.text()).not.toContain('CANARY_SECRET')
  })

  it('drops the revealed headers when the drawer closes, and never writes them to storage', async () => {
    mocks.getDiagnostic.mockResolvedValue(withHeaders())
    mocks.revealDiagnosticHeaders.mockResolvedValue({
      request_headers: { Host: 'api.anthropic.com' },
      response_headers: {},
      header_entry_count: 1,
      header_expires_at: '2999-01-01T00:00:00Z',
    })

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()
    expect(wrapper.get(HEADER_VALUES_SELECTOR).text()).toContain('api.anthropic.com')

    const stored = [
      ...Object.keys(localStorage).map((key) => `${key}=${localStorage.getItem(key)}`),
      ...Object.keys(sessionStorage).map((key) => `${key}=${sessionStorage.getItem(key)}`),
    ].join('|')
    expect(stored).not.toContain('api.anthropic.com')

    await wrapper.setProps({ show: false })
    await flushPromises()
    expect(wrapper.find(HEADER_VALUES_SELECTOR).exists()).toBe(false)

    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(wrapper.find(HEADER_VALUES_SELECTOR).exists()).toBe(false)
    expect(mocks.revealDiagnosticHeaders).toHaveBeenCalledTimes(1)
  })

  /**
   * The header reveal route is step-up gated server-side, while the body route is
   * not. A STEP_UP_REQUIRED is therefore a prompt for the headers: the admin
   * verifies and the same explicit POST is retried once. Nothing here auto-runs —
   * the first POST is still only issued by the click, the retry by the verification.
   */
  const STEP_UP_REQUIRED = { status: 403, code: 'STEP_UP_REQUIRED', message: 'recent verification required' }
  const TOTP_DIALOG_SELECTOR = '[data-testid="totp-step-up-dialog"]'
  const HEADER_BLOCKED_SELECTOR = '[data-testid="error-diagnostic-header-reveal-blocked"]'
  const HEADER_FAILED_SELECTOR = '[data-testid="error-diagnostic-header-reveal-failed"]'

  it('prompts for step-up on a refused header reveal and retries it once after verification', async () => {
    mocks.getDiagnostic.mockResolvedValue(withHeaders())
    mocks.revealDiagnosticHeaders
      .mockRejectedValueOnce(STEP_UP_REQUIRED)
      .mockResolvedValueOnce({
        request_headers: { Host: 'api.anthropic.com' },
        response_headers: {},
        header_entry_count: 1,
        header_expires_at: '2999-01-01T00:00:00Z',
      })

    const wrapper = mountDrawer()
    await flushPromises()
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(false)

    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    // The refusal asks for verification instead of reporting a failed reveal.
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(true)
    expect(stepUpController(wrapper).visible.value).toBe(true)
    expect(wrapper.find(HEADER_FAILED_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(HEADER_BLOCKED_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(HEADER_VALUES_SELECTOR).exists()).toBe(false)
    expect(mocks.revealDiagnosticHeaders).toHaveBeenCalledTimes(1)
    // A refused reveal is not a retention change, so the metadata is not re-read.
    expect(mocks.getDiagnostic).toHaveBeenCalledTimes(1)

    stepUpController(wrapper).onVerified()
    await flushPromises()

    expect(mocks.revealDiagnosticHeaders).toHaveBeenCalledTimes(2)
    expect(wrapper.get(HEADER_VALUES_SELECTOR).text()).toContain('api.anthropic.com')
    expect(wrapper.find(HEADER_FAILED_SELECTOR).exists()).toBe(false)
    // The step-up gate covers the headers only; the body was never involved.
    expect(mocks.revealDiagnosticBody).not.toHaveBeenCalled()
  })

  it('shows no failure and drops the prompt when the step-up verification is cancelled', async () => {
    mocks.getDiagnostic.mockResolvedValue(withHeaders())
    mocks.revealDiagnosticHeaders.mockRejectedValue(STEP_UP_REQUIRED)

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(true)

    stepUpController(wrapper).onCancel()
    await flushPromises()

    // Cancelling is not a failure and reads nothing: no error text, no values.
    expect(wrapper.find(HEADER_FAILED_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(HEADER_BLOCKED_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(HEADER_VALUES_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(false)
    expect(mocks.revealDiagnosticHeaders).toHaveBeenCalledTimes(1)

    // The action is available again, so the admin can retry deliberately.
    expect(wrapper.get(HEADER_REVEAL_SELECTOR).attributes('disabled')).toBeUndefined()
  })

  it.each([
    ['STEP_UP_TOTP_NOT_ENABLED', 'Enable two-factor authentication on your profile first'],
    ['STEP_UP_ADMIN_API_KEY_FORBIDDEN', 'Admin API keys cannot perform this operation'],
  ])('reports %s with the shared step-up wording instead of a reveal failure', async (code, expected) => {
    mocks.getDiagnostic.mockResolvedValue(withHeaders())
    mocks.revealDiagnosticHeaders.mockRejectedValue({ status: 403, code, message: 'refused' })

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    expect(wrapper.get(HEADER_BLOCKED_SELECTOR).text()).toContain(expected)
    expect(wrapper.find(HEADER_FAILED_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(false)
    expect(wrapper.find(HEADER_VALUES_SELECTOR).exists()).toBe(false)
    expect(mocks.revealDiagnosticHeaders).toHaveBeenCalledTimes(1)
  })

  it('discards a step-up retry that resolves after the drawer moved to another attempt', async () => {
    let resolveRetry!: (value: unknown) => void
    mocks.getDiagnostic.mockResolvedValue(withHeaders())
    mocks.revealDiagnosticHeaders
      .mockRejectedValueOnce(STEP_UP_REQUIRED)
      .mockReturnValueOnce(
        new Promise((resolve) => {
          resolveRetry = resolve
        }),
      )

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()

    stepUpController(wrapper).onVerified()
    await flushPromises()
    expect(mocks.revealDiagnosticHeaders).toHaveBeenCalledTimes(2)

    await wrapper.setProps({ show: false })
    mocks.getDiagnostic.mockResolvedValue(withHeaders({ id: 'diag_2' }))
    await wrapper.setProps({ show: true, diagnosticId: 'diag_2' })
    await flushPromises()

    resolveRetry({
      request_headers: { Host: 'late-step-up-canary' },
      response_headers: {},
      header_entry_count: 1,
      header_expires_at: '2999-01-01T00:00:00Z',
    })
    await flushPromises()

    expect(mocks.getDiagnostic).toHaveBeenLastCalledWith('diag_2')
    expect(wrapper.text()).not.toContain('late-step-up-canary')
    expect(wrapper.find(HEADER_VALUES_SELECTOR).exists()).toBe(false)
  })

  it('dismisses an open step-up prompt when the drawer closes', async () => {
    mocks.getDiagnostic.mockResolvedValue(withHeaders())
    mocks.revealDiagnosticHeaders.mockRejectedValue(STEP_UP_REQUIRED)

    const wrapper = mountDrawer()
    await flushPromises()
    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(true)

    await wrapper.setProps({ show: false })
    await flushPromises()

    // A prompt left over from a closed drawer would float over the list and its
    // retry would target an attempt nobody is looking at.
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(false)
    expect(stepUpController(wrapper).visible.value).toBe(false)
    expect(mocks.revealDiagnosticHeaders).toHaveBeenCalledTimes(1)
    expect(wrapper.find(HEADER_FAILED_SELECTOR).exists()).toBe(false)
  })

  it('keeps the ungated body reveal out of the step-up prompt', async () => {
    // The body route is not step-up gated, so a step-up code arriving there is an
    // ordinary error: the drawer must not offer a prompt the backend never asked for.
    mocks.getDiagnostic.mockResolvedValue(withHeaders())
    mocks.revealDiagnosticBody.mockRejectedValue(STEP_UP_REQUIRED)

    const wrapper = mountDrawer()
    await flushPromises()

    await wrapper.get('[data-testid="error-diagnostic-reveal"]').trigger('click')
    await flushPromises()

    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(false)
    expect(wrapper.find('[data-testid="error-diagnostic-reveal-failed"]').exists()).toBe(true)
    expect(mocks.revealDiagnosticBody).toHaveBeenCalledTimes(1)

    // The header reveal still prompts on its own refusal.
    mocks.revealDiagnosticHeaders.mockRejectedValueOnce(STEP_UP_REQUIRED)
    await wrapper.get(HEADER_REVEAL_SELECTOR).trigger('click')
    await flushPromises()
    expect(wrapper.find(TOTP_DIALOG_SELECTOR).exists()).toBe(true)
  })
})
