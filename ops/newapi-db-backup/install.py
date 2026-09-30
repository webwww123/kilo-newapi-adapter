#!/usr/bin/env python3
"""用已核验 CI 源码替换原备份入口，原文件私有保全；默认预览。"""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import stat
import subprocess
import tempfile

ENTRY = Path("/opt/backups/backup_newapi_db_no_logs.sh")
LEGACY_SHA = "755392ce47c80cac56b9e8dc3cc9a01189493980e775b060a53abe39444666be"
RUNTIME = Path("/usr/local/lib/newapi-db-backup")
ROLLBACK = Path("/opt/newapi-db-backup-rollback")


def safe_file(path):
    metadata = path.lstat()
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_uid != 0 or metadata.st_nlink != 1 or path.resolve() != path.absolute():
        raise RuntimeError("unsafe_file")


def legacy_password(text):
    matches = re.findall(r"^MYSQL_PASS=(.*)$", text, re.M)
    if len(matches) != 1:
        raise RuntimeError("legacy_credential_format_invalid")
    try:
        words = shlex.split(matches[0])
    except ValueError:
        raise RuntimeError("legacy_credential_format_invalid") from None
    if len(words) != 1 or "\n" in words[0]:
        raise RuntimeError("legacy_credential_format_invalid")
    return words[0]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    source = Path(__file__).resolve().parent
    commit = (source / "source-commit.txt").read_text().strip()
    if not re.fullmatch(r"[a-f0-9]{40}", commit):
        raise RuntimeError("ci_source_commit_invalid")
    for name in ("source-commit.txt", "backup.sh", "dump.py"):
        safe_file(source / name)
    safe_file(ENTRY)
    if hashlib.sha256(ENTRY.read_bytes()).hexdigest() != LEGACY_SHA:
        raise RuntimeError("legacy_entry_changed")
    # 本轮只安装新模块，共用模块必须已经与本次 CI 包逐字节相同。
    for name in ("common.py", "backup.py"):
        existing = Path("/usr/local/lib/audit-core-backup") / name
        safe_file(existing)
        if existing.read_bytes() != (source.parent / "audit-backup" / name).read_bytes():
            raise RuntimeError("shared_cleanup_source_mismatch")
    subprocess.run(["bash", "-n", str(source / "backup.sh")], check=True, capture_output=True)
    if not args.apply:
        print(json.dumps({"preview": True, "commit": commit, "scope": "backup_entry_and_dump_helper", "schema_only": ["logs", "error_request_traces"], "schedule_unchanged": True}))
        return
    if os.geteuid() != 0 or os.environ.get("NEWAPI_DB_BACKUP_CONFIRM_COMMIT") != commit:
        raise RuntimeError("exact_ci_commit_confirmation_required")
    if RUNTIME.exists() or RUNTIME.is_symlink() or ROLLBACK.exists() or ROLLBACK.is_symlink():
        raise RuntimeError("refuse_existing_installation")
    password = legacy_password(ENTRY.read_text())
    test = subprocess.run(["docker", "exec", "-i", "newapi-mysql", "sh", "-c", 'IFS= read -r supplied; [ "$supplied" = "${MYSQL_ROOT_PASSWORD:?}" ]'], input=(password + "\n").encode(), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
    del password
    if test.returncode:
        raise RuntimeError("container_credential_mismatch")
    for tool in ("python3", "docker", "flock", "gzip", "aws"):
        if shutil.which(tool) is None:
            raise RuntimeError("required_tool_missing")
    with open("/run/lock/audit-core-backup.lock", "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        # 旧入口未加锁，安装前另查仍在执行该入口的进程；不读取/输出其参数。
        for cmdline in Path("/proc").glob("[0-9]*/cmdline"):
            try:
                words = cmdline.read_bytes().split(b"\0")
                if str(ENTRY).encode() in words:
                    raise RuntimeError("legacy_backup_still_running")
            except (FileNotFoundError, PermissionError):
                continue
        if hashlib.sha256(ENTRY.read_bytes()).hexdigest() != LEGACY_SHA:
            raise RuntimeError("legacy_entry_changed")
        ROLLBACK.mkdir(mode=0o700)
        shutil.copyfile(ENTRY, ROLLBACK / "original.sh")
        (ROLLBACK / "original.sh").chmod(0o600)
        (ROLLBACK / "source-commit.txt").write_text(commit + "\n")
        RUNTIME.mkdir(mode=0o700)
        for name in ("dump.py", "source-commit.txt"):
            shutil.copyfile(source / name, RUNTIME / name)
            (RUNTIME / name).chmod(0o600)
        descriptor, temporary = tempfile.mkstemp(prefix=".newapi-db-backup-", dir=ENTRY.parent)
        try:
            with os.fdopen(descriptor, "wb") as output:
                output.write((source / "backup.sh").read_bytes())
                os.fchmod(output.fileno(), 0o700)
                output.flush()
                os.fsync(output.fileno())
            os.replace(temporary, ENTRY)
        finally:
            if Path(temporary).exists():
                Path(temporary).unlink()
    print(json.dumps({"installed": True, "commit": commit, "entry_sha256": hashlib.sha256(ENTRY.read_bytes()).hexdigest(), "rollback_saved": True, "database_restarted": False}))


if __name__ == "__main__":
    os.umask(0o077)
    try:
        main()
    except Exception as exc:
        # 不输出异常原文，避免文件或外部命令把真实秘密带入日志。
        print("install_failed " + (str(exc) if type(exc) is RuntimeError else type(exc).__name__))
        raise SystemExit(1)
