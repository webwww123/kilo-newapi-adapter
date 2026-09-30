#!/usr/bin/env python3
import gzip
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

import common
import restore_isolated as restore

SQL = b"-- MySQL dump\nCREATE TABLE `users` (`id` int);\nINSERT INTO `users` VALUES (1);\n-- Dump completed on 2026-09-30\n"
CID = "a" * 64


class RestoreTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.old_umask = os.umask(0o077)

    def tearDown(self):
        os.umask(self.old_umask)
        self.temp.cleanup()

    def archive(self):
        p = self.root / "99code.sql.gz"
        with gzip.open(p, "wb") as f:
            f.write(SQL)
        p.chmod(0o600)
        return p, {"id": "99code", "kind": "mysql", "status": "ok", "file": p.name, "bytes": p.stat().st_size, "raw_bytes": len(SQL), "sha256": common.digest(p), "validation": {"table_definitions": 1}}

    def test_container_is_isolated_and_secret_is_only_a_file_reference(self):
        for name, target in restore.TARGETS.items():
            args = restore.create_args(target, "mysql:8.0", "audit-core-restore-test", self.root)
            self.assertEqual(args[args.index("--network") + 1], "none")
            self.assertEqual(args[args.index("--cpus") + 1], "1")
            self.assertEqual(args[args.index("--pull") + 1], "never")
            self.assertNotIn("--publish", args)
            self.assertNotIn("-p", args)
            self.assertNotIn("--privileged", args)
            self.assertTrue(any("PASSWORD_FILE=/run/secrets/root-password" in a for a in args))
            self.assertFalse(any("PASSWORD=" in a for a in args))

    def test_bad_archive_does_not_create_a_container(self):
        archive, item = self.archive()
        item["sha256"] = "0" * 64
        with patch.object(restore, "resource_check"), patch.object(restore, "source_image", return_value="mysql:8.0"), patch.object(restore, "command") as cmd:
            with self.assertRaises(common.BackupError):
                restore.restore_item(self.root, item)
        cmd.assert_not_called()
        self.assertTrue(archive.exists())

    def test_mysql_counts_are_checked_only_on_the_owned_copy(self):
        item = {"validation": {"table_definitions": 31}}
        with patch.object(restore, "query", side_effect=["31", "users\ntokens\nredemptions\ntop_ups\nchannels", "4", "7", "8", "9", "10"]) as query:
            result = restore.metrics(CID, restore.TARGETS["99code"], item)
        self.assertEqual(result["key_table_counts"], {"users": 4, "tokens": 7, "redemptions": 8, "top_ups": 9, "channels": 10})
        self.assertTrue(all(call.args[0] == CID for call in query.call_args_list))

    def test_table_count_mismatch_is_a_failed_restore(self):
        with patch.object(restore, "query", return_value="30"):
            with self.assertRaises(common.BackupError):
                restore.metrics(CID, restore.TARGETS["99code"], {"validation": {"table_definitions": 31}})

    def test_missing_required_business_table_is_a_failed_restore(self):
        with patch.object(restore, "query", side_effect=["31", "users\ntokens"]):
            with self.assertRaises(common.BackupError):
                restore.metrics(CID, restore.TARGETS["99code"], {"validation": {"table_definitions": 31}})

    def test_cleanup_only_removes_created_full_container_id_and_own_directory(self):
        work = self.root / "job-fixture"
        work.mkdir(mode=0o700)
        (work / ".owned").write_text(CID + "\n")
        calls = []
        def fake_run(args, **kwargs):
            calls.append(args)
            return subprocess.CompletedProcess(args, 0, stdout=b"", stderr=b"")
        with patch.object(restore, "BASE", self.root), patch.object(restore.subprocess, "run", side_effect=fake_run):
            self.assertTrue(restore.cleanup(CID, work))
        self.assertFalse(work.exists())
        self.assertEqual(calls[0], ["docker", "stop", "--time", "10", CID])
        self.assertEqual(calls[1], ["docker", "rm", "-f", "-v", CID])

    def test_container_remaining_preserves_its_data_directory(self):
        work = self.root / "job-fixture"
        work.mkdir(mode=0o700)
        (work / ".owned").write_text(CID + "\n")
        def fake_run(args, **kwargs):
            return subprocess.CompletedProcess(args, 0, stdout=(b"aaaaaaaaaaaa\n" if "ps" in args else b""), stderr=b"")
        with patch.object(restore, "BASE", self.root), patch.object(restore.subprocess, "run", side_effect=fake_run):
            self.assertFalse(restore.cleanup(CID, work))
        self.assertTrue(work.exists())

    def test_failed_import_still_cleans_own_container_and_keeps_original_backup(self):
        archive, item = self.archive()
        before = common.digest(archive)
        base = self.root / "restore-tests"
        def fake_command(args, timeout=30, stdin=None):
            if args[1] == "create":
                cidfile = Path(args[args.index("--cidfile") + 1])
                cidfile.write_text(CID + "\n")
                cidfile.chmod(0o600)
                return CID
            return CID
        def fake_cleanup(cid, work):
            self.assertEqual(cid, CID)
            self.assertEqual(work.parent, base)
            shutil.rmtree(work)
            return True
        with patch.object(restore, "BASE", base), patch.object(restore, "resource_check"), patch.object(restore, "source_image", return_value="mysql:8.0"), patch.object(restore, "command", side_effect=fake_command), patch.object(restore, "wait_ready"), patch.object(restore, "import_archive", side_effect=common.BackupError("isolated_import_failed")), patch.object(restore, "cleanup", side_effect=fake_cleanup) as cleanup:
            result = restore.restore_item(self.root, item)
        self.assertEqual(result["status"], "failed")
        self.assertTrue(result["container_removed"])
        cleanup.assert_called_once()
        self.assertEqual(common.digest(archive), before)
        self.assertFalse(any(base.glob("job-*")))


if __name__ == "__main__":
    unittest.main()
