import unittest
import tempfile
import json,hashlib
from pathlib import Path
from test_journey_policy import proposal,NOW
import journey_policy
from price_facts import user_confirmed_swe2_rule


class ConfirmedFreePriceTests(unittest.TestCase):
    def test_fact_is_bound_to_approved_exact_zero_snapshot(self):
        plan,approval,source,env=proposal()
        plan['models']['sidekick']='swe-2-high'
        rule,fact=user_confirmed_swe2_rule('swe-2-high',plan['models'].values())
        with tempfile.TemporaryDirectory() as temporary:
            path=Path(temporary)/'local-price-snapshot.json'; path.write_text(json.dumps([rule]))
            env['DEVIN_JOURNEY_GROUP_PRICING_FILE']=str(path)
            plan.update(group_pricing_sha256=hashlib.sha256(path.read_bytes()).hexdigest(),user_price_facts=[fact]);approval['plan_sha256']=journey_policy.digest(plan)
            journey_policy.validate(plan,approval,'live',source,env,NOW)
            plan['user_price_facts'][0]['official_price_card_verified']=True;approval['plan_sha256']=journey_policy.digest(plan)
            with self.assertRaises(journey_policy.Denied):journey_policy.validate(plan,approval,'live',source,env,NOW)
    def test_explicit_zero_has_narrow_provenance_and_no_wildcard(self):
        rule,source=user_confirmed_swe2_rule('swe-2-high',['swe-2-high','claude-opus-5-5-medium'])
        self.assertEqual(['swe-2-high'],rule['models'])
        self.assertEqual('user_confirmed',source['source'])
        self.assertFalse(source['official_price_card_verified'])
        self.assertTrue(all(rule[key]==0 for key in ['input_price','output_price','cache_write_price','cache_read_price']))

    def test_missing_unknown_virtual_and_paid_uids_do_not_inherit_free_pricing(self):
        for uid,grants in [('swe-2-high',[]),('swe-2-unknown',['swe-2-unknown']),('swe-2-max',['swe-2-max']),
                           ('claude-opus-5-5-medium',['claude-opus-5-5-medium']),('fusion-example',['fusion-example'])]:
            with self.subTest(uid=uid),self.assertRaises(ValueError):
                user_confirmed_swe2_rule(uid,grants)
