"""Per-call project-test evidence. Raw commands, output and IDs stay in memory."""
import hashlib
import json
import re
import shlex
import time

PREFIX = 'SUB2API_TEST_RECEIPT_V1:'
SCHEMA = 'devin_project_test_v1'
HEX = re.compile(r'^[a-f0-9]{64}$')
RUN_ID = re.compile(r'^[a-f0-9]{32}$')
PROTECTED = ('requirements.json', 'test_project.py', 'run_tests.py')

# The real project runner produces this receipt; the observer never creates it.
TEST_RUNNER = '''import hashlib
import json
import os
from pathlib import Path
import sys
import time
import unittest
import uuid

started = time.time_ns()
sha = lambda data: hashlib.sha256(data).hexdigest()
protected = {name: sha(Path(name).read_bytes()) for name in ("requirements.json", "test_project.py", "run_tests.py")}
source_before = sha(Path("price_math.py").read_bytes())
nonce = json.loads(Path("requirements.json").read_text())["nonce"]
result = unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.discover("."))
passed = result.wasSuccessful() and result.testsRun > 0 and not result.skipped and not result.expectedFailures
exit_code = 0 if passed else 1
proof = {"schema": "devin_project_test_v1", "run_id": uuid.uuid4().hex,
         "started_ns": started, "finished_ns": time.time_ns(), "project_nonce_sha256": sha(nonce.encode()),
         "protected_sha256": protected, "source_before_sha256": source_before,
         "source_after_sha256": sha(Path("price_math.py").read_bytes()),
         "tests_run": result.testsRun, "failures": len(result.failures), "errors": len(result.errors),
         "skipped": len(result.skipped), "expected_failures": len(result.expectedFailures),
         "unexpected_successes": len(result.unexpectedSuccesses), "exit_code": exit_code, "passed": passed}
encoded = json.dumps(proof, sort_keys=True, separators=(",", ":"))
directory = Path("out/test-receipts")
directory.mkdir(parents=True, exist_ok=True)
target = directory / (proof["run_id"] + ".json")
temporary = target.with_suffix(".tmp")
with temporary.open("x") as output:
    output.write(encoded)
    output.flush()
    os.fsync(output.fileno())
os.replace(temporary, target)
reference = {"run_id": proof["run_id"], "sha256": sha(encoded.encode())}
print("SUB2API_TEST_RECEIPT_V1:" + json.dumps(reference, sort_keys=True, separators=(",", ":")), flush=True)
sys.exit(exit_code)
'''


def digest(value):
    return hashlib.sha256(value).hexdigest()


def command_class(command, expected):
    """Classify literal argv only; CLI product permissions still decide execution."""
    if not isinstance(command, str):
        return 'unidentified'
    # Do not interpret shell operators, expansion, control bytes or redirection.
    if not re.fullmatch(r'''[A-Za-z0-9_./ '"\t-]+''', command):
        return 'other_shell'
    try:
        words = shlex.split(command, posix=True)
    except ValueError:
        return 'invalid_syntax'
    return 'configured_project_tests' if words == shlex.split(expected, posix=True) else 'other_literal'


def output_text(event):
    if event.get('type') == 'tool_use':
        value = event.get('part', {}).get('state', {}).get('output')
        return value if isinstance(value, str) else ''
    content = event.get('result', {}).get('content', [])
    return '\n'.join(item.get('text', '') for item in content if isinstance(item, dict) and isinstance(item.get('text'), str)) if isinstance(content, list) else ''


