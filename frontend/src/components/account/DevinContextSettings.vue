<template>
  <section class="space-y-3 border-t border-blue-200 pt-3 dark:border-blue-800" data-testid="devin-context-settings">
    <label class="block text-sm font-medium">
      {{ t('admin.accounts.devin.contextTitle') }}
      <select class="input mt-2 w-full" :value="mode" data-testid="devin-context-mode" @change="changeMode">
        <option value="inherit">{{ t('admin.accounts.devin.contextInherit') }}</option>
        <option value="enabled">{{ t('admin.accounts.devin.contextOn') }}</option>
        <option value="disabled">{{ t('admin.accounts.devin.contextOff') }}</option>
      </select>
    </label>
    <p class="text-xs text-gray-600 dark:text-gray-300">{{ t(`admin.accounts.devin.contextModeHint.${mode}`) }}</p>
    <div v-if="modelValue.enabled === true" class="space-y-2 text-sm">
      <label class="flex items-center gap-2">
        <input type="checkbox" data-testid="devin-context-auto" :checked="modelValue.auto" @change="update('auto', ($event.target as HTMLInputElement).checked)">
        {{ t('admin.accounts.devin.contextAuto') }}
      </label>
      <label class="flex items-center gap-2">
        <input type="checkbox" data-testid="devin-context-manual" :checked="modelValue.manual" @change="update('manual', ($event.target as HTMLInputElement).checked)">
        {{ t('admin.accounts.devin.contextManual') }}
      </label>
    </div>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.devin.contextUsageHint') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { DevinContextSettings } from '@/utils/devinContext'

const props = defineProps<{ modelValue: DevinContextSettings }>()
const emit = defineEmits<{ 'update:modelValue': [value: DevinContextSettings] }>()
const { t } = useI18n()
const mode = computed(() => props.modelValue.enabled === null ? 'inherit' : props.modelValue.enabled ? 'enabled' : 'disabled')

function update<K extends keyof DevinContextSettings>(key: K, value: DevinContextSettings[K]) {
  emit('update:modelValue', { ...props.modelValue, [key]: value })
}
function changeMode(event: Event) {
  const value = (event.target as HTMLSelectElement).value
  update('enabled', value === 'inherit' ? null : value === 'enabled')
}
</script>
