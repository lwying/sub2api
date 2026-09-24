/**
 * Admin error-request diagnostics API (tickets 01-04).
 *
 * Reads are metadata-only. The stored request body is fetched exclusively by
 * `revealDiagnosticBody`, which is a POST on purpose:
 *
 *   - a GET could be issued by a prefetcher, a speculator or a cache before the
 *     admin ever asked for it, and its URL would end up in history;
 *   - the request carries `Cache-Control: no-store` / `Pragma: no-cache` so an
 *     intermediary does not keep a copy even if it ignores the response header.
 *
 * The endpoint path is stable, but the client never trusts its payload: every
 * response goes through the normalizers in ./types so an unexpected extra field
 * (body, credential, model alias) cannot travel further into the UI.
 */
import { apiClient } from '@/api/client'
import {
  normalizeDiagnosticAttempt,
  normalizeDiagnosticAttemptPage,
  normalizeDiagnosticBodyReveal,
  normalizeErrorDiagnosticOperatorStatus,
  type DiagnosticAttempt,
  type DiagnosticAttemptPage,
  type DiagnosticBodyReveal,
  type ErrorDiagnosticOperatorStatus,
  type ErrorDiagnosticOperatorUpdateInput,
} from './types'

const basePath = '/admin/error-diagnostics'
const operatorSettingsPath = '/admin/settings/error-diagnostic'

const NO_STORE_HEADERS = {
  'Cache-Control': 'no-store',
  Pragma: 'no-cache',
} as const

export interface DiagnosticListParams {
  page: number
  page_size: number
}

export async function listDiagnostics(
  params: DiagnosticListParams,
  options?: { signal?: AbortSignal },
): Promise<DiagnosticAttemptPage> {
  const { data } = await apiClient.get<unknown>(basePath, {
    params: { page: params.page, page_size: params.page_size },
    headers: NO_STORE_HEADERS,
    signal: options?.signal,
  })
  return normalizeDiagnosticAttemptPage(data)
}

export async function getDiagnostic(
  id: string,
  options?: { signal?: AbortSignal },
): Promise<DiagnosticAttempt> {
  const { data } = await apiClient.get<unknown>(`${basePath}/${encodeURIComponent(id)}`, {
    headers: NO_STORE_HEADERS,
    signal: options?.signal,
  })
  return normalizeDiagnosticAttempt(data)
}

/**
 * Decrypts and returns the stored request body for one attempt. Only call this
 * from an explicit admin action: the plaintext must never be fetched, cached or
 * rendered as markup on its own.
 */
export async function revealDiagnosticBody(id: string): Promise<DiagnosticBodyReveal> {
  const { data } = await apiClient.post<unknown>(
    `${basePath}/${encodeURIComponent(id)}/body`,
    undefined,
    { headers: NO_STORE_HEADERS },
  )
  return normalizeDiagnosticBodyReveal(data)
}

/**
 * Reads the capture gate. Never cached: a stale "enabled" would tell an operator
 * that failures are being recorded when they are not, and a stale "disabled" would
 * invite a pointless re-acknowledgement.
 */
export async function getOperatorSettings(options?: {
  signal?: AbortSignal
}): Promise<ErrorDiagnosticOperatorStatus> {
  const { data } = await apiClient.get<unknown>(operatorSettingsPath, {
    headers: NO_STORE_HEADERS,
    signal: options?.signal,
  })
  return normalizeErrorDiagnosticOperatorStatus(data)
}

/**
 * Applies one whole-state update to the gate.
 *
 * The payload is built explicitly from the four agreed fields: enabling is bound
 * to the typed acknowledgement the operator just gave, so no extra field (an
 * identity, a source address, a key) may be attached here. Disabling is expressed
 * the same way with `phrase: ''` — the server requires no statement for it.
 */
export async function updateOperatorSettings(
  input: ErrorDiagnosticOperatorUpdateInput,
): Promise<ErrorDiagnosticOperatorStatus> {
  const { data } = await apiClient.put<unknown>(
    operatorSettingsPath,
    {
      enabled: input.enabled,
      body_retention_enabled: input.body_retention_enabled,
      language: input.language,
      phrase: input.phrase,
    },
    { headers: NO_STORE_HEADERS },
  )
  return normalizeErrorDiagnosticOperatorStatus(data)
}

export const errorDiagnosticsAPI = {
  listDiagnostics,
  getDiagnostic,
  revealDiagnosticBody,
  getOperatorSettings,
  updateOperatorSettings,
}

export default errorDiagnosticsAPI
