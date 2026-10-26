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


class BatchControlTests(unittest.TestCase):
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
