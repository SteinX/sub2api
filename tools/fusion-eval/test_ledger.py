"""Offline evidence and budget invariants; no provider or credentials are used."""

from copy import deepcopy
from concurrent.futures import ThreadPoolExecutor
from contextlib import redirect_stderr, redirect_stdout
from decimal import Decimal
import io
import json
from pathlib import Path
import tempfile
import unittest

import ledger


def run_record(case="case-one", arm="lead", **updates):
    """Create explicit synthetic evidence, not a completed real benchmark."""
    value = {
        "schema_version": 1, "record_id": f"synthetic-{case}-{arm}",
        "cohort_id": "unit-test", "case_id": case, "client": "omp", "arm": arm,
        "comparison_snapshot": {"fixture": "synthetic-v1", "worker_variants": {"lead": "lead-real", "fusion": ["lead-real", "worker-real"]}},
        "outcome": "completed", "completed": True, "quality_passed": True,
        "cost_scope": "full_task", "confirmed_usd": "0.2",
        "reserved_unknown_usd": "0", "inflight_reserve_usd": "0",
    }
    value.update(updates)
    return value


class SummaryTests(unittest.TestCase):
    def test_failed_runs_stay_in_cost_and_quality_denominators(self):
        """A cheaper-looking successful subset must not hide failed spend."""
        result = ledger.summarize([
            run_record(confirmed_usd="1.25"),
            run_record("case-two", outcome="budget_exhausted", completed=False,
                       quality_passed=None, confirmed_usd="0.75"),
        ])
        metrics = result["observed"]
        self.assertEqual(metrics["assigned_runs"], 2)
        self.assertEqual(metrics["successful_runs"], 1)
        self.assertEqual(metrics["success_rate"], .5)
        self.assertEqual(Decimal(metrics["cost_per_success"]), Decimal("2"))
        self.assertEqual(len(result["incomplete_pairs"]), 2)

    def test_complete_and_incomplete_pairs_are_separate(self):
        """A single arm is retained but cannot masquerade as a paired comparison."""
        result = ledger.summarize([run_record(), run_record(arm="fusion"), run_record("second")])
        self.assertEqual(len(result["complete_pairs"]), 1)
        self.assertEqual(result["incomplete_pairs"][0]["missing_arms"], ["fusion"])
        self.assertEqual(result["paired_comparison"]["case_count"], 1)

    def test_unknown_and_inflight_costs_are_not_free(self):
        """Pending liabilities are explicit and make confirmed cost a lower bound."""
        result = ledger.summarize([
            run_record(), run_record(arm="fusion", outcome="unknown", completed=False,
                                     quality_passed=None, confirmed_usd="0.1",
                                     reserved_unknown_usd="0.4", inflight_reserve_usd="0.3"),
        ])["observed"]
        self.assertFalse(result["costs_final"])
        self.assertEqual(Decimal(result["confirmed_usd"]), Decimal("0.3"))
        self.assertEqual(Decimal(result["reserved_unknown_usd"]), Decimal("0.4"))
        self.assertTrue(result["cost_per_success_is_lower_bound"])

    def test_missing_and_partial_fees_do_not_become_zero(self):
        """Without full cost evidence there is no defensible cost-per-success value."""
        for updates in ({"confirmed_usd": None}, {"cost_scope": "partial"}):
            with self.subTest(updates=updates):
                metrics = ledger.summarize([run_record(**updates)])["observed"]
                self.assertIsNone(metrics["cost_per_success"])
                self.assertFalse(metrics["costs_final"])
        record = run_record()
        del record["confirmed_usd"]
        self.assertEqual(ledger.summarize([record])["observed"]["missing_confirmed_cost_runs"], 1)

    def test_zero_success_has_null_cost_per_success(self):
        """A failed cohort cannot be reported as free per successful task."""
        metrics = ledger.summarize([run_record(outcome="failed", completed=False, quality_passed=False)])["observed"]
        self.assertIsNone(metrics["cost_per_success"])

    def test_duplicates_and_changed_snapshots_are_rejected(self):
        """Evidence cannot be overwritten by a rerun or paired across configurations."""
        lead = run_record()
        for other in (deepcopy(lead), run_record(record_id="replacement"), run_record(arm="fusion", record_id=lead["record_id"])):
            with self.subTest(other=other["record_id"]), self.assertRaises(ledger.LedgerError):
                ledger.summarize([lead, other])
        with self.assertRaisesRegex(ledger.LedgerError, "snapshot mismatch"):
            ledger.summarize([lead, run_record(arm="fusion", comparison_snapshot={"fixture": "changed"})])

    def test_negative_nonfinite_and_misspelled_fields_are_rejected(self):
        """Negative or non-finite amounts and unknown accounting fields fail closed."""
        for amount in ("-0.1", "NaN", "Infinity", float("nan"), float("inf"), True):
            with self.subTest(amount=str(amount)), self.assertRaises(ledger.LedgerError):
                ledger.validate_run(run_record(confirmed_usd=amount))
        with self.assertRaises(ledger.LedgerError):
            ledger.validate_run(run_record(confirm_usd="0"))
        with self.assertRaises(ledger.LedgerError):
            ledger.validate_run(run_record(outcome="unknown", completed=False, quality_passed=None, confirmed_usd="0"))
        for raw in ('{"fee": NaN}', '{"fee": Infinity}', '{"fee": 1e999}', '{"id":1,"id":2}', '\n'):
            with self.subTest(raw=raw), self.assertRaises(ledger.LedgerError):
                ledger.parse_jsonl(raw)

    def test_example_manifest_is_eight_unexecuted_unfrozen_specs(self):
        """The template provides proposed tasks, not fabricated benchmark evidence."""
        manifest = ledger.read_json(Path(__file__).with_name("cases-manifest.example.json"))
        result = ledger.summarize([], manifest)
        self.assertFalse(result["manifest_task_set_frozen"])
        self.assertEqual(len(manifest["cases"]), 8)
        self.assertEqual(len(result["missing_run_records"]), 16)
        self.assertFalse(result["record_coverage_complete"])
        self.assertEqual(result["observed"]["assigned_runs"], 0)
        self.assertEqual(sum(c["client"] == "omo" for c in manifest["cases"]), 4)


