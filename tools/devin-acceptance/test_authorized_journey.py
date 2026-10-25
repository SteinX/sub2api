import contextlib
import copy
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import authorized_journey as runner
from test_journey_policy import proposal


class ApprovedRunnerBoundaryTests(unittest.TestCase):
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
        plan,approval,source,env=proposal()
        import datetime as dt
        approval['expires_at']=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(minutes=20)).isoformat()
        with patch.object(sys,'argv',['runner','run','--mode','live','--ledger','/tmp/different-ledger','--validate-only']), patch.object(runner,'checked',return_value=(plan,{'approval_id':approval['approval_id']})), contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(2,runner.main())
        self.assertEqual('approval_ledger_path_mismatch',json.loads(output.getvalue())['reason'])


if __name__=='__main__':
    unittest.main()
