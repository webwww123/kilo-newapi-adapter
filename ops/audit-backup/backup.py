#!/usr/bin/env python3
"""串行在线备份；默认只预览，显式 --apply 才创建新备份。"""
import argparse
import contextlib
import datetime as dt
import fcntl
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
import uuid

from common import (BackupError, LOCAL_ROOT, REMOTE_ROOT, SCHEMA, compress,
                    deadline_check, digest, fingerprint, json_write, now,
                    private_dir, regular, safe_error, sql_validate, sqlite_validate)

DEFAULT_CONFIG = Path(__file__).with_name("targets.json")
SQLITE_SOURCES = {
    "redeem": "/root/.gemini/antigravity/xiaoen-redeem-go/redeem.db",
    "dujiao": "/opt/dujiao-next/data/db/dujiao.db",
    "tagi": "/opt/tagiapi-data/site.sqlite",
    "weiwan": "/var/lib/weiwan-hub/site.sqlite",
    "monitor": "/root/.gemini/antigravity/newapi-monitor-go/monitor.db",
}


def load_config(path):
    regular(path)
    config = json.loads(Path(path).read_text())
    if config.get("source_node") != "119" or config.get("local_root") != str(LOCAL_ROOT) or config.get("remote") != {"host": "50", "root": str(REMOTE_ROOT)} or config.get("retention_days") != 7:
        raise BackupError("configuration_scope_invalid")
    ids = set()
    for item in config["targets"]:
        if not re.fullmatch(r"[a-z0-9_-]+", item["id"]) or item["id"] in ids:
            raise BackupError("invalid_or_duplicate_target")
        ids.add(item["id"])
        if not 30 <= item["timeout_seconds"] <= 900:
            raise BackupError("invalid_timeout")
        if item["kind"] == "mysql" and (item.get("container"), item.get("database")) != ("99code-mysql", "newapi_523"):
            raise BackupError("mysql_not_whitelisted")
        if item["kind"] == "mysql" and item["id"] != "99code":
            raise BackupError("mysql_identity_invalid")
        if item["kind"] == "postgres" and (item.get("container"), item.get("database")) != ("mailflow-postgres", "mailflow"):
            raise BackupError("postgres_not_whitelisted")
        if item["kind"] == "postgres" and item["id"] != "mailflow":
            raise BackupError("postgres_identity_invalid")
        if item["kind"] == "sqlite" and SQLITE_SOURCES.get(item["id"]) != item.get("source"):
            raise BackupError("sqlite_not_whitelisted")
        if item["kind"] == "existing_mysql" and (item["id"], item.get("source_dir")) != ("xiaoenai", "/opt/xiaoenai-backups/mysql"):
            raise BackupError("existing_backup_not_whitelisted")
        if item["kind"] not in ("sqlite", "mysql", "postgres", "existing_mysql"):
            raise BackupError("unknown_backup_kind")
    return config


def sqlite_backup(item, raw, deadline):
    source = Path(item["source"])
    before = regular(source)
    with open(raw, "xb"):
        os.chmod(raw, 0o600)
    with contextlib.closing(sqlite3.connect(source.as_uri() + "?mode=ro", uri=True, timeout=1)) as src, contextlib.closing(sqlite3.connect(raw, timeout=1)) as dst:
        src.execute("PRAGMA query_only=ON")
        src.execute("PRAGMA busy_timeout=1000")
        dst.execute("PRAGMA busy_timeout=1000")

        def progress(status, remaining, total):
            deadline_check(deadline)
            # 少量让出 CPU；每批只读取 128 页，避免一个长读锁。
            if status == sqlite3.SQLITE_OK:
                time.sleep(0.002)

        src.backup(dst, pages=128, progress=progress, sleep=0.05)
    after = regular(source)
    if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
        raise BackupError("source_database_replaced")


def container_query(container, engine, sql):
    if engine == "mysql":
        shell = 'export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?}"; exec mysql --user=root --connect-timeout=5 --batch --skip-column-names -e "$1"'
        sql = "SET SESSION max_execution_time=5000; SET SESSION lock_wait_timeout=5; SET SESSION innodb_lock_wait_timeout=5; " + sql
    else:
        shell = 'export PGOPTIONS="-c statement_timeout=5000 -c lock_timeout=2000"; exec psql -X -A -t --no-password --username="${POSTGRES_USER:-postgres}" --dbname=mailflow -c "$1"'
    result = subprocess.run(["docker", "exec", container, "sh", "-c", shell, "sh", sql], capture_output=True, timeout=15)
    if result.returncode:
        raise BackupError("connection_cleanup_query_failed")
    return result.stdout.decode("ascii", "strict").strip().splitlines()


