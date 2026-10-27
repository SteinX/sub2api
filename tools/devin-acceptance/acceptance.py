#!/usr/bin/env python3
"""Validate sanitized acceptance evidence. Mock/client-smoke is never live E2E.

This is an audit gate for trusted runner evidence, not a signature scheme or a
replacement for repository permissions. It never invokes clients or providers.
"""
import argparse
import base64
from collections import Counter
import json
import math
from pathlib import Path
import re
import sys

HEX40 = re.compile(r'^[a-f0-9]{40}$')
HEX64 = re.compile(r'^[a-f0-9]{64}$')
PREFIX = 'Sub2API-Devin-Acceptance-v1: '
CASE_IDS = {f'{client}/{kind}/{profile}' for client in ['opencode', 'omo', 'omp'] for kind in ['direct', 'fusion']
            for profile in (['default', 'compatibility'] if client == 'omp' else ['project'])}
CONDITIONAL_CHECKS = {'cla-check', 'cla-lock', 'live', 'sync'}
REQUIRED_CHECKS = {
    ('backend-ci.yml', name) for name in ['shell', 'test', 'frontend', 'golangci-lint', 'release-helpers']
} | {('security-scan.yml', name) for name in ['backend-security', 'frontend-security']} | {
    ('devin-e2e.yml', name) for name in ['mock', 'clients-mock', 'user-journeys-mock']
}
CLIENT_PINS = {'opencode': ('opencode-ai', '1.19.4'), 'omo': ('oh-my-opencode', '5.1.44'),
               'omp': ('@oh-my-pi/pi-coding-agent', '18.12.6')}
PRODUCT_UI_SCOPE = 'full product application: login, key list, selection and configuration copy'
TOOL_NAME = re.compile(r'^[A-Za-z0-9_.:-]{1,128}$')
FILE_MUTATORS = {'write', 'edit', 'apply_patch'}
SECRET_KEYS = {'api_key', 'apikey', 'authorization', 'access_token', 'refresh_token', 'pat', 'cookie', 'raw_log', 'prompt', 'content',
               'account_id', 'group_id', 'channel_id', 'user_id', 'session_id', 'email', 'stdout', 'stderr', 'request_body', 'response_body',
               'command', 'raw_command', 'tool_input', 'tool_output', 'brief'}


def read_report(path):
    raw = Path(path).read_bytes()
    if len(raw) > 512 * 1024:
        raise ValueError('oversized acceptance evidence')
    return json.loads(raw)


def private_fields(value):
    if isinstance(value, dict):
        return any(str(key).lower() in SECRET_KEYS or private_fields(item) for key, item in value.items())
    if isinstance(value, list):
        return any(private_fields(item) for item in value)
    return False


def valid_tool_evidence(result):
    tools, states, events = (result.get(key) for key in ['tools', 'tool_states', 'tool_events'])
    if (not all(isinstance(value, dict) for value in [tools, states, events])
            or type(result.get('incomplete_tool_calls')) is not int or result['incomplete_tool_calls'] != 0):
        return False
    if any(not isinstance(name, str) or not TOOL_NAME.fullmatch(name) or type(count) is not int or count < 0 for name, count in tools.items()):
        return False
    for collection in [states, events]:
        for name, counts in collection.items():
            if (not isinstance(name, str) or not TOOL_NAME.fullmatch(name) or not isinstance(counts, dict) or not counts
                    or any(status not in {'pending', 'running', 'completed', 'error'} or type(count) is not int or count < 0 for status, count in counts.items())):
                return False
            if counts.get('error', 0):
                return False
    if set(states) != set(events) or set(states) != {name for name, count in tools.items() if count}:
        return False
    calls = result.get('tool_calls')
    if (not isinstance(calls, list) or any(not isinstance(call, dict) or not HEX64.fullmatch(str(call.get('call_sha256', ''))) for call in calls)
            or len({call['call_sha256'] for call in calls}) != len(calls)):
        return False
    completed = Counter()
    mutations = Counter()
    for call in calls:
        if (not HEX64.fullmatch(str(call.get('call_sha256', ''))) or call.get('name') not in tools
                or call.get('status') != 'completed' or not isinstance(call.get('states'), list) or not call['states']
                or call['states'][-1] != 'completed' or any(status not in {'pending', 'running', 'completed'} for status in call['states'])):
            return False
        completed[call['name']] += 1
        if type(call.get('source_targeted')) is not bool:
            return False
        if call['source_targeted']:
            if call['name'] not in FILE_MUTATORS:
                return False
            mutations[call['name']] += 1
    mutation_stats = result.get('source_mutation_tools')
    return (all(completed[name] == count for name, count in tools.items())
            and isinstance(mutation_stats, dict)
            and all(name in FILE_MUTATORS and type(count) is int and count > 0 for name, count in mutation_stats.items())
            and dict(mutations) == mutation_stats
            and all(counts.get('completed', 0) == tools[name] and not counts.get('pending', 0) and not counts.get('running', 0)
                    and events[name].get('completed', 0) >= tools[name] for name, counts in states.items()))


