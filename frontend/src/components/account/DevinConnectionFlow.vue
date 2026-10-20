<template>
  <section class="space-y-4" data-testid="devin-connection">
    <p class="text-sm text-gray-600 dark:text-gray-300">{{ t('admin.accounts.devin.intro') }}</p>
    <fieldset :disabled="busy || !!session" class="flex flex-wrap gap-3">
      <label v-for="item in modes" :key="item" class="flex items-center gap-2 text-sm">
        <input v-model="mode" type="radio" :value="item" />{{ t(`admin.accounts.devin.${item}`) }}
      </label>
    </fieldset>
    <template v-if="!session">
      <p class="text-sm text-gray-500">{{ t(`admin.accounts.devin.${mode}Hint`) }}</p>
      <template v-if="mode !== 'browser'">
        <input v-if="mode === 'key'" v-model="content" type="password" autocomplete="off" class="input" placeholder="cog_…" />
        <template v-else>
          <input type="file" accept=".toml,.json,.txt" :aria-label="t('admin.accounts.devin.import')" @change="readFile" />
          <textarea v-model="content" class="input font-mono" rows="4" :aria-label="t('admin.accounts.devin.import')" />
        </template>
      </template>
      <button type="button" class="btn btn-primary" :disabled="busy || (mode !== 'browser' && !content.trim())" @click="start">
        {{ busy ? t('admin.accounts.devin.validating') : t('admin.accounts.devin.connect') }}
      </button>
    </template>
    <template v-else>
      <p class="text-sm" role="status">{{ t(`admin.accounts.devin.status.${session.status}`) }}</p>
      <template v-if="session.status === 'waiting'">
        <p class="text-sm text-gray-500">{{ t('admin.accounts.devin.helperHint') }}</p>
        <button v-if="session.pairing_ticket" type="button" class="btn btn-primary" @click="downloadHelper">{{ t('admin.accounts.devin.downloadHelper') }}</button>
        <details class="text-sm">
          <summary class="cursor-pointer">{{ t('admin.accounts.devin.manualFallback') }}</summary>
          <a v-if="session.auth_url" :href="session.auth_url" target="_blank" rel="noopener noreferrer" class="text-primary-600 underline">{{ t('admin.accounts.devin.openLogin') }}</a>
          <input v-model="callback" class="input mt-2" :placeholder="t('admin.accounts.devin.callbackPlaceholder')" />
          <button type="button" class="btn btn-secondary mt-2" :disabled="busy || !callback.trim()" @click="complete">{{ t('admin.accounts.devin.continue') }}</button>
        </details>
      </template>
      <template v-if="session.status === 'ready' || session.status === 'connected'">
        <p class="text-sm">{{ t('admin.accounts.devin.modelsFound', { count: session.models?.length ?? 0 }) }}</p>
        <p class="text-sm text-gray-500">{{ t(session.auto_recover ? 'admin.accounts.devin.autoRecover' : 'admin.accounts.devin.reloginRequired') }}</p>
        <button v-if="session.status === 'ready'" type="button" class="btn btn-primary" :disabled="busy" @click="commit">{{ t('admin.accounts.devin.save') }}</button>
      </template>
      <button v-if="session.status !== 'connected'" type="button" class="btn btn-secondary" :disabled="busy" @click="restart">{{ t('admin.accounts.devin.restart') }}</button>
    </template>
    <p v-if="error || session?.error" class="whitespace-pre-line text-sm text-red-600" role="alert">{{ error || session?.error }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { buildApiUrl } from '@/api/client'
import { devinAPI, type DevinAccountSettings, type DevinSession } from '@/api/admin/devin'
import helperSource from './devin-auth-helper.py?raw'

const props = defineProps<{ accountId?: number; proxyId?: number | null; settings: DevinAccountSettings }>()
const emit = defineEmits<{ connected: [] }>()
const { t } = useI18n()
const modes = ['key', 'browser', 'import'] as const
const mode = ref<typeof modes[number]>('key')
const content = ref('')
const callback = ref('')
const busy = ref(false)
const error = ref('')
const session = ref<DevinSession | null>(null)
const storedSettings = ref<DevinAccountSettings | null>(null)
const storageKey = computed(() => `devin-connection:${props.accountId ?? 'new'}`)
let timer: ReturnType<typeof setTimeout> | undefined
let stopped = false
let committing = false

function showError(e: unknown) {
  const v = e as { response?: { data?: { message?: string; detail?: string } }; message?: string }
  error.value = v.response?.data?.detail || v.response?.data?.message || v.message || t('admin.accounts.devin.failed')
}
function persist() {
  if (session.value) sessionStorage.setItem(storageKey.value, JSON.stringify({ id: session.value.id, settings: storedSettings.value }))
}
async function poll() {
  if (!session.value || stopped) return
  const id = session.value.id
  try {
    const current = await devinAPI.status(id)
    if (stopped || session.value?.id !== id) return
    session.value = { ...session.value, ...current }
    if (current.status === 'connected') { sessionStorage.removeItem(storageKey.value); emit('connected'); return }
    if (current.status === 'ready') { await commit(); return }
    if (current.status === 'failed') return
    timer = setTimeout(poll, 1500)
  } catch (e) { if (!stopped) showError(e) }
}
async function start() {
  busy.value = true; error.value = ''
  try {
    storedSettings.value = { ...props.settings, group_ids: props.settings.group_ids ? [...props.settings.group_ids] : undefined }
    session.value = await devinAPI.start({ mode: mode.value, content: content.value, proxy_id: props.proxyId, account_id: props.accountId })
    content.value = ''; persist()
    if (stopped) return
    if (session.value.status === 'ready') await commit()
    else if (session.value.status === 'waiting') void poll()
  } catch (e) { showError(e) } finally { busy.value = false }
}
async function complete() {
  if (!session.value) return
  busy.value = true; error.value = ''
  try {
    session.value = await devinAPI.complete(session.value.id, callback.value)
    callback.value = ''
    if (stopped) return
    if (session.value.status === 'ready') await commit()
  } catch (e) { showError(e) } finally { busy.value = false }
}
async function commit() {
  if (!session.value || session.value.status !== 'ready' || committing) return
  committing = true
  busy.value = true
  try {
    session.value = await devinAPI.commit(session.value.id, storedSettings.value ?? props.settings)
    sessionStorage.removeItem(storageKey.value)
    emit('connected')
  } catch (e) { showError(e) } finally { busy.value = false; committing = false }
}
async function restart() {
  if (timer) clearTimeout(timer)
  if (session.value) { try { await devinAPI.cancel(session.value.id) } catch { /* Expired or unreachable sessions still allow a fresh start. */ } }
  session.value = null; error.value = ''; sessionStorage.removeItem(storageKey.value)
}
async function readFile(event: Event) {
  const file = (event.target as HTMLInputElement).files?.[0]
  if (!file) return
  if (file.size > 1024 * 1024) { error.value = t('admin.accounts.devin.fileTooLarge'); return }
  content.value = await file.text()
}
function downloadHelper() {
  if (!session.value?.pairing_ticket || !session.value.auth_url) return
  const endpoint = new URL(buildApiUrl(`/devin/auth-sessions/${session.value.id}/callback`), window.location.origin).href
  const config = { auth_url: session.value.auth_url, callback_url: endpoint, pairing_ticket: session.value.pairing_ticket, expires_at: session.value.expires_at }
  const source = helperSource.replace('__PAIRING_JSON__', JSON.stringify(JSON.stringify(config)))
  const href = URL.createObjectURL(new Blob([source], { type: 'text/x-python' }))
  const link = document.createElement('a'); link.href = href; link.download = 'sub2api-devin-login.py'; link.click(); URL.revokeObjectURL(href)
}
onMounted(async () => {
  try {
    const saved = JSON.parse(sessionStorage.getItem(storageKey.value) || 'null')
    if (saved?.id) { session.value = await devinAPI.status(saved.id); storedSettings.value = saved.settings; if (!stopped) void poll() }
  } catch { sessionStorage.removeItem(storageKey.value) }
})
onUnmounted(() => { stopped = true; if (timer) clearTimeout(timer); content.value = ''; callback.value = '' })
</script>
