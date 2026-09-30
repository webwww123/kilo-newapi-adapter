#!/usr/bin/env python3
"""在无网络、有限资源的新容器中验证 MySQL/PG 归档；默认仅预览。"""
import argparse
import gzip
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import uuid

from common import (BackupError, LOCAL_ROOT, RUN_RE, digest, fingerprint,
                    json_read, json_write, now, private_dir, regular,
                    safe_error, sql_validate, validate_manifest)

BASE = LOCAL_ROOT / "restore-tests"
GIB = 1024**3
TARGETS = {
    "99code": {"kind": "mysql", "source_container": "99code-mysql", "database": "newapi_523", "memory": "1536m", "memory_bytes": 1536 * 1024**2, "seconds": 900, "tables": ["users", "tokens", "redemptions", "top_ups", "channels"]},
    "xiaoenai": {"kind": "existing_mysql", "source_container": "xiaoenai-mysql", "database": "xiaoenai", "memory": "1536m", "memory_bytes": 1536 * 1024**2, "seconds": 900, "tables": ["User", "AccountEntitlement", "AccountTokenEntitlementState", "PaymentOrder", "UserMessage"]},
    "mailflow": {"kind": "postgres", "source_container": "mailflow-postgres", "database": "mailflow", "memory": "512m", "memory_bytes": 512 * 1024**2, "seconds": 180, "tables": ["users", "email_accounts", "messages"]},
}


def command(args, timeout=30, stdin=None):
    p = subprocess.run(args, input=stdin, capture_output=True, timeout=timeout)
    if p.returncode:
        raise BackupError("isolated_command_failed")
    return p.stdout.decode("utf8", "strict").strip()


def source_image(target):
    # 只读 docker ps 的明确元数据，避免 inspect/env 暴露生产秘密。
    source = target["source_container"]
    output = command(["docker", "ps", "--filter", "name=^/" + source + "$", "--format", "{{.Names}} {{.Image}}"], 15)
    words = output.split()
    if len(words) != 2 or words[0] != source or not re.fullmatch(r"[a-zA-Z0-9_./:@-]+", words[1]):
        raise BackupError("source_image_unavailable")
    return words[1]


def select_items(directory, selected):
    directory = Path(directory)
    if directory.parent != LOCAL_ROOT or not RUN_RE.fullmatch(directory.name):
        raise BackupError("restore_source_scope_invalid")
    private_dir(directory)
    manifest = json_read(directory / "manifest.json")
    validate_manifest(manifest)
    if manifest["run_id"] != directory.name or not selected or not set(selected).issubset(TARGETS):
        raise BackupError("restore_target_invalid")
    results = {x["id"]: x for x in manifest["results"]}
    items = []
    for target_id in selected:
        item = results.get(target_id)
        if not item or item["status"] != "ok" or item["kind"] != TARGETS[target_id]["kind"]:
            raise BackupError("requested_successful_archive_unavailable")
        items.append(item)
    return manifest, items


def available_memory():
    for line in Path("/proc/meminfo").read_text().splitlines():
        if line.startswith("MemAvailable:"):
            return int(line.split()[1]) * 1024
    raise BackupError("memory_budget_unavailable")


def resource_check(item, target):
    if available_memory() < target["memory_bytes"] + 2 * GIB:
        raise BackupError("insufficient_memory_headroom")
    if shutil.disk_usage(LOCAL_ROOT).free < max(10 * GIB, item["raw_bytes"] * 2 + 2 * GIB):
        raise BackupError("insufficient_restore_disk_headroom")


def precheck(directory, item):
    archive = directory / item["file"]
    before = fingerprint(archive)
    if before[2] != item["bytes"] or digest(archive, time.monotonic() + 600) != item["sha256"]:
        raise BackupError("restore_archive_checksum_failed")
    if item["kind"] != "postgres":
        check = sql_validate(archive, time.monotonic() + 600, item["validation"]["table_definitions"])
        if check["raw_bytes"] != item["raw_bytes"]:
            raise BackupError("restore_raw_size_mismatch")
    else:
        total = 0
        with gzip.open(archive, "rb") as f:
            if f.read(5) != b"PGDMP":
                raise BackupError("invalid_postgres_archive")
            total = 5
            for data in iter(lambda: f.read(4 * 1024 * 1024), b""):
                total += len(data)
                if total > item["raw_bytes"]:
                    raise BackupError("restore_raw_size_mismatch")
        if total != item["raw_bytes"]:
            raise BackupError("restore_raw_size_mismatch")
    if fingerprint(archive) != before:
        raise BackupError("restore_archive_changed")
    return archive, before


