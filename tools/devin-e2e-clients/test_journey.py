import json
import io
from contextlib import redirect_stderr
import subprocess
import threading
import os
from pathlib import Path
import tempfile
import sys
import time
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

import journey


class JourneyProofTests(unittest.TestCase):
    def test_native_user_policy_fixture_cannot_change_real_client_approvals(self):
        argv = ['journey.py', '--directory', '/tmp/synthetic', '--client', 'opencode', '--model', 'synthetic',
                '--ui-url', 'http://127.0.0.1:1234', '--profile', 'project', '--upstream', 'real', '--fixture-user-policy', 'approved']
        with patch.object(sys, 'argv', argv), patch.object(journey, 'identity') as identity, redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit): journey.main()
            identity.assert_not_called()

    def test_catalog_observer_delay_cannot_expand_real_or_unbounded_execution(self):
        for flags in [['--catalog-recovery-probe', '--upstream', 'real', '--catalog-observer-delay-ms', '16000'],
                      ['--catalog-observer-delay-ms', '16000'],
                      ['--catalog-recovery-probe', '--catalog-observer-delay-ms', '18000'],
                      ['--catalog-recovery-probe', '--catalog-observer-delay-ms', '-1']]:
            argv = ['journey.py', '--directory', '/tmp/synthetic', '--client', 'opencode', '--model', 'synthetic',
                    '--ui-url', 'http://127.0.0.1:1234', '--profile', 'project', *flags]
            with self.subTest(flags=flags), patch.object(sys, 'argv', argv), patch.object(journey, 'identity') as identity, redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit): journey.main()
                identity.assert_not_called()

    def test_wrong_resume_nonce_cannot_start_optional_capability_requests(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary); workspace = root / 'workspace'; workspace.mkdir()
            (workspace / 'price_math.py').write_text('initial broken source')
            expected = {'nonce': 'synthetic', 'totals': [3]}
            first = {'passed': True, 'tools': {'read': 4}, 'source_mutation_tools': {'edit': 1},
                     'client_tests_succeeded': True, 'client_session_sha256': 'a' * 64,
                     'project_nonce_sha256': journey.digest(b'synthetic'), 'exit_code': 0,
                     'timed_out': False, 'incomplete_tool_calls': 0}
            resumed = {'passed': True, 'tools': {'read': 1}, 'client_session_sha256': 'a' * 64,
                       'project_nonce_sha256': journey.digest(b'wrong-project')}
            def invoke(*args, **kwargs):
                (workspace / 'price_math.py').write_text('repaired source')
                (workspace / 'out').mkdir(exist_ok=True)
                (workspace / 'out/report.json').write_text(json.dumps(expected))
                return resumed if len(args) > 2 and args[2] is True else first
            class UI:
                returncode = 0
                def communicate(self, *args, **kwargs):
                    return b'{"passed":true}', b''
            capability_run = Mock(return_value={})
            stream = io.StringIO()
            with patch.multiple(journey, identity=Mock(return_value={}), isolation=Mock(return_value={}),
                                isolation_evidence=Mock(return_value={}), prepare=Mock(return_value=(workspace, expected, {})),
                                project_environment=Mock(return_value={}), start_child=Mock(return_value=UI()),
                                invoke=Mock(side_effect=invoke), run_project_python=Mock(return_value=SimpleNamespace(returncode=0))), \
                 patch.dict(sys.modules, {'capabilities': SimpleNamespace(run=capability_run)}), \
                 patch.object(sys, 'argv', ['journey.py', '--directory', str(root), '--client', 'omp', '--model', 'synthetic', '--ui-url', 'http://127.0.0.1', '--profile', 'compatibility']), \
                 patch.object(sys, 'stdout', stream), \
                 patch.dict(os.environ, {'DEVIN_JOURNEY_GATEWAY_KEY': 'synthetic', 'DEVIN_JOURNEY_UI_PASSWORD': 'synthetic', 'DEVIN_JOURNEY_CAPABILITIES': 'true'}):
                code = journey.main()
            self.assertEqual(1, code)
            report = json.loads(stream.getvalue())
            self.assertTrue(all(report['task_checks'].values()))
            self.assertTrue(report['same_client_session'])
            capability_run.assert_not_called()

    def test_zero_exit_cannot_hide_truncated_reader_after_an_earlier_normal_terminal(self):
        events = [{'type': 'step_start', 'sessionID': 'synthetic-session'},
                  {'type': 'text', 'part': {'id': 'part-1', 'text': 'Verified the project.'}},
                  {'type': 'step_finish', 'part': {'reason': 'stop'}}]
        wire = b''.join(json.dumps(event).encode() + b'\n' for event in events) + b'x' * (4 * 1024 * 1024 + 1)
        class Child:
            pid = 1073741823
            returncode = 0
            stdout = io.BytesIO(wire)
            stderr = io.BytesIO(b'')
            def poll(self):
                return self.returncode
            def wait(self, timeout=None):
                return 0
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary); (root / 'workspace').mkdir()
            (root / 'workspace/requirements.json').write_text(json.dumps({'nonce': 'synthetic'}))
            (root / 'workspace/test_project.py').write_text('protected test')
            (root / 'workspace/run_tests.py').write_text('protected runner')
            args = SimpleNamespace(client='opencode', model='synthetic', ui_url='http://127.0.0.1')
            with patch.object(journey, 'isolation', return_value={}), patch.object(journey, 'project_environment', return_value={}), \
                 patch.object(journey, 'start_child', return_value=Child()), patch.object(journey.os, 'killpg'), \
                 patch.dict(os.environ, {'DEVIN_JOURNEY_GATEWAY_KEY': 'synthetic'}):
                result = journey.invoke(args, root)
            self.assertEqual(0, result['exit_code'])
            self.assertTrue(result['normal_stop'])
            self.assertTrue(result['reader_error'])
            self.assertFalse(result['reader_completed'])
            self.assertFalse(result['passed'])

    def test_pressure_setup_cannot_follow_workspace_or_context_links(self):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary); directory=root/'project'; directory.mkdir(); outside=root/'outside'; outside.mkdir()
            (directory/'workspace').symlink_to(outside,target_is_directory=True)
            with self.assertRaises(OSError): journey.prepare_pressure(directory,'synthetic')
            self.assertEqual([],list(outside.iterdir()))
            (directory/'workspace').unlink(); (directory/'workspace').mkdir()
            (directory/'workspace/context').symlink_to(outside,target_is_directory=True)
            with self.assertRaises(OSError): journey.prepare_pressure(directory,'synthetic')
            self.assertEqual([],list(outside.iterdir()))
    def test_project_code_always_uses_unprivileged_identity_and_clean_environment(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(journey.os, 'geteuid', return_value=0), patch.object(journey.subprocess, 'Popen') as run, patch.object(journey, 'track'), patch.object(journey.container_sandbox,'collect_output'):
            with patch.dict(os.environ, {'DEVIN_E2E_PAT': 'synthetic-secret', 'DEVIN_E2E_DB_PASSWORD': 'synthetic-password', 'DEVIN_E2E_REDIS_PASSWORD':'synthetic-cache-secret', 'SUB2API_API_KEY': 'synthetic-key','UNRELATED_SERVICE_TOKEN':'synthetic-other-token',
                                         'DEVIN_JOURNEY_GATEWAY_KEY': 'synthetic-gateway', 'CODEX_PROXY_GUARD_TEST': 'keep-guard'}):
                journey.run_project_python(Path(temp), ['-c', 'import price_math'])
            kwargs = run.call_args.kwargs
            self.assertEqual((kwargs['user'], kwargs['group'], kwargs['extra_groups']), (65534, 65534, []))
            self.assertTrue(journey.SENSITIVE_ENV.isdisjoint(kwargs['env']))
            self.assertNotIn('SUB2API_API_KEY', kwargs['env'])
            self.assertNotIn('UNRELATED_SERVICE_TOKEN',kwargs['env'])
            self.assertEqual(kwargs['env']['CODEX_PROXY_GUARD_TEST'], 'keep-guard')
            self.assertEqual(kwargs['env'].get('HOME'), os.environ.get('HOME'))

    def test_nonroot_refuses_missing_container_isolation_even_in_ci(self):
        with patch.object(journey.os, 'geteuid', return_value=1000), patch.object(journey.container_sandbox,'require_available',side_effect=ValueError('isolation_unavailable')), patch.dict(os.environ, {'GITHUB_ACTIONS': 'true'}):
            with self.assertRaises(ValueError):
                journey.isolation('omo')

    def test_final_artifact_and_protected_files_are_rechecked_after_resume(self):
        with tempfile.TemporaryDirectory() as temp:
            workspace = Path(temp); (workspace / 'out').mkdir()
            expected = {'nonce': 'synthetic', 'totals': [3]}
            (workspace / 'out/report.json').write_text(json.dumps(expected))
            (workspace / 'test_project.py').write_text('protected')
            (workspace / 'price_math.py').write_text('correct-source')
            protected = {'test_project.py': journey.digest(b'protected')}
            source_hash = journey.digest(b'correct-source')
            before = journey.final_project_snapshot(workspace, expected, protected, source_hash)
            self.assertTrue(before['artifact_verified_after_resume'])
            self.assertEqual(before['artifact_sha256'], journey.digest((workspace / 'out/report.json').read_bytes()))
            (workspace / 'out/report.json').write_text('{"wrong":true}')
            (workspace / 'test_project.py').write_text('tampered')
            (workspace / 'price_math.py').write_text('tampered-source')
            after = journey.final_project_snapshot(workspace, expected, protected, source_hash)
            self.assertFalse(after['artifact_verified_after_resume'])
            self.assertFalse(after['protected_project_files_intact'])
            self.assertFalse(after['source_unchanged_after_resume'])
            self.assertIsNone(after['artifact_sha256'])

    def test_project_artifacts_cannot_follow_symlinks(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); (root / 'workspace').mkdir(); (root / 'outside').mkdir()
            (root / 'outside/report.json').write_text('{"synthetic":"outside"}')
            (root / 'workspace/out').symlink_to(root / 'outside', target_is_directory=True)
            with self.assertRaises(OSError):
                journey.project_bytes(root / 'workspace', 'out/report.json')


class ProtectedSnapshotTests(unittest.TestCase):
    def test_changed_session_cannot_ack_a_completed_tool_or_normal_terminal(self):
        import batch_control
        for terminal in [{'type':'tool_use','part':{'callID':'public-call','tool':'read','state':{'status':'completed','input':{},'output':'synthetic'}}},
                         {'type':'step_finish','part':{'reason':'stop'}}]:
            with self.subTest(kind=terminal['type']), tempfile.TemporaryDirectory() as temporary:
                root=Path(temporary);workspace=root/'workspace';workspace.mkdir()
                for name,content in {'requirements.json':'{"nonce":"synthetic"}','test_project.py':'protected','run_tests.py':'protected'}.items(): (workspace/name).write_text(content)
                events=[{'type':'step_start','sessionID':'original'},{**terminal,'sessionID':'changed'}]
                proc=Mock();proc.stdout=io.BytesIO(b''.join(json.dumps(event).encode()+b'\n' for event in events));proc.stderr=io.BytesIO(b'');proc.returncode=0
                control=batch_control.Control()
                with patch.object(journey,'start_child',return_value=proc),patch.object(journey,'kill_child'),patch.object(journey,'project_environment',return_value={}),patch.object(journey,'isolation',return_value={}), \
                     patch.object(journey.batch_control,'current',return_value=control),patch.object(control,'send',return_value=True) as send, \
                     patch.dict(os.environ,{'DEVIN_JOURNEY_GATEWAY_KEY':'synthetic'}):
                    result=journey.invoke(SimpleNamespace(client='opencode',model='synthetic',ui_url='http://127.0.0.1'),root)
                self.assertTrue(result['session_changed']);self.assertFalse(result['passed'])
                self.assertNotIn('tool',[call.args[0] for call in send.call_args_list])
                self.assertNotIn('response',[call.args[0] for call in send.call_args_list])

    def test_client_cannot_replace_expected_test_fingerprints_during_start(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            workspace = directory / 'workspace'; workspace.mkdir()
            for name, text in {'requirements.json':'{"nonce":"synthetic"}', 'price_math.py':'value = 1\n',
                               'test_project.py':'import unittest\nclass Check(unittest.TestCase):\n def test_value(self): self.assertEqual(1,1)\n',
                               'run_tests.py':journey.TEST_RUNNER}.items():
                (workspace/name).write_text(text)
            original_test = (workspace/'test_project.py').read_bytes()
            observed = threading.Event()
            class ReceiptObserver(journey.TestReceipts):
                def observe(self, tool, event):
                    super().observe(tool, event)
                    if tool and tool['name'] == 'bash': observed.set()
            class Child:
                pid = 1073741823
                returncode = 0
                def wait(self, timeout=None):
                    if not observed.wait(2): raise AssertionError('receipt event was not consumed')
                    (workspace/'test_project.py').write_bytes(original_test)
                    return 0
            def start(*args, **kwargs):
                (workspace/'test_project.py').write_bytes(b'# Replaced after child launch\n' + original_test)
                executed = subprocess.run([sys.executable, 'run_tests.py'], cwd=workspace, capture_output=True, check=True)
                events = [{'type':'step_start','sessionID':'synthetic-session'},
                          {'type':'tool_use','part':{'callID':'synthetic-call','tool':'bash','state':{'status':'completed',
                           'input':{'command':journey.TEST_COMMAND},'metadata':{'exit':0},'output':executed.stdout.decode()}}},
                          {'type':'text','part':{'text':'The project is verified.'}},
                          {'type':'step_finish','part':{'reason':'stop'}}]
                child = Child()
                child.stdout = io.BytesIO(b''.join(json.dumps(event).encode()+b'\n' for event in events))
                child.stderr = io.BytesIO(b'')
                return child
            with patch.object(journey,'isolation',return_value={}), patch.object(journey,'project_environment',return_value={}), \
                 patch.object(journey,'start_child',side_effect=start), patch.object(journey,'TestReceipts',ReceiptObserver), \
                 patch.object(journey,'kill_child'), \
                 patch.dict(os.environ,{'DEVIN_JOURNEY_GATEWAY_KEY':'synthetic'}):
                result = journey.invoke(SimpleNamespace(client='opencode',model='synthetic',ui_url='http://127.0.0.1'),directory)
            self.assertEqual(original_test,(workspace/'test_project.py').read_bytes())
            self.assertFalse(result['normal_stop'],'invalid test proof stops consumption before a later false normal terminal')
            self.assertFalse(result['passed'])
            self.assertFalse(result['client_tests_succeeded'],'the client cannot establish the expected protected-file baseline')
            self.assertEqual('wrong_project_or_tests',result['test_execution']['calls'][0]['receipt_status'])


if __name__ == '__main__':
    unittest.main()
