"""Private parent-owned process identities for project-runner cleanup."""
import json
import os
from pathlib import Path
import signal
import stat
import sys
import time


def start_ticks(pid):
    return int(Path(f'/proc/{pid}/stat').read_text().rsplit(')',1)[1].split()[19])


def group_members(pgid):
    members={}
    for path in Path('/proc').iterdir():
        if not path.name.isdigit(): continue
        try:
            values=(path/'stat').read_text().rsplit(')',1)[1].split()
            if int(values[2])==pgid and values[0]!='Z': members[int(path.name)]=int(values[19])
        except FileNotFoundError:
            continue
    return members


def register(proc):
    path=os.environ.get('DEVIN_JOURNEY_PROCESS_REGISTRY')
    if not path:
        return
    try:
        fd=os.open(path,os.O_WRONLY|os.O_APPEND|os.O_NOFOLLOW)
        with os.fdopen(fd,'a') as out:
            info=os.fstat(out.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid!=os.geteuid() or info.st_mode&0o077:
                raise ValueError('private_process_registry_required')
            out.write(json.dumps({'pid':proc.pid,'start_ticks':start_ticks(proc.pid)})+'\n')
            out.flush(); os.fsync(out.fileno())
    except Exception:
        os.killpg(proc.pid,signal.SIGKILL)
        proc.wait(timeout=5)
        raise


def cleanup_processes(path):
    try:
        fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
        with os.fdopen(fd,'rb') as source:
            info=os.fstat(source.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid!=os.geteuid() or info.st_mode&0o077: return False
            data=source.read(65537)
        if len(data)>65536: return False
        records=[json.loads(line) for line in data.splitlines()]
        if any(set(row)!={'pid','start_ticks'} or type(row['pid']) is not int or row['pid']<=1 or type(row['start_ticks']) is not int for row in records): return False
        verified=True
        for row in records:
            pid=row['pid']
            try:
                if start_ticks(pid)!=row['start_ticks']:
                    # Never signal a reused PID.
                    verified=False; continue
                original_members=group_members(pid)
                os.killpg(pid,signal.SIGTERM)
            except ProcessLookupError:
                continue
            except FileNotFoundError:
                if group_members(pid): verified=False
                continue  # no identity anchor remains; never signal a reused group
            deadline=time.monotonic()+2
            while time.monotonic()<deadline:
                if not group_members(pid): break
                time.sleep(.02)
            remaining=group_members(pid)
            if not remaining: continue
            # A still-live member with the same PID/start identity anchors the
            # original PGID even after its leader exits. Without an anchor,
            # refuse to claim cleanup or signal a possibly reused group.
            if not any(original_members.get(member)==ticks for member,ticks in remaining.items()):
                verified=False; continue
            try: os.killpg(pid,signal.SIGKILL)
            except ProcessLookupError: continue
            deadline=time.monotonic()+2
            while group_members(pid) and time.monotonic()<deadline: time.sleep(.02)
            if group_members(pid): verified=False
        return verified
    except (ValueError,OSError,KeyError,TypeError):
        return False


def cleanup(path,container_registry=None):
    from container_sandbox import cleanup_registry
    # Container cleanup still runs when a killed controller has left an invalid
    # process record. Docker client exit alone is never proof of termination.
    process_ok=cleanup_processes(path)
    container_ok=cleanup_registry(container_registry or os.environ.get('DEVIN_JOURNEY_CONTAINER_REGISTRY'))
    return process_ok and container_ok


if __name__=='__main__':
    raise SystemExit(0 if len(sys.argv)==2 and cleanup(sys.argv[1]) else 2)
