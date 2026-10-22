import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import { reactive } from 'vue'
import DevinFusionProfiles from '../DevinFusionProfiles.vue'
import { newDevinFusionProfile, readDevinFusionProfiles } from '@/utils/devinFusion'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('Devin Fusion configuration', () => {
  it('creates a default-enabled pairing using discovered concrete members', async () => {
    const wrapper = mount(DevinFusionProfiles, { props: { modelValue: [], models: ['lead-real', 'worker-real'] } })
    await wrapper.get('button').trigger('click')
    const profiles = wrapper.emitted('update:modelValue')![0][0] as ReturnType<typeof newDevinFusionProfile>[]
    expect(profiles).toHaveLength(1)
    expect(profiles[0]).toMatchObject({ enabled: true, lead_model_uid: 'lead-real', sidekick_model_uid: 'lead-real' })
    expect(profiles[0].max_cost_usd_per_turn).toBeGreaterThan(0)
  })

  it('keeps model selection and opt-in independent without mutating the saved account', async () => {
    const profile = newDevinFusionProfile('lead-real')
    const wrapper = mount(DevinFusionProfiles, { props: { modelValue: [profile], models: ['lead-real', 'worker-real'] } })
    await wrapper.findAll('select')[1].setValue('worker-real')
    const changed = wrapper.emitted('update:modelValue')![0][0] as typeof profile[]
    expect(changed[0].sidekick_model_uid).toBe('worker-real')
    expect(profile.sidekick_model_uid).toBe('lead-real')
    await wrapper.findAll('input[type=checkbox]')[1].setValue(false)
    expect((wrapper.emitted('update:modelValue')![1][0] as typeof profile[])[0].enabled).toBe(false)
  })

  it('loads reactive persisted profiles without retaining aliases to account state', () => {
    const original = reactive([newDevinFusionProfile('lead-real')])
    const loaded = readDevinFusionProfiles(original)
    loaded[0].enabled = false
    expect(original[0].enabled).toBe(true)
    expect(readDevinFusionProfiles(undefined)).toEqual([])
  })
  it('defaults the account switch on and lets operators disable an inferred pair', async () => {
    const inferred = { ...newDevinFusionProfile('lead'), public_model: 'fusion-lead-sidekick-worker', sidekick_model_uid: 'worker' }
    const wrapper = mount(DevinFusionProfiles, { props: { modelValue: [], models: ['lead', 'worker'], inferredProfiles: [inferred] } })
    expect((wrapper.get('input[type=checkbox]').element as HTMLInputElement).checked).toBe(true)
    const buttons = wrapper.get('[data-testid=fusion-inferred-pairs]').findAll('button')
    await buttons[1].trigger('click')
    expect((wrapper.emitted('update:modelValue')![0][0] as typeof inferred[])[0]).toMatchObject({ public_model: inferred.public_model, enabled: false })
    await wrapper.get('input[type=checkbox]').setValue(false)
    expect(wrapper.emitted('update:enabled')![0]).toEqual([false])
  })

  it('edits independent compaction thresholds without mutating saved settings', async () => {
    const profile = newDevinFusionProfile('lead')
    const wrapper = mount(DevinFusionProfiles, { props: { modelValue: [profile], models: ['lead'] } })
    const settings = wrapper.get('[data-testid=fusion-compaction-settings]')
    await settings.findAll('input')[2].setValue('2000,4000')
    const changed = wrapper.emitted('update:modelValue')![0][0] as typeof profile[]
    expect(changed[0].compaction?.sidekick_thresholds).toEqual([2000, 4000])
    expect(changed[0].compaction?.lead_thresholds).toBeUndefined()
    expect(profile.compaction?.sidekick_thresholds).toBeUndefined()
    await settings.get('input[type=checkbox]').setValue(false)
    expect((wrapper.emitted('update:modelValue')![1][0] as typeof profile[])[0].compaction?.enabled).toBe(false)
  })

})
