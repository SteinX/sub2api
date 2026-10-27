#!/usr/bin/env python3
"""Consent and durable admission for the real product-preview journey.

This is an operator interlock, not an authorization signature or a provider
spending cap. It never obtains credentials or calls a service. The only real
runner must validate/claim this policy before starting its isolated preview.
"""
import datetime as dt
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import re
import stat
import subprocess
import time

HEX = re.compile(r'^[a-f0-9]{40}$')
UID = re.compile(r'^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$')
STOP_REASONS = frozenset({'tool_error', 'permission_rejection', 'test_failed', 'reader_error', 'parse_error',
    'client_exit', 'protocol_error', 'transport_uncertain', 'control_lost', 'supervisor_failure', 'deadline',
    'missing_terminal', 'source_or_oracle_failure', 'tool_budget_exhausted', 'tool_budget_unavailable'})
TOOL_OPERATIONS = [f'opencode/{kind}/project/{phase}' for kind in ('direct', 'fusion') for phase in ('task', 'resume')]


def validate_tool_budget(plan):
    """Four explicitly approved CLI operations, never inferred from sessions.

    This bounds the implemented real pilot, not the product's client support.
    Further clients/capabilities need their own reviewed finite operation plan.
    """
    expected = {'operations': TOOL_OPERATIONS, 'per_operation': plan['limits']['tools_per_turn'],
                'total': 4 * plan['limits']['tools_per_turn']}
    if (plan.get('clients') != ['opencode'] or plan.get('omp_capabilities', 'false') != 'false'
            or plan.get('tool_budget') != expected):
        raise Denied('explicit_fixed_tool_operation_scope_required')
    return expected


class Denied(ValueError):
    pass


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()).hexdigest()


def read_json(path):
    try:
        with open(path, 'rb') as source:
            raw = source.read(256 * 1024 + 1)
        if len(raw) > 256 * 1024:
            raise Denied('policy_oversized')
        value = json.loads(raw)
        if not isinstance(value, dict): raise Denied('policy_object_required')
        return value
    except (OSError, ValueError, TypeError):
        raise Denied('policy_unavailable_or_invalid') from None


def current_source():
    def git(*args):
        return subprocess.check_output(['git', *args], stderr=subprocess.DEVNULL, timeout=10).decode().strip()
    return {'commit': git('rev-parse', 'HEAD'), 'tree': git('rev-parse', 'HEAD^{tree}'), 'dirty': bool(git('status', '--porcelain'))}


