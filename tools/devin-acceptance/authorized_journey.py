#!/usr/bin/env python3
"""Executable opt-in for a complete isolated product preview and real upstream.

No credentials are obtained here. Missing approval fails before building,
Docker, native authentication, temporary storage or any network call.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

from journey_policy import Denied, claim, current_source, digest, ledger_action, read_json, validate

ROOT = Path(__file__).resolve().parents[2]
STATE_ROOT = ROOT / '.cache/devin-approved-runs'


def checked(args):
    if not args.approval or not args.plan:
        raise Denied('explicit_approval_and_plan_required')
    approval, plan = read_json(args.approval), read_json(args.plan)
    if approval.get('approved') is not True:
        raise Denied('explicit_approval_missing')
    result = validate(plan, approval, args.mode, current_source(), os.environ)
    return plan, result


def stop_process(process):
    if process.poll() is None:
        os.killpg(process.pid, signal.SIGTERM)
        try:
            process.wait(timeout=20)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=5)


def run(args, plan):
    if os.geteuid() != 0:
        raise Denied('real_project_requires_unprivileged_child_isolation')
    if not args.ledger or not args.artifacts:
        raise Denied('private_run_ledger_and_artifacts_required')
    artifacts = Path(args.artifacts).resolve()
    if artifacts.exists() and any(artifacts.iterdir()):
        raise Denied('fresh_private_artifact_directory_required')
    if artifacts.exists() and (artifacts.stat().st_uid != os.geteuid() or artifacts.stat().st_mode & 0o077):
        raise Denied('private_owned_artifact_directory_required')
    claim(args.ledger, plan, args.mode)
    artifacts.mkdir(parents=True, mode=0o700, exist_ok=True)
    artifacts.chmod(0o700)
    env = dict(os.environ)
    env.update(DEVIN_JOURNEY_REAL='true', DEVIN_JOURNEY_PLAN_FILE=str(Path(args.plan).resolve()),
               DEVIN_JOURNEY_APPROVAL_FILE=str(Path(args.approval).resolve()), DEVIN_JOURNEY_RUN_LEDGER=str(Path(args.ledger).resolve()),
               DEVIN_JOURNEY_RUN_MODE=args.mode, DEVIN_JOURNEY_UI_DIST=str(artifacts/'product-ui'), DEVIN_E2E_ARTIFACTS=str(artifacts),
               DEVIN_E2E_RUN='^TestDevinClientUserJourneysAuthorized$', DEVIN_E2E_MODEL=plan['models']['direct'],
               DEVIN_E2E_MAX_OUTPUT_TOKENS=str(plan['limits']['output_tokens']), DEVIN_E2E_ALLOW_PAID='true' if args.mode == 'live' else 'false',
               DEVIN_JOURNEY_CAPABILITIES=plan.get('omp_capabilities', 'false'))
    for name in ['DEVIN_JOURNEY_FETCH_DIAGNOSTIC', 'DEVIN_JOURNEY_CASE', 'DEVIN_E2E_SKIP']:
        env.pop(name, None)
    process = None
    started = time.monotonic()
    previous_handlers = {}
    def interrupted(number, _frame):
        if process is not None:
            stop_process(process)
        raise KeyboardInterrupt()
    for number in [signal.SIGINT, signal.SIGTERM]:
        previous_handlers[number] = signal.signal(number, interrupted)
    try:
        with os.fdopen(os.open(artifacts/'private-run.log', os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW, 0o600), 'wb') as log:
            commands = [
                ['pnpm', '--dir', str(ROOT/'frontend'), 'exec', 'vite', 'build', '--outDir', env['DEVIN_JOURNEY_UI_DIST'], '--emptyOutDir'],
                ['bash', str(ROOT/'deploy/devin-e2e.sh'), args.mode],
            ]
            for command in commands:
                remaining = int(plan['limits']['wall_seconds'] - (time.monotonic()-started))
                if remaining < 1:
                    raise Denied('approved_wall_time_exhausted')
                env['DEVIN_E2E_TIMEOUT_SECONDS'] = str(remaining)
                process = subprocess.Popen(command, cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
                try:
                    code = process.wait(timeout=remaining)
                except subprocess.TimeoutExpired:
                    stop_process(process)
                    raise Denied('approved_wall_time_exhausted') from None
                if code:
                    raise Denied('preview_or_client_acceptance_failed_see_private_evidence')
    finally:
        if process is not None:
            stop_process(process)
        ledger_action(args.ledger, 'close')
        for number, previous in previous_handlers.items():
            signal.signal(number, previous)
    return {'executed': True, 'mode': args.mode, 'real_user_e2e_certified': False,
            'scope': 'only implemented product/client cases; final evidence validation is separate'}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('action', choices=['run', 'verify', 'admit', 'complete'])
    parser.add_argument('--mode', choices=['plan', 'live'], required=True)
    parser.add_argument('--plan', default=os.environ.get('DEVIN_JOURNEY_PLAN_FILE'))
    parser.add_argument('--approval', default=os.environ.get('DEVIN_JOURNEY_APPROVAL_FILE'))
    parser.add_argument('--ledger', default=os.environ.get('DEVIN_JOURNEY_RUN_LEDGER'))
    parser.add_argument('--artifacts')
    parser.add_argument('--validate-only', action='store_true')
    parser.add_argument('--receipt', type=int)
    parser.add_argument('--outcome', choices=['success', 'failed', 'uncertain'])
    parser.add_argument('--cost', type=float)
    args = parser.parse_args()
    try:
        plan, checked_plan = checked(args)
        expected_ledger = (STATE_ROOT / (checked_plan['approval_id']+'.json')).resolve()
        if args.ledger and Path(args.ledger).resolve() != expected_ledger:
            raise Denied('approval_ledger_path_mismatch')
        args.ledger = str(expected_ledger)
        if args.action == 'run' and args.validate_only:
            result = {'authorized': True, 'executed': False, 'plan_sha256': checked_plan['plan_sha256']}
        elif args.action == 'run':
            result = run(args, plan)
        else:
            if not args.ledger:
                raise Denied('private_run_ledger_required')
            state = read_json(args.ledger)
            if state.get('plan_sha256') != digest(plan) or state.get('mode') != args.mode or state.get('closed') or state.get('stopped'):
                raise Denied('run_ledger_scope_or_state_mismatch')
            if args.action == 'verify':
                result = {'authorized': True, 'executed': False}
            else:
                state = ledger_action(args.ledger, args.action, args.receipt, args.outcome, args.cost)
                result = {'authorized': True, 'receipt': state['inflight'], 'requests': state['requests'], 'stopped': state['stopped']}
        print(json.dumps(result))
        return 0
    except Denied as error:
        print(json.dumps({'authorized': False, 'executed': False, 'reason': str(error)}))
        return 2
    except (ValueError, OSError, KeyError, TypeError, subprocess.SubprocessError, KeyboardInterrupt):
        print(json.dumps({'authorized': False, 'reason': 'invalid_policy_or_execution_interrupted'}))
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
