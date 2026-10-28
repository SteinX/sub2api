import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import DevinContextSettings from '../DevinContextSettings.vue'
import { devinContextExtra, readDevinContextSettings } from '@/utils/devinContext'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('Devin account compactor settings', () => {
  it('defaults to inheritance and enables both optional capabilities on opt-in', async () => {
    const original = readDevinContextSettings()
    const wrapper = mount(DevinContextSettings, { props: { modelValue: original } })
    expect(wrapper.get<HTMLSelectElement>('select').element.value).toBe('inherit')
    expect(wrapper.find('input').exists()).toBe(false)
    await wrapper.get('select').setValue('enabled')
    const updated = wrapper.emitted('update:modelValue')![0][0] as typeof original
    expect(updated).toEqual({ enabled: true, auto: true, manual: true })
    expect(original.enabled).toBeNull()
    await wrapper.setProps({ modelValue: updated })
    await wrapper.get('[data-testid=devin-context-auto]').setValue(false)
    expect(wrapper.emitted('update:modelValue')![1][0]).toEqual({ enabled: true, auto: false, manual: true })
    await wrapper.get('[data-testid=devin-context-manual]').setValue(false)
    expect(wrapper.emitted('update:modelValue')![2][0]).toEqual({ enabled: true, auto: true, manual: false })
  })

  it('round-trips explicit disable and clears overrides when inheritance is restored', async () => {
    const settings = readDevinContextSettings({ devin_context_enabled: false, devin_context_auto_compaction: false })
    expect(readDevinContextSettings(devinContextExtra(settings))).toEqual(settings)
    const wrapper = mount(DevinContextSettings, { props: { modelValue: settings } })
    expect(wrapper.get<HTMLSelectElement>('select').element.value).toBe('disabled')
    await wrapper.get('select').setValue('inherit')
    const updated = wrapper.emitted('update:modelValue')![0][0] as typeof settings
    expect(devinContextExtra(updated)).toEqual({
      devin_context_enabled: null,
      devin_context_manual_compact: null,
      devin_context_auto_compaction: null
    })
  })
})
