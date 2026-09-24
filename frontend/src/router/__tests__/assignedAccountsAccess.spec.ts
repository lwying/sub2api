import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import { resolveAssignedAccountsRedirect } from '../assignedAccountsAccess'

type NavigationGuard = (
  to: Record<string, any>,
  from: Record<string, any>,
  next: ReturnType<typeof vi.fn>
) => Promise<void>

type RouteRecord = {
  path: string
  name?: string
  component?: unknown
  meta?: Record<string, unknown>
}

const routerHarness = vi.hoisted(() => ({
  guard: null as NavigationGuard | null,
  routes: [] as RouteRecord[],
}))

const authStore = vi.hoisted(() => ({
  checkAuth: vi.fn(),
  isAuthenticated: true,
  isAdmin: false,
  isSimpleMode: false,
  hasPendingAuthSession: false,
  verifyAssignedAccountAccess: vi.fn<() => Promise<boolean>>(),
}))

const appStore = vi.hoisted(() => ({
  siteName: 'Sub2API',
  backendModeEnabled: false,
  publicSettingsLoaded: false,
  cachedPublicSettings: null as null | Record<string, unknown>,
  fetchPublicSettings: vi.fn(),
}))

vi.mock('vue-router', () => ({
  createWebHistory: vi.fn(() => ({})),
  createRouter: vi.fn((options: { routes: RouteRecord[] }) => {
    routerHarness.routes = options.routes
    return {
      beforeEach: vi.fn((guard: NavigationGuard) => {
        routerHarness.guard = guard
      }),
      afterEach: vi.fn(),
      onError: vi.fn(),
    }
  }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => authStore,
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => appStore,
}))

vi.mock('@/stores/adminSettings', () => ({
  useAdminSettingsStore: () => ({ customMenuItems: [] }),
}))

vi.mock('@/stores/adminCompliance', () => ({
  useAdminComplianceStore: () => ({
    initialized: true,
    fetchStatus: vi.fn(),
    requireAcknowledgement: vi.fn(),
  }),
}))

vi.mock('@/composables/useNavigationLoading', () => ({
  useNavigationLoadingState: () => ({
    startNavigation: vi.fn(),
    endNavigation: vi.fn(),
    isLoading: { value: false },
  }),
}))

vi.mock('@/composables/useRoutePrefetch', () => ({
  useRoutePrefetch: () => ({
    triggerPrefetch: vi.fn(),
    cancelPendingPrefetch: vi.fn(),
    resetPrefetchState: vi.fn(),
  }),
}))

vi.mock('@/api/setup', () => ({
  getSetupStatus: vi.fn(),
}))

function runGuard(path: string, meta: Record<string, unknown>) {
  if (!routerHarness.guard) {
    throw new Error('router guard was not registered')
  }

  const next = vi.fn()
  const navigation = routerHarness.guard(
    { path, fullPath: path, name: 'AssignedAccounts', params: {}, meta: { requiresAuth: true, ...meta } },
    {},
    next
  )
  return { navigation, next }
}

describe('assigned accounts access rules', () => {
  it('sends anonymous visitors to login', () => {
    expect(
      resolveAssignedAccountsRedirect({ isAuthenticated: false, isAdmin: false })
    ).toBe('/login')
  })

  it('keeps administrators on the admin account page', () => {
    expect(
      resolveAssignedAccountsRedirect({ isAuthenticated: true, isAdmin: true })
    ).toBe('/admin/accounts')
  })

  it('leaves the capability decision to the server for regular users', () => {
    expect(
      resolveAssignedAccountsRedirect({ isAuthenticated: true, isAdmin: false })
    ).toBeNull()
  })
})

describe('read-only account view route wiring', () => {
  beforeAll(async () => {
    await import('@/router')
  })

  it('registers an independent regular-user route without admin requirements', () => {
    const route = routerHarness.routes.find((candidate) => candidate.path === '/accounts')

    expect(route).toBeDefined()
    expect(route?.name).toBe('AssignedAccounts')
    expect(route?.meta?.requiresAuth).toBe(true)
    expect(route?.meta?.requiresAdmin).toBe(false)
    expect(route?.meta?.requiresAssignedAccounts).toBe(true)
  })

  it('never reuses the admin account page component', () => {
    const userRoute = routerHarness.routes.find((candidate) => candidate.path === '/accounts')
    const adminRoute = routerHarness.routes.find((candidate) => candidate.path === '/admin/accounts')

    expect(userRoute?.component).toBeDefined()
    expect(adminRoute?.component).toBeDefined()
    expect(userRoute?.component).not.toBe(adminRoute?.component)
  })
})

describe('read-only account view guard', () => {
  beforeAll(async () => {
    await import('@/router')
  })

  beforeEach(() => {
    authStore.isAuthenticated = true
    authStore.isAdmin = false
    authStore.isSimpleMode = false
    authStore.verifyAssignedAccountAccess.mockReset()
    appStore.backendModeEnabled = false
    appStore.publicSettingsLoaded = false
    appStore.cachedPublicSettings = null
  })

  it('redirects anonymous visitors to login', async () => {
    authStore.isAuthenticated = false

    const { navigation, next } = runGuard('/accounts', { requiresAssignedAccounts: true })
    await navigation

    expect(next).toHaveBeenCalledWith({ path: '/login', query: { redirect: '/accounts' } })
    expect(authStore.verifyAssignedAccountAccess).not.toHaveBeenCalled()
  })

  it('redirects administrators to the admin account page without probing', async () => {
    authStore.isAdmin = true

    const { navigation, next } = runGuard('/accounts', { requiresAssignedAccounts: true })
    await navigation

    expect(next).toHaveBeenCalledWith('/admin/accounts')
    expect(authStore.verifyAssignedAccountAccess).not.toHaveBeenCalled()
  })

  it('rechecks the grant on the server before entering and allows a granted user', async () => {
    authStore.verifyAssignedAccountAccess.mockResolvedValue(true)

    const { navigation, next } = runGuard('/accounts', { requiresAssignedAccounts: true })
    await navigation

    expect(authStore.verifyAssignedAccountAccess).toHaveBeenCalledTimes(1)
    expect(next).toHaveBeenCalledWith()
  })

  it('denies a user whose grant was revoked even when the cached menu flag is stale', async () => {
    // A stale localStorage profile still claims the capability; the server says no.
    authStore.verifyAssignedAccountAccess.mockResolvedValue(false)

    const { navigation, next } = runGuard('/accounts', { requiresAssignedAccounts: true })
    await navigation

    expect(next).toHaveBeenCalledWith('/dashboard')
  })

  it('waits for the server answer instead of optimistically entering the page', async () => {
    let resolveVerification!: (allowed: boolean) => void
    authStore.verifyAssignedAccountAccess.mockImplementation(
      () => new Promise<boolean>((resolve) => { resolveVerification = resolve })
    )

    const { navigation, next } = runGuard('/accounts', { requiresAssignedAccounts: true })
    await Promise.resolve()

    expect(next).not.toHaveBeenCalled()

    resolveVerification(true)
    await navigation

    expect(next).toHaveBeenCalledWith()
  })

  it('does not probe routes that do not require the capability', async () => {
    const { navigation, next } = runGuard('/keys', {})
    await navigation

    expect(authStore.verifyAssignedAccountAccess).not.toHaveBeenCalled()
    expect(next).toHaveBeenCalledWith()
  })

  it('leaves backend mode to the existing non-admin block', async () => {
    appStore.backendModeEnabled = true

    const { navigation, next } = runGuard('/accounts', { requiresAssignedAccounts: true })
    await navigation

    expect(authStore.verifyAssignedAccountAccess).not.toHaveBeenCalled()
    expect(next).toHaveBeenCalledWith('/login')
  })
})
