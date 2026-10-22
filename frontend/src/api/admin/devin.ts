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
}

export interface DevinClientInfo {
  ide_name: string
  ide_type?: string
  ide_version: string
  extension_name: string
  extension_version: string
  os: string
  locale: string
}

export interface DevinClientIdentity {
  schema_version: number
  revision: string
  chat: DevinClientInfo
  discovery: DevinClientInfo
  token_exchange: DevinClientInfo
}

export interface DevinClientIdentitySettings {
  manual: DevinClientIdentity | null
  source_url: string
  auto_sync: boolean
}

export interface DevinClientIdentityView extends DevinClientIdentitySettings {
  effective: DevinClientIdentity
  source: 'manual' | 'remote' | 'builtin'
  synced_at?: string
  sync_error?: string
}

const identityRoot = '/admin/settings/devin-client-identity'
export const devinIdentityAPI = {
  async get() { return (await apiClient.get<DevinClientIdentityView>(identityRoot)).data },
  async update(settings: DevinClientIdentitySettings) {
    return (await apiClient.put<DevinClientIdentityView>(identityRoot, settings)).data
  },
  async sync() { return (await apiClient.post<DevinClientIdentityView>(`${identityRoot}/sync`, {}, { timeout: 15000 })).data }
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
