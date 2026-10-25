import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import acceptance as gate
import github_snapshot
import reassess_legacy
import release_guard

COMMIT, TREE, DIGEST = 'a' * 40, 'b' * 40, 'c' * 64


def evidence(live=False):
    cases = []
    for ident in sorted(gate.CASE_IDS):
        client, kind, profile = ident.split('/')
        terminal = {'exit_code': 0, 'normal_stop': True, 'final_text_matches': True, 'event_error': False,
                    'timed_out': False, 'parse_errors': 0, 'session_changed': False, 'session_seen': True,
                    'client_session_sha256': DIGEST, 'tools': {'read': 4, 'write': 1, 'bash': 1},
                    'client_tests_succeeded': True, 'test_command_verified': True, 'bash_exit_code': 0}
        case = {'id': ident, 'model_kind': kind, 'status': 'passed', 'artifact_sha256': DIGEST,
                'public_model_uid': 'fusion-test' if kind == 'fusion' else 'physical-lead',
                'physical_lead_uid': 'physical-lead', 'physical_sidekick_uid': 'physical-sidekick' if kind == 'fusion' else '',
                'observed_physical_models': {'physical-lead': 2, 'physical-sidekick': 3} if kind == 'fusion' else {'physical-lead': 5},
                'native_context_hashes': {'lead': {DIGEST: True}, 'sidekick': {'d' * 64: True}}, 'public_requests': 8,
                'task': copy.deepcopy(terminal), 'resume': copy.deepcopy(terminal),
                'client_identity': {'package': gate.CLIENT_PINS[client][0], 'version': gate.CLIENT_PINS[client][1],
                                    'executable_sha256': DIGEST, 'engine_version': 'pinned'},
                'handoff': {'passed': True, 'source': 'UseKeyModal browser copy buttons',
                            'selected_model': 'fusion-test' if kind == 'fusion' else 'physical-lead',
                            'ui_scope': 'deployed product Use Key journey' if live else 'isolated product modal; signup/login not covered',
                            'files': [{'name': 'product-config', 'sha256': DIGEST}]}, 'pricing_contract': {'identified': True}}
        for key in ['initial_project_test_failed', 'protected_project_files_intact', 'artifact_verified_before_oracle',
                    'artifact_verified_after_resume', 'source_unchanged_after_resume', 'independent_project_test_passed', 'same_client_session']:
            case[key] = True
        for key in ['context_pressure', 'compact', 'fork', 'worker', 'disconnect_recovery']:
            case[key] = 'passed' if live else 'not_run'
        cases.append(case)
    report = {'schema_version': 1, 'tier': 'user_e2e' if live else 'client_mock', 'real_upstream': live,
              'eligible_for_real_e2e': live, 'unverified': [] if live else ['real upstream'],
              'source': {'commit': COMMIT, 'tree': TREE, 'dirty': False, 'backend_sha256': DIGEST,
                         'ui_bundle_sha256': DIGEST, 'client_lock_sha256': DIGEST}, 'cases': cases}
    state = {'commit': COMMIT, 'pr': 21, 'snapshot_complete': True, 'checks': [
        {'workflow': workflow, 'name': name, 'head_sha': COMMIT, 'status': 'completed', 'conclusion': 'success'}
        for workflow, name in sorted(gate.REQUIRED_CHECKS)], 'reviews': [], 'outstanding_checks': [],
        'unresolved_findings': 0, 'independent_review': {'head': COMMIT, 'base': 'e' * 40, 'status': 'cleared', 'patch_sha256': DIGEST}}
    return report, state


