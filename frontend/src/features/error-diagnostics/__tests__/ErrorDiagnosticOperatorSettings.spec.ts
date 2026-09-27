/**
 * Component seam for the error-diagnostic operator gate.
 *
 * ADR 0005 makes production capture opt-in and refuses to let a boolean stand in
 * for a written acknowledgement. This component is where an operator performs
 * that act, so the tests pin the safety properties rather than the layout:
 *
 *   - the server is the authority: the panel renders the status it was given and
 *     reports back exactly what the server returned, never an optimistic flip;
 *   - enabling is deliberate every single time: the required statement comes from
 *     the server, must be typed verbatim, and the field is cleared afterwards;
 *   - disabling never needs a phrase, a language or an identity;
 *   - nothing about the operator (IP, User-Agent), no key and no body is ever
 *     rendered or placed in browser storage.
 */
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  getOperatorSettings: vi.fn(),
  updateOperatorSettings: vi.fn(),
  revealDiagnosticBody: vi.fn(),
}))

vi.mock('../api', () => ({
  getOperatorSettings: mocks.getOperatorSettings,
  updateOperatorSettings: mocks.updateOperatorSettings,
  revealDiagnosticBody: mocks.revealDiagnosticBody,
  listDiagnostics: vi.fn(),
  getDiagnostic: vi.fn(),
}))

const labels: Record<string, string> = {
  'admin.errorDiagnostics.operator.title': 'Operator capture settings',
  'admin.errorDiagnostics.operator.description': 'Capture is off unless an operator acknowledges the written risk statement.',
  'admin.errorDiagnostics.operator.loading': 'Loading operator settings…',
  'admin.errorDiagnostics.operator.unavailable': 'Operator settings are temporarily unavailable; this is not a statement that capture is off.',
  'admin.errorDiagnostics.operator.state.captureLabel': 'Effective capture',
  'admin.errorDiagnostics.operator.state.on': 'On',
  'admin.errorDiagnostics.operator.state.off': 'Off',
  'admin.errorDiagnostics.operator.state.retentionLabel': 'Effective encrypted request body retention',
  'admin.errorDiagnostics.operator.state.headerValuesLabel': 'Effective encrypted 429 header value retention',
  'admin.errorDiagnostics.operator.state.plainBodyLabel': 'Effective plaintext request body retention',
  'admin.errorDiagnostics.operator.state.plainHeaderValuesLabel': 'Effective plaintext 429 header value retention',
  'admin.errorDiagnostics.operator.state.keyLabel': 'Encryption key',
  'admin.errorDiagnostics.operator.state.keyAvailable': 'Configured and restart-stable',
  'admin.errorDiagnostics.operator.state.keyUnavailable': 'Not configured or not restart-stable',
  'admin.errorDiagnostics.operator.state.storedLabel': 'Stored setting',
  'admin.errorDiagnostics.operator.state.captureMismatchStaleAck': 'Capture is stored as on, but it is not running: the acknowledgement does not cover the current statement.',
  'admin.errorDiagnostics.operator.state.captureMismatchNoAck': 'Capture is stored as on, but it is not running: no written acknowledgement is in effect.',
  'admin.errorDiagnostics.operator.state.retentionMismatchCapture': 'Request body retention is stored as on, but capture is not running.',
  'admin.errorDiagnostics.operator.state.retentionMismatchKey': 'Request body retention is stored as on, but no usable encryption key is configured.',
  'admin.errorDiagnostics.operator.state.headerValuesMismatchCapture': '429 header value retention is stored as on, but capture is not running.',
  'admin.errorDiagnostics.operator.state.headerValuesMismatchKey': '429 header value retention is stored as on, but no usable encryption key is configured.',
  'admin.errorDiagnostics.operator.state.plainBodyMismatchCapture': 'Plaintext request body retention is stored as on, but capture is not running.',
  'admin.errorDiagnostics.operator.state.plainBodyMismatchStaleAck': 'Plaintext request body retention is stored as on, but its own recorded acknowledgement does not cover the current plaintext statement.',
  'admin.errorDiagnostics.operator.state.plainBodyMismatchNoAck': 'Plaintext request body retention is stored as on, but no written acknowledgement of its own statement is in effect.',
  'admin.errorDiagnostics.operator.state.plainHeaderValuesMismatchCapture': 'Plaintext 429 header value retention is stored as on, but capture is not running.',
  'admin.errorDiagnostics.operator.state.plainHeaderValuesMismatchStaleAck': 'Plaintext 429 header value retention is stored as on, but its own recorded acknowledgement does not cover the current plaintext statement.',
  'admin.errorDiagnostics.operator.state.plainHeaderValuesMismatchNoAck': 'Plaintext 429 header value retention is stored as on, but no written acknowledgement of its own statement is in effect.',
  'admin.errorDiagnostics.operator.ack.none': 'No written risk acknowledgement has been recorded.',
  'admin.errorDiagnostics.operator.ack.stale': 'The recorded acknowledgement does not cover the current statement version; it must be given again.',
  'admin.errorDiagnostics.operator.ack.version': 'Statement version',
  'admin.errorDiagnostics.operator.ack.operator': 'Acknowledged by admin user',
  'admin.errorDiagnostics.operator.ack.acceptedAt': 'Acknowledged at',
  'admin.errorDiagnostics.operator.ack.phrase': 'Statement accepted',
  'admin.errorDiagnostics.operator.ack.captureTitle': 'Capture gate — statement',
  'admin.errorDiagnostics.operator.ack.plainBodyTitle': 'Plaintext request body — statement',
  'admin.errorDiagnostics.operator.ack.plainHeaderTitle': 'Plaintext 429 header values — statement',
  'admin.errorDiagnostics.operator.enable.title': 'Enable capture',
  'admin.errorDiagnostics.operator.enable.titleRetention': 'Enable retention',
  'admin.errorDiagnostics.operator.enable.titleLayers': 'Retention layers',
  'admin.errorDiagnostics.operator.enable.notice': 'Enabling requires typing the current statement exactly; the statement is never stored in this browser.',
  'admin.errorDiagnostics.operator.enable.layersNotice': 'Every change here is applied with the capture statement above, which the server records again.',
  'admin.errorDiagnostics.operator.enable.language': 'Statement language',
  'admin.errorDiagnostics.operator.enable.requiredPhrase': 'Required statement',
  'admin.errorDiagnostics.operator.enable.copyPhrase': 'Copy statement',
  'admin.errorDiagnostics.operator.enable.phraseLabel': 'Type the statement to confirm',
  'admin.errorDiagnostics.operator.enable.phrasePlaceholder': 'Type the statement above exactly',
  'admin.errorDiagnostics.operator.enable.retentionToggle': 'Also retain request bodies (encrypted)',
  'admin.errorDiagnostics.operator.enable.retentionUnavailable': 'Request body retention needs a configured, restart-stable encryption key; capture can still be enabled without it.',
  'admin.errorDiagnostics.operator.enable.headerValuesToggle': 'Also retain upstream 429 header values (encrypted)',
  'admin.errorDiagnostics.operator.enable.headerValuesUnavailable': '429 header value retention needs a configured, restart-stable encryption key. It is a separate switch from request body retention.',
  'admin.errorDiagnostics.operator.enable.retentionBlocked': 'Request body and 429 header value retention both need a configured, restart-stable encryption key. Capture can be enabled without them.',
  'admin.errorDiagnostics.operator.enable.confirm': 'Enable',
  'admin.errorDiagnostics.operator.enable.applyLayers': 'Apply layer changes',
  'admin.errorDiagnostics.operator.enable.confirming': 'Enabling…',
  'admin.errorDiagnostics.operator.disable.action': 'Disable',
  'admin.errorDiagnostics.operator.disable.disabling': 'Disabling…',
  'admin.errorDiagnostics.operator.disable.notice': 'Disabling is always allowed and needs no statement; it also turns every retention layer off.',
  'admin.errorDiagnostics.operator.errors.phraseRequired': 'The statement is required to enable capture.',
  'admin.errorDiagnostics.operator.errors.phraseInvalid': 'The statement does not match the required statement.',
  'admin.errorDiagnostics.operator.errors.keyUnavailable': 'Request body retention needs a configured, restart-stable encryption key.',
  'admin.errorDiagnostics.operator.errors.headerKeyUnavailable': '429 header value retention needs a configured, restart-stable encryption key.',
  'admin.errorDiagnostics.operator.errors.sessionRequired': 'Enabling needs an authenticated admin session to record the acknowledgement.',
  'admin.errorDiagnostics.operator.errors.adminApiKeyForbidden': 'Enabling needs an admin session, not an admin API key; capture can still be disabled.',
  'admin.errorDiagnostics.operator.errors.unavailable': 'The operator settings are temporarily unavailable; nothing was changed.',
  'admin.errorDiagnostics.operator.errors.generic': 'The change could not be applied; the server rejected it.',
  // 部署前提（ADR 0007）：两个明文层共同的数据库前提，以及它的拒绝文案。
  'admin.errorDiagnostics.operator.state.deploymentLabel': 'Plaintext deployment check',
  'admin.errorDiagnostics.operator.state.plaintextDeploymentBlocked':
    'Both plaintext layers are refused on this deployment because the database cannot guarantee that plaintext rows disappear together with their usage record.',
  'admin.errorDiagnostics.operator.state.deploymentSupported':
    'The database can guarantee that plaintext rows disappear together with their usage record.',
  'admin.errorDiagnostics.operator.state.deploymentPartitioned':
    'usage_logs is a partitioned table, so the ownership the plaintext layers rely on cannot be guaranteed.',
  'admin.errorDiagnostics.operator.state.deploymentMissingOwnership':
    'The usage-owned plaintext tables no longer carry their ownership foreign key.',
  'admin.errorDiagnostics.operator.state.deploymentProbeFailed':
    'The deployment check could not be answered right now.',
  'admin.errorDiagnostics.operator.state.deploymentProbeUnavailable':
    'This instance has no deployment check wired in.',
  'admin.errorDiagnostics.operator.state.deploymentUnknown':
    'The deployment check returned a shape this build does not recognise.',
  'admin.errorDiagnostics.operator.errors.deploymentUnsupported':
    'The plaintext layers cannot be enabled on this deployment; nothing was changed.',
}

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        (labels[key] ?? key).replace(/\{(\w+)\}/g, (_, token) => String(params?.[token] ?? '')),
    }),
  }
})

