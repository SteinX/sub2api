import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import process_cleanup


class ProcessIdentityTests(unittest.TestCase):
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
