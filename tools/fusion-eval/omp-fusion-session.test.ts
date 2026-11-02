import { describe, expect, test } from "bun:test";
import fixtures from "./omp-fusion-session.fixtures.json";
import {
	FUSION_API, FUSION_PROVIDER, SESSION_HEADER, fusionConfig, fusionSessionOptions, fusionStream,
} from "./omp-fusion-session";

const env = {
	SUB2API_FUSION_BASE_URL: "http://127.0.0.1:8767/v1",
	SUB2API_FUSION_MODEL: "fusion-probe",
	SUB2API_FUSION_API_KEY: "dummy-loopback-only",
};

describe("explicit provider and endpoint scope", () => {
	test("registers a distinct custom Chat API and the requested model", () => {
		const config = fusionConfig(env);
		expect(config.api).toBe(FUSION_API);
		expect(config.models?.map(model => model.id)).toEqual(["fusion-probe"]);
		expect(config.apiKey).toBe("SUB2API_FUSION_API_KEY");
		expect(config.baseUrl).toBe(env.SUB2API_FUSION_BASE_URL);
	});

	test("requires explicit HTTPS Sub2API identification outside loopback", () => {
		for (const endpoint of ["https://example.invalid/v1", "http://example.invalid/v1", "file:///tmp/probe"]) {
			expect(() => fusionConfig({ ...env, SUB2API_FUSION_BASE_URL: endpoint })).toThrow();
		}
		expect(fusionConfig({ ...env, SUB2API_FUSION_BASE_URL: "https://example.invalid/v1", SUB2API_FUSION_ENDPOINT_KIND: "sub2api" }).baseUrl)
			.toBe("https://example.invalid/v1");
		expect(() => fusionConfig({ ...env, SUB2API_FUSION_BASE_URL: "http://example.invalid/v1", SUB2API_FUSION_ENDPOINT_KIND: "sub2api" })).toThrow();
	});

	test("fails early on incomplete config and credential-bearing URLs", () => {
		for (const key of Object.keys(env)) expect(() => fusionConfig({ ...env, [key]: undefined })).toThrow();
		for (const suffix of ["?api_key=do-not-log", "#fragment"]) {
			expect(() => fusionConfig({ ...env, SUB2API_FUSION_BASE_URL: env.SUB2API_FUSION_BASE_URL + suffix })).toThrow();
		}
		expect(() => fusionConfig({ ...env, SUB2API_FUSION_BASE_URL: "http://user:pass@localhost/v1" })).toThrow();
	});
});

describe("request-local runtime identity", () => {
	test("keeps retries stable and interleaved parent/workers distinct regardless of cache keys", () => {
		for (const fixture of fixtures) {
			const options = fusionSessionOptions({}, fixture);
			expect(options.headers?.[SESSION_HEADER]).toBe(fixture.expectedSessionId);
			expect(options.sessionId).toBe(fixture.sessionId);
		}
	});

	test("never mutates caller headers and emits one canonical identity header", () => {
		const model = { headers: { "X-Litellm-Session-Id": "parent", "X-Model": "model" } };
		const options = { sessionId: "parent", headers: { "X-Session-Id": "parent", "X-Request": "request" } };
		const result = fusionSessionOptions(model, options);
		expect(result.headers).toEqual({ "X-Model": "model", "X-Request": "request", [SESSION_HEADER]: "parent" });
		expect(model.headers["X-Litellm-Session-Id"]).toBe("parent");
		expect(options.headers["X-Session-Id"]).toBe("parent");
	});

	test("rejects missing, overlong, control and invalid Unicode identities without inventing an epoch", () => {
		for (const sessionId of [undefined, "", " parent", "parent ", "a\nb", "a\u0000b", "a\u007fb", "\ud800", "a".repeat(256), "中".repeat(86)]) {
			expect(() => fusionSessionOptions({}, { sessionId })).toThrow();
		}
		expect(fusionSessionOptions({}, { sessionId: "a".repeat(255) }).headers?.[SESSION_HEADER]).toHaveLength(255);
	});

	test("rejects static or conflicting identity overrides", () => {
		for (const name of ["X-Litellm-Session-Id", "x-session-id", "x-claude-code-session-id", "x-opencode-session"]) {
			expect(() => fusionSessionOptions({ headers: { [name]: "other" } }, { sessionId: "parent" })).toThrow();
			expect(() => fusionSessionOptions({}, { sessionId: "parent", headers: { [name]: "other" } })).toThrow();
		}
		expect(() => fusionSessionOptions({}, { sessionId: "parent", headers: { "X-Sub2API-Fusion-Session": "static" } })).toThrow();
	});

	test("delegates unchanged tool context using a local native-API model clone", () => {
		const context = { messages: [], tools: [{ name: "fixture-read" }] };
		const model = { provider: FUSION_PROVIDER, api: FUSION_API, id: "fusion-probe" };
		const calls: unknown[][] = [];
		const sentinel = {};
		const delegate = ((...args: unknown[]) => { calls.push(args); return sentinel; }) as Parameters<typeof fusionStream>[0];
		const stream = fusionStream(delegate);
		expect(stream(model as never, context as never, { sessionId: "worker-a" })).toBe(sentinel as never);
		expect(calls[0][0]).toEqual({ ...model, api: "openai-completions", headers: { [SESSION_HEADER]: "worker-a" } });
		expect(calls[0][1]).toBe(context);
		expect(model.api).toBe(FUSION_API);
		expect(() => stream({ ...model, provider: "unrelated" } as never, context as never, { sessionId: "parent" })).toThrow();
		expect(() => stream({ ...model, api: "openai-completions" } as never, context as never, { sessionId: "parent" })).toThrow();
		expect(calls).toHaveLength(1);
	});
});
