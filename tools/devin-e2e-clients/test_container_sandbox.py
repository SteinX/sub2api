import http.server
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch, Mock

import container_sandbox as sandbox
import journey


class SandboxRegistryTests(unittest.TestCase):
    def test_zero_docker_client_exit_cannot_certify_unknown_child_outcome(self):
        for state,inspect_code in [({'Status':'running','Running':True,'ExitCode':0},0),
                                   ({'Status':'created','Running':False,'ExitCode':0},0),
                                   ({'Status':'exited','Running':False,'ExitCode':'0'},0),({},1)]:
            with self.subTest(state=state,inspect_code=inspect_code):
                client=Mock(returncode=0);client.wait.return_value=0
                proc=sandbox.ContainerProcess(client,'synthetic-cid','synthetic-scope','private-registry',Mock(),None)
                result=subprocess.CompletedProcess([],inspect_code,json.dumps(state).encode(),b'')
                with patch.object(sandbox,'docker',return_value=result),patch.object(sandbox,'cleanup_registry',return_value=True):
                    self.assertNotEqual(0,proc.wait())

    def test_invalid_registry_never_operates_on_docker(self):
        with tempfile.TemporaryDirectory() as temporary:
            path=Path(temporary)/'registry'; path.write_text('{"scope":"not-an-id"}\n'); path.chmod(0o600)
            with patch.object(sandbox,'docker') as docker:
                self.assertFalse(sandbox.cleanup_registry(path)); docker.assert_not_called()

    def test_output_limit_is_shared_across_stdout_and_stderr(self):
        script="import os,time;os.write(1,b'a'*2048);os.write(2,b'b'*2048);time.sleep(30)"
        proc=subprocess.Popen([sys.executable,'-c',script],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
        with self.assertRaisesRegex(ValueError,'project_output_limit_exceeded'):
            sandbox.collect_output(proc,['synthetic-output'],output_limit=3072,timeout=3)
        self.assertIsNotNone(proc.poll())

    def test_output_capture_handles_input_and_reports_real_exit(self):
        script="import sys;sys.stdout.buffer.write(sys.stdin.buffer.read());sys.stderr.write('stderr');sys.exit(7)"
        proc=subprocess.Popen([sys.executable,'-c',script],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
        result=sandbox.collect_output(proc,['synthetic-input'],input_data=b'bounded-input',timeout=3)
        self.assertEqual((7,b'bounded-input',b'stderr'),(result.returncode,result.stdout,result.stderr))


@unittest.skipUnless(os.environ.get('DEVIN_SANDBOX_INTEGRATION')=='true','explicit local Docker boundary suite')
class RootlessSandboxIntegrationTests(unittest.TestCase):
    def test_model_written_oracle_output_limit_cleans_container_and_disables_docker_log(self):
        with tempfile.TemporaryDirectory() as private,tempfile.TemporaryDirectory() as project:
            directory=Path(project);(directory/'workspace').mkdir(); registry=Path(private)/'containers'
            (directory/'workspace/price_math.py').write_text("import os,time\nwhile True: os.write(2,b'x'*65536)\n")
            with patch.dict(os.environ,{'DEVIN_JOURNEY_CONTAINER_REGISTRY':str(registry)}):
                proc=sandbox.popen(['python3','-c','import price_math'],directory,journey.project_environment(directory),stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
                config=json.loads(sandbox.docker(['inspect',proc.cid]).stdout)[0]
                self.assertEqual('none',config['HostConfig']['LogConfig']['Type'])
                with self.assertRaisesRegex(ValueError,'project_output_limit_exceeded'):
                    sandbox.collect_output(proc,['model-written-oracle'],output_limit=4096,timeout=5)
                self.assertTrue(sandbox.cleanup_registry(registry))
                self.assertEqual(b'',sandbox.docker(['ps','-aq','--filter','label='+sandbox.LABEL+'='+proc.scope]).stdout.strip())
    def test_project_cannot_read_parent_private_file_or_native_environment(self):
        if os.geteuid()==0: self.skipTest('rootless boundary is exercised by an ordinary task user')
        with tempfile.TemporaryDirectory() as parent,tempfile.TemporaryDirectory() as project:
            marker=Path(parent)/'synthetic-parent-private'; marker.write_text('synthetic-only')
            directory=Path(project); (directory/'workspace').mkdir()
            with patch.dict(os.environ,{'DEVIN_E2E_PAT':'synthetic-native-pat','DEVIN_E2E_REDIS_PASSWORD':'synthetic-cache-password'}):
                result=journey.run_project_python(directory,['-c',"import os,pathlib,sys; assert not pathlib.Path(sys.argv[1]).exists(); assert 'DEVIN_E2E_PAT' not in os.environ; assert 'DEVIN_E2E_REDIS_PASSWORD' not in os.environ; assert not pathlib.Path('/var/run/docker.sock').exists();print('isolated')",str(marker)])
            self.assertEqual(0,result.returncode); self.assertEqual(b'isolated\n',result.stdout)

    def test_networkless_container_uses_only_fixed_product_relay(self):
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200); self.end_headers(); self.wfile.write(b'synthetic-fixed-ui')
            def log_message(self,*args): pass
        server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler)
        thread=threading.Thread(target=server.serve_forever,daemon=True); thread.start()
        other=socket.socket(); other.bind(('127.0.0.1',0)); other.listen()
        try:
            with tempfile.TemporaryDirectory() as project:
                directory=Path(project); (directory/'workspace').mkdir()
                url='http://127.0.0.1:'+str(server.server_port)
                script="import socket,sys,urllib.request; assert urllib.request.urlopen(sys.argv[1],timeout=3).read()==b'synthetic-fixed-ui'; s=socket.socket();s.settimeout(1);assert s.connect_ex(('127.0.0.1',int(sys.argv[2])))!=0;print('fixed-relay-only')"
                proc=sandbox.popen(['python3','-c',script,url,str(other.getsockname()[1])],directory,journey.project_environment(directory),gateway=url,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
                stdout,_=proc.communicate(timeout=15)
                self.assertEqual(0,proc.returncode); self.assertEqual(b'fixed-relay-only\n',stdout)
        finally:
            other.close(); server.shutdown();server.server_close();thread.join(timeout=3)

    def test_killed_docker_client_does_not_leave_its_container_running(self):
        with tempfile.TemporaryDirectory() as private,tempfile.TemporaryDirectory() as project:
            directory=Path(project);(directory/'workspace').mkdir(); registry=Path(private)/'containers'
            with patch.dict(os.environ,{'DEVIN_JOURNEY_CONTAINER_REGISTRY':str(registry)}):
                proc=sandbox.popen(['python3','-c',"import time;print('ready',flush=True);time.sleep(30)"],directory,journey.project_environment(directory),stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
                try:
                    self.assertEqual(b'ready\n',proc.stdout.readline())
                    os.killpg(proc.pid,signal.SIGKILL); self.assertNotEqual(0,proc.wait(timeout=5))
                    self.assertTrue(sandbox.cleanup_registry(registry))
                    self.assertTrue(sandbox.docker(['ps','-aq','--filter','label='+sandbox.LABEL+'='+proc.scope]).stdout.strip()==b'')
                finally:
                    proc.cleanup();proc.stdout.close();proc.stderr.close()
