import copy
import datetime as dt
from pathlib import Path
import tempfile
import unittest
import unittest.mock

import journey_policy as policy

NOW = dt.datetime(2026, 10, 25, tzinfo=dt.timezone.utc)


def proposal():
    source = {'commit': 'a'*40, 'tree': 'b'*40}
    plan = {'schema_version': 1, 'source': source, 'upstream_origins': ['https://server.codeium.com'],
            'product': 'isolated_complete_preview', 'dataset': 'synthetic_price_project_v1', 'models': {'direct': 'model-direct', 'lead': 'model-lead', 'sidekick': 'model-sidekick'},
            'clients': ['opencode', 'omo', 'omp'], 'reference_pricing_sha256': 'c'*64, 'stop_on_failure': True, 'retry_after_uncertain_send': False,
            'limits': {'public_requests': 144, 'output_tokens': 1024, 'wall_seconds': 3600, 'turn_seconds': 300,
                       'internal_calls_per_turn': 64, 'tools_per_turn': 20, 'handoffs_per_turn': 2, 'parallelism': 1, 'local_usd': 3, 'turn_usd': .5}}
    approval = {'schema_version': 1, 'approved': True, 'approval_id': 'test-approval-0001', 'mode': 'live', 'plan_sha256': policy.digest(plan), 'expires_at': '2026-10-25T01:00:00Z',
                'scopes': ['native_auth', 'temporary_credential_storage', 'local_preview_accounts', 'model_generation']}
    return plan, approval, {**source, 'dirty': False}, {'DEVIN_E2E_PAT': 'synthetic-reference'}


class RealJourneyPolicyTests(unittest.TestCase):
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


if __name__ == '__main__':
    unittest.main()
