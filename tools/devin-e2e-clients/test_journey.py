import json
import os
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

import journey


class JourneyProofTests(unittest.TestCase):
    def test_project_code_always_uses_unprivileged_identity_and_clean_environment(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(journey.os, 'geteuid', return_value=0), patch.object(journey.subprocess, 'run') as run:
            with patch.dict(os.environ, {'DEVIN_E2E_PAT': 'synthetic-secret', 'DEVIN_E2E_DB_PASSWORD': 'synthetic-password', 'DEVIN_E2E_REDIS_PASSWORD':'synthetic-cache-secret', 'SUB2API_API_KEY': 'synthetic-key',
                                         'DEVIN_JOURNEY_GATEWAY_KEY': 'synthetic-gateway', 'CODEX_PROXY_GUARD_TEST': 'keep-guard'}):
                journey.run_project_python(Path(temp), ['-c', 'import price_math'])
            kwargs = run.call_args.kwargs
            self.assertEqual((kwargs['user'], kwargs['group'], kwargs['extra_groups']), (65534, 65534, []))
            self.assertTrue(journey.SENSITIVE_ENV.isdisjoint(kwargs['env']))
            self.assertNotIn('SUB2API_API_KEY', kwargs['env'])
            self.assertEqual(kwargs['env']['CODEX_PROXY_GUARD_TEST'], 'keep-guard')
            self.assertEqual(kwargs['env'].get('HOME'), os.environ.get('HOME'))

    def test_omo_refuses_writable_personal_home_as_nonroot(self):
        with patch.object(journey.os, 'geteuid', return_value=1000), patch.object(journey.os, 'access', return_value=True), patch.dict(os.environ, {'GITHUB_ACTIONS': 'false'}):
            with self.assertRaises(ValueError):
                journey.isolation('omo')

    def test_omp_test_evidence_requires_matching_command_call_and_explicit_success_receipt(self):
        with tempfile.TemporaryDirectory() as temp:
            workspace = Path(temp); (workspace / 'out').mkdir()
            started = time.time_ns() - 1000000000
            event = {'isError': False, 'result': {'details': {}, 'content': [{'text': 'Ran 1 test\n\nOK'}]}}
            receipt = workspace / 'out/test-execution.json'
            self.assertFalse(journey.omp_test_result(event, journey.TEST_COMMAND, workspace, 'nonce', started))
            receipt.write_text(json.dumps({'nonce': 'nonce', 'exit_code': 0}))
            self.assertTrue(journey.omp_test_result(event, journey.TEST_COMMAND, workspace, 'nonce', started))
            self.assertFalse(journey.omp_test_result(event, 'echo fake-success', workspace, 'nonce', started))
            self.assertFalse(journey.omp_test_result(event, None, workspace, 'nonce', started))
            self.assertFalse(journey.omp_test_result(event, journey.TEST_COMMAND, workspace, 'different', started))
            self.assertFalse(journey.omp_test_result(event, journey.TEST_COMMAND, workspace, 'nonce', time.time_ns() + 1000000000))
            event['result']['details']['exitCode'] = 1
            self.assertFalse(journey.omp_test_result(event, journey.TEST_COMMAND, workspace, 'nonce', started))
            event['result']['details'] = {'timedOut': True}
            self.assertFalse(journey.omp_test_result(event, journey.TEST_COMMAND, workspace, 'nonce', started))

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


if __name__ == '__main__':
    unittest.main()
