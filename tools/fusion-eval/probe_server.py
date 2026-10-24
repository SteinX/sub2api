#!/usr/bin/env python3
"""Loopback-only, deterministic Chat API stub for capturing client contracts.

No model is contacted. Logs contain structural metadata and hashed session IDs;
request bodies, authorization headers, prompts, and tool arguments are not saved.
"""

import argparse
import hashlib
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import threading
from urllib.parse import urlsplit


SESSION_HEADERS = (
    "x-sub2api-fusion-session", "x-session-id", "x-opencode-session",
    "x-opencode-session-id", "x-session-affinity", "x-claude-code-session-id",
    "x-litellm-session-id", "idempotency-key",
)
BODY_LIMIT = 2 * 1024 * 1024


def digest(value):
    """Return an opaque fingerprint without retaining the input."""
    raw = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(raw).hexdigest()


def summarize(body, headers, index):
    """Keep only the contract fields needed for versioned compatibility checks."""
    tools = body.get("tools", [])
    return {
        "request_index": index,
        "model": body.get("model"),
        "stream": body.get("stream", False),
        "message_roles": [m.get("role") for m in body.get("messages", [])],
        "tool_names": [t.get("function", {}).get("name") for t in tools],
        "tool_shapes": [{
            "name": t.get("function", {}).get("name"),
            "parameter_keys": sorted(t.get("function", {}).get("parameters", {})),
            "property_names": sorted(t.get("function", {}).get("parameters", {}).get("properties", {})),
            "required": t.get("function", {}).get("parameters", {}).get("required", []),
        } for t in tools],
        "tool_schema_digest": digest(tools),
        "system_instructions_digest": digest({
            "instructions": body.get("instructions"),
            "system": body.get("system"),
            "messages": [{"role": m.get("role"), "content": m.get("content")}
                         for m in body.get("messages", []) if m.get("role") in {"system", "developer"}],
        }),
        "tool_choice_digest": digest(body.get("tool_choice", "auto")),
        "header_names": sorted(name.lower() for name in headers),
        "session_headers": {name: digest(headers[name]) for name in SESSION_HEADERS if headers.get(name)},
        "has_store": "store" in body,
        "has_previous_response_id": bool(body.get("previous_response_id")),
    }


def probe_tool(body):
    """Request only a read of the synthetic probe fixture, if declared."""
    if any(m.get("role") == "tool" for m in body.get("messages", [])):
        return None
    for tool in body.get("tools", []):
        fn = tool.get("function", {})
        if fn.get("name", "").lower() not in {"read", "read_file"}:
            continue
        schema = fn.get("parameters", {})
        properties = schema.get("properties", {})
        path_key = next((key for key in ("filePath", "path", "file_path") if key in properties), None)
        if not path_key or any(key not in {path_key, "i"} for key in schema.get("required", [])):
            continue
        arguments = {path_key: str(Path.cwd() / "probe.txt")}
        if "i" in schema.get("required", []):
            # OMP 18 injects a required concise-intent field into tool schemas.
            if properties.get("i", {}).get("type") != "string":
                continue
            arguments["i"] = "read synthetic probe fixture"
        return {"id": "call_local_probe_read", "type": "function", "function": {
            "name": fn["name"], "arguments": json.dumps(arguments),
        }}
    return None


def make_handler(output, max_requests):
    """Build a handler whose shared request counter and log are synchronized."""
    lock = threading.Lock()
    count = 0

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def send_json(self, status, body):
            encoded = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def do_GET(self):
            if urlsplit(self.path).path not in {"/models", "/v1/models"}:
                self.send_json(404, {"error": {"message": "Unknown local probe route"}})
                return
            self.send_json(200, {"object": "list", "data": [
                {"id": "fusion-probe", "object": "model", "owned_by": "local-probe"},
            ]})

        def do_POST(self):
            nonlocal count
            if urlsplit(self.path).path not in {"/chat/completions", "/v1/chat/completions"}:
                self.send_json(404, {"error": {"message": "Local probe supports Chat Completions only"}})
                return
            try:
                size = int(self.headers.get("Content-Length", "0"))
            except ValueError:
                size = -1
            if not 0 < size <= BODY_LIMIT:
                self.send_json(413, {"error": {"message": "Invalid local probe body size"}})
                return
            try:
                body = json.loads(self.rfile.read(size))
                if not isinstance(body, dict):
                    raise ValueError("object required")
            except (ValueError, UnicodeDecodeError):
                self.send_json(400, {"error": {"message": "Invalid JSON object"}})
                return
            with lock:
                if count >= max_requests:
                    self.send_json(429, {"error": {"message": "Local probe request limit reached"}})
                    return
                count += 1
                index = count
                with output.open("a", encoding="utf-8") as stream:
                    stream.write(json.dumps(summarize(body, self.headers, index), ensure_ascii=False) + "\n")
            call = probe_tool(body)
            usage = {"prompt_tokens": 4, "completion_tokens": 4, "total_tokens": 8}
            base = {"id": f"chatcmpl-local-{index}", "created": 1, "model": body.get("model", "fusion-probe")}
            finish = "tool_calls" if call else "stop"
            if not body.get("stream"):
                message = {"role": "assistant", "content": None, "tool_calls": [call]} if call else {
                    "role": "assistant", "content": "LOCAL_PROBE_COMPLETE",
                }
                self.send_json(200, dict(base, object="chat.completion", choices=[
                    {"index": 0, "message": message, "finish_reason": finish},
                ], usage=usage))
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()
            delta = {"role": "assistant", "tool_calls": [dict(call, index=0)]} if call else {
                "role": "assistant", "content": "LOCAL_PROBE_COMPLETE",
            }
            chunks = [dict(base, object="chat.completion.chunk", choices=[
                {"index": 0, "delta": delta, "finish_reason": None},
            ]), dict(base, object="chat.completion.chunk", choices=[
                {"index": 0, "delta": {}, "finish_reason": finish},
            ]), dict(base, object="chat.completion.chunk", choices=[], usage=usage)]
            for chunk in chunks:
                self.wfile.write(b"data: " + json.dumps(chunk).encode() + b"\n\n")
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()

    return Handler


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=8765)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--max-requests", type=int, default=64)
    args = parser.parse_args()
    if not 1 <= args.max_requests <= 1000:
        parser.error("max-requests must be between 1 and 1000")
    args.output.parent.mkdir(parents=True, exist_ok=True)
    server = ThreadingHTTPServer(("127.0.0.1", args.port), make_handler(args.output, args.max_requests))
    print(f"Local probe only: http://127.0.0.1:{server.server_port}/v1", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
