import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import tarfile
import tempfile
import unittest
from unittest import mock

import backup
import verify


class SourceBackupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.base = Path(self.temp.name)
        self.root = self.base / "project"
        self.root.mkdir()
        self.work = self.base / "backup"

    def tearDown(self):
        self.temp.cleanup()

    def initialize(self):
        backup.git(self.root, "init")
        backup.git(self.root, "config", "user.name", "Backup Test")
        backup.git(self.root, "config", "user.email", "backup-test@example.invalid")
        backup.git(self.root, "config", "core.hooksPath", "/dev/null")

    def commit(self):
        backup.git(self.root, "add", "--all")
        backup.git(self.root, "commit", "-m", "fixture")

    def test_dirty_staged_unstaged_and_all_refs_restore(self):
        self.initialize()
        (self.root / "app.py").write_text("print('initial')\n")
        self.commit()
        backup.git(self.root, "branch", "local-only")
        backup.git(self.root, "tag", "fixture-tag")
        (self.root / "app.py").write_text("print('staged')\n")
        backup.git(self.root, "add", "app.py")
        (self.root / "app.py").write_text("print('unstaged')\n")
        (self.root / "new.py").write_text("print('untracked')\n")
        before = backup.git_state(self.root)
        archive, manifest = backup.build_archive(self.root, self.work)
        restored = self.base / "restored"
        verify.verify_archive(archive, restored)
        self.assertEqual((restored / "worktree/app.py").read_text(), "print('unstaged')\n")
        self.assertTrue((restored / "changes/staged.patch").stat().st_size > 0)
        self.assertTrue((restored / "changes/unstaged.patch").stat().st_size > 0)
        self.assertEqual(verify.refs(restored / "restore.git"), before["refs"])
        self.assertEqual(before, backup.git_state(self.root))
        self.assertTrue(manifest["git"]["head"])

    def test_no_git_and_deployment_secret_exclusions(self):
        for name in [".env", ".env.production", "config.env", "credentials.json", "private.pem", "id_ed25519"]:
            (self.root / name).write_text("synthetic fixture; not a real credential\n")
        (self.root / ".env.example").write_text("API_KEY=<API_KEY>\n")
        (self.root / "token.go").write_text("package token\n")
        (self.root / "Caddyfile").write_text("example.invalid {\n respond ok\n}\n")
        for directory in ["node_modules", ".next", "data", "logs", ".planning"]:
            (self.root / directory).mkdir()
            (self.root / directory / "generated.txt").write_text("excluded")
        _, manifest = backup.build_archive(self.root, self.work)
        self.assertFalse(manifest["git"]["enabled"])
        self.assertIn("worktree/.env.example", manifest["files"])
        self.assertIn("worktree/token.go", manifest["files"])
        self.assertIn("private-config/Caddyfile", manifest["files"])
        self.assertNotIn("worktree/Caddyfile", manifest["files"])
        self.assertEqual(manifest["omission_counts"]["credential"], 6)

    def test_empty_git_repository_saves_uncommitted_source(self):
        self.initialize()
        (self.root / "main.py").write_text("print('new project')\n")
        backup.git(self.root, "add", "main.py")
        archive, manifest = backup.build_archive(self.root, self.work)
        self.assertTrue(manifest["git"]["enabled"])
        self.assertIsNone(manifest["git"]["head"])
        self.assertNotIn("history.bundle", manifest["files"])
        self.assertIn("worktree/main.py", manifest["files"])
        verify.verify_archive(archive, self.base / "restore")

    def test_nested_repository_and_symlink_boundaries(self):
        (self.root / "app.py").write_text("main\n")
        outside = self.base / "outside.txt"
        outside.write_text("synthetic outside file\n")
        (self.root / "linked.py").symlink_to(outside)
        child = self.root / "nested"
        child.mkdir()
        backup.git(child, "init")
        (child / "child.py").write_text("separate project\n")
        _, manifest = backup.build_archive(self.root, self.work)
        self.assertEqual(manifest["omission_counts"]["nested_repository"], 1)
        self.assertEqual(manifest["omission_counts"]["symlink_or_special"], 1)
        self.assertNotIn("worktree/nested/child.py", manifest["files"])
        self.assertNotIn("worktree/linked.py", manifest["files"])

    def test_parent_git_is_not_mistaken_for_source_repository(self):
        self.initialize()
        child = self.root / "inner"
        child.mkdir()
        with self.assertRaisesRegex(RuntimeError, "different Git repository"):
            backup.git_state(child)

    def test_large_files_and_compiled_binary_are_explicit_gaps(self):
        with (self.root / "large.bin").open("wb") as sink:
            sink.truncate(backup.MAX_FILE + 1)
        (self.root / "compiled").write_bytes(b"\x7fELFfixture")
        (self.root / "app.py").write_text("print('ok')\n")
        _, manifest = backup.build_archive(self.root, self.work)
        self.assertEqual(manifest["omission_counts"]["oversized"], 1)
        self.assertEqual(manifest["omission_counts"]["compiled_binary"], 1)

    def test_file_drift_refuses_success(self):
        source = self.root / "app.py"
        source.write_text("before\n")
        original = backup.snapshot_file

        def drift(*args):
            original(*args)
            source.write_text("after\n")

        with mock.patch.object(backup, "snapshot_file", side_effect=drift):
            with self.assertRaisesRegex(RuntimeError, "drift"):
                backup.build_archive(self.root, self.work)

    def test_head_drift_refuses_success(self):
        self.initialize()
        (self.root / "app.py").write_text("before\n")
        self.commit()
        original = backup.snapshot_file

        def drift(*args):
            original(*args)
            backup.git(self.root, "commit", "--allow-empty", "-m", "new head")

        with mock.patch.object(backup, "snapshot_file", side_effect=drift):
            with self.assertRaisesRegex(RuntimeError, "drift"):
                backup.build_archive(self.root, self.work)

    def test_unsafe_archive_member_is_rejected_before_write(self):
        archive = self.base / "bad.tar.gz"
        with tarfile.open(archive, "w:gz") as package:
            member = tarfile.TarInfo("../escaped")
            member.size = 3
            package.addfile(member, io.BytesIO(b"bad"))
        with self.assertRaisesRegex(RuntimeError, "unsafe archive member"):
            verify.verify_archive(archive, self.base / "restore")
        self.assertFalse((self.base / "escaped").exists())

    def test_member_hash_mismatch_is_rejected(self):
        archive = self.base / "bad.tar.gz"
        manifest = {"files": {"worktree/app.py": {"size": 3, "sha256": "0" * 64}},
                    "git": {"head": None}, "omission_counts": {}}
        with tarfile.open(archive, "w:gz") as package:
            for name, value in [("manifest.json", json.dumps(manifest).encode()), ("worktree/app.py", b"bad")]:
                member = tarfile.TarInfo(name)
                member.size = len(value)
                package.addfile(member, io.BytesIO(value))
        with self.assertRaisesRegex(RuntimeError, "hash mismatch"):
            verify.verify_archive(archive, self.base / "restore")

    def test_retention_only_removes_verified_owned_backups(self):
        directory = self.base / "retention"
        directory.mkdir()
        for day in range(1, 10):
            name = f"200001{day:02d}T000000Z-abcdef12.tar.gpg"
            artifact = directory / name
            artifact.write_bytes(b"encrypted-test-fixture")
            (directory / (name + ".verified.json")).write_text(json.dumps({
                "artifact": name, "verified": True, "sha256": verify.sha256(artifact)}))
        failed = directory / "20000201T000000Z-abcdef12.tar.gpg"
        failed.write_bytes(b"unverified-fixture")
        unrelated = directory / "user-backup.gpg"
        unrelated.write_bytes(b"user-file")
        verify.prune(directory)
        self.assertEqual(len(list(directory.glob("*.verified.json"))), 7)
        self.assertTrue(failed.exists())
        self.assertTrue(unrelated.exists())

    def test_existing_key_is_not_overwritten(self):
        directory = self.base / "secrets"
        directory.mkdir(mode=0o700)
        key = directory / "pass"
        backup.ensure_key(key)
        before = key.read_bytes()
        backup.ensure_key(key)
        self.assertTrue(before == key.read_bytes())
        self.assertEqual(key.stat().st_mode & 0o777, 0o600)
        key.chmod(0o644)
        with self.assertRaisesRegex(RuntimeError, "unsafe encryption key"):
            backup.ensure_key(key)

    @unittest.skipUnless(shutil.which("gpg"), "gpg not installed")
    def test_aes256_roundtrip_restores_archive(self):
        (self.root / "app.py").write_text("print('roundtrip')\n")
        archive, _ = backup.build_archive(self.root, self.work)
        key_dir = self.base / "secrets"
        key_dir.mkdir(mode=0o700)
        key = key_dir / "pass"
        backup.ensure_key(key)
        home = backup.private_dir(self.base / "gnupg")
        encrypted, plain = self.base / "source.gpg", self.base / "decrypted.tar.gz"
        common = ["gpg", "--no-options", "--batch", "--quiet", "--no-symkey-cache", "--homedir", str(home),
                  "--pinentry-mode", "loopback", "--passphrase-file", str(key)]
        backup.command(common + ["--symmetric", "--cipher-algo", "AES256", "--output", str(encrypted), str(archive)])
        backup.command(common + ["--output", str(plain), "--decrypt", str(encrypted)])
        verify.stop_private_agent(home)
        restored = self.base / "decrypted-restore"
        verify.verify_archive(plain, restored)
        self.assertEqual((restored / "worktree/app.py").read_text(), "print('roundtrip')\n")


if __name__ == "__main__":
    unittest.main()
