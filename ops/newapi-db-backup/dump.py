#!/usr/bin/env python3
"""仅导出 3000 业务数据；日志与临时诊断表只保留结构。"""
import argparse
import gzip
import os
from pathlib import Path
import re
import subprocess
import sys
import threading
import time
import uuid

# 共用已经验收的连接清理逻辑，不按库名或用户名批量杀查询。
support = Path(__file__).resolve().parent.parent / "audit-backup"
sys.path.insert(0, str(support if support.is_dir() else Path("/usr/local/lib/audit-core-backup")))
from backup import container_query, mysql_cleanup
from common import BackupError, safe_error

CONTAINER = "newapi-mysql"
DATABASE = "defaultdb"
SCHEMA_ONLY = ("logs", "error_request_traces")
OUTPUT_ROOT = Path("/opt/backups/newapi-db")
NAME = re.compile(r"defaultdb_no_logs_\d{4}-\d{2}-\d{2}_\d{6}\.sql\.gz\Z")
IDENTIFIER = re.compile(r"[A-Za-z_][A-Za-z0-9_]*\Z")


def source_tables():
    rows = container_query(CONTAINER, "mysql", "SELECT TABLE_NAME FROM information_schema.tables WHERE TABLE_SCHEMA='defaultdb' AND TABLE_TYPE='BASE TABLE' ORDER BY TABLE_NAME LIMIT 201;")
    if not rows or len(rows) > 200 or any(not IDENTIFIER.fullmatch(x) for x in rows):
        raise BackupError("source_table_inventory_invalid")
    if not set(SCHEMA_ONLY).issubset(rows):
        raise BackupError("required_schema_only_tables_missing")
    return set(rows)


def dump_args(schema):
    args = ["--user=root", "--single-transaction", "--quick", "--skip-lock-tables", "--skip-add-locks", "--skip-network-timeout", "--set-gtid-purged=OFF", "--no-tablespaces", "--column-statistics=0", "--default-character-set=utf8mb4", "--hex-blob", "--triggers"]
    if schema:
        return args + ["--no-data", DATABASE, *SCHEMA_ONLY]
    return args + ["--routines", "--events", *["--ignore-table=" + DATABASE + "." + table for table in SCHEMA_ONLY], DATABASE]


def own_connections(pid):
    # 不读取 SQL 原文、业务行或凭据，连接属性必须同时匹配本次 mysqldump。
    sql = ("SELECT p.ID,p.TIME,p.COMMAND,COALESCE(NULLIF(p.STATE,''),'-') FROM information_schema.PROCESSLIST p "
           "JOIN performance_schema.session_connect_attrs a ON a.PROCESSLIST_ID=p.ID "
           "JOIN performance_schema.session_connect_attrs b ON b.PROCESSLIST_ID=p.ID "
           "WHERE a.ATTR_NAME='_pid' AND a.ATTR_VALUE='" + str(pid) + "' "
           "AND b.ATTR_NAME='program_name' AND b.ATTR_VALUE='mysqldump' "
           "AND p.USER='root' AND p.DB='defaultdb' LIMIT 4;")
    rows = container_query(CONTAINER, "mysql", sql)
    if len(rows) > 1:
        raise BackupError("own_dump_identity_ambiguous")
    parsed = []
    for row in rows:
        fields = row.split("\t", 3)
        if len(fields) != 4 or not fields[0].isdigit() or not fields[1].isdigit():
            raise BackupError("own_dump_identity_invalid")
        parsed.append((int(fields[0]), int(fields[1]), fields[2], fields[3]))
    return parsed


def enforce_query_budget(pid):
    for connection, seconds, command, state in own_connections(pid):
        waiting = state.lower().startswith("waiting") and "lock" in state.lower()
        if command == "Query" and (seconds >= 120 or waiting and seconds >= 5):
            # 服务端取消自己的查询；关闭客户端不能替代这个步骤。
            container_query(CONTAINER, "mysql", "KILL " + str(connection) + ";")
            raise BackupError("own_dump_query_or_lock_deadline")


