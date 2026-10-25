#!/usr/bin/env python3
"""Offline reassessment of existing sanitized Go test events; never calls a service."""
import argparse
import json
from pathlib import Path
import re

CLIENTS = ['opencode', 'omo', 'omp']
KINDS = ['direct', 'fusion']
CASES = ['text_and_streaming', 'client_read_and_result', 'continue', 'continue_after_backend_restart', 'fresh_session', 'interrupt_before_upstream_send', 'one_retry_after_confirmed_unsent']
PATTERN = re.compile(r'client=(opencode|omo|omp) action=(text|tool|continue) exit=(-?\d+) frames=(\d+) completed_read=(\d+) expected_text=(true|false) interrupted_before_forward=(true|false)')


def reassess(directories):
    attempts = {}
    setup = 0
    for directory in directories:
        records = {}
        for line in (directory / 'results.jsonl').read_text().splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            parts = str(event.get('Test', '')).split('/')
            if len(parts) != 4 or parts[1] not in CLIENTS or parts[2] not in KINDS:
                continue
            name = '/'.join(parts[1:])
            if parts[3] not in CASES:
                if event.get('Action') == 'pass' and parts[3].startswith('recovery_setup_'):
                    setup += 1
                continue
            record = records.setdefault(name, {'source': directory.name, 'test_status': None, 'processes': []})
            if event.get('Action') in {'pass', 'fail', 'skip'}:
                record['test_status'] = event['Action']
            match = PATTERN.search(event.get('Output', ''))
            if match:
                record['processes'].append({'exit_code': int(match[3]), 'frames': int(match[4]), 'completed_read': int(match[5]),
                                            'expected_marker': match[6] == 'true', 'interrupted_before_forward': match[7] == 'true'})
        for name, record in records.items():
            attempts.setdefault(name, []).append(record)
    result = []
    for client in CLIENTS:
        for kind in KINDS:
            for case in CASES:
                name = '/'.join([client, kind, case])
                history = attempts.get(name, [])
                passed = [attempt for attempt in history if attempt['test_status'] == 'pass']
                reusable_cancel = case == 'interrupt_before_upstream_send' and any(
                    process.get('interrupted_before_forward') for attempt in passed for process in attempt['processes'])
                result.append({'id': name, 'historical_contract_pass_recorded': bool(passed),
                               'terminal_state_available': False, 'product_ui_config_available': False,
                               'reusable_scope': 'recorded cancellation-before-forward contract' if reusable_cancel else 'historical process/tool/routing counts only',
                               'task_completion_needs_new_evidence': not reusable_cancel,
                               'user_e2e_certifiable': False,
                               'attempts': history})
    return {'schema_version': 1, 'tier': 'legacy_reassessment', 'network_requests': 0, 'generation_requests': 0,
            'canonical_cases': len(result), 'historical_contract_passes': sum(r['historical_contract_pass_recorded'] for r in result),
            'reusable_cancel_contracts': sum(r['reusable_scope'].startswith('recorded cancellation') for r in result),
            'task_completion_revalidation_required': sum(r['task_completion_needs_new_evidence'] for r in result),
            'user_e2e_certifiable_cases': 0, 'excluded_recovery_setup_passes': setup,
            'retained_failed_attempts': sum(attempt['test_status'] == 'fail' for history in attempts.values() for attempt in history),
            'reason': 'Old runner discarded raw client terminal events and generated its own configurations; missing evidence is not a pass or an observed task failure.',
            'cases': result}


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('directories', nargs='+', type=Path)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    report = reassess(args.directories)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2) + '\n')
    args.output.chmod(0o600)
    print(json.dumps({key: value for key, value in report.items() if key != 'cases'}))