def create_args(target, image, name, work):
    args = ["docker", "create", "--name", name, "--cidfile", str(work / "container-id"), "--pull", "never", "--network", "none", "--cpus", "1", "--memory", target["memory"], "--memory-swap", target["memory"], "--pids-limit", "256", "--restart", "no", "--security-opt", "no-new-privileges=true", "--mount", "type=bind,src=" + str(work / "data") + ",dst=" + ("/var/lib/postgresql/data" if target["kind"] == "postgres" else "/var/lib/mysql"), "--mount", "type=bind,src=" + str(work / "secrets") + ",dst=/run/secrets,readonly"]
    if target["kind"] == "postgres":
        args += ["--env", "POSTGRES_PASSWORD_FILE=/run/secrets/root-password", "--env", "POSTGRES_DB=" + target["database"], image]
    else:
        args += ["--env", "MYSQL_ROOT_PASSWORD_FILE=/run/secrets/root-password", "--env", "MYSQL_DATABASE=" + target["database"], image, "--event-scheduler=OFF", "--skip-log-bin", "--innodb-buffer-pool-size=256M", "--max-connections=15", "--max-allowed-packet=64M", "--local-infile=0"]
    return args


def query(cid, target, sql):
    if target["kind"] == "postgres":
        shell = 'export PGOPTIONS="-c statement_timeout=5000 -c lock_timeout=2000"; exec psql -X -A -t --no-password --username=postgres --dbname="$1" -c "$2"'
    else:
        shell = 'export MYSQL_PWD="$(cat /run/secrets/root-password)"; exec mysql --user=root --connect-timeout=5 --batch --skip-column-names --database="$1" -e "$2"'
        sql = "SET SESSION max_execution_time=5000; SET SESSION lock_wait_timeout=5; SET SESSION innodb_lock_wait_timeout=5; " + sql
    return command(["docker", "exec", cid, "sh", "-c", shell, "sh", target["database"], sql], 15)


def wait_ready(cid, target):
    end = time.monotonic() + 120
    while time.monotonic() < end:
        try:
            if query(cid, target, "SELECT 1;") == "1":
                return
        except Exception:
            pass
        time.sleep(2)
    raise BackupError("isolated_database_startup_timeout")


def import_archive(cid, target, archive):
    seconds = target["seconds"]
    if target["kind"] == "postgres":
        shell = 'export PGOPTIONS="-c statement_timeout=120000 -c lock_timeout=3000"; exec timeout -s TERM -k 5 "$1" pg_restore --exit-on-error --single-transaction --no-owner --no-acl --username=postgres --dbname="$2"'
    else:
        shell = 'export MYSQL_PWD="$(cat /run/secrets/root-password)"; exec timeout -s TERM -k 5 "$1" mysql --user=root --binary-mode --connect-timeout=5 --default-character-set=utf8mb4 --max-allowed-packet=64M --init-command="SET SESSION lock_wait_timeout=5, innodb_lock_wait_timeout=5, max_execution_time=120000" "$2"'
    process = subprocess.Popen(["docker", "exec", "-i", cid, "sh", "-c", shell, "sh", str(seconds), target["database"]], stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    writer_errors = []
    def write():
        try:
            with gzip.open(archive, "rb") as src:
                for data in iter(lambda: src.read(4 * 1024 * 1024), b""):
                    process.stdin.write(data)
        except Exception as exc:
            writer_errors.append(type(exc).__name__)
        finally:
            try:
                process.stdin.close()
            except Exception:
                pass
    writer = threading.Thread(target=write, daemon=True)
    writer.start()
    try:
        returncode = process.wait(timeout=seconds + 20)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=10)
        raise BackupError("isolated_import_timeout")
    writer.join(timeout=10)
    if returncode or writer_errors or writer.is_alive():
        raise BackupError("isolated_import_failed")