def validate(plan, approval, mode, source, environment, now=None):
    if not approval or approval.get('approved') is not True:
        raise Denied('explicit_approval_missing')
    if not re.fullmatch(r'[A-Za-z0-9_-]{8,80}', str(approval.get('approval_id', ''))):
        raise Denied('stable_approval_id_required')
    if mode not in {'plan', 'live'} or approval.get('mode') != mode:
        raise Denied('approval_mode_mismatch')
    if plan.get('schema_version') != 1 or approval.get('schema_version') != 1:
        raise Denied('unsupported_policy_schema')
    if approval.get('plan_sha256') != digest(plan):
        raise Denied('approved_plan_changed')
    if source.get('dirty') is not False or plan.get('source') != {key: source.get(key) for key in ['commit', 'tree']} or any(not HEX.fullmatch(str(source.get(key, ''))) for key in ['commit', 'tree']):
        raise Denied('reviewed_clean_source_required')
    try:
        expires = dt.datetime.fromisoformat(approval['expires_at'].replace('Z', '+00:00'))
        instant = now or dt.datetime.now(dt.timezone.utc)
        if expires.tzinfo is None or not instant < expires <= instant + dt.timedelta(days=1):
            raise Denied('approval_expired_or_too_long')
    except (ValueError, KeyError, TypeError):
        raise Denied('approval_expired_or_invalid') from None
    if plan.get('upstream_origins') != ['https://server.codeium.com']:
        raise Denied('upstream_destination_not_allowed')
    if plan.get('product') != 'isolated_complete_preview' or plan.get('dataset') != 'synthetic_price_project_v1':
        raise Denied('preview_or_data_scope_not_allowed')
    scopes = approval.get('scopes', [])
    required = {'native_auth', 'temporary_credential_storage', 'local_preview_accounts'}
    if mode == 'live':
        required.add('model_generation')
    if not required.issubset(scopes):
        raise Denied('required_action_scope_not_approved')
    models = plan.get('models', {})
    if set(models) != {'direct', 'lead', 'sidekick'} or any(not UID.fullmatch(str(uid)) or uid.lower().startswith('fusion-') for uid in models.values()):
        raise Denied('exact_physical_model_plan_required')
    clients = plan.get('clients', [])
    if not clients or len(clients) != len(set(clients)) or not set(clients).issubset({'opencode', 'omo', 'omp'}):
        raise Denied('client_scope_not_supported')
    limits = plan.get('limits', {})
    integer_caps = {'public_requests': 144, 'output_tokens': 1024, 'wall_seconds': 3600, 'turn_seconds': 300,
                    'internal_calls_per_turn': 16, 'tools_per_turn': 12, 'handoffs_per_turn': 2}
    for key, cap in integer_caps.items():
        value = limits.get(key)
        if type(value) is not int or not 0 < value <= cap:
            raise Denied('invalid_' + key + '_limit')
    for key, cap in [('local_usd', 3), ('turn_usd', .5)]:
        value = limits.get(key)
        if type(value) not in {float, int} or not math.isfinite(value) or not 0 < value <= cap:
            raise Denied('invalid_' + key + '_limit')
    if mode == 'live':
        validate_tool_budget(plan)
    if limits.get('parallelism') != 1 or plan.get('stop_on_failure') is not True or plan.get('retry_after_uncertain_send') is not False:
        raise Denied('single_concurrency_and_failure_stop_required')
    if not re.fullmatch(r'[a-f0-9]{64}', str(plan.get('reference_pricing_sha256', ''))):
        raise Denied('reviewed_reference_price_snapshot_required')
    if plan.get('omp_capabilities', 'false') not in {'false', 'true'}:
        raise Denied('unsupported_capability_scope')
    pricing_file=environment.get('DEVIN_JOURNEY_GROUP_PRICING_FILE')
    if plan.get('group_pricing_sha256'):
        if not re.fullmatch(r'[a-f0-9]{64}', str(plan['group_pricing_sha256'])) or not pricing_file:
            raise Denied('approved_group_price_snapshot_required')
        try:
            data=Path(pricing_file).read_bytes()
            if len(data)>256*1024 or hashlib.sha256(data).hexdigest()!=plan['group_pricing_sha256'] or not isinstance(json.loads(data),list):
                raise Denied('group_price_snapshot_changed')
        except (OSError,ValueError):
            raise Denied('group_price_snapshot_unavailable_or_changed') from None
    elif pricing_file:
        raise Denied('unapproved_group_price_snapshot')
    facts=plan.get('user_price_facts',[])
    if facts:
        from price_facts import user_confirmed_swe2_rule
        rule,provenance=user_confirmed_swe2_rule('swe-2-high',models.values())
        if facts!=[provenance] or not pricing_file or rule not in json.loads(Path(pricing_file).read_bytes()):
            raise Denied('user_price_fact_requires_exact_approved_zero_rule')
    credential = environment.get('DEVIN_E2E_PAT') or environment.get('DEVIN_E2E_CREDENTIAL')
    if not credential or '\n' in credential or '\r' in credential:
        raise Denied('native_runtime_credential_reference_missing')
    return {'mode': mode, 'plan_sha256': digest(plan), 'limits': limits, 'approval_id': approval['approval_id']}


def claim(path, plan, mode):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    parent = path.parent.stat()
    if parent.st_uid != os.geteuid() or parent.st_mode & 0o077:
        raise Denied('private_owned_ledger_directory_required')
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    except OSError:
        raise Denied('approval_run_already_claimed') from None
    state = {'schema_version': 1, 'plan_sha256': digest(plan), 'mode': mode, 'limits': plan['limits'],
             'requests': 0, 'local_cost_usd': 0, 'inflight': None, 'stopped': False, 'closed': False,
             'source': plan['source'], 'provider_spending_hard_cap': False}
    state['deadline_unix'] = time.time() + plan['limits']['wall_seconds']
    if mode == 'live' and plan.get('tool_budget') is not None:
        state['tool_budget'] = validate_tool_budget(plan)
        state['tool_operations'] = {}
        state['tools_reserved'] = 0
    with os.fdopen(fd, 'w') as out:
        json.dump(state, out, allow_nan=False)
        out.flush(); os.fsync(out.fileno())
    return state