def mysql_cleanup(container, marker, database):
    # PID 是本次容器内客户端自己写的，不根据用户/库名批量取消任务。
    p = subprocess.run(["docker", "exec", container, "cat", marker], capture_output=True, timeout=10)
    if p.returncode or not re.fullmatch(rb"[0-9]+ [0-9]+\s*", p.stdout):
        raise BackupError("own_dump_pid_unavailable")
    pid, ticks = map(int, p.stdout.split())
    probe = ('if [ ! -r "/proc/$1/stat" ]; then exit 0; fi; '
             'actual=$(awk \'{print $22}\' "/proc/$1/stat"); '
             '[ "$actual" = "$2" ] || exit 3; '
             'IFS= read -r client_name < "/proc/$1/comm"; '
             '[ "$client_name" = mysqldump ] || exit 3; exit 1')
    def client_state():
        state = subprocess.run(["docker", "exec", container, "sh", "-c", probe, "sh", str(pid), str(ticks)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10).returncode
        if state not in (0, 1):
            raise BackupError("own_dump_process_identity_changed")
        return state

    alive = client_state()
    sql = ("SELECT p.ID FROM information_schema.PROCESSLIST p JOIN performance_schema.session_connect_attrs a ON a.PROCESSLIST_ID=p.ID "
           "JOIN performance_schema.session_connect_attrs b ON b.PROCESSLIST_ID=p.ID "
           "WHERE a.ATTR_NAME='_pid' AND a.ATTR_VALUE='" + str(pid) + "' AND b.ATTR_NAME='program_name' AND b.ATTR_VALUE='mysqldump' "
           "AND p.USER='root' AND p.DB='" + database + "' LIMIT 4;")
    ids = container_query(container, "mysql", sql)
    if len(ids) > 1 or any(not x.isdigit() for x in ids):
        raise BackupError("own_dump_identity_ambiguous")
    for cid in ids:
        # 只终止上述本次进程对应的连接；正常结束一般已没有连接。
        container_query(container, "mysql", "KILL " + cid + ";")
    if container_query(container, "mysql", sql):
        raise BackupError("own_dump_connection_remains")
    if alive:
        # 再次校验启动时间与进程名后仅结束自己的客户端，不能杀 PID 已复用的进程。
        if client_state():
            terminate = probe.rsplit("exit 1", 1)[0] + 'kill -TERM "$1"; exit 0'
            subprocess.run(["docker", "exec", container, "sh", "-c", terminate, "sh", str(pid), str(ticks)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10, check=True)
        for _ in range(20):
            if not client_state():
                break
            time.sleep(0.1)
        if client_state():
            terminate = probe.rsplit("exit 1", 1)[0] + 'kill -KILL "$1"; exit 0'
            subprocess.run(["docker", "exec", container, "sh", "-c", terminate, "sh", str(pid), str(ticks)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10, check=True)
            time.sleep(0.1)
        if client_state():
            raise BackupError("own_dump_client_remains")
    if container_query(container, "mysql", sql):
        raise BackupError("own_dump_connection_remains")
    subprocess.run(["docker", "exec", container, "rm", "-f", "--", marker], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10, check=True)


def postgres_cleanup(container, tag):
    where = "datname='mailflow' AND application_name='" + tag + "' AND pid<>pg_backend_pid()"
    container_query(container, "postgres", "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE " + where + ";")
    rows = container_query(container, "postgres", "SELECT count(*) FROM pg_stat_activity WHERE " + where + ";")
    if rows != ["0"]:
        raise BackupError("own_dump_connection_remains")


def dump_database(item, raw, run_id):
    seconds = item["timeout_seconds"]
    marker = "/tmp/audit-core-" + run_id + ".pid"
    tag = "audit-core-" + run_id
    if item["kind"] == "mysql":
        shell = r'''umask 077; export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?}"; exec timeout -s TERM -k 5 "$1" sh -c 'ticks=$(cut -d " " -f 22 "/proc/$$/stat"); printf "%s %s\n" "$$" "$ticks" > "$1"; exec mysqldump --user=root --single-transaction --quick --skip-lock-tables --skip-network-timeout --routines --events --triggers --hex-blob --set-gtid-purged=OFF --no-tablespaces --column-statistics=0 --default-character-set=utf8mb4 "$2"' sh "$2" "$3"'''
        args = [str(seconds), marker, item["database"]]
    else:
        shell = ('umask 077; export PGAPPNAME="$2"; export PGOPTIONS="-c statement_timeout=120000 -c lock_timeout=3000 -c application_name=$2"; '
                 'exec timeout -s TERM -k 5 "$1" pg_dump --format=custom --no-owner --no-acl --lock-wait-timeout=3s --username="${POSTGRES_USER:-postgres}" --dbname="$3"')
        args = [str(seconds), tag, item["database"]]
    failed = False
    try:
        with open(raw, "xb") as out:
            os.chmod(raw, 0o600)
            p = subprocess.run(["docker", "exec", item["container"], "sh", "-c", shell, "sh", *args], stdout=out, stderr=subprocess.DEVNULL, timeout=seconds + 20)
            failed = bool(p.returncode)
            out.flush()
            os.fsync(out.fileno())
    finally:
        if item["kind"] == "mysql":
            mysql_cleanup(item["container"], marker, item["database"])
        else:
            postgres_cleanup(item["container"], tag)
    if failed:
        raise BackupError("database_dump_failed")
    if item["kind"] == "postgres":
        with open(raw, "rb") as f:
            if f.read(5) != b"PGDMP":
                raise BackupError("invalid_postgres_archive")
        # 客户端仅列本次归档目录，绝不恢复进线上数据库。
        with open(raw, "rb") as src:
            p = subprocess.run(["docker", "exec", "-i", item["container"], "timeout", "30", "pg_restore", "--list"], stdin=src, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=40)
        if p.returncode:
            raise BackupError("postgres_archive_catalog_failed")


def existing_mysql(item, dest, deadline):
    root = Path(item["source_dir"])
    candidates = [p for p in root.iterdir() if re.fullmatch(r"xiaoenai_\d{8}T\d{6}Z\.sql\.gz", p.name)]
    if not candidates:
        raise BackupError("no_existing_backup")
    source = max(candidates, key=lambda p: p.lstat().st_mtime)
    before = fingerprint(source)
    age = time.time() - source.stat().st_mtime
    if age < 60 or age > item["max_age_hours"] * 3600:
        raise BackupError("existing_backup_age_invalid")
    regular(source, private=True)
    sidecar = Path(str(source) + ".sha256")
    regular(sidecar, private=True)
    checksum = sidecar.read_text().strip()
    if not re.fullmatch(r"[a-f0-9]{64}  " + re.escape(source.name), checksum):
        raise BackupError("existing_checksum_format_invalid")
    expected = checksum[:64]
    if digest(source, deadline) != expected:
        raise BackupError("existing_checksum_mismatch")
    with open(source, "rb") as src, open(dest, "xb") as out:
        os.chmod(dest, 0o600)
        for data in iter(lambda: src.read(4 * 1024 * 1024), b""):
            deadline_check(deadline)
            out.write(data)
        out.flush()
        os.fsync(out.fileno())
    if fingerprint(source) != before or digest(dest, deadline) != expected:
        raise BackupError("existing_backup_changed")
    return source.name


def create_item(item, directory, run_id, full=False):
    started = time.monotonic()
    deadline = started + item["timeout_seconds"]
    suffix = {"sqlite": ".sqlite.gz", "mysql": ".sql.gz", "existing_mysql": ".sql.gz", "postgres": ".pgdump.gz"}[item["kind"]]
    filename = item["id"] + suffix
    dest = directory / filename
    part = directory / ("." + filename + ".part")
    metadata = {}
    try:
        if item["kind"] == "existing_mysql":
            metadata["reused_backup"] = existing_mysql(item, part, deadline)
            validation = sql_validate(part, deadline)
            raw_bytes = validation["raw_bytes"]
        else:
            with tempfile.TemporaryDirectory(prefix=".item-", dir=directory) as scratch:
                raw = Path(scratch) / "snapshot"
                if item["kind"] == "sqlite":
                    sqlite_backup(item, raw, deadline)
                    validation = sqlite_validate(raw, item["tables"], deadline, full)
                else:
                    dump_database(item, raw, run_id)
                    validation = {"check": "postgres_archive_catalog", "database_restore": "not_performed"}
                raw_bytes = raw.stat().st_size
                if raw_bytes <= 0 or raw_bytes > 32 * 1024**3:
                    raise BackupError("snapshot_size_invalid")
                compress(raw, part, deadline)
                if item["kind"] == "mysql":
                    validation = sql_validate(part, deadline)
        sha = digest(part, deadline)
        os.rename(part, dest)
        return {"id": item["id"], "kind": item["kind"], "status": "ok", "file": filename, "bytes": dest.stat().st_size, "raw_bytes": raw_bytes, "sha256": sha, "validation": validation, "seconds": round(time.monotonic() - started, 3), **metadata}
    except Exception:
        if part.exists() and part.is_file() and not part.is_symlink():
            part.unlink()
        raise


def run_backup(config, selected=None, full=False):
    root = private_dir(config["local_root"])
    if shutil.disk_usage(root).free < config["minimum_free_bytes"]:
        raise BackupError("insufficient_free_space")
    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ-") + uuid.uuid4().hex[:8]
    directory = root / (".pending-" + run_id)
    directory.mkdir(mode=0o700)
    targets = [i for i in config["targets"] if selected is None or i["id"] in selected]
    if not targets:
        raise BackupError("empty_selected_targets")
    manifest = {"schema": SCHEMA, "source_node": "119", "run_id": run_id, "created_at": now(), "remote": config["remote"], "scope": [i["id"] for i in targets], "gaps": config.get("gaps", []), "results": []}
    for item in targets:
        try:
            if shutil.disk_usage(root).free < config["minimum_free_bytes"]:
                raise BackupError("insufficient_free_space")
            result = create_item(item, directory, run_id, full)
        except Exception as exc:
            result = {"id": item["id"], "kind": item["kind"], "status": "failed", "error": safe_error(exc)}
        manifest["results"].append(result)
        print(json.dumps({k: result[k] for k in ("id", "status", "bytes", "seconds", "error") if k in result}), flush=True)
    manifest["ok"] = all(i["status"] == "ok" for i in manifest["results"])
    json_write(directory / "manifest.json", manifest)
    final = root / run_id
    os.rename(directory, final)
    d = os.open(root, os.O_DIRECTORY)
    try:
        os.fsync(d)
    finally:
        os.close(d)
    return final, manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, default=DEFAULT_CONFIG)
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--replicate", action="store_true")
    parser.add_argument("--full-check", action="store_true")
    parser.add_argument("--targets", help="逗号分隔的精确目标 ID")
    args = parser.parse_args()
    config = load_config(args.config)
    selected = set(args.targets.split(",")) if args.targets else None
    if selected is not None and not selected.issubset({i["id"] for i in config["targets"]}):
        raise BackupError("unknown_selected_target")
    if not args.apply:
        print(json.dumps({"preview": True, "source_node": "119", "local_root": config["local_root"], "remote": config["remote"], "retention_days": 7, "targets": [i for i in config["targets"] if selected is None or i["id"] in selected], "gaps": config.get("gaps", [])}, ensure_ascii=False, indent=2))
        return 0
    if os.geteuid() != 0:
        raise BackupError("root_required")
    os.umask(0o077)
    fd = os.open("/run/lock/audit-core-backup.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise BackupError("backup_already_running")
        directory, manifest = run_backup(config, selected, args.full_check)
        copied = False
        if args.replicate and any(i["status"] == "ok" for i in manifest["results"]):
            p = subprocess.run([sys.executable, str(Path(__file__).with_name("transfer.py")), "--run", str(directory), "--apply"], timeout=5400)
            copied = p.returncode == 0
        print(json.dumps({"run_directory": str(directory), "local_ok": manifest["ok"], "replica_verified": copied, "scope": manifest["scope"], "database_restore": {"sqlite": "read_only_checked", "mysql_postgres": "not_performed"}}))
        return 0 if manifest["ok"] and (not args.replicate or copied) else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:
        print(json.dumps({"ok": False, "error": safe_error(exc)}), file=sys.stderr)
        sys.exit(1)
