import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import DevinModelRoutes from '../DevinModelRoutes.vue'
import accounts from '@/i18n/locales/zh/admin/accounts'
import type { DevinModelRoute } from '@/api/admin/devin'
import { readDevinModelRoutes } from '@/utils/devinModelRoutes'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, params?: Record<string, number | string>) => {
  const value = accounts.accounts.devin[key.split('.').at(-1) as keyof typeof accounts.accounts.devin]
  return typeof value === 'string' ? value.replace(/\{(\w+)\}/g, (_, name: string) => String(params?.[name] ?? '')) : key
} }) }))

const routes: DevinModelRoute[] = [
  { model_uid: 'ordinary', catalog_kind: 'direct_model', route_mode: 'direct_chat', mechanism_status: 'not_requested' },
  { model_uid: 'router', catalog_kind: 'router_candidate', route_mode: 'assign_then_chat', mechanism_status: 'not_requested' },
  { model_uid: 'fusion-lead-fast-sidekick-worker', catalog_kind: 'client_pairing_candidate', route_mode: 'blocked', mechanism_status: 'unverified' },
  { model_uid: 'opaque-harness', catalog_kind: 'harness_backed_candidate', route_mode: 'blocked', mechanism_status: 'unverified' },
]
function render(props = {}) {
  return mount(DevinModelRoutes, { props })
}

describe('Devin execution support', () => {
  it('keeps advertised special entries visible and distinguishes their support from callable transports', () => {
    const wrapper = render({ routes })
    expect(wrapper.get('summary').text()).toContain('4 项，其中 2 项暂不可调用')
    const entries = wrapper.findAll('li')
    expect(entries[0].text()).toContain('可调用：普通模型')
    expect(entries[1].text()).toContain('运行时分配模型')
    expect(entries[2].text()).toContain('未能识别可用配对')
    expect(entries[3].text()).toContain('执行方式未验证')
    expect(wrapper.text()).toContain('不代表已执行')
  })

  it('warns on a Fusion mapping even when a legacy account has no metadata snapshot', () => {
    const wrapper = render({ modelMappings: [{ from: 'fusion-lead-fast-sidekick-worker', to: 'lead-fast' }] })
    expect(wrapper.get('[role=alert]').text()).toContain('映射会被拒绝')
    expect(wrapper.text()).toContain('同步上游模型')
    expect(wrapper.find('summary').exists()).toBe(false)
  })

  it('accepts persisted diagnostic rows without exposing unrelated account fields', () => {
    const restored = JSON.parse(JSON.stringify({ routes, credential: 'never-render-this' }))
    const wrapper = render({ routes: readDevinModelRoutes(restored) })
    expect(wrapper.findAll('li')).toHaveLength(4)
    expect(wrapper.html()).not.toContain('never-render-this')
    expect(readDevinModelRoutes({ routes: [null, 'invalid', { model_uid: 'partial' }] })).toEqual([])
  })

  it('does not describe an unknown future route mode as callable', () => {
    const wrapper = render({ routes: [{ model_uid: 'future-special', catalog_kind: 'unknown_special', route_mode: 'future_execution_mode', mechanism_status: 'unverified' }] })
    expect(wrapper.get('summary').text()).toContain('1 项暂不可调用')
    expect(wrapper.get('li').text()).toContain('执行方式未验证')
    expect(wrapper.get('li').text()).not.toContain('可调用：')
  })
  it('describes resolved pairs as gateway collaboration with both concrete members', () => {
    const wrapper = render({ routes: [{ model_uid: 'fusion-lead-sidekick-worker', catalog_kind: 'client_pairing_candidate', route_mode: 'gateway_fusion', mechanism_status: 'gateway_orchestrated', lead_model_uid: 'lead', sidekick_model_uid: 'worker', pair_source: 'model_id' }] })
    expect(wrapper.get('li').text()).toContain('Lead lead / Sidekick worker')
    expect(wrapper.get('summary').text()).toContain('0 项暂不可调用')
  })

})
