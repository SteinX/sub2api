import { defineComponent } from 'vue'
import { createPinia } from 'pinia'
import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import type { Account } from '@/types'
import ReAuthAccountModal from '../ReAuthAccountModal.vue'

vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))

describe('Devin reauthorization from the account list', () => {
  it.each(['oauth', 'apikey'] as const)('reconnects the existing %s account using Devin', async (type) => {
    const wrapper = mount(ReAuthAccountModal, {
      props: {
        show: true,
        account: { id: 42, name: 'Devin account', platform: 'devin', type, proxy_id: 3 } as Account,
      },
      global: {
        plugins: [createPinia()],
        stubs: {
          BaseDialog: defineComponent({ template: '<div><slot /><slot name="footer" /></div>' }),
          OAuthAuthorizationFlow: true,
          DevinConnectionFlow: true,
        },
      },
    })

    expect(wrapper.text()).toContain('admin.accounts.devin.account')
    expect(wrapper.text()).not.toContain('admin.accounts.claudeCodeAccount')
    expect(wrapper.findComponent({ name: 'OAuthAuthorizationFlow' }).exists()).toBe(false)
    expect(wrapper.text()).not.toContain('admin.accounts.oauth.completeAuth')
    const flow = wrapper.getComponent({ name: 'DevinConnectionFlow' })
    expect(flow.props()).toMatchObject({ accountId: 42, proxyId: 3, settings: { name: 'Devin account' } })
    await flow.vm.$emit('connected')
    expect(wrapper.emitted('reauthorized')).toEqual([[]])
    expect(wrapper.emitted('close')).toHaveLength(1)
    wrapper.unmount()
  })
})
