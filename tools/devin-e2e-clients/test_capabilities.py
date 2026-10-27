import io
import queue
import time
import signal
import subprocess
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

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
    rpc.error_vocabulary = set()
    rpc.final_text = ''
    rpc.proc = Mock()
    rpc.proc.stdin = io.BytesIO()
    rpc.proc.poll.return_value = None
    return rpc


class RpcProofTests(unittest.TestCase):
    def test_invalid_or_unreadable_diagnostics_do_not_prevent_cleanup(self):
        for bad in [b'{"truncated":', OSError('synthetic read failure')]:
            rpc = rpc_with([])
            rpc.trace = Path('/synthetic/trace')
            reader = Mock(side_effect=bad) if isinstance(bad, OSError) else Mock(return_value=bad)
            rpc.reader = Mock(); rpc.reader.is_alive.return_value = False
            rpc.h = SimpleNamespace(ACTIVE={rpc.proc}, project_bytes=reader)
            with patch('capabilities.os.killpg'):
                try:
                    self.assertEqual([], rpc.take_fetch_observations())
                    self.assertEqual('fetch_observation_unavailable_or_invalid', rpc.trace_error)
                finally:
                    self.assertTrue(rpc.close())
            self.assertNotIn(rpc.proc, rpc.h.ACTIVE)

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

    def test_local_compact_uses_its_actual_response_contract(self):
        rpc = rpc_with([{'id': '1', 'type': 'response', 'success': True, 'data': {'agentInvoked': False}}])
        self.assertEqual({'local_command_acknowledged': True}, rpc.call('prompt', message='/compact soft', local_prompt=True))
        rpc = rpc_with([{'id': '1', 'type': 'response', 'success': True, 'data': {'agentInvoked': False}},
                        {'id': '1', 'type': 'prompt_result', 'status': 'completed', 'agentInvoked': False, 'sessionSettled': True}])
        with self.assertRaises(ValueError):
            rpc.call('prompt', message='ordinary task', local_prompt=True)

    def test_cleanup_escalates_and_reaps_without_losing_evidence(self):
        for permanently_stuck in [False, True]:
            with self.subTest(permanently_stuck=permanently_stuck):
                rpc = rpc_with([])
                rpc.reader = Mock()
                rpc.reader.is_alive.return_value = False
                rpc.h = SimpleNamespace(ACTIVE={rpc.proc})
                rpc.proc.pid = 12345
                rpc.proc.wait.side_effect = [subprocess.TimeoutExpired('synthetic', 5), subprocess.TimeoutExpired('synthetic', 5) if permanently_stuck else 0]
                with patch('capabilities.os.killpg') as kill:
                    self.assertEqual(not permanently_stuck, rpc.close())
                self.assertEqual([(12345, signal.SIGTERM), (12345, signal.SIGKILL)], [call.args for call in kill.call_args_list])
                self.assertNotIn(rpc.proc, rpc.h.ACTIVE)
                self.assertTrue(rpc.proc.stdin.closed)


if __name__ == '__main__':
    unittest.main()
