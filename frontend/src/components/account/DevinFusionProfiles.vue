<template>
  <section class="space-y-3 border-t border-blue-200 pt-3 dark:border-blue-800" data-testid="devin-fusion-profiles">
    <div class="flex items-center justify-between gap-2">
      <h3 class="font-medium">{{ t('admin.accounts.devin.fusionTitle') }}</h3>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="modelValue.length >= 16" @click="add">{{ t('admin.accounts.devin.fusionAdd') }}</button>
    </div>
    <label class="flex items-center gap-2 text-sm"><input type="checkbox" :checked="enabled" @change="emit('update:enabled', ($event.target as HTMLInputElement).checked)">{{ t('admin.accounts.devin.fusionAutoEnabled') }}</label>
    <p class="text-xs text-gray-600 dark:text-gray-300">{{ t('admin.accounts.devin.fusionHint') }}</p>
    <p v-if="!models.length" class="text-xs text-amber-700 dark:text-amber-400">{{ t('admin.accounts.devin.fusionSyncRequired') }}</p>
    <div v-if="inferredProfiles.length" class="space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-500" data-testid="fusion-inferred-pairs">
      <div v-for="profile in inferredProfiles" :key="profile.public_model" class="space-y-1 text-xs">
        <p class="break-all font-mono">{{ profile.public_model }}</p>
        <p>Lead: {{ profile.lead_model_uid }} · Sidekick: {{ profile.sidekick_model_uid }}</p>
        <button type="button" class="text-primary-600" @click="override(profile, true)">{{ t('admin.accounts.devin.fusionOverride') }}</button>
        <button type="button" class="ml-3 text-gray-500" @click="override(profile, false)">{{ t('admin.accounts.devin.fusionDisablePair') }}</button>
      </div>
    </div>
    <div v-for="(profile, index) in modelValue" :key="index" class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-500">
      <div class="flex items-center justify-between">
        <label class="flex items-center gap-2 text-sm"><input type="checkbox" :checked="profile.enabled" @change="update(index, 'enabled', ($event.target as HTMLInputElement).checked)">{{ t('admin.accounts.devin.fusionEnabled') }}</label>
        <button type="button" class="text-xs text-red-600" @click="remove(index)">{{ t('admin.accounts.devin.fusionRemove') }}</button>
      </div>
      <label class="block text-xs">{{ t('admin.accounts.devin.fusionPublicModel') }}
        <input class="input mt-1 w-full" :value="profile.public_model" required pattern="fusion-[^\s]+" maxlength="128" @input="update(index, 'public_model', ($event.target as HTMLInputElement).value)">
      </label>
      <div class="grid grid-cols-2 gap-3">
        <label v-for="role in roles" :key="role.key" class="text-xs">{{ role.label }}
          <select class="input mt-1 w-full" :value="profile[role.key]" required @change="update(index, role.key, ($event.target as HTMLSelectElement).value)">
            <option value="" disabled>{{ t('admin.accounts.devin.fusionChooseModel') }}</option>
            <option v-if="profile[role.key] && !models.includes(profile[role.key])" :value="profile[role.key]" disabled>{{ profile[role.key] }}</option>
            <option v-for="model in models" :key="model" :value="model">{{ model }}</option>
          </select>
        </label>
      </div>
      <details>
        <summary class="cursor-pointer text-xs">{{ t('admin.accounts.devin.fusionLimits') }}</summary>
        <div class="mt-2 grid grid-cols-2 gap-3">
          <label v-for="field in limits" :key="field.key" class="text-xs">{{ t(`admin.accounts.devin.${field.label}`) }}
            <input class="input mt-1 w-full" type="number" required :value="profile[field.key]" :min="field.min" :max="field.max" :step="field.step || 1" @input="update(index, field.key, Number(($event.target as HTMLInputElement).value))">
          </label>
        </div>
      </details>
      <details data-testid="fusion-compaction-settings">
        <summary class="cursor-pointer text-xs">{{ t('admin.accounts.devin.compactionTitle') }}</summary>
        <div class="mt-2 space-y-2 text-xs">
          <label class="flex items-center gap-2"><input type="checkbox" :checked="compaction(profile).enabled" @change="updateCompaction(index, 'enabled', ($event.target as HTMLInputElement).checked)">{{ t('admin.accounts.devin.compactionEnabled') }}</label>
          <p class="text-gray-500">{{ t('admin.accounts.devin.compactionHint') }}</p>
          <div class="grid grid-cols-2 gap-3">
            <label v-for="role in compactionRoles" :key="role.key">{{ role.label }}
              <input class="input mt-1 w-full" :value="compaction(profile)[role.key]?.join(',') || ''" :placeholder="t('admin.accounts.devin.compactionAutomatic')" pattern="[0-9]+\s*,\s*[0-9]+(\s*,\s*[0-9]+)?" @change="updateCompaction(index, role.key, parseThresholds(($event.target as HTMLInputElement).value))">
            </label>
          </div>
          <p class="text-gray-500">{{ t('admin.accounts.devin.keepaliveUnverified') }}</p>
        </div>
      </details>
    </div>
  </section>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { defaultDevinFusionCompaction, newDevinFusionProfile, type DevinFusionCompaction, type DevinFusionProfile } from '@/utils/devinFusion'
