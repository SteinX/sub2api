#!/usr/bin/env python3
"""Validate sanitized acceptance evidence. Mock/client-smoke is never live E2E.

This is an audit gate for trusted runner evidence, not a signature scheme or a
replacement for repository permissions. It never invokes clients or providers.
"""
import argparse
import base64
import json
from pathlib import Path
import re
import subprocess
import sys

HEX40 = re.compile(r'^[a-f0-9]{40}$')
HEX64 = re.compile(r'^[a-f0-9]{64}$')
PREFIX = 'Sub2API-Devin-Acceptance-v1: '
CASE_IDS = {f'{client}/{kind}/{profile}' for client in ['opencode', 'omo', 'omp'] for kind in ['direct', 'fusion']
            for profile in (['default', 'compatibility'] if client == 'omp' else ['project'])}
CONDITIONAL_CHECKS = {'cla-check', 'cla-lock', 'live', 'sync'}
SECRET_KEYS = {'api_key', 'apikey', 'authorization', 'access_token', 'refresh_token', 'pat', 'cookie', 'raw_log', 'prompt', 'content'}


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
        known_failure = (mode == 'client_mock' and ident == 'omp/fusion/default'
                         and case.get('status') == 'failed'
                         and case.get('error_codes', {}).get('devin_fusion_history_conflict', 0) > 0)
        if known_failure:
            continue  # retained FAILED observation, never a passing E2E case
        if case.get('status') != 'passed':
            errors.append(ident + ':task_not_passed')
            continue
        for key in ['initial_project_test_failed', 'protected_project_files_intact', 'artifact_verified_before_oracle', 'independent_project_test_passed', 'same_client_session']:
            if case.get(key) is not True:
                errors.append(ident + ':' + key)
        if not HEX64.fullmatch(str(case.get('artifact_sha256', ''))):
            errors.append(ident + ':artifact_not_bound')
        handoff = case.get('handoff', {})
        if handoff.get('passed') is not True or handoff.get('source') != 'UseKeyModal browser copy buttons':
            errors.append(ident + ':product_handoff_missing')
        for file in handoff.get('files', []):
            if not HEX64.fullmatch(str(file.get('sha256', ''))):
                errors.append(ident + ':config_not_fingerprinted')
        if not handoff.get('files') or handoff.get('selected_model') is None:
            errors.append(ident + ':configuration_missing')
        for phase in ['task', 'resume']:
            result = case.get(phase) or {}
            if result.get('exit_code') != 0 or result.get('normal_stop') is not True or result.get('event_error') or result.get('timed_out') or result.get('final_text_matches') is not True:
                errors.append(ident + ':' + phase + '_terminal_failure')
        tools = case.get('task', {}).get('tools', {})
        if tools.get('read', 0) < 3 or tools.get('write', 0) < 1 or tools.get('bash', 0) < 1 or case.get('task', {}).get('client_tests_succeeded') is not True:
            errors.append(ident + ':actual_client_tools_missing')
    if mode == 'client_mock':
        if report.get('tier') != 'client_mock' or report.get('real_upstream') is not False or report.get('eligible_for_real_e2e') is not False:
            errors.append('mock_tier_misrepresented')
        return errors
    if report.get('tier') != 'user_e2e' or report.get('real_upstream') is not True:
        errors.append('real_upstream_not_verified')
    if source.get('dirty') is not False:
        errors.append('dirty_source')
    for case in cases:
        if case.get('handoff', {}).get('ui_scope') != 'deployed product Use Key journey':
            errors.append(str(case.get('id')) + ':deployed_product_journey_not_verified')
        for capability in ['context_pressure', 'compact', 'fork', 'worker', 'disconnect_recovery']:
            if case.get(capability) != 'passed':
                errors.append(str(case.get('id')) + ':' + capability + '_not_verified')
        if not case.get('client_identity') or not case.get('pricing_contract') or not case.get('physical_lead_uid') or not case.get('physical_sidekick_uid'):
            errors.append(str(case.get('id')) + ':identity_or_pricing_missing')
    if report.get('unverified'):
        errors.append('unverified_required_scope')
    if state is None or state.get('commit') != commit:
        return errors + ['review_security_state_unavailable']
    checks = state.get('checks', [])
    if not checks:
        errors.append('required_checks_missing')
    for check in checks:
        if check.get('head_sha') != commit:
            errors.append('check_source_mismatch')
        if check.get('status') != 'completed':
            errors.append('started_check_pending:' + str(check.get('name')))
        elif check.get('conclusion') != 'success' and not (check.get('conclusion') == 'skipped' and check.get('name') in CONDITIONAL_CHECKS):
            errors.append('check_not_successful:' + str(check.get('name')))
    independent = state.get('independent_review', {})
    if independent.get('head') != commit or independent.get('status') != 'cleared' or not HEX64.fullmatch(str(independent.get('patch_sha256', ''))):
        errors.append('independent_review_not_cleared')
    latest_reviews = {}
    for review in state.get('reviews', []):
        latest_reviews[review.get('reviewer')] = review.get('state')
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
