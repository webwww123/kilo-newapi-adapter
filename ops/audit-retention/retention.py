#!/usr/bin/env python3
"""Bound CPA migration debug logs and retain closed Inkling daily logs."""
import argparse
import datetime as dt
import fcntl
import gzip
import json
import os
from pathlib import Path
import re
import signal
import stat
import time
import uuid

PRIVATE_ROOT = Path('/var/log/private')
INKLING_ROOT = Path('/var/log/kilo-newapi-adapter-inkling-migration')
NEWAPI_ROOT = Path('/root/new-api-official-latest/logs')
CPA_NAME = re.compile(r'^cpa-[a-z0-9][a-z0-9-]*-migration$')
DAILY_NAME = re.compile(r'^requests-(\d{4}-\d{2}-\d{2})\.jsonl$')
NEWAPI_NAME = re.compile(r'^oneapi-(\d{14})\.log$')
ARCHIVE_NAME = re.compile(r'^debug-\d{8}T\d{6}Z-[0-9a-f]{32}\.jsonl\.gz$')
PARTIAL_NAME = re.compile(r'^\.debug-[0-9a-f]{32}\.tmp$')
DEBUG_UID = 65534  # Verified live service UID; never change live ownership.
DEBUG_LIMIT = 128 * 1024 * 1024
KEEP_ARCHIVES = 3
RETENTION_SECONDS = 48 * 3600
NOFOLLOW = os.O_NOFOLLOW | os.O_CLOEXEC


def directory(path):
    """Open every component without following symlinks."""
    path = Path(path)
    if not path.is_absolute():
        raise RuntimeError('absolute_directory_required')
    fd = os.open('/', os.O_RDONLY | os.O_DIRECTORY | NOFOLLOW)
    try:
        for part in path.parts[1:]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        return fd
    except BaseException:
        os.close(fd)
        raise


def directory_ok(fd, uid=0, mode=0o700):
    info = os.fstat(fd)
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != uid
            or stat.S_IMODE(info.st_mode) != mode):
        raise RuntimeError('unsafe_directory_metadata')
    return info


def file_ok(info, uid, mode=0o600):
    return (stat.S_ISREG(info.st_mode) and info.st_uid == uid
            and info.st_nlink == 1 and stat.S_IMODE(info.st_mode) == mode)


def identity(info):
    return (info.st_dev, info.st_ino, info.st_uid, info.st_mode, info.st_nlink)


def snapshot(info):
    return identity(info) + (info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def same_entry(parent_fd, name, expected):
    actual = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
    if identity(actual) != identity(expected):
        raise RuntimeError('entry_changed')
    return actual


def root_unchanged(path, fd, mode=0o700):
    check = directory(path)
    try:
        if identity(directory_ok(check, mode=mode)) != identity(directory_ok(fd, mode=mode)):
            raise RuntimeError('root_changed')
    finally:
        os.close(check)


def process_files(proc_root=Path('/proc')):
    """Only inode and flags metadata; no command lines or log content."""
    result = {}
    for proc in proc_root.iterdir():
        if not proc.name.isdigit():
            continue
        try:
            for fd in (proc / 'fd').iterdir():
                try:
                    info = fd.stat()
                    lines = (proc / 'fdinfo' / fd.name).read_text().splitlines()
                    flags = int(next(line.split()[1] for line in lines
                                     if line.startswith('flags:')), 8)
                    result.setdefault((info.st_dev, info.st_ino), []).append(flags)
                except (FileNotFoundError, ProcessLookupError):
                    continue
        except (FileNotFoundError, ProcessLookupError):
            continue
        except PermissionError:
            raise RuntimeError('cannot_verify_process_files') from None
    return result


def append_writers_only(info, process_scan):
    for flags in process_scan().get((info.st_dev, info.st_ino), []):
        if flags & os.O_ACCMODE != os.O_RDONLY and not flags & os.O_APPEND:
            raise RuntimeError('non_append_writer_cleanup_refused')


def cleanup_daily(root, *, now, apply=False, process_scan=process_files,
                  before_unlink=None, flavor='inkling'):
    if flavor not in ['inkling', 'newapi']:
        raise RuntimeError('unknown_daily_log_flavor')
    pattern = DAILY_NAME if flavor == 'inkling' else NEWAPI_NAME
    root_mode, file_mode = (0o700, 0o600) if flavor == 'inkling' else (0o755, 0o644)
    fd = directory(root)
    try:
        directory_ok(fd, mode=root_mode)
        today = dt.datetime.fromtimestamp(now).date()
        cutoff = int((now - RETENTION_SECONDS) * 1_000_000_000)
        opened = process_scan()
        candidates, deleted, skipped = [], [], []
        for name in sorted(os.listdir(fd)):
            match = pattern.fullmatch(name)
            if not match:
                continue
            try:
                day = (dt.date.fromisoformat(match[1]) if flavor == 'inkling'
                       else dt.datetime.strptime(match[1], '%Y%m%d%H%M%S').date())
                info = os.stat(name, dir_fd=fd, follow_symlinks=False)
            except (ValueError, FileNotFoundError):
                continue
            if (day >= today - dt.timedelta(days=1) or not file_ok(info, 0, file_mode)
                    or info.st_mtime_ns >= cutoff
                    or (info.st_dev, info.st_ino) in opened):
                continue
            candidates.append({'name': name, 'bytes': info.st_size})
            if not apply:
                continue
            if before_unlink:
                before_unlink(Path(root) / name)
            try:
                root_unchanged(root, fd, mode=root_mode)
                actual = same_entry(fd, name, info)
                if (snapshot(actual) != snapshot(info)
                        or (info.st_dev, info.st_ino) in process_scan()):
                    skipped.append({'name': name, 'reason': 'changed_or_open'})
                    continue
                os.unlink(name, dir_fd=fd)
                deleted.append({'name': name, 'bytes': info.st_size})
            except FileNotFoundError:
                skipped.append({'name': name, 'reason': 'missing'})
        return {'apply': apply, 'retention_hours': 48, 'candidates': candidates,
                'deleted': deleted, 'skipped': skipped,
                'deleted_bytes': sum(item['bytes'] for item in deleted)}
    finally:
        os.close(fd)


def archive_directory(parent_fd, name):
    try:
        os.mkdir(name, 0o700, dir_fd=parent_fd)
    except FileExistsError:
        pass
    fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | NOFOLLOW, dir_fd=parent_fd)
    try:
        directory_ok(fd)
        return fd
    except BaseException:
        os.close(fd)
        raise


