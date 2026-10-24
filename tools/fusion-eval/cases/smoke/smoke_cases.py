#!/usr/bin/env python3
"""Initialize development smoke workspaces and check them independently.

No provider, gateway or model is contacted. Keep this file outside the workspaces
under evaluation: check executes its own assertions, never workspace test files.
"""

import argparse
import hashlib
import importlib
import json
import os
from pathlib import Path
import secrets
import signal
import stat
import subprocess
import sys
import tempfile
import unittest


SCOPE = {
    "kind": "development_smoke_only",
    "checks": ["workspace_changes", "independent_assertions", "test_exit_code"],
    "does_not_check": ["agent_delegation", "tool_execution_count", "tool_deduplication",
                       "gateway_billing", "paid_evaluation_quality"],
}

# The verifier imports model-generated workspace code. It needs OS/runtime
# settings, never the runner's user JWT, provider keys, or other credentials.
# Copy HOME only if it already exists; do not redirect the user's home.
VERIFIER_ENV_KEYS = (
    "HOME", "PATH", "LANG", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "LC_TIME",
    "LC_NUMERIC", "TZ", "TMPDIR", "TEMP", "TMP", "SYSTEMROOT", "SystemRoot",
    "WINDIR", "COMSPEC", "PATHEXT",
)


class SmokeError(ValueError):
    """An unsafe path or invalid smoke operation was rejected."""


COMMON_TASK = """This is a development smoke task, not a frozen benchmark case.
The outer agent must dispatch this task to the explicitly configured
`fusion-worker`. The outer agent must not implement the repair itself.
The worker must use the declared local read/edit/write/command tools, must not
dispatch another agent, and must report files changed, commands and exit codes.
The outer agent must independently inspect the files and test results afterward.
Do not modify the supplied tests, input fixtures, run_report.py, or smoke metadata.
Do not call providers directly or install dependencies. Python standard library only.
The file checker validates code/artifacts; separate client/gateway evidence is
required to establish actual delegation, tool calls, recovery and costs.
"""


SHORT_FILES = {
    "order_math.py": '''"""Integer-cent order calculations."""


def subtotal_cents(lines):
    # BUG: quantity is ignored and invalid values are not rejected.
    return sum(line["unit_cents"] for line in lines)
''',
    "receipt.py": '''"""Discounted receipts."""
from order_math import subtotal_cents


def receipt_total(lines, shipping_cents=0, discount_percent=0):
    # BUG: discounts are ignored.
    return subtotal_cents(lines) + shipping_cents
''',
    "test_receipts.py": '''import unittest
from order_math import subtotal_cents
from receipt import receipt_total


class ReceiptTests(unittest.TestCase):
    def test_quantity(self):
        self.assertEqual(subtotal_cents([
            {"unit_cents": 350, "quantity": 2},
            {"unit_cents": 199, "quantity": 1},
        ]), 899)

    def test_discount_before_shipping(self):
        self.assertEqual(receipt_total([
            {"unit_cents": 350, "quantity": 2},
            {"unit_cents": 199, "quantity": 1},
        ], shipping_cents=125, discount_percent=10), 934)


if __name__ == "__main__":
    unittest.main()
''',
    "TASK.md": COMMON_TASK + """
Repair order_math.py and receipt.py. Both files require a real code change.
subtotal_cents must multiply each nonnegative integer unit_cents by its
nonnegative integer quantity. Reject missing/non-integer/negative fields with
ValueError; bool is not an integer for this API. Empty lines produce zero.
receipt_total must apply an integer discount_percent in [0, 100] to the subtotal,
round down in integer cents, then add nonnegative integer shipping_cents.
Invalid discount/shipping values raise ValueError; bool is invalid.
Run `python3 -m unittest -v test_receipts.py` and inspect its exit code.
Report the fix and validation evidence to the outer agent.
""",
}


