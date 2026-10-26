"""Container PID 1: one optional loopback-to-fixed-Unix relay and one child.

No Docker socket, parent process namespace or native credential is available.
The product's copied localhost configuration is used byte-for-byte.
"""
import os
import select
import signal
import socket
import socketserver
import subprocess
import sys
import threading


def copy_streams(left, right):
    while True:
        readable, _, _ = select.select([left, right], [], [], 1)
        for source in readable:
            data = source.recv(65536)
            if not data:
                return
            (right if source is left else left).sendall(data)


class Relay(socketserver.BaseRequestHandler):
    def handle(self):
        try:
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as remote:
                remote.connect('/run/devin-fixed-relay.sock')
                copy_streams(self.request, remote)
        except OSError:
            return


def main():
    port = int(sys.argv[1])
    server = None
    if port:
        class Server(socketserver.ThreadingTCPServer):
            daemon_threads = True
            allow_reuse_address = True
        server = Server(('127.0.0.1', port), Relay)
        threading.Thread(target=server.serve_forever, daemon=True).start()
    child = subprocess.Popen(sys.argv[2:], start_new_session=True)
    def stop(number, _frame):
        if child.poll() is None:
            os.killpg(child.pid, signal.SIGTERM)
            try:
                child.wait(timeout=3)
            except subprocess.TimeoutExpired:
                os.killpg(child.pid, signal.SIGKILL)
        raise SystemExit(128 + number)
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        return child.wait()
    finally:
        if child.poll() is None:
            os.killpg(child.pid, signal.SIGKILL)
            child.wait(timeout=3)
        if server:
            server.shutdown()
            server.server_close()


if __name__ == '__main__':
    raise SystemExit(main())
