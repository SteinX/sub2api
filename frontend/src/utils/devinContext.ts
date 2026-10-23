export interface DevinContextSettings {
  enabled: boolean | null
  manual: boolean
  auto: boolean
}

export function readDevinContextSettings(extra?: Record<string, unknown> | null): DevinContextSettings {
  return {
    enabled: extra?.devin_context_enabled == null ? null : extra.devin_context_enabled === true,
    manual: extra?.devin_context_manual_compact !== false,
    auto: extra?.devin_context_auto_compaction !== false
  }
}

export function devinContextExtra(settings: DevinContextSettings) {
  return {
    devin_context_enabled: settings.enabled,
    devin_context_manual_compact: settings.enabled === null ? null : settings.manual,
    devin_context_auto_compaction: settings.enabled === null ? null : settings.auto
  }
}