def archive_files(fd):
    result = []
    for name in os.listdir(fd):
        info = os.stat(name, dir_fd=fd, follow_symlinks=False)
        if not file_ok(info, 0) or not (ARCHIVE_NAME.fullmatch(name) or PARTIAL_NAME.fullmatch(name)):
            raise RuntimeError('unexpected_archive_entry')
        result.append((name, info))
    return result


def prune_archives(fd, *, now, process_scan):
    entries = archive_files(fd)
    opened = process_scan()
    archives = sorted((row for row in entries if ARCHIVE_NAME.fullmatch(row[0])),
                      key=lambda row: row[0], reverse=True)
    removable = archives[KEEP_ARCHIVES:]
    removable += [row for row in entries if PARTIAL_NAME.fullmatch(row[0])
                  and row[1].st_mtime < now - 3600]
    for name, expected in removable:
        actual = same_entry(fd, name, expected)
        if (snapshot(actual) == snapshot(expected)
                and (actual.st_dev, actual.st_ino) not in opened):
            os.unlink(name, dir_fd=fd)
    os.fsync(fd)


def copy_tail(source_fd, archive_fd, name, size, limit):
    out_fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | NOFOLLOW,
                     0o600, dir_fd=archive_fd)
    with os.fdopen(out_fd, 'wb') as raw:
        with gzip.GzipFile(filename='', mode='wb', compresslevel=1, mtime=0,
                           fileobj=raw) as zipped:
            offset, remaining = max(0, size - limit), min(size, limit)
            while remaining:
                chunk = os.pread(source_fd, min(1024 * 1024, remaining), offset)
                if not chunk:
                    raise RuntimeError('source_shrank_during_copy')
                zipped.write(chunk)
                offset += len(chunk)
                remaining -= len(chunk)
        raw.flush()
        os.fsync(raw.fileno())


