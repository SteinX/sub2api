"""Structural evidence with opt-in, allowlisted synthetic failure diagnostics."""
from collections import Counter
import os
from pathlib import Path
import re
import shlex
import time
from project_tests import command_category, command_class, digest, output_text, terminal_details

FILE_MUTATORS = frozenset({'write', 'edit', 'apply_patch'})
TOOL_NAME = re.compile(r'^[A-Za-z0-9_.:-]{1,128}$')
PROGRAM_TOKENS = frozenset({'bash', 'sh', 'python', 'python3', 'pytest', 'pytest3', 'git', 'rg',
    'ls', 'pwd', 'cat', 'tee', 'find', 'node', 'npm', 'pnpm', 'make', 'env', 'echo', 'printf', 'true', 'false'})
SAFE_COMMAND_TOKENS = PROGRAM_TOKENS | frozenset({'run_tests.py', './run_tests.py', 'test_project.py',
    'price_math.py', 'requirements.json', 'out/report.json', '-u', '-m', '-c', '--version', '--help',
    '&&', '||', '|', ';', '>', '>>', '<', '&', '(', ')'})


def synthetic_command_hint(command):
    """A bounded lexical hint, never shell interpretation or command authority.

    Only fixed public words survive; values, source, paths and credentials do
    not. Even an unknown executable is redacted rather than copied verbatim.
    """
    lexer = shlex.shlex(command[:4096], posix=True, punctuation_chars=';&|()<>')
    lexer.whitespace_split = True
    lexer.commenters = ''
    tokens = []
    truncated = len(command) > 4096
    try:
        for _ in range(25):
            token = lexer.get_token()
            if not token:
                break
            tokens.append(token)
    except ValueError:
        return {'command_preview': '<unparsed>', 'recognized_program_tokens': [], 'command_truncated': truncated}
    truncated |= len(tokens) > 24
    rendered, programs = [], []
    for token in tokens[:24]:
        base = Path(token).name if '/' in token else token
        if base in PROGRAM_TOKENS and base not in programs:
            programs.append(base)
        rendered.append(token if token in SAFE_COMMAND_TOKENS else
                        '<path>/' + base if '/' in token and base in PROGRAM_TOKENS else '<redacted>')
    preview = ' '.join(rendered)
    return {'command_preview': preview[:240], 'recognized_program_tokens': programs,
            'command_truncated': truncated or len(preview) > 240}


def tool_error_categories(detail):
    """Fixed text markers, not root-cause claims or retained error bodies."""
    if not isinstance(detail, str):
        return ['unavailable']
    patterns = {
        'ripgrep_error': r'ripgrep',
        'missing_path': r'\benoent\b|no such file|executable (?:file )?not found',
        'access_denied': r'\beacces\b|\beperm\b|permission denied|operation not permitted',
        'read_only_filesystem': r'\berofs\b|read.only file system',
        'network_error': r'\benotfound\b|network is unreachable|fetch failed|tunnel connection failed',
        'invalid_arguments': r'invalid arguments|invalid input|schema validation',
        'command_not_found': r'command not found|(?:^|\n)[^\n]{0,160}:\s*not found\b',
        'python_module_missing': r'\bmodulenotfounderror\b|no module named',
        'shell_syntax_error': r'syntax error|unexpected token',
        'execution_uncertain': r'\betimedout\b|\beconnreset\b|timed out|timeout expired|connection reset|socket hang up|broken pipe|\baborted\b|\bcancell?ed\b|still running',
    }
    return [name for name, pattern in patterns.items() if re.search(pattern, detail, re.I)] or ['unclassified']


def tool_failure(record, event):
    """Return completed ordinary failures as feedback; uncertainty stays fatal.

    This does not retry a command or establish arbitrary external side effects.
    Public model sends retain their separate transport/accounting guards.
    """
    if record['status'] not in {'completed', 'error'}:
        return {}
    state = event.get('part', {}).get('state', {})
    detail = state.get('error')
    detail = detail if isinstance(detail, str) else output_text(event)
    categories = tool_error_categories(detail)
    metadata = state.get('metadata', {}) if event.get('type') == 'tool_use' else event.get('result', {}).get('details', {})
    code, _, blocked = terminal_details(event) if record['name'] == 'bash' else (None, '', False)
    failure = record['status'] == 'error' or code is not None and code != 0
    reason = None
    if record.get('error_kind') == 'client_permission_rejection' or failure and ('access_denied' in categories or code == 126):
        reason = 'permission_rejection'
    elif (not isinstance(metadata, dict) or metadata.get('timedOut') or metadata.get('async')
          or record['name'] == 'bash' and blocked
          or failure and (not detail and code is None or any(c in categories for c in ['execution_uncertain', 'network_error'])
                          or code in {124, 130, 137, 143})):
        reason = 'transport_uncertain'
    return {'client_exit_code': code, 'fatal_reason': reason, 'recoverable_error': failure and reason is None}