def marker_identity(marker):
    for _ in range(30):
        result = subprocess.run(["docker", "exec", CONTAINER, "cat", marker], capture_output=True, timeout=10)
        if result.returncode == 0 and re.fullmatch(rb"[0-9]+ [0-9]+\s*", result.stdout):
            return int(result.stdout.split()[0])
        time.sleep(0.1)
    raise BackupError("own_dump_pid_unavailable")


def dump_stage(output, schema):
    marker = "/tmp/newapi-backup-" + uuid.uuid4().hex + ".pid"
    # 密码只在既有容器内部读取，不出现在主机命令参数、源码或输出中。
    shell = r'''umask 077; export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?}"; exec timeout -s TERM -k 5 900 sh -c 'ticks=$(cut -d " " -f 22 "/proc/$$/stat"); printf "%s %s\n" "$$" "$ticks" > "$1"; shift; exec mysqldump "$@"' sh "$@"'''
    process = subprocess.Popen(["docker", "exec", CONTAINER, "sh", "-c", shell, "sh", marker, *dump_args(schema)], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    errors = []
    def write():
        try:
            while data := process.stdout.read(1024 * 1024):
                output.write(data)
        except Exception as exc:
            errors.append(type(exc).__name__)
    writer = threading.Thread(target=write, daemon=True)
    writer.start()
    started = time.monotonic()
    identity = None
    try:
        identity = marker_identity(marker)
        while process.poll() is None:
            if time.monotonic() - started >= 900:
                raise BackupError("own_dump_total_deadline")
            enforce_query_budget(identity)
            time.sleep(1)
        writer.join(timeout=10)
        if process.returncode or errors or writer.is_alive():
            raise BackupError("database_dump_failed")
    finally:
        if process.poll() is None:
            process.kill()
        process.wait(timeout=10)
        if identity is not None:
            mysql_cleanup(CONTAINER, marker, DATABASE)
        writer.join(timeout=10)
        process.stdout.close()


def validate_archive(path, expected):
    definitions, insert_tables, completions = set(), set(), 0
    # 只检测 SQL 结构标记，不把数据行写到日志。128 字节覆盖跨块的表名标记。
    overlap = b""
    with gzip.open(path, "rb") as source:
        while block := source.read(1024 * 1024):
            chunk = overlap + block
            cutoff = len(overlap)
            for match in re.finditer(rb"(?:^|\n)CREATE TABLE `([A-Za-z_][A-Za-z0-9_]*)`", chunk):
                if match.end() > cutoff:
                    definitions.add(match.group(1).decode("ascii"))
            for match in re.finditer(rb"(?:^|\n)INSERT INTO `([A-Za-z_][A-Za-z0-9_]*)`", chunk):
                if match.end() > cutoff:
                    insert_tables.add(match.group(1).decode("ascii"))
            for match in re.finditer(rb"-- Dump completed on ", chunk):
                if match.end() > cutoff:
                    completions += 1
            overlap = chunk[-128:]
    if definitions != expected or completions != 2:
        raise BackupError("archive_schema_incomplete")
    if set(SCHEMA_ONLY).intersection(insert_tables):
        raise BackupError("archive_contains_excluded_rows")


def create_archive(path):
    expected = source_tables()
    if path.exists() or path.is_symlink():
        raise BackupError("refuse_existing_archive")
    part = path.with_name(path.name + ".part-" + uuid.uuid4().hex)
    try:
        with open(part, "xb") as raw:
            os.chmod(part, 0o600)
            with gzip.GzipFile(fileobj=raw, mode="wb", compresslevel=1, mtime=0) as compressed:
                dump_stage(compressed, False)
                compressed.write(b"\n")
                dump_stage(compressed, True)
            raw.flush()
            os.fsync(raw.fileno())
        validate_archive(part, expected)
        os.replace(part, path)
    finally:
        if part.exists():
            part.unlink()
    print("backup_verified tables=" + str(len(expected)) + " schema_only=logs,error_request_traces", flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    if args.output.parent != OUTPUT_ROOT or not NAME.fullmatch(args.output.name) or args.output.resolve() != args.output.absolute():
        raise BackupError("output_scope_invalid")
    create_archive(args.output)


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print("backup_failed " + safe_error(exc), file=sys.stderr)
        raise SystemExit(1)
