import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("devin_clients", Path(__file__).with_name("run.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class ClientBoundaryTests(unittest.TestCase):
    def test_configuration_writes_stay_in_the_owned_workspace(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            clients, run = root / "clients", root / "run"
            for package, version in runner.PINS.items():
                path = clients / "node_modules" / package / "package.json"
                path.parent.mkdir(parents=True)
                path.write_text(json.dumps({"version": version}))
            paths = []
            original = Path.write_text
            def write(path, *args, **kwargs):
                paths.append(path)
                return original(path, *args, **kwargs)
            with patch.object(runner, "ROOT", clients), patch.object(Path, "write_text", autospec=True, side_effect=write):
                runner.configuration(run, "omp", "http://127.0.0.1:8000/v1", "fixture")
                runner.configuration(run, "omo", "http://127.0.0.1:8000/v1", "fixture")
            self.assertTrue(paths and all(path.is_relative_to(run) for path in paths))
            self.assertTrue(all("synthetic-sensitive" not in path.read_text() for path in paths))

    def test_external_or_credentialed_endpoint_is_rejected_before_configuration(self):
        for endpoint in ("https://server.codeium.com/v1", "http://user:password@127.0.0.1:8000/v1", "http://127.0.0.1:8000/v1?token=synthetic", "http://127.0.0.1:8000/other"):
            with self.subTest(endpoint=endpoint), tempfile.TemporaryDirectory() as directory:
                with self.assertRaises(ValueError):
                    runner.configuration(Path(directory), "opencode", endpoint, "fixture")
                self.assertEqual(list(Path(directory).iterdir()), [])

    def test_client_cannot_inherit_native_or_panel_credentials(self):
        sentinels = {key: "synthetic-sensitive" for key in runner.SENSITIVE_ENV}
        sentinels |= {"HTTPS_PROXY": "http://proxy:8080", "CODEX_PROXY_GUARD_TEST": "synthetic-guard", "DEVIN_E2E_GATEWAY_KEY": "synthetic-local-key"}
        with patch.dict(os.environ, sentinels), tempfile.TemporaryDirectory() as directory:
            env = runner.child_environment(Path(directory))
            self.assertTrue(runner.SENSITIVE_ENV.isdisjoint(env))
            self.assertEqual(env["CODEX_PROXY_GUARD_TEST"], "synthetic-guard")
            self.assertEqual(env["HTTPS_PROXY"], "http://proxy:8080")
            self.assertEqual(env.get("HOME"), os.environ.get("HOME"))

    def execute_fixture(self, event):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path / "workspace").mkdir()
            args = SimpleNamespace(directory=directory, client="opencode", endpoint="http://127.0.0.1:8000/v1", model="fixture", mode="mock", action="text", interrupt_file=None)
            stream = io.StringIO()
            events = event if isinstance(event, list) else [event]
            command = [sys.executable, "-c", "import json; " + "; ".join("print(json.dumps(" + repr(value) + "))" for value in events)]
            with patch.object(runner, "configuration"), patch.object(runner, "command", return_value=command), patch.dict(os.environ, {"DEVIN_E2E_GATEWAY_KEY": "synthetic-local-key"}), contextlib.redirect_stdout(stream):
                code = runner.execute(args)
            return code, json.loads(stream.getvalue())

    def test_prompt_echo_is_not_assistant_completion(self):
        code, evidence = self.execute_fixture({"type": "message_end", "message": {"role": "user", "content": [{"type": "text", "text": "Reply E2E_OK"}]}})
        self.assertNotEqual(code, 0)
        self.assertFalse(evidence["saw_expected_text"])

    def test_streaming_marker_can_span_client_frames(self):
        code, evidence = self.execute_fixture([{"type":"text","part":{"text":"E2E_"}}, {"type":"text","part":{"text":"OK"}}, {"type":"step_finish","part":{"reason":"stop"}}])
        self.assertEqual(code, 0)
        self.assertTrue(evidence["saw_expected_text"])

    def test_assistant_completion_retains_no_text_or_tool_payload(self):
        code, evidence = self.execute_fixture([
            {"type": "tool_use", "part": {"tool": "read", "state": {"status": "completed", "output": "synthetic-private-output"}}},
            {"type": "text", "part": {"text": "E2E_OK"}},
            {"type": "step_finish", "part": {"reason": "stop"}}
        ])
        self.assertEqual(code, 0)
        self.assertTrue(evidence["saw_expected_text"])
        self.assertNotIn("synthetic-private-output", json.dumps(evidence))

    def test_paid_guard_prevents_client_process(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(runner, "configuration"), patch.object(runner.subprocess, "Popen") as popen, patch.dict(os.environ, {"DEVIN_E2E_GATEWAY_KEY": "synthetic-local-key", "DEVIN_E2E_ALLOW_PAID": "false"}):
            args = SimpleNamespace(directory=directory, client="omp", endpoint="http://127.0.0.1:8000/v1", model="fixture", mode="live", action="text", interrupt_file=None)
            with self.assertRaises(ValueError):
                runner.execute(args)
            popen.assert_not_called()

    def test_zero_exit_with_assistant_error_is_not_a_pass(self):
        code, evidence = self.execute_fixture([
            {"type": "text", "part": {"text": "E2E_OK"}},
            {"type": "message_end", "message": {"role": "assistant", "stopReason": "error", "content": [{"type": "text", "text": "synthetic-private-error"}]}}
        ])
        self.assertNotEqual(code, 0)
        self.assertFalse(evidence["success"])
        self.assertNotIn("synthetic-private-error", json.dumps(evidence))

    def test_omo_refuses_a_writable_personal_home_before_starting(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(runner, "configuration"), patch.object(runner.os, "access", return_value=True), patch.object(runner.subprocess, "Popen") as popen, patch.dict(os.environ, {"DEVIN_E2E_GATEWAY_KEY": "synthetic-local-key", "GITHUB_ACTIONS": "false"}):
            args = SimpleNamespace(directory=directory, client="omo", endpoint="http://127.0.0.1:8000/v1", model="fixture", mode="mock", action="text", interrupt_file=None)
            with self.assertRaises(ValueError):
                runner.execute(args)
            popen.assert_not_called()


if __name__ == "__main__":
    unittest.main()
