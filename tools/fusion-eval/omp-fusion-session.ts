/** OMP 18.12.6 public extension API; opt-in Sub2API Chat provider. */
import type { Api, Context, Model, SimpleStreamOptions } from "@oh-my-pi/pi-ai";
import type { ExtensionAPI, ProviderConfig } from "@oh-my-pi/pi-coding-agent";

export const FUSION_PROVIDER = "sub2api-fusion";
export const FUSION_API = "sub2api-fusion-openai-completions";
export const SESSION_HEADER = "x-litellm-session-id";

type Env = Record<string, string | undefined>;
type StreamSimple = NonNullable<ProviderConfig["streamSimple"]>;

/** Keep endpoint selection explicit; loading this extension never changes other providers. */
export function fusionConfig(env: Env): Omit<ProviderConfig, "streamSimple"> {
	const endpoint = env.SUB2API_FUSION_BASE_URL;
	const model = env.SUB2API_FUSION_MODEL;
	if (!endpoint || !model) {
		throw new Error("Set SUB2API_FUSION_BASE_URL and SUB2API_FUSION_MODEL before loading omp-fusion-session.ts");
	}
	const url = new URL(endpoint);
	if (url.username || url.password || url.search || url.hash) {
		throw new Error("Fusion base URL must not contain credentials, query parameters, or a fragment");
	}
	const loopback = ["127.0.0.1", "localhost", "[::1]"].includes(url.hostname);
	if (loopback ? !["http:", "https:"].includes(url.protocol) :
		url.protocol !== "https:" || env.SUB2API_FUSION_ENDPOINT_KIND !== "sub2api") {
		throw new Error("Use a loopback probe, or explicitly identify an HTTPS Sub2API endpoint with SUB2API_FUSION_ENDPOINT_KIND=sub2api");
	}
	if (!env.SUB2API_FUSION_API_KEY) {
		throw new Error("Set SUB2API_FUSION_API_KEY (use a dummy value for the loopback probe)");
	}
	return {
		baseUrl: url.toString().replace(/\/$/, ""),
		apiKey: "SUB2API_FUSION_API_KEY",
		authHeader: true,
		api: FUSION_API,
		models: [{
			id: model,
			name: "Sub2API Fusion (session adapter)",
			reasoning: false,
			input: ["text"],
			// Catalog hints only; configure actual billing/limits on Sub2API.
			cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
			contextWindow: 32768,
			maxTokens: 4096,
		}],
	};
}

function runtimeSessionID(sessionId: string | undefined): string {
	if (!sessionId || sessionId.trim() !== sessionId || /[\p{Cc}\p{Cs}]/u.test(sessionId) ||
		new TextEncoder().encode(sessionId).length > 255) {
		throw new Error("Fusion requires OMP's non-empty runtime session ID, at most 255 UTF-8 bytes without control characters");
	}
	return sessionId;
}

/** Request-local state is essential: concurrent workers share the custom API registry. */
export function fusionSessionOptions(
	model: Pick<Model<Api>, "headers">,
	options?: SimpleStreamOptions,
): SimpleStreamOptions {
	const sessionId = runtimeSessionID(options?.sessionId);
	const headers: Record<string, string> = {};
	for (const source of [model.headers, options?.headers]) {
		for (const [name, value] of Object.entries(source ?? {})) {
			const lower = name.toLowerCase();
			if (lower === "x-sub2api-fusion-session") {
				throw new Error("This OMP adapter uses runtime session IDs; remove a static X-Sub2API-Fusion-Session override");
			}
			if ([SESSION_HEADER, "x-session-id", "x-claude-code-session-id", "x-opencode-session"].includes(lower)) {
				if (value !== sessionId) throw new Error("Configured session header conflicts with OMP's runtime session ID");
				// Emit one canonical runtime header; never let casing create duplicates.
				continue;
			}
			headers[name] = value;
		}
	}
	headers[SESSION_HEADER] = sessionId;
	return { ...options, headers };
}

/** Public custom-provider hook, delegating all tool/stream serialization to OMP. */
export function fusionStream(delegate: StreamSimple): StreamSimple {
	return (model: Model<Api>, context: Context, options?: SimpleStreamOptions) => {
		if (model.provider !== FUSION_PROVIDER || model.api !== FUSION_API) {
			throw new Error("Fusion adapter called for an unrelated provider or API");
		}
		const sessionOptions = fusionSessionOptions(model, options);
		// Native streaming resolves OpenAI compatibility policy from this local clone.
		// Do not replace a built-in API registration or mutate the registry's model.
		return delegate({ ...model, api: "openai-completions", headers: sessionOptions.headers }, context, sessionOptions);
	};
}

export default async function registerFusionSessionProvider(pi: ExtensionAPI): Promise<void> {
	const config = fusionConfig(process.env);
	const { streamSimple } = await import("@oh-my-pi/pi-ai");
	pi.registerProvider(FUSION_PROVIDER, { ...config, streamSimple: fusionStream(streamSimple) });
}
