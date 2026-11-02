"""Read-only billing capture tests; no live API or credentials are used."""

from copy import deepcopy
import json
from io import BytesIO
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit
from urllib.error import HTTPError

import metering


def key(used="0", **changes):
    result = {"id": 7, "quota": "2", "quota_used": used, "status": "active",
              "current_concurrency": 0, "key": "private-api-key", "name": "private-key-name"}
    result.update(changes)
    return result


def row(identifier, cost, request=None, **changes):
    result = {"id": identifier, "api_key_id": 7, "request_id": request or f"fusion:{identifier}",
              "session_id": "private-runtime-session", "actual_cost": cost,
              "total_cost": cost, "ip_address": "private-client-ip"}
    result.update(changes)
    return result


class Transport:
    def __init__(self, keys, pages):
        self.keys = iter(keys)
        self.pages = pages
        self.calls = []

    def __call__(self, url, headers):
        self.calls.append((url, headers))
        parsed = urlsplit(url)
        if parsed.path.endswith("/keys/7"):
            return {"code": 0, "data": next(self.keys)}
        query = parse_qs(parsed.query)
        assert query["api_key_id"] == ["7"]
        assert query["sort_by"] == ["id"]
        assert query["sort_order"] == ["asc"]
        page = int(query["page"][0])
        return {"code": 0, "data": deepcopy(self.pages[page])}


def page(rows, total=None, number=1, pages=1):
    return {"items": rows, "total": len(rows) if total is None else total,
            "page": number, "page_size": 1000, "pages": pages}


def snapshot(used="0", rows=(), **key_changes):
    transport = Transport([key(used, **key_changes), key(used, **key_changes)],
                          {1: page(list(rows))})
    return metering.capture_snapshot("https://gateway.example", 7, "private-jwt", transport)


