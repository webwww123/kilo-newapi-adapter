#!/usr/bin/env python3
"""验证备份遗漏、空表、失败发布与自身查询取消边界。"""
import contextlib
import gzip
import io
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import dump
import install

BUSINESS = (b"-- MySQL dump\nCREATE TABLE `users` (`id` int);\n"
            b"INSERT INTO `users` VALUES (1),(2);\n"
            b"CREATE TABLE `tokens` (`id` int);\n"
            b"INSERT INTO `tokens` VALUES (3);\n"
            b"-- Dump completed on 2026-09-30\n")
SCHEMA = (b"CREATE TABLE `logs` (`id` bigint, KEY `id` (`id`));\n"
          b"CREATE TABLE `error_request_traces` (`id` bigint, KEY `id` (`id`));\n"
          b"-- Dump completed on 2026-09-30\n")
TABLES = {"users", "tokens", "logs", "error_request_traces"}


class DumpTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.old_umask = os.umask(0o077)

    def tearDown(self):
        os.umask(self.old_umask)
        self.temporary.cleanup()

    def archive(self, payload):
        path = self.root / "fixture.sql.gz"
        with gzip.open(path, "wb") as stream:
            stream.write(payload)
        return path

    def test_both_large_tables_have_schema_and_no_data(self):
        path = self.root / "backup.sql.gz"
        def stage(stream, schema):
            stream.write(SCHEMA if schema else BUSINESS)
        with patch.object(dump, "source_tables", return_value=TABLES), patch.object(dump, "dump_stage", side_effect=stage), contextlib.redirect_stdout(io.StringIO()):
            dump.create_archive(path)
        with gzip.open(path, "rb") as stream:
            self.assertEqual(stream.read(), BUSINESS + b"\n" + SCHEMA)
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        data_args, schema_args = dump.dump_args(False), dump.dump_args(True)
        for table in dump.SCHEMA_ONLY:
            self.assertIn("--ignore-table=defaultdb." + table, data_args)
            self.assertIn(table, schema_args)
        self.assertIn("--single-transaction", data_args)
        self.assertIn("--no-data", schema_args)

    def test_failed_second_dump_never_replaces_existing_backup(self):
        latest = self.root / "latest.sql.gz"
        latest.write_bytes(b"previous-backup")
        path = self.root / "new.sql.gz"
        def stage(stream, schema):
            if schema:
                raise dump.BackupError("fixture_second_stage_failure")
            stream.write(BUSINESS)
        with patch.object(dump, "source_tables", return_value=TABLES), patch.object(dump, "dump_stage", side_effect=stage):
            with self.assertRaises(dump.BackupError):
                dump.create_archive(path)
        self.assertFalse(path.exists())
        self.assertEqual(latest.read_bytes(), b"previous-backup")
        self.assertFalse(list(self.root.glob("*.part-*")))

    def test_existing_output_cannot_be_overwritten(self):
        path = self.root / "existing.sql.gz"
        path.write_bytes(b"previous-backup")
        with patch.object(dump, "source_tables", return_value=TABLES):
            with self.assertRaises(dump.BackupError):
                dump.create_archive(path)
        self.assertEqual(path.read_bytes(), b"previous-backup")

    def test_excluded_rows_are_rejected(self):
        path = self.archive(BUSINESS + SCHEMA + b"INSERT INTO `error_request_traces` VALUES (1);\n")
        with self.assertRaisesRegex(dump.BackupError, "excluded_rows"):
            dump.validate_archive(path, TABLES)

    def test_missing_business_schema_or_incomplete_dump_is_rejected(self):
        for payload in [BUSINESS + SCHEMA.replace(b"CREATE TABLE `logs`", b"-- lost table `logs`"), BUSINESS + SCHEMA.replace(b"-- Dump completed on ", b"-- truncated ")]:
            path = self.archive(payload)
            with self.assertRaisesRegex(dump.BackupError, "schema_incomplete"):
                dump.validate_archive(path, TABLES)

    def test_markers_across_chunk_boundary_are_not_lost_or_double_counted(self):
        prefix = b"-" * (1024 * 1024 - 8) + b"\n"
        dump.validate_archive(self.archive(prefix + BUSINESS + SCHEMA), TABLES)

    def test_table_inventory_cannot_silently_omit_diagnostics(self):
        with patch.object(dump, "container_query", return_value=["users", "tokens", "logs"]):
            with self.assertRaises(dump.BackupError):
                dump.source_tables()

    def test_connection_identity_is_tied_to_exact_client_and_database(self):
        with patch.object(dump, "container_query", return_value=["321\t3\tSleep\t-"]) as query:
            self.assertEqual(dump.own_connections(8765), [(321, 3, "Sleep", "-")])
        sql = query.call_args.args[2]
        for fragment in ["a.ATTR_NAME='_pid'", "a.ATTR_VALUE='8765'", "b.ATTR_VALUE='mysqldump'", "p.DB='defaultdb'", "p.USER='root'"]:
            self.assertIn(fragment, sql)

    def test_server_cancellation_is_only_for_the_owned_over_budget_connection(self):
        with patch.object(dump, "own_connections", return_value=[(321, 120, "Query", "Sending data")]), patch.object(dump, "container_query") as query:
            with self.assertRaises(dump.BackupError):
                dump.enforce_query_budget(8765)
        query.assert_called_once_with("newapi-mysql", "mysql", "KILL 321;")

    def test_waiting_lock_is_bounded_but_normal_state_and_sleep_are_accepted(self):
        with patch.object(dump, "own_connections", return_value=[(321, 5, "Query", "Waiting for table metadata lock")]), patch.object(dump, "container_query") as query:
            with self.assertRaises(dump.BackupError):
                dump.enforce_query_budget(8765)
        query.assert_called_once_with("newapi-mysql", "mysql", "KILL 321;")
        for row in [(321, 6, "Query", "System lock"), (321, 125, "Sleep", "-")]:
            with patch.object(dump, "own_connections", return_value=[row]), patch.object(dump, "container_query") as query:
                dump.enforce_query_budget(8765)
                query.assert_not_called()

    def test_ambiguous_connections_are_never_cancelled(self):
        with patch.object(dump, "container_query", return_value=["321\t120\tQuery\tSending data", "322\t120\tQuery\tSending data"]) as query:
            with self.assertRaises(dump.BackupError):
                dump.own_connections(8765)
        self.assertEqual(query.call_count, 1)

    def test_credential_parser_is_internal_and_rejects_ambiguous_formats(self):
        self.assertEqual(install.legacy_password("MYSQL_PASS='fixture-value'\n"), "fixture-value")
        for text in ["MYSQL_PASS='one'\nMYSQL_PASS='two'\n", "MYSQL_PASS=\n", "MYSQL_PASS='multiline\nvalue'\n"]:
            with self.assertRaises(RuntimeError):
                install.legacy_password(text)


if __name__ == "__main__":
    unittest.main()
