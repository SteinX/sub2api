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
                decision=json.loads(line)
                if not isinstance(decision,dict) or set(decision)!={'accepted'} or type(decision['accepted']) is not bool:
                    raise ValueError('control_invalid_decision')
                accepted = decision['accepted']
            except (OSError, ValueError):
                self.stopped = True
                # Force EOF at the trusted parent even if only our decoding
                # failed. Do not leave a live but unusable control connection.
                try:
                    self.socket.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
                self.socket.close()
                try:
                    self.reader.close()
                except OSError:
                    pass
                raise ValueError('batch_control_lost') from None
            if not accepted:
                self.stopped = True
            return accepted

    def begin(self, operation='task'):
        if self.stopped or not self.send('begin', operation=operation):
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
        reason = record.get('fatal_reason')
        if record.get('error_kind') == 'client_permission_rejection':
            reason = 'permission_rejection'
        elif record['status'] == 'error' and record.get('recoverable_error') is not True:
            reason = reason or 'transport_uncertain'
        if reason:
            self.stop(reason, record.get('observations', [{}])[-1].get('observed_at_ns'))
        elif record['call_sha256'] not in self.acknowledged:
            if not self.send('tool', call_sha256=record['call_sha256'], status=record['status'], failed=record.get('recoverable_error') is True):
                raise ValueError('batch_stopped')
            self.acknowledged.add(record['call_sha256'])

    def end(self, normal):
        if normal and not self.stopped:
            if not self.send('end'):
                raise ValueError('batch_stopped')
            self.phase = False
        else:
            self.stop('missing_terminal')

    def observe_terminal(self, event):
        """Stop on actual error terminals; normal text never ACKs a tool.

        No-tool protocol completion is checked at the public boundary. It may
        legitimately be followed by compact/main HTTP calls in the same phase.
        """
        kind=event.get('type')
        reason=(event.get('part',{}).get('reason') if kind=='step_finish' else
                event.get('message',{}).get('stopReason') if kind=='message_end' and event.get('message',{}).get('role')=='assistant' else None)
        if reason in {'error','aborted','length','max_tokens'}:
            self.stop('protocol_error')
        elif kind=='auto_compaction_end' and (event.get('aborted') is True or event.get('errorMessage')):
            self.stop('protocol_error')

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
