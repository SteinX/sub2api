import { apiClient } from '../client'

export interface DevinSession {
  id: string
  status: 'waiting' | 'exchanging' | 'validating' | 'ready' | 'connected' | 'failed'
  auth_url?: string
  pairing_ticket?: string
  models: string[] | null
  auto_recover: boolean
  error?: string
  account_id?: number
  expires_at: number
}

export interface DevinAccountSettings {
  name: string
  notes?: string
  group_ids?: number[]
  concurrency?: number
  priority?: number
  rate_multiplier?: number
  load_factor?: number | null
  expires_at?: number | null
  auto_pause_on_expired?: boolean
}

const root = '/admin/devin/auth-sessions'
export const devinAPI = {
  async start(input: { mode: string; content?: string; proxy_id?: number | null; account_id?: number }) {
    return (await apiClient.post<DevinSession>(root, input, { timeout: 90000 })).data
  },
  async status(id: string) { return (await apiClient.get<DevinSession>(`${root}/${encodeURIComponent(id)}`)).data },
  async complete(id: string, code: string) { return (await apiClient.post<DevinSession>(`${root}/${encodeURIComponent(id)}/complete`, { code }, { timeout: 90000 })).data },
  async commit(id: string, settings: DevinAccountSettings) { return (await apiClient.post<DevinSession>(`${root}/${encodeURIComponent(id)}/commit`, settings)).data },
  async cancel(id: string) { await apiClient.delete(`${root}/${encodeURIComponent(id)}`) }
}
