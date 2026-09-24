/**
 * Component seam for the admin diagnostics list page.
 *
 * Pins that the list is metadata-only: it renders an explicit column allowlist,
 * never a whole payload, never a stored body, and never fetches a body itself.
 * It also pins the absence of a usage link when an attempt has no usage record.
 *
 * The page is also the operator's entry point for the capture gate, so it pins
 * the two ways that gate can be reported: an explicit "capture is off" state that
 * must not read as "nothing ever failed", and an unreadable status that must not
 * be shown as "off" either.
 */
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  listDiagnostics: vi.fn(),
  revealDiagnosticBody: vi.fn(),
  getOperatorSettings: vi.fn(),
}))

vi.mock('../api', () => ({
  listDiagnostics: mocks.listDiagnostics,
  revealDiagnosticBody: mocks.revealDiagnosticBody,
  getDiagnostic: vi.fn(),
  getOperatorSettings: mocks.getOperatorSettings,
  updateOperatorSettings: vi.fn(),
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const labels: Record<string, string> = {
    'admin.errorDiagnostics.title': 'Error diagnostics',
    'admin.errorDiagnostics.list.createdAt': 'Created',
    'admin.errorDiagnostics.list.protocol': 'Protocol',
    'admin.errorDiagnostics.list.attempt': 'Attempt',
    'admin.errorDiagnostics.list.upstreamStatus': 'Upstream status',
    'admin.errorDiagnostics.list.body': 'Stored body',
    'admin.errorDiagnostics.list.usage': 'Usage',
    'admin.errorDiagnostics.list.actions': 'Actions',
    'admin.errorDiagnostics.list.view': 'View',
    'admin.errorDiagnostics.list.empty': 'No diagnostics recorded',
    'admin.errorDiagnostics.list.loadFailed': 'Could not load diagnostics',
    'admin.errorDiagnostics.list.notEnabled': 'Error diagnostics are not enabled on this instance',
    'admin.errorDiagnostics.list.refresh': 'Refresh',
    'admin.errorDiagnostics.list.usageAbsent': 'No usage record',
    'admin.errorDiagnostics.list.expiresAt': 'Expires',
    'admin.errorDiagnostics.list.captureOffNotice':
      'Failed upstream attempts are not being recorded while capture is off, so nothing here does not mean nothing failed.',
    'admin.errorDiagnostics.list.capturePaused':
      'Capture is paused, so new failed upstream attempts are not being recorded; the rows below were captured before it was paused.',
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
    'admin.errorDiagnostics.reasons.skipped_too_large': 'Request exceeded the retention size limit',
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

import ErrorDiagnosticsView from '../ErrorDiagnosticsView.vue'

const row = (overrides: Record<string, unknown> = {}) => ({
  id: 'diag_1',
  created_at: '2026-09-24T00:00:00Z',
  protocol: 'messages',
  attempt_index: 1,
  upstream_status: 400,
  usage_log_id: 77,
  body_state: 'stored',
  reason: 'retained',
  body_expires_at: '2999-01-01T00:00:00Z',
  metadata_expires_at: '2999-02-01T00:00:00Z',
  ...overrides,
})

const page_ = (items: unknown[]) => ({ items, total: items.length, page: 1, page_size: 20, pages: 1 })

const DetailStub = {
  props: ['show', 'diagnosticId'],
  template: '<div data-testid="detail-stub" :data-show="String(show)" :data-id="diagnosticId ?? \'\'" />',
}

const OperatorStub = {
  name: 'ErrorDiagnosticOperatorSettings',
  props: ['status', 'loading'],
  emits: ['updated'],
  // The panel is asserted through what it displays, not through the object it was
  // handed: `data-capture-allowed` is the gate state the operator would read there.
  template:
    '<div data-testid="operator-stub" :data-has-status="String(status !== null)" :data-capture-allowed="status ? String(status.capture_allowed) : \'\'" />',
}

const operatorStatus = (overrides: Record<string, unknown> = {}) => ({
  enabled: true,
  risk_acknowledged: true,
  body_retention_enabled: true,
  capture_allowed: true,
  body_retention_allowed: true,
  body_encryption_key_available: true,
  risk_version: 'v2026.09.24',
  risk_phrase_en: 'EN statement',
  risk_phrase_zh: 'ZH statement',
  risk_acknowledgement_current: true,
  ...overrides,
})

function mountView() {
  return mount(ErrorDiagnosticsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        Pagination: { template: '<div data-testid="pagination-stub" />' },
        ErrorDiagnosticDetailDrawer: DetailStub,
        ErrorDiagnosticOperatorSettings: OperatorStub,
      },
    },
  })
}

async function emitOperatorStatus(wrapper: ReturnType<typeof mountView>, next: Record<string, unknown>) {
  const panel = wrapper.findComponent(OperatorStub)
  panel.vm.$emit('updated', next)
  await flushPromises()
}

describe('ErrorDiagnosticsView', () => {
  beforeEach(() => {
    mocks.listDiagnostics.mockReset()
    mocks.revealDiagnosticBody.mockReset()
    mocks.getOperatorSettings.mockReset()
    mocks.listDiagnostics.mockResolvedValue(page_([row()]))
    mocks.getOperatorSettings.mockResolvedValue(operatorStatus())
  })

  it('renders one metadata row per attempt and never fetches a body', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(mocks.listDiagnostics).toHaveBeenCalledWith({ page: 1, page_size: 20 })
    expect(wrapper.findAll('[data-testid="error-diagnostic-row"]')).toHaveLength(1)
    expect(wrapper.text()).toContain('Messages')
    expect(wrapper.text()).toContain('400')
    expect(wrapper.text()).toContain('Body stored')
    expect(mocks.revealDiagnosticBody).not.toHaveBeenCalled()
    // The detail drawer stays closed until the admin picks a row.
    expect(wrapper.find('[data-testid="detail-stub"]').attributes('data-show')).toBe('false')
  })

  it('renders only the column allowlist even if a row carries extra sensitive keys', async () => {
    mocks.listDiagnostics.mockResolvedValue(
      page_([
        row({
          body_text: 'LIST_BODY_CANARY',
          model: 'model-canary',
          account_id: 42,
          api_key: 'sk-canary',
          headers: { authorization: 'Bearer canary' },
        }),
      ]),
    )

    const wrapper = mountView()
    await flushPromises()

    const text = wrapper.text()
    expect(text).not.toContain('LIST_BODY_CANARY')
    expect(text).not.toContain('model-canary')
    expect(text).not.toContain('sk-canary')
    expect(text).not.toContain('Bearer canary')
  })

  it('shows the usage id as plain text and never fabricates a link', async () => {
    const withUsage = mountView()
    await flushPromises()
    expect(withUsage.findAll('[data-testid="error-diagnostic-usage-id"]')).toHaveLength(1)
    expect(withUsage.find('[data-testid="error-diagnostic-usage-id"]').text()).toContain('#77')
    // No deep link exists for a single usage log, so no anchor is offered at all.
    expect(withUsage.findAll('a')).toHaveLength(0)

    mocks.listDiagnostics.mockResolvedValue(page_([row({ usage_log_id: undefined })]))
    const withoutUsage = mountView()
    await flushPromises()

    expect(withoutUsage.findAll('[data-testid="error-diagnostic-usage-id"]')).toHaveLength(0)
    expect(withoutUsage.findAll('[data-testid="error-diagnostic-usage-absent"]')).toHaveLength(1)
    expect(withoutUsage.text()).toContain('No usage record')
  })

  it('explains an omitted body with the authoritative reason code', async () => {
    mocks.listDiagnostics.mockResolvedValue(
      page_([
        row({ body_state: 'skipped', reason: 'skipped_too_large', body_expires_at: undefined }),
      ]),
    )

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.text()).toContain('Body not retained')
    expect(wrapper.text()).toContain('Request exceeded the retention size limit')
  })

  it('opens the detail drawer for the clicked attempt', async () => {
    const wrapper = mountView()
    await flushPromises()

    await wrapper.find('[data-testid="error-diagnostic-view"]').trigger('click')
    await flushPromises()

    const detail = wrapper.find('[data-testid="detail-stub"]')
    expect(detail.attributes('data-show')).toBe('true')
    expect(detail.attributes('data-id')).toBe('diag_1')
  })

  it('renders a disabled state instead of an error when the feature is off', async () => {
    mocks.listDiagnostics.mockRejectedValue({ status: 404, message: 'not found' })

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.text()).toContain('Error diagnostics are not enabled on this instance')
    expect(wrapper.text()).not.toContain('Could not load diagnostics')
  })

  it('surfaces a generic failure without echoing raw server text', async () => {
    mocks.listDiagnostics.mockRejectedValue({ status: 500, message: 'Bearer CANARY_SECRET' })

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.text()).toContain('Could not load diagnostics')
    expect(wrapper.text()).not.toContain('CANARY_SECRET')
  })

  it('shows an empty state when no diagnostics match', async () => {
    mocks.listDiagnostics.mockResolvedValue(page_([]))

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.findAll('[data-testid="error-diagnostic-row"]')).toHaveLength(0)
    expect(wrapper.text()).toContain('No diagnostics recorded')
  })

  it('still lists records captured before capture was turned off, and never implies nothing failed', async () => {
    mocks.getOperatorSettings.mockResolvedValue(operatorStatus({ enabled: false, capture_allowed: false }))

    const wrapper = mountView()
    await flushPromises()

    // Turning capture off stops new records; it does not delete the ones already
    // written, and reads are not gated by the capture switch.
    expect(mocks.listDiagnostics).toHaveBeenCalledWith({ page: 1, page_size: 20 })
    expect(wrapper.findAll('[data-testid="error-diagnostic-row"]')).toHaveLength(1)
    expect(wrapper.text()).toContain('Capture is paused')
    expect(wrapper.text()).not.toContain('No diagnostics recorded')
    // The gate is off, not unreadable, so the operator surface still gets it.
    expect(wrapper.find('[data-testid="operator-stub"]').attributes('data-has-status')).toBe('true')
  })

  it('shows the paused notice rather than an empty list when capture is off', async () => {
    mocks.getOperatorSettings.mockResolvedValue(operatorStatus({ enabled: false, capture_allowed: false }))
    mocks.listDiagnostics.mockResolvedValue(page_([]))

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.findAll('[data-testid="error-diagnostic-row"]')).toHaveLength(0)
    expect(wrapper.text()).toContain('Capture is paused')
    // An empty list with the gate off must never read as "nothing ever failed".
    expect(wrapper.text()).toContain('nothing here does not mean nothing failed')
    expect(wrapper.text()).not.toContain('No diagnostics recorded')
    expect(wrapper.text()).not.toContain('Error diagnostics are not enabled on this instance')
  })

  it('still lists diagnostics when the gate status cannot be read', async () => {
    mocks.getOperatorSettings.mockRejectedValue({ status: 503 })

    const wrapper = mountView()
    await flushPromises()

    expect(mocks.listDiagnostics).toHaveBeenCalled()
    expect(wrapper.findAll('[data-testid="error-diagnostic-row"]')).toHaveLength(1)
    // An unreadable status must not be replaced by an invented "off" status.
    expect(wrapper.find('[data-testid="operator-stub"]').attributes('data-has-status')).toBe('false')
    expect(wrapper.text()).not.toContain('Capture is paused')
  })

  it('re-reads the gate on refresh, so an unreadable status can recover', async () => {
    mocks.getOperatorSettings.mockRejectedValueOnce({ status: 503 })
    mocks.getOperatorSettings.mockResolvedValueOnce(operatorStatus({ enabled: false, capture_allowed: false }))

    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.find('[data-testid="operator-stub"]').attributes('data-has-status')).toBe('false')

    mocks.listDiagnostics.mockClear()
    await wrapper.find('[data-testid="error-diagnostics-refresh"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="operator-stub"]').attributes('data-has-status')).toBe('true')
    expect(wrapper.text()).toContain('Capture is paused')
    // A recovered gate that says "off" still leaves the stored records readable.
    expect(mocks.listDiagnostics).toHaveBeenCalledTimes(1)
    expect(wrapper.findAll('[data-testid="error-diagnostic-row"]')).toHaveLength(1)
  })

  it('follows the operator gate: turning capture on loads, turning it off keeps the records', async () => {
    mocks.getOperatorSettings.mockResolvedValue(operatorStatus({ enabled: false, capture_allowed: false }))

    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.text()).toContain('Capture is paused')
    expect(wrapper.findAll('[data-testid="error-diagnostic-row"]')).toHaveLength(1)

    await emitOperatorStatus(wrapper, operatorStatus({ enabled: true, capture_allowed: true }))
    expect(wrapper.text()).not.toContain('Capture is paused')
    expect(wrapper.findAll('[data-testid="error-diagnostic-row"]')).toHaveLength(1)

    await emitOperatorStatus(wrapper, operatorStatus({ enabled: false, capture_allowed: false }))
    expect(wrapper.text()).toContain('Capture is paused')
    // Turning capture off stops new writes; it must not hide what is already stored.
    expect(wrapper.findAll('[data-testid="error-diagnostic-row"]')).toHaveLength(1)
  })

  it('keeps the gate state the PUT returned when an earlier status read answers after it', async () => {
    // The bootstrap read is left in flight on purpose: it is what a slow first load
    // looks like next to an operator who acts before it lands. The read, the write
    // and the late read then settle in the order GET -> PUT -> GET.
    let resolveBootstrapRead: (status: unknown) => void = () => {}
    mocks.getOperatorSettings.mockReturnValueOnce(
      new Promise((resolve) => {
        resolveBootstrapRead = resolve
      }),
    )

    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.find('[data-testid="operator-stub"]').attributes('data-capture-allowed')).toBe('')

    // Enable: the server answers with the state it now holds, and the page follows.
    await emitOperatorStatus(wrapper, operatorStatus({ enabled: true, capture_allowed: true }))
    expect(wrapper.find('[data-testid="operator-stub"]').attributes('data-capture-allowed')).toBe('true')

    // The older read finally answers with the state it saw before the write. It is
    // older than the PUT, so it must not overwrite the newer answer: the panel keeps
    // showing the state the server last returned, and the list is not re-explained
    // by a gate state the operator already moved past.
    resolveBootstrapRead(operatorStatus({ enabled: false, capture_allowed: false }))
    await flushPromises()

    expect(wrapper.find('[data-testid="operator-stub"]').attributes('data-capture-allowed')).toBe('true')
    expect(wrapper.text()).not.toContain('Capture is paused')
    expect(wrapper.findAll('[data-testid="error-diagnostic-row"]')).toHaveLength(1)
  })
})
