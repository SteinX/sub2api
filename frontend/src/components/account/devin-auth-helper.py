#!/usr/bin/env python3
"""One-shot Devin login helper. Only a short-lived pairing ticket is embedded."""
import json
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import webbrowser
from http.server import BaseHTTPRequestHandler, HTTPServer


def deliver_callback(config, payload, deadline):
    request = urllib.request.Request(config["callback_url"], data=payload,
                                     headers={"Content-Type": "application/json"})
    delay = 1
    while time.time() < deadline:
        try:
            # Keep system TLS verification and proxy configuration.
            with urllib.request.urlopen(request, timeout=min(75, deadline - time.time())) as response:
                result = json.load(response)
            if not isinstance(result, dict):
                return False
            data = result.get("data", result)
            return isinstance(data, dict) and data.get("status") in ("ready", "connected")
        except urllib.error.HTTPError as error:
            retryable = error.code in (408, 429) or 500 <= error.code < 600
            if error.code == 409:
                try:
                    detail = json.loads(error.read(16 * 1024))
                    retryable = isinstance(detail, dict) and detail.get("reason") == "DEVIN_AUTH_BUSY"
                except (OSError, ValueError):
                    retryable = False
            error.close()
            if not retryable:
                return False
        except (OSError, urllib.error.URLError):
            pass
        except ValueError:
            return False
        remaining = deadline - time.time()
        if remaining <= 0:
            break
        time.sleep(min(delay, remaining))
        delay = min(delay * 2, 5)
    return False


def run(config):
    target = urllib.parse.urlsplit(config["callback_url"])
    if target.scheme not in ("https", "http") or target.username or target.password:
        raise ValueError("Invalid sub2api server address")
    if target.scheme == "http" and target.hostname not in ("localhost", "127.0.0.1", "::1"):
        raise ValueError("Remote sub2api servers require HTTPS")
    auth = urllib.parse.urlsplit(config["auth_url"])
    if auth.scheme != "https" or auth.netloc != "app.devin.ai":
        raise ValueError("Unexpected Devin authorization host")
    expected = urllib.parse.parse_qs(auth.query).get("state", [""])[0]
    if not expected:
        raise ValueError("Missing authorization state")
    done = threading.Event()
    outcome = {"success": False}

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass  # Callback URLs contain short-lived authorization codes.

        def do_GET(self):
            url = urllib.parse.urlsplit(self.path)
            query = urllib.parse.parse_qs(url.query)
            state = query.get("state", [""])[0]
            if url.path != "/callback" or state != expected or done.is_set():
                self.send_error(400, "Invalid authorization callback")
                return
            code = query.get("code", [""])[0]
            message = "Authorization was not completed. Return to sub2api and try again."
            status = 400
            if code or query.get("error"):
                callback_code = code or ("http://127.0.0.1:59653/callback?" + url.query)
                payload = json.dumps({"code": callback_code, "state": state,
                                      "pairing_ticket": config["pairing_ticket"]}).encode()
                outcome["success"] = deliver_callback(config, payload, deadline)
                if outcome["success"]:
                    status = 200
                    message = "Devin connected. Return to sub2api; you may close this tab."
                else:
                    message = "Could not finish authorization. Check the sub2api connection page."
            self.send_response(status)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(message.encode())
            done.set()

    server = HTTPServer(("127.0.0.1", 59653), Handler)
    server.timeout = 1
    deadline = min(config["expires_at"], time.time() + 600)
    print("Opening Devin sign-in. Finish sign-in in your browser.")
    webbrowser.open(config["auth_url"])
    try:
        while not done.is_set() and time.time() < deadline:
            server.handle_request()
    finally:
        server.server_close()
    if not outcome["success"]:
        raise RuntimeError("Login failed or timed out. Start a new connection in sub2api.")
    print("Devin connected. Return to sub2api.")


if __name__ == "__main__":
    try:
        run(json.loads(__PAIRING_JSON__))
    except (OSError, ValueError, RuntimeError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
