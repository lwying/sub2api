import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import RequestTraceSettingsDialog from '../RequestTraceSettingsDialog.vue'

const api = vi.hoisted(() => ({ getOperatorSettings: vi.fn() }))
vi.mock('../api', () => ({ getOperatorSettings: api.getOperatorSettings }))
vi.mock('vue-i18n', async original => {
  const actual = await original<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

describe('Trace settings on the Trace page', () => {
  it('loads capture state when opened and keeps capture/export confirmations separate', async () => {
    api.getOperatorSettings.mockResolvedValue({ enabled: false, capture_allowed: false })
    const wrapper = mount(RequestTraceSettingsDialog, {
      props: { show: false },
      global: { stubs: {
        BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /></div>' },
        RequestTraceOperatorSettings: { props: ['status'], template: '<section data-testid="capture-settings">{{ status?.enabled }}</section>' },
        RequestTraceExportSettings: { template: '<section data-testid="export-settings">export</section>' },
      } },
    })
    expect(api.getOperatorSettings).not.toHaveBeenCalled()
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(api.getOperatorSettings).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-testid="capture-settings"]').text()).toBe('false')
    expect(wrapper.get('[data-testid="export-settings"]').exists()).toBe(true)
  })
})
