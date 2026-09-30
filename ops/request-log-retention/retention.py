#!/usr/bin/env python3
"""Retain inactive Kilo daily request logs for 48 hours; preview by default."""
import argparse
import datetime as dt
import fcntl
import json
import os
from pathlib import Path
import re
import stat
import time

LOG_ROOT = Path('/var/log/kilo-newapi-adapter')
NAME = re.compile(r'^requests-(\d{4}-\d{2}-\d{2})\.jsonl$')
RETENTION_SECONDS = 48 * 60 * 60
LOCK = '/run/lock/kilo-request-log-retention.lock'


def fingerprint(path):
    info = path.lstat()
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns,
            info.st_ctime_ns, info.st_nlink, info.st_mode)


def opened_inodes(proc_root=Path('/proc')):
    opened = set()
    for proc in proc_root.iterdir():
        if not proc.name.isdigit():
            continue
        try:
            for fd in (proc / 'fd').iterdir():
                try:
                    info = fd.stat()
                    opened.add((info.st_dev, info.st_ino))
                except (FileNotFoundError, ProcessLookupError):
                    continue
        except (FileNotFoundError, ProcessLookupError):
            continue
        except PermissionError:
            raise RuntimeError('Cannot verify open files; cleanup refused') from None
    return opened


def candidates(root, now, opened):
    today = dt.datetime.fromtimestamp(now).date()
    cutoff_ns = int((now - RETENTION_SECONDS) * 1_000_000_000)
    selected = []
    for path in sorted(root.iterdir()):
        match = NAME.fullmatch(path.name)
        if not match:
            continue
        try:
            file_date = dt.date.fromisoformat(match[1])
        except ValueError:
            continue
        if file_date >= today:
            continue
        try:
            identity = fingerprint(path)
        except FileNotFoundError:
            continue
        if (not stat.S_ISREG(identity[6]) or identity[5] != 1
                or identity[3] >= cutoff_ns or identity[:2] in opened):
            continue
        selected.append((path, identity))
    return selected


def cleanup(root, *, now, apply, open_files=opened_inodes, before_unlink=None):
    root_info = root.lstat()
    if not stat.S_ISDIR(root_info.st_mode):
        raise RuntimeError('Log root must be a real directory')
    selected = candidates(root, now, open_files())
    deleted, skipped = [], []
    if apply:
        for path, expected in selected:
            if before_unlink is not None:
                before_unlink(path)
            try:
                actual = fingerprint(path)
            except FileNotFoundError:
                skipped.append({'name': path.name, 'reason': 'missing'})
                continue
            current_root = root.lstat()
            if ((current_root.st_dev, current_root.st_ino) != (root_info.st_dev, root_info.st_ino)
                    or not stat.S_ISDIR(current_root.st_mode)):
                raise RuntimeError('Log directory changed; cleanup refused')
            if actual != expected or actual[:2] in open_files():
                skipped.append({'name': path.name, 'reason': 'changed_or_open'})
                continue
            path.unlink()
            deleted.append({'name': path.name, 'bytes': expected[2]})
    return {'apply': apply, 'directory': str(root), 'retention_hours': 48,
            'candidates': [{'name': path.name, 'bytes': info[2]} for path, info in selected],
            'deleted': deleted, 'skipped': skipped,
            'deleted_bytes': sum(row['bytes'] for row in deleted)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    with open(LOCK, 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        result = cleanup(LOG_ROOT, now=time.time(), apply=args.apply)
    print(json.dumps(result, ensure_ascii=False), flush=True)


if __name__ == '__main__':
    try:
        main()
    except (OSError, RuntimeError) as error:
        print(json.dumps({'error': type(error).__name__, 'message': str(error)}), flush=True)
        raise SystemExit(1)
