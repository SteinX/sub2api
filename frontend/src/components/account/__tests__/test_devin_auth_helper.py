import importlib.util
import io
import json
import threading
import time
import unittest
import urllib.request
from http.server import HTTPServer
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("devin_helper", Path(__file__).parents[1] / "devin-auth-helper.py")
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)


class DevinHelperTest(unittest.TestCase):
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
