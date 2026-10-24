# OMP Fusion runtime session adapter

This adapter targets the published `@oh-my-pi/pi-coding-agent` and `@oh-my-pi/pi-ai` **18.12.6** packages. It registers only `sub2api-fusion/<SUB2API_FUSION_MODEL>`, using a separate custom API ID and the native OpenAI Chat serializer. It does not change OMP's native Devin provider or any other configured provider.

OMP 18.12.6's catalog types contain `compat.sessionHeader`, but the `models.yml` parser removes that unrecognized compatibility field. A real CLI request to the loopback probe confirmed that configuring it does not send `x-litellm-session-id`. This extension supplies the header through the supported custom-provider interface instead.

## Usage

Load `omp-fusion-session.ts` using OMP's `--extension` / `-e` option and explicitly select `sub2api-fusion/<model>`. The extension needs:

```sh
export SUB2API_FUSION_BASE_URL=http://127.0.0.1:8767/v1
export SUB2API_FUSION_MODEL=fusion-probe
export SUB2API_FUSION_API_KEY=dummy-loopback-only
omp -e /absolute/path/to/omp-fusion-session.ts --model sub2api-fusion/fusion-probe --print 'Read probe.txt and report its contents.'
```

This example points to the deterministic loopback probe, not Devin. Use the isolated config/cache/data directories described by the client probe harness when recording acceptance evidence; run from the directory containing its synthetic `probe.txt`.

For an actual Sub2API deployment, explicitly set `SUB2API_FUSION_ENDPOINT_KIND=sub2api`, use an HTTPS base URL, supply its API key through the environment, and select a model that the deployment already binds to Fusion. Non-loopback HTTP URLs and credential-bearing URLs are rejected. Do not use the extension with another provider's endpoint.

The advertised context/output limits are conservative local catalog hints (32768/4096), and the zero catalog costs are placeholders. These values do not establish actual model limits, pricing, quality, or usage accounting; configure those on the server before evaluating them. This initial adapter exposes text input and does not advertise native reasoning controls or vision.

## Identity and worker isolation

Each request obtains its identity from the `streamSimple` hook's **own `options.sessionId`**. OMP's SDK derives that value from `sessionManager.getSessionId()` unless a distinct provider session ID was explicitly supplied, passes it into `Agent`, and forwards it through the agent loop to the streamer. New task workers have their own session managers. The extension reads the request-local value every time; it does not capture the parent's context, create a shared current-session variable, derive an ID from the prompt/cache key, or invent a replacement epoch.

The adapter sets one canonical `x-litellm-session-id` header. Empty IDs, control characters, invalid Unicode, IDs exceeding 255 UTF-8 bytes, and conflicting preconfigured session headers fail explicitly. Static `X-Sub2API-Fusion-Session` overrides are rejected by this adapter because they defeat its per-worker identity. The server's explicit Fusion header priority remains available to other integrations.

Keep OMP's task/smol/slow/tiny/memory/advisor/widget roles on explicit direct models for this initial rollout. Sending a separate worker to Fusion requires a separately validated model/agent/tool binding. The adapter supports distinct worker IDs; offline fixtures establish its isolation behavior. They do not establish that every OMP worker lifecycle, role fallback, retry policy, or future release is compatible. See the deliberately incomplete [role template](templates/omp-fusion-session-roles.yml): replace its model placeholders and verify actual dispatched models before making upstream calls. OMP can select a worker's model through an explicit task request, `task.agentModelOverrides`, the agent's frontmatter, or inheritance, in that order; role defaults alone do not freeze every agent.

### Early smoke: disable automatic compaction

For the short loopback smoke, put this in the isolated agent directory's `config.yml` (`PI_CODING_AGENT_DIR/config.yml`), or the synthetic project's `.omp/config.yml`:

```yaml
compaction:
  enabled: false
```

OMP 18.12.6's public [`compaction.enabled` setting](https://github.com/can1357/oh-my-pi/blob/main/packages/coding-agent/src/session/context-settings.ts) at line 106 is the **Auto-Compact** toggle. It disables automatic compaction; it does not establish compatibility with manual `/compact`.

