"""Private trusted observer channel. Never forward it to a model subprocess.

The gateway registers public tool-call hashes before releasing its shared slot.
Only actual client terminal tool events release that barrier. Any uncertainty
permanently stops the batch; this module never resends a control operation.
"""
import json
import os
import socket
import threading
import time


class Control:
    def __init__(self):
        self.lock = threading.Lock()
        self.socket = self.reader = None
        self.stopped = False
        self.phase = False
        self.acknowledged = set()
        path = os.environ.get('DEVIN_JOURNEY_CONTROL_SOCKET')
        if path:
            self.socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self.socket.settimeout(25)
            self.socket.connect(path)
            self.reader = self.socket.makefile('rb')

    def send(self, kind, **fields):
        if not self.socket:
            return True  # Unit/legacy protocol runners do not certify this guard.
        with self.lock:
            try:
                self.socket.sendall((json.dumps({'kind': kind, 'observed_at_ns': time.time_ns(), **fields})+'\n').encode())
                line = self.reader.readline(4097)
                if len(line)>4096 or not line.endswith(b'\n'):
                    raise ValueError('control_incomplete')
                accepted = json.loads(line).get('accepted') is True
            except (OSError, ValueError):
                self.stopped = True
                raise ValueError('batch_control_lost') from None
            if not accepted:
                self.stopped = True
            return accepted

    def begin(self):
        if self.stopped or not self.send('begin'):
            raise ValueError('batch_stopped')
        self.phase = True
        self.acknowledged.clear()

    def stop(self, reason, observed_ns=None):
        if not self.stopped:
            try:
                self.send('stop', reason=reason, observed_at_ns=observed_ns or time.time_ns())
            finally:
                self.stopped = True

    def tool(self, record):
        if not record or record['status'] not in {'completed', 'error'}:
            return
        if record['status']=='error':
            self.stop('permission_rejection' if record.get('error_kind')=='client_permission_rejection' else 'tool_error',
                      record.get('observations', [{}])[-1].get('observed_at_ns'))
        elif record['call_sha256'] not in self.acknowledged:
            if not self.send('tool', call_sha256=record['call_sha256'], status='completed'):
                raise ValueError('batch_stopped')
            self.acknowledged.add(record['call_sha256'])

    def end(self, normal):
        if normal and not self.stopped:
            if not self.send('end'):
                raise ValueError('batch_stopped')
            self.phase = False
        else:
            self.stop('missing_terminal')

    def finish(self, normal):
        try:
            if not normal:
                self.stop('source_or_oracle_failure')
            elif not self.stopped:
                self.send('finish')
        finally:
            if self.reader:
                self.reader.close()
            if self.socket:
                self.socket.close()


_control = None


def current():
    global _control
    if not os.environ.get('DEVIN_JOURNEY_CONTROL_SOCKET'):
        return Control()
    if _control is None:
        _control = Control()
    return _control
