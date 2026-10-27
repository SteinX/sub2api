"""Rootless client isolation using the already available local Docker daemon.

No host networking, privileged mode, extra capabilities, personal home or
credential mounts. A networkless container can reach only a fixed UI Unix relay.
Native authentication and the private budget controller stay outside containers.
"""
import json
import os
from pathlib import Path
import re
import secrets
import selectors
import signal
import socket
import socketserver
import stat
import subprocess
import tempfile
import threading
import time
from urllib.parse import urlsplit

from container_entry import copy_streams

ROOT = Path(__file__).resolve().parent
IMAGE = 'redis@sha256:3811787313eba226a2ef38658c6ccb91cd5e110edc89c37767de373120a0e5a0'
LABEL = 'com.sub2api.devin-sandbox'
CID = re.compile(r'^[a-f0-9]{64}$')
SCOPE = re.compile(r'^[a-f0-9]{32}$')
_available = False
_proof = None


def docker_environment():
    # Select the managed local daemon; preserve its registry, proxy and CA setup.
    return {k:v for k,v in os.environ.items() if k not in {'DOCKER_HOST','DOCKER_CONTEXT','DOCKER_TLS','DOCKER_TLS_VERIFY','DOCKER_CERT_PATH'}}


def docker(arguments, timeout=20, **kwargs):
    return subprocess.run(['docker','--host=unix:///var/run/docker.sock',*arguments],
                          env=docker_environment(),capture_output=True,timeout=timeout,**kwargs)


def require_available():
    global _available,_proof
    if _available:
        return
    result=docker(['image','inspect',IMAGE,'--format','{{.Os}}/{{.Architecture}}'])
    if result.returncode or result.stdout.strip()!=b'linux/amd64':
        raise ValueError('rootless_sandbox_pinned_image_unavailable')
    _available=True
    try:
        with tempfile.TemporaryDirectory(prefix='devin-isolation-parent-') as parent, tempfile.TemporaryDirectory(prefix='devin-isolation-project-') as project:
            outside=Path(parent)/'synthetic-native-secret'; outside.write_text('synthetic')
            directory=Path(project); (directory/'workspace').mkdir(); (directory/'workspace/allowed').write_text('allowed')
            env={k:v for k,v in os.environ.items() if k.endswith('_PROXY') or k.endswith('_proxy') or k.startswith('CODEX_PROXY') or k in {'PATH','HOME','SSL_CERT_FILE','NODE_EXTRA_CA_CERTS','REQUESTS_CA_BUNDLE'}}
            script="import os,pathlib,sys; s=pathlib.Path('/proc/self/status').read_text(); assert '0000000000000000' in next(x for x in s.splitlines() if x.startswith('CapEff:')); assert next(x for x in s.splitlines() if x.startswith('NoNewPrivs:')).split()[-1]=='1'; assert pathlib.Path('allowed').read_text()=='allowed'; assert not pathlib.Path(sys.argv[1]).exists(); assert not pathlib.Path('/var/run/docker.sock').exists(); assert not pathlib.Path('/proc/%s/environ'%sys.argv[2]).exists(); assert not os.access(os.environ.get('HOME','/home/agent'),os.W_OK)"
            result=run(['/usr/bin/python3','-c',script,str(outside),str(os.getpid())],directory,env,timeout=15)
            if result.returncode: raise ValueError('rootless_sandbox_boundary_probe_failed')
            _proof={'mode':'networkless_container_fixed_unix_relay','image':IMAGE,'private_pid_namespace':True,
                    'read_only_rootfs':True,'no_new_privileges':True,'capabilities':'none','network':'none',
                    'private_parent_file_hidden':True,'parent_proc_hidden':True,'docker_socket_absent':True,'personal_home_writable':False}
    except BaseException:
        _available=False
        _proof=None
        raise


def evidence():
    require_available()
    return dict(_proof)


def private_registry(path):
    path=Path(path)
    parent=path.parent.stat()
    if parent.st_uid!=os.geteuid() or parent.st_mode&0o077:
        raise ValueError('sandbox_registry_parent_not_private')
    fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_APPEND|os.O_NOFOLLOW,0o600)
    info=os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid!=os.geteuid() or info.st_mode&0o077:
        os.close(fd)
        raise ValueError('sandbox_registry_not_private')
    return os.fdopen(fd,'a')


def record(path, row):
    with private_registry(path) as output:
        output.write(json.dumps(row)+'\n'); output.flush(); os.fsync(output.fileno())


