"""Read-only Sub2API billing snapshots for a dedicated local smoke-test key.

The authoritative settled delta and best-effort usage rows are different pieces
of evidence. Neither endpoint exposes unknown/inflight liabilities or proves
that late billing has finished, so comparisons always retain partial scope.
Only call this module on the user's chosen deployment. It never discovers keys,
loads saved credentials, sends model requests, or changes a key's limits.
"""

from datetime import datetime, timezone
from decimal import Decimal, InvalidOperation
import hashlib
import hmac
import json
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, urlsplit, urlunsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener


class MeteringError(ValueError):
    """The endpoint cannot provide a trustworthy billing observation."""


PAGE_SIZE = 1000
MAX_PAGES = 100
MAX_RESPONSE_BYTES = 8 * 1024 * 1024
MATCH_TOLERANCE_USD = Decimal("0.000001")


def _usd(value):
    if isinstance(value, bool) or not isinstance(value, (int, float, str, Decimal)):
        raise MeteringError("billing amounts must be finite nonnegative numbers")
    try:
        number = Decimal(str(value))
    except InvalidOperation as error:
        raise MeteringError("invalid billing amount") from error
    if not number.is_finite() or number < 0:
        raise MeteringError("billing amounts must be finite and nonnegative")
    return number


def _text(number):
    return "0" if number == 0 else format(number.normalize(), "f")


def _integer(value, minimum=0):
    if type(value) is not int or value < minimum:
        raise MeteringError("invalid integer in billing response")
    return value


def _fingerprint(value):
    try:
        return hashlib.sha256(value.encode("utf-8")).hexdigest()
    except UnicodeError:
        raise MeteringError("invalid Unicode identity in billing response") from None


def _panel_root(base_url):
    if not isinstance(base_url, str):
        raise MeteringError("a deployment base URL is required")
    try:
        parsed = urlsplit(base_url)
        _ = parsed.port  # Reject malformed ports before any request.
    except ValueError as error:
        raise MeteringError("invalid deployment URL") from error
    if (not parsed.hostname or parsed.username is not None or parsed.password is not None
            or parsed.query or parsed.fragment):
        raise MeteringError("deployment URL must not include credentials, query or fragment")
    loopback = parsed.hostname.lower() in {"localhost", "127.0.0.1", "::1"}
    if parsed.scheme != "https" and not (parsed.scheme == "http" and loopback):
        raise MeteringError("use HTTPS for a remote deployment or HTTP on loopback")
    path = parsed.path.rstrip("/")
    if path.endswith("/v1") and not path.endswith("/api/v1"):
        path = path[:-3]
    if not path.endswith("/api/v1"):
        path += "/api/v1"
    # Preserve deployment prefixes; no credentials or auth-bearing query is kept.
    return urlunsplit((parsed.scheme, parsed.netloc.lower(), path, "", ""))


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise MeteringError("billing endpoint redirects are refused")


def _strict_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise MeteringError("duplicate JSON fields in billing response")
        result[key] = value
    return result


def _nonfinite(_):
    raise MeteringError("nonfinite value in billing response")


def _get_json(url, headers):
    request = Request(url, headers=headers, method="GET")
    try:
        with build_opener(_NoRedirect()).open(request, timeout=15) as response:
            raw = response.read(MAX_RESPONSE_BYTES + 1)
        if len(raw) > MAX_RESPONSE_BYTES:
            raise MeteringError("billing response exceeds the size limit")
        return json.loads(raw, parse_float=Decimal, parse_constant=_nonfinite,
                          object_pairs_hook=_strict_object)
    except HTTPError as error:
        raise MeteringError(f"billing HTTP request failed with status {error.code}") from None
    except (URLError, TimeoutError, OSError) as error:
        raise MeteringError("billing endpoint is unavailable") from None
    except (UnicodeError, json.JSONDecodeError) as error:
        raise MeteringError("invalid JSON billing response") from None


def _data(envelope):
    if (not isinstance(envelope, dict) or type(envelope.get("code")) is not int
            or envelope["code"] != 0 or not isinstance(envelope.get("data"), dict)):
        # Do not surface an arbitrary server message or raw response.
        raise MeteringError("billing endpoint returned an unsuccessful envelope")
    return envelope["data"]


def _key(data, api_key_id):
    if _integer(data.get("id"), 1) != api_key_id:
        raise MeteringError("billing endpoint returned a different API key")
    quota = _usd(data.get("quota"))
    if quota <= 0:
        raise MeteringError("metering requires a dedicated key with finite positive quota")
    status = data.get("status")
    if not isinstance(status, str) or status not in {"active", "disabled", "quota_exhausted", "expired"}:
        raise MeteringError("invalid API key status")
    raw_key = data.get("key")
    raw_group = data.get("group")
    if raw_group is not None and not isinstance(raw_group, dict):
        raise MeteringError("invalid API key group metadata")
    subscription_type = raw_group.get("subscription_type") if raw_group else None
    if subscription_type is not None and (not isinstance(subscription_type, str)
                                          or subscription_type not in {"standard", "subscription"}):
        raise MeteringError("invalid API key group subscription type")
    group_id = data.get("group_id")
    if group_id is not None:
        _integer(group_id, 1)
    return {
        "id": api_key_id,
        "key_fingerprint": _fingerprint(raw_key) if isinstance(raw_key, str) and raw_key else None,
        "group_id": group_id,
        "group_subscription_type": subscription_type,
        "quota_usd": _text(quota),
        "quota_used_usd": _text(_usd(data.get("quota_used"))),
        "status": status,
        "current_concurrency": _integer(data.get("current_concurrency")),
    }