class ToolEvidence:
    def __init__(self, workspace, test_command, *, synthetic_failure_diagnostics=False):
        self.workspace = Path(workspace)
        self.test_command = test_command
        self.calls = {}
        self.events = {}
        self.failed = False
        self.synthetic_failure_diagnostics = synthetic_failure_diagnostics
        self.command_hints = {}

    def source_targeted(self, name, inputs):
        if name not in FILE_MUTATORS or not isinstance(inputs, dict):
            return False
        def target(value):
            if not isinstance(value, str):
                return False
            return os.path.abspath(self.workspace / value) == str(self.workspace / 'price_math.py')
        if name == 'apply_patch':
            patch = inputs.get('patchText', inputs.get('patch', ''))
            return isinstance(patch, str) and any(target(line.partition(': ')[2]) for line in patch.splitlines()
                if line.startswith(('*** Update File: ', '*** Add File: ', '*** Move to: ')))
        return any(target(inputs.get(field)) for field in ('filePath', 'file_path', 'path'))

    def observe(self, event):
        kind = event.get('type')
        if kind == 'tool_use':
            part = event.get('part', {})
            state = part.get('state', {})
            name, status = part.get('tool'), state.get('status')
            call_id = part.get('callID') or part.get('id')
            inputs = state.get('input', {})
        elif kind in {'tool_execution_start', 'tool_execution_end'}:
            name, call_id = event.get('toolName'), event.get('toolCallId')
            status = 'running' if kind == 'tool_execution_start' else ('completed' if event.get('isError') is False else 'error')
            inputs = event.get('args') if kind == 'tool_execution_start' else None
        else:
            return None
        if (not isinstance(name, str) or not TOOL_NAME.fullmatch(name) or not isinstance(call_id, str) or not call_id
                or status not in {'pending', 'running', 'completed', 'error'}):
            raise ValueError('unidentified_tool_event')
        previous = self.calls.get(call_id)
        if previous and previous['name'] != name:
            raise ValueError('tool_identity_changed')
        # Duplicate terminal events are observations, not additional tool calls.
        if previous and previous['status'] in {'completed', 'error'} and status != previous['status']:
            raise ValueError('tool_terminal_changed')
        record = previous or {'name': name, 'call_sha256': digest(call_id.encode()), 'source_targeted': False,
                              'test_command': False, 'states': [], 'error_kind': None, 'first_observed_at_ns': time.time_ns(), 'observations': []}
        if inputs is not None:
            record['source_targeted'] |= self.source_targeted(name, inputs)
            if name == 'bash' and isinstance(inputs, dict) and isinstance(inputs.get('command'), str):
                fingerprint = digest(inputs['command'].encode())
                if record.get('command_sha256') not in {None, fingerprint}:
                    raise ValueError('tool_command_changed')
                record['command_sha256'] = fingerprint
                record['command_class'] = command_class(inputs['command'], self.test_command)
                record['command_category'] = command_category(inputs['command'], self.test_command, self.workspace)
                record['test_command'] = record['command_class'] == 'configured_project_tests'
                if self.synthetic_failure_diagnostics:
                    self.command_hints[call_id] = synthetic_command_hint(inputs['command'])
        record['status'] = status
        if name == 'bash' and status in {'completed', 'error'} and self.synthetic_failure_diagnostics:
            code, _, _ = terminal_details(event)
            hint = self.command_hints.pop(call_id, None)
            if status == 'error' or code is not None and code != 0:
                detail = event.get('part', {}).get('state', {}).get('error')
                source = 'client_error' if isinstance(detail, str) else 'client_combined_tool_output'
                detail = detail if isinstance(detail, str) else output_text(event)
                sample = detail if len(detail) <= 8192 else detail[:4096] + detail[-4096:]
                record['failure_diagnostic'] = {
                    'scope': 'synthetic_project_only', 'client_exit_code': code,
                    **(hint or {'command_preview': '<unavailable>', 'recognized_program_tokens': [], 'command_truncated': False}),
                    'output_source': source, 'output_categories': tool_error_categories(sample),
                    'output_classification_truncated': len(detail) > 8192,
                    'raw_command_or_output_retained': False,
                }
        if status == 'error':
            # Classify the actual client error without retaining its body/rules.
            detail = (event.get('part', {}).get('state', {}).get('error') if kind == 'tool_use' else None)
            text = detail.lower() if isinstance(detail, str) else ''
            denied = ('permission' in text and any(word in text for word in ['deny', 'denied', 'reject'])) or text.startswith('the user has specified a rule which prevents')
            record['error_kind'] = 'client_permission_rejection' if denied else 'client_tool_error'
            record['tool_error_categories'] = tool_error_categories(detail)
        record.update(tool_failure(record, event))
        if not record['states'] or record['states'][-1] != status:
            record['states'].append(status)
            record['observations'].append({'status': status, 'observed_at_ns': time.time_ns()})
        self.calls[call_id] = record
        self.events.setdefault(name, Counter())[status] += 1
        self.failed |= bool(record.get('fatal_reason'))
        return record

    def report(self):
        states = {}
        tools = {'read': 0, 'write': 0, 'bash': 0}
        mutations = Counter()
        for record in self.calls.values():
            name, status = record['name'], record['status']
            states.setdefault(name, Counter())[status] += 1
            tools.setdefault(name, 0)
            if status == 'completed':
                tools[name] += 1
                if record['source_targeted']:
                    mutations[name] += 1
        return {'tools': tools, 'tool_states': states, 'tool_events': self.events,
                'recoverable_tool_errors': sum(r.get('recoverable_error') is True for r in self.calls.values()),
                'tool_calls': list(self.calls.values()),
                'source_mutation_tools': dict(mutations),
                'incomplete_tool_calls': sum(r['status'] not in {'completed', 'error'} for r in self.calls.values())}