def terminal_details(event):
    if event.get('type') == 'tool_use':
        state = event.get('part', {}).get('state', {})
        details = state.get('metadata', {})
        if not isinstance(details, dict):
            return None, 'unverified', True
        code = details.get('exit')
        return code if type(code) is int else None, 'client_metadata', bool(type(code) is not int or details.get('timedOut') or details.get('async'))
    details = event.get('result', {}).get('details', {})
    if not isinstance(details, dict):
        return None, 'unverified', True
    code = details.get('exitCode')
    # Pinned OMP omits exitCode on successful synchronous commands. This alone
    # proves no exit value; the matching runner receipt must supply it as well.
    source = 'client_metadata' if type(code) is int else 'omp_success_and_runner_receipt'
    blocked = bool(details.get('timedOut') or details.get('async') or ('exitCode' in details and type(code) is not int)
                   or (code is None and event.get('isError') is not False))
    return code if type(code) is int else None, source, blocked


class TestReceipts:
    def __init__(self, read_project, nonce, protected, started_ns):
        self.read = read_project
        self.nonce_hash = digest(nonce.encode())
        self.protected = dict(protected)
        self.started_ns = started_ns
        self.calls = {}
        self.run_ids = {}
        self.sequence = 0
        self.failed = False

    def observe(self, tool, event):
        if not tool or tool['name'] != 'bash':
            return
        call_hash = tool['call_sha256']
        record = self.calls.setdefault(call_hash, {'call_sha256': call_hash, 'command_class': tool.get('command_class', 'unidentified'),
            'command_sha256': tool.get('command_sha256'), 'state': tool['status'], 'exit_code': None,
            'exit_source': 'unverified', 'receipt_status': 'not_terminal', 'execution_verified': False, 'passed': None})
        record['state'] = tool['status']
        record['tool_error_kind'] = tool.get('error_kind')
        record['command_class'] = tool.get('command_class', 'unidentified')
        record['command_sha256'] = tool.get('command_sha256')
        if tool['status'] not in {'completed', 'error'}:
            return
        text = output_text(event)
        code, exit_source, blocked = terminal_details(event)
        terminal_hash = digest(json.dumps([tool['status'], code, exit_source, blocked, digest(text.encode())], separators=(',', ':')).encode())
        if '_terminal' in record:
            if record['_terminal'] != terminal_hash:
                self.failed = True
                raise ValueError('test_tool_terminal_changed')
            return
        record['_terminal'] = terminal_hash
        self.sequence += 1
        record['completion_sequence'] = self.sequence
        record['exit_code'], record['exit_source'] = code, exit_source
        record['receipt_status'] = 'not_test_command' if record['command_class'] != 'configured_project_tests' else 'missing'
        if record['command_class'] != 'configured_project_tests':
            return
        lines = [line[len(PREFIX):] for line in text.splitlines() if line.startswith(PREFIX)]
        if len(lines) != 1:
            record['receipt_status'] = 'missing' if not lines else 'ambiguous'
            return
        record['receipt_output_bytes'] = len(lines[0].encode())
        record['receipt_output_sha256'] = digest(lines[0].encode())
        record['receipt_output_closed_object'] = lines[0].rstrip().endswith('}')
        try:
            reference = json.loads(lines[0])
        except ValueError:
            record['receipt_status'] = 'output_invalid_json'
            return
        try:
            if (not isinstance(reference, dict) or set(reference) != {'run_id', 'sha256'}
                    or not RUN_ID.fullmatch(str(reference['run_id'])) or not HEX.fullmatch(str(reference['sha256']))):
                record['receipt_status'] = 'invalid_output_reference'
                return
            run_id = reference['run_id']
            raw = self.read('out/test-receipts/' + run_id + '.json')
            if digest(raw) != reference['sha256']:
                record['receipt_status'] = 'file_output_mismatch'
                return
            proof = json.loads(raw)
            if not isinstance(proof, dict) or proof.get('run_id') != run_id:
                record['receipt_status'] = 'run_identity_mismatch'
                return
            status = self.validate(proof, code, blocked)
            record['receipt_status'] = status
            if status != 'verified':
                return
            if run_id in self.run_ids and self.run_ids[run_id] != call_hash:
                record['receipt_status'] = 'reused_by_another_call'
                self.failed = True
                return
            self.run_ids[run_id] = call_hash
            record.update(execution_verified=True, passed=proof['passed'], exit_code=proof['exit_code'],
                receipt_sha256=digest(raw), output_reference_sha256=reference['sha256'], run_sha256=digest(run_id.encode()), project_nonce_sha256=proof['project_nonce_sha256'],
                protected_sha256=proof['protected_sha256'], source_before_sha256=proof['source_before_sha256'],
                source_after_sha256=proof['source_after_sha256'], started_ns=proof['started_ns'], finished_ns=proof['finished_ns'],
                tests_run=proof['tests_run'], failures=proof['failures'], errors=proof['errors'], skipped=proof['skipped'],
                expected_failures=proof['expected_failures'], unexpected_successes=proof['unexpected_successes'])
        except OSError:
            record['receipt_status'] = 'file_unavailable'
        except ValueError:
            record['receipt_status'] = 'file_invalid_json'
        except (KeyError, TypeError):
            record['receipt_status'] = 'invalid_value'

    def validate(self, proof, code, blocked):
        fields = {'schema', 'run_id', 'started_ns', 'finished_ns', 'project_nonce_sha256', 'protected_sha256',
                  'source_before_sha256', 'source_after_sha256', 'tests_run', 'failures', 'errors', 'skipped',
                  'expected_failures', 'unexpected_successes', 'exit_code', 'passed'}
        if not isinstance(proof, dict) or set(proof) != fields or proof['schema'] != SCHEMA or not RUN_ID.fullmatch(str(proof['run_id'])):
            return 'invalid_schema'
        counts = ('tests_run', 'failures', 'errors', 'skipped', 'expected_failures', 'unexpected_successes')
        if any(type(proof[key]) is not int or proof[key] < 0 for key in counts) or type(proof['passed']) is not bool or type(proof['exit_code']) is not int:
            return 'invalid_result'
        passed = proof['tests_run'] > 0 and not any(proof[key] for key in counts[1:])
        if proof['passed'] != passed or proof['exit_code'] != (0 if passed else 1):
            return 'inconsistent_result'
        if any(type(proof[key]) is not int for key in ('started_ns', 'finished_ns')) or not self.started_ns <= proof['started_ns'] <= proof['finished_ns'] <= time.time_ns():
            return 'stale_or_invalid_time'
        if proof['project_nonce_sha256'] != self.nonce_hash or proof['protected_sha256'] != self.protected:
            return 'wrong_project_or_tests'
        if set(self.protected) != set(PROTECTED) or any(digest(self.read(name)) != fingerprint for name, fingerprint in self.protected.items()):
            return 'protected_files_changed'
        source = digest(self.read('price_math.py'))
        if proof['source_before_sha256'] != source or proof['source_after_sha256'] != source:
            return 'source_changed'
        if blocked or (code is not None and code != proof['exit_code']):
            return 'exit_unverified'
        return 'verified'

    def report(self):
        calls = [{key: value for key, value in record.items() if not key.startswith('_')} for record in self.calls.values()]
        candidates = [record for record in calls if record['command_class'] == 'configured_project_tests']
        selected = max(candidates, key=lambda record: record.get('completion_sequence', -1), default=None)
        try:
            source = digest(self.read('price_math.py'))
        except (OSError, ValueError):
            source = None
        passed = bool(selected and selected['execution_verified'] and selected['passed'] is True and selected.get('source_after_sha256') == source and not self.failed)
        return {'test_execution': {'schema': SCHEMA, 'calls': calls, 'selected_call_sha256': selected['call_sha256'] if selected else None,
                'source_after_task_sha256': source, 'passed': passed, 'conflicting_evidence': self.failed},
                'client_tests_succeeded': passed, 'test_command_verified': bool(selected and selected['execution_verified']),
                'bash_exit_code': selected['exit_code'] if selected and selected['execution_verified'] else None}
