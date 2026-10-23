import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import DevinClientIdentitySettings from '../DevinClientIdentitySettings.vue'

const api = vi.hoisted(() => ({ get: vi.fn(), update: vi.fn(), sync: vi.fn() }))
vi.mock('@/api/admin/devin', () => ({ devinIdentityAPI: api }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const info = { ide_name: 'devin-cli', ide_version: '3000.6.2', extension_name: 'chisel', extension_version: '3000.6.2', os: 'linux', locale: 'en' }
const identity = { schema_version: 1, revision: 'baseline', chat: info, discovery: info, token_exchange: info }
const baseline = { manual: null, source_url: '', auto_sync: false, effective: identity, source: 'builtin' }
const prefix = 'admin.settings.devinClientIdentity'

function button(wrapper: ReturnType<typeof mount>, key: string) {
  return wrapper.findAll('button').find(b => b.text() === `${prefix}.${key}`)!
}

describe('Devin client identity settings', () => {
  beforeEach(() => { vi.resetAllMocks(); api.get.mockResolvedValue(structuredClone(baseline)) })

  it('saves edited client metadata independently and shows the effective result', async () => {
    const wrapper = mount(DevinClientIdentitySettings)
    await flushPromises()
    const manual = structuredClone(identity)
    manual.revision = 'release-2'
    manual.chat.ide_version = '4000.1.0'
    manual.chat.extension_version = '4000.1.0'
    api.update.mockResolvedValue({ ...baseline, manual, effective: manual, source: 'manual' })
    await wrapper.findAll('input[type=checkbox]')[1]!.setValue(true)
    await wrapper.get('textarea').setValue(JSON.stringify(manual, null, 2))
    await button(wrapper, 'save').trigger('click')
    await flushPromises()
    expect(api.update).toHaveBeenCalledWith({ manual, source_url: '', auto_sync: false })
    expect(wrapper.get('[role=status]').text()).toBe(`${prefix}.saved`)
    expect(wrapper.get('textarea').element.value).toContain('4000.1.0')
    expect(api.sync).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('rejects malformed manual JSON before sending a save request', async () => {
    const wrapper = mount(DevinClientIdentitySettings)
    await flushPromises()
    await wrapper.findAll('input[type=checkbox]')[1]!.setValue(true)
    await wrapper.get('textarea').setValue('{invalid')
    await button(wrapper, 'save').trigger('click')
    expect(api.update).not.toHaveBeenCalled()
    expect(wrapper.get('[role=alert]').text()).toBe(`${prefix}.invalidJSON`)
    wrapper.unmount()
  })

  it('requires saving a changed source before syncing and retains the effective profile on failure', async () => {
    api.get.mockResolvedValue({ ...baseline, source_url: 'https://example.com/profile.json' })
    const wrapper = mount(DevinClientIdentitySettings)
    await flushPromises()
    await wrapper.get('input[type=url]').setValue('https://example.com/new-profile.json')
    expect(button(wrapper, 'syncNow').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain(`${prefix}.saveFirst`)
    api.update.mockResolvedValue({ ...baseline, source_url: 'https://example.com/new-profile.json' })
    await button(wrapper, 'save').trigger('click')
    await flushPromises()
    api.sync.mockResolvedValue({ ...baseline, source_url: 'https://example.com/new-profile.json', sync_error: 'unavailable' })
    await button(wrapper, 'syncNow').trigger('click')
    await flushPromises()
    expect(api.sync).toHaveBeenCalledOnce()
    expect(wrapper.get('[role=alert]').text()).toBe(`${prefix}.syncFailed`)
    expect(wrapper.find('[role=status]').exists()).toBe(false)
    expect(wrapper.find('[data-testid=devin-effective-identity]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('clears the manual pin while preserving the remote source', async () => {
    const pinned = { ...baseline, source_url: 'https://example.com/profile.json', manual: identity, source: 'manual' }
    api.get.mockResolvedValue(pinned)
    api.update.mockResolvedValue({ ...pinned, manual: null, source: 'remote' })
    const wrapper = mount(DevinClientIdentitySettings)
    await flushPromises()
    await wrapper.findAll('input[type=checkbox]')[1]!.setValue(false)
    await button(wrapper, 'save').trigger('click')
    await flushPromises()
    expect(api.update).toHaveBeenCalledWith({ manual: null, source_url: pinned.source_url, auto_sync: false })
    wrapper.unmount()
  })
})
