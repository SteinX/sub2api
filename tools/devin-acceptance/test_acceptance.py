import copy
import hashlib
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
                    'completion_contract': 'project_results_v3', 'terminal_event_seen': True,
                    'reader_completed': True, 'reader_error': False,
                    'final_text_bytes': 35, 'final_text_sha256': DIGEST, 'project_nonce_sha256': DIGEST,
                    'client_tests_succeeded': True, 'test_command_verified': True, 'bash_exit_code': 0, 'plugin_runtime_started': True}
        case = {'id': ident, 'model_kind': kind, 'status': 'passed', 'artifact_sha256': DIGEST,
                'batch_guard':{'installed':True,'response_observation':'original_stream_with_source_proof' if live else 'bounded_response_buffer','buffered_transport':not live,'stopped':False,'phase_active':False,'public_inflight':False,'pending_tools':0,'first_failure':None},
                'source_before_task_sha256': '1' * 64, 'source_after_task_sha256': '2' * 64,
                'source_changed_by_task': True, 'project_nonce_sha256': DIGEST,
                'protected_project_sha256': {name: DIGEST for name in ['requirements.json','test_project.py','run_tests.py']},
                'independent_project_test_executed': True,
                'tier': 'user_e2e' if live else 'client_mock', 'eligible_for_real_e2e': live,
                'public_model_uid': 'fusion-test' if kind == 'fusion' else 'physical-lead',
                'physical_lead_uid': 'physical-lead', 'physical_sidekick_uid': 'physical-sidekick' if kind == 'fusion' else '',
                'observed_physical_models': {'physical-lead': 2, 'physical-sidekick': 3} if kind == 'fusion' else {'physical-lead': 5},
                'native_context_hashes': {'lead': {DIGEST: True}, 'sidekick': {'d' * 64: True}}, 'public_requests': 8,
                'task': copy.deepcopy(terminal), 'resume': copy.deepcopy(terminal),
                'client_identity': {'package': gate.CLIENT_PINS[client][0], 'version': gate.CLIENT_PINS[client][1],
                                    'executable_sha256': DIGEST, 'engine_version': 'pinned'},
                'handoff': {'passed': True, 'source': 'UseKeyModal browser copy buttons',
                            'selected_model': 'fusion-test' if kind == 'fusion' else 'physical-lead',
                            'ui_scope': gate.PRODUCT_UI_SCOPE,
                            'product_journey': {'login_http_status': 200, 'key_list_http_status': 200, 'selected_key_matches': True, 'routes': ['/login', '/dashboard', '/keys']},
                            'files': [{'name': 'product-config', 'sha256': DIGEST}]},
                'pricing_contract': {'identified': True, 'physical_model_uids': ['physical-lead', 'physical-sidekick'] if kind == 'fusion' else ['physical-lead'],
                                     'snapshot_sha256': DIGEST, 'group_config_sha256': DIGEST, 'public_key_route_verified': True,
                                     'sources': {'physical-lead': 'builtin_exact', 'physical-sidekick': 'group'}}}
        for phase in ['task', 'resume']:
            result = case[phase]
            result['tool_states'] = {name: {'completed': count} for name, count in result['tools'].items() if count}
            result['tool_events'] = copy.deepcopy(result['tool_states'])
            result['incomplete_tool_calls'] = 0
            result['tool_calls'] = [{'name': name, 'call_sha256': hashlib.sha256((phase + name + str(index)).encode()).hexdigest(),
                                     'status': 'completed', 'states': ['completed'], 'source_targeted': name == 'write' and phase == 'task',
                                     **({'command_class': 'configured_project_tests', 'command_sha256': DIGEST} if name == 'bash' else {})}
                                    for name, count in result['tools'].items() for index in range(count)]
            bash = next(call for call in result['tool_calls'] if call['name'] == 'bash')
            receipt = {'call_sha256': bash['call_sha256'], 'command_class': 'configured_project_tests', 'command_sha256': DIGEST,
                       'state': 'completed', 'completion_sequence': 1, 'execution_verified': True, 'receipt_status': 'verified',
                       'passed': True, 'exit_code': 0, 'exit_source': 'client_metadata', 'receipt_sha256': DIGEST, 'output_reference_sha256': DIGEST, 'run_sha256': DIGEST,
                       'project_nonce_sha256': DIGEST, 'source_before_sha256': '2' * 64, 'source_after_sha256': '2' * 64,
                       'protected_sha256': dict(case['protected_project_sha256']), 'tests_run': 1, 'failures': 0, 'errors': 0,
                       'skipped': 0, 'expected_failures': 0, 'unexpected_successes': 0, 'started_ns': 2, 'finished_ns': 3}
            result.update(client_started_ns=1, client_finished_ns=4)
            result['test_execution'] = {'schema': 'devin_project_test_v1', 'passed': True, 'conflicting_evidence': False,
                                        'source_after_task_sha256': '2' * 64, 'calls': [receipt], 'selected_call_sha256': bash['call_sha256']}
            result['source_mutation_tools'] = {'write': 1} if phase == 'task' else {}
        for key in ['initial_project_test_failed', 'protected_project_files_intact', 'artifact_verified_before_oracle',
                    'artifact_verified_after_resume', 'source_unchanged_after_resume', 'independent_project_test_passed', 'same_client_session']:
            case[key] = True
        if kind == 'fusion':
            attempts = [{'attempt_sha256': hashlib.sha256(str(index).encode()).hexdigest(), 'role': role,
                         'model_uid': 'physical-' + ('lead' if role == 'lead' else 'sidekick'),
                         'purpose': 'inference', 'status': 'completed', 'settled': True, 'applied': True, 'failure_present': False,
                         'usage_query_available': True, 'usage_available': True, 'usage_matches': True, 'usage_rows': 1,
                         'cost_usd': .001, 'actual_cost_usd': .001, 'total_cost_usd': .001,
                         'input_tokens': 4, 'output_tokens': 2, 'cache_creation_tokens': 0, 'cache_read_tokens': 0}
                        for index, role in enumerate(['lead', 'sidekick', 'lead'])]
            case['fusion_execution'] = {'schema_version': 1, 'available': True, 'complete': True, 'missing_session_headers': 0,
                'sessions': [{'session_sha256': DIGEST, 'available': True, 'complete': True, 'state': 'TURN_COMPLETED',
                             'turns': 1, 'role': 'lead', 'separate_contexts': True, 'role_usage_settled': True,
                             'handoffs': [{'turn': 1, 'handoff_sha256': DIGEST, 'report_present': True, 'report_bytes': 30,
                                           'report_sha256': DIGEST, 'lead_report_linked': True}],
                             'attempts': attempts, 'role_attempts': {'lead': 2, 'sidekick': 1},
                             'lead_review': {'state': 'completed_with_report_context', 'current_turn': 1, 'final_request_index': 3,
                                             'final_answer_bytes': 30, 'final_answer_sha256': DIGEST,
                                             'report_context_linked': True, 'sidekick_then_lead': True, 'request_boundaries_valid': True}}]}
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
    def test_buffered_guard_does_not_certify_unmodified_live_transport(self):
        report,_=evidence(live=True);case=report['cases'][0]
        case['batch_guard'].update(buffered_transport=True,response_observation='bounded_response_buffer')
        self.assertIn(case['id']+':unbuffered_transport_not_verified',gate.validate(report,'user_e2e',COMMIT,TREE))

    def test_missing_stopped_or_unsettled_batch_guard_cannot_certify_a_case(self):
        for change in [None,{'stopped':True},{'pending_tools':1},{'public_inflight':True},{'phase_active':True},{'first_failure':{'reason':'tool_error'}}]:
            report,_=evidence();case=report['cases'][0]
            if change is None: case.pop('batch_guard')
            else: case['batch_guard'].update(change)
            with self.subTest(change=change):
                self.assertIn(case['id']+':first_error_guard_unverified',gate.validate(report,'client_mock',COMMIT,TREE))

    def test_role_counts_alone_cannot_replace_fusion_review_and_settlement(self):
        for target, field, value in [('session', 'complete', False), ('review', 'report_context_linked', False),
                                      ('review', 'sidekick_then_lead', False), ('handoff', 'report_present', False),
                                      ('handoff', 'report_bytes', 0), ('attempt', 'usage_rows', 2),
                                      ('attempt', 'usage_available', False), ('attempt', 'actual_cost_usd', None),
                                      ('attempt', 'total_cost_usd', .002)]:
            report, _ = evidence()
            case = next(case for case in report['cases'] if case['model_kind'] == 'fusion')
            session = case['fusion_execution']['sessions'][0]
            destination = {'session': session, 'review': session['lead_review'], 'handoff': session['handoffs'][0], 'attempt': session['attempts'][0]}[target]
            destination[field] = value
            with self.subTest(target=target, field=field):
                self.assertIn(case['id'] + ':fusion_collaboration_unverified', gate.validate(report, 'client_mock', COMMIT, TREE))

    def test_aggregate_or_text_success_cannot_replace_per_call_test_receipt(self):
        for field, value in [('test_execution', None), ('tool_calls', []), ('tool_calls', None)]:
            report, _ = evidence()
            report['cases'][0]['task'][field] = value
            with self.subTest(field=field, value=value):
                if field == 'test_execution':
                    report['cases'][0]['task'].pop(field)
                self.assertIn(report['cases'][0]['id'] + ':client_test_receipt_unverified', gate.validate(report, 'client_mock', COMMIT, TREE))

    def test_wrong_stale_unproven_or_empty_test_receipts_are_rejected(self):
        for field, value in [('tests_run', 0), ('exit_code', False), ('state', 'error'), ('execution_verified', False),
                             ('project_nonce_sha256', '0' * 64), ('source_after_sha256', '0' * 64),
                             ('protected_sha256', {}), ('started_ns', 0), ('finished_ns', 5), ('receipt_sha256', '')]:
            report, _ = evidence()
            case = report['cases'][0]
            case['task']['test_execution']['calls'][0][field] = value
            with self.subTest(field=field):
                self.assertIn(case['id'] + ':client_test_receipt_unverified', gate.validate(report, 'client_mock', COMMIT, TREE))

    def test_natural_language_completion_does_not_need_exact_marker(self):
        report, _ = evidence()
        for case in report['cases']:
            for phase in ['task', 'resume']:
                case[phase]['final_text_matches'] = False
        self.assertEqual([], gate.validate(report, 'client_mock', COMMIT, TREE))

    def test_unexecuted_oracle_cannot_certify_success(self):
        report, _ = evidence()
        case = report['cases'][0]
        case['independent_project_test_executed'] = False
        self.assertIn(case['id'] + ':independent_oracle_not_executed', gate.validate(report, 'client_mock', COMMIT, TREE))

    def test_marker_cannot_replace_terminal_reader_or_nonce_evidence(self):
        for field, value in [('terminal_event_seen', False), ('reader_completed', False), ('reader_error', True),
                             ('completion_contract', 'old'), ('final_text_bytes', 0), ('final_text_sha256', ''),
                             ('project_nonce_sha256', 'd' * 64)]:
            report, _ = evidence()
            report['cases'][0]['task'][field] = value
            with self.subTest(field=field):
                self.assertTrue(gate.validate(report, 'client_mock', COMMIT, TREE))


    def test_edit_only_mutation_is_valid_without_write(self):
        report, _ = evidence()
        case = report['cases'][0]
        task = case['task']
        task['tools']['write'] = 0
        task['tools']['edit'] = 1
        for field in ['tool_states', 'tool_events']:
            task[field].pop('write')
            task[field]['edit'] = {'completed': 1}
        task['source_mutation_tools'] = {'edit': 1}
        next(call for call in task['tool_calls'] if call['name'] == 'write')['name'] = 'edit'
        self.assertEqual([], gate.validate(report, 'client_mock', COMMIT, TREE))

    def test_completed_write_cannot_replace_independent_source_change(self):
        report, _ = evidence()
        case = report['cases'][0]
        case['source_after_task_sha256'] = case['source_before_task_sha256']
        self.assertIn(case['id'] + ':source_mutation_unverified', gate.validate(report, 'client_mock', COMMIT, TREE))

    def test_results_do_not_require_read_or_mutator_tools_or_resume_tools(self):
        report,_=evidence()
        case=report['cases'][0]
        task=case['task'];bash=next(call for call in task['tool_calls'] if call['name']=='bash')
        bash.update(command_class='other_shell',source_targeted=False)
        task.update(tools={'bash':1},tool_states={'bash':{'completed':1}},tool_events={'bash':{'completed':1}},
                    tool_calls=[bash],source_mutation_tools={})
        receipt=task['test_execution']['calls'][0]
        receipt.update(command_class='other_shell',test_runner_candidate=True)
        case['resume'].update(tools={},tool_states={},tool_events={},tool_calls=[],source_mutation_tools={})
        self.assertEqual([],gate.validate(report,'client_mock',COMMIT,TREE))
        case['artifact_verified_before_oracle']=False
        self.assertIn(case['id']+':artifact_verified_before_oracle',gate.validate(report,'client_mock',COMMIT,TREE))

    def test_tool_diagnostics_must_match_actual_events_and_remain_well_typed(self):
        for changes in [ {'source_mutation_tools': {}}, {'source_mutation_tools': {'bash': 1}},
                         {'source_mutation_tools': {'write': 2}}, {'source_mutation_tools': {'write': True}},
                         {'tool_states': {'write': {'error': 1}}}, {'tool_states': {'write': {'pending': 1}}},
                         {'incomplete_tool_calls': 1}, {'tool_events': {}}, {'tools': {'write': -1}} ]:
            report, _ = evidence()
            case = report['cases'][0]
            case['task'].update(changes)
            with self.subTest(changes=changes):
                self.assertTrue(gate.validate(report, 'client_mock', COMMIT, TREE))

    def test_old_count_only_evidence_cannot_silently_pass_new_gate(self):
        report, _ = evidence()
        report['cases'][0]['task'].pop('tool_states')
        self.assertTrue(gate.validate(report, 'client_mock', COMMIT, TREE))


    def test_installed_omo_package_does_not_prove_plugin_runtime_started(self):
        for phase in ['task','resume']:
            report,_=evidence()
            case=next(c for c in report['cases'] if c['id']=='omo/direct/project')
            case[phase].pop('plugin_runtime_started')
            self.assertIn('omo/direct/project:'+phase+'_omo_plugin_not_observed',gate.validate(report,'client_mock',COMMIT,TREE))

    def test_direct_case_needs_no_invented_sidekick(self):
        report, state = evidence(True)
        self.assertEqual([], gate.validate(report, 'user_e2e', COMMIT, TREE, state))
        next(case for case in report['cases'] if case['model_kind'] == 'fusion')['physical_sidekick_uid'] = ''
        self.assertTrue(gate.validate(report, 'user_e2e', COMMIT, TREE, state))

    def test_mock_cannot_certify_live_even_with_green_checks(self):
        report, state = evidence()
        self.assertEqual([], gate.validate(report, 'client_mock', COMMIT, TREE))
        self.assertIn('real_upstream_not_verified', gate.validate(report, 'user_e2e', COMMIT, TREE, state))
        report, state = evidence(True)
        report['cases'][0].update(tier='client_mock', eligible_for_real_e2e=False)
        self.assertTrue(gate.validate(report, 'user_e2e', COMMIT, TREE, state))
        report, state = evidence(True)
        report['eligible_for_real_e2e'] = False
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
                    handoff={'blocked': True, 'source': 'UseKeyModal browser refusal', 'reason': 'omp_default_fusion_history_pruning', 'files': [],
                             'ui_scope': gate.PRODUCT_UI_SCOPE, 'product_journey': {'login_http_status': 200, 'key_list_http_status': 200, 'selected_key_matches': True, 'routes': ['/login', '/dashboard', '/keys']}})
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

    def test_truthy_pricing_object_cannot_hide_an_unidentified_physical_member(self):
        for change in [{'identified': False}, {'physical_model_uids': ['virtual-fusion']}, {'sources': {'physical-lead': 'family_guess'}}, {'public_key_route_verified': False}]:
            with self.subTest(change=change):
                report, state = evidence(True)
                report['cases'][0]['pricing_contract'].update(change)
                self.assertTrue(gate.validate(report, 'user_e2e', COMMIT, TREE, state))

    def test_comment_does_not_clear_requested_changes(self):
        report, state = evidence(True)
        state['reviews'] = [{'reviewer': 'same', 'state': 'CHANGES_REQUESTED'}, {'reviewer': 'same', 'state': 'COMMENTED'}]
        self.assertIn('review_pending_or_changes_requested', gate.validate(report, 'user_e2e', COMMIT, TREE, state))
        state['reviews'].append({'reviewer': 'same', 'state': 'APPROVED'})
        self.assertEqual([], gate.validate(report, 'user_e2e', COMMIT, TREE, state))

    def test_component_mount_or_injected_login_is_not_a_product_journey(self):
        for change in [{'ui_scope': 'isolated product modal; signup/login not covered'},
                       {'product_journey': {'login_http_status': 200, 'key_list_http_status': 200, 'selected_key_matches': True, 'routes': ['/keys']}}]:
            report, state = evidence(True)
            report['cases'][0]['handoff'].update(change)
            self.assertTrue(gate.validate(report, 'user_e2e', COMMIT, TREE, state))


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


