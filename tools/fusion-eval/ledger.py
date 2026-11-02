#!/usr/bin/env python3
"""Summarize offline Fusion runs and reserve paired budgets; never call a provider.

Run JSONL has one immutable record per (cohort_id, case_id, client, arm):
  schema_version, record_id, cohort_id, case_id, client (omo/omp),
  arm (lead/fusion), comparison_snapshot (the common frozen comparison config),
  outcome, completed, quality_passed, cost_scope (full_task/partial),
  confirmed_usd (number, decimal string, or null), reserved_unknown_usd,
  inflight_reserve_usd. Unknown confirmed costs may also be omitted; they never
  become zero-cost runs. Optional wall_seconds and failure_reason are retained.

Both arms must carry exactly the same comparison_snapshot, describing BOTH
worker variants and the common client/tool/price/task-set versions. Only the
selected arm differs. Use a new cohort for a changed configuration or rerun.

The budget journal is a separate local reference, not a production admission
service. Unsettled arms retain their entire allocation. settle-run cannot release
an allocation with missing confirmed cost or partial cost coverage. Positive
unknown/inflight reserves remain charged against admission after settlement.
"""

import argparse
from collections import Counter
from contextlib import contextmanager
from decimal import Decimal, InvalidOperation
import hashlib
import json
import math
import os
from pathlib import Path
import sys


class LedgerError(ValueError):
    """The input cannot support an unambiguous accounting result."""


ARMS = ("lead", "fusion")
CLIENTS = ("omo", "omp")
OUTCOMES = {
    "completed", "failed", "protocol_error", "upstream_error", "tool_error",
    "budget_exhausted", "timeout", "unknown", "cancelled",
}
RUN_REQUIRED = {
    "schema_version", "record_id", "cohort_id", "case_id", "client", "arm",
    "comparison_snapshot", "outcome", "completed", "quality_passed",
    "cost_scope", "reserved_unknown_usd", "inflight_reserve_usd",
}
RUN_OPTIONAL = {"confirmed_usd", "wall_seconds", "failure_reason"}


def unique_object(pairs):
    """Reject duplicate JSON keys instead of silently replacing evidence."""
    result = {}
    for key, value in pairs:
        if key in result:
            raise LedgerError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def reject_constant(value):
    """Reject JSON NaN and Infinity extensions."""
    raise LedgerError(f"non-finite JSON number: {value}")


def check_finite(value):
    """Reject exponent overflow inside snapshots as well as monetary fields."""
    if isinstance(value, float) and not math.isfinite(value):
        raise LedgerError("non-finite JSON number")
    if isinstance(value, dict):
        for child in value.values():
            check_finite(child)
    elif isinstance(value, list):
        for child in value:
            check_finite(child)


def parse_json(raw):
    """Decode strict JSON without duplicate keys or non-finite numbers."""
    try:
        value = json.loads(raw, object_pairs_hook=unique_object,
                           parse_constant=reject_constant)
    except (json.JSONDecodeError, UnicodeError) as error:
        raise LedgerError(str(error)) from error
    check_finite(value)
    return value


def read_json(path):
    """Read a local JSON object; callers validate its contract."""
    return parse_json(Path(path).read_text(encoding="utf-8"))


def parse_jsonl(raw):
    """Read one object per line, preserving line numbers on rejected input."""
    records = []
    for number, line in enumerate(raw.splitlines(), 1):
        try:
            value = parse_json(line)
            if not isinstance(value, dict):
                raise LedgerError("JSONL records must be objects")
            records.append(value)
        except LedgerError as error:
            raise LedgerError(f"line {number}: {error}") from error
    return records


def fields(value, required, optional=()):
    """Require the declared object shape so misspelled accounting fields fail."""
    if not isinstance(value, dict):
        raise LedgerError("expected an object")
    missing = set(required) - value.keys()
    extra = value.keys() - set(required) - set(optional)
    if missing or extra:
        raise LedgerError(f"missing fields: {sorted(missing)}; unknown fields: {sorted(extra)}")


def identifier(value, name):
    """Validate a stable local evidence identifier."""
    if not isinstance(value, str) or not value.strip() or any(ord(c) < 32 for c in value):
        raise LedgerError(f"{name} must be a nonempty string without control characters")
    return value


