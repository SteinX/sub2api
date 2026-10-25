import io
import queue
import time
import unittest
from unittest.mock import Mock

from capabilities import Rpc


def rpc_with(events):
    rpc = Rpc.__new__(Rpc)
    rpc.serial = 0
    rpc.deadline = time.monotonic() + 1
    rpc.events = queue.Queue()
    for event in events:
        rpc.events.put(event)
    rpc.frames = rpc.parse_errors = rpc.reads = 0
    rpc.failed = False
    rpc.error_categories = set()
    rpc.error_http_status = None
    rpc.final_text = ''
    rpc.proc = Mock()
    rpc.proc.stdin = io.BytesIO()
    rpc.proc.poll.return_value = None
    return rpc


class RpcProofTests(unittest.TestCase):
    def test_zero_exit_or_response_ack_does_not_complete_a_failed_prompt(self):
        for status, invoked, settled in [('error', True, True), ('completed', False, True), ('completed', True, False)]:
            with self.subTest(status=status, invoked=invoked, settled=settled):
                rpc = rpc_with([{'id': '1', 'type': 'response', 'success': True},
                                {'id': '1', 'type': 'prompt_result', 'status': status, 'agentInvoked': invoked, 'sessionSettled': settled}])
                with self.assertRaises(ValueError):
                    rpc.call('prompt', message='synthetic')

    def test_tool_error_prevents_a_successful_terminal_from_passing(self):
        rpc = rpc_with([{'type': 'tool_execution_end', 'toolName': 'read', 'isError': True},
                        {'id': '1', 'type': 'prompt_result', 'status': 'completed', 'agentInvoked': True, 'sessionSettled': True}])
        with self.assertRaises(ValueError):
            rpc.call('prompt', message='synthetic')

    def test_rpc_responses_are_bound_to_the_actual_request_id(self):
        rpc = rpc_with([{'id': 'unrelated', 'type': 'response', 'success': True, 'data': {'wrong': True}},
                        {'id': '1', 'type': 'response', 'success': True, 'data': {'right': True}}])
        self.assertEqual({'right': True}, rpc.call('get_state'))


if __name__ == '__main__':
    unittest.main()
