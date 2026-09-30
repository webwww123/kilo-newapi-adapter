#!/usr/bin/env python3
"""异机只读恢复检查，以及本任务归档目录的固定边界操作。"""
import argparse
import json
import os
from pathlib import Path
import sys

from common import (BackupError, REMOTE_ROOT, RUN_RE, digest, json_read,
                    json_write, private_dir, prune_runs, safe_error,
                    validate_manifest, verify_run)


def prepare(run_id):
    if not RUN_RE.fullmatch(run_id):
        raise BackupError("invalid_run_id")
    root = private_dir(REMOTE_ROOT)
    pending = root / (".pending-" + run_id)
    if pending.exists() or (root / run_id).exists():
        raise BackupError("refuse_existing_archive")
    pending.mkdir(mode=0o700)
    return {"prepared": str(pending)}


def finish(run_id, full=False):
    if not RUN_RE.fullmatch(run_id):
        raise BackupError("invalid_run_id")
    pending = private_dir(REMOTE_ROOT / (".pending-" + run_id))
    manifest = json_read(pending / "manifest.json")
    allowed = validate_manifest(manifest) | {"manifest.json"}
    if manifest["run_id"] != run_id or {p.name for p in pending.iterdir()} != allowed:
        raise BackupError("incoming_archive_scope_invalid")
    report = verify_run(pending, full)
    json_write(pending / "remote-verification.json", report)
    final = REMOTE_ROOT / run_id
    if final.exists():
        raise BackupError("refuse_existing_archive")
    os.rename(pending, final)
    fd = os.open(REMOTE_ROOT, os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
    report["published"] = str(final)
    if report["ok"]:
        report["retention"] = prune_runs(REMOTE_ROOT, run_id, remote=True)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    action = parser.add_mutually_exclusive_group(required=True)
    action.add_argument("--directory", type=Path)
    action.add_argument("--prepare")
    action.add_argument("--finish")
    parser.add_argument("--full-check", action="store_true")
    args = parser.parse_args()
    os.umask(0o077)
    if args.directory:
        report = verify_run(args.directory, args.full_check)
    elif args.prepare:
        report = prepare(args.prepare)
    else:
        report = finish(args.finish, args.full_check)
    print(json.dumps(report, ensure_ascii=False))
    return 0 if report.get("ok", True) else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:
        print(json.dumps({"ok": False, "error": safe_error(exc)}), file=sys.stderr)
        sys.exit(1)
