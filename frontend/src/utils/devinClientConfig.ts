// These files follow the pinned OMP short-session compatibility profile.
// JSON is valid YAML 1.2 and preserves model identifiers and keys verbatim.
export function devinOpenCodeConfig(baseURL: string, apiKey: string, model: string, models: string[], options: {
  omo: boolean
}) {
  const ref = `devin/${model}`
  return {
    $schema: 'https://opencode.ai/config.json', model: ref, small_model: ref,
    ...(options.omo ? { plugin: ['oh-my-opencode@5.1.44'] } : {}),
    // Connect the provider without replacing the client's agents, prompts or
    // approval policy. These settings are not a filesystem or shell sandbox.
    provider: { devin: {
      npm: '@ai-sdk/openai-compatible', name: 'Devin via Sub2API', options: { baseURL, apiKey },
      models: Object.fromEntries(models.map(id => [id, { name: id }]))
    } }
  }
}

export function devinOmpConfig(baseUrl: string, apiKey: string, model: string, advertisedContext?: unknown, profile: 'compatibility' | 'default' = 'compatibility') {
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
      ...(profile === 'compatibility' ? {
        memory: { backend: 'off' },
        compaction: { enabled: false, supersedeReads: false, dropUseless: false },
        retry: { enabled: false }
      } : {})
    }
  }
}