const props = withDefaults(defineProps<{ modelValue: DevinFusionProfile[]; models: string[]; enabled?: boolean; inferredProfiles?: DevinFusionProfile[] }>(), { enabled: true, inferredProfiles: () => [] })
const emit = defineEmits<{ 'update:modelValue': [value: DevinFusionProfile[]]; 'update:enabled': [value: boolean] }>()
const { t } = useI18n()
const roles = computed(() => [
  { key: 'lead_model_uid' as const, label: t('admin.accounts.devin.fusionLead') },
  { key: 'sidekick_model_uid' as const, label: t('admin.accounts.devin.fusionSidekick') }
])
type NumericKey = { [K in keyof DevinFusionProfile]-?: DevinFusionProfile[K] extends number ? K : never }[keyof DevinFusionProfile]
const limits: {key: NumericKey; label: string; min: number; max: number; step?: number}[] = [
  { key: 'max_handoffs_per_turn', label: 'fusionMaxHandoffs', min: 1, max: 8 },
  { key: 'max_internal_requests_per_turn', label: 'fusionMaxRequests', min: 3, max: 64 },
  { key: 'max_external_tool_calls_per_turn', label: 'fusionMaxTools', min: 1, max: 128 },
  { key: 'max_turn_wall_time_seconds', label: 'fusionMaxWall', min: 10, max: 1800 },
  { key: 'max_cost_usd_per_turn', label: 'fusionMaxCost', min: 0.01, max: 100, step: 0.01 },
  { key: 'max_context_bytes', label: 'fusionMaxContext', min: 1024, max: 2097152 },
  { key: 'max_output_tokens', label: 'fusionMaxOutput', min: 64, max: 32768 },
  { key: 'session_ttl_seconds', label: 'fusionTTL', min: 60, max: 604800 }
]
function update<K extends keyof DevinFusionProfile>(index: number, key: K, value: DevinFusionProfile[K]) {
  emit('update:modelValue', props.modelValue.map((profile, i) => i === index ? { ...profile, [key]: value } : profile))
}
function add() { emit('update:modelValue', [...props.modelValue, newDevinFusionProfile(props.models[0])]) }
function override(profile: DevinFusionProfile, enabled: boolean) {
  emit('update:modelValue', [...props.modelValue.filter(existing => existing.public_model !== profile.public_model), { ...profile, enabled }])
}
function remove(index: number) { emit('update:modelValue', props.modelValue.filter((_, i) => i !== index)) }
const compactionRoles = computed(() => [
  { key: 'lead_thresholds' as const, label: t('admin.accounts.devin.compactionLead') },
  { key: 'sidekick_thresholds' as const, label: t('admin.accounts.devin.compactionSidekick') }
])
function compaction(profile: DevinFusionProfile): DevinFusionCompaction { return { ...defaultDevinFusionCompaction(), ...profile.compaction } }
function parseThresholds(value: string): number[] | undefined { return value.trim() ? value.split(',').map(part => Number(part.trim())) : undefined }
function updateCompaction<K extends keyof DevinFusionCompaction>(index: number, key: K, value: DevinFusionCompaction[K]) {
  const profile = props.modelValue[index]
  if (profile) update(index, 'compaction', { ...compaction(profile), [key]: value })
}
</script>
