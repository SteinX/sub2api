#!/usr/bin/env python3
"""Run pinned real clients against an isolated loopback gateway.

Configuration formats and version pins reuse PR #17 at f2de531383945df2fb0b456ab9c417ee013bdebb.
Only structural evidence leaves this process; raw client output stays in memory.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import threading
import time
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parent
PINS = {"opencode-ai": "1.19.4", "@opencode-ai/plugin": "1.19.4", "oh-my-opencode": "5.1.44", "@oh-my-pi/pi-coding-agent": "18.12.6", "@oven/bun-linux-x64": "1.4.4"}
SENSITIVE_ENV = {"DEVIN_E2E_PAT", "DEVIN_E2E_CREDENTIAL", "DEVIN_PAT", "DEVIN_API_KEY", "SUB2API_API_KEY", "SUB2API_USER_TOKEN",
                 "DEVIN_E2E_DB_PASSWORD", "DEVIN_E2E_REDIS_PASSWORD", "DEVIN_E2E_PG_PORT", "DEVIN_E2E_REDIS_PORT", "DEVIN_JOURNEY_GATEWAY_KEY"}


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n")
    path.chmod(0o600)


def configuration(directory, client, endpoint, model):
    url = urlsplit(endpoint)
    if url.scheme != "http" or url.hostname != "127.0.0.1" or not url.port or url.path != "/v1" or url.username or url.query or url.fragment:
        raise ValueError("only the disposable loopback gateway is supported")
    for package, version in PINS.items():
        if json.loads((ROOT / "node_modules" / package / "package.json").read_text())["version"] != version:
            raise ValueError("installed client differs from the pinned version")
    for name in ("config/opencode", "data", "cache", "state", "claude", "omp-agent", "omp-root", "workspace"):
        (directory / name).mkdir(parents=True, exist_ok=True)
    (directory / "workspace/probe.txt").write_text("E2E_TOOL_RESULT\n")
    ref = "sub2api-e2e/" + model
    if client in {"opencode", "omo"}:
        config = {
            "model": ref, "small_model": ref,
            "provider": {"sub2api-e2e": {"npm": "@ai-sdk/openai-compatible", "options": {"baseURL": endpoint, "apiKey": "{env:DEVIN_E2E_GATEWAY_KEY}"},
                "models": {model: {"name": "Isolated Devin E2E", "limit": {"context": 32768, "output": 1024}}}}},
            "agent": {"probe": {"mode": "primary", "model": ref,
                "prompt": "Follow the short test instruction. Only read probe.txt when requested. Never spawn client-side worker agents, edit or execute commands.",
                "tools": {name: name == "read" for name in ("read", "bash", "edit", "write", "task", "glob", "grep", "webfetch", "websearch", "todowrite", "question")}}},
            "permission": {"*": "deny", "read": "allow", "external_directory": {str(directory / "workspace") + "/*": "allow"}},
        }
        if client == "omo":
            config["plugin"] = [(ROOT / "node_modules/oh-my-opencode/dist/index.js").as_uri()]
            write_json(directory / "workspace/.omo/omo.jsonc", {"model_fallback": False, "runtime_fallback": False, "disabled_mcps": ["websearch", "context7", "grep_app"]})
        write_json(directory / "config/opencode/opencode.json", config)
    else:
        # JSON is also YAML 1.2; no templating library or secret substitution needed.
        write_json(directory / "omp-agent/models.yml", {"providers": {"sub2api-e2e": {"api": "openai-completions", "baseUrl": endpoint,
            "apiKey": "DEVIN_E2E_GATEWAY_KEY", "models": [{"id": model, "name": "Isolated Devin E2E", "reasoning": False, "input": ["text"],
                "contextWindow": 32768, "maxTokens": 1024, "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
                "compat": {"supportsStore": False, "supportsDeveloperRole": False}}]}}})
        write_json(directory / "omp-agent/config.yml", {"modelRoles": {role: ref for role in ("default", "smol", "slow", "vision", "plan", "commit", "tiny", "memory", "task", "advisor", "widget")},
            "memory": {"backend": "off"}, "compaction": {"enabled": False, "supersedeReads": False, "dropUseless": False}, "retry": {"enabled": False}})


def child_environment(directory):
    env = {key: value for key, value in os.environ.items() if key not in SENSITIVE_ENV}
    # Preserve the platform proxy, trust and guard variables; never change HOME.
    env.update({"XDG_CONFIG_HOME": str(directory / "config"), "XDG_DATA_HOME": str(directory / "data"),
        "XDG_CACHE_HOME": str(directory / "cache"), "XDG_STATE_HOME": str(directory / "state"),
        "CLAUDE_CONFIG_DIR": str(directory / "claude"), "PI_CODING_AGENT_DIR": str(directory / "omp-agent"),
        "PI_CONFIG_DIR": os.path.relpath(directory / "omp-root", Path.home()), "OPENCODE_DISABLE_AUTOUPDATE": "true",
        "npm_config_cache": str(directory / "cache/npm"), "PWD": str(directory / "workspace")})
    return env


def command(client, model, action, prompt):
    ref = "sub2api-e2e/" + model
    if client in {"opencode", "omo"}:
        cmd = [str(ROOT / "node_modules/opencode-linux-x64/bin/opencode"), "run", "--agent", "probe", "--model", ref, "--format", "json", "--title", "Devin E2E"]
        if client == "opencode":
            cmd += ["--pure"]
    else:
        cmd = [str(ROOT / "node_modules/@oven/bun-linux-x64/bin/bun"), str(ROOT / "node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js"),
            "--model", ref, "--tools", "read", "--no-lsp", "--no-pty", "--no-extensions", "--no-skills", "--no-rules", "--no-title", "--thinking", "off", "--max-time", "60", "--mode", "json", "-p"]
    if action == "continue":
        cmd += ["--continue"]
    return cmd + [prompt]


def execute(args):
    directory = Path(args.directory).resolve()
    configuration(directory, args.client, args.endpoint, args.model)
    if not os.environ.get("DEVIN_E2E_GATEWAY_KEY"):
        raise ValueError("ephemeral gateway key unavailable")
    if args.mode == "live" and os.environ.get("DEVIN_E2E_ALLOW_PAID") != "true":
        raise ValueError("paid generation disabled")
    if args.client == "omo" and os.access(Path.home(), os.W_OK) and os.environ.get("GITHUB_ACTIONS") != "true":
        raise ValueError("OMO requires a disposable CI user or a read-only user directory; its startup migration is not isolated by XDG")
    prompt = "Reply with E2E_OK only. Do not call tools."
    if args.action == "tool":
        prompt = "Read the local probe.txt with the read tool, then reply E2E_OK only after the read has completed."
    if args.model == "fusion-e2e-agents":
        prompt = prompt.replace("Do not call tools.", "Do not call client-side tools for this text-only check.")
        prompt = "Use gateway-internal Lead/Sidekick delegation: have the Sidekick independently verify this task, then have the Lead review the report before the final answer. " + prompt
    result = {"client": args.client, "action": args.action, "saw_expected_text": False, "tool_completed": 0, "frames": 0, "parse_errors": 0, "timed_out": False, "interrupted_before_forward": False, "event_types": {}, "tool_statuses": {}}
    result.update(tier="client_smoke", eligible_for_real_e2e=False, configuration_source="legacy_runner_template", normal_stop=False, terminal_error=False)
    proc = subprocess.Popen(command(args.client, args.model, args.action, prompt), cwd=directory / "workspace", env=child_environment(directory),
        stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True)
    byte_count = 0
    assistant_tail = ""

    def consume():
        nonlocal byte_count, assistant_tail
        for raw in iter(lambda: proc.stdout.readline(2 * 1024 * 1024 + 1), b''):
            byte_count += len(raw)
            if byte_count > 8 * 1024 * 1024 or len(raw) > 2 * 1024 * 1024:
                os.killpg(proc.pid, signal.SIGKILL)
                return
            try:
                event = json.loads(raw)
            except (ValueError, UnicodeDecodeError):
                result["parse_errors"] += 1
                continue
            result["frames"] += 1
            kind = event.get("type")
            if kind == "step_start":
                assistant_tail = ""
            if kind == "error":
                result["terminal_error"] = True
            if kind == "step_finish":
                result["normal_stop"] = event.get("part", {}).get("reason") in {"stop", "end_turn"}
            kind = kind if kind in {"text", "tool_use", "step_start", "step_finish", "error", "tool_execution_start", "tool_execution_end", "message_start", "message_update", "message_end", "agent_start", "agent_end", "turn_start", "turn_end", "session"} else "other"
            result["event_types"][kind] = result["event_types"].get(kind, 0) + 1
            # Inspect in memory, retain only booleans and counts. No text/thinking/tool arguments.
            assistant_text = ""
            if event.get("type") == "text":
                assistant_text = event.get("part", {}).get("text", "")
            message = event.get("message", {})
            if message.get("role") == "assistant" and event.get("type") == "message_end":
                assistant_tail = ""
                assistant_text += "".join(part.get("text", "") for part in message.get("content", []) if isinstance(part, dict) and part.get("type") == "text")
                result["normal_stop"] = message.get("stopReason") in {"stop", "end_turn"}
                result["terminal_error"] |= message.get("stopReason") in {"error", "aborted"}
            delta = event.get("assistantMessageEvent", {})
            if delta.get("type") == "text_delta":
                assistant_text += delta.get("delta", "")
            assistant_tail = (assistant_tail + assistant_text)[-256:]
            part = event.get("part", {})
            if kind == "tool_use":
                status = part.get("state", {}).get("status", "other")
                status = status if status in {"completed", "error", "pending", "running"} else "other"
                result["tool_statuses"][status] = result["tool_statuses"].get(status, 0) + 1
            if event.get("type") == "tool_use" and part.get("tool") == "read" and part.get("state", {}).get("status") == "completed":
                result["tool_completed"] += 1
            if event.get("type") == "tool_execution_end" and event.get("toolName") in {"read", "read_file"} and not event.get("isError"):
                result["tool_completed"] += 1

    reader = threading.Thread(target=consume, daemon=True)
    def stop_on_signal(number, _frame):
        if proc.poll() is None:
            os.killpg(proc.pid, signal.SIGTERM)
            try:
                proc.wait(timeout=3)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
        raise SystemExit(128 + number)

    old_term = signal.signal(signal.SIGTERM, stop_on_signal)
    reader.start()
    deadline = time.monotonic() + 75
    try:
        while proc.poll() is None:
            if args.interrupt_file and Path(args.interrupt_file).exists():
                result["interrupted_before_forward"] = True
                os.killpg(proc.pid, signal.SIGINT)
                try:
                    proc.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    os.killpg(proc.pid, signal.SIGKILL)
                break
            if time.monotonic() > deadline:
                result["timed_out"] = True
                os.killpg(proc.pid, signal.SIGKILL)
                break
            time.sleep(0.05)
        proc.wait(timeout=5)
    finally:
        if proc.poll() is None:
            os.killpg(proc.pid, signal.SIGKILL)
            proc.wait()
        reader.join(timeout=5)
        proc.stdout.close()
        signal.signal(signal.SIGTERM, old_term)
    result["exit_code"] = proc.returncode
    result["saw_expected_text"] = assistant_tail.strip() == "E2E_OK"
    result["task_completed"] = bool(proc.returncode == 0 and result["saw_expected_text"] and result["normal_stop"] and not result["terminal_error"] and not result["parse_errors"] and not result["timed_out"] and (args.action != "tool" or result["tool_completed"] > 0))
    result["success"] = bool(result["interrupted_before_forward"] or result["task_completed"])
    print(json.dumps(result, sort_keys=True))
    return 0 if result["success"] else 1


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--directory", required=True)
    parser.add_argument("--client", choices=("opencode", "omo", "omp"), required=True)
    parser.add_argument("--endpoint", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--mode", choices=("mock", "live"), required=True)
    parser.add_argument("--action", choices=("text", "tool", "continue"), required=True)
    parser.add_argument("--interrupt-file")
    return execute(parser.parse_args())


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (ValueError, OSError, KeyError):
        # Exception strings can include configuration or provider values.
        print(json.dumps({"success": False, "setup_error": True}))
        raise SystemExit(2)
