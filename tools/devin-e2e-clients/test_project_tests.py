import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest

from project_evidence import ToolEvidence
from project_tests import PREFIX, PROTECTED, TEST_RUNNER, TestReceipts, command_category, command_class, digest


class ProjectTestReceiptTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.nonce = 'a' * 24
        for name, content in {'requirements.json': json.dumps({'nonce': self.nonce}), 'price_math.py': 'value = 1\n',
                              'test_project.py': 'import unittest\nfrom price_math import value\nclass Check(unittest.TestCase):\n def test_value(self): self.assertEqual(value, 1)\n',
                              'run_tests.py': TEST_RUNNER}.items():
            (self.root / name).write_text(content)
        self.protected = {name: digest((self.root / name).read_bytes()) for name in PROTECTED}
        self.started = time.time_ns()
        self.tracker = ToolEvidence(self.root, 'python3 run_tests.py')
        self.receipts = TestReceipts(lambda name: (self.root / name).read_bytes(), self.nonce, self.protected, self.started)

    def run_tests(self):
        return subprocess.run([sys.executable, 'run_tests.py'], cwd=self.root, capture_output=True, check=False)

    def event(self, result, command='python3 run_tests.py', call_id='synthetic-call', client='opencode', output=None, code='actual'):
        text = (result.stdout + result.stderr).decode() if output is None else output
        exit_code = result.returncode if code == 'actual' else code
        if client == 'opencode':
            event = {'type': 'tool_use', 'part': {'callID': call_id, 'tool': 'bash', 'state': {'status': 'completed',
                     'input': {'command': command}, 'metadata': {'exit': exit_code}, 'output': text}}}
        else:
            self.tracker.observe({'type': 'tool_execution_start', 'toolName': 'bash', 'toolCallId': call_id, 'args': {'command': command}})
            event = {'type': 'tool_execution_end', 'toolName': 'bash', 'toolCallId': call_id, 'isError': False,
                     'result': {'content': [{'type': 'text', 'text': text}], 'details': {} if exit_code is None else {'exitCode': exit_code}}}
        self.receipts.observe(self.tracker.observe(event), event)
        return event

    def test_literal_argv_is_not_arbitrary_shell(self):
        for command in ['python3 run_tests.py', 'python3   run_tests.py', 'python3\trun_tests.py', "python3 'run_tests.py'"]:
            self.assertEqual('configured_project_tests', command_class(command, 'python3 run_tests.py'))
        for command in ['python3 run_tests.py\necho OK', 'python3 run_tests.py && printf OK', 'python3 run_tests.py > out/fake',
                        'python3 $TEST', 'python3 `echo run_tests.py`', 'bash run_tests.py', 'python3 other.py']:
            self.assertNotEqual('configured_project_tests', command_class(command, 'python3 run_tests.py'))

    def test_finite_command_categories_do_not_authorize_or_retain_values(self):
        cases = {
            'python3 run_tests.py': 'exact_test_command',
            'python3   run_tests.py': 'test_argv_variant',
            "python3 'run_tests.py'": 'test_argv_variant',
            'python3 ./run_tests.py': 'test_path_alias',
            '/usr/bin/python3 run_tests.py': 'test_path_alias',
            f'python3 {self.root}/run_tests.py': 'test_path_alias',
            'pwd': 'pwd', '/bin/pwd -P': 'pwd',
            'ls /private/synthetic-secret': 'ls',
            'rm /private/synthetic-secret': 'other_literal',
            'python3 other.py': 'other_literal',
            'python3 -u run_tests.py': 'other_literal',
            'ls /private/synthetic-secret && pwd': 'other_shell',
        }
        for command, expected in cases.items():
            with self.subTest(category=expected):
                self.assertEqual(expected, command_category(command, 'python3 run_tests.py', self.root))
                if expected not in {'exact_test_command', 'test_argv_variant'}:
                    self.assertNotEqual('configured_project_tests', command_class(command, 'python3 run_tests.py'))
        self.assertEqual('other_literal', command_category('python3 /elsewhere/run_tests.py', 'python3 run_tests.py', self.root))

    def test_real_runner_receipt_and_allowed_whitespace_do_not_need_english_markers(self):
        result = self.run_tests()
        receipt_line = next(line for line in result.stdout.decode().splitlines() if line.startswith(PREFIX))
        self.assertLessEqual(len(receipt_line.encode()), 256, 'keep the reference below pinned OMP\'s default 768-byte line cap')
        self.event(result, command='python3   run_tests.py', output=receipt_line)
        report = self.receipts.report()
        self.assertTrue(report['client_tests_succeeded'])
        self.assertEqual(1, report['test_execution']['calls'][0]['tests_run'])
        self.assertEqual(self.protected, report['test_execution']['calls'][0]['protected_sha256'])

    def test_native_path_alias_needs_a_real_matching_runner_receipt(self):
        result = subprocess.run([sys.executable, './run_tests.py'], cwd=self.root, capture_output=True, check=False)
        self.event(result, command='python3 ./run_tests.py')
        report = self.receipts.report()
        self.assertTrue(report['client_tests_succeeded'])
        call = report['test_execution']['calls'][0]
        self.assertEqual('other_literal', call['command_class'])
        self.assertEqual('test_path_alias', call['command_category'])
        self.assertTrue(call['execution_verified'])

    def test_compound_command_needs_the_same_real_receipt_not_a_particular_spelling(self):
        result=subprocess.run('python3 -u run_tests.py && printf done',shell=True,cwd=self.root,capture_output=True)
        self.event(result,command='python3 -u run_tests.py && printf done')
        report=self.receipts.report()
        self.assertTrue(report['client_tests_succeeded'])
        self.assertEqual('other_shell',report['test_execution']['calls'][0]['command_class'])
        self.assertNotIn('printf done',json.dumps(report))

    def test_fabricated_receipt_reference_does_not_replace_a_real_receipt(self):
        output=PREFIX+json.dumps({'run_id':'f'*32,'sha256':'f'*64})
        self.event(subprocess.CompletedProcess([],0,b'',b''),command='printf fake-receipt',output=output)
        report=self.receipts.report()
        self.assertFalse(report['client_tests_succeeded'])
        self.assertEqual('file_unavailable',report['test_execution']['calls'][0]['receipt_status'])

    def test_each_actual_call_survives_later_command_and_duplicate_terminal(self):
        result = self.run_tests()
        event = self.event(result)
        self.receipts.observe(self.tracker.observe(event), event)
        self.event(result, 'printf synthetic', 'later-call', output='OK', code=0)
        report = self.receipts.report()
        self.assertTrue(report['client_tests_succeeded'])
        self.assertEqual(2, len(report['test_execution']['calls']))
        self.assertEqual('not_test_command', report['test_execution']['calls'][1]['receipt_status'])
        self.assertEqual(0, report['bash_exit_code'])
        for raw in ['synthetic-call', 'later-call', 'printf synthetic', 'Ran 1 test', self.nonce]:
            self.assertNotIn(raw, json.dumps(report))

    def test_failed_test_is_preserved_and_cannot_be_hidden_by_later_successful_non_test(self):
        (self.root / 'price_math.py').write_text('value = 9\n')
        result = self.run_tests()
        self.assertEqual(1, result.returncode)
        self.event(result)
        self.event(result, 'printf OK', 'extra', output='OK', code=0)
        report = self.receipts.report()
        self.assertFalse(report['client_tests_succeeded'])
        first = report['test_execution']['calls'][0]
        self.assertTrue(first['execution_verified'])
        self.assertFalse(first['passed'])
        self.assertEqual(1, first['failures'])
        self.assertEqual(1, report['bash_exit_code'])

    def test_later_failed_matching_test_cannot_be_hidden_by_earlier_success(self):
        self.event(self.run_tests())
        # Preserve file size changes to avoid timestamp-based Python bytecode reuse.
        (self.root / 'price_math.py').write_text('value = 99999\n')
        self.event(self.run_tests(), call_id='failed-later')
        report = self.receipts.report()
        self.assertFalse(report['client_tests_succeeded'])
        self.assertEqual([True, False], [entry['passed'] for entry in report['test_execution']['calls']])

    def test_missing_receipt_fake_ok_and_missing_exit_fail_closed(self):
        result = self.run_tests()
        self.event(result, output='Ran 1 test\n\nOK\n')
        self.assertFalse(self.receipts.report()['client_tests_succeeded'])
        self.event(result, call_id='unknown-exit', code=None)
        self.assertEqual('exit_unverified', self.receipts.report()['test_execution']['calls'][1]['receipt_status'])
        self.assertFalse(self.receipts.report()['client_tests_succeeded'])

    def test_actual_nonzero_exit_stops_but_untrusted_receipt_cannot_stop_a_successful_tool(self):
        for code,output,reason in [(1,'truncated before receipt','test_failed'),(0,'OK',None)]:
            with self.subTest(code=code):
                self.event(self.run_tests(),call_id='no-proof-'+str(code),output=output,code=code)
                tool=self.tracker.calls['no-proof-'+str(code)]
                self.assertEqual(reason,self.receipts.fatal_reason(tool))
        self.event(self.run_tests(),call_id='actual-valid')
        self.assertIsNone(self.receipts.fatal_reason(self.tracker.calls['actual-valid']))

    def test_log_reread_is_a_repeated_display_not_a_new_test_or_conflict(self):
        result=self.run_tests()
        self.event(result,command='python3 run_tests.py | tee out/test.log')
        first=self.receipts.report()['test_execution']['selected_call_sha256']
        self.event(result,command='cat out/test.log',call_id='reread')
        report=self.receipts.report()['test_execution']
        self.assertFalse(report['conflicting_evidence'])
        self.assertEqual(first,report['selected_call_sha256'])
        self.assertEqual('repeated_display',report['calls'][1]['receipt_status'])
        self.assertFalse(report['calls'][1]['test_runner_candidate'])
        self.assertFalse(report['eligible_as_execution_proof'])

    def test_omp_success_requires_actual_output_receipt_and_matching_file(self):
        result = self.run_tests()
        self.event(result, client='omp', code=None)
        self.assertTrue(self.receipts.report()['client_tests_succeeded'])
        (self.root / 'price_math.py').write_text('changed = True')
        self.assertFalse(self.receipts.report()['client_tests_succeeded'])

    def test_omp_omitted_exit_cannot_inherit_a_failed_receipt_exit(self):
        (self.root/'price_math.py').write_text('value = 999\n')
        result=self.run_tests()
        self.assertEqual(1,result.returncode)
        self.event(result,client='omp',code=None)
        call=self.receipts.report()['test_execution']['calls'][0]
        self.assertEqual(1,call['exit_code'],'untrusted receipt remains diagnostic')
        self.assertIsNone(call['client_exit_code'],'the actual success event omitted an exit code')
        self.assertIsNone(self.receipts.fatal_reason(self.tracker.calls['synthetic-call']))

    def test_stale_wrong_nonce_changed_protected_source_and_zero_tests_are_rejected(self):
        result = self.run_tests()
        reference = json.loads(next(line[len(PREFIX):] for line in result.stdout.decode().splitlines() if line.startswith(PREFIX)))
        receipt = self.root / ('out/test-receipts/' + reference['run_id'] + '.json')
        original = json.loads(receipt.read_bytes())
        for mutation in ['stale', 'nonce', 'protected', 'source', 'zero', 'exit']:
            proof = {**original}
            if mutation == 'stale': proof['started_ns'] = self.started - 1
            elif mutation == 'nonce': proof['project_nonce_sha256'] = '0' * 64
            elif mutation == 'protected': proof['protected_sha256'] = {}
            elif mutation == 'source': proof['source_after_sha256'] = '0' * 64
            elif mutation == 'zero': proof['tests_run'] = 0
            else: proof['exit_code'] = 1
            raw = json.dumps(proof).encode()
            receipt.write_bytes(raw)  # Explicitly corrupt an actual receipt for the negative test.
            self.event(result, call_id='bad-' + mutation, output=PREFIX + json.dumps({'run_id': reference['run_id'], 'sha256': digest(raw)}))
            self.assertFalse(self.receipts.report()['client_tests_succeeded'], mutation)
        # The observer has not created or rewritten any receipt file.
        self.assertEqual(1, len(list((self.root / 'out/test-receipts').glob('*.json'))))

    def test_reference_cannot_bless_a_changed_file_or_unrelated_run(self):
        result = self.run_tests()
        reference = json.loads(next(line[len(PREFIX):] for line in result.stdout.decode().splitlines() if line.startswith(PREFIX)))
        receipt = self.root / ('out/test-receipts/' + reference['run_id'] + '.json')
        receipt.write_bytes(receipt.read_bytes() + b' ')
        self.event(result)
        self.assertEqual('file_output_mismatch', self.receipts.report()['test_execution']['calls'][0]['receipt_status'])
        self.assertFalse(self.receipts.report()['client_tests_succeeded'])

    def test_omp_malformed_explicit_exit_is_not_a_documented_omission(self):
        result = self.run_tests()
        self.event(result, client='omp', code=False)
        self.assertFalse(self.receipts.report()['client_tests_succeeded'])

    def test_repeated_receipt_display_is_diagnostic_but_changed_tool_terminal_is_rejected(self):
        result = self.run_tests()
        event = self.event(result)
        self.event(result, call_id='different-call')
        self.assertTrue(self.receipts.report()['client_tests_succeeded'])
        self.assertFalse(self.receipts.report()['test_execution']['conflicting_evidence'])
        self.assertEqual('repeated_display',self.receipts.report()['test_execution']['calls'][1]['receipt_status'])
        event['part']['state']['output'] = 'changed terminal output'
        with self.assertRaisesRegex(ValueError, 'terminal_changed'):
            self.receipts.observe(self.tracker.observe(event), event)


if __name__ == '__main__':
    unittest.main()
