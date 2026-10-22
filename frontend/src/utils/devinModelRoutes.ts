import type { DevinModelRoute } from '@/api/admin/devin'

export function readDevinModelRoutes(catalog: unknown): DevinModelRoute[] {
  if (!catalog || typeof catalog !== 'object' || !('routes' in catalog) || !Array.isArray(catalog.routes)) return []
  return catalog.routes.filter((route): route is DevinModelRoute =>
    !!route && typeof route === 'object' &&
    typeof route.model_uid === 'string' && typeof route.catalog_kind === 'string' &&
    typeof route.route_mode === 'string' && typeof route.mechanism_status === 'string'
  )
}

export function isDevinFusionCandidate(model: string): boolean {
  const id = model.trim().toLowerCase()
  return id.startsWith('fusion')
}
