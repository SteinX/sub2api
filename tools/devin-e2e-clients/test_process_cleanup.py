import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import signal
from unittest.mock import patch

import process_cleanup


class ProcessIdentityTests(unittest.TestCase):
    def test_reaped_proc_stat_read_does_not_invalidate_an_empty_owned_group(self):
        with tempfile.TemporaryDirectory() as temporary:
            registry=Path(temporary)/'registry'; registry.touch(mode=0o600)
            tracked=subprocess.Popen(['python3','-c','import time;time.sleep(30)'],start_new_session=True)
            foreign=subprocess.Popen(['python3','-c','import time;time.sleep(30)'],start_new_session=True)
            observed=[]
            try:
                with patch.dict(os.environ,{'DEVIN_JOURNEY_PROCESS_REGISTRY':str(registry)}): process_cleanup.register(tracked)
                tracked.terminate(); tracked.wait(timeout=3)
                original=Path.read_text
                target=Path(f'/proc/{foreign.pid}/stat')
                def exit_after_open(path,*args,**kwargs):
                    if path==target and not observed:
                        with path.open() as stream:
                            foreign.terminate(); foreign.wait(timeout=3)
                            try: return stream.read()
                            except ProcessLookupError as error:
                                observed.append(error.errno)
                                raise
                    return original(path,*args,**kwargs)
                # Control scheduling only: the kernel supplies the real ESRCH
                # after open/reap/read, rather than a mocked cleanup outcome.
                with patch.object(Path,'read_text',exit_after_open):
                    self.assertTrue(process_cleanup.cleanup_processes(registry))
                self.assertEqual([3],observed)
                self.assertEqual({},process_cleanup.group_members(tracked.pid))
            finally:
                for proc in [tracked,foreign]:
                    if proc.poll() is None: proc.kill()
                    proc.wait(timeout=3)

    def test_diagnostics_separate_process_and_container_proof_without_exposing_identity(self):
        for process_ok,container_ok in [(False,True),(True,False),(True,True)]:
            evidence={}
            with patch.object(process_cleanup,'cleanup_processes',return_value=process_ok), patch('container_sandbox.cleanup_registry',return_value=container_ok) as containers:
                self.assertEqual(process_ok and container_ok,process_cleanup.cleanup('/private/synthetic-registry',diagnostics=evidence))
            self.assertEqual({'schema_version':1,'process_cleanup_verified':process_ok,'container_cleanup_verified':container_ok},evidence)
            containers.assert_called_once()

    def test_exited_group_leader_cannot_hide_a_surviving_tool(self):
        with tempfile.TemporaryDirectory() as temporary:
            registry=Path(temporary)/'registry'; registry.touch(mode=0o600)
            registry.write_text(json.dumps({'pid':12345,'start_ticks':100})+'\n')
            # TERM removes the leader, but its already observed tool survives.
            with patch.object(process_cleanup,'start_ticks',return_value=100), patch.object(process_cleanup,'group_members',side_effect=[{12345:100,12346:101},{12346:101},{},{}]), patch.object(process_cleanup.os,'killpg') as kill, patch.object(process_cleanup.time,'monotonic',side_effect=[0,3,4]):
                self.assertTrue(process_cleanup.cleanup(registry))
            self.assertEqual([signal.SIGTERM,signal.SIGKILL],[call.args[1] for call in kill.call_args_list])
            with patch.object(process_cleanup,'start_ticks',return_value=100), patch.object(process_cleanup,'group_members',side_effect=[{12345:100},{12346:101}]), patch.object(process_cleanup.os,'killpg') as kill, patch.object(process_cleanup.time,'monotonic',side_effect=[0,3]):
                self.assertFalse(process_cleanup.cleanup(registry))
            self.assertEqual([signal.SIGTERM],[call.args[1] for call in kill.call_args_list])

    def test_group_cleanup_kills_a_tool_that_ignores_term_after_its_leader_exits(self):
        with tempfile.TemporaryDirectory() as temporary:
            registry=Path(temporary)/'registry'; registry.touch(mode=0o600)
            script='''import os,signal,time
if os.fork()==0:
 signal.signal(signal.SIGTERM,signal.SIG_IGN)
 print('ready',flush=True)
 time.sleep(30)
else:
 time.sleep(30)
'''
            proc=subprocess.Popen(['python3','-c',script],start_new_session=True,stdout=subprocess.PIPE)
            try:
                self.assertEqual(b'ready\n',proc.stdout.readline())
                with patch.dict(os.environ,{'DEVIN_JOURNEY_PROCESS_REGISTRY':str(registry)}): process_cleanup.register(proc)
                self.assertTrue(process_cleanup.cleanup(registry))
                proc.wait(timeout=3)
                self.assertEqual({},process_cleanup.group_members(proc.pid))
            finally:
                try: os.killpg(proc.pid,signal.SIGKILL)
                except ProcessLookupError: pass
                proc.wait(timeout=3); proc.stdout.close()

    def test_tracked_session_is_reaped_and_reused_identity_is_never_signaled(self):
        for mismatch in [False,True]:
            with tempfile.TemporaryDirectory() as temporary:
                registry=Path(temporary)/'registry'; registry.touch(mode=0o600)
                proc=subprocess.Popen(['python3','-c','import time;time.sleep(30)'],start_new_session=True)
                try:
                    with patch.dict(os.environ,{'DEVIN_JOURNEY_PROCESS_REGISTRY':str(registry)}): process_cleanup.register(proc)
                    if mismatch:
                        record=json.loads(registry.read_text()); record['start_ticks']-=1
                        registry.write_text(json.dumps(record)+'\n')
                    self.assertEqual(not mismatch,process_cleanup.cleanup(registry))
                    if mismatch: self.assertIsNone(proc.poll())
                    else: self.assertIsNotNone(proc.wait(timeout=3))
                finally:
                    if proc.poll() is None: os.killpg(proc.pid,9)
                    proc.wait(timeout=3)

    def test_registry_rejects_untrusted_files(self):
        with tempfile.TemporaryDirectory() as temporary:
            path=Path(temporary)/'registry'; path.write_text('{"truncated":'); path.chmod(0o600)
            self.assertFalse(process_cleanup.cleanup(path))
            path.write_text(''); path.chmod(0o644)
            self.assertFalse(process_cleanup.cleanup(path))