def valid_client_test_receipt(case):
    task = case.get('task', {})
    if not valid_tool_evidence(task):
        return False
    execution = task.get('test_execution', {})
    if not isinstance(execution, dict):
        return False
    calls = execution.get('calls')
    if (execution.get('schema') != 'devin_project_test_v1' or execution.get('passed') is not True
            or execution.get('conflicting_evidence') is not False or not isinstance(calls, list) or not calls
            or any(not isinstance(call, dict) or not HEX64.fullmatch(str(call.get('call_sha256', ''))) for call in calls)
            or execution.get('source_after_task_sha256') != case.get('source_after_task_sha256')):
        return False
    actual = {call['call_sha256']: call for call in task.get('tool_calls', []) if call.get('name') == 'bash'}
    if len(calls) != len(actual) or len({call.get('call_sha256') for call in calls}) != len(calls):
        return False
    for call in calls:
        tool = actual.get(call.get('call_sha256'), {})
        if (call.get('state') != tool.get('status') or call.get('command_class') != tool.get('command_class')
                or call.get('command_sha256') != tool.get('command_sha256') or not HEX64.fullmatch(str(call.get('command_sha256', '')))
                or 'test_runner_candidate' in call and type(call['test_runner_candidate']) is not bool
                or type(call.get('completion_sequence')) is not int or call['completion_sequence'] <= 0):
            return False
    candidates = [call for call in calls if call.get('test_runner_candidate', call.get('command_class') == 'configured_project_tests') is True]
    selected = max(candidates, key=lambda call: call['completion_sequence'], default={})
    protected = case.get('protected_project_sha256', {})
    if (execution.get('selected_call_sha256') != selected.get('call_sha256') or selected.get('execution_verified') is not True
            or selected.get('receipt_status') != 'verified' or selected.get('passed') is not True or type(selected.get('exit_code')) is not int or selected['exit_code'] != 0
            or selected.get('state') != 'completed' or selected.get('exit_source') not in {'client_metadata', 'omp_success_and_runner_receipt'}
            or not all(HEX64.fullmatch(str(selected.get(key, ''))) for key in ['receipt_sha256', 'run_sha256'])
            or selected.get('output_reference_sha256') != selected.get('receipt_sha256')
            or selected.get('project_nonce_sha256') != case.get('project_nonce_sha256')
            or selected.get('source_before_sha256') != case.get('source_after_task_sha256')
            or selected.get('source_after_sha256') != case.get('source_after_task_sha256')
            or not isinstance(protected, dict) or set(protected) != ({'requirements.json', 'test_project.py', 'run_tests.py'} |
                ({f'context/part{index:02d}.txt' for index in range(8)} if case.get('project_kind')=='long' else set()))
            or any(not HEX64.fullmatch(str(value)) for value in protected.values()) or selected.get('protected_sha256') != protected
            or type(selected.get('tests_run')) is not int or selected['tests_run'] <= 0
            or any(type(selected.get(key)) is not int or selected[key] != 0 for key in ['failures', 'errors', 'skipped', 'expected_failures', 'unexpected_successes'])):
        return False
    stamps = [task.get('client_started_ns'), selected.get('started_ns'), selected.get('finished_ns'), task.get('client_finished_ns')]
    return all(type(value) is int and value > 0 for value in stamps) and stamps == sorted(stamps)