import ErrorDiagnosticOperatorSettings from '../components/ErrorDiagnosticOperatorSettings.vue'
import type { ErrorDiagnosticOperatorStatus } from '../types'

const PHRASE_EN = 'Statement EN: bodies are retained 7 days, metadata 30 days, and this is not an erasure tool.'
const PHRASE_ZH = '确认语句：正文保留 7 天、元数据保留 30 天，且本功能不是擦除手段。'
const PLAIN_BODY_PHRASE_EN = 'Plaintext EN: unencrypted at rest, follows its usage, 30 days unlinked.'
const PLAIN_BODY_PHRASE_ZH = '明文语句：明文落库，随用量记录，未关联三十天。'
const PLAIN_HEADER_PHRASE_EN = 'Plaintext headers EN: allowlist only, unencrypted at rest.'
const PLAIN_HEADER_PHRASE_ZH = '明文头值语句：仅白名单，明文落库。'


const status = (overrides: Partial<ErrorDiagnosticOperatorStatus> = {}): ErrorDiagnosticOperatorStatus => ({
  enabled: false,
  risk_acknowledged: false,
  body_retention_enabled: false,
  header_values_enabled: false,
  capture_allowed: false,
  body_retention_allowed: false,
  header_values_allowed: false,
  body_encryption_key_available: true,
  risk_version: 'v2026.09.27.2',
  risk_phrase_en: PHRASE_EN,
  risk_phrase_zh: PHRASE_ZH,
  risk_acknowledgement_current: false,
  plain_body_enabled: false,
  plain_body_allowed: false,
  plain_header_values_enabled: false,
  plain_header_values_allowed: false,
  plain_body_risk_version: 'v2026.09.27.1',
  plain_body_risk_phrase_en: PLAIN_BODY_PHRASE_EN,
  plain_body_risk_phrase_zh: PLAIN_BODY_PHRASE_ZH,
  plain_header_risk_version: 'v2026.09.27.1',
  plain_header_risk_phrase_en: PLAIN_HEADER_PHRASE_EN,
  plain_header_risk_phrase_zh: PLAIN_HEADER_PHRASE_ZH,
  plain_body_risk_acknowledgement_current: false,
  plain_header_risk_acknowledgement_current: false,
  plaintext_capture_supported: true,
  plaintext_capture_support_reason: 'supported',
  ...overrides,
})

function mountPanel(props: {
  status?: ErrorDiagnosticOperatorStatus | null
  loading?: boolean
  unavailable?: boolean
} = {}) {
  return mount(ErrorDiagnosticOperatorSettings, {
    props: {
      status: props.status === undefined ? status() : props.status,
      loading: props.loading ?? false,
      unavailable: props.unavailable ?? false,
    },
  })
}

