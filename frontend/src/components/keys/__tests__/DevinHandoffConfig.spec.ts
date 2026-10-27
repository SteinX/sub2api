import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import UseKeyModal from '../UseKeyModal.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard: vi.fn().mockResolvedValue(true) }) }))
vi.mock('file-saver', () => ({ saveAs: vi.fn() }))
afterEach(() => vi.unstubAllGlobals())

function modal() {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, text: async () => JSON.stringify({ models: [{ slug: 'granted-uid' }, { slug: 'fusion-granted-pair' }] }) }))
  return mount(UseKeyModal, {
    props: { show: true, apiKey: 'synthetic-key-must-not-enter-files', baseUrl: 'https://example.invalid', platform: 'devin' },
    global: { stubs: { BaseDialog: { template: '<div><slot /></div>' }, Icon: true } }
  })
}

describe('Devin user configuration handoff', () => {
  it.each(['opencode', 'omo'])('connects %s without replacing native agents or approval settings', async client => {
    const wrapper = modal()
    await flushPromises()
    await wrapper.get(`button[data-client="${client}"]`).trigger('click')
    expect(wrapper.find('[data-testid="devin-project-mode"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="devin-workspace-path"]').exists()).toBe(false)
    await wrapper.get('[data-testid="devin-key-from-env"]').setValue(true)
    const text = wrapper.get('pre code').text()
    const config = JSON.parse(text)
    for (const key of ['permission', 'agent', 'default_agent', 'tools', 'shell']) expect(config).not.toHaveProperty(key)
    expect(config.model).toBe('devin/granted-uid')
    expect(config.provider.devin.options.apiKey).toBe('{env:SUB2API_API_KEY}')
    expect(text).not.toContain('synthetic-key-must-not-enter-files')
    wrapper.unmount()
  })

  it('keeps OMP default and compatibility profiles separate without inserting a fabricated cost card', async () => {
    const wrapper = modal()
    await flushPromises()
    await wrapper.get('button[data-client="omp"]').trigger('click')
    await wrapper.get('[data-testid="devin-key-from-env"]').setValue(true)
    const configs = () => wrapper.findAll('pre code').map(node => JSON.parse(node.text()))
    expect(configs()[1].compaction).toEqual({ enabled: false, supersedeReads: false, dropUseless: false })
    await wrapper.get('[data-testid="devin-omp-profile"]').setValue('default')
    expect(configs()[1]).not.toHaveProperty('compaction')
    expect(configs()[1]).not.toHaveProperty('retry')
    expect(configs()[0].providers.devin.apiKey).toBe('SUB2API_API_KEY')
    expect(configs()[0].providers.devin.models[0]).not.toHaveProperty('cost')
    expect(wrapper.get('[data-testid="devin-instructions"]').text()).toContain('ompDefaultInstructions')
    wrapper.unmount()
  })

  it('refuses the known incompatible OMP default Fusion configuration without silently switching profiles', async () => {
    const wrapper = modal()
    await flushPromises()
    await wrapper.get('button[data-client="omp"]').trigger('click')
    await wrapper.get('[data-testid="devin-client-model"]').setValue('fusion-granted-pair')
    expect(wrapper.findAll('[data-testid="client-config-file"]')).toHaveLength(2)
    await wrapper.get('[data-testid="devin-omp-profile"]').setValue('default')
    expect(wrapper.get('[data-testid="devin-omp-incompatible"]').text()).toContain('ompDefaultBlocked')
    expect(wrapper.findAll('[data-testid="client-config-file"]')).toHaveLength(0)
    expect(wrapper.get<HTMLSelectElement>('[data-testid="devin-omp-profile"]').element.value).toBe('default')
    await wrapper.get('[data-testid="devin-client-model"]').setValue('granted-uid')
    expect(wrapper.findAll('[data-testid="client-config-file"]')).toHaveLength(2)
    wrapper.unmount()
  })
})