class BudgetTests(unittest.TestCase):
    def setUp(self):
        """Use a new local journal for each offline test."""
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / "budget.jsonl"
        self.snapshot = run_record()["comparison_snapshot"]

    def reserve(self, case="case-one", total="2", cap="0.75"):
        """Reserve both synthetic arms together."""
        return ledger.reserve_pair(self.path, "unit-test", case, "omp", self.snapshot, cap, cap, total)

    def test_pair_is_admitted_atomically_and_repeated_reservation_is_idempotent(self):
        """No first arm can consume the budget needed for its missing sibling."""
        result = self.reserve()
        self.assertTrue(result["admitted"])
        self.assertEqual(Decimal(result["unsettled_pair_reserve_usd"]), Decimal("1.5"))
        before = self.path.read_text()
        self.assertTrue(self.reserve()["replayed"])
        self.assertEqual(before, self.path.read_text())
        self.assertFalse(self.reserve("second")["admitted"])
        self.assertEqual(before, self.path.read_text())
        with self.assertRaises(ledger.LedgerError):
            self.reserve(total="20")

    def test_first_pair_must_fit_and_decimal_spellings_do_not_duplicate_allocations(self):
        """An unaffordable first pair is denied; equal decimal amounts are identical."""
        self.assertFalse(self.reserve(total="1")["admitted"])
        self.assertEqual(self.path.read_text(), "")
        self.reserve()
        self.assertTrue(self.reserve(cap="0.750")["replayed"])

    def test_missing_fee_cannot_release_original_allocation(self):
        """A missing fee leaves both original caps held in the reference journal."""
        self.reserve()
        before = self.path.read_text()
        with self.assertRaises(ledger.LedgerError):
            ledger.settle_run(self.path, run_record(confirmed_usd=None))
        self.assertEqual(before, self.path.read_text())

    def test_concurrent_pair_admission_cannot_overspend_the_shared_budget(self):
        """Two local callers cannot both reserve the same remaining headroom."""
        with ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(lambda case: self.reserve(case, total="3", cap="1"), ["one", "two"]))
        self.assertEqual(sum(result["admitted"] for result in results), 1)
        total, pairs, runs = ledger.replay_budget(ledger.parse_jsonl(self.path.read_text()))
        self.assertEqual(Decimal(ledger.budget_metrics(total, pairs, runs)["allocated_usd"]), Decimal("2"))

    def test_unknown_reserve_and_unstarted_sibling_remain_allocated(self):
        """Confirmed fees plus unknown/inflight liabilities all reduce admission."""
        self.reserve(total="3", cap="1")
        metrics = ledger.settle_run(self.path, run_record(outcome="unknown", completed=False,
                                  quality_passed=None, confirmed_usd="0.2",
                                  reserved_unknown_usd="0.6", inflight_reserve_usd="0.1"))
        self.assertEqual(Decimal(metrics["allocated_usd"]), Decimal("1.9"))
        self.assertEqual(Decimal(metrics["unsettled_pair_reserve_usd"]), Decimal("1"))
        self.assertFalse(self.reserve("second", total="3", cap="1")["admitted"])

    def test_duplicate_settlement_and_changed_snapshot_are_rejected(self):
        """Appending a second result cannot erase a first result or change its cohort."""
        self.reserve()
        ledger.settle_run(self.path, run_record())
        with self.assertRaises(ledger.LedgerError):
            ledger.settle_run(self.path, run_record())
        with self.assertRaises(ledger.LedgerError):
            ledger.settle_run(self.path, run_record(arm="fusion", comparison_snapshot={"fixture": "different"}))

    def test_budget_journal_rejects_unadmitted_or_duplicate_events(self):
        """A hand-edited journal cannot manufacture a valid second allocation."""
        self.reserve()
        events = ledger.parse_jsonl(self.path.read_text())
        extra = deepcopy(events[-1])
        extra["case_id"] = "unadmitted"
        with self.assertRaises(ledger.LedgerError):
            ledger.replay_budget(events + [extra])
        with self.assertRaises(ledger.LedgerError):
            ledger.replay_budget(events + [events[-1]])

    def test_cli_summary_and_budget_denial_have_machine_readable_results(self):
        """The command boundary reports local evidence and explicit denial statuses."""
        results = Path(self.directory.name) / "runs.jsonl"
        results.write_text(json.dumps(run_record()) + "\n")
        output = io.StringIO()
        with redirect_stdout(output):
            code = ledger.main(["summarize", str(results)])
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(output.getvalue())["observed"]["assigned_runs"], 1)
        snapshot = Path(self.directory.name) / "snapshot.json"
        snapshot.write_text(json.dumps(self.snapshot))
        with redirect_stdout(io.StringIO()):
            code = ledger.main(["reserve-pair", str(self.path), "--cohort-id", "unit-test",
                                "--case-id", "one", "--client", "omp", "--snapshot", str(snapshot),
                                "--lead-cap-usd", ".75", "--fusion-cap-usd", ".75", "--total-budget-usd", "1"])
        self.assertEqual(code, 3)
        results.write_bytes(b"\xff")
        with redirect_stderr(io.StringIO()):
            self.assertEqual(ledger.main(["summarize", str(results)]), 2)

    def test_observed_overrun_is_recorded_and_blocks_new_pairs(self):
        """A late higher bill must be recorded even if it exhausts the planned cap."""
        self.reserve()
        metrics = ledger.settle_run(self.path, run_record(confirmed_usd="3"))
        self.assertTrue(metrics["over_budget"])
        self.assertLess(Decimal(metrics["remaining_usd"]), 0)
        self.assertFalse(self.reserve("another")["admitted"])


if __name__ == "__main__":
    unittest.main()
