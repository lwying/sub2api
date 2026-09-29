import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({ getTrace: vi.fn() }))
vi.mock('../api', () => ({ getTrace: api.getTrace }))
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import RequestTraceDetailDrawer from '../RequestTraceDetailDrawer.vue'
import { requestTraceStageNames } from '../types'

const BaseDialogStub = {
  props: ['show'], emits: ['close'],
  template: '<div v-if="show" data-testid="trace-dialog"><slot /></div>',
}
const RouterLinkStub = { props: { to: { type: Object, required: true } }, template: '<a><slot /></a>' }

function stage(overrides: Record<string, unknown> = {}) {
  return {
    ordinal: 1, stage: 'client_entry', attempt_index: 0, view_name: '', state: 'not_observed',
    reason: 'body_not_observed', observed_bytes: 0, retained_bytes: 0, dropped_events: 0,
    redaction_unverified: false, ...overrides,
  }
}

function detail(stages: unknown[]) {
  return {
    trace_id: 'a'.repeat(32), route_family: 'messages', inbound_endpoint: '/v1/messages',
    created_at: '2026-09-28T00:00:00Z', completed_at: '2026-09-28T00:00:01Z',
    client_status: 200, capture_state: 'partial', usage_log_id: null, cleanup_after: null, stages,
  }
}

async function mountWithStages(stages: unknown[]) {
  api.getTrace.mockResolvedValue(detail(stages))
  const wrapper = mount(RequestTraceDetailDrawer, {
    props: { show: true, traceId: 'a'.repeat(32) },
    global: { stubs: { BaseDialog: BaseDialogStub, RouterLink: RouterLinkStub } },
  })
  await flushPromises()
  return wrapper
}

/**
 * The amber note claims a body was not observed ("this is not an empty request").
 * Only the stages that carry a client or upstream body can make that claim:
 * `client_metadata` and `wire_attempt` report observed facts instead, so their
 * `not_observed` state describes a body they never carry, and rendering the note
 * next to their facts would deny the facts the operator can see.
 */
describe('admin Request Trace unobserved body note', () => {
  beforeEach(() => api.getTrace.mockReset())

  const bodyStages = ['client_entry', 'wire_request', 'upstream_response', 'client_response']

  it.each(bodyStages)('marks the unobserved body of the %s stage', async (stageName) => {
    const wrapper = await mountWithStages([stage({ stage: stageName })])

    expect(wrapper.get('[data-testid="trace-stage-not-observed"]').text()).toContain('admin.requestTrace.detail.notObserved')
    wrapper.unmount()
  })

  it.each(['client_metadata', 'wire_attempt'])('does not claim an unobserved body for %s facts', async (stageName) => {
    const wrapper = await mountWithStages([
      stage({ stage: stageName, reason: stageName === 'client_metadata' ? 'metadata_observed' : 'wire_observed' }),
    ])

    expect(wrapper.find('[data-testid="trace-stage-not-observed"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('admin.requestTrace.detail.notObserved')
    wrapper.unmount()
  })

  it('keeps the note on observed-body stages even when their facts are present', async () => {
    // A body-less observation must not suppress the note for a stage that really did
    // not hand over its body, and an observed body must not carry the note at all.
    const wrapper = await mountWithStages([
      stage({ ordinal: 1, attempt_index: 1, stage: 'wire_attempt', reason: 'wire_observed' }),
      stage({ ordinal: 2, attempt_index: 1, stage: 'wire_request', reason: 'body_not_observed' }),
      stage({ ordinal: 3, attempt_index: 1, stage: 'wire_request', state: 'stored', reason: 'retained', retained_bytes: 4, payload_text: 'BODY' }),
    ])

    expect(wrapper.get('[data-testid="trace-stage-2"]').find('[data-testid="trace-stage-not-observed"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="trace-stage-1"]').find('[data-testid="trace-stage-not-observed"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="trace-stage-3"]').find('[data-testid="trace-stage-not-observed"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('never claims a body for the body-less decision stage', async () => {
    expect(requestTraceStageNames).toContain('gateway_decision')
    const wrapper = await mountWithStages([stage({ stage: 'gateway_decision', reason: 'decision_not_recorded' })])

    expect(wrapper.find('[data-testid="trace-stage-not-observed"]').exists()).toBe(false)
    wrapper.unmount()
  })
})
