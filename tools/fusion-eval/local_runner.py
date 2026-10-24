#!/usr/bin/env python3
"""Prepare and run one isolated, explicitly requested local Fusion smoke.

prepare and inspect never contact a model or a gateway. run executes the local
client and can incur real charges. It never retries a started run. The reserved
budget is a conservative local hold, not a provider spending cap; settlement is
always partial because the user API cannot reveal unknown/inflight attempts.
"""

import argparse
from contextlib import contextmanager
from datetime import datetime, timezone
from decimal import Decimal, InvalidOperation
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import sys
import time
from urllib.parse import urlsplit
import uuid


ROOT = Path(__file__).resolve().parent
CLIENT_VERSIONS = {"omo": "1.19.4", "omp": "18.12.6"}
CASES = ("short-repair", "tool-roundtrip")
MAX_EVENT_BYTES = 256 * 1024
MAX_EVENTS = 2000
TOOL_NAMES = {"task", "delegate_task", "read", "read_file", "write", "write_file", "edit", "bash",
              "apply_patch", "glob", "grep", "ls", "find", "call_omo_agent", "background_task"}


class RunnerError(ValueError):
    """A bounded machine-readable failure; never include secret API bodies."""


def digest(value):
    return hashlib.sha256(str(value).encode("utf-8")).hexdigest()


def now():
    return datetime.now(timezone.utc).isoformat()


def positive_money(value):
    try:
        amount = Decimal(str(value))
    except InvalidOperation as error:
        raise RunnerError("invalid_budget") from error
    if not amount.is_finite() or amount <= 0:
        raise RunnerError("invalid_budget")
    return format(amount, "f")


def safe_path(value, *, must_exist=False):
    """Resolve a local absolute path without following existing symlinks."""
    path = Path(os.path.abspath(os.fspath(value)))
    for part in [path, *path.parents]:
        if part.is_symlink():
            raise RunnerError("symlink_path_rejected")
    if must_exist and not path.exists():
        raise RunnerError("path_missing")
    return path


def read_json(path):
    try:
        return json.loads(safe_path(path, must_exist=True).read_text(encoding="utf-8"))
    except (UnicodeError, json.JSONDecodeError) as error:
        raise RunnerError("invalid_json_file") from error


def write_new_json(path, value):
    """Persist immutable evidence before returning or starting a model call."""
    path = safe_path(path)
    raw = json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n"
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    flags |= getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    parent = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(parent)
    finally:
        os.close(parent)


def validate_endpoint(value):
    if not isinstance(value, str) or any(ord(char) <= 32 or char in {'"', '\\'} for char in value):
        raise RunnerError("invalid_endpoint")
    parsed = urlsplit(value)
    loopback = parsed.hostname in {"localhost", "127.0.0.1", "::1"}
    if (not parsed.hostname or parsed.username or parsed.password or parsed.query
            or parsed.fragment or parsed.scheme not in ({"https", "http"} if loopback else {"https"})):
        raise RunnerError("invalid_endpoint")
    if not parsed.path.rstrip("/").endswith("/v1"):
        raise RunnerError("endpoint_must_end_in_v1")
    return value.rstrip("/")


def validate_cli(value):
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except json.JSONDecodeError as error:
            raise RunnerError("invalid_cli_json") from error
    if (not isinstance(value, list) or not 1 <= len(value) <= 2
            or any(not isinstance(part, str) or not part or "\x00" in part
                   or part.startswith("-") or "://" in part or "=" in part for part in value)):
        raise RunnerError("cli_must_be_executable_and_optional_script")
    if Path(value[0]).name in {"sh", "bash", "zsh", "fish", "cmd", "powershell"}:
        raise RunnerError("shell_cli_rejected")
    # Homebrew/npm launchers commonly are symlinks. Resolve executable inputs;
    # the stricter no-symlink rule applies to files this runner creates/writes.
    try:
        return [str(Path(part).resolve(strict=True)) if "/" in part else part for part in value]
    except OSError as error:
        raise RunnerError("cli_path_missing") from error


def model_id(value):
    if not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9_.:+-]{1,256}", value):
        raise RunnerError("invalid_model_id")
    return value


