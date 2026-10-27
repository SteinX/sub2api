import json
from pathlib import Path
import tempfile
import unittest

from project_evidence import ToolEvidence, tool_error_categories


def event(name, status='completed', inputs=None, call_id='call-1'):
    return {'type': 'tool_use', 'part': {'callID': call_id, 'tool': name,
            'state': {'status': status, 'input': inputs or {}}}}


class ProjectToolEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.workspace = Path(self.directory.name)
        self.tracker = ToolEvidence(self.workspace, 'python3 run_tests.py')

    def test_write_edit_and_patch_keep_actual_names_and_source_targets(self):
        for name, inputs in [('write', {'filePath': str(self.workspace / 'price_math.py')}),
                             ('edit', {'filePath': './price_math.py'}),
                             ('apply_patch', {'patchText': '*** Begin Patch\n*** Update File: price_math.py\n@@\n-a\n+b\n*** End Patch'})]:
            with self.subTest(tool=name):
                tracker = ToolEvidence(self.workspace, 'python3 run_tests.py')
                tracker.observe(event(name, inputs=inputs))
                report = tracker.report()
                self.assertEqual({name: 1}, report['source_mutation_tools'])
                self.assertEqual({'completed': 1}, report['tool_states'][name])
                self.assertEqual(1, report['tools'][name])

    def test_failure_pending_unrelated_targets_and_bash_cannot_prove_source_mutation(self):
        for name, status, inputs in [('edit', 'error', {'filePath': 'price_math.py'}),
                                     ('write', 'running', {'filePath': 'price_math.py'}),
                                     ('edit', 'completed', {'filePath': 'out/other.py'}),
                                     ('apply_patch', 'completed', {'patchText': '*** Update File: other.py'}),
                                     ('bash', 'completed', {'command': 'python3 run_tests.py'})]:
            with self.subTest(tool=name, status=status):
                tracker = ToolEvidence(self.workspace, 'python3 run_tests.py')
                tracker.observe(event(name, status, inputs))
                self.assertEqual({}, tracker.report()['source_mutation_tools'])
                self.assertEqual(status == 'error', tracker.failed)
                self.assertEqual(int(status == 'running'), tracker.report()['incomplete_tool_calls'])

    def test_duplicate_completion_is_not_another_call_and_inputs_are_not_retained(self):
        value = event('edit', inputs={'filePath': 'price_math.py', 'oldString': 'synthetic-private', 'newString': 'replacement'})
        self.tracker.observe(value)
        self.tracker.observe(value)
        report = self.tracker.report()
        self.assertEqual(1, report['tools']['edit'])
        self.assertEqual({'completed': 2}, report['tool_events']['edit'])
        self.assertEqual({'completed': 1}, report['tool_states']['edit'])
        for secret in ['synthetic-private', 'replacement', 'price_math.py', 'call-1']:
            self.assertNotIn(secret, json.dumps(report))

    def test_omp_start_end_are_joined_and_every_tool_failure_is_visible(self):
        self.tracker.observe({'type': 'tool_execution_start', 'toolCallId': 'omp-1', 'toolName': 'write', 'args': {'path': 'price_math.py'}})
        self.tracker.observe({'type': 'tool_execution_end', 'toolCallId': 'omp-1', 'toolName': 'write', 'isError': False})
        self.tracker.observe(event('custom_tool', 'error', call_id='custom-1'))
        report = self.tracker.report()
        self.assertEqual({'write': 1}, report['source_mutation_tools'])
        self.assertEqual({'error': 1}, report['tool_states']['custom_tool'])
        self.assertEqual(0, report['tools']['custom_tool'])
        self.assertTrue(self.tracker.failed)

    def test_command_categories_retain_no_raw_command_or_path(self):
        for index, (command, category) in enumerate([
            ('pwd', 'pwd'), ('ls /private/synthetic-secret', 'ls'),
            ('python3 ./run_tests.py', 'test_path_alias'),
            ('rm /private/synthetic-secret', 'other_literal'),
        ]):
            self.tracker.observe(event('bash', 'error', call_id=f'category-{index}', inputs={'command': command}))
            call = self.tracker.report()['tool_calls'][-1]
            self.assertEqual(category, call['command_category'])
            self.assertFalse(call['test_command'])
        encoded = json.dumps(self.tracker.report())
        for raw in ['synthetic-secret', '/private/', 'python3 ./run_tests.py', 'category-0']:
            self.assertNotIn(raw, encoded)

    def test_tool_error_categories_keep_only_finite_markers(self):
        detail = 'Ripgrep.Error: ripgrep execution failed: ENOENT /private/synthetic-secret'
        value = event('glob', 'error', inputs={'pattern': '*.py'})
        value['part']['state']['error'] = detail
        self.tracker.observe(value)
        record = self.tracker.report()['tool_calls'][0]
        self.assertEqual('client_tool_error', record['error_kind'])
        self.assertEqual(['ripgrep_error', 'missing_path'], record['tool_error_categories'])
        self.assertNotIn('synthetic-secret', json.dumps(self.tracker.report()))
        self.assertEqual(['unclassified'], tool_error_categories('opaque synthetic-secret'))
        self.assertEqual(['unavailable'], tool_error_categories(None))

    def test_changed_or_missing_tool_identity_is_rejected(self):
        self.tracker.observe(event('edit', 'running'))
        for value in [event('write'), event('edit', call_id=None), event('edit', status='unknown')]:
            with self.subTest(event=value), self.assertRaises(ValueError):
                self.tracker.observe(value)


if __name__ == '__main__':
    unittest.main()