def money(value, name):
    """Parse finite nonnegative USD amounts; decimal strings preserve precision."""
    if isinstance(value, bool) or not isinstance(value, (str, int, float, Decimal)):
        raise LedgerError(f"{name} must be a nonnegative USD amount")
    try:
        amount = Decimal(str(value))
    except InvalidOperation as error:
        raise LedgerError(f"invalid {name}") from error
    if not amount.is_finite() or amount < 0:
        raise LedgerError(f"{name} must be finite and nonnegative")
    return amount


def usd(amount):
    """Emit decimal strings instead of rounding monetary totals through float."""
    if amount == 0:
        return "0"
    text = format(amount, "f")
    return text.rstrip("0").rstrip(".") if "." in text else text


def snapshot_digest(snapshot):
    """Hash the common comparison configuration, independent of object order."""
    if not isinstance(snapshot, dict) or not snapshot:
        raise LedgerError("comparison_snapshot must be a nonempty object")
    check_finite(snapshot)
    raw = json.dumps(snapshot, sort_keys=True, separators=(",", ":"), allow_nan=False)
    return hashlib.sha256(raw.encode()).hexdigest()


def pair_key(value):
    """Identify one planned case/client pair within a frozen cohort."""
    return tuple(value[name] for name in ("cohort_id", "case_id", "client"))


def validate_run(record):
    """Normalize an immutable run without inventing missing cost evidence."""
    fields(record, RUN_REQUIRED, RUN_OPTIONAL)
    if type(record["schema_version"]) is not int or record["schema_version"] != 1:
        raise LedgerError("unsupported run schema_version")
    result = dict(record)
    for key in ("record_id", "cohort_id", "case_id"):
        identifier(result[key], key)
    if result["client"] not in CLIENTS or result["arm"] not in ARMS:
        raise LedgerError("client must be omo/omp and arm must be lead/fusion")
    snapshot_digest(result["comparison_snapshot"])
    if not isinstance(result["outcome"], str) or result["outcome"] not in OUTCOMES:
        raise LedgerError("invalid outcome")
    if type(result["completed"]) is not bool or not (
        result["quality_passed"] is None or type(result["quality_passed"]) is bool
    ):
        raise LedgerError("completed must be boolean; quality_passed must be boolean or null")
    if result["outcome"] == "completed" and not result["completed"]:
        raise LedgerError("completed outcome requires completed=true")
    if result["cost_scope"] not in ("full_task", "partial"):
        raise LedgerError("cost_scope must be full_task or partial")
    confirmed = result.get("confirmed_usd")
    result["confirmed_usd"] = None if confirmed is None else usd(money(confirmed, "confirmed_usd"))
    for key in ("reserved_unknown_usd", "inflight_reserve_usd"):
        result[key] = usd(money(result[key], key))
    if (result["outcome"] == "unknown" and result["confirmed_usd"] is not None
            and money(result["reserved_unknown_usd"], "reserved_unknown_usd") == 0):
        raise LedgerError("unknown outcome needs a positive unknown reserve or missing confirmed cost")
    if "wall_seconds" in result:
        seconds = result["wall_seconds"]
        if isinstance(seconds, bool) or not isinstance(seconds, (int, float)) or seconds < 0 or not math.isfinite(seconds):
            raise LedgerError("wall_seconds must be finite and nonnegative")
    if "failure_reason" in result:
        identifier(result["failure_reason"], "failure_reason")
    return result


def is_success(run):
    """Require both completed execution and independent quality evidence."""
    return run["outcome"] == "completed" and run["completed"] and run["quality_passed"] is True


def checked_runs(records):
    """Reject duplicate evidence and mismatched configurations within a pair."""
    runs, ids, slots, snapshots = [], set(), set(), {}
    for record in records:
        run = validate_run(record)
        slot = pair_key(run) + (run["arm"],)
        if run["record_id"] in ids or slot in slots:
            raise LedgerError(f"duplicate record or case/client/arm: {slot}")
        digest = snapshot_digest(run["comparison_snapshot"])
        previous = snapshots.setdefault(pair_key(run), digest)
        if previous != digest:
            raise LedgerError(f"snapshot mismatch within pair: {pair_key(run)}")
        ids.add(run["record_id"])
        slots.add(slot)
        runs.append(run)
    return runs