REPORT_RUNNER = '''"""Create synthetic report artifacts after event_report has been repaired."""
import hashlib
import json
from pathlib import Path
from event_report import summarize


root = Path(__file__).absolute().parent
source = root / "event_report.py"
data = root / "inputs" / "events.jsonl"
out = root / "artifacts"
if out.is_symlink() or source.is_symlink() or data.is_symlink():
    raise SystemExit("Refusing symlink fixture path")
out.mkdir(exist_ok=True)
report_path = out / "report.json"
receipt_path = out / "command-receipt.json"
if report_path.is_symlink() or receipt_path.is_symlink():
    raise SystemExit("Refusing symlink output path")
events = [json.loads(line) for line in data.read_text().splitlines() if line.strip()]
report_bytes = (json.dumps(summarize(events), sort_keys=True) + "\\n").encode()
report_path.write_bytes(report_bytes)
receipt = {
    "schema_version": 1,
    "command": "python3 run_report.py",
    "exit_code": 0,
    "source_sha256": hashlib.sha256(source.read_bytes()).hexdigest(),
    "input_sha256": hashlib.sha256(data.read_bytes()).hexdigest(),
    "report_sha256": hashlib.sha256(report_bytes).hexdigest(),
}
receipt_path.write_text(json.dumps(receipt, sort_keys=True) + "\\n")
print("SMOKE_REPORT_WRITTEN")
'''


ROUNDTRIP_FILES = {
    "event_report.py": '''"""Deduplicated synthetic event reports."""


def summarize(events):
    # BUG: the report implementation is missing.
    return {"event_count": 0, "totals": {}, "event_ids": []}
''',
    "inputs/events.jsonl": (
        '{"id":"evt-1","kind":"read","value":2}\n'
        '{"id":"evt-2","kind":"write","value":3}\n'
        '{"id":"evt-1","kind":"read","value":99}\n'
        '{"id":"evt-3","kind":"read","value":5}\n'
    ),
    "run_report.py": REPORT_RUNNER,
    "test_event_report.py": '''import unittest
from event_report import summarize


class EventTests(unittest.TestCase):
    def test_first_duplicate_wins(self):
        self.assertEqual(summarize([
            {"id": "b", "kind": "read", "value": 2},
            {"id": "a", "kind": "write", "value": 3},
            {"id": "b", "kind": "read", "value": 99},
        ]), {"event_count": 2, "totals": {"read": 2, "write": 3},
             "event_ids": ["a", "b"]})

    def test_empty(self):
        self.assertEqual(summarize([]),
                         {"event_count": 0, "totals": {}, "event_ids": []})


if __name__ == "__main__":
    unittest.main()
''',
    "TASK.md": COMMON_TASK + """
Read inputs/events.jsonl and repair event_report.py:summarize(events).
Each event must have nonempty string id/kind and a nonnegative integer value
(bool is invalid). Validate every event; invalid events raise ValueError.
For duplicate IDs the first valid event wins. Return event_count of unique IDs,
per-kind summed totals, and event_ids sorted lexicographically. Empty input
returns {"event_count": 0, "totals": {}, "event_ids": []}.
Run `python3 -m unittest -v test_event_report.py`, then `python3 run_report.py`.
Read artifacts/report.json and artifacts/command-receipt.json using tools and
verify the report has 3 events, read total 7, write total 3. Preserve the input,
runner and supplied tests. Report actual command exit codes and artifact paths.
The receipt is a consistency artifact, not proof of a tool invocation; the
outer agent and evaluation runner must retain their own execution evidence.
""",
}


CASES = {
    "short-repair": {"files": SHORT_FILES,
                     "editable": ("order_math.py", "receipt.py"),
                     "protected": ("test_receipts.py",)},
    "tool-roundtrip": {"files": ROUNDTRIP_FILES,
                       "editable": ("event_report.py",),
                       "protected": ("inputs/events.jsonl", "run_report.py", "test_event_report.py")},
}


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def safe_path(value, *, must_exist):
    """Require an absolute path with no symlink or '..' components."""
    path = Path(value)
    if not path.is_absolute() or ".." in path.parts or path == Path(path.anchor):
        raise SmokeError("workspace must be an absolute non-root path without '..'")
    current = Path(path.anchor)
    for part in path.parts[1:]:
        current /= part
        try:
            mode = current.lstat().st_mode
        except FileNotFoundError:
            if current == path and not must_exist:
                return path
            raise SmokeError("workspace parent does not exist") from None
        if stat.S_ISLNK(mode):
            raise SmokeError("symlink workspace paths are not allowed")
        if not stat.S_ISDIR(mode):
            raise SmokeError("workspace path components must be directories")
    if not must_exist:
        raise SmokeError("workspace already exists; initialization never overwrites it")
    return path


