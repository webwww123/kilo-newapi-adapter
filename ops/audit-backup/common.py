#!/usr/bin/env python3
"""备份包的固定边界、私有文件与一致性校验。"""
import contextlib
import datetime as dt
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import stat
import tempfile
import time

SCHEMA = "audit-core/v1"
LOCAL_ROOT = Path("/opt/backups/audit-core")
REMOTE_ROOT = Path("/opt/backup-archive/node119/audit-core")
RUN_RE = re.compile(r"\d{8}T\d{6}Z-[a-f0-9]{8}\Z")
NAME_RE = re.compile(r"[a-z0-9_-]+\.(?:sqlite\.gz|sql\.gz|pgdump\.gz)\Z")


class BackupError(RuntimeError):
    pass


def safe_error(exc):
    # 只输出本模块定义的错误码，不输出数据库/外部命令异常原文。
    return str(exc) if type(exc) is BackupError else type(exc).__name__


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def deadline_check(deadline):
    if time.monotonic() >= deadline:
        raise BackupError("deadline_exceeded")


def regular(path, private=False):
    path = Path(path)
    s = path.lstat()
    if not stat.S_ISREG(s.st_mode) or s.st_nlink != 1:
        raise BackupError("not_single_regular_file")
    if path.resolve() != path.absolute():
        raise BackupError("symlink_in_path")
    if private and (s.st_uid != os.geteuid() or s.st_mode & 0o077):
        raise BackupError("not_private_file")
    return s


def fingerprint(path):
    s = regular(path)
    return s.st_dev, s.st_ino, s.st_size, s.st_mtime_ns


def private_dir(path):
    path = Path(path)
    if not path.exists():
        path.mkdir(mode=0o700, parents=True, exist_ok=False)
    s = path.lstat()
    if not stat.S_ISDIR(s.st_mode) or path.resolve() != path.absolute():
        raise BackupError("not_real_directory")
    if s.st_uid != os.geteuid() or s.st_mode & 0o077:
        raise BackupError("not_private_directory")
    return path


def digest(path, deadline=None):
    regular(path, private=True)
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for data in iter(lambda: f.read(4 * 1024 * 1024), b""):
            if deadline is not None:
                deadline_check(deadline)
            h.update(data)
    return h.hexdigest()


def json_write(path, value):
    path = Path(path)
    if path.exists() or path.is_symlink():
        raise BackupError("refuse_existing_file")
    with open(path, "x", encoding="utf8") as f:
        os.chmod(path, 0o600)
        json.dump(value, f, ensure_ascii=False, indent=2)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())


def json_read(path):
    regular(path, private=True)
    with open(path, encoding="utf8") as f:
        return json.load(f)


def compress(source, dest, deadline):
    with open(source, "rb") as src, open(dest, "xb") as out:
        os.chmod(dest, 0o600)
        with gzip.GzipFile(fileobj=out, mode="wb", compresslevel=1, mtime=0) as gz:
            for data in iter(lambda: src.read(4 * 1024 * 1024), b""):
                deadline_check(deadline)
                gz.write(data)
        out.flush()
        os.fsync(out.fileno())


def inflate(source, dest, deadline, max_bytes):
    total = 0
    with gzip.open(source, "rb") as src, open(dest, "xb") as out:
        os.chmod(dest, 0o600)
        for data in iter(lambda: src.read(4 * 1024 * 1024), b""):
            deadline_check(deadline)
            total += len(data)
            if total > max_bytes:
                raise BackupError("uncompressed_size_limit")
            out.write(data)
    return total