def cleanup_registry(path, only_scope=None):
    if not path:
        return True
    try:
        fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
        with os.fdopen(fd,'rb') as source:
            info=os.fstat(source.fileno())
            if info.st_uid!=os.geteuid() or info.st_mode&0o077 or not stat.S_ISREG(info.st_mode):
                return False
            data=source.read(65537)
        if len(data)>65536:
            return False
        scopes=set()
        for line in data.splitlines():
            row=json.loads(line)
            if set(row)-{'scope','cid'} or not SCOPE.fullmatch(str(row.get('scope'))):
                return False
            if row.get('cid') and not CID.fullmatch(row['cid']):
                return False
            scopes.add(row['scope'])
        for scope in scopes:
            if only_scope is not None and scope!=only_scope:
                continue
            result=docker(['ps','-aq','--no-trunc','--filter','label='+LABEL+'='+scope])
            if result.returncode:
                return False
            for raw in result.stdout.decode().splitlines():
                if not CID.fullmatch(raw):
                    return False
                inspected=docker(['inspect','--format','{{index .Config.Labels "'+LABEL+'"}}',raw])
                if inspected.returncode or inspected.stdout.decode().strip()!=scope:
                    return False
                if docker(['rm','--force','--volumes',raw]).returncode:
                    return False
            remaining=docker(['ps','-aq','--filter','label='+LABEL+'='+scope])
            if remaining.returncode or remaining.stdout.strip():
                return False
        return True
    except (ValueError,TypeError,KeyError,OSError,subprocess.SubprocessError):
        return False


class FixedRelay:
    def __init__(self, path, url):
        parsed=urlsplit(url)
        if parsed.scheme!='http' or parsed.hostname!='127.0.0.1' or not parsed.port or parsed.username or parsed.query or parsed.fragment or parsed.path not in {'','/'}:
            raise ValueError('sandbox_requires_exact_loopback_ui')
        port=parsed.port
        self.lock=threading.Lock()
        self.connections=set()
        self.closed=False
        relay=self
        class Handler(socketserver.BaseRequestHandler):
            def handle(self):
                try:
                    with socket.create_connection(('127.0.0.1',port),timeout=5) as remote:
                        remote.settimeout(None)
                        with relay.lock:
                            if relay.closed: return
                            relay.connections.update((self.request,remote))
                        try: copy_streams(self.request,remote)
                        finally:
                            with relay.lock:
                                relay.connections.discard(self.request);relay.connections.discard(remote)
                except OSError:
                    return
        class Server(socketserver.ThreadingUnixStreamServer):
            daemon_threads=True
        self.server=Server(str(path),Handler)
        os.chmod(path,0o600)
        self.thread=threading.Thread(target=self.server.serve_forever,daemon=True)
        self.thread.start()
        self.port=port

    def close(self):
        with self.lock:
            if self.closed: return
            self.closed=True
            for connection in self.connections:
                try: connection.shutdown(socket.SHUT_RDWR)
                except OSError: pass
                connection.close()
        self.server.shutdown(); self.server.server_close(); self.thread.join(timeout=2)


def runtime_mounts():
    # Public executable/library roots only. Never mount /, /proc, /run, /var,
    # the repository root, Docker config/socket or the operator's personal home.
    roots=[('/usr','/usr'),('/usr/bin','/bin'),('/usr/lib','/lib'),('/usr/lib64','/lib64'),('/etc/ssl/certs','/etc/ssl/certs'),('/etc/hosts','/etc/hosts'),('/etc/passwd','/etc/passwd')]
    if Path('/etc/fonts').is_dir(): roots.append(('/etc/fonts','/etc/fonts'))
    for path in ['/etc/chromium','/etc/chromium.d']:
        if Path(path).is_dir(): roots.append((path,path))
    # GitHub's installed Chrome uses a /usr/bin symlink into this public
    # distribution. Mount that directory, never the surrounding /opt tree.
    if Path('/opt/google/chrome').is_dir():
        roots.append(('/opt/google/chrome','/opt/google/chrome'))
    import shutil
    node=Path(shutil.which('node')).resolve()
    if str(node).startswith('/opt/'):
        prefix=node.parent.parent
        roots.append((str(prefix),str(prefix)))
    for key in ['CODEX_PROXY_CERT','NODE_EXTRA_CA_CERTS','SSL_CERT_FILE','REQUESTS_CA_BUNDLE']:
        value=os.environ.get(key)
        if value and Path(value).is_file(): roots.append((value,value))
    return sorted(set(roots))


