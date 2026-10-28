<template>
  <div class="space-y-1">
    <div v-if="loading" class="h-3 w-32 animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
    <div v-else-if="errorLabel" class="max-w-[220px] text-xs text-amber-600 dark:text-amber-400">
      {{ errorLabel }}
    </div>
    <template v-else-if="usage">
      <div v-if="usage.devin_usage?.plan_name" class="max-w-[220px] truncate text-[10px] text-gray-500 dark:text-gray-400" :title="usage.devin_usage.plan_name">
        {{ usage.devin_usage.plan_name }}
      </div>
      <UsageProgressBar
        v-if="usage.daily"
        :label="t('admin.accounts.devin.dailyUsage')"
        label-width="auto"
        :utilization="usage.daily.utilization"
        :resets-at="usage.daily.resets_at"
        color="indigo"
      />
      <div v-else class="text-[10px] text-gray-400">
        {{ t('admin.accounts.devin.dailyUsage') }} · {{ t('admin.accounts.devin.quotaUnavailable') }}
      </div>
      <UsageProgressBar
        v-if="usage.seven_day"
        :label="t('admin.accounts.devin.weeklyUsage')"
        label-width="auto"
        :utilization="usage.seven_day.utilization"
        :resets-at="usage.seven_day.resets_at"
        color="emerald"
      />
      <div v-else class="text-[10px] text-gray-400">
        {{ t('admin.accounts.devin.weeklyUsage') }} · {{ t('admin.accounts.devin.quotaUnavailable') }}
      </div>
      <div v-for="credit in credits" :key="credit.key" class="text-[10px] text-gray-500 dark:text-gray-400">
        {{ t(`admin.accounts.devin.${credit.key}`) }}: {{ formatCompactNumber(credit.value) }}
      </div>
      <div v-if="usage.devin_usage?.acu_consumed != null || usage.devin_usage?.acu_limit != null" class="text-[10px] text-gray-500 dark:text-gray-400">
        ACU: {{ usage.devin_usage.acu_consumed ?? '—' }} / {{ usage.devin_usage.acu_limit ?? '—' }}
      </div>
    </template>
    <button
      type="button"
      class="inline-flex rounded px-1.5 py-0.5 text-[10px] font-medium text-blue-600 hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
      :disabled="loading"
      @click="$emit('refresh')"
    >
      {{ t('admin.accounts.usageWindow.activeQuery') }}
    </button>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AccountUsageInfo } from '@/types'
import { formatCompactNumber } from '@/utils/format'
import UsageProgressBar from './UsageProgressBar.vue'

const props = defineProps<{
  usage: AccountUsageInfo | null
  loading: boolean
  error: string | null
}>()
defineEmits<{ refresh: [] }>()
const { t } = useI18n()
const errorLabel = computed(() => {
  if (props.error) return t('admin.accounts.devin.usageFailed')
  if (props.usage?.needs_reauth) return t('admin.accounts.needsReauth')
  switch (props.usage?.error_code) {
    case 'forbidden': return t('admin.accounts.devin.usageForbidden')
    case 'rate_limited': return t('admin.accounts.devin.usageRateLimited')
  }
  return props.usage?.error ? t('admin.accounts.devin.usageFailed') : ''
})
const credits = computed(() => {
  const summary = props.usage?.devin_usage
  return [
    { key: 'promptCredits', value: summary?.available_prompt_credits },
    { key: 'flowCredits', value: summary?.available_flow_credits },
    { key: 'flexCredits', value: summary?.available_flex_credits }
  ].filter((item): item is { key: string; value: number } => item.value != null)
})
</script>