def regular_file(root, relative):
    """Read only a regular fixture file through nonsymlink directories."""
    path = root
    parts = Path(relative).parts
    for index, part in enumerate(parts):
        path /= part
        try:
            mode = path.lstat().st_mode
        except FileNotFoundError:
            raise SmokeError(f"required file missing: {relative}") from None
        if stat.S_ISLNK(mode):
            raise SmokeError(f"symlink fixture path is not allowed: {relative}")
        if index < len(parts) - 1 and not stat.S_ISDIR(mode):
            raise SmokeError(f"fixture parent is not a directory: {relative}")
        if index == len(parts) - 1 and not stat.S_ISREG(mode):
            raise SmokeError(f"fixture is not a regular file: {relative}")
    if path.stat().st_size > 1024 * 1024:
        raise SmokeError(f"fixture exceeds 1 MiB limit: {relative}")
    return path.read_bytes()


def initialize(case, workspace):
    if case not in CASES:
        raise SmokeError("unknown smoke case")
    root = safe_path(workspace, must_exist=False)
    root.mkdir(mode=0o700)
    for name, contents in CASES[case]["files"].items():
        path = root / name
        path.parent.mkdir(exist_ok=True)
        with path.open("x", encoding="utf-8") as stream:
            stream.write(contents)
    metadata = {"schema_version": 1, "case_id": case, "scope": SCOPE}
    with (root / ".fusion-smoke.json").open("x", encoding="utf-8") as stream:
        json.dump(metadata, stream, sort_keys=True)
        stream.write("\n")
    return {"schema_version": 1, "case_id": case, "initialized": True,
            "workspace": str(root), "task_file": str(root / "TASK.md"), "scope": SCOPE}


def run_verifier(case, root, timeout_seconds):
    """Execute trusted assertions outside the workspace with a bounded lifetime."""
    # A process exit of zero is not evidence that unittest finished: imported
    # workspace code can call os._exit(0). Require a fresh completion receipt,
    # written by this trusted verifier only after the whole suite returns.
    with tempfile.TemporaryDirectory(prefix="fusion-smoke-verify-") as temporary:
        completion_path = Path(temporary) / "suite-completion.json"
        if completion_path.resolve().is_relative_to(root):
            raise SmokeError("verification receipts must remain outside the workspace")
        completion_token = secrets.token_hex(32)
        return _run_verifier_process(case, root, timeout_seconds, completion_path, completion_token)


