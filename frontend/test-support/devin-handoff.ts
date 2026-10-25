// Browser fixture only. The production modal performs discovery and generates
// every copied file. No fetch, clipboard or Vue component is mocked here.
import { createApp, h } from 'vue'
import { createPinia } from 'pinia'
import UseKeyModal from '../src/components/keys/UseKeyModal.vue'
import { i18n, initI18n } from '../src/i18n'

async function main() {
  const response = await fetch('/__journey/session', { cache: 'no-store' })
  if (!response.ok) throw new Error('Disposable session unavailable')
  const session = await response.json()
  await initI18n()
  createApp({ render: () => h(UseKeyModal, { show: true, platform: 'devin', apiKey: session.key, baseUrl: location.origin }) })
    .use(createPinia()).use(i18n).mount('#app')
}
void main()