class ContainerProcess:
    def __init__(self,process,cid,scope,registry,control,relay):
        self.process,self.cid,self.scope,self.registry=process,cid,scope,registry
        self.control,self.relay=control,relay
        self.cleaned=False
        self.cleanup_lock=threading.RLock()
        self.exitcode=None

    def __getattr__(self,name):
        return getattr(self.process,name)

    @property
    def returncode(self):
        return self.exitcode if self.exitcode is not None else self.process.returncode

    def cleanup(self):
        with self.cleanup_lock:
            self._cleanup()

    def kill_owned(self):
        with self.cleanup_lock:
            if self.cleaned:
                return
            # Close egress first. Popen.kill checks the owned attach process;
            # never signal its stale process group after wait/reap or cleanup.
            if self.relay:
                self.relay.close()
            if self.process.poll() is None:
                self.process.kill()
            self._cleanup()

    def _cleanup(self):
        if self.cleaned:
            return
        try:
            # An attach client's zero exit is not a child outcome. Created,
            # still-running or unreadable containers must remain failures.
            self.exitcode=125
            result=docker(['inspect','--format','{{json .State}}',self.cid])
            if result.returncode==0:
                state=json.loads(result.stdout)
                if state.get('Status')=='exited' and state.get('Running') is False and type(state.get('ExitCode')) is int:
                    self.exitcode=state['ExitCode']
            if not cleanup_registry(self.registry,self.scope):
                raise ValueError('sandbox_container_cleanup_unverified')
        finally:
            if self.relay: self.relay.close()
            self.control.cleanup()
            self.cleaned=True

    def wait(self,*args,**kwargs):
        code=self.process.wait(*args,**kwargs)
        self.cleanup()
        return self.returncode

    def communicate(self,*args,**kwargs):
        result=self.process.communicate(*args,**kwargs)
        self.cleanup()
        return result


def popen(command,directory,env,*,gateway=None,on_start=None,**kwargs):
    require_available()
    directory=Path(directory).resolve()
    if not directory.is_dir() or directory.is_symlink():
        raise ValueError('sandbox_project_directory_required')
    control=tempfile.TemporaryDirectory(prefix='devin-container-control-')
    private=Path(control.name)
    registry=os.environ.get('DEVIN_JOURNEY_CONTAINER_REGISTRY') or str(private/'containers.jsonl')
    if Path(registry).resolve().is_relative_to(directory):
        raise ValueError('sandbox_registry_must_not_be_mounted')
    scope=secrets.token_hex(16)
    record(registry,{'scope':scope})  # durable before create, including interrupted create
    relay=None
    try:
        if gateway:
            relay=FixedRelay(private/'relay.sock',gateway)
        home=env.get('HOME','/home/agent')
        if home in {'/','/tmp','/usr'} or not home.startswith('/'):
            raise ValueError('unsupported_sandbox_home')
        args=['create','--interactive','--log-driver=none','--read-only','--cap-drop=ALL','--security-opt=no-new-privileges','--network=none',
              '--user',str(os.geteuid())+':'+str(os.getegid()),'--pids-limit=256','--memory=2g','--cpus=2',
              '--label',LABEL+'='+scope,'--workdir',str(directory/'workspace'),
              '--tmpfs','/tmp:rw,nosuid,nodev,mode=1777','--tmpfs','/data:ro,nosuid,nodev,mode=555',
              '--tmpfs',home+':ro,nosuid,nodev,mode=555','--entrypoint','/usr/bin/python3']
        mounts=runtime_mounts()+[(str(ROOT),str(ROOT))]
        for source,target in mounts:
            args+=['--mount','type=bind,src='+source+',dst='+target+',readonly']
        args+=['--mount','type=bind,src='+str(directory)+',dst='+str(directory)]
        if relay:
            args+=['--mount','type=bind,src='+str(private/'relay.sock')+',dst=/run/devin-fixed-relay.sock,readonly']
        child_env={k:v for k,v in env.items() if not k.startswith('DOCKER_') and k not in {'DEVIN_JOURNEY_CONTAINER_REGISTRY','DEVIN_JOURNEY_PROCESS_REGISTRY','DEVIN_JOURNEY_UI_PASSWORD','DEVIN_JOURNEY_CONTROL_SOCKET'}}
        for name in sorted(child_env): args+=['--env',name]
        args += [IMAGE,str(ROOT/'container_entry.py'),str(relay.port if relay else 0),*command]
        # Only the explicitly supplied child environment reaches the container.
        # Native credentials are stripped by the caller and checked again here.
        from run import SENSITIVE_ENV
        if SENSITIVE_ENV.intersection(child_env)-{'SUB2API_API_KEY'}:
            raise ValueError('native_or_controller_credential_in_sandbox_environment')
        created=subprocess.run(['docker','--host=unix:///var/run/docker.sock',*args],env={**docker_environment(),**child_env},capture_output=True,timeout=30)
        cid=created.stdout.decode().strip()
        if created.returncode or not CID.fullmatch(cid):
            raise ValueError('sandbox_container_create_failed')
        record(registry,{'scope':scope,'cid':cid})
        inspect=docker(['inspect',cid])
        if inspect.returncode: raise ValueError('sandbox_container_inspection_failed')
        config=json.loads(inspect.stdout)[0]; host=config['HostConfig']
        if host['Privileged'] or not host['ReadonlyRootfs'] or host['NetworkMode']!='none' or host.get('PidMode') not in {'','private'} or config['Config']['User']!=str(os.geteuid())+':'+str(os.getegid()) or host.get('CapAdd') or 'ALL' not in host['CapDrop'] or 'no-new-privileges' not in host['SecurityOpt'] or host['LogConfig']['Type']!='none':
            raise ValueError('sandbox_container_configuration_mismatch')
        # Complete CID is persisted and inspected before this command can start.
        proc=subprocess.Popen(['docker','--host=unix:///var/run/docker.sock','start','--attach','--interactive',cid],
                              cwd=directory/'workspace',env=docker_environment(),start_new_session=True,**kwargs)
        wrapped=ContainerProcess(proc,cid,scope,registry,control,relay)
        if on_start: on_start(wrapped)
        return wrapped
    except BaseException:
        cleanup_registry(registry,scope)
        if relay: relay.close()
        control.cleanup()
        raise


