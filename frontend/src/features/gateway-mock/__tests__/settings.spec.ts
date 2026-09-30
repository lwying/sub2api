/**
 * The rule editor: a default-off switch, a whole-set save, presets for the empty
 * state, and server-side validation reported as the bounded reason the server
 * answered with — the form never decides on its own that a rule is acceptable.
 *
 * The stored status belongs to the parent, so a test that wants to see the
 * editor after a save feeds the emitted status back in, exactly as the view does.
 */
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({ updateOperatorSettings: vi.fn(), seedPresets: vi.fn() }))
vi.mock('../api', () => api)
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import GatewayMockSettings from '../GatewayMockSettings.vue'
import type { GatewayMockOperatorStatus } from '../types'

const rule = (patch: Partial<GatewayMockOperatorStatus['rules'][number]> = {}) => ({
  id: 'gmr_1',
  keyword: 'hi',
  normalized_keyword: 'hi',
  reply: 'Hello!',
  enabled: true,
  updated_at: '2026-09-30T03:00:00Z',
  ...patch,
})

const status = (patch: Partial<GatewayMockOperatorStatus> = {}): GatewayMockOperatorStatus => ({
  enabled: false,
  rules: [],
  preset_available: true,
  preset_created: 0,
  ...patch,
})

function mountSettings(current: GatewayMockOperatorStatus | null = status(), loading = false) {
  return mount(GatewayMockSettings, { props: { status: current, loading } })
}

async function mountWithRules(rules: ReturnType<typeof rule>[], enabled = true) {
  const wrapper = mountSettings(status({ enabled, rules }))
  await flushPromises()
  return wrapper
}

/** The view hands the answer back to the editor, so the tests do too. */
async function adoptEmitted(wrapper: ReturnType<typeof mountSettings>) {
  const emitted = wrapper.emitted('updated')?.[0]?.[0] as GatewayMockOperatorStatus | undefined
  expect(emitted).toBeTruthy()
  await wrapper.setProps({ status: emitted })
  await flushPromises()
}

