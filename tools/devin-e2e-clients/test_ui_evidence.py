import json
import unittest
import io
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from contextlib import redirect_stdout
from unittest.mock import Mock,patch
import journey
from ui_evidence import parse_ui_output


def progress(stage='model-target-option'):
    return {'ui_progress':True,'stage':stage,'elapsed_ms':16000,'catalog_requests':2,'catalog_responses':1,'copied_files':0,
            'failure_stage':None,'catalog_event':'stage','catalog_ordinal':0,'http_status':None}


class UIEvidenceTests(unittest.TestCase):
    def test_progress_and_browser_shutdown_never_replace_final_result(self):
        records=[progress(),{'passed':True,'source':'synthetic terminal'},progress('browser-closed')]
        terminal,last=parse_ui_output(b'\n'.join(json.dumps(r).encode() for r in records))
        self.assertTrue(terminal['passed']);self.assertEqual('browser-closed',last['stage'])
        with self.assertRaisesRegex(ValueError,'terminal_missing'):parse_ui_output(json.dumps(progress()).encode())

    def test_timeout_retains_safe_stage_without_claiming_success(self):
        raw=json.dumps(progress('catalog-refresh')).encode()+b'\n{"partial'
        terminal,last=parse_ui_output(raw,partial=True)
        self.assertIsNone(terminal);self.assertEqual('catalog-refresh',last['stage'])

    def test_unknown_fields_unbounded_output_and_multiple_terminals_fail_closed(self):
        for raw in [json.dumps({**progress(),'password':'must-not-retain'}).encode(),b'x'*(1024*1024+1),
                    b'{"passed":true}\n{"passed":false}\n',b'null\n']:
            with self.assertRaises(ValueError):parse_ui_output(raw)

    def test_legacy_single_terminal_is_supported(self):
        terminal,last=parse_ui_output(b'{"passed":false,"blocked":true}\n')
        self.assertFalse(terminal['passed']);self.assertIsNone(last)

    def test_outer_timeout_with_a_success_frame_never_starts_a_model(self):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);workspace=root/'workspace';workspace.mkdir()
            wire=(json.dumps({'passed':True})+'\n'+json.dumps(progress('browser-close'))+'\n').encode()
            ui=Mock();ui.communicate.side_effect=subprocess.TimeoutExpired('synthetic-ui',60,output=wire)
            argv=['journey.py','--directory',str(root),'--client','opencode','--model','synthetic','--ui-url','http://127.0.0.1:1234','--profile','project']
            output=io.StringIO()
            with patch.object(sys,'argv',argv),patch.object(journey,'identity',return_value={}),patch.object(journey,'isolation',return_value={}), \
                 patch.object(journey,'isolation_evidence',return_value={}),patch.object(journey,'project_environment',return_value={}), \
                 patch.object(journey,'prepare',return_value=(workspace,{'nonce':'synthetic'},{})),patch.object(journey,'start_child',return_value=ui), \
                 patch.object(journey.os,'killpg'),patch.object(journey,'invoke') as invoke, \
                 patch.dict(os.environ,{'DEVIN_JOURNEY_GATEWAY_KEY':'synthetic','DEVIN_JOURNEY_UI_PASSWORD':'synthetic'}),redirect_stdout(output):
                self.assertEqual(1,journey.main())
            report=json.loads(output.getvalue())
            self.assertEqual('failed',report['status']);self.assertEqual('product-ui-process-timeout',report['stage'])
            self.assertEqual('browser-close',report['ui_progress']['stage']);invoke.assert_not_called()
