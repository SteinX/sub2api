"""Structural tool evidence; never persist tool inputs, outputs or raw call IDs."""
from collections import Counter
import os
from pathlib import Path
import re
from project_tests import command_class, digest

FILE_MUTATORS = frozenset({'write', 'edit', 'apply_patch'})
TOOL_NAME = re.compile(r'^[A-Za-z0-9_.:-]{1,128}$')


class ToolEvidence:
    def __init__(self, workspace, test_command):
        self.workspace = Path(workspace)
        self.test_command = test_command
        self.calls = {}
        self.events = {}
        self.failed = False

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
                              'test_command': False, 'states': []}
        if inputs is not None:
            record['source_targeted'] |= self.source_targeted(name, inputs)
            if name == 'bash' and isinstance(inputs, dict) and isinstance(inputs.get('command'), str):
                fingerprint = digest(inputs['command'].encode())
                if record.get('command_sha256') not in {None, fingerprint}:
                    raise ValueError('tool_command_changed')
                record['command_sha256'] = fingerprint
                record['command_class'] = command_class(inputs['command'], self.test_command)
                record['test_command'] = record['command_class'] == 'configured_project_tests'
        record['status'] = status
        if not record['states'] or record['states'][-1] != status:
            record['states'].append(status)
        self.calls[call_id] = record
        self.events.setdefault(name, Counter())[status] += 1
        self.failed |= status == 'error'
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
                'tool_calls': list(self.calls.values()),
                'source_mutation_tools': dict(mutations),
                'incomplete_tool_calls': sum(r['status'] not in {'completed', 'error'} for r in self.calls.values())}
