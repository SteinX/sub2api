// These files follow the pinned OMP short-session compatibility profile.
// JSON is valid YAML 1.2 and preserves model identifiers and keys verbatim.
export function normalizeDevinWorkspacePath(raw: string): string | null {
  const value = raw.trim().replace(/\\/g, '/').replace(/\/+$/, '')
  if (!value || value.startsWith('//') || /[*?[\]{}()!|]/.test(value)) return null
  if ([...value].some(char => char.charCodeAt(0) < 32 || char.charCodeAt(0) === 127)) return null
  if (!/^\/[^/]/.test(value) && !/^[a-z]:\/[^/]/i.test(value)) return null
  if (value.split('/').some(part => part === '.' || part === '..')) return null
  return value
}

export function devinOmpConfig(baseUrl: string, apiKey: string, model: string, advertisedContext?: unknown) {
  const ref = `devin/${model}`
  const contextWindow = typeof advertisedContext === 'number' && Number.isFinite(advertisedContext) && advertisedContext > 0
    ? Math.min(32768, Math.floor(advertisedContext))
    : 32768
  return {
    models: {
      providers: {
        devin: {
          api: 'openai-completions', baseUrl, apiKey,
          models: [{
            id: model, name: model, reasoning: false, input: ['text'],
            contextWindow, maxTokens: Math.min(1024, contextWindow),
            compat: { supportsStore: false, supportsDeveloperRole: false }
          }]
        }
      }
    },
    settings: {
      modelRoles: Object.fromEntries(
        ['default', 'smol', 'slow', 'vision', 'plan', 'commit', 'tiny', 'memory', 'task', 'advisor', 'widget']
          .map(role => [role, ref])
      ),
      memory: { backend: 'off' },
      compaction: { enabled: false, supersedeReads: false, dropUseless: false },
      retry: { enabled: false }
    }
  }
}