class AcceptanceTests(unittest.TestCase):
    def test_direct_case_needs_no_invented_sidekick(self):
        report, state = evidence(True)
        self.assertEqual([], gate.validate(report, 'user_e2e', COMMIT, TREE, state))
        next(case for case in report['cases'] if case['model_kind'] == 'fusion')['physical_sidekick_uid'] = ''
        self.assertTrue(gate.validate(report, 'user_e2e', COMMIT, TREE, state))

    def test_mock_cannot_certify_live_even_with_green_checks(self):
        report, state = evidence()
        self.assertEqual([], gate.validate(report, 'client_mock', COMMIT, TREE))
        self.assertIn('real_upstream_not_verified', gate.validate(report, 'user_e2e', COMMIT, TREE, state))

    def test_boolean_session_claim_cannot_hide_identity_or_terminal_failure(self):
        for field, value in [('client_session_sha256', 'e' * 64), ('parse_errors', 1), ('session_changed', True),
                             ('normal_stop', False), ('event_error', True), ('timed_out', True)]:
            with self.subTest(field=field):
                report, state = evidence(True)
                report['cases'][0]['resume'][field] = value
                self.assertTrue(gate.validate(report, 'user_e2e', COMMIT, TREE, state))

    def test_resume_artifact_tampering_and_wrong_config_model_fail(self):
        for field in ['artifact_verified_after_resume', 'source_unchanged_after_resume', 'protected_project_files_intact']:
            report, state = evidence(True)
            report['cases'][0][field] = False
            self.assertTrue(gate.validate(report, 'user_e2e', COMMIT, TREE, state))
        report, state = evidence(True)
        report['cases'][0]['handoff']['selected_model'] = 'another-model'
        self.assertTrue(gate.validate(report, 'user_e2e', COMMIT, TREE, state))

    def test_filtered_green_snapshot_is_rejected(self):
        report, state = evidence(True)
        state['checks'] = [state['checks'][0]]
        self.assertTrue(any('required_check_missing:' in error for error in gate.validate(report, 'user_e2e', COMMIT, TREE, state)))
        report, state = evidence(True)
        state['snapshot_complete'] = False
        self.assertIn('review_security_state_unavailable', gate.validate(report, 'user_e2e', COMMIT, TREE, state))

    def test_all_started_security_review_and_source_requirements_apply(self):
        for mutation in ['external_pending', 'external_failure', 'security_skipped', 'unresolved', 'changes_requested', 'wrong_sha', 'old_review']:
            with self.subTest(mutation=mutation):
                report, state = evidence(True)
                if mutation.startswith('external'):
                    state['checks'].append({'name': 'independent security', 'head_sha': COMMIT, 'status': 'in_progress' if mutation.endswith('pending') else 'completed', 'conclusion': 'failure'})
                elif mutation == 'security_skipped':
                    next(check for check in state['checks'] if check['workflow'] == 'security-scan.yml')['conclusion'] = 'skipped'
                elif mutation == 'unresolved':
                    state['unresolved_findings'] = 1
                elif mutation == 'changes_requested':
                    state['reviews'] = [{'reviewer': 'hash', 'state': 'CHANGES_REQUESTED'}]
                elif mutation == 'wrong_sha':
                    state['checks'][0]['head_sha'] = 'd' * 40
                else:
                    state['independent_review']['head'] = 'd' * 40
                self.assertTrue(gate.validate(report, 'user_e2e', COMMIT, TREE, state))

    def test_product_refusal_is_not_a_task_pass_or_live_acceptance(self):
        report, state = evidence()
        case = next(case for case in report['cases'] if case['id'] == 'omp/fusion/default')
        case.update(status='unsupported_by_product', public_requests=0, observed_physical_models={},
                    handoff={'blocked': True, 'source': 'UseKeyModal browser refusal', 'reason': 'omp_default_fusion_history_pruning', 'files': []})
        self.assertEqual([], gate.validate(report, 'client_mock', COMMIT, TREE))
        self.assertTrue(gate.validate(report, 'user_e2e', COMMIT, TREE, state))
        case['public_requests'] = 1
        self.assertTrue(gate.validate(report, 'client_mock', COMMIT, TREE))

    def test_frozen_default_omp_failure_is_retained_as_failure(self):
        report = json.loads((Path(__file__).parent / 'fixtures/omp-default-fusion-before.json').read_text())
        case = report['cases'][0]
        self.assertEqual('failed', case['status'])
        self.assertTrue(case['task']['normal_stop'])
        self.assertTrue(case['resume']['event_error'])
        self.assertEqual(1, case['error_codes']['devin_fusion_history_conflict'])
        self.assertFalse(report['source']['dirty'])
        self.assertTrue(gate.validate(report, 'client_mock', report['source']['commit'], report['source']['tree']))

    def test_wrong_tree_and_private_payload_are_rejected(self):
        report, _ = evidence()
        self.assertIn('source_mismatch', gate.validate(report, 'client_mock', COMMIT, 'f' * 40))
        report['api_key'] = 'synthetic-only'
        self.assertIn('private_fields_present', gate.validate(report, 'client_mock', COMMIT, TREE))


class LegacyTests(unittest.TestCase):
    def test_cancellation_marker_requires_same_source_parent_pass(self):
        for parent in ['pass', 'fail', None]:
            with self.subTest(parent=parent), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                name = 'TestDevinRealAgentsMock/omp/direct/interrupt_before_upstream_send'
                events = [{'Test': name, 'Action': 'output', 'Output': 'client=omp action=tool exit=-15 frames=2 completed_read=0 expected_text=false interrupted_before_forward=true'},
                          {'Test': name, 'Action': 'pass'}]
                if parent:
                    events.append({'Test': name.rsplit('/', 1)[0], 'Action': parent})
                (directory / 'results.jsonl').write_text('\n'.join(map(json.dumps, events)))
                self.assertEqual(1 if parent == 'pass' else 0, reassess_legacy.reassess([directory])['reusable_cancel_contracts'])


class ReleaseBoundaryTests(unittest.TestCase):
    def test_lightweight_tag_cannot_authorize_runtime_release(self):
        def git(*args):
            if args[0] == 'rev-parse':
                return TREE if args[-1].endswith('^{tree}') else COMMIT
            if args[0] == 'cat-file':
                return 'commit'
            raise AssertionError(args)
        with patch.object(release_guard, 'requires_acceptance', return_value=True), patch.object(release_guard, 'git', side_effect=git), patch.object(release_guard, 'collect') as collect:
            with self.assertRaisesRegex(ValueError, 'annotated'):
                release_guard.evaluate('v9.9.9', 'owner/repo')
            collect.assert_not_called()

    def test_release_queries_fresh_state_instead_of_trusting_annotation(self):
        import base64
        report, state = evidence(True)
        annotation = gate.PREFIX + base64.b64encode(json.dumps({'report': report, 'prerequisites': state}).encode()).decode()
        fresh = copy.deepcopy(state)
        fresh['checks'][0]['status'] = 'in_progress'
        def git(*args):
            return {'rev-parse': TREE if args[-1].endswith('^{tree}') else COMMIT, 'cat-file': 'tag', 'for-each-ref': annotation}[args[0]]
        with patch.object(release_guard, 'requires_acceptance', return_value=True), patch.object(release_guard, 'git', side_effect=git), patch.object(release_guard, 'collect', return_value=fresh) as collect:
            self.assertFalse(release_guard.evaluate('v9.9.9', 'owner/repo')['accepted'])
            collect.assert_called_once_with('owner/repo', 21, COMMIT, state['independent_review'])


if __name__ == '__main__':
    unittest.main()
