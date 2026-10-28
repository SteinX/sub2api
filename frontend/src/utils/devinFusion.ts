export interface DevinFusionCompaction {
  enabled: boolean
  lead_thresholds?: number[]
  sidekick_thresholds?: number[]
  max_attempts_per_role_per_turn: number
  timeout_seconds: number
  max_output_tokens: number
}

export const defaultDevinFusionCompaction = (): DevinFusionCompaction => ({
  enabled: true, max_attempts_per_role_per_turn: 2, timeout_seconds: 30, max_output_tokens: 1024
})

export interface DevinFusionProfile {
  enabled: boolean
  public_model: string
  lead_model_uid: string
  sidekick_model_uid: string
  max_handoffs_per_turn: number
  max_internal_requests_per_turn: number
  max_external_tool_calls_per_turn: number
  max_turn_wall_time_seconds: number
  max_cost_usd_per_turn: number
  max_context_bytes: number
  max_output_tokens: number
  session_ttl_seconds: number
  compaction?: DevinFusionCompaction
}

export function readDevinFusionProfiles(value: unknown): DevinFusionProfile[] {
  return Array.isArray(value) ? JSON.parse(JSON.stringify(value)) as DevinFusionProfile[] : []
}

export function newDevinFusionProfile(model = ''): DevinFusionProfile {
  return {
    enabled: true, public_model: 'fusion-team', lead_model_uid: model, sidekick_model_uid: model,
    max_handoffs_per_turn: 4, max_internal_requests_per_turn: 20,
    max_external_tool_calls_per_turn: 40, max_turn_wall_time_seconds: 900,
    max_cost_usd_per_turn: 5, max_context_bytes: 262144,
    max_output_tokens: 4096, session_ttl_seconds: 86400,
    compaction: defaultDevinFusionCompaction()
  }
}

export function inferredDevinFusionProfile(publicModel: string, lead: string, sidekick: string): DevinFusionProfile {
  return { ...newDevinFusionProfile(lead), public_model: publicModel, sidekick_model_uid: sidekick }
}