def _run_verifier_process(case, root, timeout_seconds, completion_path, completion_token):
    command = [sys.executable, "-I", "-B", str(Path(__file__).resolve()),
               "_verify", "--case", case, "--workspace", str(root),
               "--completion-receipt", str(completion_path),
               "--completion-token", completion_token]
    with tempfile.TemporaryFile() as output:
        process = subprocess.Popen(command, cwd=root, stdin=subprocess.DEVNULL,
                                   stdout=output, stderr=subprocess.STDOUT,
                                   env={key: os.environ[key] for key in VERIFIER_ENV_KEYS if key in os.environ},
                                   start_new_session=os.name == "posix")
        timed_out = False
        try:
            exit_code = process.wait(timeout=timeout_seconds)
        except subprocess.TimeoutExpired:
            timed_out = True
            if os.name == "posix":
                os.killpg(process.pid, signal.SIGKILL)
            else:
                process.kill()
            process.wait(timeout=5)
            exit_code = process.returncode
        output.seek(0)
        digest, byte_count = hashlib.sha256(), 0
        while chunk := output.read(65536):
            digest.update(chunk)
            byte_count += len(chunk)
    completion = None
    try:
        if completion_path.is_file() and not completion_path.is_symlink() and completion_path.stat().st_size <= 4096:
            completion = json.loads(completion_path.read_text())
    except (OSError, UnicodeError, ValueError):
        pass
    expected = {"schema_version": 1, "case_id": case, "token": completion_token,
                "completed": True, "tests_run": 2,
                "skipped": 1 if case == "short-repair" else 0,
                "failures": 0, "errors": 0, "successful": True}
    suite_successful = (completion == expected and
                        all(type(completion.get(name)) is int for name in
                            ("schema_version", "tests_run", "skipped", "failures", "errors")) and
                        completion.get("completed") is True and
                        completion.get("successful") is True)
    suite_completed = (isinstance(completion, dict) and
                       completion.get("schema_version") == 1 and
                       completion.get("case_id") == case and
                       completion.get("token") == completion_token and
                       completion.get("completed") is True and
                       type(completion.get("tests_run")) is int and
                       completion["tests_run"] == 2)
    return {"test_exit_code": exit_code, "timed_out": timed_out,
            "test_output_sha256": digest.hexdigest(), "test_output_bytes": byte_count,
            "test_suite_completed": suite_completed,
            "test_suite_successful": suite_successful,
            "tests_run": completion["tests_run"] if suite_completed else None}


def check(case, workspace, timeout_seconds=20):
    if case not in CASES:
        raise SmokeError("unknown smoke case")
    if isinstance(timeout_seconds, bool) or not 0 < timeout_seconds <= 60:
        raise SmokeError("timeout must be greater than zero and at most 60 seconds")
    root = safe_path(workspace, must_exist=True)
    if Path(__file__).resolve().is_relative_to(root):
        raise SmokeError("independent checker must remain outside the workspace")
    specification = CASES[case]
    changed = []
    for name in specification["editable"]:
        actual = regular_file(root, name)
        if sha256(actual) != sha256(specification["files"][name].encode()):
            changed.append(name)
    modified_protected = [name for name in specification["protected"]
                          if regular_file(root, name) != specification["files"][name].encode()]
    result = {"schema_version": 1, "case_id": case, "workspace": str(root),
              "scope": SCOPE, "changed_files": changed,
              "required_changes_complete": len(changed) == len(specification["editable"]),
              "modified_protected_files": modified_protected,
              "quality_passed": False, "test_exit_code": None, "timed_out": False,
              "test_suite_completed": False, "test_suite_successful": False,
              "tests_run": None}
    if modified_protected:
        result["failure_reason"] = "protected_fixture_modified"
        return result
    result.update(run_verifier(case, root, timeout_seconds))
    result["quality_passed"] = (result["required_changes_complete"] and
                                result["test_exit_code"] == 0 and not result["timed_out"] and
                                result["test_suite_completed"] and result["test_suite_successful"])
    if not result["quality_passed"]:
        if not result["test_suite_completed"]:
            result["failure_reason"] = "independent_test_suite_incomplete"
        elif not result["test_suite_successful"] or result["test_exit_code"]:
            result["failure_reason"] = "independent_assertions_failed"
        else:
            result["failure_reason"] = "required_source_changes_missing"
    return result