const phraseInput = (wrapper: ReturnType<typeof mountPanel>) =>
  wrapper.find('[data-testid="operator-phrase-input"]')

async function typePhrase(wrapper: ReturnType<typeof mountPanel>, phrase: string) {
  await phraseInput(wrapper).setValue(phrase)
}

const enableButton = (wrapper: ReturnType<typeof mountPanel>) => wrapper.find('[data-testid="operator-enable"]')
const disableButton = (wrapper: ReturnType<typeof mountPanel>) => wrapper.find('[data-testid="operator-disable"]')

describe('ErrorDiagnosticOperatorSettings', () => {
  beforeEach(() => {
    mocks.getOperatorSettings.mockReset()
    mocks.updateOperatorSettings.mockReset()
    mocks.revealDiagnosticBody.mockReset()
    localStorage.clear()
    sessionStorage.clear()
  })

  it('renders the default-off state the server reported', () => {
    const wrapper = mountPanel()

    expect(wrapper.find('[data-testid="operator-capture-state"]').text()).toContain('Off')
    expect(wrapper.find('[data-testid="operator-body-retention-state"]').text()).toContain('Off')
    expect(wrapper.text()).toContain('No written risk acknowledgement has been recorded')
    // Default off is a real state, not an error and not an unknown.
    expect(wrapper.find('[data-testid="operator-state-unavailable"]').exists()).toBe(false)
    expect(mocks.revealDiagnosticBody).not.toHaveBeenCalled()
  })

  it('reports encryption key availability without ever printing a key', () => {
    const available = mountPanel({ status: status({ body_encryption_key_available: true }) })
    expect(available.find('[data-testid="operator-encryption-key"]').text()).toContain('Configured and restart-stable')

    const missing = mountPanel({ status: status({ body_encryption_key_available: false }) })
    expect(missing.find('[data-testid="operator-encryption-key"]').text()).toContain('Not configured or not restart-stable')
  })

  it('never renders an operator IP, User-Agent, key or body even if the payload carried one', () => {
    const extra = {
      ...status({ enabled: true, capture_allowed: true }),
      ip_address: '203.0.113.9',
      user_agent: 'CANARY_UA',
      encryption_key: 'CANARY_KEY',
      admin_api_key: 'sk-CANARY',
      body_text: 'CANARY_BODY',
      body_ciphertext: 'CANARY_CIPHERTEXT',
    } as unknown as ErrorDiagnosticOperatorStatus

    const wrapper = mountPanel({ status: extra })
    const text = wrapper.text()

    expect(text).not.toContain('203.0.113.9')
    expect(text).not.toContain('CANARY_UA')
    expect(text).not.toContain('CANARY_KEY')
    expect(text).not.toContain('sk-CANARY')
    expect(text).not.toContain('CANARY_BODY')
    expect(text).not.toContain('CANARY_CIPHERTEXT')
  })

  it('never presents an unreadable status as disabled', () => {
    const unavailable = mountPanel({ status: null, unavailable: true })
    expect(unavailable.find('[data-testid="operator-state-unavailable"]').exists()).toBe(true)
    expect(unavailable.find('[data-testid="operator-capture-state"]').exists()).toBe(false)

    // No status and no failure flag is still "unknown", not "off".
    const silent = mountPanel({ status: null, unavailable: false })
    expect(silent.find('[data-testid="operator-state-unavailable"]').exists()).toBe(true)

    const loading = mountPanel({ status: null, loading: true })
    expect(loading.find('[data-testid="operator-state-loading"]').exists()).toBe(true)
  })

  it('shows the statement exactly as the server sent it for the selected language', async () => {
    const wrapper = mountPanel()

    expect(wrapper.find('[data-testid="operator-required-phrase"]').text()).toBe(PHRASE_EN)

    await wrapper.find('[data-testid="operator-language-zh"]').setValue()

    expect(wrapper.find('[data-testid="operator-required-phrase"]').text()).toBe(PHRASE_ZH)
  })

  it('requires the typed statement to match exactly, without transforming it', async () => {
    const wrapper = mountPanel()

    expect(enableButton(wrapper).attributes('disabled')).toBeDefined()

    await typePhrase(wrapper, PHRASE_EN.toUpperCase())
    expect(enableButton(wrapper).attributes('disabled')).toBeDefined()

    // A near miss: the same statement with one word changed must not be accepted.
    await typePhrase(wrapper, PHRASE_EN.replace('not an erasure tool', 'an erasure tool'))
    expect(enableButton(wrapper).attributes('disabled')).toBeDefined()

    await typePhrase(wrapper, PHRASE_EN)
    expect(enableButton(wrapper).attributes('disabled')).toBeUndefined()
  })

  it('clears a typed statement when the acknowledgement language changes', async () => {
    const wrapper = mountPanel()
    await typePhrase(wrapper, PHRASE_EN)
    expect(enableButton(wrapper).attributes('disabled')).toBeUndefined()

    await wrapper.find('[data-testid="operator-language-zh"]').setValue()

    expect((phraseInput(wrapper).element as HTMLTextAreaElement).value).toBe('')
    expect(enableButton(wrapper).attributes('disabled')).toBeDefined()
  })

  it('sends the enable request for the acknowledged language and nothing else', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(status({ enabled: true, capture_allowed: true }))
    const wrapper = mountPanel()
    await typePhrase(wrapper, PHRASE_EN)

    await enableButton(wrapper).trigger('click')
    await flushPromises()

    expect(mocks.updateOperatorSettings).toHaveBeenCalledTimes(1)
    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      body_retention_enabled: false,
      header_values_enabled: false,
      language: 'en',
      phrase: PHRASE_EN,
      plain_body_enabled: false,
      plain_header_values_enabled: false,
      plain_body_phrase: '',
      plain_header_values_phrase: '',
    })
    expect(wrapper.emitted('updated')?.[0]?.[0]).toMatchObject({ enabled: true, capture_allowed: true })
  })

  it('sends the Chinese request when the operator acknowledges the Chinese statement', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(status({ enabled: true, capture_allowed: true }))
    const wrapper = mountPanel()
    await wrapper.find('[data-testid="operator-language-zh"]').setValue()
    await typePhrase(wrapper, PHRASE_ZH)

    await enableButton(wrapper).trigger('click')
    await flushPromises()

    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      body_retention_enabled: false,
      header_values_enabled: false,
      language: 'zh',
      phrase: PHRASE_ZH,
      plain_body_enabled: false,
      plain_header_values_enabled: false,
      plain_body_phrase: '',
      plain_header_values_phrase: '',
    })
  })

  it('asks for the statement again for every enable, and never keeps it', async () => {
    // 「全部开启」必须把四个留存层都算进去：新明文层默认关闭，只开密文层时
    // 表单仍然有可做的事，界面不该把「还有一层没开」显示成「没有可开启项」。
    const enabled = status({
      enabled: true,
      capture_allowed: true,
      body_retention_enabled: true,
      body_retention_allowed: true,
      header_values_enabled: true,
      header_values_allowed: true,
      plain_body_enabled: true,
      plain_body_allowed: true,
      plain_header_values_enabled: true,
      plain_header_values_allowed: true,
    })
    mocks.updateOperatorSettings.mockResolvedValue(enabled)
    const wrapper = mountPanel()
    await typePhrase(wrapper, PHRASE_EN)
    await enableButton(wrapper).trigger('click')
    await flushPromises()

    // The exact same confirmation cannot be replayed: the field is cleared and the
    // action is unavailable again until the operator types the statement anew.
    expect((phraseInput(wrapper).element as HTMLTextAreaElement).value).toBe('')
    expect(enableButton(wrapper).attributes('disabled')).toBeDefined()

    // With every layer on the form stays available — that form is the only place a
    // single layer can be turned off again — but it no longer promises an "enable":
    // it applies layer changes, and needs the statement typed afresh.
    await wrapper.setProps({ status: enabled })
    expect(enableButton(wrapper).exists()).toBe(true)
    expect(enableButton(wrapper).text()).toBe('Apply layer changes')
    expect(enableButton(wrapper).attributes('disabled')).toBeDefined()
    expect(mocks.updateOperatorSettings).toHaveBeenCalledTimes(1)
  })

  it('never writes the typed statement to browser storage', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    mocks.updateOperatorSettings.mockResolvedValue(status({ enabled: true, capture_allowed: true }))

    const wrapper = mountPanel()
    await typePhrase(wrapper, PHRASE_EN)
    await enableButton(wrapper).trigger('click')
    await flushPromises()

    for (const call of setItem.mock.calls) {
      const serialized = call.map((value) => String(value)).join('|')
      expect(serialized).not.toContain(PHRASE_EN)
      expect(serialized).not.toContain(PHRASE_ZH)
    }
    expect(JSON.stringify({ ...localStorage })).not.toContain(PHRASE_EN)
    expect(JSON.stringify({ ...sessionStorage })).not.toContain(PHRASE_EN)
    setItem.mockRestore()
  })

  it('can always be disabled, with no statement and no language', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(status())
    const wrapper = mountPanel({
      status: status({
        enabled: true,
        capture_allowed: true,
        body_retention_enabled: false,
        body_retention_allowed: false,
        risk_acknowledgement_current: false,
      }),
    })

    expect(disableButton(wrapper).attributes('disabled')).toBeUndefined()
    expect((phraseInput(wrapper).element as HTMLTextAreaElement).value).toBe('')

    await disableButton(wrapper).trigger('click')
    await flushPromises()

    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: false,
      body_retention_enabled: false,
      header_values_enabled: false,
      language: expect.any(String),
      phrase: '',
      plain_body_enabled: false,
      plain_header_values_enabled: false,
      plain_body_phrase: '',
      plain_header_values_phrase: '',
    })
  })

  it('keeps the disable action usable after a refused enable', async () => {
    mocks.updateOperatorSettings.mockRejectedValueOnce({
      status: 400,
      reason: 'ERROR_DIAGNOSTIC_RISK_ACK_INVALID',
      message: 'raw server text CANARY',
    })
    const wrapper = mountPanel({
      status: status({
        enabled: true,
        capture_allowed: true,
        body_retention_enabled: false,
        risk_acknowledgement_current: false,
      }),
    })

    await typePhrase(wrapper, PHRASE_EN)
    await enableButton(wrapper).trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="operator-error"]').text()).toContain('does not match the required statement')
    expect(wrapper.text()).not.toContain('CANARY')
    expect(disableButton(wrapper).attributes('disabled')).toBeUndefined()
  })

  it('maps stable server reason codes to its own copy and never echoes server text', async () => {
    const cases: Array<[string, string]> = [
      ['ERROR_DIAGNOSTIC_RISK_ACK_REQUIRED', 'The statement is required to enable capture.'],
      ['ERROR_DIAGNOSTIC_RISK_ACK_INVALID', 'The statement does not match the required statement.'],
      ['ERROR_DIAGNOSTIC_BODY_KEY_UNAVAILABLE', 'Request body retention needs a configured, restart-stable encryption key.'],
      ['ERROR_DIAGNOSTIC_HEADER_KEY_UNAVAILABLE', '429 header value retention needs a configured, restart-stable encryption key.'],
      ['ERROR_DIAGNOSTIC_OPERATOR_SESSION_REQUIRED', 'Enabling needs an authenticated admin session to record the acknowledgement.'],
      ['ERROR_DIAGNOSTIC_ADMIN_API_KEY_FORBIDDEN', 'Enabling needs an admin session, not an admin API key; capture can still be disabled.'],
      ['ERROR_DIAGNOSTIC_SETTINGS_UNAVAILABLE', 'The operator settings are temporarily unavailable; nothing was changed.'],
    ]

    for (const [reason, expected] of cases) {
      mocks.updateOperatorSettings.mockRejectedValueOnce({ status: 500, reason, message: 'SERVER_CANARY' })
      const wrapper = mountPanel()
      await typePhrase(wrapper, PHRASE_EN)
      await enableButton(wrapper).trigger('click')
      await flushPromises()

      const error = wrapper.find('[data-testid="operator-error"]')
      expect(error.text(), reason).toContain(expected)
      expect(error.text(), reason).not.toContain('SERVER_CANARY')
    }

    // A response without a reason (malformed body, storage failure) is a generic
    // failure, never a silent success.
    mocks.updateOperatorSettings.mockRejectedValueOnce({ status: 500 })
    const wrapper = mountPanel()
    await typePhrase(wrapper, PHRASE_EN)
    await enableButton(wrapper).trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="operator-error"]').text()).toContain('could not be applied')
  })

  it('offers body retention only when the key can actually be used', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(
      status({ enabled: true, capture_allowed: true, body_retention_enabled: true, body_retention_allowed: true }),
    )

    const noKey = mountPanel({ status: status({ body_encryption_key_available: false }) })
    const noKeyToggle = noKey.find('[data-testid="operator-retention-toggle"]')
    expect(noKeyToggle.attributes('disabled')).toBeDefined()
    expect(noKey.find('[data-testid="operator-retention-unavailable"]').exists()).toBe(true)

    const withKey = mountPanel({ status: status({ body_encryption_key_available: true }) })
    await withKey.find('[data-testid="operator-retention-toggle"]').setValue(true)
    await typePhrase(withKey, PHRASE_EN)
    await enableButton(withKey).trigger('click')
    await flushPromises()

    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      body_retention_enabled: true,
      header_values_enabled: false,
      language: 'en',
      phrase: PHRASE_EN,
      plain_body_enabled: false,
      plain_header_values_enabled: false,
      plain_body_phrase: '',
      plain_header_values_phrase: '',
    })
  })

  it('explains why retention cannot be turned on when capture is already running', () => {
    const wrapper = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: true,
        risk_acknowledgement_current: true,
        body_retention_enabled: false,
        body_retention_allowed: false,
        body_encryption_key_available: false,
        // 两个密文层都已开着：没有密钥时没有任何一层还能被打开。
        header_values_enabled: true,
        header_values_allowed: false,
      }),
    })

    expect(wrapper.find('[data-testid="operator-retention-blocked"]').exists()).toBe(true)
    // 缺密钥只挡住两个密文层；两个明文层不加密，因此仍然可以被打开——
    // 这正是「缺密钥」与「明文留存不可用」必须分开显示的实例。
    expect(enableButton(wrapper).exists()).toBe(true)
    expect(wrapper.find('[data-testid="operator-plain-body-toggle"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="operator-plain-body-unavailable"]').exists()).toBe(false)
    expect(disableButton(wrapper).exists()).toBe(true)
  })

  it('renders the verified gates the server reported, not the stored flags', () => {
    const running = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: true,
        body_retention_enabled: true,
        body_retention_allowed: true,
        risk_acknowledgement_current: true,
      }),
    })
    expect(running.find('[data-testid="operator-capture-state"]').text()).toContain('On')
    expect(running.find('[data-testid="operator-body-retention-state"]').text()).toContain('On')
    expect(running.find('[data-testid="operator-capture-mismatch"]').exists()).toBe(false)
    expect(running.find('[data-testid="operator-retention-mismatch"]').exists()).toBe(false)

    // Stored on without a current acknowledgement: the server reports this on
    // purpose, and the panel must not present it as capturing.
    const staleAck = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: false,
        risk_acknowledgement_current: false,
        body_retention_enabled: true,
        body_retention_allowed: false,
      }),
    })
    expect(staleAck.find('[data-testid="operator-capture-state"]').text()).toContain('Off')
    expect(staleAck.find('[data-testid="operator-body-retention-state"]').text()).toContain('Off')
    expect(staleAck.find('[data-testid="operator-capture-mismatch"]').text()).toContain(
      'the acknowledgement does not cover the current statement',
    )
    expect(staleAck.find('[data-testid="operator-retention-mismatch"]').text()).toContain('capture is not running')
    // Capture is not running, so the panel offers the action that fixes it.
    expect(enableButton(staleAck).exists()).toBe(true)
  })

  it('explains a stored-on gate with no acknowledgement flag differently', () => {
    const noAck = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: false,
        capture_allowed: false,
        risk_acknowledgement_current: false,
      }),
    })

    expect(noAck.find('[data-testid="operator-capture-mismatch"]').text()).toContain(
      'no written acknowledgement is in effect',
    )
    expect(enableButton(noAck).exists()).toBe(true)
  })

  it('keeps the stored retention intent when an acknowledgement has to be re-given', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(status())
    const wrapper = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: false,
        risk_acknowledgement_current: false,
        body_retention_enabled: true,
        body_retention_allowed: false,
        body_encryption_key_available: true,
      }),
    })

    const toggle = wrapper.find('[data-testid="operator-retention-toggle"]')
    expect((toggle.element as HTMLInputElement).checked).toBe(true)

    await typePhrase(wrapper, PHRASE_EN)
    await enableButton(wrapper).trigger('click')
    await flushPromises()

    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      body_retention_enabled: true,
      header_values_enabled: false,
      language: 'en',
      phrase: PHRASE_EN,
      plain_body_enabled: false,
      plain_header_values_enabled: false,
      plain_body_phrase: '',
      plain_header_values_phrase: '',
    })
  })

  it('surfaces a stale or missing acknowledgement as something to re-give', () => {
    const none = mountPanel()
    expect(none.find('[data-testid="operator-ack-none"]').exists()).toBe(true)
    expect(none.find('[data-testid="operator-ack-stale"]').exists()).toBe(false)

    const stale = mountPanel({
      status: status({
        risk_acknowledgement_current: false,
        risk_acknowledgement: {
          version: 'v2026.01.01',
          phrase: PHRASE_EN,
          admin_user_id: 7,
          accepted_at: '2026-01-01T00:00:00Z',
        },
      }),
    })
    expect(stale.find('[data-testid="operator-ack-stale"]').exists()).toBe(true)
    expect(stale.find('[data-testid="operator-ack-none"]').exists()).toBe(false)
    expect(stale.find('[data-testid="operator-ack-version"]').text()).toContain('v2026.01.01')
    expect(stale.find('[data-testid="operator-ack-operator"]').text()).toContain('7')

    const current = mountPanel({
      status: status({
        risk_acknowledgement_current: true,
        risk_acknowledgement: {
          version: 'v2026.09.24',
          phrase: PHRASE_EN,
          admin_user_id: 7,
          accepted_at: '2026-09-24T00:00:00Z',
        },
      }),
    })
    expect(current.find('[data-testid="operator-ack-stale"]').exists()).toBe(false)
    expect(current.find('[data-testid="operator-ack-none"]').exists()).toBe(false)
  })

  it('copies the statement only when the operator asks for it', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })

    const wrapper = mountPanel()
    expect(writeText).not.toHaveBeenCalled()

    await wrapper.find('[data-testid="operator-copy-phrase"]').trigger('click')
    await flushPromises()

    expect(writeText).toHaveBeenCalledWith(PHRASE_EN)
  })

  // ---------------------------------------------------------------------------
  // 429 header value retention: a second layer with its own switch and its own row
  // ---------------------------------------------------------------------------

  it('reports the two retention layers as separate verified states', () => {
    const headerOnly = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: true,
        risk_acknowledgement_current: true,
        body_retention_enabled: false,
        body_retention_allowed: false,
        header_values_enabled: true,
        header_values_allowed: true,
      }),
    })

    // One row per layer: the header layer being on says nothing about the body layer.
    expect(headerOnly.find('[data-testid="operator-body-retention-state"]').text()).toContain('Off')
    expect(headerOnly.find('[data-testid="operator-header-values-state"]').text()).toContain('On')
    expect(headerOnly.find('[data-testid="operator-header-values-mismatch"]').exists()).toBe(false)

    const bodyOnly = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: true,
        risk_acknowledgement_current: true,
        body_retention_enabled: true,
        body_retention_allowed: true,
        header_values_enabled: false,
        header_values_allowed: false,
      }),
    })
    expect(bodyOnly.find('[data-testid="operator-body-retention-state"]').text()).toContain('On')
    expect(bodyOnly.find('[data-testid="operator-header-values-state"]').text()).toContain('Off')
  })

  it('enables the plaintext body layer only with its own statement', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(
      status({
        enabled: true,
        capture_allowed: true,
        plain_body_enabled: true,
        plain_body_allowed: true,
      }),
    )
    const wrapper = mountPanel()

    // 明文层不需要密钥：即使部署没有可用密钥，它的开关也必须可用。
    expect((wrapper.find('[data-testid="operator-plain-body-toggle"]').element as HTMLInputElement).checked).toBe(false)
    await wrapper.find('[data-testid="operator-plain-body-toggle"]').setValue(true)
    await typePhrase(wrapper, PHRASE_EN)

    // 采集语句不覆盖明文层：它自己的语句必须逐字输入，否则按钮保持不可用。
    expect(enableButton(wrapper).attributes('disabled')).toBeDefined()
    await wrapper
      .find('[data-testid="operator-plain-body-phrase-input"]')
      .setValue(PLAIN_BODY_PHRASE_EN)
    await enableButton(wrapper).trigger('click')
    await flushPromises()

    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      body_retention_enabled: false,
      header_values_enabled: false,
      plain_body_enabled: true,
      plain_header_values_enabled: false,
      language: 'en',
      phrase: PHRASE_EN,
      plain_body_phrase: PLAIN_BODY_PHRASE_EN,
      plain_header_values_phrase: '',
    })
    // 提交后勾选被清掉（语句输入块随之消失）：下一次开启必须重新逐字确认，
    // 而不是把刚输入过的语句留在界面上等着被再次提交。
    expect((wrapper.find('[data-testid="operator-plain-body-toggle"]').element as HTMLInputElement).checked).toBe(false)
    expect(wrapper.find('[data-testid="operator-plain-body-phrase-input"]').exists()).toBe(false)
  })

  it('enables only the header value layer when the operator asks for it', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(
      status({
        enabled: true,
        capture_allowed: true,
        header_values_enabled: true,
        header_values_allowed: true,
      }),
    )
    const wrapper = mountPanel()

    // Nothing is pre-selected for the operator: both layers start off and stay off
    // unless they are ticked and the statement is typed.
    expect((wrapper.find('[data-testid="operator-retention-toggle"]').element as HTMLInputElement).checked).toBe(false)
    expect((wrapper.find('[data-testid="operator-header-values-toggle"]').element as HTMLInputElement).checked).toBe(false)

    await wrapper.find('[data-testid="operator-header-values-toggle"]').setValue(true)
    await typePhrase(wrapper, PHRASE_EN)
    await enableButton(wrapper).trigger('click')
    await flushPromises()

    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      body_retention_enabled: false,
      header_values_enabled: true,
      language: 'en',
      phrase: PHRASE_EN,
      plain_body_enabled: false,
      plain_header_values_enabled: false,
      plain_body_phrase: '',
      plain_header_values_phrase: '',
    })
  })

  it('offers the header value switch even when body retention is already running', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(
      status({
        enabled: true,
        capture_allowed: true,
        body_retention_enabled: true,
        body_retention_allowed: true,
        header_values_enabled: true,
        header_values_allowed: true,
      }),
    )
    const wrapper = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: true,
        risk_acknowledgement_current: true,
        body_retention_enabled: true,
        body_retention_allowed: true,
        header_values_enabled: false,
        header_values_allowed: false,
      }),
    })

    // The form appears for the layer that is still off, and the switch of the layer
    // that is already on is offered as well: unchecking it turns that one layer off
    // while capture and the layer below stay exactly as the server reported them.
    expect(enableButton(wrapper).exists()).toBe(true)
    const runningToggle = wrapper.find('[data-testid="operator-retention-toggle"]')
    expect((runningToggle.element as HTMLInputElement).checked).toBe(true)
    const toggle = wrapper.find('[data-testid="operator-header-values-toggle"]')
    expect((toggle.element as HTMLInputElement).checked).toBe(false)

    await toggle.setValue(true)
    await typePhrase(wrapper, PHRASE_EN)
    await enableButton(wrapper).trigger('click')
    await flushPromises()

    // The layer this form is not touching keeps the value the server reported.
    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      body_retention_enabled: true,
      header_values_enabled: true,
      language: 'en',
      phrase: PHRASE_EN,
      plain_body_enabled: false,
      plain_header_values_enabled: false,
      plain_body_phrase: '',
      plain_header_values_phrase: '',
    })
  })

  it('explains a stored-on header value layer that is not in effect', () => {
    const noKey = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: true,
        risk_acknowledgement_current: true,
        header_values_enabled: true,
        header_values_allowed: false,
        body_encryption_key_available: false,
      }),
    })
    expect(noKey.find('[data-testid="operator-header-values-state"]').text()).toContain('Off')
    expect(noKey.find('[data-testid="operator-header-values-mismatch"]').text()).toContain(
      'no usable encryption key',
    )

    const capturePaused = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: false,
        risk_acknowledgement_current: false,
        header_values_enabled: true,
        header_values_allowed: false,
        body_encryption_key_available: true,
      }),
    })
    expect(capturePaused.find('[data-testid="operator-header-values-mismatch"]').text()).toContain(
      'capture is not running',
    )
  })

  it('keeps the header value switch unusable, and says so, without a usable key', () => {
    const wrapper = mountPanel({ status: status({ body_encryption_key_available: false }) })

    const toggle = wrapper.find('[data-testid="operator-header-values-toggle"]')
    expect(toggle.attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-testid="operator-header-values-unavailable"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="operator-retention-unavailable"]').exists()).toBe(true)
  })

  it('keeps the stored header value intent when an acknowledgement has to be re-given', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(status())
    const wrapper = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: false,
        risk_acknowledgement_current: false,
        header_values_enabled: true,
        header_values_allowed: false,
        body_encryption_key_available: true,
      }),
    })

    // Pre-set from the server's stored flag, not invented locally: re-acknowledging
    // does not silently drop a layer that is already recorded.
    expect((wrapper.find('[data-testid="operator-header-values-toggle"]').element as HTMLInputElement).checked).toBe(
      true,
    )

    await typePhrase(wrapper, PHRASE_EN)
    await enableButton(wrapper).trigger('click')
    await flushPromises()

    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      body_retention_enabled: false,
      header_values_enabled: true,
      language: 'en',
      phrase: PHRASE_EN,
      plain_body_enabled: false,
      plain_header_values_enabled: false,
      plain_body_phrase: '',
      plain_header_values_phrase: '',
    })
  })

  it('clears both retention layers when the operator disables the gate', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(status())
    const wrapper = mountPanel({
      status: status({
        enabled: true,
        capture_allowed: true,
        body_retention_enabled: true,
        body_retention_allowed: true,
        header_values_enabled: true,
        header_values_allowed: true,
      }),
    })

    await disableButton(wrapper).trigger('click')
    await flushPromises()

    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: false,
      body_retention_enabled: false,
      header_values_enabled: false,
      language: expect.any(String),
      phrase: '',
      plain_body_enabled: false,
      plain_header_values_enabled: false,
      plain_body_phrase: '',
      plain_header_values_phrase: '',
    })
  })

  // ---------------------------------------------------------------------------
  // Plaintext layers: status, re-consent, and one layer at a time
  // ---------------------------------------------------------------------------

  const plainBodyStaleAck = (overrides: Partial<ErrorDiagnosticOperatorStatus> = {}) =>
    status({
      enabled: true,
      risk_acknowledged: true,
      capture_allowed: true,
      risk_acknowledgement_current: true,
      plain_body_enabled: true,
      plain_body_allowed: false,
      plain_body_risk_acknowledgement_current: false,
      ...overrides,
    })

  it('offers re-consent for a plaintext layer that is stored as on but not running', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(status({ enabled: true, capture_allowed: true }))
    const wrapper = mountPanel({
      status: plainBodyStaleAck({ body_retention_enabled: false, body_retention_allowed: false }),
    })

    // The stored switch is on, but the layer is *not running*: its own statement is
    // no longer current. Its switch is offered, unchecked, so the layer can be
    // acknowledged again — and it is not pre-selected into a request the operator
    // never asked for.
    const toggle = wrapper.find('[data-testid="operator-plain-body-toggle"]')
    expect(toggle.exists()).toBe(true)
    expect((toggle.element as HTMLInputElement).checked).toBe(false)

    // The capture statement alone still enables the form: a stale plaintext layer
    // must not leave the button dead with no field to fill in.
    await typePhrase(wrapper, PHRASE_EN)
    expect(enableButton(wrapper).attributes('disabled')).toBeUndefined()

    // Asking for the layer needs its own statement, given verbatim.
    await toggle.setValue(true)
    expect(enableButton(wrapper).attributes('disabled')).toBeDefined()
    await wrapper.find('[data-testid="operator-plain-body-phrase-input"]').setValue(PLAIN_BODY_PHRASE_EN)

    await enableButton(wrapper).trigger('click')
    await flushPromises()

    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      body_retention_enabled: false,
      header_values_enabled: false,
      plain_body_enabled: true,
      plain_header_values_enabled: false,
      language: 'en',
      phrase: PHRASE_EN,
      plain_body_phrase: PLAIN_BODY_PHRASE_EN,
      plain_header_values_phrase: '',
    })
  })

  it('keeps a plaintext layer that is stored and current pre-selected', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(status({ enabled: true, capture_allowed: true }))
    const wrapper = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: true,
        risk_acknowledgement_current: true,
        plain_body_enabled: true,
        plain_body_allowed: true,
        plain_body_risk_acknowledgement_current: true,
      }),
    })

    // Re-giving the capture acknowledgement does not silently drop a layer that is
    // genuinely in effect: its box stays ticked, and its own statement is asked for.
    expect(
      (wrapper.find('[data-testid="operator-plain-body-toggle"]').element as HTMLInputElement).checked,
    ).toBe(true)
    await typePhrase(wrapper, PHRASE_EN)
    expect(enableButton(wrapper).attributes('disabled')).toBeDefined()
    await wrapper.find('[data-testid="operator-plain-body-phrase-input"]').setValue(PLAIN_BODY_PHRASE_EN)
    await enableButton(wrapper).trigger('click')
    await flushPromises()

    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith(
      expect.objectContaining({ enabled: true, plain_body_enabled: true, phrase: PHRASE_EN }),
    )
  })

  it('explains a refused deployment premise once, instead of blaming the plaintext statements', async () => {
    // 存量开着、两层语句都是当前版本，但数据库形态不支持：这时按层提示「语句过期/没确认」
    // 会把操作员引到一个永远开不了这一层的按钮上。部署前提必须自己说清楚。
    const wrapper = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: true,
        risk_acknowledgement_current: true,
        plain_body_enabled: true,
        plain_body_allowed: false,
        plain_body_risk_acknowledgement_current: true,
        plaintext_capture_supported: false,
        plaintext_capture_support_reason: 'unsupported_partitioned_usage_logs',
      }),
    })

    const deployment = wrapper.get('[data-testid="operator-plaintext-deployment"]')
    expect(deployment.attributes('data-state')).toBe('off')
    expect(deployment.text()).toContain('usage_logs is a partitioned table')

    const blocked = wrapper.get('[data-testid="operator-plaintext-deployment-blocked"]')
    expect(blocked.text()).toContain('Both plaintext layers are refused on this deployment')

    // 逐层提示不得出现：语句是当前的，问题不在确认。
    expect(wrapper.find('[data-testid="operator-plain-body-mismatch"]').exists()).toBe(false)

    // 旧密文层与元数据采集不受部署前提影响，仍然照常显示。
    expect(
      wrapper.get('[data-testid="operator-plain-body-retention-state"]').text(),
    ).toBe('Off')
    const plainBodyToggle = wrapper.get('[data-testid="operator-plain-body-toggle"]')
    expect((plainBodyToggle.element as HTMLInputElement).checked).toBe(false)
  })

  it('reports a probe failure as a probe failure, never as a supported deployment', async () => {
    const wrapper = mountPanel({
      status: status({ plaintext_capture_supported: false, plaintext_capture_support_reason: 'probe_failed' }),
    })

    const deployment = wrapper.get('[data-testid="operator-plaintext-deployment"]')
    expect(deployment.attributes('data-state')).toBe('off')
    expect(deployment.text()).toContain('The deployment check could not be answered right now.')
    // 数据库错误原文不进界面：原因码是闭集，未知码也只显示 unkonwn 文案，不回显原字符串。
    expect(deployment.text()).not.toContain('password')
    expect(deployment.text()).not.toContain('pq:')
  })

  it('maps an unknown deployment reason code to the closed "unknown" copy', async () => {
    const wrapper = mountPanel({
      status: status({ plaintext_capture_supported: false, plaintext_capture_support_reason: 'brand_new_code' }),
    })

    const deployment = wrapper.get('[data-testid="operator-plaintext-deployment"]')
    expect(deployment.text()).toContain('The deployment check returned a shape this build does not recognise.')
    expect(deployment.text()).not.toContain('brand_new_code')
  })

  it('never sends a ciphertext layer as on when the key it needs is gone', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(status({ enabled: true, capture_allowed: true }))
    const wrapper = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: true,
        risk_acknowledgement_current: true,
        body_retention_enabled: true,
        body_retention_allowed: false,
        body_encryption_key_available: false,
      }),
    })

    // The stored flag is still on, but the server refuses to be asked for retention
    // with no usable key: a request that echoed it back would be rejected outright
    // and take an unrelated change (a plaintext layer) down with it.
    await typePhrase(wrapper, PHRASE_EN)
    await wrapper.find('[data-testid="operator-plain-body-toggle"]').setValue(true)
    await wrapper.find('[data-testid="operator-plain-body-phrase-input"]').setValue(PLAIN_BODY_PHRASE_EN)
    await enableButton(wrapper).trigger('click')
    await flushPromises()

    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith(
      expect.objectContaining({ body_retention_enabled: false, plain_body_enabled: true }),
    )
  })

  it('reports the plaintext layers next to the encrypted ones', () => {
    const wrapper = mountPanel({
      status: status({
        enabled: true,
        capture_allowed: true,
        body_retention_enabled: false,
        body_retention_allowed: false,
        plain_body_enabled: true,
        plain_body_allowed: true,
        plain_header_values_enabled: false,
        plain_header_values_allowed: false,
      }),
    })

    // "Effective encrypted request body retention: Off" must not be the whole
    // answer while the server is writing plaintext request bodies.
    expect(wrapper.find('[data-testid="operator-body-retention-state"]').text()).toContain('Off')
    expect(wrapper.find('[data-testid="operator-plain-body-retention-state"]').text()).toContain('On')
    expect(wrapper.find('[data-testid="operator-plain-header-values-state"]').text()).toContain('Off')

    // The two encrypted rows are labelled as encrypted, so the plaintext rows are
    // not read as a second copy of the same fact.
    const labels = wrapper.findAll('dt').map((node) => node.text())
    expect(labels).toContain('Effective encrypted request body retention')
    expect(labels).toContain('Effective plaintext request body retention')
  })

  it('explains a stored-on plaintext layer that is not running, and why', () => {
    const staleAck = mountPanel({
      status: plainBodyStaleAck({
        plain_body_risk_acknowledgement: {
          version: 'v2026.01.01',
          phrase: PLAIN_BODY_PHRASE_EN,
          admin_user_id: 7,
          accepted_at: '2026-01-01T00:00:00Z',
        },
      }),
    })
    expect(staleAck.find('[data-testid="operator-plain-body-retention-state"]').text()).toContain('Off')
    expect(staleAck.find('[data-testid="operator-plain-body-mismatch"]').text()).toContain(
      'does not cover the current plaintext statement',
    )

    const noAck = mountPanel({
      status: plainBodyStaleAck({ plain_body_risk_acknowledgement_current: false }),
    })
    expect(noAck.find('[data-testid="operator-plain-body-mismatch"]').text()).toContain(
      'no written acknowledgement of its own statement',
    )

    const capturePaused = mountPanel({
      status: plainBodyStaleAck({ capture_allowed: false }),
    })
    expect(capturePaused.find('[data-testid="operator-plain-body-mismatch"]').text()).toContain(
      'capture is not running',
    )
  })

  it('shows each statement, and each acknowledgement, on its own', () => {
    const wrapper = mountPanel({
      status: plainBodyStaleAck({
        risk_acknowledgement: {
          version: 'v2026.01.01',
          phrase: PHRASE_EN,
          admin_user_id: 7,
          accepted_at: '2026-01-01T00:00:00Z',
        },
        plain_body_risk_acknowledgement: {
          version: 'v2026.09.27.1',
          phrase: PLAIN_BODY_PHRASE_EN,
          admin_user_id: 7,
          accepted_at: '2026-09-27T00:00:00Z',
        },
        plain_header_values_enabled: false,
      }),
    })

    // The capture statement and the plaintext statement are separate records, and
    // the panel names which one is which instead of collapsing them into one.
    expect(wrapper.find('[data-testid="operator-ack-version"]').text()).toContain('v2026.01.01')
    expect(wrapper.find('[data-testid="operator-plain-body-ack"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="operator-plain-body-ack-version"]').text()).toContain('v2026.09.27.1')
    expect(wrapper.find('[data-testid="operator-plain-body-ack-operator"]').text()).toContain('7')
    expect(wrapper.find('[data-testid="operator-plain-body-ack-stale"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="operator-plain-header-ack-none"]').exists()).toBe(true)
  })

  it('turns one layer off without stopping capture or the other layers', async () => {
    mocks.updateOperatorSettings.mockResolvedValue(status({ enabled: true, capture_allowed: true }))
    const wrapper = mountPanel({
      status: status({
        enabled: true,
        risk_acknowledged: true,
        capture_allowed: true,
        risk_acknowledgement_current: true,
        body_retention_enabled: true,
        body_retention_allowed: true,
        header_values_enabled: true,
        header_values_allowed: true,
        plain_body_enabled: true,
        plain_body_allowed: true,
        plain_body_risk_acknowledgement_current: true,
        plain_header_values_enabled: true,
        plain_header_values_allowed: true,
        plain_header_risk_acknowledgement_current: true,
      }),
    })

    // Everything that is on stays on, so the form is still offered: it is the only
    // place a single layer can be turned off.
    expect(enableButton(wrapper).text()).toBe('Apply layer changes')
    expect(wrapper.find('[data-testid="operator-layers-notice"]').exists()).toBe(true)

    await wrapper.find('[data-testid="operator-plain-body-toggle"]').setValue(false)
    await typePhrase(wrapper, PHRASE_EN)
    await wrapper
      .find('[data-testid="operator-plain-header-phrase-input"]')
      .setValue(PLAIN_HEADER_PHRASE_EN)
    await enableButton(wrapper).trigger('click')
    await flushPromises()

    // Capture, the metadata and the other layers are untouched: only the plaintext
    // body layer is asked off.
    expect(mocks.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      body_retention_enabled: true,
      header_values_enabled: true,
      plain_body_enabled: false,
      plain_header_values_enabled: true,
      language: 'en',
      phrase: PHRASE_EN,
      plain_body_phrase: '',
      plain_header_values_phrase: PLAIN_HEADER_PHRASE_EN,
    })
  })
})
