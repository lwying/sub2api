/**
 * Access rules for the customer-facing read-only assigned-account view.
 *
 * The view is a regular-user route: it never reuses the admin account page and
 * it is not a new role. `resolveAssignedAccountsRedirect` covers the checks the
 * router can make from local state; the caller then confirms the capability
 * with the server before rendering, because a cached profile is not proof of a
 * grant that an administrator may already have revoked.
 */

export const ASSIGNED_ACCOUNTS_PATH = '/accounts'

/** Regular users land here when the capability is not (or no longer) granted. */
export const ASSIGNED_ACCOUNTS_FALLBACK_PATH = '/dashboard'

/** Administrators keep using the admin account page for account management. */
export const ASSIGNED_ACCOUNTS_ADMIN_PATH = '/admin/accounts'

export interface AssignedAccountsAccessState {
  isAuthenticated: boolean
  isAdmin: boolean
}

/**
 * Local-state portion of the guard. Returns the redirect target when the route
 * must not be entered, or `null` when the caller still has to confirm the
 * capability with the server.
 */
export function resolveAssignedAccountsRedirect(
  state: AssignedAccountsAccessState
): string | null {
  if (!state.isAuthenticated) {
    return '/login'
  }

  if (state.isAdmin) {
    return ASSIGNED_ACCOUNTS_ADMIN_PATH
  }

  return null
}