def group_metrics(runs):
    """Keep failures in denominators and unknown liabilities outside free cost."""
    success = sum(is_success(run) for run in runs)
    confirmed = sum((money(r["confirmed_usd"], "confirmed_usd") for r in runs if r["confirmed_usd"] is not None), Decimal(0))
    unknown = sum((money(r["reserved_unknown_usd"], "reserved_unknown_usd") for r in runs), Decimal(0))
    inflight = sum((money(r["inflight_reserve_usd"], "inflight_reserve_usd") for r in runs), Decimal(0))
    missing = sum(r["confirmed_usd"] is None for r in runs)
    partial = sum(r["cost_scope"] != "full_task" for r in runs)
    costs_observed = missing == 0 and partial == 0
    return {
        "assigned_runs": len(runs), "completed_runs": sum(r["completed"] for r in runs),
        "successful_runs": success, "unsuccessful_runs": len(runs) - success,
        "success_rate": success / len(runs) if runs else None,
        "outcomes": dict(sorted(Counter(r["outcome"] for r in runs).items())),
        "confirmed_usd": usd(confirmed), "reserved_unknown_usd": usd(unknown),
        "inflight_reserve_usd": usd(inflight), "missing_confirmed_cost_runs": missing,
        "partial_cost_scope_runs": partial,
        "costs_final": costs_observed and unknown == 0 and inflight == 0,
        "cost_per_success": usd(confirmed / success) if success and costs_observed else None,
        "cost_per_success_is_lower_bound": bool(success and costs_observed and (unknown > 0 or inflight > 0)),
    }


def summarize(records, manifest=None):
    """Summarize each arm separately and expose incomplete pairs explicitly."""
    runs = checked_runs(records)
    pairs, grouped = {}, {}
    for run in runs:
        pairs.setdefault(pair_key(run), {})[run["arm"]] = run
        key = (run["cohort_id"], run["client"], run["arm"], snapshot_digest(run["comparison_snapshot"]))
        grouped.setdefault(key, []).append(run)
    missing_records = []
    if manifest is not None:
        planned = {(manifest["cohort_id"], case["case_id"], case["client"]) for case in manifest["cases"]}
        if len(planned) != len(manifest["cases"]):
            raise LedgerError("manifest contains duplicate case/client pairs")
        if set(pairs) - planned:
            raise LedgerError("run is outside the supplied manifest")
        for key in sorted(planned):
            present = pairs.get(key, {})
            for arm in ARMS:
                if arm not in present:
                    missing_records.append(dict(zip(("cohort_id", "case_id", "client"), key), arm=arm))
    complete, incomplete = [], []
    for key, arms in sorted(pairs.items()):
        item = dict(zip(("cohort_id", "case_id", "client"), key))
        item["record_ids"] = {arm: run["record_id"] for arm, run in sorted(arms.items())}
        if len(arms) == 2:
            item["snapshot_sha256"] = snapshot_digest(arms["lead"]["comparison_snapshot"])
            item["successful_arms"] = [arm for arm in ARMS if is_success(arms[arm])]
            complete.append(item)
        else:
            item["missing_arms"] = [arm for arm in ARMS if arm not in arms]
            incomplete.append(item)
    return {
        "schema_version": 1, "observed": group_metrics(runs),
        "groups": [dict(zip(("cohort_id", "client", "arm", "snapshot_sha256"), key), **group_metrics(value)) for key, value in sorted(grouped.items())],
        "complete_pairs": complete, "incomplete_pairs": incomplete,
        "paired_comparison": {
            "case_count": len(complete),
            "lead_successful_cases": sum("lead" in pair["successful_arms"] for pair in complete),
            "fusion_successful_cases": sum("fusion" in pair["successful_arms"] for pair in complete),
            "lead_only_successes": sum(pair["successful_arms"] == ["lead"] for pair in complete),
            "fusion_only_successes": sum(pair["successful_arms"] == ["fusion"] for pair in complete),
        },
        "missing_run_records": missing_records,
        "record_coverage_complete": not missing_records if manifest is not None else None,
        "manifest_task_set_frozen": manifest.get("task_set_frozen", False) if manifest else None,
        "run_records": runs,
        "notes": [
            "Arm groups retain all executed failures, including incomplete pairs; complete_pairs alone are paired comparisons.",
            "confirmed_usd sums observed fees only; missing/partial costs make cost_per_success null.",
            "Unknown/inflight reserves are liabilities, not free usage; known cost_per_success is a lower bound while they remain.",
            "The observed denominator uses supplied run records; missing manifest records need reconciliation before a full-cohort claim.",
            "A missing record does not prove a run was unstarted or free; interrupted launched runs need UNKNOWN evidence and held reserves.",
        ],
    }