Compaction is **not** a `modelRoles` role. [`resolveCompactionModelCandidates`](https://github.com/can1357/oh-my-pi/blob/main/packages/coding-agent/src/session/session-maintenance.ts) near line 3505 tries the current model's configured `compactionModel`, then the current model itself, then role candidates. Pinning `smol` to a direct model therefore does not by itself keep compaction away from Fusion. Although the `models.yml` schema supports `compactionModel`, the published extension `ProviderModelConfig` does not expose that field; this minimal extension does not claim to configure it.

Ordinary `/compact`, `/fork`, and two concurrent real workers remain required, **unaccepted daily-use cases**. Closing the short-smoke boundary does not waive them. If the server rejects changed history/tools or lineage, retain that explicit error; do not silently change the session ID or repeat the request in a new epoch to make a check pass.

## Supported API evidence

The following are official upstream sources. GitHub's `main` branch can move; the package version and hashes below identify the inspected release files.

| Contract | Official source / inspected 18.12.6 location |
| --- | --- |
| `before_provider_request` can replace only a payload | [Extension event types](https://github.com/can1357/oh-my-pi/blob/main/packages/coding-agent/src/extensibility/extensions/types.ts), `BeforeProviderRequestEvent` near line 1054 |
| `registerProvider`, custom `api`, and `streamSimple(model, context, options)` are public | [Extension provider types](https://github.com/can1357/oh-my-pi/blob/main/packages/coding-agent/src/extensibility/extensions/types.ts), lines 1920–2054 |
| SDK binds the provider ID to the session manager and forwards it into the agent | [SDK](https://github.com/can1357/oh-my-pi/blob/main/packages/coding-agent/src/sdk.ts), lines 1943 and 4548 |
| Agent carries `sessionId` into stream configuration | [Agent](https://github.com/can1357/oh-my-pi/blob/main/packages/agent/src/agent.ts), near line 1892 |
| Request options include that configuration | [Agent loop](https://github.com/can1357/oh-my-pi/blob/main/packages/agent/src/agent-loop.ts), near line 2152 |
| Task workers create sessions using their own manager | [Task executor](https://github.com/can1357/oh-my-pi/blob/main/packages/coding-agent/src/task/executor.ts), near line 4717 |
| Custom API dispatch is distinct from the native API and builtin IDs are reserved | [API registry](https://github.com/can1357/oh-my-pi/blob/main/packages/ai/src/api-registry.ts) and [streaming](https://github.com/can1357/oh-my-pi/blob/main/packages/ai/src/stream.ts), near line 1409 |
| Custom APIs can delegate to the OpenAI streamer using a request-local model clone | [OpenAI Chat streamer](https://github.com/can1357/oh-my-pi/blob/main/packages/ai/src/providers/openai-completions.ts), near line 1850 |

Inspected npm release: [coding-agent 18.12.6](https://www.npmjs.com/package/@oh-my-pi/pi-coding-agent/v/18.12.6), [pi-ai 18.12.6](https://www.npmjs.com/package/@oh-my-pi/pi-ai/v/18.12.6). SHA-256 hashes of the installed published sources:

```text
coding-agent/src/extensibility/extensions/types.ts cc2cb835d1a4f66657239889ba33c1f9324755b19e8925b0ce59c591ce412d6f
coding-agent/src/sdk.ts                           c0228addd4cfe93cfd370c7248e9db29743f0fa802aea0e1d1adf4994dbe6957
pi-ai/src/stream.ts                               d2dd74a466cf770493e7ad2ce7e7e0b3b19e0cf40c11bf11da6a98829d289bbe
```

## Offline validation

```sh
bun test tools/fusion-eval/omp-fusion-session.test.ts
```

The fixtures use synthetic identities and no network. Eight tests cover parent/retry stability, interleaved worker isolation, independence from prompt-cache keys, rejection of malformed/conflicting headers, endpoint/provider scope, and unchanged tool-context delegation. The checks passed on Bun 1.4.4. The extension also passed TypeScript strict checking against the installed 18.12.6 published type declarations.

Real OMP 18.12.6 CLI-to-stub requests are captured in [the extension fixture](fixtures/omp-18.12.6-extension.jsonl): a fresh conversation's read/tool-result cycle, `--continue` on that conversation, and a second fresh read/tool-result cycle all exited successfully. The first three requests share the session-header fingerprint; the new conversation has a distinct fingerprint. No third-party package was patched to obtain these requests.

This confirms transport behavior only: it does not contact the Devin upstream or establish model quality, cancellation recovery, long-conversation stability, compact/fork behavior, actual concurrent workers, or production quota/cost behavior. Freeze the user's actual OMP version, extension hash, selected role models, server revision, and protocol before applying these results to a personal deployment.
