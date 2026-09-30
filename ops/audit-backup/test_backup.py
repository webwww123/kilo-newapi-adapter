#!/usr/bin/env python3
"""仅使用临时测试库：WAL、一致性、坏包、白名单与删除边界。"""
import contextlib
import datetime as dt
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

import backup
import common


SQL = b"-- MySQL dump\nCREATE TABLE `orders` (`id` int);\nINSERT INTO `orders` VALUES (1);\n-- Dump completed on 2026-09-30 00:00:00\n"


class BackupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.old_umask = os.umask(0o077)

    def tearDown(self):
        os.umask(self.old_umask)
        self.temp.cleanup()

    def sqlite_source(self, wal=True):
        source = self.root / "source.sqlite"
        c = sqlite3.connect(source)
        if wal:
            c.execute("PRAGMA journal_mode=WAL")
        c.execute("CREATE TABLE orders (id INTEGER PRIMARY KEY)")
        c.executemany("INSERT INTO orders VALUES (?)", [(1,), (2,), (3,)])
        c.commit()
        return source, c

    def item(self, source):
        return {"id": "fixture", "kind": "sqlite", "source": str(source), "tables": ["orders"], "timeout_seconds": 30}

    def manifest(self, directory, item, run_id="20260930T000000Z-1234abcd", created=None):
        value = {"schema": common.SCHEMA, "source_node": "119", "run_id": run_id, "scope": [item["id"]], "created_at": created or common.now(), "remote": {"host": "50", "root": str(common.REMOTE_ROOT)}, "results": [item], "ok": item["status"] == "ok"}
        common.json_write(directory / "manifest.json", value)
        return value

    def test_wal_backup_restores_all_committed_rows_without_changing_source(self):
        source, c = self.sqlite_source()
        try:
            self.assertTrue(Path(str(source) + "-wal").exists())
            result = backup.create_item(self.item(source), self.root, "20260930T000000Z-1234abcd", full=True)
            self.assertEqual(result["validation"]["table_counts"], {"orders": 3})
            self.manifest(self.root, result)
            c.execute("INSERT INTO orders VALUES (4)")
            c.commit()
            restored = common.verify_run(self.root, full=True)
            self.assertTrue(restored["ok"])
            self.assertEqual(restored["results"][0]["validation"]["table_counts"], {"orders": 3})
            self.assertEqual(c.execute("SELECT COUNT(*) FROM orders").fetchone()[0], 4)
            self.assertEqual((self.root / result["file"]).stat().st_mode & 0o777, 0o600)
        finally:
            c.close()

    def test_corrupt_archive_is_not_accepted(self):
        source, c = self.sqlite_source()
        try:
            result = backup.create_item(self.item(source), self.root, "20260930T000000Z-1234abcd")
            self.manifest(self.root, result)
            archive = self.root / result["file"]
            with open(archive, "r+b") as f:
                f.seek(15)
                f.write(b"bad")
            report = common.verify_run(self.root)
            self.assertFalse(report["ok"])
            self.assertEqual(report["results"][0]["error"], "artifact_checksum_failed")
        finally:
            c.close()

    def test_expired_deadline_does_not_publish_backup(self):
        source, c = self.sqlite_source()
        try:
            raw = self.root / "timed-out.sqlite"
            with self.assertRaises(common.BackupError):
                backup.sqlite_backup(self.item(source), raw, time.monotonic() - 1)
            self.assertEqual(c.execute("SELECT COUNT(*) FROM orders").fetchone()[0], 3)
        finally:
            c.close()

    def test_busy_source_is_bounded_and_source_transaction_remains(self):
        source, c = self.sqlite_source(wal=False)
        c.execute("BEGIN EXCLUSIVE")
        try:
            with self.assertRaises(common.BackupError):
                backup.sqlite_backup(self.item(source), self.root / "busy.sqlite", time.monotonic() + 0.03)
            self.assertTrue(c.in_transaction)
        finally:
            c.rollback()
            c.close()

    def test_linked_sources_are_rejected(self):
        source, c = self.sqlite_source()
        c.close()
        alias = self.root / "alias.sqlite"
        alias.symlink_to(source)
        with self.assertRaises(common.BackupError):
            common.regular(alias)
        alias.unlink()
        os.link(source, alias)
        with self.assertRaises(common.BackupError):
            common.regular(source)

    def test_config_cannot_expand_to_another_database_or_destination(self):
        base = json.loads(backup.DEFAULT_CONFIG.read_text())
        for kind in ("sqlite", "mysql", "postgres", "existing_mysql", "remote"):
            config = json.loads(json.dumps(base))
            if kind == "remote":
                config["remote"]["host"] = "74"
            else:
                item = next(x for x in config["targets"] if x["kind"] == kind)
                key = {"sqlite": "source", "mysql": "database", "postgres": "database", "existing_mysql": "source_dir"}[kind]
                item[key] = "/tmp/unapproved" if kind in ("sqlite", "existing_mysql") else "unapproved"
            p = self.root / (kind + ".json")
            p.write_text(json.dumps(config))
            with self.assertRaises(common.BackupError):
                backup.load_config(p)

    def test_incomplete_mysql_dump_with_good_gzip_is_rejected(self):
        p = self.root / "broken.sql.gz"
        with gzip.open(p, "wb") as f:
            f.write(b"CREATE TABLE `orders` (`id` int);\n")
        p.chmod(0o600)
        with self.assertRaises(common.BackupError):
            common.sql_validate(p, time.monotonic() + 5)

    def test_mysql_definition_at_chunk_boundary_is_counted_once(self):
        p = self.root / "boundary.sql.gz"
        payload = b"-" * (4 * 1024 * 1024 - 5) + b"\nCREATE TABLE `orders` (`id` int);\n-- Dump completed on 2026-09-30\n"
        with gzip.open(p, "wb") as f:
            f.write(payload)
        p.chmod(0o600)
        validation = common.sql_validate(p, time.monotonic() + 5)
        self.assertEqual(validation["table_definitions"], 1)
        self.assertEqual(validation["raw_bytes"], len(payload))

    def test_successful_targets_survive_another_target_failure(self):
        source, c = self.sqlite_source()
        try:
            output = self.root / "backups"
            config = {"local_root": str(output), "minimum_free_bytes": 0, "remote": {"host": "50", "root": str(common.REMOTE_ROOT)}, "targets": [self.item(source), {**self.item(source), "id": "missing", "source": str(self.root / "missing.sqlite")}]}
            with contextlib.redirect_stdout(io.StringIO()):
                directory, manifest = backup.run_backup(config)
            self.assertFalse(manifest["ok"])
            self.assertEqual([r["status"] for r in manifest["results"]], ["ok", "failed"])
            self.assertTrue((directory / "fixture.sqlite.gz").is_file())
            self.assertFalse(any(output.glob(".pending-*")))
        finally:
            c.close()

    def test_mysql_client_wrapper_has_private_marker_and_no_password_argument(self):
        raw = self.root / "mysql.sql"
        run_id = "20260930T000000Z-8765dcba"
        marker = Path("/tmp/audit-core-" + run_id + ".pid")
        fake_bin = self.root / "bin"
        fake_bin.mkdir(mode=0o700)
        fake_dump = fake_bin / "mysqldump"
        fake_dump.write_text("#!/bin/sh\nprintf '%s\\n' '-- MySQL dump' 'CREATE TABLE `orders` (`id` int);' '-- Dump completed on 2026-09-30'\n")
        fake_dump.chmod(0o700)
        real_run = subprocess.run
        calls = []

        def fake_docker(args, **kwargs):
            calls.append(args)
            self.assertEqual(args[:3], ["docker", "exec", "99code-mysql"])
            env = {**os.environ, "PATH": str(fake_bin) + ":" + os.environ["PATH"], "MYSQL_ROOT_PASSWORD": "synthetic-not-a-secret"}
            return real_run(args[3:], env=env, **kwargs)

        try:
            with patch.object(backup.subprocess, "run", side_effect=fake_docker), patch.object(backup, "mysql_cleanup"):
                backup.dump_database({"kind": "mysql", "container": "99code-mysql", "database": "newapi_523", "timeout_seconds": 30}, raw, run_id)
            self.assertRegex(marker.read_text(), r"^[0-9]+ [0-9]+\n$")
            self.assertEqual(marker.stat().st_mode & 0o777, 0o600)
            self.assertNotIn("synthetic-not-a-secret", str(calls))
            self.assertIn("newapi_523", calls[0])
            self.assertNotIn("--all-databases", calls[0][5])
        finally:
            if marker.exists():
                marker.unlink()

    def test_mysql_cleanup_only_cancels_the_matching_dump_connection(self):
        completed = subprocess.CompletedProcess
        def command(args, **kwargs):
            if "cat" in args:
                return completed(args, 0, stdout=b"123 999\n", stderr=b"")
            return completed(args, 0, stdout=b"", stderr=b"")
        with patch.object(backup.subprocess, "run", side_effect=command), patch.object(backup, "container_query", side_effect=[["55"], [], [], []]) as query:
            backup.mysql_cleanup("99code-mysql", "/tmp/owned.pid", "newapi_523")
        queries = [call.args[2] for call in query.call_args_list]
        self.assertIn("a.ATTR_VALUE='123'", queries[0])
        self.assertIn("b.ATTR_VALUE='mysqldump'", queries[0])
        self.assertEqual([q for q in queries if q.startswith("KILL ")], ["KILL 55;"])
        self.assertEqual(len(queries), 4)

    def test_reused_mysql_client_pid_is_never_killed(self):
        responses = [subprocess.CompletedProcess([], 0, stdout=b"123 999\n", stderr=b""), subprocess.CompletedProcess([], 3)]
        with patch.object(backup.subprocess, "run", side_effect=responses), patch.object(backup, "container_query") as query:
            with self.assertRaises(common.BackupError):
                backup.mysql_cleanup("99code-mysql", "/tmp/owned.pid", "newapi_523")
        query.assert_not_called()

    def archive_for_retention(self, name, age_days, receipt=True, extra=False, scope="fixture"):
        directory = self.root / name
        directory.mkdir(mode=0o700)
        artifact = directory / (scope + ".sql.gz")
        with gzip.open(artifact, "wb") as f:
            f.write(SQL)
        artifact.chmod(0o600)
        item = {"id": scope, "kind": "mysql", "status": "ok", "file": artifact.name, "bytes": artifact.stat().st_size, "raw_bytes": len(SQL), "sha256": common.digest(artifact), "validation": {"table_definitions": 1}}
        created = (dt.datetime.now(dt.timezone.utc) - dt.timedelta(days=age_days)).isoformat()
        self.manifest(directory, item, name, created)
        if receipt:
            common.json_write(directory / "replica.json", {"ok": True, "manifest_sha256": common.digest(directory / "manifest.json")})
        if extra:
            (directory / "user-file.txt").write_text("preserve")
        return directory

    def test_retention_requires_verified_replica_and_does_not_delete_user_files(self):
        fresh = self.archive_for_retention("20260930T000000Z-1234abcd", 0)
        expired = self.archive_for_retention("20260920T000000Z-1111abcd", 10)
        no_receipt = self.archive_for_retention("20260920T000000Z-2222abcd", 10, receipt=False)
        with_extra = self.archive_for_retention("20260920T000000Z-3333abcd", 10, extra=True)
        other_scope = self.archive_for_retention("20260920T000000Z-4444abcd", 10, scope="different")
        recent = self.archive_for_retention("20260929T000000Z-5555abcd", 1)
        result = common.prune_runs(self.root, fresh.name)
        self.assertEqual(result["removed"], [expired.name])
        for directory in (no_receipt, with_extra, other_scope, recent, fresh):
            self.assertTrue(directory.exists())

    def test_bad_manifest_cannot_escape_archive_directory(self):
        source, c = self.sqlite_source()
        try:
            item = backup.create_item(self.item(source), self.root, "20260930T000000Z-1234abcd")
            item["file"] = "../fixture.sqlite.gz"
            manifest = self.manifest(self.root, item)
            with self.assertRaises(common.BackupError):
                common.validate_manifest(manifest)
        finally:
            c.close()


if __name__ == "__main__":
    unittest.main()