def metrics(cid, target, item):
    postgres = target["kind"] == "postgres"
    schema = "public" if postgres else target["database"]
    total = int(query(cid, target, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='" + schema + "';"))
    if not total:
        raise BackupError("isolated_restore_empty_schema")
    if not postgres and total != item["validation"]["table_definitions"]:
        raise BackupError("isolated_table_definition_count_mismatch")
    names = ",".join("'" + name + "'" for name in target["tables"])
    existing = query(cid, target, "SELECT table_name FROM information_schema.tables WHERE table_schema='" + schema + "' AND table_name IN (" + names + ") LIMIT 5;").splitlines()
    if set(existing) != set(target["tables"]):
        raise BackupError("isolated_required_business_tables_missing")
    counts = {}
    for name in existing:
        if name not in target["tables"]:
            raise BackupError("isolated_table_name_invalid")
        table = 'public."' + name + '"' if postgres else '`' + name + '`'
        counts[name] = int(query(cid, target, "SELECT COUNT(*) FROM " + table + ";"))
    return {"database": target["database"], "schema_count": 1, "table_count": total, "key_table_counts": counts}


def cleanup(cid, work):
    if not re.fullmatch(r"[a-f0-9]{64}", cid or ""):
        return False
    subprocess.run(["docker", "stop", "--time", "10", cid], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)
    subprocess.run(["docker", "rm", "-f", "-v", cid], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)
    p = subprocess.run(["docker", "ps", "-a", "--filter", "id=" + cid, "--format", "{{.ID}}"], capture_output=True, timeout=15)
    if p.returncode or p.stdout.strip():
        return False
    if work.parent != BASE or not re.fullmatch(r"job-[a-z0-9_-]+", work.name):
        raise BackupError("cleanup_directory_scope_invalid")
    private_dir(work)
    if (work / ".owned").read_text().strip() != cid:
        raise BackupError("cleanup_ownership_marker_invalid")
    shutil.rmtree(work)
    return True


def restore_item(directory, item):
    target = TARGETS[item["id"]]
    resource_check(item, target)
    image = source_image(target)
    archive, before = precheck(directory, item)
    private_dir(BASE)
    work = Path(tempfile.mkdtemp(prefix="job-" + item["id"] + "-", dir=BASE))
    private_dir(work / "data")
    private_dir(work / "secrets")
    secret_file = work / "secrets" / "root-password"
    with open(secret_file, "x") as f:
        os.chmod(secret_file, 0o600)
        f.write(secrets.token_urlsafe(40) + "\n")
    cid = None
    started = time.monotonic()
    result = {"id": item["id"], "status": "failed", "image": image, "network": "none", "published_ports": 0, "cpu_limit": 1, "memory_limit": target["memory"]}
    try:
        output = command(create_args(target, image, "audit-core-restore-" + item["id"] + "-" + uuid.uuid4().hex[:12], work), 60)
        regular(work / "container-id", private=True)
        cid = (work / "container-id").read_text().strip()
        if cid != output or not re.fullmatch(r"[a-f0-9]{64}", cid):
            raise BackupError("created_container_identity_invalid")
        with open(work / ".owned", "x") as f:
            os.chmod(work / ".owned", 0o600)
            f.write(cid + "\n")
        command(["docker", "start", cid], 30)
        wait_ready(cid, target)
        import_archive(cid, target, archive)
        if fingerprint(archive) != before:
            raise BackupError("restore_archive_changed")
        result["validation"] = metrics(cid, target, item)
        result["status"] = "ok"
    except Exception as exc:
        result["error"] = safe_error(exc)
    finally:
        if cid is None and (work / "container-id").exists():
            try:
                candidate = (work / "container-id").read_text().strip()
                if re.fullmatch(r"[a-f0-9]{64}", candidate):
                    cid = candidate
                    if not (work / ".owned").exists():
                        json_marker = work / ".owned"
                        json_marker.write_text(cid + "\n")
                        json_marker.chmod(0o600)
            except Exception:
                pass
        try:
            result["container_removed"] = cleanup(cid, work)
        except Exception:
            result["container_removed"] = False
        if not result["container_removed"]:
            result["status"] = "failed"
            result["cleanup_attention_directory"] = str(work)
        result["seconds"] = round(time.monotonic() - started, 3)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", type=Path, required=True)
    parser.add_argument("--targets", default="99code,xiaoenai,mailflow")
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    manifest, items = select_items(args.run, args.targets.split(","))
    if not args.apply:
        print(json.dumps({"preview": True, "run_id": manifest["run_id"], "sequential": True, "plans": [{"id": item["id"], "image": source_image(TARGETS[item["id"]]), "cpus": 1, "memory": TARGETS[item["id"]]["memory"], "network": "none", "ports": [], "timeout_seconds": TARGETS[item["id"]]["seconds"], "raw_archive_bytes": item["raw_bytes"]} for item in items]}, ensure_ascii=False, indent=2))
        return 0
    if os.geteuid() != 0:
        raise BackupError("root_required")
    os.umask(0o077)
    results = []
    for item in items:
        try:
            result = restore_item(args.run, item)
        except Exception as exc:
            result = {"id": item["id"], "status": "failed", "error": safe_error(exc)}
        results.append(result)
        print(json.dumps(result), flush=True)
        if result.get("cleanup_attention_directory"):
            break
    report = {"run_id": manifest["run_id"], "checked_at": now(), "manifest_sha256": digest(args.run / "manifest.json"), "requested": args.targets.split(","), "ok": len(results) == len(items) and all(r["status"] == "ok" for r in results), "results": results}
    reports = private_dir(BASE / "reports")
    report_path = reports / (manifest["run_id"] + "-" + uuid.uuid4().hex[:8] + ".json")
    json_write(report_path, report)
    print(json.dumps({"ok": report["ok"], "report": str(report_path)}))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:
        print(json.dumps({"ok": False, "error": safe_error(exc)}), file=sys.stderr)
        sys.exit(1)
