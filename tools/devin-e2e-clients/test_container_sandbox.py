import http.server
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import patch

import container_sandbox as sandbox
import journey


class SandboxRegistryTests(unittest.TestCase):
    def test_invalid_registry_never_operates_on_docker(self):
        with tempfile.TemporaryDirectory() as temporary:
            path=Path(temporary)/'registry'; path.write_text('{"scope":"not-an-id"}\n'); path.chmod(0o600)
            with patch.object(sandbox,'docker') as docker:
                self.assertFalse(sandbox.cleanup_registry(path)); docker.assert_not_called()


@unittest.skipUnless(os.environ.get('DEVIN_SANDBOX_INTEGRATION')=='true','explicit local Docker boundary suite')
class RootlessSandboxIntegrationTests(unittest.TestCase):
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
