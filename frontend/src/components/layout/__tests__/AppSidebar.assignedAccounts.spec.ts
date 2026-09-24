import { beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'

import AppSidebar from '../AppSidebar.vue'

const stores = vi.hoisted(() => ({
  app: null as null | Record<string, any>,
  auth: null as null | Record<string, any>,
  adminSettings: null as null | Record<string, any>,
  onboarding: null as null | Record<string, any>
}))

vi.mock('@/stores/app', async () => {
  const { reactive } = await import('vue')

  stores.app = reactive({
    sidebarCollapsed: false,
    mobileOpen: false,
    siteName: 'Sub2API',
    siteLogo: '',
    siteVersion: '1.0.0',
    publicSettingsLoaded: true,
    cachedPublicSettings: { custom_menu_items: [], payment_enabled: false },
    backendModeEnabled: false,
    sidebarScrollTop: 0,
    toggleSidebar: vi.fn(),
    setMobileOpen: vi.fn()
  })

  return { useAppStore: () => stores.app }
})

vi.mock('@/stores/auth', async () => {
  const { reactive } = await import('vue')

  stores.auth = reactive({
    isAdmin: false,
    isSimpleMode: false,
    canViewAssignedAccounts: true
  })

  return { useAuthStore: () => stores.auth }
})

vi.mock('@/stores/adminSettings', async () => {
  const { reactive } = await import('vue')

  stores.adminSettings = reactive({
    customMenuItems: [],
    opsMonitoringEnabled: false,
    paymentEnabled: false,
    fetch: vi.fn()
  })

  return { useAdminSettingsStore: () => stores.adminSettings }
})

vi.mock('@/stores/onboarding', async () => {
  const { reactive } = await import('vue')

  stores.onboarding = reactive({
    isCurrentStep: vi.fn(() => false),
    nextStep: vi.fn()
  })

  return { useOnboardingStore: () => stores.onboarding }
})

// The barrel re-exports the same single store instances the components use.
vi.mock('@/stores', async () => {
  const app = await import('@/stores/app')
  const auth = await import('@/stores/auth')
  const adminSettings = await import('@/stores/adminSettings')
  const onboarding = await import('@/stores/onboarding')

  return {
    useAppStore: app.useAppStore,
    useAuthStore: auth.useAuthStore,
    useAdminSettingsStore: adminSettings.useAdminSettingsStore,
    useOnboardingStore: onboarding.useOnboardingStore
  }
})

vi.mock('@/composables/useBatchImageAccess', () => ({
  useBatchImageAccess: () => ({
    canUseBatchImage: { value: false },
    refreshBatchImageAccess: vi.fn()
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

function createTestRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/dashboard', component: { template: '<div />' } },
      { path: '/accounts', component: { template: '<div />' } },
      { path: '/admin/accounts', component: { template: '<div />' } },
      { path: '/:pathMatch(.*)*', component: { template: '<div />' } }
    ]
  })
}

async function mountSidebar() {
  const router = createTestRouter()
  await router.push('/dashboard')
  await router.isReady()

  const wrapper = mount(AppSidebar, {
    global: {
      plugins: [router],
      stubs: {
        VersionBadge: true
      }
    }
  })
  await flushPromises()
  return wrapper
}

describe('AppSidebar read-only account entry', () => {
  beforeEach(() => {
    stores.auth!.isAdmin = false
    stores.auth!.isSimpleMode = false
    stores.auth!.canViewAssignedAccounts = true
    stores.app!.backendModeEnabled = false
  })

  it('shows the account menu only for a granted regular user', async () => {
    const wrapper = await mountSidebar()

    const link = wrapper.find('a[href="/accounts"]')
    expect(link.exists()).toBe(true)
    expect(link.text()).toBe('nav.assignedAccounts')

    wrapper.unmount()
  })

  it('hides the menu entry when the capability is revoked', async () => {
    const wrapper = await mountSidebar()
    expect(wrapper.find('a[href="/accounts"]').exists()).toBe(true)

    // 授权被撤销后，刷新到的用户资料让入口立即消失。
    stores.auth!.canViewAssignedAccounts = false
    await nextTick()

    expect(wrapper.find('a[href="/accounts"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('never exposes the entry to administrators, who keep the admin account page', async () => {
    stores.auth!.isAdmin = true
    stores.auth!.canViewAssignedAccounts = false

    const wrapper = await mountSidebar()

    expect(wrapper.find('a[href="/accounts"]').exists()).toBe(false)
    expect(wrapper.find('a[href="/admin/accounts"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('keeps the regular-user menu untouched in backend mode', async () => {
    stores.app!.backendModeEnabled = true

    const wrapper = await mountSidebar()

    expect(wrapper.find('a[href="/accounts"]').exists()).toBe(false)
    expect(wrapper.find('a[href="/keys"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('keeps the existing user menu entries alongside the account view', async () => {
    const wrapper = await mountSidebar()

    expect(wrapper.find('a[href="/keys"]').exists()).toBe(true)
    expect(wrapper.find('a[href="/usage"]').exists()).toBe(true)
    wrapper.unmount()
  })
})
