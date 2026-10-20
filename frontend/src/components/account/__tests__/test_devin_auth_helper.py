import importlib.util
import io
import json
import threading
import time
import unittest
import urllib.error
import urllib.request
from http.server import HTTPServer
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("devin_helper", Path(__file__).parents[1] / "devin-auth-helper.py")
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)


class DevinHelperTest(unittest.TestCase):
    def test_retries_transient_callback_deliveries(self):
        config = {"callback_url": "https://sub2api.example/callback"}
        payload = b'{"code":"one-shot","state":"nonce","pairing_ticket":"ephemeral"}'
        failures = [urllib.error.URLError("temporary connection failure")]
        failures.extend(urllib.error.HTTPError(config["callback_url"], status, "temporary", {}, io.BytesIO()) for status in (408, 429, 503))
        failures.append(urllib.error.HTTPError(config["callback_url"], 409, "busy", {}, io.BytesIO(b'{"reason":"DEVIN_AUTH_BUSY"}')))
        for failure in failures:
            with self.subTest(failure=failure):
                response = io.BytesIO(b'{"data":{"status":"ready"}}')
                with patch.object(helper.urllib.request, "urlopen", side_effect=[failure, response]) as send, patch.object(helper.time, "sleep"):
                    self.assertTrue(helper.deliver_callback(config, payload, time.time() + 10))
                self.assertEqual(send.call_count, 2)
                self.assertTrue(all(call.args[0].data == payload for call in send.call_args_list))

    def test_does_not_retry_permanent_callback_failure(self):
        config = {"callback_url": "https://sub2api.example/callback"}
        failure = urllib.error.HTTPError(config["callback_url"], 400, "invalid ticket", {}, io.BytesIO())
        with patch.object(helper.urllib.request, "urlopen", side_effect=failure) as send, patch.object(helper.time, "sleep") as sleep:
            self.assertFalse(helper.deliver_callback(config, b'{}', time.time() + 10))
        self.assertEqual(send.call_count, 1)
        sleep.assert_not_called()

    def test_does_not_retry_unrelated_conflicts(self):
        config = {"callback_url": "https://sub2api.example/callback"}
        failure = urllib.error.HTTPError(config["callback_url"], 409, "conflict", {}, io.BytesIO(b'{"reason":"OTHER_CONFLICT"}'))
        with patch.object(helper.urllib.request, "urlopen", side_effect=failure) as send, patch.object(helper.time, "sleep") as sleep:
            self.assertFalse(helper.deliver_callback(config, b'{}', time.time() + 10))
        self.assertEqual(send.call_count, 1)
        sleep.assert_not_called()

    def test_callback_retries_are_bounded_by_the_session_deadline(self):
        config = {"callback_url": "https://sub2api.example/callback"}
        clock = [0]
        def advance(seconds):
            clock[0] += seconds
        with patch.object(helper.time, "time", side_effect=lambda: clock[0]), patch.object(helper.time, "sleep", side_effect=advance), patch.object(helper.urllib.request, "urlopen", side_effect=urllib.error.URLError("offline")) as send:
            self.assertFalse(helper.deliver_callback(config, b'{}', 3))
        self.assertEqual(send.call_count, 2)
        self.assertEqual(clock[0], 3)

    def test_browser_callback_returns_only_one_shot_credentials(self):
        servers = []
        received = []
        threads = []
        original_urlopen = urllib.request.urlopen

        def server_factory(address, handler):
            self.assertEqual(address, ("127.0.0.1", 59653))
            server = HTTPServer(("127.0.0.1", 0), handler)
            servers.append(server)
            return server

        def send_browser_callback(_url):
            def send():
                with original_urlopen(f"http://127.0.0.1:{servers[0].server_port}/callback?state=nonce&code=one-shot", timeout=3) as response:
                    self.assertEqual(response.status, 200)
            thread = threading.Thread(target=send)
            threads.append(thread)
            thread.start()

        def send_to_sub2api(request, **_kwargs):
            self.assertEqual(request.full_url, "https://sub2api.example/api/v1/devin/auth-sessions/id/callback")
            received.append(json.loads(request.data))
            return io.BytesIO(b'{"data":{"status":"ready"}}')

        config = {"auth_url": "https://app.devin.ai/auth/cli/continue?state=nonce",
                  "callback_url": "https://sub2api.example/api/v1/devin/auth-sessions/id/callback",
                  "pairing_ticket": "ephemeral", "expires_at": time.time() + 5}
        with patch.object(helper, "HTTPServer", side_effect=server_factory), patch.object(helper.webbrowser, "open", side_effect=send_browser_callback), patch.object(helper.urllib.request, "urlopen", side_effect=send_to_sub2api):
            helper.run(config)
        for thread in threads:
            thread.join(timeout=3)
        self.assertEqual(received, [{"code": "one-shot", "state": "nonce", "pairing_ticket": "ephemeral"}])

    def test_rejects_plain_http_remote_callback(self):
        with self.assertRaises(ValueError):
            helper.run({"callback_url": "http://remote.example/callback"})


if __name__ == "__main__":
    unittest.main()
