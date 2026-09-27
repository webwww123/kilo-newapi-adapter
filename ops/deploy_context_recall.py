#!/usr/bin/env python3
"""Deploy an exact CI artifact to the main Kilo adapter; dry-run by default.

Run on the indexed service node after source/CI and Monitor checks. The old
executable remains untouched. All configuration is owned by this narrow release.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import time
import urllib.request

UNIT = 'kilo-newapi-adapter-migration.service'
OLD_BINARY = Path('/usr/local/bin/kilo-newapi-adapter-migration')
ROOT = Path('/usr/local/lib/kilo-context-recall')
DROPIN = Path('/etc/systemd/system') / (UNIT + '.d') / '90-context-recall.conf'
CONTROL = ROOT / 'control.json'
PORT = 28083


def digest(path):
    with Path(path).open('rb') as f:
        return hashlib.file_digest(f, 'sha256').hexdigest()


def run(*args):
    p = subprocess.run(args, capture_output=True, text=True, timeout=100)
    if p.returncode:
        # Service-manager diagnostics may contain environment values.
        raise RuntimeError('service_command_failed')
    return p.stdout.strip()


def process():
    pid = run('systemctl', 'show', UNIT, '-p', 'MainPID', '--value')
    if not pid.isdigit() or pid == '0':
        raise RuntimeError('service_not_running')
    return pid, digest('/proc/' + pid + '/exe')


def healthy():
    try:
        with urllib.request.urlopen(f'http://127.0.0.1:{PORT}/healthz', timeout=2) as r:
            return r.status == 200 and r.read(16) == b'ok\n'
    except Exception:
        return False


def atomic_write(path, value, mode=0o600):
    temporary = path.with_name(path.name + '.next-' + str(os.getpid()))
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    with os.fdopen(fd, 'wb') as f:
        f.write(value)
        f.flush()
        os.fsync(f.fileno())
    os.replace(temporary, path)


def policy(mode, percent):
    if mode not in ('shadow', 'apply') or type(percent) is not int or not 0 <= percent <= 100:
        raise ValueError('invalid_policy')
    return (json.dumps({'mode': mode, 'percent': percent}) + '\n').encode()


def wait_ready(expected):
    end = time.monotonic() + 40
    while time.monotonic() < end:
        try:
            pid, actual = process()
            if actual == expected and healthy():
                return pid
        except RuntimeError:
            pass
        time.sleep(.5)
    raise RuntimeError('service_readiness_failed')


def validate_manifest(m):
    if set(m) != {'commit', 'candidate_sha256', 'previous_sha256', 'ci_run', 'ci_job', 'ci_verified'}:
        raise ValueError('unexpected_manifest_shape')
    if not re.fullmatch(r'[0-9a-f]{40}', m['commit']):
        raise ValueError('full_commit_required')
    if any(not re.fullmatch(r'[0-9a-f]{64}', m[k]) for k in ('candidate_sha256', 'previous_sha256')):
        raise ValueError('full_digest_required')
    if m['ci_verified'] is not True or any(not re.fullmatch(r'[0-9a-f-]{36}', m[k]) for k in ('ci_run', 'ci_job')):
        raise ValueError('verified_ci_required')


def dropin_bytes(binary):
    return ('[Service]\nExecStart=\nExecStart=' + str(binary) +
            '\nEnvironment=CONTEXT_RECALL_CONFIG=' + str(CONTROL) + '\n').encode()


def restore(record):
    atomic_write(CONTROL, policy('shadow', 0))
    if DROPIN.read_bytes() != dropin_bytes(Path(record['candidate_binary'])):
        raise RuntimeError('rollback_configuration_drift')
    if digest(OLD_BINARY) != record['previous_sha256']:
        raise RuntimeError('rollback_binary_drift')
    DROPIN.unlink()  # Only the exact file created by this release.
    run('systemctl', 'daemon-reload')
    run('systemctl', 'restart', UNIT)
    return wait_ready(record['previous_sha256'])


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('action', choices=['deploy', 'rollback', 'verify', 'policy'])
    p.add_argument('--manifest', type=Path, required=True)
    p.add_argument('--artifact', type=Path)
    p.add_argument('--mode', choices=['shadow', 'apply'], default='shadow')
    p.add_argument('--percent', type=int, default=0)
    p.add_argument('--apply', action='store_true')
    a = p.parse_args()
    m = json.loads(a.manifest.read_text())
    validate_manifest(m)
    release = ROOT / m['commit']
    binary = release / 'kilo-newapi-adapter'
    record_file = release / 'deployment.json'
    pid, actual = process()
    if not healthy():
        raise RuntimeError('baseline_unhealthy')
    if a.action == 'deploy':
        if actual != m['previous_sha256'] or digest(OLD_BINARY) != actual:
            raise RuntimeError('baseline_binary_drift')
        if DROPIN.exists() or record_file.exists():
            raise RuntimeError('release_already_present')
        if a.artifact is None or digest(a.artifact) != m['candidate_sha256']:
            raise RuntimeError('artifact_digest_mismatch')
        # EnvironmentFile entries override Environment=; refuse silent override.
        environment = Path('/proc/' + pid + '/environ').read_bytes().split(b'\0')
        if any(v.startswith(b'CONTEXT_RECALL_CONFIG=') for v in environment):
            raise RuntimeError('existing_recall_configuration')
        print(json.dumps({'action': 'deploy', 'preflight': 'passed', 'apply': a.apply,
                          'old_pid': pid, 'previous_sha256': actual, 'commit': m['commit']}), flush=True)
        if not a.apply:
            return
        ROOT.mkdir(mode=0o700, parents=True, exist_ok=True)
        release.mkdir(mode=0o700)
        with a.artifact.open('rb') as src, binary.open('xb') as dst:
            shutil.copyfileobj(src, dst)
        binary.chmod(0o755)
        if digest(binary) != m['candidate_sha256']:
            raise RuntimeError('staged_digest_mismatch')
        atomic_write(CONTROL, policy('shadow', 0))
        record = {**m, 'candidate_binary': str(binary), 'old_pid': pid,
                  'deployed_at_epoch': int(time.time())}
        atomic_write(record_file, (json.dumps(record, indent=2) + '\n').encode())
        DROPIN.parent.mkdir(mode=0o755, exist_ok=True)
        if process()[1] != m['previous_sha256'] or DROPIN.exists():
            raise RuntimeError('baseline_changed_during_prepare')
        atomic_write(DROPIN, dropin_bytes(binary), 0o644)
        try:
            run('systemctl', 'daemon-reload')
            started = time.monotonic()
            run('systemctl', 'restart', UNIT)
            new_pid = wait_ready(m['candidate_sha256'])
            print(json.dumps({'deployed': True, 'pid': new_pid, 'recall_percent': 0,
                              'switch_seconds': round(time.monotonic() - started, 3)}), flush=True)
        except Exception:
            restored_pid = restore(record)
            print(json.dumps({'rolled_back': True, 'pid': restored_pid}), flush=True)
            raise
    else:
        record = json.loads(record_file.read_text())
        if any(record.get(k) != v for k, v in m.items()):
            raise RuntimeError('release_manifest_mismatch')
        if actual != m['candidate_sha256'] or DROPIN.read_bytes() != dropin_bytes(binary):
            raise RuntimeError('live_release_drift')
        if a.action == 'rollback':
            print(json.dumps({'action': 'rollback', 'apply': a.apply}), flush=True)
            if a.apply:
                print(json.dumps({'rolled_back': True, 'pid': restore(record)}))
        elif a.action == 'policy':
            value = policy(a.mode, a.percent)
            print(json.dumps({'action': 'policy', 'mode': a.mode, 'percent': a.percent, 'apply': a.apply}), flush=True)
            if a.apply:
                atomic_write(CONTROL, value)
        else:
            environment = Path('/proc/' + pid + '/environ').read_bytes().split(b'\0')
            if ('CONTEXT_RECALL_CONFIG=' + str(CONTROL)).encode() not in environment:
                raise RuntimeError('live_recall_configuration_missing')
            print(json.dumps({'verified': True, 'pid': pid, 'sha256': actual,
                              'policy': json.loads(CONTROL.read_text())}))


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        print(json.dumps({'failed': True, 'error_type': type(exc).__name__,
                          'reason': str(exc) if type(exc) in (RuntimeError, ValueError) else 'operation_failed'}))
        raise SystemExit(1)
