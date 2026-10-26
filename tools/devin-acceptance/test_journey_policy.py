import copy
import datetime as dt
from pathlib import Path
import tempfile
import time
import unittest
import unittest.mock

import journey_policy as policy

NOW = dt.datetime(2026, 10, 25, tzinfo=dt.timezone.utc)


def proposal():
    source = {'commit': 'a'*40, 'tree': 'b'*40}
    plan = {'schema_version': 1, 'source': source, 'upstream_origins': ['https://server.codeium.com'],
            'product': 'isolated_complete_preview', 'dataset': 'synthetic_price_project_v1', 'models': {'direct': 'model-direct', 'lead': 'model-lead', 'sidekick': 'model-sidekick'},
            'clients': ['opencode'], 'tool_budget': {'operations': policy.TOOL_OPERATIONS, 'per_operation': 12, 'total': 48},
            'reference_pricing_sha256': 'c'*64, 'stop_on_failure': True, 'retry_after_uncertain_send': False,
            'limits': {'public_requests': 144, 'output_tokens': 1024, 'wall_seconds': 3600, 'turn_seconds': 300,
                       'internal_calls_per_turn': 16, 'tools_per_turn': 12, 'handoffs_per_turn': 2, 'parallelism': 1, 'local_usd': 3, 'turn_usd': .5}}
    approval = {'schema_version': 1, 'approved': True, 'approval_id': 'test-approval-0001', 'mode': 'live', 'plan_sha256': policy.digest(plan), 'expires_at': '2026-10-25T01:00:00Z',
                'scopes': ['native_auth', 'temporary_credential_storage', 'local_preview_accounts', 'model_generation']}
    return plan, approval, {**source, 'dirty': False}, {'DEVIN_E2E_PAT': 'synthetic-reference'}


