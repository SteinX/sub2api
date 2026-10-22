<template>
  <section class="card" data-testid="devin-client-identity-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t(`${prefix}.title`) }}</h2>
      <p class="mt-1 text-sm text-gray-500">{{ t(`${prefix}.description`) }}</p>
    </div>
    <div class="space-y-4 p-6" :aria-busy="busy">
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
      <p v-if="notice" role="status" class="text-sm text-green-600">{{ notice }}</p>
      <template v-if="view">
        <p class="text-sm text-gray-600 dark:text-gray-300" data-testid="devin-effective-identity">
          {{ t(`${prefix}.effective`, { source: t(`${prefix}.sources.${view.source}`), revision: view.effective.revision, chat: view.effective.chat.ide_version, discovery: view.effective.discovery.ide_version, exchange: view.effective.token_exchange.ide_version }) }}
        </p>
        <p v-if="view.synced_at" class="text-xs text-gray-500">{{ t(`${prefix}.syncedAt`, { time: view.synced_at }) }}</p>
        <label class="block text-sm">
          {{ t(`${prefix}.sourceURL`) }}
          <input v-model="sourceURL" type="url" class="input mt-2 w-full font-mono" placeholder="https://example.com/devin/client-identity.json" :disabled="busy" />
        </label>
        <label class="flex items-center gap-2 text-sm">
          <input v-model="autoSync" type="checkbox" :disabled="busy" />{{ t(`${prefix}.autoSync`) }}
        </label>
        <p class="text-xs text-gray-500">{{ t(`${prefix}.syncHint`) }}</p>
        <label class="flex items-center gap-2 text-sm">
          <input v-model="manualEnabled" type="checkbox" :disabled="busy" />{{ t(`${prefix}.manual`) }}
        </label>
        <template v-if="manualEnabled">
          <label class="block text-sm">
            {{ t(`${prefix}.profile`) }}
            <textarea v-model="manualJSON" class="input mt-2 w-full font-mono text-xs" rows="18" spellcheck="false" :disabled="busy" />
          </label>
          <p class="text-xs text-gray-500">{{ t(`${prefix}.manualHint`) }}</p>
        </template>
        <div class="flex flex-wrap gap-3">
          <button type="button" class="btn btn-primary" :disabled="busy" @click="save">{{ t(`${prefix}.save`) }}</button>
          <button type="button" class="btn btn-secondary" :disabled="busy || !view.source_url || dirty" @click="sync">{{ t(`${prefix}.syncNow`) }}</button>
          <button type="button" class="btn btn-secondary" :disabled="busy || dirty" @click="load">{{ t(`${prefix}.refresh`) }}</button>
        </div>
        <p v-if="dirty" class="text-xs text-gray-500">{{ t(`${prefix}.saveFirst`) }}</p>
      </template>
      <button v-else type="button" class="btn btn-secondary" :disabled="busy" @click="load">{{ t(`${prefix}.refresh`) }}</button>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { devinIdentityAPI, type DevinClientIdentity, type DevinClientIdentityView } from '@/api/admin/devin'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const prefix = 'admin.settings.devinClientIdentity'
const view = ref<DevinClientIdentityView | null>(null)
const sourceURL = ref('')
const autoSync = ref(false)
const manualEnabled = ref(false)
const manualJSON = ref('')
const busy = ref(false)
const error = ref('')
const notice = ref('')
const dirty = computed(() => !!view.value && (
  sourceURL.value.trim() !== view.value.source_url || autoSync.value !== view.value.auto_sync ||
  manualEnabled.value !== !!view.value.manual ||
  manualEnabled.value && manualJSON.value !== JSON.stringify(view.value.manual ?? view.value.effective, null, 2)
))

function apply(result: DevinClientIdentityView) {
  view.value = result
  sourceURL.value = result.source_url
  autoSync.value = result.auto_sync
  manualEnabled.value = !!result.manual
  manualJSON.value = JSON.stringify(result.manual ?? result.effective, null, 2)
  if (result.sync_error) error.value = t(`${prefix}.syncFailed`)
}

async function run(action: () => Promise<DevinClientIdentityView>, success = '') {
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    apply(await action())
    if (!error.value && success) notice.value = t(`${prefix}.${success}`)
  } catch (e) {
    error.value = extractApiErrorMessage(e, t(`${prefix}.failed`))
  } finally {
    busy.value = false
  }
}

async function load() { await run(() => devinIdentityAPI.get()) }
async function sync() { await run(() => devinIdentityAPI.sync(), 'synced') }
async function save() {
  let manual: DevinClientIdentity | null = null
  if (manualEnabled.value) {
    try {
      const parsed: unknown = JSON.parse(manualJSON.value)
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('object required')
      manual = parsed as DevinClientIdentity
    } catch {
      error.value = t(`${prefix}.invalidJSON`)
      return
    }
  }
  await run(() => devinIdentityAPI.update({ manual, source_url: sourceURL.value.trim(), auto_sync: autoSync.value }), 'saved')
}

onMounted(load)
</script>
