<template>
  <section class="space-y-2 text-sm" data-testid="devin-model-routes">
    <p class="text-gray-600 dark:text-gray-300">{{ t('admin.accounts.devin.routeScope') }}</p>
    <p v-if="hasFusionMapping" role="alert" class="text-amber-700 dark:text-amber-400">{{ t('admin.accounts.devin.fusionMappingWarning') }}</p>
    <details v-if="routes.length" class="rounded-lg border border-gray-200 p-3 dark:border-dark-500">
      <summary class="cursor-pointer">{{ t('admin.accounts.devin.routeSummary', { total: routes.length, blocked: blockedCount }) }}</summary>
      <ul class="mt-2 max-h-60 space-y-2 overflow-y-auto">
        <li v-for="route in routes" :key="route.model_uid" class="flex flex-wrap justify-between gap-2">
          <span class="break-all font-mono text-xs">{{ route.model_uid }}</span>
          <span :class="isCallable(route) ? 'text-gray-600 dark:text-gray-300' : 'text-amber-700 dark:text-amber-400'">{{ routeLabel(route) }}</span>
        </li>
      </ul>
    </details>
    <p v-else class="text-xs text-gray-500">{{ t('admin.accounts.devin.routeSyncRequired') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { DevinModelRoute } from '@/api/admin/devin'
import { isDevinFusionCandidate } from '@/utils/devinModelRoutes'

const props = withDefaults(defineProps<{
  routes?: DevinModelRoute[]
  modelMappings?: { from: string; to: string }[]
}>(), { routes: () => [], modelMappings: () => [] })
const { t } = useI18n()
const isCallable = (route: DevinModelRoute) => route.route_mode === 'direct_chat' || route.route_mode === 'assign_then_chat' || route.route_mode === 'gateway_fusion'
const blockedCount = computed(() => props.routes.filter(route => !isCallable(route)).length)
const hasFusionMapping = computed(() => props.modelMappings.some(mapping =>
  isDevinFusionCandidate(mapping.from) && !isDevinFusionCandidate(mapping.to) && mapping.from.trim() !== mapping.to.trim()
))
function routeLabel(route: DevinModelRoute): string {
  if (route.route_mode === 'gateway_fusion') return t('admin.accounts.devin.routeGatewayFusion', { lead: route.lead_model_uid || '', sidekick: route.sidekick_model_uid || '' })
  if (!isCallable(route)) {
    return t(route.catalog_kind === 'client_pairing_candidate'
      ? 'admin.accounts.devin.routePairingUnsupported'
      : 'admin.accounts.devin.routeUnverified')
  }
  return t(route.route_mode === 'assign_then_chat'
    ? 'admin.accounts.devin.routeRouter'
    : 'admin.accounts.devin.routeDirect')
}
</script>
