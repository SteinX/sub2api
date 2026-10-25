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

HEX = re.compile(r'^[a-f0-9]{40}$')
UID = re.compile(r'^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$')


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
        return json.loads(raw)
    except (OSError, ValueError, TypeError):
        raise Denied('policy_unavailable_or_invalid') from None


def current_source():
    def git(*args):
        return subprocess.check_output(['git', *args], stderr=subprocess.DEVNULL, timeout=10).decode().strip()
    return {'commit': git('rev-parse', 'HEAD'), 'tree': git('rev-parse', 'HEAD^{tree}'), 'dirty': bool(git('status', '--porcelain'))}


def validate(plan, approval, mode, source, environment, now=None):
    if not approval or approval.get('approved') is not True:
        raise Denied('explicit_approval_missing')
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
                    'internal_calls_per_turn': 64, 'tools_per_turn': 20, 'handoffs_per_turn': 2}
    for key, cap in integer_caps.items():
        value = limits.get(key)
        if type(value) is not int or not 0 < value <= cap:
            raise Denied('invalid_' + key + '_limit')
    for key, cap in [('local_usd', 3), ('turn_usd', .5)]:
        value = limits.get(key)
        if type(value) not in {float, int} or not math.isfinite(value) or not 0 < value <= cap:
            raise Denied('invalid_' + key + '_limit')
    if limits.get('parallelism') != 1 or plan.get('stop_on_failure') is not True or plan.get('retry_after_uncertain_send') is not False:
        raise Denied('single_concurrency_and_failure_stop_required')
    if not re.fullmatch(r'[a-f0-9]{64}', str(plan.get('reference_pricing_sha256', ''))):
        raise Denied('reviewed_reference_price_snapshot_required')
    credential = environment.get('DEVIN_E2E_PAT') or environment.get('DEVIN_E2E_CREDENTIAL')
    if not credential or '\n' in credential or '\r' in credential:
        raise Denied('native_runtime_credential_reference_missing')
    return {'mode': mode, 'plan_sha256': digest(plan), 'limits': limits}


def claim(path, plan, mode):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    except OSError:
        raise Denied('approval_run_already_claimed') from None
    state = {'schema_version': 1, 'plan_sha256': digest(plan), 'mode': mode, 'limits': plan['limits'],
             'requests': 0, 'local_cost_usd': 0, 'inflight': None, 'stopped': False, 'closed': False,
             'source': plan['source'], 'provider_spending_hard_cap': False}
    with os.fdopen(fd, 'w') as out:
        json.dump(state, out, allow_nan=False)
        out.flush(); os.fsync(out.fileno())
    return state


def ledger_action(path, action, receipt=None, outcome=None, cost=None):
    try:
        fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
    except OSError:
        raise Denied('run_ledger_unavailable') from None
    with os.fdopen(fd, 'r+') as file:
        if not stat.S_ISREG(os.fstat(file.fileno()).st_mode) or os.fstat(file.fileno()).st_mode & 0o077:
            raise Denied('run_ledger_not_private_regular_file')
        fcntl.flock(file, fcntl.LOCK_EX)
        state = json.load(file)
        if action == 'admit':
            if state['mode'] != 'live' or state['closed'] or state['stopped'] or state['inflight'] is not None:
                raise Denied('run_stopped_or_previous_send_uncertain')
            if state['requests'] >= state['limits']['public_requests'] or state['local_cost_usd'] >= state['limits']['local_usd']:
                raise Denied('approved_run_budget_exhausted')
            state['requests'] += 1
            state['inflight'] = state['requests']
        elif action == 'complete':
            if receipt != state['inflight'] or type(receipt) is not int:
                raise Denied('request_receipt_mismatch')
            if type(cost) not in {float, int} or not math.isfinite(cost) or cost < state['local_cost_usd']:
                outcome = 'uncertain'
            else:
                state['local_cost_usd'] = cost
            state['stopped'] = state['stopped'] or outcome != 'success'
            state['inflight'] = None if outcome == 'success' else receipt
        elif action == 'close':
            state['closed'] = True
            state['stopped'] = state['stopped'] or state['inflight'] is not None
        else:
            raise Denied('unknown_ledger_action')
        file.seek(0); file.truncate()
        json.dump(state, file, allow_nan=False)
        file.flush(); os.fsync(file.fileno())
        return state