def valid_fusion_audit(case):
    audit = case.get('fusion_execution', {})
    if (audit.get('schema_version') != 1 or audit.get('available') is not True or audit.get('complete') is not True
            or audit.get('missing_session_headers') != 0 or not isinstance(audit.get('sessions'), list) or not audit['sessions']):
        return False
    sessions, attempts = set(), set()
    def amount(value):
        return type(value) in {int, float} and math.isfinite(value) and value >= 0
    for session in audit['sessions']:
        fingerprint = session.get('session_sha256')
        if (not HEX64.fullmatch(str(fingerprint)) or fingerprint in sessions or session.get('available') is not True
                or session.get('complete') is not True or session.get('state') != 'TURN_COMPLETED' or session.get('role') != 'lead'
                or session.get('separate_contexts') is not True or session.get('role_usage_settled') is not True):
            return False
        sessions.add(fingerprint)
        handoffs = [item for item in session.get('handoffs', []) if item.get('turn') == session.get('turns')]
        if not handoffs or any(item.get('report_present') is not True or item.get('lead_report_linked') is not True
                               or type(item.get('report_bytes')) is not int or item['report_bytes'] <= 0
                               or not all(HEX64.fullmatch(str(item.get(key))) for key in ['handoff_sha256', 'report_sha256']) for item in handoffs):
            return False
        roles = Counter()
        for attempt in session.get('attempts', []):
            identity, role = attempt.get('attempt_sha256'), attempt.get('role')
            if (not HEX64.fullmatch(str(identity)) or identity in attempts or role not in {'lead', 'sidekick'}
                    or attempt.get('model_uid') != case.get('physical_' + role + '_uid') or attempt.get('status') != 'completed'
                    or any(attempt.get(field) is not True for field in ['settled', 'applied', 'usage_query_available', 'usage_available', 'usage_matches'])
                    or attempt.get('failure_present') is not False or attempt.get('usage_rows') != 1
                    or any(not amount(attempt.get(field)) for field in ['cost_usd', 'actual_cost_usd', 'total_cost_usd'])
                    or abs(attempt['cost_usd'] - max(attempt['actual_cost_usd'], attempt['total_cost_usd'])) > 1e-9
                    or any(type(attempt.get(field)) is not int or attempt[field] < 0 for field in ['input_tokens', 'output_tokens', 'cache_creation_tokens', 'cache_read_tokens'])):
                return False
            attempts.add(identity)
            if attempt.get('purpose') == 'inference':
                roles[role] += 1
            elif attempt.get('purpose') not in {'compaction', 'cache_refresh'}:
                return False
        review = session.get('lead_review', {})
        if (roles.get('lead', 0) < 2 or roles.get('sidekick', 0) < 1 or dict(roles) != session.get('role_attempts')
                or review.get('state') != 'completed_with_report_context' or review.get('current_turn') != session.get('turns')
                or type(review.get('final_request_index')) is not int or review['final_request_index'] < 0
                or type(review.get('final_answer_bytes')) is not int or review['final_answer_bytes'] <= 0
                or not HEX64.fullmatch(str(review.get('final_answer_sha256')))
                or any(review.get(field) is not True for field in ['report_context_linked', 'sidekick_then_lead', 'request_boundaries_valid'])):
            return False
    return True