describe('gateway mock rule editor', () => {
  beforeEach(() => {
    api.updateOperatorSettings.mockReset()
    api.seedPresets.mockReset()
  })

  it('shows the loading and unavailable states instead of an empty, off rule set', () => {
    const loadingWrapper = mountSettings(null, true)
    expect(loadingWrapper.get('[data-testid="gateway-mock-loading"]').text()).toBe('admin.gatewayMock.loading')
    expect(loadingWrapper.find('[data-testid="gateway-mock-state"]').exists()).toBe(false)

    const unavailable = mountSettings(null, false)
    expect(unavailable.get('[data-testid="gateway-mock-unavailable"]').text()).toBe('admin.gatewayMock.unavailable')
    // 不可读时不能渲染成"关闭且没有规则"。
    expect(unavailable.find('[data-testid="gateway-mock-switch"]').exists()).toBe(false)
  })

  it('states the stored switch truthfully and keeps the draft switch separate from it', async () => {
    const wrapper = await mountWithRules([rule()])
    expect(wrapper.get('[data-testid="gateway-mock-state"]').attributes('data-state')).toBe('on')
    expect((wrapper.get('[data-testid="gateway-mock-switch"]').element as HTMLInputElement).checked).toBe(true)
    expect(wrapper.find('[data-testid="gateway-mock-unsaved"]').exists()).toBe(false)

    await wrapper.get('[data-testid="gateway-mock-switch"]').setValue(false)
    // 存下的状态仍然是服务端上一次答复的那一个。
    expect(wrapper.get('[data-testid="gateway-mock-state"]').attributes('data-state')).toBe('on')
    expect(wrapper.find('[data-testid="gateway-mock-unsaved"]').exists()).toBe(true)
  })

  it('renders the empty state with presets and no rule rows', () => {
    const wrapper = mountSettings()
    expect(wrapper.get('[data-testid="gateway-mock-rules-empty"]').text()).toBe('admin.gatewayMock.rules.empty')
    expect(wrapper.find('[data-testid="gateway-mock-rule-row"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="gateway-mock-state"]').attributes('data-state')).toBe('off')
  })

  it('saves the whole rule set, including the switch and every edited row', async () => {
    api.updateOperatorSettings.mockResolvedValue(status({
      enabled: true,
      rules: [rule({ reply: 'Hi there!' }), rule({ id: 'gmr_2', keyword: 'ping', reply: 'pong' })],
    }))
    const wrapper = await mountWithRules([rule()])

    await wrapper.get('[data-testid="gateway-mock-rule-reply"]').setValue('Hi there!')
    await wrapper.get('[data-testid="gateway-mock-rule-add"]').trigger('click')
    const rows = wrapper.findAll('[data-testid="gateway-mock-rule-row"]')
    expect(rows).toHaveLength(2)
    await rows[1].get('[data-testid="gateway-mock-rule-keyword"]').setValue('ping')
    await rows[1].get('[data-testid="gateway-mock-rule-reply"]').setValue('pong')

    await wrapper.get('[data-testid="gateway-mock-save"]').trigger('click')
    await flushPromises()

    expect(api.updateOperatorSettings).toHaveBeenCalledTimes(1)
    expect(api.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      rules: [
        { id: 'gmr_1', keyword: 'hi', reply: 'Hi there!', enabled: true },
        { id: '', keyword: 'ping', reply: 'pong', enabled: true },
      ],
    })
    expect(wrapper.get('[data-testid="gateway-mock-notice"]').text()).toBe('admin.gatewayMock.saved')

    await adoptEmitted(wrapper)
    expect(wrapper.find('[data-testid="gateway-mock-unsaved"]').exists()).toBe(false)
  })

  it('deletes a row and disables another through the same whole-set save', async () => {
    api.updateOperatorSettings.mockResolvedValue(status({
      rules: [rule({ id: 'gmr_2', keyword: 'ping', enabled: false })],
    }))
    const wrapper = await mountWithRules([rule(), rule({ id: 'gmr_2', keyword: 'ping' })])

    await wrapper.findAll('[data-testid="gateway-mock-rule-remove"]')[0].trigger('click')
    await wrapper.get('[data-testid="gateway-mock-rule-enabled"]').setValue(false)
    expect(wrapper.get('[data-testid="gateway-mock-rule-enabled-label"]').text()).toBe('admin.gatewayMock.rules.enabledOff')

    await wrapper.get('[data-testid="gateway-mock-save"]').trigger('click')
    await flushPromises()

    expect(api.updateOperatorSettings).toHaveBeenCalledWith({
      enabled: true,
      rules: [{ id: 'gmr_2', keyword: 'ping', reply: 'Hello!', enabled: false }],
    })

    // 服务端归一化后的关键词大小写与顺序不算改动。
    await adoptEmitted(wrapper)
    expect(wrapper.find('[data-testid="gateway-mock-unsaved"]').exists()).toBe(false)
  })

  it('reports the bounded reason the server answered with, and a generic one otherwise', async () => {
    const wrapper = await mountWithRules([rule()])

    api.updateOperatorSettings.mockRejectedValueOnce({ status: 400, reason: 'GATEWAY_MOCK_RULE_KEYWORD_TAKEN', message: 'taken' })
    await wrapper.get('[data-testid="gateway-mock-save"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="gateway-mock-error"]').text()).toBe('admin.gatewayMock.failures.keywordTaken')

    api.updateOperatorSettings.mockRejectedValueOnce(new Error('network'))
    await wrapper.get('[data-testid="gateway-mock-save"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="gateway-mock-error"]').text()).toBe('admin.gatewayMock.failures.generic')
    // 原文只留在服务端；表单不会把未知文本当成校验结论渲染出来。
    expect(wrapper.text()).not.toContain('network')
  })

  it('seeds presets for an empty set and reports them as disabled', async () => {
    api.seedPresets.mockResolvedValue({
      status: status({ rules: [rule({ keyword: 'hello', enabled: false })] }),
      created: 7,
    })
    const wrapper = mountSettings()

    await wrapper.get('[data-testid="gateway-mock-presets"]').trigger('click')
    await flushPromises()

    expect(api.seedPresets).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-testid="gateway-mock-notice"]').text()).toBe('admin.gatewayMock.presetsLoaded')

    await adoptEmitted(wrapper)
    expect(wrapper.find('[data-testid="gateway-mock-rule-row"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="gateway-mock-state"]').attributes('data-state')).toBe('off')
    expect((wrapper.get('[data-testid="gateway-mock-rule-enabled"]').element as HTMLInputElement).checked).toBe(false)

    api.seedPresets.mockResolvedValueOnce({ status: status({ rules: [rule()] }), created: 0 })
    await wrapper.get('[data-testid="gateway-mock-presets"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="gateway-mock-notice"]').text()).toBe('admin.gatewayMock.presetsUnchanged')
  })

  it('never renders an unexpected credential or body field supplied with the status', () => {
    const wrapper = mountSettings({
      ...status({ rules: [rule()] }),
      body: 'BODY_CANARY',
      authorization: 'Bearer secret',
    } as GatewayMockOperatorStatus)
    expect(wrapper.text()).not.toContain('BODY_CANARY')
    expect(wrapper.text()).not.toContain('Bearer secret')
  })
})