def sqlite_validate(path, tables, deadline, full=False):
    regular(path, private=True)
    with contextlib.closing(sqlite3.connect(Path(path).as_uri() + "?mode=ro&immutable=1", uri=True)) as conn:
        conn.execute("PRAGMA query_only=ON")
        conn.set_progress_handler(lambda: int(time.monotonic() >= deadline), 1000)
        check = "integrity_check" if full else "quick_check"
        if conn.execute("PRAGMA " + check).fetchall() != [("ok",)]:
            raise BackupError("sqlite_integrity_failed")
        counts = {}
        for table in tables:
            if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", table):
                raise BackupError("invalid_table_name")
            deadline_check(deadline)
            counts[table] = conn.execute('SELECT COUNT(*) FROM "' + table + '"').fetchone()[0]
    return {"check": check, "result": "ok", "table_counts": counts}


def sql_validate(path, deadline, minimum_tables=1):
    # 只在程序内部检查语法标记，不把 SQL 或业务行写入日志。
    table_count = 0
    tail, overlap = b"", b"\n"
    total = 0
    with gzip.open(path, "rb") as src:
        for data in iter(lambda: src.read(4 * 1024 * 1024), b""):
            deadline_check(deadline)
            total += len(data)
            chunk = overlap + data
            table_count += chunk.count(b"\nCREATE TABLE ")
            overlap = chunk[-13:]
            tail = (tail + data)[-2048:]
    if table_count < minimum_tables or b"-- Dump completed on " not in tail:
        raise BackupError("incomplete_mysql_dump")
    return {"check": "gzip_crc_and_mysql_completion", "table_definitions": table_count, "raw_bytes": total, "database_restore": "not_performed"}


def validate_manifest(manifest):
    if manifest.get("schema") != SCHEMA or manifest.get("source_node") != "119" or not RUN_RE.fullmatch(manifest.get("run_id", "")):
        raise BackupError("manifest_scope_invalid")
    remote = manifest.get("remote", {})
    if remote != {"host": "50", "root": str(REMOTE_ROOT)}:
        raise BackupError("replica_destination_invalid")
    ids, files = set(), set()
    results = manifest.get("results", [])
    if not results:
        raise BackupError("empty_manifest")
    for item in results:
        if item.get("id") in ids or not re.fullmatch(r"[a-z0-9_-]+", item.get("id", "")):
            raise BackupError("duplicate_or_invalid_target")
        ids.add(item["id"])
        if item.get("status") != "ok":
            continue
        name = item.get("file", "")
        if not NAME_RE.fullmatch(name) or name in files:
            raise BackupError("invalid_artifact_name")
        if name != item["id"] + {"sqlite": ".sqlite.gz", "mysql": ".sql.gz", "existing_mysql": ".sql.gz", "postgres": ".pgdump.gz"}.get(item.get("kind"), ""):
            raise BackupError("artifact_kind_invalid")
        if not re.fullmatch(r"[a-f0-9]{64}", item.get("sha256", "")):
            raise BackupError("invalid_digest")
        if not isinstance(item.get("raw_bytes"), int) or not 0 < item["raw_bytes"] <= 32 * 1024**3:
            raise BackupError("invalid_raw_size")
        files.add(name)
    return files


