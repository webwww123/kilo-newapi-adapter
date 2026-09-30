import hashlib
from contextlib import contextmanager
import io
import json
import os
from pathlib import Path
import re
import shutil
import tarfile
import tempfile
import time
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

    def shallow_fixture(self):
        self.initialize()
        (self.root / "app.py").write_text("print('old upstream')\n")
        self.commit()
        older = backup.git(self.root, "rev-parse", "HEAD").decode().strip()
        (self.root / "app.py").write_text("print('shallow boundary')\n")
        self.commit()
        upstream = self.base / "upstream"
        self.root.rename(upstream)
        # 仅测试夹具通过 file:// 建浅仓，生产备份不 fetch、不补历史。
        backup.command(["git", "clone", "--depth", "1", upstream.as_uri(), str(self.root)])
        self.initialize()
        (self.root / "app.py").write_text("print('local commit')\n")
        self.commit()
        backup.git(self.root, "branch", "local-only")
        backup.git(self.root, "tag", "local-tag")
        return upstream, older

    def rewrite_archive(self, original, destination, change):
        with tarfile.open(original, "r:gz") as package:
            contents = {m.name: package.extractfile(m).read() for m in package.getmembers()}
        manifest = json.loads(contents["manifest.json"])
        change(manifest, contents)
        contents["manifest.json"] = json.dumps(manifest).encode()
        with tarfile.open(destination, "w:gz") as package:
            for name, value in contents.items():
                member = tarfile.TarInfo(name)
                member.size = len(value)
                package.addfile(member, io.BytesIO(value))

    def test_shallow_local_commit_and_wip_restore_without_original_sources(self):
        upstream, older = self.shallow_fixture()
        (self.root / "app.py").write_text("print('staged')\n")
        backup.git(self.root, "add", "app.py")
        (self.root / "app.py").write_text("print('unstaged')\n")
        (self.root / "local.py").write_text("print('untracked')\n")
        before = backup.git_state(self.root)
        archive, manifest = backup.build_archive(self.root, self.work)
        self.assertEqual(before, backup.git_state(self.root))
        self.assertTrue(manifest["git"]["shallow"])
        self.assertNotIn("history.bundle", manifest["files"])
        self.root.rename(self.base / "source-offline")
        upstream.rename(self.base / "upstream-offline")
        restored = self.base / "restored-shallow"
        verify.verify_archive(archive, restored)
        history = restored / "history.git"
        self.assertEqual(verify.refs(history), before["refs"])
        self.assertEqual(backup.git(history, "rev-parse", "HEAD").decode().strip(), before["head"])
        self.assertEqual(backup.git(history, "rev-parse", "--is-shallow-repository").strip(), b"true")
        self.assertEqual(verify.sha256(history / "shallow"), before["shallow_sha256"])
        self.assertFalse((history / "objects/info/alternates").exists())
        self.assertTrue(all(p.stat().st_nlink == 1 for p in history.rglob("*") if p.is_file()))
        backup.git(history, "fsck", "--connectivity-only", "--no-reflogs")
        with self.assertRaises(RuntimeError):
            backup.git(history, "cat-file", "-e", older + "^{commit}")
        self.assertEqual((restored / "worktree/app.py").read_text(), "print('unstaged')\n")
        self.assertEqual((restored / "worktree/local.py").read_text(), "print('untracked')\n")
        self.assertGreater((restored / "changes/staged.patch").stat().st_size, 0)
        self.assertGreater((restored / "changes/unstaged.patch").stat().st_size, 0)

    def test_shallow_detached_head_is_preserved(self):
        self.shallow_fixture()
        backup.git(self.root, "checkout", "--detach")
        (self.root / "app.py").write_text("print('detached local commit')\n")
        self.commit()
        before = backup.git_state(self.root)
        self.assertIsNone(before["symbolic_head"])
        archive, _ = backup.build_archive(self.root, self.work)
        self.root.rename(self.base / "source-offline")
        target = self.base / "restore-detached"
        verify.verify_archive(archive, target)
        self.assertEqual((target / "history.git/HEAD").read_text().strip(), before["head"])

    def test_shallow_boundary_drift_during_restore_refuses_success(self):
        self.shallow_fixture()
        original = backup.verify_archive

        def drift(*args):
            value = original(*args)
            boundary = self.root / ".git/shallow"
            boundary.write_text(boundary.read_text() + backup.git_state(self.root)["head"] + "\n")
            return value

        with mock.patch.object(backup, "verify_archive", side_effect=drift):
            with self.assertRaisesRegex(RuntimeError, "source drift during restore"):
                backup.build_archive(self.root, self.work)

    def test_shallow_restore_rejects_alternates_dependency(self):
        self.shallow_fixture()
        archive, _ = backup.build_archive(self.root, self.work)

        def dependency(manifest, contents):
            name = "history.git/objects/info/alternates"
            value = b"/nonexistent-test-only-source/objects\n"
            contents[name] = value
            manifest["files"][name] = {"size": len(value), "sha256": hashlib.sha256(value).hexdigest()}

        bad = self.base / "dependent.tar.gz"
        self.rewrite_archive(archive, bad, dependency)
        with self.assertRaisesRegex(RuntimeError, "git_history_dependency"):
            verify.verify_archive(bad, self.base / "restore-dependent")

    def test_shallow_restore_requires_original_refs_and_head(self):
        self.shallow_fixture()
        archive, _ = backup.build_archive(self.root, self.work)
        for field, value, expected in [("refs", [], "restored refs mismatch"),
                                       ("head", "0" * 40, "git_history_head_mismatch")]:
            with self.subTest(field=field):
                bad = self.base / (field + ".tar.gz")
                self.rewrite_archive(archive, bad, lambda manifest, _: manifest["git"].update({field: value}))
                with self.assertRaisesRegex(RuntimeError, expected):
                    verify.verify_archive(bad, self.base / ("restore-" + field))

    def test_failure_report_only_contains_fixed_safe_codes(self):
        external = RuntimeError("synthetic private stderr must stay hidden")
        self.assertEqual(verify.safe_error(external), {"error_type": "RuntimeError", "error_code": "unexpected_error"})
        self.assertEqual(verify.safe_error(RuntimeError("source drift; snapshot refused"))["error_code"],
                         "source_snapshot_drift")
        with self.assertRaises(RuntimeError) as raised:
            backup.command(["python3", "-c", "import sys; sys.stderr.write('synthetic private stderr'); sys.exit(1)"],
                           failure="gpg_encrypt_failed")
        self.assertEqual(verify.safe_error(raised.exception)["error_code"], "gpg_encrypt_failed")

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
        encrypted, plain = self.base / "source.gpg", self.base / "decrypted.tar.gz"
        with verify.temporary_gpg_home() as home:
            common = ["gpg", "--no-options", "--batch", "--quiet", "--no-symkey-cache", "--homedir", str(home),
                      "--pinentry-mode", "loopback", "--passphrase-file", str(key)]
            backup.command(common + ["--symmetric", "--cipher-algo", "AES256", "--output", str(encrypted), str(archive)])
            backup.command(common + ["--output", str(plain), "--decrypt", str(encrypted)])
        restored = self.base / "decrypted-restore"
        verify.verify_archive(plain, restored)
        self.assertEqual((restored / "worktree/app.py").read_text(), "print('roundtrip')\n")

    @unittest.skipUnless(os.geteuid() == 0 and all(shutil.which(name) for name in
                         ["gpg", "gpgconf", "gpg-connect-agent"]), "requires root and GPG tools")
    def test_long_repository_id_real_local_and_remote_gpg_with_agent_exit(self):
        self.root.rename(self.base / ("long-project-" + "x" * 90))
        self.root = self.base / ("long-project-" + "x" * 90)
        (self.root / "app.py").write_text("print('long path')\n")
        key_dir = verify.private_dir(self.base / "secrets")
        key = key_dir / "pass"
        backup.ensure_key(key)
        local, remote = self.base / "local", self.base / "remote"
        old_home = local / backup.repository_id(self.root) / ".snapshot-12345678" / "gnupg"
        self.assertGreaterEqual(len(str(old_home / "S.gpg-agent.browser").encode()), 108)
        original_command = backup.command
        original_home = verify.temporary_gpg_home
        agents = []

        @contextmanager
        def observed_home():
            with original_home() as home:
                self.assertLess(len(str(home / "S.gpg-agent.browser").encode()), 108)
                self.assertEqual(home.stat().st_mode & 0o777, 0o700)
                response = verify.run(["gpg-connect-agent", "--homedir", str(home), "GETINFO pid", "/bye"], timeout=10)
                pid = int(re.search(rb"^D ([0-9]+)$", response, re.MULTILINE).group(1))
                agents.append((home, pid))
                yield home

        def local_transport(args, **kwargs):
            if args[0] == "scp":
                destination = Path(args[-1].removeprefix("50:"))
                self.assertTrue(destination.is_relative_to(remote))
                shutil.copyfile(args[-2], destination)
                destination.chmod(0o600)
                return b""
            return original_command(args, **kwargs)

        def local_remote(script, *arguments, **kwargs):
            if script == backup.PREPARE_SCRIPT:
                verify.private_dir(verify.private_dir(remote) / arguments[0])
                return b"ready"
            return json.dumps(verify.remote_verify(*arguments)).encode()

        with mock.patch.object(backup, "LOCAL_ROOT", local), mock.patch.object(backup, "REMOTE_ROOT", remote), \
                mock.patch.object(verify, "REMOTE_ROOT", remote), \
                mock.patch.object(backup, "KEY", key), mock.patch.object(verify, "KEY", key), \
                mock.patch.object(backup, "MIN_FREE", 0), mock.patch.object(verify, "MIN_FREE", 0), \
                mock.patch.object(backup, "command", side_effect=local_transport), \
                mock.patch.object(backup, "remote_python", side_effect=local_remote), \
                mock.patch.object(backup, "temporary_gpg_home", side_effect=observed_home), \
                mock.patch.object(verify, "temporary_gpg_home", side_effect=observed_home):
            result = backup.backup_repository(self.root)
        self.assertTrue(result["verified"])
        self.assertEqual(len(agents), 2)
        for home, pid in agents:
            self.assertFalse(home.exists())
            deadline = time.monotonic() + 5
            running = True
            while time.monotonic() < deadline:
                try:
                    state = Path(f"/proc/{pid}/stat").read_text().rpartition(") ")[2].split()[0]
                    running = state not in {"Z", "X"}
                except FileNotFoundError:
                    running = False
                if not running:
                    break
                time.sleep(0.02)
            self.assertFalse(running, "private GPG agent did not exit")


if __name__ == "__main__":
    unittest.main()
