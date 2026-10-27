import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from runtime_dependencies import inspect_rg


class RuntimeDependencyTests(unittest.TestCase):
    def test_absent_executable_is_structural_and_does_not_download(self):
        with patch('runtime_dependencies.shutil.which', return_value=None), patch('runtime_dependencies.subprocess.run') as run:
            result = inspect_rg()
        self.assertFalse(result['found'])
        self.assertFalse(result['runnable'])
        self.assertEqual('executable_missing', result['error_category'])
        run.assert_not_called()

    def test_version_probe_never_persists_raw_output_or_path(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'synthetic-secret-rg'
            path.write_bytes(b'synthetic public executable fixture')
            with patch('runtime_dependencies.shutil.which', return_value=str(path)), patch('runtime_dependencies.subprocess.run', return_value=subprocess.CompletedProcess([], 0, b'ripgrep 15.1.0\nprivate-output', b'private-error')):
                result = inspect_rg()
            self.assertTrue(result['runnable'])
            self.assertEqual('15.1.0', result['version'])
            self.assertEqual(64, len(result['sha256']))
            for private in ['synthetic-secret', root, 'private-output', 'private-error']:
                self.assertNotIn(private, json.dumps(result))

