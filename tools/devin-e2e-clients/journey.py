#!/usr/bin/env python3
"""Real CLI project journey, using files copied from the product browser UI.

This entry point supports simulated upstreams only. It cannot certify live E2E.
Model outputs and CLI logs remain in memory; reports contain structural evidence.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import threading
import time

from run import ROOT, PINS, SENSITIVE_ENV, child_environment, write_json

TEST_COMMAND = "python3 run_tests.py"
ACTIVE = set()


def terminate_children(number, _frame):
    for proc in list(ACTIVE):
        if proc.poll() is None:
            os.killpg(proc.pid, signal.SIGTERM)
            try:
                proc.wait(timeout=3)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait(timeout=3)
    raise SystemExit(128 + number)


signal.signal(signal.SIGTERM, terminate_children)
signal.signal(signal.SIGINT, terminate_children)
INITIAL_SOURCE = '''import json
from pathlib import Path

def discounted(subtotal, discount):
    return subtotal + discount

def report():
    data = json.loads(Path("requirements.json").read_text())
    result = {"nonce": data["nonce"], "totals": [discounted(row[0], row[1]) for row in data["rows"]]}
    Path("out").mkdir(exist_ok=True)
    Path("out/report.json").write_text(json.dumps(result))
    return result
'''
TEST_SOURCE = '''import json
from pathlib import Path
import unittest
from price_math import report

class ProjectTest(unittest.TestCase):
    def test_discount(self):
        data = json.loads(Path("requirements.json").read_text())
        self.assertEqual(report()["totals"], [subtotal - discount for subtotal, discount in data["rows"]])
'''
TEST_RUNNER = '''import json
from pathlib import Path
import sys
import unittest

result = unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.discover("."))
exit_code = 0 if result.wasSuccessful() else 1
Path("out").mkdir(exist_ok=True)
data = json.loads(Path("requirements.json").read_text())
Path("out/test-execution.json").write_text(json.dumps({"nonce": data["nonce"], "exit_code": exit_code}))
sys.exit(exit_code)
'''


def isolation(client=None):
    if os.geteuid() == 0:
        return {"user": 65534, "group": 65534, "extra_groups": []}
    if client == "omo" and os.access(Path.home(), os.W_OK) and os.environ.get("GITHUB_ACTIONS") != "true":
        raise ValueError("OMO requires a disposable CI user or a read-only personal directory")
    return {}


def project_environment(directory):
    env = child_environment(directory)
    for name in {"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "DEVIN_E2E_GATEWAY_KEY", "DEVIN_JOURNEY_GATEWAY_KEY"}:
        env.pop(name, None)
    return env


def run_project_python(directory, arguments, input_data=None):
    return subprocess.run(['python3', *arguments], cwd=directory / 'workspace', env=project_environment(directory),
                          input=input_data, capture_output=True, timeout=15, **isolation())


def project_bytes(workspace, relative):
    """Read bounded regular project files without following symlinks as parent."""
    parts = Path(relative).parts
    if not parts or Path(relative).is_absolute() or any(p in {'.', '..'} for p in parts):
        raise ValueError('invalid project artifact')
    directory = os.open(workspace, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in parts[:-1]:
            nested = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=directory)
            os.close(directory); directory = nested
        fd = os.open(parts[-1], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
        try:
            if not stat.S_ISREG(os.fstat(fd).st_mode):
                raise ValueError('nonregular project artifact')
            chunks = bytearray()
            while len(chunks) <= 1024 * 1024:
                chunk = os.read(fd, min(65536, 1024 * 1024 + 1 - len(chunks)))
                if not chunk:
                    return bytes(chunks)
                chunks.extend(chunk)
            raise ValueError('oversized project artifact')
        finally:
            os.close(fd)
    finally:
        os.close(directory)


def omp_test_result(event, command, workspace, nonce, client_started_ns):
    details = event.get('result', {}).get('details', {})
    if command != TEST_COMMAND or event.get('isError') is not False or details.get('timedOut') or details.get('async') or details.get('exitCode', 0) != 0:
        return False
    try:
        proof = json.loads(project_bytes(workspace, 'out/test-execution.json'))
        timestamp = (workspace / 'out/test-execution.json').stat().st_mtime_ns
    except (OSError, ValueError):
        return False
    output = json.dumps(event.get('result', {}))
    return proof == {'nonce': nonce, 'exit_code': 0} and timestamp >= client_started_ns and 'Ran 1 test' in output and 'OK' in output


def final_project_snapshot(workspace, expected, protected, source_hash):
    try:
        raw = project_bytes(workspace, 'out/report.json')
    except FileNotFoundError:
        raw = b'null'
    valid = json.loads(raw) == expected
    return {
        'artifact_verified_after_resume': valid,
        'protected_project_files_intact': all(digest(project_bytes(workspace, name)) == fingerprint for name, fingerprint in protected.items()),
        'source_unchanged_after_resume': digest(project_bytes(workspace, 'price_math.py')) == source_hash,
        'artifact_sha256': digest(raw) if valid else None,
    }


def digest(data):
    return hashlib.sha256(data).hexdigest()


def prepare(directory):
    workspace = directory / "workspace"
    workspace.mkdir(parents=True)
    data = {"nonce": os.urandom(12).hex(), "rows": [[100 + int.from_bytes(os.urandom(2), "big"), 1 + byte] for byte in os.urandom(8)]}
    write_json(workspace / "requirements.json", data)
    (workspace / "price_math.py").write_text(INITIAL_SOURCE)
    (workspace / "test_project.py").write_text(TEST_SOURCE)
    (workspace / "run_tests.py").write_text(TEST_RUNNER)
    expected = {"nonce": data["nonce"], "totals": [a - b for a, b in data["rows"]]}
    for name in ("config/opencode", "data", "cache", "state", "claude", "omp-agent", "omp-root"):
        (directory / name).mkdir(parents=True, exist_ok=True)
    if os.geteuid() == 0:
        for base, dirs, files in os.walk(directory):
            if Path(base) == directory:
                os.chmod(base, 0o755)
            else:
                os.chown(base, 65534, 65534)
            for name in files:
                os.chown(Path(base) / name, 65534, 65534)
    before = run_project_python(directory, ['run_tests.py'])
    if before.returncode != 1 or b"AssertionError" not in before.stderr:
        raise ValueError("project bug was not reproduced before the client task")
    # Remove intentionally incorrect outputs so a no-op client cannot pass.
    (workspace / "out/report.json").unlink(missing_ok=True)
    (workspace / "out/test-execution.json").unlink(missing_ok=True)
    protected = {name: digest(project_bytes(workspace, name)) for name in ("requirements.json", "test_project.py", "run_tests.py")}
    return workspace, expected, protected


def identity(client):
    for package, version in PINS.items():
        if json.loads((ROOT / "node_modules" / package / "package.json").read_text())["version"] != version:
            raise ValueError("client pin mismatch")
    package = {'opencode': 'opencode-ai', 'omo': 'oh-my-opencode', 'omp': '@oh-my-pi/pi-coding-agent'}[client]
    executable = ROOT / ('node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js' if client == 'omp' else 'node_modules/opencode-linux-x64/bin/opencode')
    fingerprint = hashlib.sha256()
    with executable.open('rb') as source:
        for chunk in iter(lambda: source.read(65536), b''):
            fingerprint.update(chunk)
    return {'package': package, 'version': PINS[package], 'executable_sha256': fingerprint.hexdigest(),
            'engine': 'bun' if client == 'omp' else 'opencode',
            'engine_version': PINS['@oven/bun-linux-x64'] if client == 'omp' else PINS['opencode-ai'],
            'platform': 'linux-x64', 'omo_senpi_equivalent': False if client == 'omo' else None}


def command(client, model, resume):
    if client in {"opencode", "omo"}:
        cmd = [str(ROOT / "node_modules/opencode-linux-x64/bin/opencode"), "run", "--agent", "devin-project", "--model", "devin/" + model, "--format", "json", "--title", "Synthetic project acceptance"]
        if client == "opencode":
            cmd.append("--pure")
    else:
        cmd = [str(ROOT / "node_modules/@oven/bun-linux-x64/bin/bun"), str(ROOT / "node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js"),
               "--model", "devin/" + model, "--tools", "read,write,bash", "--no-lsp", "--no-pty", "--no-extensions", "--no-skills", "--no-rules", "--no-title", "--thinking", "off", "--max-time", "120", "--mode", "json", "-p"]
    if resume:
        cmd.append("--continue")
    prompt = ("Resume this project session. Read out/report.json and confirm the completed project using the existing session history. Reply with exactly PROJECT_REVIEWED."
              if resume else
              "Repair price_math.py so the project tests pass. Read requirements.json and price_math.py, fix the calculation, run python3 run_tests.py, then re-read requirements.json and out/report.json to verify the result. Do not change the requirements, tests or test runner. Reply with exactly PROJECT_COMPLETE after verification.")
    return cmd + [prompt]


def invoke(args, directory, resume=False):
    env = project_environment(directory)
    for name in SENSITIVE_ENV | {"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"}:
        env.pop(name, None)
    env["SUB2API_API_KEY"] = os.environ["DEVIN_JOURNEY_GATEWAY_KEY"]
    env["BUN_INSTALL_CACHE_DIR"] = str(directory / "cache/bun")
    kwargs = isolation(args.client)
    client_started_ns = time.time_ns()
    nonce = json.loads(project_bytes(directory / 'workspace', 'requirements.json'))['nonce']
    proc = subprocess.Popen(command(args.client, args.model, resume), cwd=directory / "workspace", env=env,
                            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True, **kwargs)
    ACTIVE.add(proc)
    result = {"exit_code": None, "timed_out": False, "event_error": False, "final_text_matches": False,
              "normal_stop": False, "tools": {"read": 0, "write": 0, "bash": 0}, "client_tests_succeeded": False,
              "frames": 0, "parse_errors": 0, "session_seen": False, "client_session_sha256": None, "session_changed": False,
              "bash_exit_code": None, "test_command_verified": False}
    final_text = ""
    size = 0
    tool_commands = {}

    def consume():
        nonlocal size, final_text
        for raw in proc.stdout:
            size += len(raw)
            if size > 16 * 1024 * 1024 or len(raw) > 4 * 1024 * 1024:
                os.killpg(proc.pid, signal.SIGKILL)
                return
            try:
                event = json.loads(raw)
            except (ValueError, UnicodeDecodeError):
                result["parse_errors"] += 1
                continue
            result["frames"] += 1
            kind = event.get("type")
            if kind == "tool_execution_start" and event.get("toolName") == "bash":
                call_id = event.get("toolCallId")
                tool_commands[call_id] = event.get("args", {}).get("command")
            result["session_seen"] |= bool(event.get("sessionID") or kind == "session")
            session = event.get("sessionID", event.get("sessionId", event.get("id") if kind == "session" else None))
            if isinstance(session, str):
                fingerprint = digest(session.encode())
                result["session_changed"] |= result["client_session_sha256"] not in {None, fingerprint}
                result["client_session_sha256"] = fingerprint
            if kind == "error":
                result["event_error"] = True
            if kind == "text":
                final_text += event.get("part", {}).get("text", "")
            if kind == "step_start":
                final_text = ""
            if kind == "step_finish":
                result["normal_stop"] = event.get("part", {}).get("reason") in {"stop", "end_turn"}
            if kind == "tool_use":
                part = event.get("part", {}); state = part.get("state", {})
                if state.get("status") == "error":
                    result["event_error"] = True
                name = part.get("tool")
                if state.get("status") == "completed" and name in result["tools"]:
                    result["tools"][name] += 1
                    if name == "bash":
                        text = json.dumps(state.get("output", ""))
                        result["bash_exit_code"] = state.get("metadata", {}).get("exit")
                        result["test_command_verified"] = state.get("input", {}).get("command") == TEST_COMMAND
                        result["client_tests_succeeded"] |= result["bash_exit_code"] == 0 and result["test_command_verified"] and "Ran 1 test" in text and "OK" in text
            if kind == "tool_execution_end":
                name = event.get("toolName")
                if event.get("isError"):
                    result["event_error"] = True
                elif name in result["tools"]:
                    result["tools"][name] += 1
                    if name == "bash":
                        text = json.dumps(event.get("result", {}))
                        call_id = event.get("toolCallId")
                        result["test_command_verified"] = tool_commands.get(call_id) == TEST_COMMAND
                        # OMP documents exitCode only for nonzero exits. Require
                        # the protected project runner's explicit, fresh exit
                        # receipt as well as the corresponding actual tool ID.
                        success = omp_test_result(event, tool_commands.get(call_id), directory / 'workspace', nonce, client_started_ns)
                        result['bash_exit_code'] = 0 if success else None
                        result['bash_exit_source'] = 'protected project test runner receipt' if success else 'unverified'
                        result['client_tests_succeeded'] |= success
            if kind == "message_end" and event.get("message", {}).get("role") == "assistant":
                message = event["message"]
                final_text = "".join(p.get("text", "") for p in message.get("content", []) if p.get("type") == "text")
                reason = message.get("stopReason")
                result["normal_stop"] = reason in {"stop", "end_turn"}
                result["event_error"] |= reason in {"error", "aborted"}

    thread = threading.Thread(target=consume, daemon=True)
    thread.start()
    try:
        proc.wait(timeout=150)
    except subprocess.TimeoutExpired:
        result["timed_out"] = True
        os.killpg(proc.pid, signal.SIGKILL)
        proc.wait(timeout=5)
    finally:
        thread.join(timeout=5)
        proc.stdout.close()
        ACTIVE.discard(proc)
    result["exit_code"] = proc.returncode
    result["final_text_matches"] = final_text.strip() == ("PROJECT_REVIEWED" if resume else "PROJECT_COMPLETE")
    result["passed"] = proc.returncode == 0 and result["normal_stop"] and result["final_text_matches"] and not result["event_error"] and not result["timed_out"] and result["parse_errors"] == 0 and not result["session_changed"]
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--directory", required=True)
    parser.add_argument("--client", choices=("opencode", "omo", "omp"), required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--ui-url", required=True)
    parser.add_argument("--profile", choices=("project", "compatibility", "default"), required=True)
    args = parser.parse_args()
    client_identity = identity(args.client)
    isolation(args.client)  # reject writable personal OMO state before setup
    directory = Path(args.directory).resolve()
    workspace, expected, protected = prepare(directory)
    ui_input = {"directory": str(directory), "workspace": str(workspace), "client": args.client, "model": args.model, "ui_url": args.ui_url, "profile": args.profile, "test_command": TEST_COMMAND}
    kwargs = isolation(args.client)
    ui = subprocess.Popen(["node", str(ROOT / "ui-handoff.mjs")], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=project_environment(directory), start_new_session=True, **kwargs)
    ACTIVE.add(ui)
    try:
        stdout, _ = ui.communicate(json.dumps(ui_input).encode(), timeout=60)
    except subprocess.TimeoutExpired:
        os.killpg(ui.pid, signal.SIGTERM)
        try:
            ui.wait(timeout=3)
        except subprocess.TimeoutExpired:
            os.killpg(ui.pid, signal.SIGKILL)
            ui.wait(timeout=3)
        raise
    finally:
        ACTIVE.discard(ui)
    handoff = json.loads(stdout)
    if ui.returncode == 0 and handoff.get('blocked') is True:
        print(json.dumps({'status': 'unsupported_by_product', 'handoff': handoff, 'client_identity': client_identity,
                          'client': args.client, 'profile': args.profile, 'tier': 'client_mock', 'eligible_for_real_e2e': False}))
        return 0
    if ui.returncode or not handoff.get("passed"):
        print(json.dumps({"status": "failed", "stage": "product-ui-handoff", "ui": handoff, "tier": "client_mock"})); return 1
    first = invoke(args, directory)
    artifact = workspace / "out/report.json"
    valid = artifact.exists() and json.loads(project_bytes(workspace, 'out/report.json')) == expected
    intact = all(digest(project_bytes(workspace, name)) == fingerprint for name, fingerprint in protected.items())
    # An independent oracle verifies the client's changes; it is not counted as
    # the client running tests and cannot repair or generate the final artifact.
    oracle_samples = [[100 + int.from_bytes(os.urandom(2), "big"), byte] for byte in os.urandom(16)]
    after = run_project_python(directory, ['-c', "import json,sys; from price_math import discounted; assert all(discounted(a,b)==a-b for a,b in json.load(sys.stdin))"],
                               json.dumps(oracle_samples).encode()) if first["passed"] else None
    task_passed = first["passed"] and valid and intact and first["tools"]["read"] >= 3 and first["tools"]["write"] >= 1 and first["client_tests_succeeded"] and after is not None and after.returncode == 0
    source_before_resume = digest(project_bytes(workspace, 'price_math.py'))
    resumed = invoke(args, directory, True) if task_passed else None
    same_session = resumed is not None and first["client_session_sha256"] is not None and first["client_session_sha256"] == resumed["client_session_sha256"]
    final = final_project_snapshot(workspace, expected, protected, source_before_resume)
    passed = task_passed and resumed is not None and resumed["passed"] and resumed["tools"]["read"] >= 1 and same_session and all(final[key] for key in ['artifact_verified_after_resume', 'protected_project_files_intact', 'source_unchanged_after_resume'])
    report = {"tier": "client_mock", "status": "passed" if passed else "failed", "eligible_for_real_e2e": False,
              "client_identity": client_identity,
              "client": args.client, "profile": args.profile, "handoff": handoff, "task": first, "resume": resumed,
              "initial_project_test_failed": True, "artifact_verified_before_oracle": valid, **final,
              "independent_project_test_passed": after is not None and after.returncode == 0,
              "same_client_session": same_session,
              "context_pressure": "not_run", "compact": "not_run", "fork": "not_run", "worker": "not_run"}
    print(json.dumps(report, sort_keys=True))
    return 0 if passed else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (ValueError, OSError, KeyError, subprocess.TimeoutExpired):
        print(json.dumps({"status": "failed", "stage": "runner-boundary", "tier": "client_mock"}))
        raise SystemExit(2)