def _row(data, api_key_id):
    if not isinstance(data, dict) or _integer(data.get("api_key_id"), 1) != api_key_id:
        raise MeteringError("usage row belongs to another API key")
    identifier = _integer(data.get("id"), 1)
    request = data.get("request_id")
    if request is not None and not isinstance(request, str):
        raise MeteringError("invalid usage request identity")
    session = data.get("session_id")
    if session is not None and not isinstance(session, str):
        raise MeteringError("invalid usage session identity")
    return {
        "id": identifier,
        "request_fingerprint": _fingerprint(request) if request else None,
        "session_fingerprint": _fingerprint(session) if session else None,
        "request_kind": "fusion" if request and request.startswith("fusion:") else "other",
        "actual_cost_usd": _text(_usd(data.get("actual_cost"))),
        "total_cost_usd": _text(_usd(data.get("total_cost"))),
    }


def _verify_key(data, expected_key):
    if expected_key is None:
        return
    supplied = data.get("key")
    if not isinstance(supplied, str) or not supplied:
        raise MeteringError("key endpoint omitted the credential needed to verify metering scope")
    try:
        matches = hmac.compare_digest(supplied.encode("utf-8"), expected_key.encode("utf-8"))
    except UnicodeError:
        raise MeteringError("invalid credential encoding") from None
    if not matches:
        raise MeteringError("numeric API key ID does not match the model credential")


