#!/usr/bin/env python3
"""Real CLI project journey, using files copied from the product browser UI.

Real upstream selection is made only by the separately approved supervisor and
gateway admission; this child never obtains native credentials. Neither mode
automatically certifies all user E2E capabilities.
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
import sys
import threading
import time

from run import ROOT, PINS, SENSITIVE_ENV, child_environment, write_json
import process_cleanup
import container_sandbox
from project_evidence import ToolEvidence
from project_completion import CompletionEvidence
from project_tests import TestReceipts, TEST_RUNNER

TEST_COMMAND = "python3 run_tests.py"
ACTIVE = set()


def track(proc):
    ACTIVE.add(proc)
    process_cleanup.register(proc)


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



def isolation(client=None):
    if os.geteuid() == 0:
        return {"user": 65534, "group": 65534, "extra_groups": []}
    container_sandbox.require_available()
    return {}


def isolation_evidence():
    if os.geteuid()==0:
        return {'mode':'separate_uid','parent_uid':0,'child_uid':65534}
    return container_sandbox.evidence()


def project_environment(directory):
    env = child_environment(directory)
    for name in {"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "DEVIN_E2E_GATEWAY_KEY", "DEVIN_JOURNEY_GATEWAY_KEY", "DEVIN_JOURNEY_UI_PASSWORD"}:
        env.pop(name, None)
    for name in ['DEVIN_JOURNEY_APPROVAL_FILE', 'DEVIN_JOURNEY_PLAN_FILE', 'DEVIN_JOURNEY_RUN_LEDGER','DEVIN_JOURNEY_PROCESS_REGISTRY','DEVIN_JOURNEY_CONTAINER_REGISTRY']:
        env.pop(name, None)
    allowed={'PATH','HOME','USER','LOGNAME','SHELL','LANG','LANGUAGE','TERM','TZ','TMPDIR','TMP','TEMP','PWD',
             'XDG_CONFIG_HOME','XDG_DATA_HOME','XDG_CACHE_HOME','XDG_STATE_HOME','CLAUDE_CONFIG_DIR','PI_CODING_AGENT_DIR','PI_CONFIG_DIR',
             'OPENCODE_DISABLE_AUTOUPDATE','npm_config_cache','BUN_INSTALL_CACHE_DIR','CHROMIUM_EXECUTABLE_PATH',
             'HTTP_PROXY','HTTPS_PROXY','ALL_PROXY','NO_PROXY','http_proxy','https_proxy','all_proxy','no_proxy',
             'SSL_CERT_FILE','SSL_CERT_DIR','NODE_EXTRA_CA_CERTS','REQUESTS_CA_BUNDLE','PIP_CERT','CURL_CA_BUNDLE','GIT_SSL_CAINFO',
             'LD_PRELOAD','LD_LIBRARY_PATH','NODE_OPTIONS'}
    return {key:value for key,value in env.items() if key in allowed or key.startswith(('CODEX_PROXY','LC_'))}


def run_project_python(directory, arguments, input_data=None):
    if os.geteuid()!=0:
        return container_sandbox.run(['python3',*arguments],directory,project_environment(directory),input_data=input_data,timeout=15)
    command=['python3',*arguments]
    proc=start_child(command,directory,project_environment(directory),stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
    try:
        return container_sandbox.collect_output(proc,command,input_data=input_data,timeout=15)
    finally:
        ACTIVE.discard(proc)


def start_child(command,directory,env,*,gateway=None,client=None,**kwargs):
    if os.geteuid()!=0:
        return container_sandbox.popen(command,directory,env,gateway=gateway,on_start=track,**kwargs)
    proc=subprocess.Popen(command,cwd=directory/'workspace',env=env,start_new_session=True,**isolation(client),**kwargs)
    track(proc)
    return proc


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


def prepare_pressure(directory,nonce):
    workspace=os.open(directory/'workspace',os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
    try:
        os.mkdir('context',0o755,dir_fd=workspace)
        context=os.open('context',os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=workspace)
        try:
            for index in range(16):
                noise=os.urandom(4096).hex()
                data=('CONTEXT_NONCE='+nonce+'\n'+'\n'.join(noise[i:i+64] for i in range(0,len(noise),64))+'\n').encode()
                fd=os.open(f'part-{index:02d}.txt',os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW|os.O_NONBLOCK,0o600,dir_fd=context)
                with os.fdopen(fd,'wb') as target:
                    target.write(data)
                    if os.geteuid()==0: os.fchown(target.fileno(),65534,65534)
            if os.geteuid()==0: os.fchown(context,65534,65534)
        finally:
            os.close(context)
    finally:
        os.close(workspace)


def prepare(directory):
    workspace = directory / "workspace"
    workspace.mkdir(parents=True)
    data = {"nonce": os.urandom(12).hex(), "rows": [[100 + int.from_bytes(os.urandom(2), "big"), 1 + byte] for byte in os.urandom(8)]}
    write_json(workspace / "requirements.json", data)
    (workspace / "price_math.py").write_text(INITIAL_SOURCE)
    (workspace / "test_project.py").write_text(TEST_SOURCE)
    (workspace / "run_tests.py").write_text(TEST_RUNNER)
    expected = {"nonce": data["nonce"], "totals": [a - b for a, b in data["rows"]]}
    for name in ("config/opencode", "data", "cache/client-tmp", "state", "claude", "omp-agent", "omp-root"):
        (directory / name).mkdir(parents=True, exist_ok=True)
    # Install dependencies before credential use via npm ci. Mirror its genuine
    # locked metadata into disposable client directories; Npm.install can then
    # verify them without reaching a registry from the networkless client.
    for name in ['package.json','package-lock.json']:
        (directory/'config/opencode'/name).write_bytes((ROOT/name).read_bytes())
    (directory/'config/opencode/node_modules').symlink_to(ROOT/'node_modules',target_is_directory=True)
    plugin_cache=directory/'cache/opencode/packages'/('oh-my-opencode@'+PINS['oh-my-opencode'])
    plugin_cache.mkdir(parents=True)
    (plugin_cache/'node_modules').symlink_to(ROOT/'node_modules',target_is_directory=True)
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
    for receipt in (workspace / 'out/test-receipts').glob('*.json'):
        receipt.unlink()
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
    prompt = ("Resume this project session. Read out/report.json and confirm the completed project using the existing session history. Summarize what you verified."
              if resume else
              "Repair price_math.py so the project tests pass. Read requirements.json and price_math.py, fix the calculation, run python3 run_tests.py, then re-read requirements.json and out/report.json to verify the result. Do not change the requirements, tests or test runner. Summarize the verified result.")
    if model.startswith('fusion-') and not resume:
        prompt = "Lead: delegate the project implementation to Sidekick, then review its returned work and verify the result. " + prompt
    return cmd + [prompt]


def invoke(args, directory, resume=False, protected=None):
    env = project_environment(directory)
    for name in SENSITIVE_ENV | {"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"}:
        env.pop(name, None)
    env["SUB2API_API_KEY"] = os.environ["DEVIN_JOURNEY_GATEWAY_KEY"]
    env["BUN_INSTALL_CACHE_DIR"] = str(directory / "cache/bun")
    env['TMPDIR']=str(directory/'cache/client-tmp')
    def plugin_startups():
        try: data=project_bytes(directory,'cache/client-tmp/oh-my-opencode.log')
        except FileNotFoundError: return 0
        return data.count(b'[oh-my-openagent] ENTRY - plugin loading')
    plugin_before=plugin_startups() if args.client=='omo' else None
    kwargs = isolation(args.client)
    client_started_ns = time.time_ns()
    nonce = json.loads(project_bytes(directory / 'workspace', 'requirements.json'))['nonce']
    protected = dict(protected) if protected is not None else {name: digest(project_bytes(directory / 'workspace', name)) for name in ('requirements.json', 'test_project.py', 'run_tests.py')}
    tests = TestReceipts(lambda name: project_bytes(directory / 'workspace', name), nonce, protected, client_started_ns)
    proc = start_child(command(args.client,args.model,resume),directory,env,gateway=args.ui_url,client=args.client,
                       stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
    result = {"exit_code": None, "timed_out": False, "event_error": False, "final_text_matches": False,
              "normal_stop": False, "tools": {"read": 0, "write": 0, "bash": 0}, "client_tests_succeeded": False,
              "frames": 0, "parse_errors": 0, "session_seen": False, "client_session_sha256": None, "session_changed": False,
              "bash_exit_code": None, "test_command_verified": False}
    result.update(reader_completed=False, reader_error=False, project_nonce_sha256=digest(nonce.encode()))
    size = 0
    tool_evidence = ToolEvidence(directory / 'workspace', TEST_COMMAND)
    completion = CompletionEvidence()

    def consume_lines():
        nonlocal size
        for raw in iter(lambda: proc.stdout.readline(4 * 1024 * 1024 + 1), b''):
            size += len(raw)
            if size > 16 * 1024 * 1024 or len(raw) > 4 * 1024 * 1024:
                result['reader_error'] = True
                os.killpg(proc.pid, signal.SIGKILL)
                return False
            try:
                event = json.loads(raw)
            except (ValueError, UnicodeDecodeError):
                result["parse_errors"] += 1
                continue
            if not isinstance(event, dict):
                result['parse_errors'] += 1
                continue
            result["frames"] += 1
            kind = event.get("type")
            try:
                tool = tool_evidence.observe(event)
                tests.observe(tool, event)
                completion.observe(event)
            except ValueError:
                result['parse_errors'] += 1
                continue
            result["session_seen"] |= bool(event.get("sessionID") or kind == "session")
            session = event.get("sessionID", event.get("sessionId", event.get("id") if kind == "session" else None))
            if isinstance(session, str):
                fingerprint = digest(session.encode())
                result["session_changed"] |= result["client_session_sha256"] not in {None, fingerprint}
                result["client_session_sha256"] = fingerprint
            if kind == "error":
                result["event_error"] = True
            if kind == "tool_use":
                part = event.get("part", {}); state = part.get("state", {})
                if state.get("status") == "error":
                    result["event_error"] = True
            if kind == "tool_execution_end" and event.get("isError"):
                result["event_error"] = True
        return True  # Only iterator EOF certifies complete event consumption.

    def consume():
        try:
            result['reader_completed'] = consume_lines()
        except (OSError, ValueError, TypeError, AttributeError):
            result['reader_error'] = True

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
        if thread.is_alive():
            result['reader_error'] = True
        proc.stdout.close()
        ACTIVE.discard(proc)
    result["exit_code"] = proc.returncode
    result.update(client_started_ns=client_started_ns, client_finished_ns=time.time_ns())
    result.update(tool_evidence.report())
    result.update(tests.report())
    result.update(completion.report('PROJECT_REVIEWED' if resume else 'PROJECT_COMPLETE'))
    result['event_error'] |= tool_evidence.failed or completion.failed
    if args.client=='omo':
        result['plugin_runtime_started']=plugin_startups()>plugin_before
        result['plugin_runtime_witness']='fresh oh-my-openagent server plugin startup log'
    result["passed"] = (proc.returncode == 0 and result['reader_completed'] and not result['reader_error']
                        and result['terminal_event_seen'] and result['normal_stop'] and result['final_text_bytes'] > 0
                        and not result['event_error'] and not result['timed_out'] and result['parse_errors'] == 0
                        and result['incomplete_tool_calls'] == 0 and result['session_seen'] and result['client_session_sha256'] is not None
                        and not result['session_changed'] and (args.client != 'omo' or result['plugin_runtime_started']))
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--directory", required=True)
    parser.add_argument("--client", choices=("opencode", "omo", "omp"), required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--ui-url", required=True)
    parser.add_argument("--profile", choices=("project", "compatibility", "default"), required=True)
    parser.add_argument('--stage', choices=('task', 'handoff'), default='task')
    parser.add_argument('--upstream', choices=('synthetic', 'real'), default='synthetic')
    parser.add_argument('--catalog-recovery-probe', action='store_true')
    args = parser.parse_args()
    if args.catalog_recovery_probe and args.upstream != 'synthetic':
        parser.error('catalog recovery probe requires synthetic upstream')
    tier = 'client_mock' if args.upstream == 'synthetic' else 'product_live_partial'
    client_identity = identity(args.client)
    isolation(args.client)  # reject writable personal OMO state before setup
    isolated=isolation_evidence()
    directory = Path(args.directory).resolve()
    workspace, expected, protected = prepare(directory)
    project_nonce_hash = digest(expected['nonce'].encode())
    ui_input = {"directory": str(directory), "workspace": str(workspace), "client": args.client, "model": args.model, "ui_url": args.ui_url, "profile": args.profile, "test_command": TEST_COMMAND}
    if args.catalog_recovery_probe:
        ui_input['catalog_recovery_probe'] = True
    ui_input.update(email='user@devin-e2e.invalid', password=os.environ['DEVIN_JOURNEY_UI_PASSWORD'], key_name='e2e-key',
                    expected_key_sha256=digest(os.environ['DEVIN_JOURNEY_GATEWAY_KEY'].encode()))
    kwargs = isolation(args.client)
    ui=start_child(['node',str(ROOT/'ui-handoff.mjs')],directory,project_environment(directory),gateway=args.ui_url,client=args.client,
                   stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
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
                          'client': args.client, 'profile': args.profile, 'tier': tier, 'eligible_for_real_e2e': False,'isolation':isolated}))
        return 0
    if ui.returncode or not handoff.get("passed"):
        print(json.dumps({"status": "failed", "stage": "product-ui-handoff", "ui": handoff, "tier": tier})); return 1
    if args.stage == 'handoff':
        print(json.dumps({'status':'handoff_only', 'handoff':handoff, 'client_identity':client_identity,
                          'client':args.client, 'profile':args.profile, 'tier':tier, 'eligible_for_real_e2e':False,'isolation':isolated}))
        return 0
    source_before_task = digest(project_bytes(workspace, 'price_math.py'))
    first = invoke(args, directory, protected=protected)
    # Snapshot before executing any oracle: a correct output alone cannot prove
    # the client repaired the source, nor can a later helper supply the change.
    source_after_task = digest(project_bytes(workspace, 'price_math.py'))
    artifact = workspace / "out/report.json"
    valid = artifact.exists() and json.loads(project_bytes(workspace, 'out/report.json')) == expected
    intact = all(digest(project_bytes(workspace, name)) == fingerprint for name, fingerprint in protected.items())
    # An independent oracle verifies the client's changes; it is not counted as
    # the client running tests and cannot repair or generate the final artifact.
    oracle_samples = [[100 + int.from_bytes(os.urandom(2), "big"), byte] for byte in os.urandom(16)]
    after = run_project_python(directory, ['-c', "import json,sys; from price_math import discounted; assert all(discounted(a,b)==a-b for a,b in json.load(sys.stdin))"],
                               json.dumps(oracle_samples).encode()) if intact and first['exit_code'] is not None and not first['timed_out'] and first['incomplete_tool_calls'] == 0 else None
    task_checks = {'terminal': first['passed'], 'artifact': valid, 'protected_files': intact,
                   'project_nonce': first['project_nonce_sha256'] == project_nonce_hash,
                   'reads': first['tools']['read'] >= 3,
                   'source_mutation_tool': sum(first['source_mutation_tools'].values()) >= 1,
                   'source_changed': source_before_task != source_after_task,
                   'client_tests': first['client_tests_succeeded'], 'oracle': after.returncode == 0 if after is not None else None}
    task_passed = all(task_checks.values())
    source_before_resume = source_after_task
    resumed = invoke(args, directory, True, protected=protected) if task_passed else None
    same_session = resumed is not None and first["client_session_sha256"] is not None and first["client_session_sha256"] == resumed["client_session_sha256"]
    resume_nonce_valid = resumed is not None and resumed['project_nonce_sha256'] == project_nonce_hash
    extended = {}
    if (os.environ.get('DEVIN_JOURNEY_CAPABILITIES') in {'true', 'independent'} and same_session and resume_nonce_valid and resumed['passed']
            and args.client == 'omp' and args.profile == 'compatibility' and not args.model.startswith('fusion-')):
        import capabilities
        extended = capabilities.run(args, directory, first['client_session_sha256'], expected['nonce'], sys.modules[__name__])
    final = final_project_snapshot(workspace, expected, protected, source_before_resume)
    if resumed is None:
        final['artifact_verified_after_resume'] = final['source_unchanged_after_resume'] = None
    passed = task_passed and resumed is not None and resumed["passed"] and resume_nonce_valid and resumed["tools"]["read"] >= 1 and same_session and all(final[key] for key in ['artifact_verified_after_resume', 'protected_project_files_intact', 'source_unchanged_after_resume'])
    report = {"tier": tier, "status": "passed" if passed else "failed", "eligible_for_real_e2e": False,
              "execution_isolation": "separate_uid" if os.geteuid()==0 else "networkless_container_fixed_unix_relay",
              "isolation":isolated,
              "client_identity": client_identity,
              "client": args.client, "profile": args.profile, "handoff": handoff, "task": first, "resume": resumed,
              'task_checks': task_checks, 'source_before_task_sha256': source_before_task,
              'source_after_task_sha256': source_after_task, 'source_changed_by_task': task_checks['source_changed'],
              'project_nonce_sha256': project_nonce_hash,
              'protected_project_sha256': protected,
              "initial_project_test_failed": True, "artifact_verified_before_oracle": valid, **final,
              "independent_project_test_executed": after is not None,
              "independent_project_test_passed": after.returncode == 0 if after is not None else None,
              "same_client_session": same_session,
              "context_pressure": extended.get('context_pressure', 'not_run'), "compact": extended.get('compact', 'not_run'),
              "fork": extended.get('fork', 'not_run'), "disconnect_recovery": 'not_run', "worker": "not_run",
              "capability_evidence": extended}
    print(json.dumps(report, sort_keys=True))
    return 0 if passed else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (ValueError, OSError, KeyError, subprocess.TimeoutExpired):
        print(json.dumps({"status": "failed", "stage": "runner-boundary", "tier": "client_mock"}))
        raise SystemExit(2)
