"""Local runner failure/identity/privacy checks; fake processes, no live API."""

import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import ledger
import local_runner as runner


API_KEY = "synthetic-model-secret"
USER_TOKEN = "synthetic-panel-secret"


def snapshot(*, used="0", quota="1", **overrides):
    key = {"id": 42, "key_fingerprint": runner.digest(API_KEY), "quota_usd": quota,
           "quota_used_usd": used, "status": "active", "current_concurrency": 0,
           "group_subscription_type": "standard"}
    key.update(overrides)
    return {"api_key": key, "capture_stable": True, "usage_rows": [], "evidence_complete": False}


def comparison(before, after):
    return {"verified_settled_delta_usd": "0.20", "cost_scope": "partial", "evidence_complete": False,
            "usage_sum_usd": "0.20", "requires_retained_local_reserve": True}


FAKE_CLIENT = '''import json, os, sys
if "--version" in sys.argv:
    print("18.12.6")
    raise SystemExit(0)
secret = os.environ["SUB2API_API_KEY"]
with open("child-env-check.json", "w") as output:
    json.dump({"user_token_present": "SUB2API_USER_TOKEN" in os.environ,
               "unrelated_key_present": "OPENAI_API_KEY" in os.environ,
               "model_keys_same": secret == os.environ["SUB2API_FUSION_API_KEY"],
               "home": os.environ.get("HOME")}, output)
print(json.dumps({"type":"session", "id":"synthetic-private-worker-session"}))
print(json.dumps({"type":"tool_execution_start", "toolName":"task", "toolCallId":"private-call-id",
                  "args":{"agent":"fusion-worker", "task":secret}}))
print(json.dumps({"type":"tool_execution_end", "toolName":"task", "toolCallId":"private-call-id", "isError":False,
                  "result":{"content":[{"text":secret}], "details":{"results":[{
                      "agent":"fusion-worker", "exitCode":0, "requests":2,
                      "resolvedModelIdentity":"sub2api-fusion/fusion-worker-test", "resolvedModelIsFallback":False}]}}}))
print(json.dumps({"type":"message_end", "message":{"role":"assistant", "model":"concrete-model", "stopReason":"stop",
                 "content":[{"type":"thinking", "thinking":secret}]}}))
print(secret, file=sys.stderr)
'''


class RunnerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.fake = self.root / "fake_client.py"
        self.fake.write_text(FAKE_CLIENT)

    def prepare(self, *, client="omp", **overrides):
        args = {"directory": self.root / "run", "client": client, "case": "short-repair",
                "base_url": "https://sub2api.invalid/v1", "direct_model": "concrete-model",
                "fusion_model": "fusion-worker-test", "api_key_id": 42, "budget_usd": "1",
                "cli": [sys.executable, str(self.fake)], "timeout_seconds": 5}
        args.update(overrides)
        runner.prepare(**args)
        return Path(args["directory"])

    def run_prepared(self, directory, *, before=None, after=None):
        calls = []

        def capture(base, key_id, token, *, expected_key):
            self.assertEqual(base, "https://sub2api.invalid/v1")
            self.assertEqual(key_id, 42)
            self.assertEqual(token, USER_TOKEN)
            self.assertEqual(expected_key, API_KEY)
            calls.append(True)
            return (before or snapshot()) if len(calls) == 1 else (after or snapshot(used="0.20"))

        checker = SimpleNamespace(check=lambda *_: {"quality_passed": True, "test_exit_code": 0})
        with patch.dict(os.environ, {"SUB2API_API_KEY": API_KEY, "SUB2API_USER_TOKEN": USER_TOKEN,
                                     "OPENAI_API_KEY": "unrelated-provider-secret"}):
            with patch.object(runner, "smoke_module", return_value=checker):
                return runner.run(directory, capture=capture, compare=comparison)

    def test_prepare_is_offline_and_pins_only_one_fusion_worker(self):
        with patch("urllib.request.urlopen", side_effect=AssertionError("network forbidden")):
            directory = self.prepare()
        metadata = json.loads((directory / "run.json").read_text())
        self.assertEqual(metadata["expected_client_version"], "18.12.6")
        self.assertEqual(metadata["budget_usd"], "1")
        self.assertTrue((directory / "workspace/TASK.md").is_file())
        config = (directory / "omp-agent/config.yml").read_text()
        self.assertIn("default: sub2api-direct/concrete-model", config)
        self.assertIn("fusion-worker: sub2api-fusion/fusion-worker-test", config)
        self.assertIn("enabled: false", config)
        self.assertEqual(runner.inspect(directory)["state"], "prepared")
        self.assertFalse((directory / "start.json").exists())

    def test_prepare_rejects_existing_and_symlink_paths(self):
        directory = self.prepare()
        with self.assertRaisesRegex(runner.RunnerError, "new_run_directory_required"):
            self.prepare()
        (self.root / "link").symlink_to(directory, target_is_directory=True)
        with self.assertRaisesRegex(runner.RunnerError, "symlink"):
            self.prepare(directory=self.root / "link/new")

    def test_invalid_endpoint_budget_and_shell_are_rejected_before_creation(self):
        for endpoint in ("https://key:secret@example.com/v1", "http://example.com/v1", "https://example.com/v1?key=x",
                         "https://example.com/v1\n", "https://example.com/v1#secret"):
            with self.subTest(endpoint=endpoint), self.assertRaises(runner.RunnerError):
                self.prepare(base_url=endpoint)
        for budget in ("NaN", "-1", "Infinity", "0"):
            with self.subTest(budget=budget), self.assertRaises(runner.RunnerError):
                self.prepare(budget_usd=budget)
        with self.assertRaises(runner.RunnerError):
            self.prepare(cli=["bash", "-c"])
        self.assertFalse((self.root / "run").exists())

    def test_client_environment_excludes_panel_and_other_provider_credentials(self):
        directory = self.prepare()
        _, metadata = runner.load_run(directory)
        with patch.dict(os.environ, {"SUB2API_USER_TOKEN": USER_TOKEN, "OPENAI_API_KEY": "another-secret",
                                     "PI_PROFILE": "production", "HTTPS_PROXY": "http://proxy.invalid:9999"}):
            env = runner.child_environment(directory, metadata, API_KEY)
        self.assertNotIn("SUB2API_USER_TOKEN", env)
        self.assertNotIn("OPENAI_API_KEY", env)
        self.assertNotIn("PI_PROFILE", env)
        self.assertEqual(env["HOME"], os.environ["HOME"])
        self.assertEqual(env["SUB2API_API_KEY"], env["SUB2API_FUSION_API_KEY"])
        self.assertEqual(env["HTTPS_PROXY"], "http://proxy.invalid:9999")
        self.assertEqual(Path(os.path.expanduser("~"), env["PI_CONFIG_DIR"]).resolve(), directory / "omp-root")

    def test_real_fake_process_is_redacted_and_accounting_retains_full_reserve(self):
        directory = self.prepare()
        report = self.run_prepared(directory)
        result = report["result"]
        self.assertEqual(result["outcome"], "completed")
        self.assertTrue(result["quality_passed"])
        self.assertEqual(result["cost_scope"], "partial")
        self.assertEqual(result["confirmed_usd"], "0.2")
        self.assertEqual(result["reserved_unknown_usd"], "1")
        self.assertTrue(report["requires_manual_reconciliation"])
        ledger.validate_run(result)
        evidence = json.loads((directory / "evidence.json").read_text())
        self.assertEqual(evidence["dispatch_evidence"], "verified_model_identity")
        self.assertFalse(evidence["child_tool_trace_complete"])
        checks = json.loads((directory / "workspace/child-env-check.json").read_text())
        self.assertFalse(checks["user_token_present"])
        self.assertFalse(checks["unrelated_key_present"])
        self.assertTrue(checks["model_keys_same"])
        self.assertEqual(checks["home"], os.environ["HOME"])
        for name in ("run.json", "start.json", "before.json", "after.json", "result.json", "evidence.json"):
            raw = (directory / name).read_text()
            for private in (API_KEY, USER_TOKEN, "unrelated-provider-secret", "private-call-id", "synthetic-private-worker-session"):
                self.assertNotIn(private, raw)
        self.assertEqual(runner.inspect(directory)["state"], "recorded")

    def test_key_metadata_must_match_funded_standard_dedicated_key(self):
        cases = [snapshot(quota="0"), snapshot(quota="2"), snapshot(status="disabled"),
                 snapshot(current_concurrency=1), snapshot(group_subscription_type="subscription"),
                 snapshot(key_fingerprint="wrong-key")]
        for index, before in enumerate(cases):
            with self.subTest(index=index):
                directory = self.prepare(directory=self.root / f"run-{index}")
                with self.assertRaises(runner.RunnerError):
                    self.run_prepared(directory, before=before)
                self.assertFalse((directory / "start.json").exists())
                self.assertFalse((directory / "workspace/child-env-check.json").exists())

    def test_started_run_is_never_reissued_even_after_process_crash(self):
        directory = self.prepare()

        class SimulatedCrash(BaseException):
            pass

        with patch.object(runner, "stream_client", side_effect=SimulatedCrash):
            with self.assertRaises(SimulatedCrash):
                self.run_prepared(directory)
        report = runner.inspect(directory)
        self.assertEqual(report["outcome"], "unknown")
        self.assertEqual(report["reserved_unknown_usd"], "1")
        self.assertFalse(report["automatic_retry_permitted"])
        with patch.dict(os.environ, {"SUB2API_API_KEY": API_KEY, "SUB2API_USER_TOKEN": USER_TOKEN}):
            capture = unittest.mock.Mock(side_effect=AssertionError("no network after marker"))
            with self.assertRaisesRegex(runner.RunnerError, "started_runs_cannot_be_reissued"):
                runner.run(directory, capture=capture, compare=comparison)
            capture.assert_not_called()

    def test_prepared_config_and_symlink_changes_block_launch(self):
        directory = self.prepare()
        (directory / "omp-agent/config.yml").write_text("changed: true\n")
        with self.assertRaisesRegex(runner.RunnerError, "prepared_configuration_changed"):
            self.run_prepared(directory)
        other = self.prepare(directory=self.root / "other")
        config = other / "omp-agent/config.yml"
        config.unlink()
        config.symlink_to(directory / "omp-agent/config.yml")
        with self.assertRaisesRegex(runner.RunnerError, "symlink"):
            self.run_prepared(other)

    def test_failed_client_cannot_pass_from_files_alone(self):
        self.fake.write_text(FAKE_CLIENT + '\nraise SystemExit(9)\n')
        directory = self.prepare()
        report = self.run_prepared(directory)
        self.assertEqual(report["result"]["outcome"], "failed")
        self.assertFalse(report["result"]["completed"])
        self.assertEqual(report["result"]["reserved_unknown_usd"], "1")

    def test_valid_dispatch_followed_by_invalid_or_truncated_events_is_not_completed(self):
        for index, tail in enumerate(('print("private malformed event")\n', 'print("x" * 300000)\n',
                                      'print(json.dumps({"type":"session", "id":"x"}))\n' * 10)):
            with self.subTest(index=index):
                self.fake.write_text(FAKE_CLIENT + '\n' + tail)
                directory = self.prepare(directory=self.root / f"bad-events-{index}")
                with patch.object(runner, "MAX_EVENTS", 4 if index == 2 else 2000):
                    result = self.run_prepared(directory)["result"]
                self.assertEqual(result["outcome"], "protocol_error")
                self.assertFalse(result["completed"])
                self.assertTrue(result["quality_passed"], "task assertions and protocol evidence are separate")
                self.assertEqual(result["reserved_unknown_usd"], "1")

    def test_missing_post_run_billing_cannot_release_the_unknown_reserve(self):
        directory = self.prepare()
        checker = SimpleNamespace(check=lambda *_: {"quality_passed": True, "test_exit_code": 0})
        calls = []

        def capture(*_, **__):
            calls.append(True)
            if len(calls) > 1:
                raise ValueError("synthetic-private-error-body")
            return snapshot()

        with patch.dict(os.environ, {"SUB2API_API_KEY": API_KEY, "SUB2API_USER_TOKEN": USER_TOKEN}), \
                patch.object(runner, "smoke_module", return_value=checker):
            report = runner.run(directory, capture=capture, compare=comparison)
        self.assertIsNone(report["result"]["confirmed_usd"])
        self.assertEqual(report["result"]["reserved_unknown_usd"], "1")
        self.assertEqual(report["result"]["cost_scope"], "partial")
        self.assertNotIn("synthetic-private-error-body", (directory / "evidence.json").read_text())

    def test_version_mismatch_prevents_even_metering_preflight(self):
        self.fake.write_text('print("999.0.0")\n')
        directory = self.prepare()
        capture = unittest.mock.Mock(side_effect=AssertionError("no network"))
        with patch.dict(os.environ, {"SUB2API_API_KEY": API_KEY, "SUB2API_USER_TOKEN": USER_TOKEN}):
            with self.assertRaisesRegex(runner.RunnerError, "client_version_mismatch"):
                runner.run(directory, capture=capture, compare=comparison)
        capture.assert_not_called()
        self.assertFalse((directory / "start.json").exists())

    def test_timeout_terminates_the_process_group_and_keeps_unknown(self):
        self.fake.write_text('''import subprocess, sys, time
if "--version" in sys.argv:
    print("18.12.6")
    raise SystemExit(0)
subprocess.Popen([sys.executable, "-c", "import time; from pathlib import Path; time.sleep(1); Path('escaped-child').write_text('bad')"])
time.sleep(10)
''')
        directory = self.prepare(timeout_seconds=1)
        _, metadata = runner.load_run(directory)
        metadata["timeout_seconds"] = 0.15
        # Exercise the group termination directly with a shorter test deadline.
        collector = runner.EventEvidence(metadata)
        process = runner.stream_client([sys.executable, str(self.fake)], directory / "workspace", os.environ.copy(), 0.15, collector)
        self.assertEqual(process["status"], "timeout")
        time.sleep(1.05)
        self.assertFalse((directory / "workspace/escaped-child").exists())

    def test_escaped_descendant_pipe_cannot_block_runner_beyond_deadline(self):
        self.fake.write_text('''import subprocess, sys, time
if "--version" in sys.argv:
    print("18.12.6")
    raise SystemExit(0)
child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(10)"], start_new_session=True)
with open("escape-pid", "w") as output:
    output.write(str(child.pid))
''')
        directory = self.prepare()
        _, metadata = runner.load_run(directory)
        started = time.monotonic()
        try:
            process = runner.stream_client([sys.executable, str(self.fake)], directory / "workspace", os.environ.copy(), 0.5,
                                           runner.EventEvidence(metadata))
            self.assertEqual(process["status"], "child_process_unfinished")
            self.assertLess(time.monotonic() - started, 2)
        finally:
            child_pid = int((directory / "workspace/escape-pid").read_text())
            try:
                os.killpg(child_pid, signal.SIGKILL)
            except ProcessLookupError:
                pass


