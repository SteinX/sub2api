"""An abruptly exiting Go runner must still trigger scoped outer cleanup."""
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import unittest

import process_cleanup


ROOT = Path(__file__).resolve().parents[2]


class MockRunnerCleanupTests(unittest.TestCase):
    def run_aborted_runner(self, directory, corrupt=False):
        binaries = directory / 'bin'
        binaries.mkdir()
        docker = binaries / 'docker'
        # Only fixture startup/teardown is stubbed. The child, /proc identity
        # checks and the script's actual EXIT cleanup execute normally.
        docker.write_text('''#!/usr/bin/env python3
import sys
if 'port' in sys.argv:
 print('127.0.0.1:54321')
''')
        docker.chmod(0o700)
        go = binaries / 'go'
        go.write_text('''#!/usr/bin/env python3
import json,os,pathlib,subprocess,sys
if sys.argv[1]=='build':
 pathlib.Path(sys.argv[sys.argv.index('-o')+1]).write_text('synthetic binary')
elif sys.argv[1]=='version':
 print('synthetic Go runner')
elif sys.argv[1]=='test':
 sys.path.insert(0,os.environ['CLEANUP_TEST_MODULE'])
 import process_cleanup
 registry=pathlib.Path(os.environ['DEVIN_JOURNEY_PROCESS_REGISTRY'])
 if os.environ['CLEANUP_TEST_CORRUPT']=='true':
  registry.write_text('{invalid')
  os._exit(0)
 child=subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)'],start_new_session=True,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 process_cleanup.register(child)
 pathlib.Path(os.environ['CLEANUP_TEST_PROOF']).write_text(json.dumps({'pid':child.pid,'registry':str(registry),'container_registry':os.environ['DEVIN_JOURNEY_CONTAINER_REGISTRY']}))
 # Simulate Go's package timeout: do not run any child cleanup/defer here.
 os._exit(37)
''')
        go.chmod(0o700)
        proof = directory / 'proof.json'
        env = {k: v for k, v in os.environ.items() if not k.startswith(('DEVIN_', 'SUB2API_'))}
        env.update(PATH=str(binaries) + os.pathsep + os.environ['PATH'],
                   DEVIN_JOURNEY_SUITE='true', DEVIN_E2E_ARTIFACTS=str(directory / 'artifacts'),
                   CLEANUP_TEST_MODULE=str(ROOT / 'tools/devin-e2e-clients'),
                   CLEANUP_TEST_PROOF=str(proof), CLEANUP_TEST_CORRUPT=str(corrupt).lower())
        result = subprocess.run(['bash', str(ROOT / 'deploy/devin-e2e.sh'), 'mock'],
                                env=env, capture_output=True, text=True, timeout=20)
        return result, proof

    def test_aborted_go_still_reaps_owned_child_and_preserves_failure(self):
        foreign = subprocess.Popen(['python3', '-c', 'import time;time.sleep(30)'], start_new_session=True)
        child_pid = None
        try:
            with tempfile.TemporaryDirectory() as temporary:
                result, proof = self.run_aborted_runner(Path(temporary))
                data = json.loads(proof.read_text())
                child_pid = data['pid']
                self.assertEqual(37, result.returncode, result.stderr)
                self.assertIn('"process_cleanup_verified": true', result.stdout)
                self.assertIn('"container_cleanup_verified": true', result.stdout)
                self.assertEqual({}, process_cleanup.group_members(child_pid))
                self.assertIsNone(foreign.poll(), 'unregistered processes must remain untouched')
                for name in ['registry', 'container_registry']:
                    path = Path(data[name])
                    self.assertEqual(0, path.stat().st_mode & 0o077)
                    self.assertEqual(0, path.parent.stat().st_mode & 0o077)
        finally:
            foreign.kill()
            foreign.wait(timeout=3)
            if child_pid and process_cleanup.group_members(child_pid):
                os.killpg(child_pid, signal.SIGKILL)

    def test_unverified_cleanup_cannot_turn_a_runner_exit_into_success(self):
        with tempfile.TemporaryDirectory() as temporary:
            result, _ = self.run_aborted_runner(Path(temporary), corrupt=True)
            self.assertEqual(1, result.returncode)
            self.assertIn('"process_cleanup_verified": false', result.stdout)
            self.assertIn('"container_cleanup_verified": true', result.stdout)
