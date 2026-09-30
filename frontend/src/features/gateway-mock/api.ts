import { apiClient } from '@/api/client'
import {
  normalizeGatewayMockEventPage,
  normalizeGatewayMockOperatorStatus,
  normalizeGatewayMockPresetSeed,
  type GatewayMockEventListParams,
  type GatewayMockEventPage,
  type GatewayMockOperatorStatus,
  type GatewayMockOperatorUpdateInput,
  type GatewayMockPresetSeed,
} from './types'

const path = '/admin/settings/gateway-mock'
const eventsPath = '/admin/settings/gateway-mock/events'
const headers = { 'Cache-Control': 'no-store', Pragma: 'no-cache' }

/**
 * Reads the whole rule set plus the global switch. The switch is off unless it
 * was turned on: an absent or unreadable setting is not an enabled one.
 */
export async function getOperatorSettings(): Promise<GatewayMockOperatorStatus> {
  const { data } = await apiClient.get<unknown>(path, { headers })
  return normalizeGatewayMockOperatorStatus(data)
}

/**
 * Replaces the whole rule set and the switch in one call — the API has
 * replace-set semantics, so what is sent here is the complete new set and not a
 * patch. Validation stays on the server; the bounded reason it answers with is
 * what the settings form shows.
 */
export async function updateOperatorSettings(input: GatewayMockOperatorUpdateInput): Promise<GatewayMockOperatorStatus> {
  const { data } = await apiClient.put<unknown>(path, {
    enabled: input.enabled,
    rules: input.rules.map(rule => ({
      id: rule.id,
      keyword: rule.keyword,
      reply: rule.reply,
      enabled: rule.enabled,
    })),
  }, { headers })
  return normalizeGatewayMockOperatorStatus(data)
}

/**
 * Seeds the built-in preset keywords into an empty set. They arrive disabled:
 * seeding never turns the feature on, and a non-empty set is left untouched.
 */
export async function seedPresets(): Promise<GatewayMockPresetSeed> {
  const { data } = await apiClient.post<unknown>(`${path}/presets`, null, { headers })
  return normalizeGatewayMockPresetSeed(data)
}

/**
 * Reads one page of minimal hit events, newest first. The route answers with
 * metadata only, so there is no content filter and no search term to pass.
 */
export async function listEvents(
  params: GatewayMockEventListParams,
  options?: { signal?: AbortSignal },
): Promise<GatewayMockEventPage> {
  const { data } = await apiClient.get<unknown>(eventsPath, { params, headers, signal: options?.signal })
  return normalizeGatewayMockEventPage(data)
}
