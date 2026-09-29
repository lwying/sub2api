import { flushPromises, shallowMount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  getRequestAudit: vi.fn(),
  push: vi.fn(),
}))

vi.mock('@/api/admin/usage', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/admin/usage')>()
  return { ...actual, getRequestAudit: mocks.getRequestAudit }
})
vi.mock('vue-router', () => ({ useRouter: () => ({ push: mocks.push }) }))
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import UsageRequestAuditDrawer from '../UsageRequestAuditDrawer.vue'

/**
 * Navigation from a usage row to its request Trace. The Trace page keeps the
 * session-only detail gate, so this link only forwards the usage log id; it must
 * not read or embed any Trace body.
 */
describe('UsageRequestAuditDrawer Trace navigation', () => {
  beforeEach(() => {
    mocks.getRequestAudit.mockReset()
    mocks.push.mockReset()
    mocks.getRequestAudit.mockResolvedValue({
      usage_log_id: 4242, headers: {}, events: [], attempts: [], capture_completeness: 'complete',
    })
  })

  function mountDrawer(usageLogId: number) {
    return shallowMount(UsageRequestAuditDrawer, {
      props: { show: true, usageLogId },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' }, Icon: true } },
    })
  }

  it('offers a session-only jump to the Trace list filtered by this usage record', async () => {
    const wrapper = mountDrawer(4242)
    await flushPromises()
    await wrapper.get('[data-testid="usage-open-trace"]').trigger('click')
    expect(mocks.push).toHaveBeenCalledTimes(1)
    expect(mocks.push).toHaveBeenCalledWith({ path: '/admin/request-traces', query: { usage_log_id: '4242' } })
  })

  it('does not offer the jump without a usable usage id', async () => {
    const wrapper = mountDrawer(0)
    await flushPromises()
    expect(wrapper.find('[data-testid="usage-open-trace"]').exists()).toBe(false)
  })
})