def collect_output(proc,command,*,input_data=None,timeout=15,output_limit=2*1024*1024):
    """Bound combined child output in the host, including model-written code.

    Container resource limits do not constrain Python's capture buffers. Drain
    both pipes without line buffering and stop before retaining excess bytes.
    """
    deadline=time.monotonic()+timeout
    buffers={proc.stdout:bytearray(),proc.stderr:bytearray()}
    pending=memoryview(input_data or b'')
    total=0
    try:
        with selectors.DefaultSelector() as ready:
            for stream in buffers:
                os.set_blocking(stream.fileno(),False)
                ready.register(stream,selectors.EVENT_READ)
            if pending:
                os.set_blocking(proc.stdin.fileno(),False)
                ready.register(proc.stdin,selectors.EVENT_WRITE)
            else:
                proc.stdin.close()
            while ready.get_map():
                remaining=deadline-time.monotonic()
                if remaining<=0: raise subprocess.TimeoutExpired(command,timeout)
                for key,_ in ready.select(remaining):
                    stream=key.fileobj
                    if stream is proc.stdin:
                        try: pending=pending[os.write(stream.fileno(),pending[:65536]):]
                        except BrokenPipeError: pending=pending[len(pending):]
                        if not pending:
                            ready.unregister(stream); stream.close()
                        continue
                    chunk=os.read(stream.fileno(),min(65536,output_limit-total+1))
                    if not chunk:
                        ready.unregister(stream)
                        continue
                    total+=len(chunk)
                    if total>output_limit:
                        raise ValueError('project_output_limit_exceeded')
                    buffers[stream].extend(chunk)
        proc.wait(timeout=max(.01,deadline-time.monotonic()))
        return subprocess.CompletedProcess(command,proc.returncode,bytes(buffers[proc.stdout]),bytes(buffers[proc.stderr]))
    except BaseException:
        # Removing the CID terminates all descendants even when the attached
        # Docker client has already exited. Never infer cleanup from its PID.
        # First release the attach pipes: a flooding child can otherwise block
        # the daemon's log copier while rm is trying to detach that same client.
        for stream in [proc.stdin,proc.stdout,proc.stderr]:
            stream.close()
        if proc.poll() is None:
            try: os.killpg(proc.pid,signal.SIGKILL)
            except ProcessLookupError: pass
        if isinstance(proc,ContainerProcess):
            proc.process.wait(timeout=5)
            proc.cleanup()
        proc.wait(timeout=5)
        raise
    finally:
        for stream in [proc.stdin,proc.stdout,proc.stderr]:
            stream.close()


def run(command,directory,env,*,input_data=None,timeout=15,on_start=None,output_limit=2*1024*1024):
    proc=popen(command,directory,env,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,on_start=on_start)
    return collect_output(proc,command,input_data=input_data,timeout=timeout,output_limit=output_limit)


if __name__=='__main__':
    import sys
    if sys.argv[1:]==['prepare']:
        status=docker(['image','inspect',IMAGE],timeout=15)
        if status.returncode:
            status=docker(['pull',IMAGE],timeout=180)
        if status.returncode:
            print('Pinned sandbox image unavailable; preserve Docker proxy/CA and inspect policy.'); raise SystemExit(2)
        require_available()
        print('Networkless client sandbox boundary probe passed; no native credentials or model requests.')
    else:
        raise SystemExit('Usage: container_sandbox.py prepare')
