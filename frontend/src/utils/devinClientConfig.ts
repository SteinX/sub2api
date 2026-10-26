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

export function normalizeDevinTestCommand(raw: string): string | null {
  const value = raw.trim()
  return value && value.length <= 256 && /^[a-zA-Z0-9_./:\\ -]+$/.test(value) ? value : null
}

export function devinOpenCodeConfig(baseURL: string, apiKey: string, model: string, models: string[], options: {
  omo: boolean; workspace: string | null; project: boolean; testCommand: string | null
}) {
  const ref = `devin/${model}`
  const scoped = options.omo || options.project
  if (scoped && (!options.workspace || normalizeDevinWorkspacePath(options.workspace) !== options.workspace)) throw new Error('Select a workspace')
  if (options.project && (!options.testCommand || normalizeDevinTestCommand(options.testCommand) !== options.testCommand)) throw new Error('Select an exact test command')
  const agent = options.project ? 'devin-project' : 'devin-check'
  const permission = {
    '*': 'deny', read: 'allow',
    ...(options.project ? { edit: 'allow', glob: 'allow', grep: 'allow', bash: { '*': 'deny', [options.testCommand!]: 'allow' } } : {}),
    external_directory: { [`${options.workspace}/*`]: 'allow' }
  }
  return {
    $schema: 'https://opencode.ai/config.json', model: ref, small_model: ref,
    ...(options.omo ? { plugin: ['oh-my-opencode@5.1.44'] } : {}),
    ...(scoped ? {
      default_agent: agent,
      permission,
      agent: { [agent]: {
        mode: 'primary', model: ref,
        prompt: options.project
          ? 'Complete the requested project task within the selected workspace. Run only the configured test command. Do not spawn client-side workers. Gateway-internal Fusion Lead/Sidekick delegation remains allowed.'
          : 'Read probe.txt when asked. Use only read; do not spawn client-side workers or modify files. Gateway-internal Fusion Lead/Sidekick delegation remains allowed.',
        // Pinned OpenCode translates legacy tools.bash=true into an agent-wide
        // allow, overriding the command rules. Keep one explicit permission
        // policy at both scopes instead; edit also governs write/apply_patch.
        permission
      } }
    } : {}),
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