class CaptureTests(unittest.TestCase):
    def test_capture_drops_credentials_and_private_values(self):
        grouped = key("0.25", group_id=9,
                      group={"id": 9, "subscription_type": "standard", "name": "private-group-name"})
        transport = Transport([grouped, grouped], {1: page([row(1, "0.25")])})
        result = metering.capture_snapshot("https://gateway.example/prefix/api/v1/", 7,
                                          "private-jwt", transport)
        rendered = json.dumps(result)
        for private in ("private-jwt", "private-api-key", "private-runtime-session",
                        "private-client-ip", "private-key-name", "private-group-name", "fusion:1"):
            self.assertNotIn(private, rendered)
        self.assertEqual(result["api_key"]["quota_used_usd"], "0.25")
        self.assertEqual(result["api_key"]["key_fingerprint"],
                         metering._fingerprint("private-api-key"))
        self.assertEqual(result["api_key"]["group_id"], 9)
        self.assertEqual(result["api_key"]["group_subscription_type"], "standard")
        self.assertTrue(result["capture_stable"])
        self.assertFalse(result["evidence_complete"])
        self.assertTrue(all(url.startswith("https://gateway.example/prefix/api/v1/")
                            for url, _ in transport.calls))
        self.assertTrue(all(headers["Authorization"] == "Bearer private-jwt"
                            for _, headers in transport.calls))

    def test_ascending_pagination_and_changing_totals_are_reported(self):
        first = [row(index, "0.001") for index in range(1, 1001)]
        transport = Transport([key("1"), key("1.002")], {
            1: page(first, total=1001, pages=2),
            2: page([row(1000, "0.001"), row(1001, "0.001")], total=1002, number=2, pages=2),
        })
        result = metering.capture_snapshot("http://127.0.0.1:8000", 7, "jwt", transport)
        self.assertFalse(result["capture_stable"])
        self.assertEqual(result["duplicate_usage_ids"], 1)
        self.assertIn("usage_total_changed_during_capture", result["capture_reasons"])
        self.assertIn("key_changed_during_capture", result["capture_reasons"])
        self.assertIn("usage_row_count_mismatch", result["capture_reasons"])

    def test_finite_quota_key_identity_and_pagination_are_enforced(self):
        for bad in [key(quota=0), key(id=8), key(quota_used="NaN"), key(current_concurrency=-1),
                    key(status=[]), key(group={"subscription_type": []})]:
            with self.subTest(bad=bad):
                transport = Transport([bad], {})
                with self.assertRaises(metering.MeteringError):
                    metering.capture_snapshot("https://gateway.example", 7, "jwt", transport)
        transport = Transport([key()], {1: dict(page([]), page_size=20)})
        with self.assertRaises(metering.MeteringError):
            metering.capture_snapshot("https://gateway.example", 7, "jwt", transport)

    def test_endpoint_and_token_rejections_happen_before_transport(self):
        for endpoint, token in [("http://remote.example", "jwt"),
                                ("https://user:pass@gateway.example", "jwt"),
                                ("https://gateway.example?token=private", "jwt"),
                                ("https://gateway.example", "jwt\r\nsecret")]:
            with self.subTest(endpoint=endpoint):
                with self.assertRaises(metering.MeteringError):
                    metering.capture_snapshot(endpoint, 7, token,
                                              lambda *_: self.fail("unexpected transport call"))

    def test_model_api_root_maps_to_the_same_deployment_panel_root(self):
        transport = Transport([key(), key()], {1: page([])})
        result = metering.capture_snapshot("https://gateway.example/prefix/v1", 7, "jwt",
                                          transport, expected_key="private-api-key")
        self.assertTrue(result["capture_stable"])
        self.assertTrue(all(url.startswith("https://gateway.example/prefix/api/v1/")
                            for url, _ in transport.calls))

    def test_conflicting_duplicate_rows_and_active_work_are_unstable(self):
        transport = Transport([key(current_concurrency=1), key(current_concurrency=1)],
                              {1: page([row(1, "0.1"), row(1, "0.2")])})
        result = metering.capture_snapshot("https://gateway.example", 7, "jwt", transport)
        self.assertFalse(result["capture_stable"])
        self.assertIn("usage_row_changed_during_capture", result["capture_reasons"])
        self.assertIn("active_requests_observed", result["capture_reasons"])

    def test_page_limit_is_partial_and_does_not_loop(self):
        transport = Transport([key(), key()], {1: page([], total=2000, pages=2)})
        with patch.object(metering, "MAX_PAGES", 1):
            result = metering.capture_snapshot("https://gateway.example", 7, "jwt", transport)
        self.assertIn("pagination_limit_reached", result["capture_reasons"])
        self.assertEqual(len(transport.calls), 3)

    def test_transport_errors_do_not_repeat_requests(self):
        calls = []
        def failure(url, headers):
            calls.append(url)
            raise metering.MeteringError("synthetic transport failure")
        with self.assertRaises(metering.MeteringError):
            metering.capture_snapshot("https://gateway.example", 7, "jwt", failure)
        self.assertEqual(len(calls), 1)

    def test_expected_model_key_is_verified_before_reading_usage(self):
        for raw_key in (None, "", "a-different-api-key"):
            transport = Transport([key(key=raw_key)], {})
            with self.assertRaises(metering.MeteringError):
                metering.capture_snapshot("https://gateway.example", 7, "jwt", transport,
                                          expected_key="private-api-key")
            self.assertEqual(len(transport.calls), 1)
        transport = Transport([key(), key()], {1: page([])})
        result = metering.capture_snapshot("https://gateway.example", 7, "jwt", transport,
                                          expected_key="private-api-key")
        self.assertEqual(result["api_key"]["key_fingerprint"],
                         metering._fingerprint("private-api-key"))

    def test_redirects_are_refused_and_http_error_bodies_are_not_exposed(self):
        with self.assertRaises(metering.MeteringError):
            metering._NoRedirect().redirect_request(None, None, 302, "redirect", {},
                                                    "https://another.example")
        error = HTTPError("https://private.example/path", 401, "private-server-message",
                          {}, BytesIO(b"private-api-key private-jwt"))
        with patch.object(metering, "build_opener") as opener:
            opener.return_value.open.side_effect = error
            with self.assertRaises(metering.MeteringError) as caught:
                metering._get_json("https://gateway.example", {"Authorization": "Bearer jwt"})
        self.assertEqual(str(caught.exception), "billing HTTP request failed with status 401")
        self.assertEqual(error.fp.tell(), 0, "untrusted HTTP error bodies must not be read")