def rotate_one(root, root_fd, name, *, now, apply, limit,
               process_scan, before_truncate=None, copier=copy_tail):
    child_fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | NOFOLLOW, dir_fd=root_fd)
    source_fd, retained_fd, archive_fd = None, None, None
    temp_name, finished_name = None, None
    truncated = False
    try:
        child_info = directory_ok(child_fd, DEBUG_UID)
        source_fd = os.open('debug.jsonl', os.O_RDWR | os.O_APPEND | NOFOLLOW, dir_fd=child_fd)
        source = os.fstat(source_fd)
        if not file_ok(source, DEBUG_UID):
            raise RuntimeError('unsafe_debug_file_metadata')
        same_entry(child_fd, 'debug.jsonl', source)
        if source.st_size <= limit:
            return {'service': name, 'action': 'within_limit', 'bytes': source.st_size}
        append_writers_only(source, process_scan)
        if not apply:
            return {'service': name, 'action': 'would_rotate', 'bytes': source.st_size,
                    'retained_tail_bytes': min(source.st_size, limit)}
        retained_fd = archive_directory(root_fd, 'cpa-debug-retained')
        archive_fd = archive_directory(retained_fd, name)
        prune_archives(archive_fd, now=now, process_scan=process_scan)
        entries = archive_files(archive_fd)
        if (len(entries) > KEEP_ARCHIVES
                or any(info.st_size > limit + 1024 * 1024 for _, info in entries)):
            raise RuntimeError('archive_budget_exceeded')
        storage = os.fstatvfs(archive_fd)
        if storage.f_bavail * storage.f_frsize < limit + 1024 * 1024 + 64 * 1024 * 1024:
            raise RuntimeError('insufficient_archive_space')
        temp_name = '.debug-' + uuid.uuid4().hex + '.tmp'
        copier(source_fd, archive_fd, temp_name, source.st_size, limit)
        archived = os.stat(temp_name, dir_fd=archive_fd, follow_symlinks=False)
        if not file_ok(archived, 0) or archived.st_size > limit + 1024 * 1024:
            raise RuntimeError('archive_size_budget_exceeded')
        finished_name = 'debug-' + dt.datetime.fromtimestamp(now, dt.timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex + '.jsonl.gz'
        os.rename(temp_name, finished_name, src_dir_fd=archive_fd, dst_dir_fd=archive_fd)
        temp_name = None
        os.fsync(archive_fd)
        if before_truncate:
            before_truncate(Path(root) / name / 'debug.jsonl')
        root_unchanged(root, root_fd)
        same_entry(root_fd, name, child_info)
        directory_ok(child_fd, DEBUG_UID)
        same_entry(root_fd, 'cpa-debug-retained', os.fstat(retained_fd))
        same_entry(retained_fd, name, os.fstat(archive_fd))
        current = same_entry(child_fd, 'debug.jsonl', source)
        current_fd = os.fstat(source_fd)
        if (snapshot(current) != snapshot(current_fd) or current.st_size < source.st_size
                or current.st_mtime_ns < source.st_mtime_ns
                or (current.st_size == source.st_size and snapshot(current) != snapshot(source))):
            raise RuntimeError('source_changed_during_copy')
        append_writers_only(current, process_scan)
        os.ftruncate(source_fd, 0)
        truncated = True
        os.fsync(source_fd)
        prune_archives(archive_fd, now=now, process_scan=process_scan)
        return {'service': name, 'action': 'rotated', 'bytes_before': source.st_size,
                'retained_tail_bytes': min(source.st_size, limit),
                'archive_bytes': archived.st_size,
                'copytruncate_window_bytes': current.st_size - source.st_size,
                'inode': source.st_ino}
    finally:
        if archive_fd is not None:
            for temporary in [temp_name, finished_name if not truncated else None]:
                if temporary:
                    try:
                        info = os.stat(temporary, dir_fd=archive_fd, follow_symlinks=False)
                        if file_ok(info, 0):
                            os.unlink(temporary, dir_fd=archive_fd)
                    except FileNotFoundError:
                        pass
        for fd in [source_fd, child_fd, archive_fd, retained_fd]:
            if fd is not None:
                os.close(fd)


def rotate_debug(root, *, now, apply=False, limit=DEBUG_LIMIT,
                 process_scan=process_files, before_truncate=None, copier=copy_tail):
    root_fd = directory(root)
    try:
        directory_ok(root_fd)
        rows = []
        for name in sorted(os.listdir(root_fd)):
            if not CPA_NAME.fullmatch(name):
                continue
            try:
                rows.append(rotate_one(root, root_fd, name, now=now, apply=apply,
                                       limit=limit, process_scan=process_scan,
                                       before_truncate=before_truncate, copier=copier))
            except FileNotFoundError:
                rows.append({'service': name, 'action': 'missing_debug_file'})
        return {'apply': apply, 'limit_bytes': limit, 'keep_archives': KEEP_ARCHIVES,
                'files': rows}
    finally:
        os.close(root_fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kind', choices=['inkling', 'debug', 'newapi'], required=True)
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise RuntimeError('root_required')
    lock_fd = os.open('/run/lock/audit-log-retention-' + args.kind + '.lock',
                      os.O_RDWR | os.O_CREAT | NOFOLLOW, 0o600)
    try:
        if not file_ok(os.fstat(lock_fd), 0):
            raise RuntimeError('unsafe_lock_metadata')
        fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.kind == 'inkling':
            result = cleanup_daily(INKLING_ROOT, now=time.time(), apply=args.apply)
        elif args.kind == 'newapi':
            result = cleanup_daily(NEWAPI_ROOT, now=time.time(), apply=args.apply, flavor='newapi')
        else:
            result = rotate_debug(PRIVATE_ROOT, now=time.time(), apply=args.apply)
        print(json.dumps(result, ensure_ascii=False), flush=True)
    finally:
        os.close(lock_fd)


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, lambda signum, frame: (_ for _ in ()).throw(SystemExit(1)))
    try:
        main()
    except (OSError, RuntimeError) as error:
        print(json.dumps({'error': type(error).__name__, 'reason': str(error)}), flush=True)
        raise SystemExit(1)