def capture_snapshot(base_url, api_key_id, bearer_token, transport=None, expected_key=None):
    """Capture read-only evidence using an explicit user JWT.

    ``base_url`` is the deployment root (an optional path prefix is supported),
    its /v1 model root, or its /api/v1 panel root. ``transport(url, headers)`` may return a decoded
    JSON envelope for local tests. The default transport preserves system TLS
    trust/proxies, refuses redirects and makes only bounded GET requests.
    Pass ``expected_key`` from the explicitly provided model API key to verify
    its equality in memory before collecting usage. Missing/mismatched returned
    credentials fail closed when this verification is requested.

    The returned object is safe to serialize: credentials, raw responses,
    request IDs, session IDs and unrelated user/account fields are discarded.
    """
    _integer(api_key_id, 1)
    if (not isinstance(bearer_token, str) or not bearer_token
            or any(ord(char) < 32 or ord(char) == 127 for char in bearer_token)):
        raise MeteringError("provide an explicit nonempty user JWT without control characters")
    if expected_key is not None and (not isinstance(expected_key, str) or not expected_key):
        raise MeteringError("expected model credential must be a nonempty string")
    root = _panel_root(base_url)
    get = transport or _get_json
    headers = {"Authorization": "Bearer " + bearer_token, "Accept": "application/json"}
    key_url = f"{root}/keys/{api_key_id}"
    before_data = _data(get(key_url, dict(headers)))
    _verify_key(before_data, expected_key)
    before = _key(before_data, api_key_id)
    rows, reasons, totals, duplicate_ids = {}, [], set(), 0
    page, pages_to_read = 1, 1
    while page <= pages_to_read and page <= MAX_PAGES:
        query = urlencode({"api_key_id": api_key_id, "page": page, "page_size": PAGE_SIZE,
                           "sort_by": "id", "sort_order": "asc"})
        data = _data(get(f"{root}/usage?{query}", dict(headers)))
        total = _integer(data.get("total"))
        totals.add(total)
        if (_integer(data.get("page"), 1) != page
                or _integer(data.get("page_size"), 1) != PAGE_SIZE):
            raise MeteringError("usage endpoint ignored requested pagination")
        declared_pages = _integer(data.get("pages"), 1)
        expected_pages = max(1, (total + PAGE_SIZE - 1) // PAGE_SIZE)
        if declared_pages != expected_pages:
            reasons.append("pagination_metadata_inconsistent")
        pages_to_read = max(pages_to_read, declared_pages, expected_pages)
        items = data.get("items")
        if not isinstance(items, list) or len(items) > PAGE_SIZE:
            raise MeteringError("invalid usage page items")
        for item in items:
            row = _row(item, api_key_id)
            prior = rows.get(row["id"])
            if prior is not None:
                duplicate_ids += 1
                if prior != row:
                    reasons.append("usage_row_changed_during_capture")
            else:
                rows[row["id"]] = row
        page += 1
    after_data = _data(get(key_url, dict(headers)))
    _verify_key(after_data, expected_key)
    after = _key(after_data, api_key_id)
    if pages_to_read > MAX_PAGES:
        reasons.append("pagination_limit_reached")
    if len(totals) != 1:
        reasons.append("usage_total_changed_during_capture")
    if totals and len(rows) != max(totals):
        reasons.append("usage_row_count_mismatch")
    if duplicate_ids:
        reasons.append("duplicate_usage_ids_during_capture")
    if before != after:
        reasons.append("key_changed_during_capture")
    if before["current_concurrency"] or after["current_concurrency"]:
        reasons.append("active_requests_observed")
    return {
        "schema_version": 1,
        "captured_at": datetime.now(timezone.utc).isoformat(),
        "deployment_fingerprint": _fingerprint(root),
        "api_key": after,
        "usage_rows": sorted(rows.values(), key=lambda row: row["id"]),
        "duplicate_usage_ids": duplicate_ids,
        "observed_totals": sorted(totals),
        "capture_stable": not reasons,
        "capture_reasons": sorted(set(reasons)),
        "evidence_complete": False,
    }


def compare_snapshots(before, after):
    """Compare settled observations; never assert complete task accounting.

    ``verified_settled_delta_usd`` / ``confirmed_usd`` denote an observed
    nonnegative change in a finite-quota key's settled counter only. They are
    not the total liability of a task. Unknown and inflight amounts cannot be
    queried here and are returned as null, never zero.
    """
    for snapshot in (before, after):
        if not isinstance(snapshot, dict) or snapshot.get("schema_version") != 1:
            raise MeteringError("unsupported billing snapshot")
    if (before.get("deployment_fingerprint") != after.get("deployment_fingerprint")
            or before["api_key"]["id"] != after["api_key"]["id"]
            or before["api_key"].get("key_fingerprint") != after["api_key"].get("key_fingerprint")):
        raise MeteringError("snapshots must belong to the same deployment and API key")
    reasons = [*before.get("capture_reasons", []), *after.get("capture_reasons", [])]
    before_key, after_key = before["api_key"], after["api_key"]
    start, finish = _usd(before_key["quota_used_usd"]), _usd(after_key["quota_used_usd"])
    quota = _usd(before_key["quota_usd"])
    valid_delta = quota > 0 and quota == _usd(after_key["quota_usd"]) and finish >= start
    if not valid_delta:
        reasons.append("quota_changed_or_counter_reset")
    delta = finish - start if valid_delta else None
    old_rows = {row["id"]: row for row in before["usage_rows"]}
    old_requests = {row["request_fingerprint"] for row in old_rows.values()
                    if row["request_fingerprint"] is not None}
    unique_requests, duplicate_requests, missing_request_ids, new_rows = {}, 0, 0, 0
    conflict = False
    for row in after["usage_rows"]:
        prior = old_rows.get(row["id"])
        if prior is not None:
            if row != prior:
                reasons.append("previous_usage_row_changed")
            continue
        new_rows += 1
        request = row["request_fingerprint"]
        if request is None:
            missing_request_ids += 1
            request = f"row:{row['id']}"
        if request in old_requests:
            duplicate_requests += 1
            continue
        costs = (_usd(row["actual_cost_usd"]), _usd(row["total_cost_usd"]))
        if request in unique_requests:
            duplicate_requests += 1
            if unique_requests[request] != costs:
                conflict = True
        else:
            unique_requests[request] = costs
    if set(old_rows) - {row["id"] for row in after["usage_rows"]}:
        reasons.append("previous_usage_rows_missing")
    if conflict:
        reasons.append("duplicate_request_cost_conflict")
    if missing_request_ids:
        reasons.append("usage_request_identity_missing")
    usage_sum = None if conflict else sum((cost[0] for cost in unique_requests.values()), Decimal(0))
    total_sum = None if conflict else sum((cost[1] for cost in unique_requests.values()), Decimal(0))
    matches = (None if delta is None or usage_sum is None else
               abs(delta - usage_sum) <= MATCH_TOLERANCE_USD)
    if matches is False:
        reasons.append("settled_counter_and_usage_sum_differ")
    if delta == 0 and new_rows == 0:
        reasons.append("no_new_settled_or_usage_evidence")
    reasons.extend(["unknown_and_inflight_reserves_not_exposed",
                    "usage_logs_are_best_effort_and_late_billing_is_possible"])
    return {
        "schema_version": 1,
        "api_key_id": before_key["id"],
        "confirmed_usd": _text(delta) if delta is not None else None,
        "verified_settled_delta_usd": _text(delta) if delta is not None else None,
        "usage_sum_usd": _text(usage_sum) if usage_sum is not None else None,
        "usage_total_cost_sum_usd": _text(total_sum) if total_sum is not None else None,
        "usage_matches_settled_delta": matches,
        "usage_new_row_count": new_rows,
        "usage_unique_request_count": len(unique_requests),
        "usage_duplicate_request_count": duplicate_requests,
        "usage_missing_request_id_count": missing_request_ids,
        "capture_stable": before.get("capture_stable") is True and after.get("capture_stable") is True,
        "cost_scope": "partial",
        "evidence_complete": False,
        "reserved_unknown_usd": None,
        "inflight_reserve_usd": None,
        "requires_retained_local_reserve": True,
        "reasons": sorted(set(reasons)),
    }


# Short alias for callers that use the originally proposed API name.
compare = compare_snapshots
