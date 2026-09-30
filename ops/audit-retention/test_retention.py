import datetime as dt
import gzip
import importlib.util
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location('audit_retention', Path(__file__).with_name('retention.py'))
retention = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(retention)


class RetentionTest(unittest.TestCase):
    def setUp(self):
        if os.geteuid() != 0:
            self.fail('Run tests as root: synthetic 65534 ownership checks must not be skipped')
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.now = dt.datetime(2026, 9, 30, 12).timestamp()

    def daily(self, name, age=72):
        path = self.root / name
        path.write_bytes(b'synthetic request metadata\n')
        path.chmod(0o600)
        os.utime(path, (self.now - age * 3600, self.now - age * 3600))
        return path

    def debug(self, name='cpa-example-migration', data=None):
        child = self.root / name
        child.mkdir(mode=0o700)
        os.chown(child, retention.DEBUG_UID, retention.DEBUG_UID)
        path = child / 'debug.jsonl'
        path.write_bytes(data if data is not None else b'synthetic-log-line\n' * 150)
        path.chmod(0o600)
        os.chown(path, retention.DEBUG_UID, retention.DEBUG_UID)
        return path

    def rotate(self, **kwargs):
        return retention.rotate_debug(self.root, now=self.now, limit=1024,
                                      process_scan=lambda: {}, **kwargs)

    def test_daily_preview_and_apply_keep_today_yesterday_recent_future_and_other(self):
        old = self.daily('requests-2026-09-27.jsonl')
        kept = [self.daily('requests-2026-09-29.jsonl', 96),
                self.daily('requests-2026-09-30.jsonl', 96),
                self.daily('requests-2026-10-01.jsonl', 96),
                self.daily('requests-2026-09-28.jsonl', 48),
                self.daily('requests-2026-02-30.jsonl'),
                self.daily('other.data')]
        preview = retention.cleanup_daily(self.root, now=self.now, process_scan=dict)
        self.assertEqual([row['name'] for row in preview['candidates']], [old.name])
        self.assertTrue(old.exists())
        result = retention.cleanup_daily(self.root, now=self.now, apply=True, process_scan=dict)
        self.assertEqual(len(result['deleted']), 1)
        self.assertFalse(old.exists())
        self.assertTrue(all(path.exists() for path in kept))

    def test_daily_symlink_hardlink_and_open_file_preserved(self):
        target = self.daily('protected.data')
        soft = self.root / 'requests-2026-09-26.jsonl'
        soft.symlink_to(target)
        hard = self.root / 'requests-2026-09-25.jsonl'
        os.link(target, hard)
        opened = self.daily('requests-2026-09-27.jsonl')
        info = opened.stat()
        rows = retention.cleanup_daily(self.root, now=self.now, apply=True,
                 process_scan=lambda: {(info.st_dev, info.st_ino): [os.O_RDONLY]})
        self.assertEqual(rows['deleted'], [])
        self.assertTrue(all(path.exists() for path in [target, soft, hard, opened]))

    def test_daily_drift_and_newly_opened_file_preserved(self):
        old = self.daily('requests-2026-09-27.jsonl')
        def change(path):
            path.write_bytes(b'changed synthetic metadata')
        result = retention.cleanup_daily(self.root, now=self.now, apply=True,
                  process_scan=dict, before_unlink=change)
        self.assertEqual(result['deleted'], [])
        self.assertTrue(old.exists())
        self.daily(old.name)
        active = {}
        def opened(path):
            info = path.stat()
            active[(info.st_dev, info.st_ino)] = [os.O_RDONLY]
        result = retention.cleanup_daily(self.root, now=self.now, apply=True,
                  process_scan=lambda: active, before_unlink=opened)
        self.assertEqual(result['deleted'], [])

    def test_inaccessible_process_scan_prevents_daily_deletion(self):
        old = self.daily('requests-2026-09-27.jsonl')
        def denied():
            raise PermissionError('synthetic access denial')
        with self.assertRaises(PermissionError):
            retention.cleanup_daily(self.root, now=self.now, apply=True, process_scan=denied)
        self.assertTrue(old.exists())

    def test_root_symlink_and_unsafe_permissions_refused(self):
        linked = self.root / 'linked'
        linked.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(OSError):
            retention.cleanup_daily(linked, now=self.now, apply=True, process_scan=dict)
        self.root.chmod(0o755)
        with self.assertRaises(RuntimeError):
            self.rotate(apply=True)

    def test_legacy_newapi_closed_logs_keep_historically_named_active_files(self):
        self.root.chmod(0o755)
        old = self.daily('oneapi-20260913202135.log')
        old.chmod(0o644)
        active = self.daily('oneapi-20260725073235.log')
        active.chmod(0o644)
        recent = self.daily('oneapi-20260925225657.log', 1)
        recent.chmod(0o644)
        other = self.daily('requests-2026-09-27.jsonl')
        info = active.stat()
        result = retention.cleanup_daily(self.root, now=self.now, apply=True, flavor='newapi',
               process_scan=lambda: {(info.st_dev, info.st_ino): [os.O_WRONLY | os.O_APPEND]})
        self.assertEqual([row['name'] for row in result['deleted']], [old.name])
        self.assertTrue(all(path.exists() for path in [active, recent, other]))

    def test_debug_preview_is_read_only_and_only_migration_debug_selected(self):
        live = self.debug()
        excluded = self.root / 'cpa-main'
        excluded.mkdir(mode=0o700)
        (excluded / 'debug.jsonl').write_bytes(b'keep' * 1000)
        before = live.read_bytes()
        result = self.rotate()
        self.assertEqual(len(result['files']), 1)
        self.assertEqual(result['files'][0]['action'], 'would_rotate')
        self.assertEqual(live.read_bytes(), before)
        self.assertFalse((self.root / 'cpa-debug-retained').exists())

    def test_debug_rotation_retains_bounded_tail_inode_permissions_and_three_archives(self):
        live = self.debug()
        inode = live.stat().st_ino
        expected = live.read_bytes()[-1024:]
        for attempt in range(4):
            if attempt:
                live.write_bytes(b'x' * 2500)
            result = retention.rotate_debug(self.root, now=self.now + attempt,
                     apply=True, limit=1024, process_scan=dict)
            self.assertEqual(result['files'][0]['action'], 'rotated')
            self.assertEqual(live.stat().st_ino, inode)
            self.assertEqual(live.stat().st_size, 0)
            self.assertEqual(live.stat().st_uid, retention.DEBUG_UID)
            archives = sorted((self.root / 'cpa-debug-retained' / live.parent.name).glob('*.gz'))
            self.assertEqual(len(archives), min(attempt + 1, 3))
            self.assertEqual(archives[-1].stat().st_uid, 0)
            self.assertEqual(archives[-1].stat().st_mode & 0o777, 0o600)
            self.assertEqual(gzip.decompress(archives[-1].read_bytes()), expected if not attempt else b'x' * 1024)

    def test_debug_append_writer_keeps_writing_after_rotation(self):
        live = self.debug()
        fd = os.open(live, os.O_WRONLY | os.O_APPEND)
        try:
            retention.rotate_debug(self.root, now=self.now, apply=True, limit=1024)
            os.write(fd, b'continued synthetic log\n')
            self.assertEqual(live.read_bytes(), b'continued synthetic log\n')
            self.assertEqual(live.stat().st_ino, os.fstat(fd).st_ino)
        finally:
            os.close(fd)

    def test_nonappend_writer_is_refused_without_truncation(self):
        live = self.debug()
        before = live.read_bytes()
        info = live.stat()
        with self.assertRaisesRegex(RuntimeError, 'non_append_writer'):
            retention.rotate_debug(self.root, now=self.now, apply=True, limit=1024,
               process_scan=lambda: {(info.st_dev, info.st_ino): [os.O_WRONLY]})
        self.assertEqual(live.read_bytes(), before)

    def test_debug_symlink_hardlink_owner_and_mode_refused(self):
        live = self.debug()
        before = live.read_bytes()
        protected = live.parent / 'protected.data'
        live.rename(protected)
        live.symlink_to(protected)
        with self.assertRaises(OSError):
            self.rotate(apply=True)
        live.unlink()
        os.link(protected, live)
        with self.assertRaises(RuntimeError):
            self.rotate(apply=True)
        live.unlink()
        protected.rename(live)
        live.chmod(0o644)
        with self.assertRaises(RuntimeError):
            self.rotate(apply=True)
        live.chmod(0o600)
        os.chown(live, 0, 0)
        with self.assertRaises(RuntimeError):
            self.rotate(apply=True)
        self.assertEqual(live.read_bytes(), before)

    def test_copy_failure_leaves_source_and_no_partial_archive(self):
        live = self.debug()
        before = live.read_bytes()
        def fail(source_fd, archive_fd, name, size, limit):
            fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600, dir_fd=archive_fd)
            os.write(fd, b'partial synthetic archive')
            os.close(fd)
            raise OSError('synthetic storage failure')
        with self.assertRaises(OSError):
            self.rotate(apply=True, copier=fail)
        self.assertEqual(live.read_bytes(), before)
        self.assertEqual(list((self.root / 'cpa-debug-retained' / live.parent.name).iterdir()), [])

    def test_low_free_space_refuses_truncation(self):
        live = self.debug()
        before = live.read_bytes()
        with patch.object(retention.os, 'fstatvfs', return_value=SimpleNamespace(f_bavail=0, f_frsize=4096)):
            with self.assertRaisesRegex(RuntimeError, 'insufficient_archive_space'):
                self.rotate(apply=True)
        self.assertEqual(live.read_bytes(), before)

    def test_source_swap_and_shrink_before_truncate_refused(self):
        live = self.debug()
        original = live.read_bytes()
        preserved = live.parent / 'preserved.data'
        def swap(path):
            path.rename(preserved)
            path.write_bytes(b'replacement synthetic data')
            path.chmod(0o600)
            os.chown(path, retention.DEBUG_UID, retention.DEBUG_UID)
        with self.assertRaisesRegex(RuntimeError, 'entry_changed'):
            self.rotate(apply=True, before_truncate=swap)
        self.assertEqual(preserved.read_bytes(), original)
        self.assertEqual(live.read_bytes(), b'replacement synthetic data')
        live.write_bytes(original)
        def shrink(path):
            path.write_bytes(b'synthetic shorter log')
        with self.assertRaisesRegex(RuntimeError, 'source_changed'):
            self.rotate(apply=True, before_truncate=shrink)
        self.assertEqual(live.read_bytes(), b'synthetic shorter log')

    def test_final_writer_recheck_refuses_new_nonappend_writer(self):
        live = self.debug()
        before = live.read_bytes()
        unsafe = {}
        def new_writer(path):
            info = path.stat()
            unsafe[(info.st_dev, info.st_ino)] = [os.O_WRONLY]
        with self.assertRaisesRegex(RuntimeError, 'non_append_writer'):
            retention.rotate_debug(self.root, now=self.now, apply=True, limit=1024,
                 process_scan=lambda: unsafe, before_truncate=new_writer)
        self.assertEqual(live.read_bytes(), before)

    def test_archive_directory_symlink_refused(self):
        live = self.debug()
        before = live.read_bytes()
        (self.root / 'cpa-debug-retained').symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(OSError):
            self.rotate(apply=True)
        self.assertEqual(live.read_bytes(), before)


if __name__ == '__main__':
    unittest.main()
