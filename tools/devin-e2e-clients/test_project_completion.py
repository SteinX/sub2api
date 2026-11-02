import hashlib
import unittest

from project_completion import CompletionEvidence


class ProjectCompletionTests(unittest.TestCase):
    def test_natural_text_fragments_and_part_snapshots_keep_only_fingerprints(self):
        value = CompletionEvidence()
        value.observe({'type': 'step_start'})
        value.observe({'type': 'text', 'part': {'id': 'part-1', 'text': 'Tests'}})
        value.observe({'type': 'text', 'part': {'id': 'part-1', 'text': 'Tests passed.'}})
        value.observe({'type': 'text', 'part': {'text': ' Project repaired.'}})
        value.observe({'type': 'step_finish', 'part': {'reason': 'stop'}})
        report = value.report('PROJECT_COMPLETE')
        self.assertTrue(report['normal_stop'])
        self.assertFalse(report['final_text_matches'])
        self.assertEqual(hashlib.sha256(b'Tests passed. Project repaired.').hexdigest(), report['final_text_sha256'])
        self.assertEqual(2, report['final_text_parts'])
        self.assertNotIn('Tests passed.', str(report))

    def test_old_marker_or_old_stop_cannot_complete_a_new_unfinished_step(self):
        value = CompletionEvidence()
        value.observe({'type': 'step_start'})
        value.observe({'type': 'text', 'part': {'text': 'PROJECT_COMPLETE'}})
        value.observe({'type': 'step_finish', 'part': {'reason': 'stop'}})
        value.observe({'type': 'step_start'})
        value.observe({'type': 'text', 'part': {'text': 'Still working'}})
        report = value.report('PROJECT_COMPLETE')
        self.assertFalse(report['normal_stop'])
        self.assertFalse(report['terminal_event_seen'])
        self.assertTrue(report['marker_seen_anywhere'])

    def test_early_marker_followed_by_error_is_sticky(self):
        value = CompletionEvidence()
        for event in [{'type': 'step_start'}, {'type': 'text', 'part': {'text': 'PROJECT_COMPLETE'}},
                      {'type': 'step_finish', 'part': {'reason': 'tool_calls'}}, {'type': 'error'}]:
            value.observe(event)
        self.assertTrue(value.failed)
        self.assertFalse(value.report('PROJECT_COMPLETE')['normal_stop'])

    def test_omp_requires_the_latest_assistant_and_turn_to_finish(self):
        value = CompletionEvidence()
        value.observe({'type': 'message_start', 'message': {'role': 'assistant'}})
        value.observe({'type': 'message_end', 'message': {'role': 'assistant', 'stopReason': 'stop', 'content': [{'type': 'text', 'text': 'Verified the report.'}]}})
        self.assertTrue(value.report('PROJECT_REVIEWED')['normal_stop'])
        value.observe({'type': 'turn_start'})
        value.observe({'type': 'message_start', 'message': {'role': 'assistant'}})
        self.assertFalse(value.report('PROJECT_REVIEWED')['normal_stop'])
        value.observe({'type': 'message_end', 'message': {'role': 'assistant', 'stopReason': 'error', 'content': []}})
        self.assertTrue(value.failed)

    def test_unmatched_or_empty_terminal_is_not_completion(self):
        value = CompletionEvidence()
        with self.assertRaises(ValueError):
            value.observe({'type': 'step_finish', 'part': {'reason': 'stop'}})
        value.observe({'type': 'step_start'})
        value.observe({'type': 'step_finish', 'part': {'reason': 'stop'}})
        self.assertEqual(0, value.report('PROJECT_COMPLETE')['final_text_bytes'])


if __name__ == '__main__':
    unittest.main()
