#!/usr/bin/env python3
"""Preview or apply the verified ActivityWatch TLS-only source repair."""
import argparse
from contextlib import closing
import fcntl
import hashlib
import http.client
import json
import os
import pwd
from pathlib import Path
import socket
import sqlite3
import ssl
import stat
import subprocess
import time
import uuid

SOURCE = Path('/opt/activitywatch-sync/server.py')
UNIT = Path('/etc/systemd/system/activitywatch-sync.service')
DATABASE = Path('/opt/activitywatch-sync/data/activitywatch.sqlite3')
ENVIRONMENT = Path('/opt/activitywatch-sync/config.env')
SERVICE = 'activitywatch-sync.service'
BACKUP_ROOT = Path('/opt/activitywatch-sync-deploy-backups')


def digest(data):
    return hashlib.sha256(data).hexdigest()


def regular(path, uid=None):
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1
            or (uid is not None and info.st_uid != uid)):
        raise RuntimeError('unsafe_file_metadata')
    return info


def metadata(info):
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns,
            info.st_ctime_ns, info.st_uid, info.st_gid, info.st_mode, info.st_nlink)


def command(arguments):
    result = subprocess.run(arguments, capture_output=True, timeout=45)
    if result.returncode:
        raise RuntimeError('service_command_failed')
    return result.stdout


def service_state():
    result = command(['systemctl', 'show', SERVICE, '-p', 'ActiveState',
                      '-p', 'SubState', '-p', 'MainPID'])
    return dict(line.split('=', 1) for line in result.decode().splitlines())


def root_directory(path):
    path = Path(path)
    fd = os.open('/', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in path.parts[1:]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        info = os.fstat(fd)
        if info.st_uid != 0 or info.st_mode & 0o022:
            raise RuntimeError('unsafe_source_directory')
        return fd
    except BaseException:
        os.close(fd)
        raise


def atomic_source(path, data, expected_info):
    compile(data, str(path), 'exec')
    directory_fd = root_directory(path.parent)
    temporary = '.server-rollout-' + uuid.uuid4().hex + '.tmp'
    try:
        if metadata(path.lstat()) != metadata(expected_info):
            raise RuntimeError('source_metadata_drift')
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                     0o600, dir_fd=directory_fd)
        with os.fdopen(fd, 'wb') as output:
            output.write(data)
            os.fchown(output.fileno(), expected_info.st_uid, expected_info.st_gid)
            os.fchmod(output.fileno(), stat.S_IMODE(expected_info.st_mode))
            output.flush()
            os.fsync(output.fileno())
        if metadata(path.lstat()) != metadata(expected_info):
            raise RuntimeError('source_metadata_drift')
        current_directory = root_directory(path.parent)
        try:
            if ((os.fstat(current_directory).st_dev, os.fstat(current_directory).st_ino)
                    != (os.fstat(directory_fd).st_dev, os.fstat(directory_fd).st_ino)):
                raise RuntimeError('source_directory_drift')
        finally:
            os.close(current_directory)
        os.replace(temporary, path.name, src_dir_fd=directory_fd, dst_dir_fd=directory_fd)
        os.fsync(directory_fd)
    finally:
        try:
            os.unlink(temporary, dir_fd=directory_fd)
        except FileNotFoundError:
            pass
        os.close(directory_fd)


def backup_sqlite(source, destination, deadline_seconds=30):
    regular(source)
    started = time.monotonic()
    def progress(status, remaining, total):
        if time.monotonic() - started > deadline_seconds:
            raise TimeoutError('sqlite_backup_deadline')
    fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    os.close(fd)
    try:
        with closing(sqlite3.connect(source.as_uri() + '?mode=ro', uri=True, timeout=1)) as reader:
            reader.execute('PRAGMA busy_timeout=1000')
            with closing(sqlite3.connect(destination, timeout=1)) as writer:
                reader.backup(writer, pages=128, progress=progress, sleep=0.05)
                if writer.execute('PRAGMA quick_check').fetchall() != [('ok',)]:
                    raise RuntimeError('sqlite_backup_integrity_failed')
        fd = os.open(destination, os.O_RDONLY | os.O_NOFOLLOW)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    except BaseException:
        destination.unlink(missing_ok=True)
        raise


def health(port=18765, concurrent_stall=False):
    stalled = None
    try:
        if concurrent_stall:
            stalled = socket.create_connection(('127.0.0.1', port), timeout=2)
            time.sleep(0.05)
        with closing(http.client.HTTPSConnection('127.0.0.1', port, timeout=2,
                     context=ssl._create_unverified_context())) as connection:
            connection.request('GET', '/health')
            response = connection.getresponse()
            payload = json.loads(response.read(8192))
            return response.status == 200 and payload.get('service') == 'activitywatch-sync' and payload.get('status') == 'ok'
    finally:
        if stalled is not None:
            stalled.close()


def wait_healthy(concurrent_stall=False):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        try:
            if health(concurrent_stall=concurrent_stall):
                return True
        except (OSError, ValueError, http.client.HTTPException):
            pass
        time.sleep(0.3)
    return False


