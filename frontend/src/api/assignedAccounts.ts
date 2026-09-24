/**
 * Dedicated read-only API for accounts an administrator explicitly assigned to
 * the current (non-admin) user.
 *
 * These endpoints are intentionally separate from the admin account APIs: the
 * server returns an allowlist of already-masked upstream identity fields and
 * enforces the per-user assignment on every list and detail read. The client
 * never computes a mask and never expects credentials, proxy data, notes or
 * scheduler state here.
 */

import { apiClient } from './client'
import type { PaginatedResponse } from '@/types'

/**
 * One assigned account as the customer is allowed to see it.
 *
 * Identity fields are masked server-side; an unavailable identity is returned
 * as an empty string (never as a raw account name or credential).
 */
export interface AssignedAccount {
  id: number
  platform: string
  account_type: string
  email_masked: string
  username_masked: string
  upstream_account_id_masked: string
}

/**
 * List the accounts currently assigned to the signed-in user.
 * Disabled and revoked accounts are absent from `items` and `total` alike.
 */
export async function list(
  page: number = 1,
  pageSize: number = 20,
  options?: { signal?: AbortSignal }
): Promise<PaginatedResponse<AssignedAccount>> {
  const { data } = await apiClient.get<PaginatedResponse<AssignedAccount>>('/accounts', {
    params: { page, page_size: pageSize },
    signal: options?.signal
  })
  return data
}

/**
 * Read a single assigned account.
 *
 * The server answers 404 for unassigned, disabled or deleted accounts, so a
 * guessed id cannot be distinguished from a nonexistent one.
 */
export async function getById(
  id: number,
  options?: { signal?: AbortSignal }
): Promise<AssignedAccount> {
  const { data } = await apiClient.get<AssignedAccount>(`/accounts/${id}`, {
    signal: options?.signal
  })
  return data
}

export const assignedAccountsAPI = {
  list,
  getById
}

/**
 * True when the backend refused the read-only account view itself (capability
 * revoked or never granted), as opposed to a transient failure or a 404 for a
 * single account.
 */
export function isAssignedAccountsAccessDenied(error: unknown): boolean {
  const candidate = error as { status?: number; code?: string | number } | null | undefined
  if (!candidate) {
    return false
  }

  return candidate.status === 403 || candidate.code === 'ACCOUNT_VIEW_DISABLED'
}

/**
 * True when a single account is not visible to this user. The server answers
 * the same 404 for unassigned, disabled, deleted and nonexistent accounts, so
 * callers must not try to distinguish those cases.
 *
 * Anything else (network failure, 5xx) is a real error and must not be shown as
 * "not visible".
 */
export function isAssignedAccountNotFound(error: unknown): boolean {
  const candidate = error as { status?: number } | null | undefined
  return candidate?.status === 404
}

export default assignedAccountsAPI