class EventEvidenceTests(unittest.TestCase):
    def collector(self, client="omp"):
        return runner.EventEvidence({"client": client, "direct_model": "concrete", "fusion_model": "fusion-a"})

    def task(self, collector, *, call="call-a", phase="start", requests=1, fallback=False, identity="sub2api-fusion/fusion-a"):
        if phase == "start":
            event = {"type": "tool_execution_start", "toolName": "task", "toolCallId": call, "args": {"agent": "fusion-worker", "task": "secret-task"}}
        else:
            event = {"type": "tool_execution_end", "toolName": "task", "toolCallId": call, "isError": False, "result": {"details": {"results": [
                {"agent": "fusion-worker", "exitCode": 0, "requests": requests, "resolvedModelIdentity": identity, "resolvedModelIsFallback": fallback}]}}}
        collector.accept(json.dumps(event))

    def test_worker_proof_requires_matched_calls_real_requests_and_exact_model(self):
        for failure in ("end_only", "different_call", "zero_requests", "fallback", "different_model"):
            with self.subTest(failure=failure):
                collector = self.collector()
                if failure != "end_only":
                    self.task(collector)
                self.task(collector, phase="end", call="call-b" if failure == "different_call" else "call-a",
                          requests=0 if failure == "zero_requests" else 1,
                          fallback=failure == "fallback", identity="other-provider/fusion-a" if failure == "different_model" else "sub2api-fusion/fusion-a")
                self.assertFalse(collector.dispatch_verified)
        collector = self.collector()
        self.task(collector)
        self.task(collector, phase="end")
        self.assertTrue(collector.dispatch_verified)
        self.assertNotIn("secret-task", json.dumps(collector.report()))

    def test_omo_requested_worker_is_not_invented_model_or_child_trace_proof(self):
        collector = self.collector("omo")
        collector.accept(json.dumps({"type": "tool_use", "sessionID": "private-session", "part": {
            "tool": "task", "callID": "private-call", "state": {"status": "completed", "input": {
                "subagent_type": "fusion-worker", "prompt": "private-prompt"}, "metadata": {"sessionId": "private-child"}, "output": "private-output"}}}))
        report = collector.report()
        self.assertEqual(report["dispatch_evidence"], "requested_only")
        self.assertFalse(report["child_tool_trace_complete"])
        self.assertFalse(collector.dispatch_verified)
        for private in ("private-session", "private-call", "private-prompt", "private-child", "private-output"):
            self.assertNotIn(private, json.dumps(report))

    def test_an_unexpected_parent_model_is_recorded_without_exporting_its_value(self):
        collector = self.collector()
        collector.accept(json.dumps({"type": "message_end", "message": {
            "role": "assistant", "model": "private-unexpected-model", "content": "private-content"}}))
        report = collector.report()
        self.assertTrue(report["model_route_mismatch_observed"])
        self.assertNotIn("private-unexpected-model", json.dumps(report))
        self.assertNotIn("private-content", json.dumps(report))

    def test_malformed_and_oversized_output_does_not_become_success(self):
        collector = self.collector()
        collector.accept("not-json-private-text")
        collector.accept(json.dumps({"type": "error", "error": "private-error"}))
        collector.accept(json.dumps({"type": "tool_execution_start", "toolName": "task", "toolCallId": "a", "args": {"tasks": None}}))
        self.assertEqual(collector.invalid_lines, 1)
        self.assertTrue(collector.error_observed)
        self.assertFalse(collector.dispatch_verified)
        self.assertNotIn("private-error", json.dumps(collector.report()))


if __name__ == "__main__":
    unittest.main()
