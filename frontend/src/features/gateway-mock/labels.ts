import { gatewayMockProtocols, type GatewayMockProtocol } from './types'

/**
 * The one key a protocol renders through: its own label, or the generic fallback
 * when the value is outside the set this build knows about. The raw wire value is
 * never the visible text.
 */
export function gatewayMockProtocolKey(protocol: string): string {
  const known = (gatewayMockProtocols as readonly string[]).includes(protocol)
  return `admin.gatewayMock.events.protocol.${known ? (protocol as GatewayMockProtocol) : 'unknown'}`
}

/**
 * The bounded reasons the mock endpoints answer with, each with its own message.
 * A reason outside this map is not guessed at: the form says the change failed
 * and that the set on the server is therefore unknown, rather than echoing a
 * server token or claiming the save went through.
 */
const gatewayMockFailureKeys: Record<string, string> = {
  GATEWAY_MOCK_SETTINGS_UNAVAILABLE: 'admin.gatewayMock.failures.settingsUnavailable',
  GATEWAY_MOCK_RULE_INVALID: 'admin.gatewayMock.failures.ruleInvalid',
  GATEWAY_MOCK_RULE_KEYWORD_EMPTY: 'admin.gatewayMock.failures.keywordEmpty',
  GATEWAY_MOCK_RULE_KEYWORD_TOO_LONG: 'admin.gatewayMock.failures.keywordTooLong',
  GATEWAY_MOCK_RULE_REPLY_EMPTY: 'admin.gatewayMock.failures.replyEmpty',
  GATEWAY_MOCK_RULE_REPLY_TOO_LONG: 'admin.gatewayMock.failures.replyTooLong',
  GATEWAY_MOCK_RULE_KEYWORD_TAKEN: 'admin.gatewayMock.failures.keywordTaken',
  GATEWAY_MOCK_RULE_NOT_FOUND: 'admin.gatewayMock.failures.ruleNotFound',
  GATEWAY_MOCK_TOO_MANY_RULES: 'admin.gatewayMock.failures.tooManyRules',
  GATEWAY_MOCK_ADMIN_SESSION_REQUIRED: 'admin.gatewayMock.failures.sessionRequired',
}

/** Every bounded reason a form has its own message for; the generic one is the fallback. */
export const gatewayMockFailureReasons = Object.keys(gatewayMockFailureKeys)

/**
 * Maps a failed call to its message key. The API client carries the bounded
 * reason in `reason` and the transport code in `code`; both are consulted, and
 * anything unmapped falls back to the generic failure.
 */
export function gatewayMockFailureKey(error: unknown): string {
  const source = (error ?? {}) as { reason?: unknown; code?: unknown }
  const reason = typeof source.reason === 'string' ? source.reason : ''
  const code = typeof source.code === 'string' ? source.code : ''
  return gatewayMockFailureKeys[reason] ?? gatewayMockFailureKeys[code] ?? 'admin.gatewayMock.failures.generic'
}