def verify(case, workspace, completion_path, completion_token):
    """Trusted assertions; workspace-provided tests are intentionally not imported."""
    root = safe_path(workspace, must_exist=True)
    sys.path.insert(0, str(root))

    class IndependentTests(unittest.TestCase):
        def test_implementation(self):
            if case == "short-repair":
                math = importlib.import_module("order_math")
                receipt = importlib.import_module("receipt")
                lines = [{"unit_cents": 101, "quantity": 3}, {"unit_cents": 999, "quantity": 0}]
                self.assertEqual(math.subtotal_cents(lines), 303)
                self.assertEqual(receipt.receipt_total(lines, 7, 33), 210)
                self.assertEqual(receipt.receipt_total([], 75, 100), 75)
                self.assertEqual(receipt.receipt_total(lines, 0, 100), 0)
                for field in ("unit_cents", "quantity"):
                    for bad in (-1, 1.5, True, "2", None):
                        invalid = {"unit_cents": 1, "quantity": 1, field: bad}
                        with self.subTest(field=field, bad=bad), self.assertRaises(ValueError):
                            math.subtotal_cents([invalid])
                    with self.assertRaises(ValueError):
                        math.subtotal_cents([{key: 1 for key in ("unit_cents", "quantity") if key != field}])
                for shipping, discount in ((-1, 0), (True, 0), (1.5, 0), (0, -1), (0, 101), (0, True), (0, 1.5)):
                    with self.subTest(shipping=shipping, discount=discount), self.assertRaises(ValueError):
                        receipt.receipt_total(lines, shipping, discount)
            else:
                report = importlib.import_module("event_report")
                events = [{"id": "b", "kind": "read", "value": 4},
                          {"id": "a", "kind": "read", "value": 6},
                          {"id": "b", "kind": "write", "value": 99},
                          {"id": "z", "kind": "write", "value": 0}]
                self.assertEqual(report.summarize(events), {"event_count": 3, "totals": {"read": 10, "write": 0}, "event_ids": ["a", "b", "z"]})
                self.assertEqual(report.summarize([]), {"event_count": 0, "totals": {}, "event_ids": []})
                for bad in ({"id": "", "kind": "read", "value": 1},
                            {"id": "a", "kind": "", "value": 1},
                            {"id": "a", "kind": "read", "value": -1},
                            {"id": "a", "kind": "read", "value": True},
                            {"id": "a", "kind": "read", "value": 1.5}, {}):
                    with self.subTest(bad=bad), self.assertRaises(ValueError):
                        report.summarize([bad])

        @unittest.skipUnless(case == "tool-roundtrip", "report artifacts are specific to this case")
        def test_existing_artifacts(self):
            output = regular_file(root, "artifacts/report.json")
            actual = json.loads(output)
            self.assertEqual(actual, {"event_count": 3, "totals": {"read": 7, "write": 3}, "event_ids": ["evt-1", "evt-2", "evt-3"]})
            receipt = json.loads(regular_file(root, "artifacts/command-receipt.json"))
            self.assertEqual(receipt, {"schema_version": 1, "command": "python3 run_report.py", "exit_code": 0,
                                      "source_sha256": sha256(regular_file(root, "event_report.py")),
                                      "input_sha256": sha256(regular_file(root, "inputs/events.jsonl")),
                                      "report_sha256": sha256(output)})

    suite = unittest.defaultTestLoader.loadTestsFromTestCase(IndependentTests)
    result = unittest.TextTestRunner(stream=sys.stderr, verbosity=2).run(suite)
    completion = {"schema_version": 1, "case_id": case, "token": completion_token,
                  "completed": True, "tests_run": result.testsRun,
                  "skipped": len(result.skipped), "failures": len(result.failures),
                  "errors": len(result.errors), "successful": result.wasSuccessful()}
    with completion_path.open("x", encoding="utf-8") as stream:
        json.dump(completion, stream, sort_keys=True)
        stream.write("\n")
    return 0 if result.wasSuccessful() else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("init", "check", "_verify"))
    parser.add_argument("--case", choices=tuple(CASES), required=True)
    parser.add_argument("--workspace", type=Path, required=True)
    parser.add_argument("--timeout-seconds", type=float, default=20)
    parser.add_argument("--completion-receipt", type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--completion-token", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.action == "_verify":
        if args.completion_receipt is None or not args.completion_token:
            parser.error("internal verification requires a completion receipt and token")
        return verify(args.case, args.workspace, args.completion_receipt, args.completion_token)
    try:
        result = initialize(args.case, args.workspace) if args.action == "init" else check(args.case, args.workspace, args.timeout_seconds)
    except (SmokeError, OSError) as error:
        print(json.dumps({"schema_version": 1, "case_id": args.case,
                          "quality_passed": False, "error": str(error), "scope": SCOPE}, sort_keys=True))
        return 2
    print(json.dumps(result, sort_keys=True))
    return 0 if args.action == "init" or result["quality_passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