def smoke_module():
    spec = importlib.util.spec_from_file_location("fusion_smoke_cases", ROOT / "cases/smoke/smoke_cases.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def config_hashes(directory):
    result = {}
    roots = [directory / "config", directory / "omp-agent", directory / "workspace/.omo", directory / "workspace/.omp/agents"]
    for root in roots:
        safe_path(root)
        if root.exists():
            for path in sorted(root.rglob("*")):
                if path.is_symlink():
                    raise RunnerError("symlink_config_rejected")
                if path.is_file():
                    result[str(path.relative_to(directory))] = hashlib.sha256(path.read_bytes()).hexdigest()
    return result


def prepare(directory, client, case, base_url, direct_model, fusion_model,
            api_key_id, budget_usd, cli=None, timeout_seconds=180):
    if client not in CLIENT_VERSIONS or case not in CASES:
        raise RunnerError("invalid_client_or_case")
    directory = safe_path(directory)
    if directory.exists() or not directory.parent.is_dir():
        raise RunnerError("new_run_directory_required")
    base_url = validate_endpoint(base_url)
    direct_model, fusion_model = model_id(direct_model), model_id(fusion_model)
    if direct_model.lower().startswith("fusion") or not fusion_model.lower().startswith("fusion-"):
        raise RunnerError("explicit_direct_and_fusion_models_required")
    if type(api_key_id) is not int or api_key_id <= 0:
        raise RunnerError("invalid_api_key_id")
    if not isinstance(timeout_seconds, (int, float)) or not 1 <= timeout_seconds <= 1800:
        raise RunnerError("invalid_timeout")
    cli = validate_cli(cli or (["opencode"] if client == "omo" else ["omp"]))
    budget_usd = positive_money(budget_usd)
    directory.mkdir(mode=0o700)
    for name in ("config/opencode", "data", "state", "cache", "claude", "omp-agent", "omp-root"):
        (directory / name).mkdir(parents=True, exist_ok=True, mode=0o700)
    smoke_module().initialize(case, directory / "workspace")
    templates = ROOT / "templates/personal"
    replacements = {"REPLACE_WITH_CONCRETE_MODEL_UID": direct_model,
                    "https://YOUR-SUB2API.invalid/v1": base_url,
                    "fusion-personal-worker": fusion_model}

    def template(name):
        raw = (templates / name).read_text(encoding="utf-8")
        for original, replacement in replacements.items():
            raw = raw.replace(original, replacement)
        return raw

    if client == "omo":
        (directory / "config/opencode/opencode.json").write_text(template("opencode.example.json"), encoding="utf-8")
        (directory / "workspace/.omo").mkdir()
        (directory / "workspace/.omo/omo.jsonc").write_text(template("omo.example.jsonc"), encoding="utf-8")
    else:
        (directory / "omp-agent/config.yml").write_text(template("omp-config.example.yml"), encoding="utf-8")
        (directory / "omp-agent/models.yml").write_text(template("omp-models.example.yml"), encoding="utf-8")
        (directory / "workspace/.omp/agents").mkdir(parents=True)
        (directory / "workspace/.omp/agents/fusion-worker.md").write_text(template("fusion-worker.md"), encoding="utf-8")
    metadata = {
        "schema_version": 1, "run_id": uuid.uuid4().hex, "prepared_at": now(),
        "client": client, "case_id": case, "base_url": base_url,
        "direct_model": direct_model, "fusion_model": fusion_model,
        "api_key_id": api_key_id, "budget_usd": budget_usd,
        "cli": cli, "expected_client_version": CLIENT_VERSIONS[client],
        "timeout_seconds": timeout_seconds, "config_hashes": config_hashes(directory),
        "adapter_sha256": hashlib.sha256((ROOT / "omp-fusion-session.ts").read_bytes()).hexdigest(),
        "budget_scope": "local_unknown_reserve_not_provider_cap",
    }
    write_new_json(directory / "run.json", metadata)
    return {"prepared": True, "directory": str(directory), "run_id": metadata["run_id"],
            "network_used": False, "next_command": [sys.executable, str(ROOT / "local_runner.py"), "run", str(directory)]}


def load_run(directory):
    directory = safe_path(directory, must_exist=True)
    metadata = read_json(directory / "run.json")
    required = {"schema_version", "run_id", "prepared_at", "client", "case_id", "base_url", "direct_model", "fusion_model",
                "api_key_id", "budget_usd", "cli", "expected_client_version", "timeout_seconds", "config_hashes", "adapter_sha256", "budget_scope"}
    if not isinstance(metadata, dict) or set(metadata) != required or metadata.get("schema_version") != 1:
        raise RunnerError("invalid_run_manifest")
    if metadata["client"] not in CLIENT_VERSIONS or metadata["case_id"] not in CASES:
        raise RunnerError("invalid_run_manifest")
    if metadata["expected_client_version"] != CLIENT_VERSIONS[metadata["client"]]:
        raise RunnerError("invalid_version_pin")
    if not isinstance(metadata["timeout_seconds"], (int, float)) or not 1 <= metadata["timeout_seconds"] <= 1800:
        raise RunnerError("invalid_timeout")
    if type(metadata["api_key_id"]) is not int or metadata["api_key_id"] <= 0:
        raise RunnerError("invalid_api_key_id")
    validate_endpoint(metadata["base_url"])
    model_id(metadata["direct_model"])
    model_id(metadata["fusion_model"])
    validate_cli(metadata["cli"])
    positive_money(metadata["budget_usd"])
    if metadata["config_hashes"] != config_hashes(directory):
        raise RunnerError("prepared_configuration_changed")
    adapter_hash = hashlib.sha256((ROOT / "omp-fusion-session.ts").read_bytes()).hexdigest()
    if adapter_hash != metadata["adapter_sha256"]:
        raise RunnerError("prepared_adapter_changed")
    return directory, metadata


def child_environment(directory, metadata, api_key):
    allowed = {"HOME", "PATH", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "LC_CTYPE", "TERM",
               "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "all_proxy", "no_proxy",
               "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "SYSTEMROOT"}
    result = {key: value for key, value in os.environ.items() if key in allowed}
    result.update({
        "XDG_CONFIG_HOME": str(directory / "config"), "XDG_DATA_HOME": str(directory / "data"),
        "XDG_STATE_HOME": str(directory / "state"), "XDG_CACHE_HOME": str(directory / "cache"),
        "CLAUDE_CONFIG_DIR": str(directory / "claude"), "PI_CODING_AGENT_DIR": str(directory / "omp-agent"),
        "PI_CONFIG_DIR": os.path.relpath(directory / "omp-root", Path.home()),
        "OPENCODE_DISABLE_AUTOUPDATE": "true", "npm_config_cache": str(directory / "cache/npm"),
        "SUB2API_API_KEY": api_key, "SUB2API_FUSION_API_KEY": api_key,
        "SUB2API_FUSION_BASE_URL": metadata["base_url"], "SUB2API_FUSION_MODEL": metadata["fusion_model"],
        "SUB2API_FUSION_ENDPOINT_KIND": "sub2api",
    })
    return result


def client_command(directory, metadata, prompt):
    direct = "sub2api-direct/" + metadata["direct_model"]
    if metadata["client"] == "omo":
        return metadata["cli"] + ["run", "--agent", "Sisyphus - ultraworker", "--model", direct, "--format", "json", prompt]
    return metadata["cli"] + ["--model", direct, "-e", str(ROOT / "omp-fusion-session.ts"), "--mode", "json",
                              "--no-lsp", "--no-pty", "--no-title", "--no-skills", "--no-rules", "--thinking", "off",
                              "--max-time", str(metadata["timeout_seconds"]), "-p", prompt]


class EventEvidence:
    """Retain only structural event facts, never text/tool args/results/thinking."""

    def __init__(self, metadata):
        self.metadata = metadata
        self.events = []
        self.invalid_lines = 0
        self.error_observed = False
        self.truncated = False
        self.sessions = set()
        self.tasks = {}
        self.dispatch_verified = False
        self.parse_errors = 0
        self.frame_count = 0
        self.route_mismatch = False

    def add(self, event):
        if len(self.events) < MAX_EVENTS:
            self.events.append(event)
        else:
            self.truncated = True

    def accept(self, raw):
        self.frame_count += 1
        if self.frame_count > MAX_EVENTS:
            self.truncated = True
            return
        try:
            self._accept(raw)
        except Exception:
            # An unexpected event shape is incomplete evidence, never a reason
            # to dump the raw (possibly secret-bearing) CLI frame.
            self.parse_errors += 1

    def _accept(self, raw):
        try:
            event = json.loads(raw)
        except (ValueError, UnicodeError):
            self.invalid_lines += 1
            return
        if not isinstance(event, dict):
            self.invalid_lines += 1
            return
        kind = event.get("type")
        if kind in {"error", "agent_error"}:
            self.error_observed = True
        session = event.get("sessionID") or (event.get("id") if kind == "session" else None)
        if isinstance(session, str):
            self.sessions.add(digest(session))
        if kind == "tool_use" and isinstance(event.get("part"), dict):
            part = event["part"]
            state = part.get("state") if isinstance(part.get("state"), dict) else {}
            self.tool(part.get("tool"), part.get("callID"), state.get("status"), state.get("input"), state.get("metadata"))
        elif kind in {"tool_execution_start", "tool_execution_end"}:
            self.tool(event.get("toolName"), event.get("toolCallId"),
                      "started" if kind.endswith("start") else ("failed" if event.get("isError") else "completed"),
                      event.get("args"), event.get("result"))
        elif kind == "message_end" and isinstance(event.get("message"), dict):
            message = event["message"]
            if message.get("stopReason") in {"error", "aborted"}:
                self.error_observed = True
            model = message.get("model")
            expected = {self.metadata["direct_model"], self.metadata["fusion_model"]}
            if isinstance(model, str) and model in expected:
                self.add({"kind": "message_model", "model": model})
            elif isinstance(model, str) and message.get("role") == "assistant":
                self.route_mismatch = True
                self.add({"kind": "unexpected_message_model", "model_sha256": digest(model)})

    def tool(self, name, call_id, phase, args, result):
        if not isinstance(name, str) or not re.fullmatch(r"[A-Za-z0-9_.:-]{1,100}", name):
            return
        call_hash = digest(call_id) if isinstance(call_id, str) else None
        entry = {"kind": "tool", "name": name if name in TOOL_NAMES else "other", "phase": phase if isinstance(phase, str) and phase in {"started", "running", "completed", "error", "failed", "pending"} else "unknown"}
        if name not in TOOL_NAMES:
            entry["name_sha256"] = digest(name)
        if call_hash:
            entry["call_id_sha256"] = call_hash
        if name in {"task", "delegate_task"}:
            tasks = args.get("tasks", []) if isinstance(args, dict) else []
            requested = isinstance(args, dict) and (args.get("agent") == "fusion-worker" or args.get("subagent_type") == "fusion-worker"
                                                    or any(isinstance(task, dict) and task.get("agent") == "fusion-worker" for task in (tasks if isinstance(tasks, list) else [])))
            if requested:
                entry["requested_agent"] = "fusion-worker"
                self.tasks[call_hash] = True
            if self.metadata["client"] == "omp" and phase == "completed" and call_hash in self.tasks and isinstance(result, dict):
                details = result.get("details")
                for task in details.get("results", []) if isinstance(details, dict) else []:
                    if not isinstance(task, dict) or task.get("agent") != "fusion-worker" or task.get("exitCode") != 0:
                        continue
                    identity = task.get("resolvedModelIdentity")
                    expected = "sub2api-fusion/" + self.metadata["fusion_model"]
                    requests = task.get("requests")
                    if identity == expected and type(requests) is int and requests > 0 and not task.get("resolvedModelIsFallback"):
                        entry["resolved_model"] = expected
                        self.dispatch_verified = True
        self.add(entry)

    def report(self):
        return {"tool_events": self.events, "session_sha256": sorted(self.sessions),
                "invalid_stdout_lines": self.invalid_lines, "event_limit_reached": self.truncated,
                "event_parse_errors": self.parse_errors,
                "stdout_json_frame_count": self.frame_count,
                "client_error_observed": self.error_observed,
                "model_route_mismatch_observed": self.route_mismatch,
                "dispatch_evidence": "verified_model_identity" if self.dispatch_verified else ("requested_only" if self.tasks else "unknown"),
                "child_tool_trace_complete": False,
                "scope": "CLI structure only; task arguments, outputs, and private reasoning discarded"}


def terminate_group(process):
    for sig, grace in ((signal.SIGTERM, 0.5), (signal.SIGKILL, 0.5)):
        try:
            os.killpg(process.pid, sig)
        except ProcessLookupError:
            return
        try:
            process.wait(timeout=grace)
        except subprocess.TimeoutExpired:
            continue


def stream_client(command, cwd, env, timeout, collector):
    selector = selectors.DefaultSelector()
    try:
        process = subprocess.Popen(command, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   start_new_session=True, bufsize=0)
    except BaseException:
        selector.close()
        raise
    try:
        for stream in (process.stdout, process.stderr):
            os.set_blocking(stream.fileno(), False)
            selector.register(stream, selectors.EVENT_READ)
    except (OSError, ValueError):
        terminate_group(process)
        selector.close()
        process.stdout.close()
        process.stderr.close()
        return {"status": "stream_reader_failed", "exit_code": process.poll()}
    pending = bytearray()
    oversized = False
    deadline = time.monotonic() + timeout
    exit_drain_deadline = None
    status = "exited"

    def consume(chunk):
        nonlocal oversized
        while chunk:
            newline = chunk.find(b"\n")
            piece = chunk if newline < 0 else chunk[:newline]
            if not oversized:
                if len(pending) + len(piece) > MAX_EVENT_BYTES:
                    pending.clear()
                    oversized = True
                    collector.invalid_lines += 1
                else:
                    pending.extend(piece)
            if newline < 0:
                return
            if not oversized:
                collector.accept(bytes(pending))
            pending.clear()
            oversized = False
            chunk = chunk[newline + 1:]

    try:
        while selector.get_map() or process.poll() is None:
            current = time.monotonic()
            if current >= deadline:
                status = "timeout"
                terminate_group(process)
                break
            if process.poll() is not None:
                if exit_drain_deadline is None:
                    exit_drain_deadline = current + 0.25
                elif current >= exit_drain_deadline and selector.get_map():
                    status = "child_process_unfinished"
                    terminate_group(process)
                    break
            for key, _ in selector.select(min(0.1, max(0, deadline - current))):
                stream = key.fileobj
                try:
                    chunk = os.read(stream.fileno(), 64 * 1024)
                except BlockingIOError:
                    continue
                if not chunk:
                    selector.unregister(stream)
                    if stream is process.stdout and pending and not oversized:
                        collector.accept(bytes(pending))
                        pending.clear()
                    continue
                if stream is process.stdout:
                    consume(chunk)
    except KeyboardInterrupt:
        status = "cancelled"
        terminate_group(process)
    except (OSError, ValueError):
        status = "stream_reader_failed"
        terminate_group(process)
    finally:
        selector.close()
        process.stdout.close()
        process.stderr.close()
    return {"status": status, "exit_code": process.poll()}


@contextmanager
def run_lock(directory):
    try:
        import fcntl
    except ImportError as error:
        raise RunnerError("run_requires_macos_or_linux") from error
    flags = os.O_WRONLY | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(directory / ".run.lock", flags, 0o600)
    with os.fdopen(descriptor, "w") as stream:
        try:
            fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise RunnerError("run_already_active") from error
        yield


def inspect(directory):
    directory = safe_path(directory, must_exist=True)
    metadata = read_json(directory / "run.json")
    result_path = directory / "result.json"
    if result_path.exists() or result_path.is_symlink():
        return {"state": "recorded", "result": read_json(result_path), "automatic_retry_permitted": False}
    if (directory / "start.json").exists() or (directory / "start.json").is_symlink():
        return {"state": "started_or_interrupted", "outcome": "unknown", "automatic_retry_permitted": False,
                "reserved_unknown_usd": metadata["budget_usd"], "requires_manual_reconciliation": True}
    return {"state": "prepared", "network_used": False, "budget_reserved_usd": metadata["budget_usd"]}


def run(directory, *, capture=None, compare=None):
    directory, metadata = load_run(directory)
    with run_lock(directory):
        if any((directory / name).exists() or (directory / name).is_symlink() for name in ("start.json", "result.json", "evidence.json")):
            raise RunnerError("started_runs_cannot_be_reissued")
        api_key, token = os.environ.get("SUB2API_API_KEY"), os.environ.get("SUB2API_USER_TOKEN")
        if not api_key or not token:
            raise RunnerError("set_api_key_and_user_token_environment")
        if capture is None or compare is None:
            import metering
            capture, compare = metering.capture_snapshot, metering.compare_snapshots
        env = child_environment(directory, metadata, api_key)
        try:
            version = subprocess.run(metadata["cli"] + ["--version"], env=env, cwd=directory / "workspace",
                                     stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=15, check=False)
        except (OSError, subprocess.TimeoutExpired) as error:
            raise RunnerError("client_version_check_failed") from error
        output = version.stdout.decode("utf-8", errors="replace")
        expected = metadata["expected_client_version"]
        if version.returncode != 0 or not re.search(r"(?<![0-9.])" + re.escape(expected) + r"(?![0-9.])", output):
            raise RunnerError("client_version_mismatch")
        try:
            before = capture(metadata["base_url"], metadata["api_key_id"], token, expected_key=api_key)
        except Exception as error:
            raise RunnerError("metering_preflight_failed") from error
        key = before.get("api_key", {})
        quota = key.get("quota_usd")
        used = key.get("quota_used_usd")
        if quota is None or used is None:
            raise RunnerError("metering_quota_fields_missing")
        try:
            remaining = Decimal(str(quota)) - Decimal(str(used))
        except InvalidOperation as error:
            raise RunnerError("invalid_key_quota") from error
        if not remaining.is_finite() or remaining <= 0 or Decimal(str(quota)) <= 0:
            raise RunnerError("finite_funded_key_required")
        if remaining > Decimal(metadata["budget_usd"]):
            raise RunnerError("key_quota_exceeds_local_reservation")
        if key.get("key_fingerprint") != digest(api_key):
            raise RunnerError("metering_key_identity_mismatch")
        if key.get("status") != "active" or key.get("current_concurrency") != 0:
            raise RunnerError("inactive_or_busy_test_key")
        if key.get("group_subscription_type") != "standard":
            raise RunnerError("standard_billing_test_key_required")
        if not before.get("capture_stable"):
            raise RunnerError("unstable_metering_preflight")
        write_new_json(directory / "start.json", {"schema_version": 1, "run_id": metadata["run_id"], "started_at": now(),
                                                  "reserved_unknown_usd": metadata["budget_usd"], "pid": os.getpid()})
        write_new_json(directory / "before.json", before)
        start = time.monotonic()
        collector = EventEvidence(metadata)
        task = safe_path(directory / "workspace/TASK.md", must_exist=True).read_text(encoding="utf-8")
        prompt = ("Delegate this task to the explicitly named fusion-worker using the task tool. "
                  "Wait for that worker, then independently verify the required files and checks. "
                  "Do not solve it entirely in the parent agent. Do not create nested workers.\n\n" + task)
        process = {"status": "launch_failed", "exit_code": None}
        try:
            process = stream_client(client_command(directory, metadata, prompt), directory / "workspace", env,
                                    metadata["timeout_seconds"], collector)
        except OSError:
            pass
        try:
            checker = smoke_module().check(metadata["case_id"], directory / "workspace")
        except Exception:
            checker = {"quality_passed": False, "test_exit_code": None, "failure_reason": "independent_checker_failed"}
        try:
            after = capture(metadata["base_url"], metadata["api_key_id"], token, expected_key=api_key)
            comparison = compare(before, after)
            write_new_json(directory / "after.json", after)
        except Exception:
            comparison = {"verified_settled_delta_usd": None, "cost_scope": "partial", "evidence_complete": False,
                          "failure_reason": "metering_after_failed"}
        evidence = collector.report()
        evidence.update({"schema_version": 1, "run_id": metadata["run_id"], "process": process,
                         "independent_check": checker, "metering": comparison,
                         "isolation": {"home_redefined": False, "user_token_passed_to_client": False,
                                       "other_provider_credentials_inherited": False,
                                       "omo_home_migration_may_access_user_directory": metadata["client"] == "omo",
                                       "client_session_files_may_contain_raw_local_content": True}})
        outcome, failure = "completed", None
        if process["status"] != "exited":
            outcome, failure = "unknown", process["status"]
        elif process["exit_code"] != 0 or collector.error_observed:
            outcome, failure = "failed", "client_failed"
        elif collector.invalid_lines or collector.truncated or collector.parse_errors:
            outcome, failure = "protocol_error", "client_event_evidence_incomplete"
        elif collector.route_mismatch:
            outcome, failure = "protocol_error", "client_model_route_mismatch"
        elif not checker.get("quality_passed"):
            outcome, failure = "failed", "independent_check_failed"
        elif not collector.dispatch_verified:
            outcome, failure = "unknown", "fusion_worker_dispatch_unverified"
        snapshot = {"smoke_only": True, "client": metadata["client"], "expected_client_version": expected,
                    "config_hashes": metadata["config_hashes"], "adapter_sha256": metadata["adapter_sha256"],
                    "worker_variants": {"lead": metadata["direct_model"], "fusion": metadata["fusion_model"]},
                    "task_case": metadata["case_id"], "api_key_id": metadata["api_key_id"]}
        result = {"schema_version": 1, "record_id": metadata["run_id"], "cohort_id": "development-smoke-" + metadata["run_id"],
                  "case_id": metadata["case_id"], "client": metadata["client"], "arm": "fusion", "comparison_snapshot": snapshot,
                  "outcome": outcome, "completed": outcome == "completed", "quality_passed": bool(checker.get("quality_passed")),
                  "cost_scope": "partial", "confirmed_usd": comparison.get("verified_settled_delta_usd"),
                  "reserved_unknown_usd": metadata["budget_usd"], "inflight_reserve_usd": "0", "wall_seconds": time.monotonic() - start}
        if failure:
            result["failure_reason"] = failure
        import ledger
        result = ledger.validate_run(result)
        write_new_json(directory / "evidence.json", evidence)
        write_new_json(directory / "result.json", result)
        return {"result": result, "evidence_file": str(directory / "evidence.json"),
                "requires_manual_reconciliation": True, "automatic_retry_permitted": False}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    create = commands.add_parser("prepare", help="create a new local smoke; no network or model calls")
    create.add_argument("--directory", required=True)
    create.add_argument("--client", choices=CLIENT_VERSIONS, required=True)
    create.add_argument("--case", choices=CASES, required=True)
    create.add_argument("--base-url", required=True)
    create.add_argument("--direct-model", required=True)
    create.add_argument("--fusion-model", required=True)
    create.add_argument("--api-key-id", type=int, required=True)
    create.add_argument("--budget-usd", required=True)
    create.add_argument("--cli-json")
    create.add_argument("--timeout-seconds", type=int, default=180)
    commands.add_parser("run", help="execute one real local client; this can incur charges").add_argument("directory")
    commands.add_parser("inspect", help="read existing evidence without sending requests").add_argument("directory")
    args = parser.parse_args(argv)
    try:
        if args.command == "prepare":
            result = prepare(args.directory, args.client, args.case, args.base_url, args.direct_model,
                             args.fusion_model, args.api_key_id, args.budget_usd, args.cli_json, args.timeout_seconds)
        elif args.command == "inspect":
            result = inspect(args.directory)
        else:
            result = run(args.directory)
        print(json.dumps(result, indent=2, sort_keys=True, allow_nan=False))
        if args.command == "run":
            return 2 if result["result"]["outcome"] == "failed" else 3
        return 0
    except (RunnerError, OSError, UnicodeError, ValueError, KeyError, TypeError) as error:
        code = str(error) if isinstance(error, RunnerError) else "local_runner_failed"
        print(json.dumps({"error": code}), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