class SnapshotTests(unittest.TestCase):
    def test_retry_maps_old_and_new_attempts_and_keeps_pending_external_checks(self):
        review = {'base': 'e' * 40, 'head': COMMIT, 'status': 'cleared', 'patch_sha256': hashlib.sha256(b'fixed-patch').hexdigest()}
        seen = []
        def api(route, *arguments):
            seen.append(route)
            if route.endswith('/pulls/21'):
                return [{'head': {'sha': COMMIT}, 'merge_commit_sha': None}]
            if '/actions/runs?' in route:
                return [{'workflow_runs': [{'id': 10, 'path': '.github/workflows/backend-ci.yml', 'event': 'pull_request'}]}]
            if '/actions/runs/10/jobs?' in route:
                self.assertIn('filter=all', route)
                return [{'jobs': [{'check_run_url': 'https://api.github.com/check-runs/1'}, {'check_run_url': 'https://api.github.com/check-runs/2'}]}]
            if '/check-runs?' in route:
                base = {'head_sha': COMMIT, 'name': 'test', 'app': {'slug': 'github-actions'}, 'status': 'completed'}
                external = {**base, 'id': 3, 'name': 'external security', 'status': 'in_progress', 'conclusion': None, 'app': {'slug': 'scanner'}}
                return [{'check_runs': [{**base, 'id': 1, 'conclusion': 'failure'}]}, {'check_runs': [{**base, 'id': 2, 'conclusion': 'success'}, external]}]
            if '/statuses?' in route:
                return [[{'context': 'external-status', 'state': 'pending'}]]
            if route.endswith('/reviews?per_page=100'):
                return [[]]
            if route == 'graphql':
                return [{'data': {'repository': {'pullRequest': {'reviewThreads': {'nodes': [{'isResolved': True}]}}}}},
                        {'data': {'repository': {'pullRequest': {'reviewThreads': {'nodes': [{'isResolved': False}]}}}}}]
            raise AssertionError(route)
        with patch.object(github_snapshot, 'api', side_effect=api), patch.object(github_snapshot.subprocess, 'run') as run:
            run.return_value.returncode, run.return_value.stdout = 0, b'fixed-patch'
            state = github_snapshot.collect('owner/repo', 21, COMMIT, review)
        tests = [check for check in state['checks'] if check['name'] == 'test']
        self.assertEqual(1, len(tests))
        self.assertEqual('success', tests[0]['conclusion'])
        self.assertEqual('backend-ci.yml', tests[0]['workflow'])
        self.assertEqual([3], state['outstanding_checks'])
        self.assertEqual(1, state['unresolved_findings'])
        self.assertTrue(any(check['name'] == 'external-status' and check['status'] == 'in_progress' for check in state['checks']))
        self.assertTrue(any('filter=all' in route and '/check-runs?' in route for route in seen))

    def test_review_patch_mismatch_stops_before_github_reads(self):
        with patch.object(github_snapshot.subprocess, 'run') as run, patch.object(github_snapshot, 'api') as api:
            run.return_value.returncode, run.return_value.stdout = 0, b'changed-patch'
            with self.assertRaises(ValueError):
                github_snapshot.collect('owner/repo', 21, COMMIT, {'base': 'e' * 40, 'patch_sha256': DIGEST})
            api.assert_not_called()


class ReleaseBoundaryTests(unittest.TestCase):
    def test_any_target_with_devin_still_requires_acceptance_after_failed_tag(self):
        with patch.object(release_guard, 'git', return_value='') as git:
            self.assertTrue(release_guard.requires_acceptance(COMMIT))
            git.assert_called_once_with('cat-file', '-e', COMMIT + ':backend/internal/service/openai_gateway_devin_fusion.go')
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