def replay_budget(events):
    """Rebuild held pair allocations and immutable settlements from a journal."""
    if not events:
        return None, {}, []
    header = events[0]
    fields(header, {"event", "schema_version", "total_budget_usd"})
    if header["event"] != "budget" or type(header["schema_version"]) is not int or header["schema_version"] != 1:
        raise LedgerError("invalid budget journal header")
    total = money(header["total_budget_usd"], "total_budget_usd")
    reservations, records = {}, []
    for event in events[1:]:
        if event.get("event") == "reserve_pair":
            fields(event, {"event", "cohort_id", "case_id", "client", "comparison_snapshot", "caps_usd"})
            for name in ("cohort_id", "case_id"):
                identifier(event[name], name)
            if event["client"] not in CLIENTS or pair_key(event) in reservations:
                raise LedgerError("invalid or duplicate reserved pair")
            snapshot_digest(event["comparison_snapshot"])
            fields(event["caps_usd"], set(ARMS))
            if any(money(event["caps_usd"][arm], arm) <= 0 for arm in ARMS):
                raise LedgerError("both arm allocations must be positive")
            held = Decimal(budget_metrics(total, reservations, records)["allocated_usd"])
            required = sum((money(event["caps_usd"][arm], arm) for arm in ARMS), Decimal(0))
            if held + required > total:
                raise LedgerError("journal reserves a pair beyond its remaining budget")
            reservations[pair_key(event)] = event
        elif event.get("event") == "settle_run":
            fields(event, {"event", "run"})
            run = validate_run(event["run"])
            reservation = reservations.get(pair_key(run))
            if reservation is None or snapshot_digest(run["comparison_snapshot"]) != snapshot_digest(reservation["comparison_snapshot"]):
                raise LedgerError("settlement has no matching frozen pair allocation")
            if run["confirmed_usd"] is None or run["cost_scope"] != "full_task":
                raise LedgerError("missing or partial costs cannot release a pair allocation")
            records.append(run)
            checked_runs(records)
        else:
            raise LedgerError("invalid budget journal event")
    return total, reservations, records


def budget_metrics(total, reservations, runs):
    """Count all unstarted arms and unresolved fees against new admission."""
    metrics = group_metrics(runs)
    settled = {pair_key(run) + (run["arm"],) for run in runs}
    pending = sum((money(pair["caps_usd"][arm], arm) for key, pair in reservations.items() for arm in ARMS if key + (arm,) not in settled), Decimal(0))
    allocated = pending + sum((money(metrics[name], name) for name in ("confirmed_usd", "reserved_unknown_usd", "inflight_reserve_usd")), Decimal(0))
    return {
        "total_budget_usd": usd(total),
        **{name: metrics[name] for name in ("confirmed_usd", "reserved_unknown_usd", "inflight_reserve_usd")},
        "unsettled_pair_reserve_usd": usd(pending), "allocated_usd": usd(allocated),
        "remaining_usd": usd(total - allocated), "over_budget": allocated > total,
    }


@contextmanager
def locked_journal(path):
    """Serialize local Unix journal updates; this does not enforce remote spending."""
    try:
        import fcntl
    except ImportError as error:
        raise LedgerError("budget journal commands require Unix flock; summarize is portable") from error
    with open(path, "a+", encoding="utf-8") as stream:
        fcntl.flock(stream, fcntl.LOCK_EX)
        stream.seek(0)
        raw = stream.read()
        yield stream, raw, parse_jsonl(raw)


def append_events(stream, raw, events):
    """Append only validated events and flush their allocation before returning."""
    stream.seek(0, os.SEEK_END)
    prefix = "\n" if raw and not raw.endswith("\n") else ""
    stream.write(prefix + "".join(json.dumps(event, sort_keys=True, allow_nan=False) + "\n" for event in events))
    stream.flush()
    os.fsync(stream.fileno())