class RealJourneyPolicyTests(unittest.TestCase):
    def test_fatal_stop_is_durable_and_inflight_can_only_settle_once(self):
        plan, *_ = proposal()
        with tempfile.TemporaryDirectory() as temporary:
            ledger = Path(temporary) / 'ledger.json'; policy.claim(ledger, plan, 'live')
            policy.ledger_action(ledger, 'admit')
            observed = time.time_ns()
            stopped = policy.ledger_action(ledger, 'stop', reason='permission_rejection', observed_ns=observed)
            self.assertEqual(1, stopped['inflight'])
            self.assertEqual(observed, stopped['first_failure']['observed_at_ns'])
            with self.assertRaises(policy.Denied): policy.ledger_action(ledger, 'admit')
            settled = policy.ledger_action(ledger, 'complete', 1, 'success', .01)
            self.assertTrue(settled['stopped']); self.assertIsNone(settled['inflight'])
            self.assertEqual(.01, settled['local_cost_usd'])
            with self.assertRaises(policy.Denied): policy.ledger_action(ledger, 'admit')
            with self.assertRaises(policy.Denied): policy.ledger_action(ledger, 'complete', 1, 'success', .01)
            policy.ledger_action(ledger, 'close')
            self.assertTrue(policy.read_json(ledger)['stopped'])
            self.assertEqual(['admit', 'stop', 'complete'], [entry['action'] for entry in settled['journal']])

    def test_approval_and_credentials_fail_before_execution(self):
        plan, approval, source, env = proposal()
        with self.assertRaisesRegex(policy.Denied, 'explicit_approval_missing'):
            policy.validate(plan, None, 'live', source, env, NOW)
        with self.assertRaisesRegex(policy.Denied, 'credential_reference_missing'):
            policy.validate(plan, approval, 'live', source, {}, NOW)
        with self.assertRaisesRegex(policy.Denied, 'scope_not_approved'):
            approval['scopes'].remove('model_generation')
            policy.validate(plan, approval, 'live', source, env, NOW)

    def test_destination_is_exact_and_cannot_be_expanded(self):
        for origins in [['http://server.codeium.com'], ['https://server.codeium.com.evil.invalid'], ['https://user:pass@server.codeium.com'],
                        ['https://server.codeium.com:8443'], ['https://server.codeium.com', 'https://other.invalid']]:
            plan, approval, source, env = proposal()
            plan['upstream_origins'] = origins; approval['plan_sha256'] = policy.digest(plan)
            with self.subTest(origins=origins), self.assertRaisesRegex(policy.Denied, 'destination_not_allowed'):
                policy.validate(plan, approval, 'live', source, env, NOW)

    def test_changed_plan_source_expiry_or_budget_is_rejected(self):
        for mutation in ['plan', 'dirty', 'expiry', 'budget', 'nan', 'parallel']:
            plan, approval, source, env = proposal()
            if mutation == 'plan': plan['models']['direct'] = 'different-model'
            elif mutation == 'dirty': source['dirty'] = True
            elif mutation == 'expiry': approval['expires_at'] = '2026-10-24T00:00:00Z'
            else:
                plan['limits'][{'budget': 'local_usd', 'nan': 'turn_usd', 'parallel': 'parallelism'}[mutation]] = {'budget': 4, 'nan': -1, 'parallel': 2}[mutation]
                approval['plan_sha256'] = policy.digest(plan)
            with self.subTest(mutation=mutation), self.assertRaises(policy.Denied):
                policy.validate(plan, approval, 'live', source, env, NOW)

    def test_same_approval_cannot_reset_a_run_or_an_uncertain_send(self):
        plan, approval, source, env = proposal()
        self.assertEqual('live', policy.validate(plan, approval, 'live', source, env, NOW)['mode'])
        with tempfile.TemporaryDirectory() as temporary:
            ledger = Path(temporary)/'ledger.json'
            policy.claim(ledger, plan, 'live')
            with self.assertRaises(policy.Denied): policy.claim(ledger, plan, 'live')
            first = policy.ledger_action(ledger, 'admit')
            with self.assertRaises(policy.Denied): policy.ledger_action(ledger, 'admit')
            policy.ledger_action(ledger, 'complete', first['inflight'], 'uncertain', .1)
            with self.assertRaises(policy.Denied): policy.ledger_action(ledger, 'admit')

    def test_request_and_cost_limits_are_cumulative_and_close_is_terminal(self):
        plan, *_ = proposal(); plan['limits'].update(public_requests=2, local_usd=.1)
        with tempfile.TemporaryDirectory() as temporary:
            ledger = Path(temporary)/'ledger.json'; policy.claim(ledger, plan, 'live')
            policy.ledger_action(ledger, 'admit'); policy.ledger_action(ledger, 'complete', 1, 'success', .1)
            with self.assertRaises(policy.Denied): policy.ledger_action(ledger, 'admit')
            state=policy.ledger_action(ledger, 'close'); self.assertTrue(state['closed']); self.assertEqual(1,state['requests'])
            with self.assertRaises(policy.Denied): policy.ledger_action(ledger, 'admit')

    def test_plan_mode_never_admits_generation(self):
        plan, *_ = proposal()
        with tempfile.TemporaryDirectory() as temporary:
            ledger=Path(temporary)/'ledger.json'; policy.claim(ledger,plan,'plan')
            with self.assertRaises(policy.Denied): policy.ledger_action(ledger,'admit')

    def test_deadline_or_changed_group_price_input_cannot_extend_a_batch(self):
        plan,approval,source,env=proposal()
        with tempfile.TemporaryDirectory() as temporary:
            ledger=Path(temporary)/'ledger.json'; policy.claim(ledger,plan,'live')
            with unittest.mock.patch('journey_policy.time.time',return_value=10**20):
                with self.assertRaisesRegex(policy.Denied,'wall_time_exhausted'): policy.ledger_action(ledger,'admit')
            snapshot=Path(temporary)/'group.json'; snapshot.write_text('[]')
            env['DEVIN_JOURNEY_GROUP_PRICING_FILE']=str(snapshot)
            with self.assertRaisesRegex(policy.Denied,'unapproved_group_price'): policy.validate(plan,approval,'live',source,env,NOW)
            import hashlib
            plan['group_pricing_sha256']=hashlib.sha256(snapshot.read_bytes()).hexdigest(); approval['plan_sha256']=policy.digest(plan)
            policy.validate(plan,approval,'live',source,env,NOW)
            snapshot.write_text('[{"changed":true}]')
            with self.assertRaisesRegex(policy.Denied,'price_snapshot'): policy.validate(plan,approval,'live',source,env,NOW)

    def test_direct_quote_obeys_turn_and_remaining_run_limits_before_admit(self):
        for cost,estimate in [(0,.0011),(.0995,.0008)]:
            plan,*_=proposal(); plan['limits'].update(turn_usd=.001,local_usd=.1)
            with tempfile.TemporaryDirectory() as temporary:
                ledger=Path(temporary)/'ledger.json'; policy.claim(ledger,plan,'live')
                with self.assertRaisesRegex(policy.Denied,'direct_turn_or_remaining_cost'):
                    policy.ledger_action(ledger,'admit',cost=cost,estimated_cost=estimate)
                self.assertEqual(0,policy.read_json(ledger)['requests'])

    def test_tool_scope_cannot_expand_by_client_session_or_capability(self):
        for mutation in ('missing', 'extra', 'total', 'per', 'client', 'advanced'):
            plan, approval, source, env = proposal()
            if mutation == 'missing': plan.pop('tool_budget')
            elif mutation == 'extra': plan['tool_budget']['operations'] = [*policy.TOOL_OPERATIONS, 'another/session']
            elif mutation == 'total': plan['tool_budget']['total'] = 49
            elif mutation == 'per': plan['tool_budget']['per_operation'] = 13
            elif mutation == 'client': plan['clients'] = ['opencode', 'omp']
            elif mutation == 'advanced': plan['omp_capabilities'] = 'true'
            approval['plan_sha256'] = policy.digest(plan)
            with self.subTest(mutation=mutation), self.assertRaisesRegex(policy.Denied, 'fixed_tool_operation'):
                policy.validate(plan, approval, 'live', source, env, NOW)

    def test_twelfth_tool_reserved_thirteenth_stops_without_partial_batch(self):
        plan, *_ = proposal()
        with tempfile.TemporaryDirectory() as temporary:
            ledger = Path(temporary) / 'ledger.json'; policy.claim(ledger, plan, 'live')
            operation = policy.TOOL_OPERATIONS[0]
            policy.ledger_action(ledger, 'tool_begin', operation=operation)
            policy.ledger_action(ledger, 'admit')
            calls = [policy.digest({'call': i}) for i in range(13)]
            state = policy.ledger_action(ledger, 'tool_reserve', operation=operation, calls=calls[:11])
            self.assertEqual(11, state['tools_reserved'])
            state = policy.ledger_action(ledger, 'tool_reserve', operation=operation, calls=calls[11:12])
            self.assertEqual(12, state['tools_reserved']); self.assertFalse(state['stopped'])
            state = policy.ledger_action(ledger, 'tool_reserve', operation=operation, calls=calls[12:])
            self.assertEqual(12, state['tools_reserved']); self.assertTrue(state['stopped'])
            policy.ledger_action(ledger, 'complete', 1, 'success', 0)
            for action in ('admit', 'tool_begin', 'tool_reserve'):
                with self.assertRaises(policy.Denied):
                    policy.ledger_action(ledger, action, operation=policy.TOOL_OPERATIONS[1], calls=calls[12:])
            self.assertEqual(12, policy.read_json(ledger)['tools_reserved'])

    def test_tool_budget_survives_requests_processes_and_four_operations(self):
        import subprocess, sys, json
        plan, *_ = proposal()
        with tempfile.TemporaryDirectory() as temporary:
            ledger = Path(temporary) / 'ledger.json'; policy.claim(ledger, plan, 'live')
            for index, operation in enumerate(policy.TOOL_OPERATIONS):
                policy.ledger_action(ledger, 'tool_begin', operation=operation)
                for number in range(12):
                    state = policy.ledger_action(ledger, 'admit')
                    call = policy.digest({'operation': operation, 'number': number})
                    policy.ledger_action(ledger, 'tool_reserve', operation=operation, calls=[call])
                    policy.ledger_action(ledger, 'complete', state['inflight'], 'success', 0)
                self.assertEqual((index+1)*12, policy.read_json(ledger)['tools_reserved'])
            result = subprocess.check_output([sys.executable, '-c',
                "import json,sys;from journey_policy import ledger_action,Denied\ntry: ledger_action(sys.argv[1],'tool_begin',operation=sys.argv[2])\nexcept Denied as e: print(json.dumps({'denied':str(e)}))", str(ledger), policy.TOOL_OPERATIONS[0]], cwd=Path(policy.__file__).parent)
            self.assertIn('already_claimed', json.loads(result)['denied'])
            self.assertEqual(48, policy.read_json(ledger)['tools_reserved'])

    def test_tool_batch_is_atomic_and_uncertain_reservation_never_refunded(self):
        plan, *_ = proposal()
        with tempfile.TemporaryDirectory() as temporary:
            ledger = Path(temporary) / 'ledger.json'; policy.claim(ledger, plan, 'live')
            operation = policy.TOOL_OPERATIONS[0]
            policy.ledger_action(ledger, 'tool_begin', operation=operation)
            policy.ledger_action(ledger, 'admit')
            calls = [policy.digest({'call': i}) for i in range(13)]
            policy.ledger_action(ledger, 'tool_reserve', operation=operation, calls=calls[:11])
            state = policy.ledger_action(ledger, 'tool_reserve', operation=operation, calls=calls[11:])
            self.assertTrue(state['stopped']); self.assertEqual(11, state['tools_reserved'])
            self.assertEqual(calls[:11], state['tool_operations'][operation])
            policy.ledger_action(ledger, 'complete', 1, 'uncertain', 0)
            self.assertEqual(11, policy.read_json(ledger)['tools_reserved'])



    def test_long_plan_is_explicit_and_resume_zero_budget_is_durable(self):
        plan,approval,source,env=proposal()
        with tempfile.TemporaryDirectory() as temporary:
            ledger=Path(temporary)/'short.json';policy.claim(ledger,plan,'live')
            with self.assertRaisesRegex(policy.Denied,'unapproved_tool_operation'):
                policy.ledger_action(ledger,'tool_begin',operation=policy.LONG_OPERATIONS[0])
        plan.update(journey_set='core_with_long_v1',dataset='synthetic_price_project_long_v1')
        plan['limits']['cli_seconds']=300
        plan['tool_budget'].update(operations=policy.TOOL_OPERATIONS+policy.LONG_OPERATIONS,total=60,no_tools_operations=[policy.LONG_OPERATIONS[1]])
        approval['plan_sha256']=policy.digest(plan)
        policy.validate(plan,approval,'live',source,env,NOW)
        with tempfile.TemporaryDirectory() as temporary:
            ledger=Path(temporary)/'long.json';policy.claim(ledger,plan,'live')
            policy.ledger_action(ledger,'tool_begin',operation=policy.LONG_OPERATIONS[1])
            policy.ledger_action(ledger,'admit')
            state=policy.ledger_action(ledger,'tool_reserve',operation=policy.LONG_OPERATIONS[1],calls=['a'*64])
            self.assertTrue(state['stopped']);self.assertEqual(0,state['tools_reserved'])
        for mutation in ('scope','no_tools','total','cli'):
            changed=copy.deepcopy(plan)
            if mutation=='scope':changed['tool_budget']['operations']=policy.TOOL_OPERATIONS
            if mutation=='no_tools':changed['tool_budget']['no_tools_operations']=[]
            if mutation=='total':changed['tool_budget']['total']=72
            if mutation=='cli':changed['limits'].pop('cli_seconds')
            approval['plan_sha256']=policy.digest(changed)
            with self.subTest(mutation=mutation),self.assertRaises(policy.Denied):policy.validate(changed,approval,'live',source,env,NOW)

if __name__ == '__main__':
    unittest.main()
