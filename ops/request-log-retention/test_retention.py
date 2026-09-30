import datetime as dt
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location('request_log_retention', Path(__file__).with_name('retention.py'))
retention = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(retention)


class RetentionTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.now = dt.datetime(2026, 9, 30, 12).timestamp()

    def file(self, name, age_hours):
        path = self.root / name
        path.write_bytes(b'test-only log\n')
        stamp = self.now - age_hours * 3600
        os.utime(path, (stamp, stamp))
        return path

    def test_preview_and_apply_preserve_current_recent_future_and_other_data(self):
        old = self.file('requests-2026-09-27.jsonl', 72)
        keep = [self.file('requests-2026-09-29.jsonl', 24),
                self.file('requests-2026-09-30.jsonl', 96),
                self.file('requests-2026-10-01.jsonl', 96),
                self.file('requests-2026-02-30.jsonl', 96),
                self.file('other.jsonl', 96)]
        result = retention.cleanup(self.root, now=self.now, apply=False, open_files=set)
        self.assertEqual([item['name'] for item in result['candidates']], [old.name])
        self.assertTrue(old.exists())
        result = retention.cleanup(self.root, now=self.now, apply=True, open_files=set)
        self.assertEqual(result['deleted_bytes'], len(b'test-only log\n'))
        self.assertFalse(old.exists())
        self.assertTrue(all(path.exists() for path in keep))

    def test_open_file_and_retention_boundary_are_preserved(self):
        opened = self.file('requests-2026-09-26.jsonl', 96)
        boundary = self.file('requests-2026-09-28.jsonl', 48)
        identity = retention.fingerprint(opened)[:2]
        result = retention.cleanup(self.root, now=self.now, apply=True, open_files=lambda: {identity})
        self.assertEqual(result['deleted'], [])
        self.assertTrue(opened.exists())
        self.assertTrue(boundary.exists())

    def test_symlinks_hardlinks_and_directories_are_preserved(self):
        target = self.file('protected.data', 96)
        link = self.root / 'requests-2026-09-26.jsonl'
        link.symlink_to(target)
        hard = self.root / 'requests-2026-09-27.jsonl'
        os.link(target, hard)
        directory = self.root / 'requests-2026-09-25.jsonl'
        directory.mkdir()
        result = retention.cleanup(self.root, now=self.now, apply=True, open_files=set)
        self.assertEqual(result['deleted'], [])
        self.assertTrue(target.exists())
        self.assertTrue(link.is_symlink())
        self.assertTrue(hard.exists())
        self.assertTrue(directory.exists())

    def test_modified_or_newly_opened_candidate_is_preserved(self):
        changed = self.file('requests-2026-09-26.jsonl', 96)
        opened = self.file('requests-2026-09-27.jsonl', 96)
        active = set()
        def change(path):
            if path == changed:
                path.write_bytes(b'new test-only data')
            else:
                active.add(retention.fingerprint(path)[:2])
        result = retention.cleanup(self.root, now=self.now, apply=True,
                                   open_files=lambda: active, before_unlink=change)
        self.assertEqual(result['deleted'], [])
        self.assertEqual(len(result['skipped']), 2)
        self.assertTrue(changed.exists())
        self.assertTrue(opened.exists())

    def test_inaccessible_process_prevents_deletion(self):
        old = self.file('requests-2026-09-27.jsonl', 96)
        def denied():
            raise PermissionError('test-only access denial')
        with self.assertRaises(PermissionError):
            retention.cleanup(self.root, now=self.now, apply=True, open_files=denied)
        self.assertTrue(old.exists())

    def test_symlink_root_is_refused(self):
        link = self.root / 'linked-root'
        link.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(RuntimeError):
            retention.cleanup(link, now=self.now, apply=True, open_files=set)


if __name__ == '__main__':
    unittest.main()
