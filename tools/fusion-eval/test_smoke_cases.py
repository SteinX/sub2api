"""Development smoke fixtures must be independently checkable and path-safe."""

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).parent / "cases" / "smoke" / "smoke_cases.py"
SPEC = importlib.util.spec_from_file_location("fusion_smoke_cases", SCRIPT)
smoke = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(smoke)


FIXED_MATH = '''def subtotal_cents(lines):
    result = 0
    for line in lines:
        for field in ("unit_cents", "quantity"):
            if field not in line or type(line[field]) is not int or line[field] < 0:
                raise ValueError("invalid line")
        result += line["unit_cents"] * line["quantity"]
    return result
'''

FIXED_RECEIPT = '''from order_math import subtotal_cents


def receipt_total(lines, shipping_cents=0, discount_percent=0):
    if type(shipping_cents) is not int or shipping_cents < 0:
        raise ValueError("invalid shipping")
    if type(discount_percent) is not int or not 0 <= discount_percent <= 100:
        raise ValueError("invalid discount")
    return subtotal_cents(lines) * (100 - discount_percent) // 100 + shipping_cents
'''

FIXED_REPORT = '''def summarize(events):
    seen = {}
    for event in events:
        if (not isinstance(event.get("id"), str) or not event["id"] or
                not isinstance(event.get("kind"), str) or not event["kind"] or
                type(event.get("value")) is not int or event["value"] < 0):
            raise ValueError("invalid event")
        seen.setdefault(event["id"], event)
    totals = {}
    for event in seen.values():
        totals[event["kind"]] = totals.get(event["kind"], 0) + event["value"]
    return {"event_count": len(seen), "totals": totals, "event_ids": sorted(seen)}
'''


class SmokeCaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()

    def initialize(self, case):
        root = self.base / case
        result = smoke.initialize(case, root)
        self.assertTrue(result["initialized"])
        self.assertEqual(Path(result["task_file"]), root / "TASK.md")
        self.assertIn("fusion-worker", (root / "TASK.md").read_text())
        self.assertEqual(result["scope"]["kind"], "development_smoke_only")
        self.assertIn("gateway_billing", result["scope"]["does_not_check"])
        return root

    def fix_short(self, root):
        (root / "order_math.py").write_text(FIXED_MATH)
        (root / "receipt.py").write_text(FIXED_RECEIPT)

    def test_initial_cases_fail_real_assertions_and_do_not_create_artifacts(self):
        for case in smoke.CASES:
            with self.subTest(case=case):
                root = self.initialize(case)
                result = smoke.check(case, root)
                self.assertFalse(result["quality_passed"])
                self.assertNotEqual(result["test_exit_code"], 0)
                self.assertFalse((root / "__pycache__").exists())
                self.assertFalse((root / "artifacts").exists())

    def test_short_repair_passes_only_with_both_file_changes_and_valid_behavior(self):
        root = self.initialize("short-repair")
        (root / "order_math.py").write_text(FIXED_MATH)
        failed = smoke.check("short-repair", root)
        self.assertFalse(failed["quality_passed"])
        self.assertFalse(failed["required_changes_complete"])
        self.fix_short(root)
        passed = smoke.check("short-repair", root)
        self.assertTrue(passed["quality_passed"])
        self.assertEqual(passed["test_exit_code"], 0)
        self.assertTrue(passed["test_suite_completed"])
        self.assertTrue(passed["test_suite_successful"])
        self.assertEqual(passed["tests_run"], 2)
        self.assertCountEqual(passed["changed_files"], ["order_math.py", "receipt.py"])

    def test_rewriting_workspace_tests_cannot_fake_a_pass(self):
        root = self.initialize("short-repair")
        (root / "test_receipts.py").write_text("# the workspace test now does nothing\n")
        self.fix_short(root)
        result = smoke.check("short-repair", root)
        self.assertFalse(result["quality_passed"])
        self.assertEqual(result["failure_reason"], "protected_fixture_modified")
        self.assertIsNone(result["test_exit_code"])

    def test_independent_assertions_reject_public_example_only_solution(self):
        root = self.initialize("short-repair")
        (root / "order_math.py").write_text("def subtotal_cents(lines):\n    return 899\n")
        (root / "receipt.py").write_text("def receipt_total(*args, **kwargs):\n    return 934\n")
        visible = subprocess.run([sys.executable, "-B", "-m", "unittest", "test_receipts.py"],
                                 cwd=root, capture_output=True)
        self.assertEqual(visible.returncode, 0)
        self.assertFalse(smoke.check("short-repair", root)["quality_passed"])

    def test_roundtrip_requires_existing_report_and_matching_command_receipt(self):
        root = self.initialize("tool-roundtrip")
        (root / "event_report.py").write_text(FIXED_REPORT)
        self.assertFalse(smoke.check("tool-roundtrip", root)["quality_passed"])
        self.assertFalse((root / "artifacts").exists(), "check never manufactures command evidence")
        command = subprocess.run([sys.executable, "-B", "run_report.py"], cwd=root,
                                 capture_output=True)
        self.assertEqual(command.returncode, 0)
        self.assertTrue(smoke.check("tool-roundtrip", root)["quality_passed"])
        (root / "event_report.py").write_text(FIXED_REPORT + "\n# changed after command\n")
        self.assertFalse(smoke.check("tool-roundtrip", root)["quality_passed"],
                         "an artifact receipt must identify the currently tested source")

    def test_initializer_never_overwrites_existing_directory(self):
        root = self.base / "existing"
        root.mkdir()
        marker = root / "user-file.txt"
        marker.write_text("preserve me")
        with self.assertRaisesRegex(smoke.SmokeError, "already exists"):
            smoke.initialize("short-repair", root)
        self.assertEqual(marker.read_text(), "preserve me")
        self.assertEqual(list(root.iterdir()), [marker])

    def test_symlink_root_ancestor_and_required_file_are_rejected(self):
        actual = self.base / "actual"
        actual.mkdir()
        link = self.base / "linked"
        try:
            link.symlink_to(actual, target_is_directory=True)
        except (OSError, NotImplementedError):
            self.skipTest("symlinks unavailable")
        with self.assertRaisesRegex(smoke.SmokeError, "symlink"):
            smoke.initialize("short-repair", link)
        with self.assertRaisesRegex(smoke.SmokeError, "symlink"):
            smoke.initialize("short-repair", link / "child")
        self.assertFalse((actual / "child").exists())
        root = self.initialize("short-repair")
        outside = self.base / "outside.py"
        outside.write_text("preserve me")
        (root / "order_math.py").unlink()
        (root / "order_math.py").symlink_to(outside)
        with self.assertRaisesRegex(smoke.SmokeError, "symlink"):
            smoke.check("short-repair", root)
        self.assertEqual(outside.read_text(), "preserve me")

    def test_relative_traversal_and_root_paths_are_rejected(self):
        for path in (Path("relative"), self.base / ".." / "other", Path(self.base.anchor)):
            with self.subTest(path=path), self.assertRaises(smoke.SmokeError):
                smoke.initialize("short-repair", path)

    def test_checker_timeout_stops_hanging_code(self):
        root = self.initialize("short-repair")
        self.fix_short(root)
        (root / "order_math.py").write_text("while True:\n    pass\n")
        result = smoke.check("short-repair", root, timeout_seconds=0.2)
        self.assertFalse(result["quality_passed"])
        self.assertTrue(result["timed_out"])
        self.assertNotEqual(result["test_exit_code"], 0)

    def test_zero_exit_during_import_cannot_fake_suite_completion(self):
        for case, module in (("short-repair", "order_math.py"),
                             ("tool-roundtrip", "event_report.py")):
            with self.subTest(case=case):
                root = self.initialize(case)
                if case == "short-repair":
                    (root / "receipt.py").write_text(FIXED_RECEIPT)
                (root / module).write_text("import os\nos._exit(0)\n")
                result = smoke.check(case, root)
                self.assertTrue(result["required_changes_complete"])
                self.assertEqual(result["test_exit_code"], 0)
                self.assertFalse(result["test_suite_completed"])
                self.assertFalse(result["test_suite_successful"])
                self.assertIsNone(result["tests_run"])
                self.assertFalse(result["quality_passed"])
                self.assertEqual(result["failure_reason"], "independent_test_suite_incomplete")

    def test_cli_emits_machine_readable_json_and_failure_exit_code(self):
        root = self.base / "cli"
        initialized = subprocess.run([sys.executable, str(SCRIPT), "init", "--case", "short-repair",
                                      "--workspace", str(root)], capture_output=True, text=True)
        self.assertEqual(initialized.returncode, 0, initialized.stderr)
        self.assertTrue(json.loads(initialized.stdout)["initialized"])
        checked = subprocess.run([sys.executable, str(SCRIPT), "check", "--case", "short-repair",
                                  "--workspace", str(root)], capture_output=True, text=True)
        self.assertEqual(checked.returncode, 1)
        self.assertFalse(json.loads(checked.stdout)["quality_passed"])
        again = subprocess.run([sys.executable, str(SCRIPT), "init", "--case", "short-repair",
                                "--workspace", str(root)], capture_output=True, text=True)
        self.assertEqual(again.returncode, 2)
        self.assertIn("already exists", json.loads(again.stdout)["error"])

    def test_check_output_retains_only_hash_and_size_of_test_output(self):
        root = self.initialize("short-repair")
        self.fix_short(root)
        (root / "order_math.py").write_text('print("synthetic-private-test-output")\n' + FIXED_MATH)
        result = smoke.check("short-repair", root)
        self.assertTrue(result["quality_passed"])
        self.assertNotIn("synthetic-private-test-output", json.dumps(result))
        self.assertGreater(result["test_output_bytes"], 0)
        self.assertEqual(len(result["test_output_sha256"]), 64)

    def test_generated_implementation_never_inherits_runner_credentials(self):
        root = self.initialize("short-repair")
        self.fix_short(root)
        credential_keys = ("SUB2API_USER_TOKEN", "SUB2API_API_KEY", "OPENAI_API_KEY",
                           "AWS_SECRET_ACCESS_KEY", "UNRELATED_PROVIDER_SECRET")
        original_home = os.environ.get("HOME")
        environment_assertions = (
            "import os\n"
            f"assert not any(key in os.environ for key in {credential_keys!r})\n"
            f"assert os.environ.get('HOME') == {original_home!r}\n"
        )
        (root / "order_math.py").write_text(environment_assertions + FIXED_MATH)
        with patch.dict(os.environ, {key: "synthetic-parent-only-secret" for key in credential_keys}):
            result = smoke.check("short-repair", root)
        self.assertTrue(result["quality_passed"])
        self.assertTrue(result["test_suite_completed"])
        self.assertTrue(result["test_suite_successful"])
        self.assertNotIn("synthetic-parent-only-secret", json.dumps(result))


if __name__ == "__main__":
    unittest.main()