def verify_run(directory, full=False):
    directory = private_dir(directory)
    manifest = json_read(directory / "manifest.json")
    validate_manifest(manifest)
    results = []
    for item in manifest["results"]:
        result = {"id": item["id"], "status": "failed"}
        if item["status"] != "ok":
            result["error"] = "source_backup_failed"
            results.append(result)
            continue
        try:
            source = directory / item["file"]
            deadline = time.monotonic() + 600
            if source.stat().st_size != item["bytes"] or digest(source, deadline) != item["sha256"]:
                raise BackupError("artifact_checksum_failed")
            if item["kind"] == "sqlite":
                if shutil.disk_usage(directory).free < item["raw_bytes"] + 1024**3:
                    raise BackupError("restore_free_space_insufficient")
                with tempfile.TemporaryDirectory(prefix=".verify-", dir=directory) as scratch:
                    db = Path(scratch) / "restore.sqlite"
                    if inflate(source, db, deadline, item["raw_bytes"]) != item["raw_bytes"]:
                        raise BackupError("raw_size_mismatch")
                    validation = sqlite_validate(db, list(item["validation"]["table_counts"]), deadline, full)
                    if validation["table_counts"] != item["validation"]["table_counts"]:
                        raise BackupError("restored_counts_mismatch")
                    result["validation"] = validation
                    result["database_restore"] = "sqlite_readonly_passed"
            elif item["kind"] in ("mysql", "existing_mysql"):
                result["validation"] = sql_validate(source, deadline, item["validation"]["table_definitions"])
                if result["validation"]["raw_bytes"] != item["raw_bytes"]:
                    raise BackupError("raw_size_mismatch")
                result["database_restore"] = "not_performed"
            else:
                if shutil.disk_usage(directory).free < item["raw_bytes"] + 1024**3:
                    raise BackupError("restore_free_space_insufficient")
                with tempfile.TemporaryDirectory(prefix=".verify-", dir=directory) as scratch:
                    raw = Path(scratch) / "restore.pgdump"
                    if inflate(source, raw, deadline, item["raw_bytes"]) != item["raw_bytes"]:
                        raise BackupError("raw_size_mismatch")
                    with open(raw, "rb") as f:
                        if f.read(5) != b"PGDMP":
                            raise BackupError("postgres_header_invalid")
                result["validation"] = {"check": "gzip_crc_and_postgres_header"}
                result["database_restore"] = "not_performed"
            result["status"] = "ok"
        except Exception as exc:
            result["error"] = safe_error(exc)
        results.append(result)
    return {"schema": SCHEMA, "run_id": manifest["run_id"], "checked_at": now(), "manifest_sha256": digest(directory / "manifest.json"), "ok": all(x["status"] == "ok" for x in results), "results": results}


def prune_runs(root, fresh_run, remote=False):
    """只清本任务创建且已复制验收的包；当前成功范围须覆盖旧范围。"""
    root = private_dir(root)
    if not RUN_RE.fullmatch(fresh_run):
        raise BackupError("invalid_run_id")
    fresh_dir = root / fresh_run
    fresh = json_read(fresh_dir / "manifest.json")
    validate_manifest(fresh)
    receipt_name = "remote-verification.json" if remote else "replica.json"
    fresh_receipt = json_read(fresh_dir / receipt_name)
    if not fresh_receipt.get("ok") or fresh_receipt.get("manifest_sha256") != digest(fresh_dir / "manifest.json"):
        raise BackupError("fresh_replica_not_verified")
    if dt.datetime.now(dt.timezone.utc) - dt.datetime.fromisoformat(fresh["created_at"]) > dt.timedelta(hours=48):
        raise BackupError("fresh_replica_too_old")
    cutoff = dt.datetime.now(dt.timezone.utc) - dt.timedelta(days=7)
    removed, skipped = [], []
    for directory in root.iterdir():
        if directory.name == fresh_run or not RUN_RE.fullmatch(directory.name):
            continue
        try:
            private_dir(directory)
            manifest = json_read(directory / "manifest.json")
            files = validate_manifest(manifest)
            if manifest["run_id"] != directory.name or dt.datetime.fromisoformat(manifest["created_at"]) >= cutoff:
                continue
            if not set(manifest["scope"]).issubset(set(fresh["scope"])):
                continue
            receipt = json_read(directory / receipt_name)
            if not receipt.get("ok") or receipt.get("manifest_sha256") != digest(directory / "manifest.json"):
                continue
            allowed = files | {"manifest.json", receipt_name}
            actual = {p.name for p in directory.iterdir()}
            if actual != allowed:
                raise BackupError("unexpected_archive_file")
            fingerprints = {p.name: fingerprint(p) for p in directory.iterdir()}
            for name in sorted(allowed):
                p = directory / name
                regular(p, private=True)
                if fingerprint(p) != fingerprints[name]:
                    raise BackupError("archive_file_changed")
                p.unlink()
            directory.rmdir()
            removed.append(directory.name)
        except Exception:
            skipped.append(directory.name)
    return {"removed": removed, "skipped": skipped}