def reserve_pair(path, cohort_id, case_id, client, snapshot, lead_cap, fusion_cap, total_budget):
    """Reserve both arms atomically; an identical repeated request is idempotent."""
    event = {"event": "reserve_pair", "cohort_id": cohort_id, "case_id": case_id,
             "client": client, "comparison_snapshot": snapshot,
             "caps_usd": {"lead": usd(money(lead_cap, "lead_cap")), "fusion": usd(money(fusion_cap, "fusion_cap"))}}
    header = {"event": "budget", "schema_version": 1, "total_budget_usd": usd(money(total_budget, "total_budget_usd"))}
    for name in ("cohort_id", "case_id"):
        identifier(event[name], name)
    snapshot_digest(snapshot)
    if client not in CLIENTS or any(money(cap, arm) <= 0 for arm, cap in event["caps_usd"].items()):
        raise LedgerError("client must be omo/omp and both arm allocations must be positive")
    with locked_journal(path) as (stream, raw, events):
        total, reservations, runs = replay_budget(events or [header])
        if total != money(total_budget, "total_budget_usd"):
            raise LedgerError("journal total budget cannot change implicitly")
        key = pair_key(event)
        previous = reservations.get(key)
        if previous is not None:
            if previous != event or snapshot_digest(previous["comparison_snapshot"]) != snapshot_digest(snapshot):
                raise LedgerError("reserved pair configuration or caps changed")
            return {"admitted": True, "replayed": True, **budget_metrics(total, reservations, runs)}
        metrics = budget_metrics(total, reservations, runs)
        required = sum((money(cap, arm) for arm, cap in event["caps_usd"].items()), Decimal(0))
        if required > total - Decimal(metrics["allocated_usd"]):
            return {"admitted": False, "reason": "budget_exhausted", "required_pair_usd": usd(required), **metrics}
        next_events = ([] if events else [header]) + [event]
        replay_budget((events or [header]) + [event])
        append_events(stream, raw, next_events)
        reservations[key] = event
        return {"admitted": True, "replayed": False, **budget_metrics(total, reservations, runs)}


def settle_run(path, record):
    """Record confirmed costs once; retain unknown fees and unstarted sibling caps."""
    run = validate_run(record)
    event = {"event": "settle_run", "run": run}
    with locked_journal(path) as (stream, raw, events):
        if not events:
            raise LedgerError("reserve the pair before settling a run")
        total, reservations, runs = replay_budget(events + [event])
        append_events(stream, raw, [event])
        return budget_metrics(total, reservations, runs)


def main(argv=None):
    """Run local reporting or paired admission without contacting any API."""
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest="command", required=True)
    summary = commands.add_parser("summarize", help="summarize immutable run JSONL")
    summary.add_argument("results")
    summary.add_argument("--manifest")
    reserve = commands.add_parser("reserve-pair", help="hold the complete A/B pair before either run")
    reserve.add_argument("journal")
    for name in ("cohort-id", "case-id", "snapshot", "lead-cap-usd", "fusion-cap-usd", "total-budget-usd"):
        reserve.add_argument("--" + name, required=True)
    reserve.add_argument("--client", choices=CLIENTS, required=True)
    settle = commands.add_parser("settle-run", help="settle a local allocation from one JSON result")
    settle.add_argument("journal")
    settle.add_argument("--record", required=True)
    status = commands.add_parser("budget-status", help="inspect confirmed fees and remaining held allocations")
    status.add_argument("journal")
    args = parser.parse_args(argv)
    try:
        if args.command == "summarize":
            records = parse_jsonl(Path(args.results).read_text(encoding="utf-8"))
            result = summarize(records, read_json(args.manifest) if args.manifest else None)
        elif args.command == "reserve-pair":
            result = reserve_pair(args.journal, args.cohort_id, args.case_id, args.client,
                                  read_json(args.snapshot), args.lead_cap_usd,
                                  args.fusion_cap_usd, args.total_budget_usd)
        elif args.command == "settle-run":
            result = settle_run(args.journal, read_json(args.record))
        else:
            total, reservations, runs = replay_budget(parse_jsonl(Path(args.journal).read_text(encoding="utf-8")))
            if total is None:
                raise LedgerError("empty budget journal")
            result = budget_metrics(total, reservations, runs)
        print(json.dumps(result, indent=2, sort_keys=True, allow_nan=False))
        return 3 if result.get("admitted") is False else 0
    except (LedgerError, OSError, UnicodeError, KeyError, TypeError) as error:
        print(f"ledger: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