def validate(report, mode, commit, tree, state=None):
    errors = []
    if report.get('schema_version') != 1:
        errors.append('unsupported_schema')
    if private_fields(report):
        errors.append('private_fields_present')
    source = report.get('source', {})
    if not HEX40.fullmatch(str(commit)) or not HEX40.fullmatch(str(tree)):
        errors.append('invalid_expected_source')
    if source.get('commit') != commit or source.get('tree') != tree:
        errors.append('source_mismatch')
    for field in ['backend_sha256', 'ui_bundle_sha256', 'client_lock_sha256']:
        if not HEX64.fullmatch(str(source.get(field, ''))):
            errors.append('missing_' + field)
    cases = report.get('cases', [])
    if not isinstance(cases, list):
        return errors + ['invalid_case_list']
    ids = [case.get('id') for case in cases]
    if len(ids) != len(set(ids)):
        errors.append('duplicate_cases')
    if set(ids) != CASE_IDS:
        errors.append('incomplete_case_matrix')
    for case in cases:
        ident = str(case.get('id'))
        parts = ident.split('/')
        if len(parts) != 3 or case.get('model_kind') != parts[1]:
            errors.append(ident + ':model_kind_mismatch')
        if mode == 'user_e2e' and (case.get('tier') != 'user_e2e' or case.get('eligible_for_real_e2e') is not True):
            errors.append(ident + ':case_not_real_e2e_eligible')
        refusal = case.get('handoff', {})
        product = refusal.get('product_journey', {})
        if (refusal.get('ui_scope') != PRODUCT_UI_SCOPE or product.get('login_http_status') != 200
                or product.get('key_list_http_status') != 200 or product.get('selected_key_matches') is not True
                or product.get('routes') != ['/login', '/dashboard', '/keys']):
            errors.append(ident + ':complete_product_route_missing')
        if (mode == 'client_mock' and ident == 'omp/fusion/default' and case.get('status') == 'unsupported_by_product'
                and refusal.get('blocked') is True and refusal.get('source') == 'UseKeyModal browser refusal'
                and refusal.get('reason') == 'omp_default_fusion_history_pruning' and refusal.get('files') == []
                and case.get('public_requests') == 0 and not case.get('observed_physical_models')):
            continue  # A successful product refusal is not a completed client task.
        if case.get('status') != 'passed':
            errors.append(ident + ':task_not_passed')
            continue
        guard=case.get('batch_guard') or {}
        if (guard.get('installed') is not True or guard.get('stopped') is not False
                or guard.get('response_observation') not in {'bounded_response_buffer','original_stream_with_source_proof'}
                or type(guard.get('buffered_transport')) is not bool
                or guard.get('buffered_transport') != (guard.get('response_observation') == 'bounded_response_buffer')
                or guard.get('phase_active') is not False or guard.get('public_inflight') is not False
                or guard.get('pending_tools') != 0 or guard.get('first_failure') is not None):
            errors.append(ident + ':first_error_guard_unverified')
        if mode=='user_e2e' and guard.get('buffered_transport') is True:
            errors.append(ident + ':unbuffered_transport_not_verified')
        for key in ['initial_project_test_failed', 'protected_project_files_intact', 'artifact_verified_before_oracle', 'artifact_verified_after_resume',
                    'source_unchanged_after_resume', 'independent_project_test_passed', 'same_client_session']:
            if case.get(key) is not True:
                errors.append(ident + ':' + key)
        if case.get('independent_project_test_executed') is not True:
            errors.append(ident + ':independent_oracle_not_executed')
        nonce = case.get('project_nonce_sha256')
        if not HEX64.fullmatch(str(nonce or '')):
            errors.append(ident + ':project_nonce_unverified')
        if not HEX64.fullmatch(str(case.get('artifact_sha256', ''))):
            errors.append(ident + ':artifact_not_bound')
        handoff = case.get('handoff', {})
        if handoff.get('passed') is not True or handoff.get('source') != 'UseKeyModal browser copy buttons':
            errors.append(ident + ':product_handoff_missing')
        for file in handoff.get('files', []):
            if not HEX64.fullmatch(str(file.get('sha256', ''))):
                errors.append(ident + ':config_not_fingerprinted')
        if not handoff.get('files') or not case.get('public_model_uid') or handoff.get('selected_model') != case.get('public_model_uid'):
            errors.append(ident + ':configuration_missing')
        client = ident.split('/')[0]
        identity = case.get('client_identity', {})
        if ((identity.get('package'), identity.get('version')) != CLIENT_PINS.get(client)
                or not HEX64.fullmatch(str(identity.get('executable_sha256', ''))) or not identity.get('engine_version')):
            errors.append(ident + ':client_identity_unverified')
        lead, sidekick = case.get('physical_lead_uid'), case.get('physical_sidekick_uid')
        physical = case.get('observed_physical_models', {})
        if not lead or physical.get(lead, 0) < 1 or case.get('public_requests', 0) < 1:
            errors.append(ident + ':physical_calls_unverified')
        if case.get('model_kind') == 'direct' and sidekick:
            errors.append(ident + ':unexpected_sidekick')
        if case.get('model_kind') == 'fusion':
            if not valid_fusion_audit(case):
                errors.append(ident + ':fusion_collaboration_unverified')
            contexts = case.get('native_context_hashes', {})
            lead_contexts, sidekick_contexts = set(contexts.get('lead', {})), set(contexts.get('sidekick', {}))
            if (not sidekick or physical.get(sidekick, 0) < 1 or not lead_contexts or not sidekick_contexts
                    or lead_contexts & sidekick_contexts):
                errors.append(ident + ':fusion_roles_unverified')
        for phase in ['task', 'resume']:
            result = case.get(phase) or {}
            if (result.get('completion_contract') != 'project_results_v3' or result.get('terminal_event_seen') is not True
                    or result.get('reader_completed') is not True or result.get('reader_error') is not False
                    or type(result.get('final_text_bytes')) is not int or result['final_text_bytes'] <= 0
                    or not HEX64.fullmatch(str(result.get('final_text_sha256', '')))
                    or result.get('project_nonce_sha256') != nonce):
                errors.append(ident + ':' + phase + '_completion_unverified')
            if not valid_tool_evidence(result):
                errors.append(ident + ':' + phase + '_tool_evidence_invalid')
            if client=='omo' and result.get('plugin_runtime_started') is not True:
                errors.append(ident+':'+phase+'_omo_plugin_not_observed')
            if (result.get('exit_code') != 0 or result.get('normal_stop') is not True or result.get('event_error') is not False
                    or result.get('timed_out') is not False
                    or result.get('parse_errors') != 0 or result.get('session_changed') is not False
                    or result.get('session_seen') is not True or not HEX64.fullmatch(str(result.get('client_session_sha256', '')))):
                errors.append(ident + ':' + phase + '_terminal_failure')
        if case.get('task', {}).get('client_session_sha256') != (case.get('resume') or {}).get('client_session_sha256'):
            errors.append(ident + ':session_identity_mismatch')
        before, after = case.get('source_before_task_sha256'), case.get('source_after_task_sha256')
        if (not HEX64.fullmatch(str(before or '')) or not HEX64.fullmatch(str(after or '')) or before == after
                or case.get('source_changed_by_task') is not True):
            errors.append(ident + ':source_mutation_unverified')
        if (case.get('task', {}).get('client_tests_succeeded') is not True or case.get('task', {}).get('test_command_verified') is not True
                or case.get('task', {}).get('bash_exit_code') != 0):
            errors.append(ident + ':client_test_execution_unverified')
        if not valid_client_test_receipt(case):
            errors.append(ident + ':client_test_receipt_unverified')
        if case.get('capabilities_required') is True:
            extended = case.get('capability_evidence', {})
            if extended.get('status') != 'passed' or any(extended.get(name) != 'passed' for name in ['context_pressure', 'compact', 'fork', 'idle_process_recovery']):
                errors.append(ident + ':required_client_capabilities_failed')
        if case.get('capabilities_scope') == 'fork_pressure_idle_only':
            extended = case.get('capability_evidence', {})
            if (extended.get('status') != 'partial' or extended.get('compact') != 'not_run' or extended.get('cleanup_passed') is not True
                    or case.get('native_context_max_bytes', 0) < 131072
                    or any(extended.get(name) != 'passed' for name in ['context_pressure', 'fork', 'idle_process_recovery'])):
                errors.append(ident + ':independent_client_capabilities_failed')
    if mode == 'client_mock':
        if report.get('tier') != 'client_mock' or report.get('real_upstream') is not False or report.get('eligible_for_real_e2e') is not False:
            errors.append('mock_tier_misrepresented')
        return errors
    if report.get('tier') != 'user_e2e' or report.get('real_upstream') is not True or report.get('eligible_for_real_e2e') is not True:
        errors.append('real_upstream_not_verified')
    if source.get('dirty') is not False:
        errors.append('dirty_source')
    for case in cases:
        if case.get('handoff', {}).get('ui_scope') != PRODUCT_UI_SCOPE:
            errors.append(str(case.get('id')) + ':deployed_product_journey_not_verified')
        for capability in ['context_pressure', 'compact', 'fork', 'worker', 'disconnect_recovery']:
            if case.get(capability) != 'passed':
                errors.append(str(case.get('id')) + ':' + capability + '_not_verified')
        pricing = case.get('pricing_contract', {})
        expected_uids = {case.get('physical_lead_uid')}
        if case.get('model_kind') == 'fusion':
            expected_uids.add(case.get('physical_sidekick_uid'))
        if (pricing.get('identified') is not True or set(pricing.get('physical_model_uids', [])) != expected_uids
                or not HEX64.fullmatch(str(pricing.get('snapshot_sha256', ''))) or pricing.get('public_key_route_verified') is not True
                or not HEX64.fullmatch(str(pricing.get('group_config_sha256', '')))
                or any(pricing.get('sources', {}).get(uid) not in {'group', 'channel', 'catalog_exact', 'catalog_canonical', 'builtin_exact'} for uid in expected_uids)):
            errors.append(str(case.get('id')) + ':physical_pricing_contract_unverified')
        if (not case.get('client_identity') or not case.get('physical_lead_uid')
                or (case.get('model_kind') == 'fusion' and not case.get('physical_sidekick_uid'))):
            errors.append(str(case.get('id')) + ':identity_or_pricing_missing')
    if report.get('unverified'):
        errors.append('unverified_required_scope')
    if (state is None or state.get('commit') != commit or state.get('snapshot_complete') is not True
            or not isinstance(state.get('pr'), int) or state['pr'] <= 0):
        return errors + ['review_security_state_unavailable']
    checks = state.get('checks', [])
    if not checks:
        errors.append('required_checks_missing')
    observed = {(check.get('workflow'), check.get('name')) for check in checks}
    for workflow, name in REQUIRED_CHECKS - observed:
        errors.append('required_check_missing:' + workflow + '/' + name)
    if state.get('outstanding_checks'):
        errors.append('started_check_pending')
    for check in checks:
        if check.get('head_sha') != commit:
            errors.append('check_source_mismatch')
        if check.get('status') != 'completed':
            errors.append('started_check_pending:' + str(check.get('name')))
        elif check.get('conclusion') != 'success' and not (check.get('conclusion') == 'skipped' and check.get('name') in CONDITIONAL_CHECKS):
            errors.append('check_not_successful:' + str(check.get('name')))
    independent = state.get('independent_review', {})
    if (independent.get('head') != commit or independent.get('status') != 'cleared'
            or not HEX40.fullmatch(str(independent.get('base', ''))) or not HEX64.fullmatch(str(independent.get('patch_sha256', '')))):
        errors.append('independent_review_not_cleared')
    latest_reviews = {}
    for review in state.get('reviews', []):
        if review.get('state') in {'APPROVED', 'CHANGES_REQUESTED', 'DISMISSED'}:
            latest_reviews[review.get('reviewer')] = review.get('state')
        elif review.get('state') == 'PENDING':
            errors.append('review_pending_or_changes_requested')
    if any(value in {'CHANGES_REQUESTED', 'PENDING'} for value in latest_reviews.values()):
        errors.append('review_pending_or_changes_requested')
    if state.get('unresolved_findings') != 0:
        errors.append('security_or_review_findings_unresolved')
    return errors


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('action', choices=['verify', 'tag-note'])
    parser.add_argument('--report', required=True)
    parser.add_argument('--mode', choices=['client_mock', 'user_e2e'], default='user_e2e')
    parser.add_argument('--commit', required=True)
    parser.add_argument('--tree', required=True)
    parser.add_argument('--state')
    args = parser.parse_args()
    report = read_report(args.report)
    state = read_report(args.state) if args.state else None
    errors = validate(report, args.mode, args.commit, args.tree, state)
    if errors:
        print(json.dumps({'accepted': False, 'mode': args.mode, 'reasons': sorted(set(errors))}))
        return 1
    if args.action == 'tag-note':
        if args.mode != 'user_e2e':
            raise ValueError('simulated evidence cannot authorize release')
        payload = {'report': report, 'prerequisites': state}
        encoded = base64.b64encode(json.dumps(payload, separators=(',', ':'), sort_keys=True).encode()).decode()
        print(PREFIX + encoded)
    else:
        print(json.dumps({'accepted': True, 'mode': args.mode, 'real_e2e': args.mode == 'user_e2e'}))
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (ValueError, OSError, KeyError, TypeError):
        print(json.dumps({'accepted': False, 'reason': 'invalid_evidence'}))
        raise SystemExit(2)
