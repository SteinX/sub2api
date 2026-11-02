"""Public executable diagnostics inside the same CLI isolation; no path output."""
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess


def inspect_rg():
    link = Path('/usr/local/bin/rg')
    result = {'found': False, 'runnable': False, 'version': None, 'sha256': None,
              'error_category': None, 'known_symlink_broken': link.is_symlink() and not link.exists()}
    path = shutil.which('rg')
    if not path:
        result['error_category'] = 'executable_missing'
        return result
    result['found'] = True
    try:
        executable = Path(path)
        if executable.stat().st_size > 64 * 1024 * 1024:
            result['error_category'] = 'executable_oversized'
            return result
        fingerprint = hashlib.sha256()
        with executable.open('rb') as source:
            for chunk in iter(lambda: source.read(65536), b''):
                fingerprint.update(chunk)
        result['sha256'] = fingerprint.hexdigest()
        probe = subprocess.run([path, '--version'], capture_output=True, timeout=3)
        version = re.search(rb'^ripgrep ([0-9]+\.[0-9]+\.[0-9]+)(?:\s|$)', probe.stdout)
        result['runnable'] = probe.returncode == 0 and version is not None
        result['version'] = version[1].decode('ascii') if version else None
        if not result['runnable']:
            result['error_category'] = 'version_probe_failed'
    except FileNotFoundError:
        result['error_category'] = 'executable_missing'
    except PermissionError:
        result['error_category'] = 'access_denied'
    except subprocess.TimeoutExpired:
        result['error_category'] = 'version_probe_timeout'
    except OSError:
        result['error_category'] = 'executable_probe_error'
    return result


if __name__ == '__main__':
    print(json.dumps({'rg': inspect_rg()}, sort_keys=True))
