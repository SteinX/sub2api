import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import DevinConnectionFlow from '../DevinConnectionFlow.vue'

const api = vi.hoisted(() => ({ start: vi.fn(), status: vi.fn(), complete: vi.fn(), commit: vi.fn(), cancel: vi.fn() }))
vi.mock('@/api/admin/devin', () => ({ devinAPI: api }))
vi.mock('@/api/client', () => ({ buildApiUrl: (path: string) => `/api/v1${path}` }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const ready = { id: 'session', status: 'ready', models: ['swe-2-max'], auto_recover: true, expires_at: 9999999999 }
const connected = { ...ready, status: 'connected', account_id: 11 }
const settings = { name: 'Devin account', group_ids: [4], concurrency: 2 }

describe('Devin connection lifecycle', () => {
  beforeEach(() => { vi.resetAllMocks(); sessionStorage.clear(); api.commit.mockResolvedValue(connected) })
  afterEach(() => { vi.useRealTimers() })

  it('validates a durable key, saves the selected settings and clears credential input', async () => {
    api.start.mockResolvedValue(ready)
    const wrapper = mount(DevinConnectionFlow, { props: { settings, proxyId: 3 } })
    await wrapper.get('input[type=password]').setValue('cog_private')
    await wrapper.get('button').trigger('click'); await flushPromises()
    expect(api.start).toHaveBeenCalledWith({ mode: 'key', content: 'cog_private', proxy_id: 3, account_id: undefined })
    expect(api.commit).toHaveBeenCalledWith('session', settings)
    expect(wrapper.emitted('connected')).toHaveLength(1)
    expect(sessionStorage.getItem('devin-connection:new')).toBeNull()
    expect(wrapper.html()).not.toContain('cog_private')
    wrapper.unmount()
  })

  it('keeps pairing tickets out of resumable browser state', async () => {
    const waiting = { ...ready, status: 'waiting', pairing_ticket: 'one-shot-ticket', auth_url: 'https://app.devin.ai/auth/cli/continue?state=test' }
    api.start.mockResolvedValue(waiting); api.status.mockResolvedValue({ ...waiting, pairing_ticket: undefined })
    const wrapper = mount(DevinConnectionFlow, { props: { settings } })
    await wrapper.get('input[value=browser]').setValue()
    await wrapper.get('button').trigger('click'); await flushPromises()
    const stored = sessionStorage.getItem('devin-connection:new') || ''
    expect(stored).toContain('session'); expect(stored).not.toContain('one-shot-ticket')
    expect(api.commit).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('resumes with the original settings and commits once', async () => {
    sessionStorage.setItem('devin-connection:9', JSON.stringify({ id: 'session', settings }))
    api.status.mockResolvedValue(ready)
    const wrapper = mount(DevinConnectionFlow, { props: { accountId: 9, settings: { name: 'new value' } } })
    await flushPromises()
    expect(api.commit).toHaveBeenCalledOnce(); expect(api.commit).toHaveBeenCalledWith('session', settings)
    expect(wrapper.emitted('connected')).toHaveLength(1)
    wrapper.unmount()
  })

  it.each([undefined, 429, 503])('recovers polling after a transient status failure %s and commits once', async (status) => {
    vi.useFakeTimers()
    api.start.mockResolvedValue({ ...ready, status: 'waiting' })
    api.status.mockRejectedValueOnce(Object.assign(new Error('Temporary connection failure'), { response: status ? { status } : undefined }))
      .mockResolvedValueOnce(ready)
    const wrapper = mount(DevinConnectionFlow, { props: { settings } })
    await wrapper.get('input[value=browser]').setValue()
    await wrapper.get('button').trigger('click'); await flushPromises()
    expect(api.status).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[role=alert]').text()).toContain('Temporary connection failure')
    await vi.advanceTimersByTimeAsync(3000); await flushPromises()
    expect(api.status).toHaveBeenCalledTimes(2)
    expect(api.commit).toHaveBeenCalledOnce()
    expect(wrapper.emitted('connected')).toHaveLength(1)
    expect(wrapper.find('[role=alert]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('stops retrying when the component is closed', async () => {
    vi.useFakeTimers()
    api.start.mockResolvedValue({ ...ready, status: 'waiting' })
    api.status.mockRejectedValue(new Error('Temporary connection failure'))
    const wrapper = mount(DevinConnectionFlow, { props: { settings } })
    await wrapper.get('input[value=browser]').setValue()
    await wrapper.get('button').trigger('click'); await flushPromises()
    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(9000)
    expect(api.status).toHaveBeenCalledTimes(1)
    expect(api.commit).not.toHaveBeenCalled()
  })

  it.each([400, 401, 403])('does not retry a permanent status failure %s', async (status) => {
    vi.useFakeTimers()
    api.start.mockResolvedValue({ ...ready, status: 'waiting' })
    api.status.mockRejectedValue({ response: { status } })
    const wrapper = mount(DevinConnectionFlow, { props: { settings } })
    await wrapper.get('input[value=browser]').setValue()
    await wrapper.get('button').trigger('click'); await flushPromises()
    await vi.advanceTimersByTimeAsync(9000)
    expect(api.status).toHaveBeenCalledTimes(1)
    expect(api.commit).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('does not retry after the authorization expires', async () => {
    vi.useFakeTimers()
    api.start.mockResolvedValue({ ...ready, status: 'waiting', expires_at: Math.floor(Date.now() / 1000) - 1 })
    api.status.mockRejectedValue(new Error('Temporary connection failure'))
    const wrapper = mount(DevinConnectionFlow, { props: { settings } })
    await wrapper.get('input[value=browser]').setValue()
    await wrapper.get('button').trigger('click'); await flushPromises()
    await vi.advanceTimersByTimeAsync(9000)
    expect(api.status).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('does not create an account after the user leaves a pending validation', async () => {
    let resolve: (value: typeof ready) => void = () => {}
    api.start.mockReturnValue(new Promise(r => { resolve = r }))
    const wrapper = mount(DevinConnectionFlow, { props: { settings } })
    await wrapper.get('input[type=password]').setValue('cog_key')
    await wrapper.get('button').trigger('click'); wrapper.unmount()
    resolve(ready); await flushPromises()
    expect(api.commit).not.toHaveBeenCalled()
    expect(sessionStorage.getItem('devin-connection:new')).toContain('session')
  })

  it('allows starting over when a failed session has already expired on the server', async () => {
    api.start.mockResolvedValue({ ...ready, status: 'failed', error: 'expired' })
    api.cancel.mockRejectedValue(new Error('Session expired'))
    const wrapper = mount(DevinConnectionFlow, { props: { settings } })
    await wrapper.get('input[type=password]').setValue('cog_key')
    await wrapper.get('button').trigger('click'); await flushPromises()
    const restart = wrapper.findAll('button').find(button => button.text().includes('devin.restart'))
    expect(restart).toBeDefined(); await restart!.trigger('click'); await flushPromises()
    expect(wrapper.find('input[type=password]').exists()).toBe(true)
    expect(sessionStorage.getItem('devin-connection:new')).toBeNull()
    wrapper.unmount()
  })
})