def ledger_action(path, action, receipt=None, outcome=None, cost=None, estimated_cost=None, reason=None, observed_ns=None, expected_scope=None,
                  operation=None, calls=None):
    try:
        fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
    except OSError:
        raise Denied('run_ledger_unavailable') from None
    with os.fdopen(fd, 'r+') as file:
        if not stat.S_ISREG(os.fstat(file.fileno()).st_mode) or os.fstat(file.fileno()).st_mode & 0o077:
            raise Denied('run_ledger_not_private_regular_file')
        fcntl.flock(file, fcntl.LOCK_EX)
        state = json.load(file)
        if expected_scope is not None and any(state.get(key)!=value for key,value in expected_scope.items()):
            raise Denied('run_ledger_scope_or_state_mismatch')
        instant = time.time_ns()
        journal = state.setdefault('journal', [])
        if action in {'tool_begin', 'tool_reserve'}:
            if state['mode'] != 'live' or state['closed'] or state['stopped']:
                raise Denied('run_stopped_or_previous_send_uncertain')
            if time.time() >= state['deadline_unix']:
                raise Denied('approved_wall_time_exhausted')
            budget = state.get('tool_budget', {})
            operations = state.get('tool_operations', {})
            if operation not in budget.get('operations', []):
                raise Denied('unapproved_tool_operation')
            if action == 'tool_begin':
                if operation in operations or state['inflight'] is not None:
                    raise Denied('tool_operation_already_claimed_or_inflight')
                operations[operation] = []
                state['tool_operations'] = operations
            else:
                if (operation not in operations or state['inflight'] is None or not isinstance(calls, list)
                        or not 0 < len(calls) <= 64 or len(set(calls)) != len(calls)
                        or any(not isinstance(value, str) or not re.fullmatch(r'[a-f0-9]{64}', value) for value in calls)):
                    raise Denied('invalid_tool_reservation')
                if any(value in prior for prior in operations.values() for value in calls):
                    raise Denied('reused_tool_identity')
                if (len(operations[operation]) + len(calls) > budget['per_operation']
                        or state['tools_reserved'] + len(calls) > budget['total']):
                    # Persist the stop with the rejecting decision; no partial
                    # reservation and no refund on failure, ACK or cancellation.
                    state['stopped'] = True
                    state.setdefault('first_failure', {'reason': 'tool_budget_exhausted', 'observed_at_ns': instant,
                        'persisted_at_ns': instant, 'requests_at_stop': state['requests'],
                        'tools_reserved_at_stop': state['tools_reserved']})
                    journal.append({'action': 'tool_rejected', 'at_ns': instant, 'operation': operation, 'count': len(calls)})
                else:
                    operations[operation].extend(calls)
                    state['tools_reserved'] += len(calls)
            journal.append({'action': action, 'at_ns': instant, 'operation': operation,
                            'tools_reserved': state['tools_reserved']})
        elif action == 'admit':
            if state['mode'] != 'live' or state['closed'] or state['stopped'] or state['inflight'] is not None:
                raise Denied('run_stopped_or_previous_send_uncertain')
            if time.time() >= state['deadline_unix']:
                raise Denied('approved_wall_time_exhausted')
            if cost is not None:
                if type(cost) not in {float,int} or not math.isfinite(cost) or cost < state['local_cost_usd']:
                    raise Denied('current_local_cost_unavailable')
                state['local_cost_usd'] = cost
            if state['requests'] >= state['limits']['public_requests'] or state['local_cost_usd'] >= state['limits']['local_usd']:
                raise Denied('approved_run_budget_exhausted')
            if estimated_cost is not None:
                if (type(estimated_cost) not in {float,int} or not math.isfinite(estimated_cost) or estimated_cost<0
                        or estimated_cost>state['limits']['turn_usd'] or state['local_cost_usd']+estimated_cost>state['limits']['local_usd']):
                    raise Denied('direct_turn_or_remaining_cost_bound_exceeded')
            state['request_start_cost_usd']=state['local_cost_usd']
            state['requests'] += 1
            state['inflight'] = state['requests']
            journal.append({'action': 'admit', 'at_ns': instant, 'receipt': state['inflight'], 'local_cost_usd': state['local_cost_usd']})
        elif action == 'complete':
            if receipt != state['inflight'] or type(receipt) is not int:
                raise Denied('request_receipt_mismatch')
            completed = state.setdefault('completed_receipts', [])
            if receipt in completed:
                raise Denied('request_already_completed')
            if outcome not in {'success', 'failed', 'uncertain'}:
                raise Denied('invalid_request_outcome')
            if type(cost) not in {float, int} or not math.isfinite(cost) or cost < state['local_cost_usd']:
                outcome = 'uncertain'
            else:
                if cost-state['request_start_cost_usd']>state['limits']['turn_usd']:
                    outcome='failed'
                state['local_cost_usd'] = cost
            state['stopped'] = state['stopped'] or outcome != 'success'
            state['inflight'] = None if outcome == 'success' else receipt
            completed.append(receipt)
            journal.append({'action': 'complete', 'at_ns': instant, 'receipt': receipt, 'outcome': outcome,
                            'local_cost_usd': state['local_cost_usd']})
        elif action == 'stop':
            if reason not in STOP_REASONS or type(observed_ns) is not int or not 0 < observed_ns <= instant:
                raise Denied('invalid_stop_observation')
            state['stopped'] = True
            if 'first_failure' not in state:
                state['first_failure'] = {'reason': reason, 'observed_at_ns': observed_ns, 'persisted_at_ns': instant,
                    'requests_at_stop': state['requests'], 'local_cost_recorded_at_stop_usd': state['local_cost_usd'],
                    'inflight_receipt_at_stop': state['inflight']}
                journal.append({'action': 'stop', 'at_ns': instant, 'reason': reason, 'receipt': state['inflight']})
        elif action == 'close':
            state['closed'] = True
            state['stopped'] = state['stopped'] or state['inflight'] is not None
        else:
            raise Denied('unknown_ledger_action')
        file.seek(0); file.truncate()
        json.dump(state, file, allow_nan=False)
        file.flush(); os.fsync(file.fileno())
        return state