class ComparisonTests(unittest.TestCase):
    def test_matching_costs_still_require_partial_scope_and_unknown_reserve(self):
        result = metering.compare_snapshots(snapshot(), snapshot("0.2", [row(1, "0.2")]))
        self.assertEqual(result["verified_settled_delta_usd"], "0.2")
        self.assertEqual(result["confirmed_usd"], "0.2")
        self.assertEqual(result["usage_sum_usd"], "0.2")
        self.assertTrue(result["usage_matches_settled_delta"])
        self.assertFalse(result["evidence_complete"])
        self.assertEqual(result["cost_scope"], "partial")
        self.assertIsNone(result["reserved_unknown_usd"])
        self.assertIsNone(result["inflight_reserve_usd"])
        self.assertTrue(result["requires_retained_local_reserve"])

    def test_zero_counter_and_zero_usage_do_not_establish_free_completion(self):
        result = metering.compare(snapshot(), snapshot())
        self.assertEqual(result["confirmed_usd"], "0")
        self.assertFalse(result["evidence_complete"])
        self.assertIn("no_new_settled_or_usage_evidence", result["reasons"])

    def test_missing_usage_and_zero_cost_failure_rows_are_not_free(self):
        for rows in ([], [row(1, "0")]):
            result = metering.compare(snapshot(), snapshot("0.4", rows))
            self.assertEqual(result["confirmed_usd"], "0.4")
            self.assertFalse(result["usage_matches_settled_delta"])
            self.assertIn("settled_counter_and_usage_sum_differ", result["reasons"])
            self.assertFalse(result["evidence_complete"])

    def test_counter_reset_and_changed_quota_invalidate_delta(self):
        for after in (snapshot("0.1"), snapshot("0.4", quota="3")):
            result = metering.compare(snapshot("0.2"), after)
            self.assertIsNone(result["confirmed_usd"])
            self.assertIsNone(result["verified_settled_delta_usd"])
            self.assertIn("quota_changed_or_counter_reset", result["reasons"])

    def test_request_identity_dedup_excludes_prior_rows_and_duplicates(self):
        before = snapshot("0.1", [row(1, "0.1", "old-request")])
        after = snapshot("0.3", [row(1, "0.1", "old-request"), row(2, "0.1", "old-request"),
                                 row(3, "0.2", "new-request"), row(4, "0.2", "new-request")])
        result = metering.compare(before, after)
        self.assertEqual(result["usage_sum_usd"], "0.2")
        self.assertEqual(result["usage_unique_request_count"], 1)
        self.assertEqual(result["usage_duplicate_request_count"], 2)

    def test_conflicting_request_costs_and_missing_request_identity_are_explicit(self):
        after = snapshot("0.3", [row(1, "0.1", "same"), row(2, "0.2", "same"),
                                 row(3, "0", request_id="")])
        result = metering.compare(snapshot(), after)
        self.assertIsNone(result["usage_sum_usd"])
        self.assertIn("duplicate_request_cost_conflict", result["reasons"])
        self.assertEqual(result["usage_missing_request_id_count"], 1)

    def test_different_deployment_or_key_is_rejected(self):
        before = snapshot()
        after = snapshot()
        for field in ("deployment", "key", "key_fingerprint"):
            changed = deepcopy(after)
            if field == "deployment":
                changed["deployment_fingerprint"] = "another-deployment"
            elif field == "key":
                changed["api_key"]["id"] = 8
            else:
                changed["api_key"]["key_fingerprint"] = "different-model-credential"
            with self.assertRaises(metering.MeteringError):
                metering.compare(before, changed)


if __name__ == "__main__":
    unittest.main()
