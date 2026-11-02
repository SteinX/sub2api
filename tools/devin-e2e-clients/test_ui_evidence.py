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
from ui_evidence import parse_ui_output,communicate_ui


def progress(stage='model-target-option'):
    return {'ui_progress':True,'stage':stage,'elapsed_ms':16000,'catalog_requests':2,'catalog_responses':1,'copied_files':0,
            'failure_stage':None,'catalog_event':'stage','catalog_ordinal':0,'http_status':None}


class UIEvidenceTests(unittest.TestCase):
    def test_only_mock_recovery_gets_a_bounded_cold_start_window(self):
        ready={**progress('navigate'),'elapsed_ms':39646}
        wire=json.dumps(ready).encode()+b'\n'
        proc=Mock();proc.communicate.side_effect=[subprocess.TimeoutExpired('ui',45,output=wire),(b'{"passed":true}',b'')]
        with patch('ui_evidence.time.monotonic',side_effect=[0,45]):
            self.assertEqual((b'{"passed":true}',b''),communicate_ui(proc,b'private-input',upstream='synthetic',catalog_recovery=True))
        self.assertEqual(2,proc.communicate.call_count)
        self.assertEqual((b'private-input',),proc.communicate.call_args_list[0].args)
        self.assertEqual((),proc.communicate.call_args_list[1].args,'stdin is never resent')
        self.assertEqual(40,proc.communicate.call_args_list[1].kwargs['timeout'])
        for upstream,probe in [('real',False),('real',True),('synthetic',False)]:
            proc=Mock();proc.communicate.side_effect=subprocess.TimeoutExpired('ui',60,output=wire)
            with self.assertRaises(subprocess.TimeoutExpired):communicate_ui(proc,b'private-input',upstream=upstream,catalog_recovery=probe)
            proc.communicate.assert_called_once_with(b'private-input',timeout=60)

    def test_missing_or_late_readiness_cannot_extend_a_hung_browser(self):
        for wire in [b'',b'null\n',json.dumps({**progress('navigate'),'elapsed_ms':46000}).encode()]:
            proc=Mock();proc.communicate.side_effect=subprocess.TimeoutExpired('ui',45,output=wire)
            with patch('ui_evidence.time.monotonic',side_effect=[0,45]),self.assertRaises(subprocess.TimeoutExpired):
                communicate_ui(proc,b'private-input',upstream='synthetic',catalog_recovery=True)
            self.assertEqual(1,proc.communicate.call_count)

    def test_ui_work_and_absolute_deadlines_are_both_preserved(self):
        wire=json.dumps({**progress('navigate'),'elapsed_ms':1000}).encode()
        proc=Mock();proc.communicate.side_effect=[subprocess.TimeoutExpired('ui',45,output=wire),(b'{"passed":true}',b'')]
        with patch('ui_evidence.time.monotonic',side_effect=[0,45]):
            communicate_ui(proc,b'private-input',upstream='synthetic',catalog_recovery=True)
        self.assertEqual(16,proc.communicate.call_args_list[1].kwargs['timeout'],'UI work remains bounded by 60 seconds')
        proc=Mock();proc.communicate.side_effect=subprocess.TimeoutExpired('ui',45,output=wire)
        with patch('ui_evidence.time.monotonic',side_effect=[0,85]),self.assertRaises(subprocess.TimeoutExpired):
            communicate_ui(proc,b'private-input',upstream='synthetic',catalog_recovery=True)
        self.assertEqual(1,proc.communicate.call_count,'an elapsed absolute deadline cannot reopen')

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
