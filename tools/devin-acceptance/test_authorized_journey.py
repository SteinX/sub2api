import contextlib
import copy
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import authorized_journey as runner
from test_journey_policy import proposal


class ApprovedRunnerBoundaryTests(unittest.TestCase):
    def test_settlement_scope_never_reads_a_concurrently_truncated_ledger(self):
        import fcntl
        plan,approval,*_=proposal()
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);ledger=root/(approval['approval_id']+'.json')
            plan_path=root/'plan.json';plan_path.write_text(json.dumps(plan))
            approval_path=root/'approval.json';approval_path.write_text(json.dumps(approval))
            runner.claim(ledger,plan,'live')
            args=SimpleNamespace(plan=plan_path,approval=approval_path,ledger=ledger,mode='live')
            with ledger.open('r+') as writer:
                fcntl.flock(writer,fcntl.LOCK_EX)
                contents=writer.read();writer.seek(0);writer.truncate();writer.flush()
                with patch.object(runner,'STATE_ROOT',root): scope=runner.settlement_scope(args)
                writer.write(contents);writer.flush()
            state=runner.ledger_action(ledger,'stop',reason='client_exit',observed_ns=__import__('time').time_ns(),expected_scope=scope)
            self.assertTrue(state['stopped'])
            with self.assertRaisesRegex(runner.Denied,'scope_or_state_mismatch'):
                runner.ledger_action(ledger,'stop',reason='client_exit',observed_ns=__import__('time').time_ns(),expected_scope={**scope,'mode':'plan'})

    def test_stop_and_existing_settlement_need_no_new_credentials_or_source_approval(self):
        plan,approval,*_=proposal()
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary)
            plan_path=root/'plan.json';plan_path.write_text(json.dumps(plan))
            approval_path=root/'approval.json';approval_path.write_text(json.dumps(approval))
            ledger=root/(approval['approval_id']+'.json')
            runner.claim(ledger,plan,'live');runner.ledger_action(ledger,'admit')
            base=['runner','stop','--mode','live','--plan',str(plan_path),'--approval',str(approval_path),'--ledger',str(ledger),'--reason','client_exit','--observed-ns',str(__import__('time').time_ns())]
            with patch.object(runner,'STATE_ROOT',root),patch.dict(os.environ,{},clear=True),patch.object(runner,'current_source',side_effect=AssertionError('must not reauthorize')),contextlib.redirect_stdout(io.StringIO()):
                with patch.object(sys,'argv',base): self.assertEqual(0,runner.main())
                complete=base[:];complete[1]='complete';complete=complete[:complete.index('--reason')]+['--receipt','1','--outcome','success','--cost','0']
                with patch.object(sys,'argv',complete): self.assertEqual(0,runner.main())
            state=runner.read_json(ledger)
            self.assertTrue(state['stopped']);self.assertIsNone(state['inflight'])
            with self.assertRaises(runner.Denied): runner.ledger_action(ledger,'admit')

    def invoke(self, plan, approval, source, env, **changes):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root/'plan.json').write_text(json.dumps(plan))
            (root/'approval.json').write_text(json.dumps(approval))
            argv=['runner', 'run', '--mode','live','--plan',str(root/'plan.json'),'--approval',str(root/'approval.json'),'--artifacts',str(root/'artifacts')]
            with patch.object(sys,'argv',argv), patch.dict(os.environ,env,clear=True), patch.object(runner,'current_source',return_value=source), patch.object(runner.subprocess,'Popen') as spawn, contextlib.redirect_stdout(io.StringIO()) as output:
                # Time and network are irrelevant: all cases must reject first.
                code=runner.main()
                spawn.assert_not_called()
                self.assertFalse((root/'artifacts').exists())
            return code,json.loads(output.getvalue())

    def test_no_approval_never_builds_stores_credentials_or_calls_network(self):
        plan,approval,source,env=proposal(); approval['approved']=False
        code,result=self.invoke(plan,approval,source,env)
        self.assertEqual(2,code); self.assertEqual('explicit_approval_missing',result['reason'])

    def test_missing_credentials_and_wrong_destination_never_spawn(self):
        for mutation in ['credential','destination','source']:
            plan,approval,source,env=proposal()
            # Keep the synthetic approval fresh; no actual approved invocation.
            import datetime as dt
            approval['expires_at']=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(minutes=20)).isoformat()
            if mutation=='credential': env={}
            elif mutation=='destination':
                plan['upstream_origins']=['https://server.codeium.com.evil.invalid']; approval['plan_sha256']=runner.digest(plan)
            else: source['dirty']=True
            with self.subTest(mutation=mutation):
                code,result=self.invoke(plan,approval,source,env)
                self.assertEqual(2,code); self.assertFalse(result['executed'])

    def test_same_approval_cannot_select_a_fresh_ledger_path(self):
        for calls in (16,32):
            plan,approval,source,env=proposal()
            plan['limits']['internal_calls_per_turn']=calls
            approval['plan_sha256']=runner.digest(plan)
            with self.subTest(calls=calls), patch.object(sys,'argv',['runner','run','--mode','live','--ledger','/tmp/different-ledger','--validate-only']), patch.object(runner,'checked',return_value=(plan,{'approval_id':approval['approval_id']})), contextlib.redirect_stdout(io.StringIO()) as output:
                self.assertEqual(2,runner.main())
            self.assertEqual('approval_ledger_path_mismatch',json.loads(output.getvalue())['reason'])

    def test_internal_call_changes_are_validated_before_any_execution(self):
        import datetime as dt
        for calls,update_hash,expected in [(32,False,'approved_plan_changed'),(33,True,'invalid_internal_calls_per_turn_limit')]:
            plan,approval,source,env=proposal()
            approval['expires_at']=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(minutes=20)).isoformat()
            plan['limits']['internal_calls_per_turn']=calls
            if update_hash: approval['plan_sha256']=runner.digest(plan)
            with self.subTest(calls=calls,update_hash=update_hash):
                code,result=self.invoke(plan,approval,source,env)
                self.assertEqual(2,code);self.assertEqual(expected,result['reason'])

    def test_explicit_32_validation_only_never_claims_or_executes(self):
        import datetime as dt
        plan,approval,source,env=proposal()
        plan['limits']['internal_calls_per_turn']=32
        approval.update(plan_sha256=runner.digest(plan),expires_at=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(minutes=20)).isoformat())
        with patch.object(sys,'argv',['runner','run','--mode','live','--plan','synthetic-plan','--approval','synthetic-approval','--validate-only']), patch.dict(os.environ,env,clear=True), patch.object(runner,'read_json',side_effect=[approval,plan]), patch.object(runner,'current_source',return_value=source), patch.object(runner,'claim') as claim, patch.object(runner,'run') as execute, patch.object(runner.subprocess,'Popen') as spawn, contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(0,runner.main())
            claim.assert_not_called();execute.assert_not_called();spawn.assert_not_called()
        result=json.loads(output.getvalue())
        self.assertEqual({'authorized':True,'executed':False,'plan_sha256':runner.digest(plan)},result)


if __name__=='__main__':
    unittest.main()
