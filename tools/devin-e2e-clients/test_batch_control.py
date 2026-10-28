import json
import os
import socket
import tempfile
import threading
import unittest
from pathlib import Path
from unittest.mock import patch

import batch_control
import journey
from project_evidence import ToolEvidence


class BatchControlTests(unittest.TestCase):
    def test_context_deadline_never_acknowledges_tool_or_allows_continuation(self):
        for name, status in [('read', 'error'), ('bash', 'completed')]:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as temporary:
                tracker = ToolEvidence(Path(temporary), 'python3 run_tests.py')
                state = {'status': status, 'input': {}, 'metadata': {'exit': 1} if name == 'bash' else {}}
                state['output' if name == 'bash' else 'error'] = 'context deadline exceeded'
                tool = tracker.observe({'type': 'tool_use', 'part': {'tool': name, 'callID': 'synthetic-deadline', 'state': state}})
                left, right = socket.socketpair()
                control = batch_control.Control(); control.socket = left; control.reader = left.makefile('rb')
                messages = []
                def parent():
                    with right.makefile('rb') as reader:
                        for line in reader:
                            messages.append(json.loads(line)); right.sendall(b'{"accepted":true}\n')
                thread = threading.Thread(target=parent); thread.start()
                try:
                    control.begin(); control.tool(tool)
                    self.assertTrue(control.stopped)
                    with self.assertRaisesRegex(ValueError, 'batch_stopped'):
                        control.begin()
                    control.finish(False); thread.join(timeout=3)
                    self.assertFalse(thread.is_alive())
                    self.assertEqual(['begin', 'stop'], [x['kind'] for x in messages])
                    self.assertEqual('transport_uncertain', messages[1]['reason'])
                finally:
                    control.finish(False); right.close()

    def test_control_path_is_never_in_model_or_browser_environment(self):
        with tempfile.TemporaryDirectory() as temporary, patch.dict(os.environ, {'DEVIN_JOURNEY_CONTROL_SOCKET': '/private/control.sock'}):
            env = journey.project_environment(Path(temporary))
            self.assertNotIn('DEVIN_JOURNEY_CONTROL_SOCKET', env)

    def test_error_is_one_shot_and_late_success_never_reopens(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = str(Path(temporary)/'control.sock')
            server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            server.bind(path); server.listen()
            messages = []
            def serve():
                conn, _ = server.accept()
                with conn, conn.makefile('rb') as reader:
                    for line in reader:
                        event = json.loads(line); messages.append(event)
                        conn.sendall(json.dumps({'accepted':event['kind']=='begin'}).encode()+b'\n')
            thread = threading.Thread(target=serve); thread.start()
            with patch.dict(os.environ, {'DEVIN_JOURNEY_CONTROL_SOCKET': path}):
                control = batch_control.Control(); control.begin()
                control.tool({'status':'error','error_kind':'client_permission_rejection'})
                self.assertTrue(control.stopped)
                control.stop('tool_error')
                with self.assertRaisesRegex(ValueError,'batch_stopped'): control.begin()
                control.finish(False)
            thread.join(timeout=3); server.close()
            self.assertFalse(thread.is_alive())
            self.assertEqual(['begin','stop'],[m['kind'] for m in messages])
            self.assertEqual('permission_rejection',messages[1]['reason'])
            self.assertTrue(all(type(m['observed_at_ns']) is int for m in messages))

    def test_disconnected_observer_does_not_resend_an_uncertain_message(self):
        left, right = socket.socketpair()
        control = batch_control.Control()
        control.socket=left; control.reader=left.makefile('rb')
        right.close()
        try:
            with self.assertRaisesRegex(ValueError,'batch_control_lost'): control.begin()
            self.assertTrue(control.stopped)
            with self.assertRaisesRegex(ValueError,'batch_stopped'): control.begin()
        finally:
            control.finish(False)

    def test_completed_error_acknowledges_once_without_retry_or_budget_refund(self):
        left,right=socket.socketpair()
        control=batch_control.Control();control.socket=left;control.reader=left.makefile('rb')
        messages=[]
        def parent():
            with right.makefile('rb') as reader:
                for line in reader:
                    messages.append(json.loads(line));right.sendall(b'{"accepted":true}\n')
        thread=threading.Thread(target=parent);thread.start()
        try:
            control.begin()
            record={'status':'error','error_kind':'client_tool_error','recoverable_error':True,'fatal_reason':None,'call_sha256':'a'*64}
            control.tool(record);control.tool(record)
            self.assertFalse(control.stopped)
            control.end(True);control.finish(True)
            thread.join(timeout=3)
            self.assertEqual(['begin','tool','end','finish'],[x['kind'] for x in messages])
            self.assertEqual('error',messages[1]['status']);self.assertTrue(messages[1]['failed'])
        finally:
            right.close()

    def test_unknown_control_reply_immediately_closes_the_parent_channel(self):
        for reply in [b'null\n',None]:
            with self.subTest(reply=reply):
                left,right=socket.socketpair();left.settimeout(.03);right.settimeout(2)
                control=batch_control.Control();control.socket=left;control.reader=left.makefile('rb')
                eof=[]
                def parent():
                    right.recv(4096)
                    if reply is not None: right.sendall(reply)
                    eof.append(right.recv(4096))
                thread=threading.Thread(target=parent);thread.start()
                try:
                    with self.assertRaisesRegex(ValueError,'batch_control_lost'):control.begin()
                    thread.join(timeout=3)
                    self.assertFalse(thread.is_alive());self.assertEqual([b''],eof)
                    self.assertTrue(control.stopped)
                finally:
                    right.close();control.finish(False)


if __name__=='__main__': unittest.main()
