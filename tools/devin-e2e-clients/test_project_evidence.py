import json
from pathlib import Path
import tempfile
import unittest

from project_evidence import ToolEvidence, synthetic_command_hint, tool_error_categories


def event(name, status='completed', inputs=None, call_id='call-1'):
    return {'type': 'tool_use', 'part': {'callID': call_id, 'tool': name,
            'state': {'status': status, 'input': inputs or {}, **({'metadata':{'exit':0}} if name=='bash' and status=='completed' else {})}}}


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

    def test_synthetic_failure_hint_is_opt_in_and_never_copies_values_or_paths(self):
        command = "python -m pytest --token 'ghp_synthetic_private_token' /home/private/project.py"
        value = event('bash', inputs={'command': command})
        value['part']['state'].update(metadata={'exit': 127}, output='bash: python: command not found\nprivate output')
        self.tracker.observe(value)
        self.assertNotIn('failure_diagnostic', self.tracker.report()['tool_calls'][0])
        tracker = ToolEvidence(self.workspace, 'python3 run_tests.py', synthetic_failure_diagnostics=True)
        tracker.observe(value)
        diagnostic = tracker.report()['tool_calls'][0]['failure_diagnostic']
        self.assertEqual(['python', 'pytest'], diagnostic['recognized_program_tokens'])
        self.assertEqual(['command_not_found'], diagnostic['output_categories'])
        self.assertEqual(127, diagnostic['client_exit_code'])
        self.assertEqual('client_combined_tool_output', diagnostic['output_source'])
        self.assertFalse(diagnostic['raw_command_or_output_retained'])
        encoded = json.dumps(tracker.report())
        for private in ['ghp_synthetic_private_token', '/home/private', 'project.py', 'private output', '--token']:
            self.assertNotIn(private, encoded)

    def test_failure_preview_is_bounded_and_does_not_reveal_unknown_programs(self):
        for command in ['private_program ' + 'private-value ' * 10000, "python 'unterminated-private-value"]:
            hint = synthetic_command_hint(command)
            self.assertLessEqual(len(hint['command_preview']), 240)
            self.assertNotIn('private', json.dumps(hint))
        hint = synthetic_command_hint('/private/path/python3 run_tests.py || python -m pytest')
        self.assertEqual('<path>/python3 run_tests.py || python -m pytest', hint['command_preview'])
        self.assertEqual(['python3', 'python', 'pytest'], hint['recognized_program_tokens'])

    def test_successful_shell_fallback_is_not_classified_as_a_failed_tool(self):
        tracker = ToolEvidence(self.workspace, 'python3 run_tests.py', synthetic_failure_diagnostics=True)
        value = event('bash', inputs={'command': 'python run_tests.py || python3 run_tests.py'})
        value['part']['state'].update(metadata={'exit': 0}, output='bash: python: command not found\nOK')
        tracker.observe(value)
        self.assertNotIn('failure_diagnostic', tracker.report()['tool_calls'][0])

    def test_omp_start_end_join_failure_hint_and_keep_only_output_categories(self):
        tracker = ToolEvidence(self.workspace, 'python3 run_tests.py', synthetic_failure_diagnostics=True)
        tracker.observe({'type': 'tool_execution_start', 'toolCallId': 'omp-private', 'toolName': 'bash',
                         'args': {'command': 'python3 -m private_module'}})
        tracker.observe({'type': 'tool_execution_end', 'toolCallId': 'omp-private', 'toolName': 'bash', 'isError': True,
                         'result': {'details': {'exitCode': 1}, 'content': [{'text': 'No module named private_module\n' + 'private-output' * 2000}]}})
        diagnostic = tracker.report()['tool_calls'][0]['failure_diagnostic']
        self.assertEqual(['python_module_missing'], diagnostic['output_categories'])
        self.assertTrue(diagnostic['output_classification_truncated'])
        self.assertNotIn('private', json.dumps(tracker.report()))

    def test_completed_nonzero_and_returned_local_error_are_recoverable(self):
        for name, status, code, detail in [('bash','completed',127,'bash: python: command not found'),
                                          ('read','error',None,'File not found: synthetic.txt'),
                                          ('edit','error',None,'oldString not found in content')]:
            with self.subTest(name=name):
                tracker=ToolEvidence(self.workspace,'python3 run_tests.py')
                value=event(name,status)
                value['part']['state'].update(metadata={'exit':code} if code is not None else {},
                                             **({'error':detail} if status=='error' else {'output':detail}))
                tool=tracker.observe(value)
                self.assertTrue(tool['recoverable_error'])
                self.assertIsNone(tool['fatal_reason'])
                self.assertFalse(tracker.failed)

    def test_permissions_timeout_async_and_ambiguous_execution_remain_fatal(self):
        cases=[({'exit':1},'Permission denied','permission_rejection'),
               ({'exit':127,'timedOut':True},'command not found','transport_uncertain'),
               ({'exit':1,'async':True},'failed','transport_uncertain'),
               ({},'missing exit','transport_uncertain'),
               ({'exit':1},'request timed out; remote outcome unknown','transport_uncertain')]
        for metadata,detail,reason in cases:
            with self.subTest(metadata=metadata):
                tracker=ToolEvidence(self.workspace,'python3 run_tests.py')
                value=event('bash');value['part']['state'].update(metadata=metadata,output=detail)
                tool=tracker.observe(value)
                self.assertEqual(reason,tool['fatal_reason'])
                self.assertFalse(tool['recoverable_error']);self.assertTrue(tracker.failed)

    def test_changed_or_missing_tool_identity_is_rejected(self):
        self.tracker.observe(event('edit', 'running'))
        for value in [event('write'), event('edit', call_id=None), event('edit', status='unknown')]:
            with self.subTest(event=value), self.assertRaises(ValueError):
                self.tracker.observe(value)


if __name__ == '__main__':
    unittest.main()