def apply_repair(package, baseline, new_code):
    old_info = regular(SOURCE, 0)
    old_code = SOURCE.read_bytes()
    if digest(old_code) != baseline['source_sha256'] or digest(UNIT.read_bytes()) != baseline['unit_sha256']:
        raise RuntimeError('production_source_or_unit_drift')
    before = service_state()
    if before['ActiveState'] != 'active' or before['SubState'] != 'running':
        raise RuntimeError('expected_live_service_missing')
    environment_info = metadata(regular(ENVIRONMENT))  # Metadata only; never read secrets.
    database_info = regular(DATABASE, pwd.getpwnam('activitywatch-sync').pw_uid)
    if stat.S_IMODE(database_info.st_mode) != 0o600:
        raise RuntimeError('unexpected_database_permissions')
    if not BACKUP_ROOT.exists():
        BACKUP_ROOT.mkdir(mode=0o700)
    root_fd = root_directory(BACKUP_ROOT)
    os.close(root_fd)
    if stat.S_IMODE(BACKUP_ROOT.stat().st_mode) != 0o700:
        raise RuntimeError('backup_directory_not_root_only')
    storage = os.statvfs(BACKUP_ROOT)
    if storage.f_bavail * storage.f_frsize < database_info.st_size * 2 + 128 * 1024 * 1024:
        raise RuntimeError('insufficient_backup_space')
    backup = BACKUP_ROOT / (time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()) + '-' + uuid.uuid4().hex)
    backup.mkdir(mode=0o700)
    for name, contents in [('server.py.before', old_code), ('activitywatch-sync.service.before', UNIT.read_bytes())]:
        fd = os.open(backup / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'wb') as output:
            output.write(contents)
            output.flush()
            os.fsync(output.fileno())
    backup_sqlite(DATABASE, backup / 'activitywatch.sqlite3')
    manifest = {'old_sha256': digest(old_code), 'new_sha256': digest(new_code),
                'source_mode': stat.S_IMODE(old_info.st_mode), 'source_uid': old_info.st_uid,
                'source_gid': old_info.st_gid, 'old_pid': before['MainPID'],
                'source_commit': (package / 'source-commit.txt').read_text().strip()}
    (backup / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    (backup / 'manifest.json').chmod(0o600)
    if (service_state() != before or digest(UNIT.read_bytes()) != baseline['unit_sha256']
            or metadata(regular(ENVIRONMENT)) != environment_info):
        raise RuntimeError('production_state_drift_before_replace')
    atomic_source(SOURCE, new_code, old_info)
    try:
        command(['systemctl', 'restart', SERVICE])
        if not wait_healthy(concurrent_stall=True):
            raise RuntimeError('patched_service_health_failed')
        after = service_state()
        if (after['ActiveState'] != 'active' or after['MainPID'] == before['MainPID']
                or digest(UNIT.read_bytes()) != baseline['unit_sha256']
                or digest(SOURCE.read_bytes()) != digest(new_code)
                or metadata(regular(ENVIRONMENT)) != environment_info):
            raise RuntimeError('patched_service_verification_failed')
    except BaseException as error:
        if (digest(SOURCE.read_bytes()) != digest(new_code) or digest(UNIT.read_bytes()) != baseline['unit_sha256']
                or metadata(regular(ENVIRONMENT)) != environment_info):
            raise RuntimeError('rollback_refused_due_to_concurrent_change') from error
        atomic_source(SOURCE, old_code, regular(SOURCE, 0))
        command(['systemctl', 'restart', SERVICE])
        restored = wait_healthy()
        print(json.dumps({'applied': False, 'rolled_back': True,
                          'restored_health': restored, 'backup_directory': str(backup)}), flush=True)
        raise RuntimeError('deployment_failed_and_code_rolled_back') from error
    return {'applied': True, 'health': True, 'stalled_client_regression': True,
            'service': SERVICE, 'new_pid': after['MainPID'], 'backup_directory': str(backup),
            'database_restored': False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    package = Path(__file__).resolve().parent
    baseline = json.loads((package / 'baseline.json').read_text())
    code = (package / 'server.py').read_bytes()
    compile(code, 'server.py', 'exec')
    current = regular(SOURCE, 0)
    regular(UNIT, 0)
    if digest(SOURCE.read_bytes()) != baseline['source_sha256'] or digest(UNIT.read_bytes()) != baseline['unit_sha256']:
        raise RuntimeError('production_source_or_unit_drift')
    regular(DATABASE)
    if not args.apply:
        print(json.dumps({'mode': 'preview', 'source_sha256': digest(code),
              'source_mode_preserved': stat.S_IMODE(current.st_mode),
              'service_to_restart': SERVICE, 'database_consistency_backup_required': True,
              'changes': 'TLS handshake in request thread; 15s handshake, 60s per-read idle'}))
        return
    if os.geteuid() != 0:
        raise RuntimeError('root_required')
    commit = (package / 'source-commit.txt').read_text().strip()
    if (len(commit) != 40 or any(c not in '0123456789abcdef' for c in commit)
            or os.environ.get('ACTIVITYWATCH_SYNC_CONFIRM_COMMIT') != commit):
        raise RuntimeError('exact_verified_ci_commit_confirmation_required')
    os.umask(0o077)
    fd = os.open('/run/lock/activitywatch-sync-rollout.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode) or os.fstat(fd).st_uid != 0 or os.fstat(fd).st_nlink != 1:
            raise RuntimeError('unsafe_rollout_lock')
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        print(json.dumps(apply_repair(package, baseline, code)), flush=True)
    finally:
        os.close(fd)


if __name__ == '__main__':
    try:
        main()
    except (OSError, RuntimeError, ValueError, sqlite3.Error) as error:
        print(json.dumps({'error': type(error).__name__, 'reason': str(error)}), flush=True)
        raise SystemExit(1)
