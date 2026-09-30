#!/usr/bin/env python3
"""只发送本任务的包到 SSH 别名 50；异机核验通过后清理过期自有包。"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys

from common import (BackupError, LOCAL_ROOT, REMOTE_ROOT, RUN_RE, digest,
                    json_read, json_write, private_dir, prune_runs,
                    safe_error, validate_manifest)

SSH = ["ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "ConnectTimeout=10", "50"]
REMOTE_VERIFY = "/usr/local/lib/audit-core-backup/verify.py"


def remote(action, run_id, timeout):
    if action not in ("--prepare", "--finish") or not RUN_RE.fullmatch(run_id):
        raise BackupError("invalid_remote_action")
    p = subprocess.run([*SSH, "python3", REMOTE_VERIFY, action, run_id], capture_output=True, timeout=timeout)
    try:
        result = json.loads(p.stdout)
    except Exception:
        raise BackupError("remote_response_invalid")
    if p.returncode not in (0, 1) or (p.returncode and not result.get("published")):
        raise BackupError("remote_verification_failed")
    return result


def transfer(directory):
    directory = Path(directory)
    if directory.parent != LOCAL_ROOT or not RUN_RE.fullmatch(directory.name):
        raise BackupError("local_archive_scope_invalid")
    private_dir(directory)
    manifest = json_read(directory / "manifest.json")
    files = validate_manifest(manifest)
    if manifest["run_id"] != directory.name:
        raise BackupError("local_manifest_identity_invalid")
    if (directory / "replica.json").exists():
        raise BackupError("replica_receipt_already_exists")
    remote("--prepare", manifest["run_id"], 30)
    destination = "50:" + str(REMOTE_ROOT / (".pending-" + manifest["run_id"])) + "/"
    for name in sorted(files | {"manifest.json"}):
        p = subprocess.run(["scp", "-q", "-p", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "ConnectTimeout=10", str(directory / name), destination], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=900)
        if p.returncode:
            raise BackupError("archive_transfer_failed")
    report = remote("--finish", manifest["run_id"], 5400)
    if report.get("manifest_sha256") != digest(directory / "manifest.json"):
        raise BackupError("remote_manifest_checksum_mismatch")
    json_write(directory / "replica.json", report)
    if report["ok"]:
        report["local_retention"] = prune_runs(LOCAL_ROOT, manifest["run_id"])
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", required=True, type=Path)
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    if not args.apply:
        print(json.dumps({"preview": True, "run": str(args.run), "remote": {"host": "50", "root": str(REMOTE_ROOT)}}))
        return 0
    if os.geteuid() != 0:
        raise BackupError("root_required")
    os.umask(0o077)
    report = transfer(args.run)
    print(json.dumps(report, ensure_ascii=False))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:
        print(json.dumps({"ok": False, "error": safe_error(exc)}), file=sys.stderr)
        sys.exit(1)
