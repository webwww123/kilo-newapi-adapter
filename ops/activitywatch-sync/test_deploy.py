from contextlib import closing
import json
import os
from pathlib import Path
import sqlite3
import stat
import tempfile
import unittest
from unittest.mock import patch

import deploy


class ActivityWatchDeployTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def test_atomic_code_replace_preserves_owner_mode_and_rejects_drift(self):
        if os.geteuid() != 0:
            self.skipTest('root-owned deployment directory checks require root')
        source = self.root / 'server.py'
        source.write_bytes(b'VALUE = 1\n')
        source.chmod(0o640)
        old = source.stat()
        deploy.atomic_source(source, b'VALUE = 2\n', old)
        self.assertEqual(source.read_bytes(), b'VALUE = 2\n')
        self.assertEqual(stat.S_IMODE(source.stat().st_mode), 0o640)
        self.assertEqual(source.stat().st_uid, old.st_uid)
        self.assertEqual(source.stat().st_gid, old.st_gid)
        with self.assertRaisesRegex(RuntimeError, 'source_metadata_drift'):
            deploy.atomic_source(source, b'VALUE = 3\n', old)
        self.assertEqual(source.read_bytes(), b'VALUE = 2\n')
        self.assertEqual(list(self.root.glob('.server-rollout-*')), [])

    def test_sqlite_backup_is_consistent_with_active_wal_and_keeps_new_data(self):
        source = self.root / 'synthetic.sqlite3'
        destination = self.root / 'backup.sqlite3'
        with closing(sqlite3.connect(source)) as writer:
            writer.execute('PRAGMA journal_mode=WAL')
            writer.execute('CREATE TABLE synthetic(value INTEGER)')
            writer.executemany('INSERT INTO synthetic VALUES(?)', [(i,) for i in range(100)])
            writer.commit()
            deploy.backup_sqlite(source, destination)
            writer.execute('INSERT INTO synthetic VALUES(100)')
            writer.commit()
            self.assertEqual(writer.execute('SELECT count(*) FROM synthetic').fetchone(), (101,))
        with closing(sqlite3.connect(destination)) as reader:
            self.assertEqual(reader.execute('PRAGMA quick_check').fetchall(), [('ok',)])
            self.assertEqual(reader.execute('SELECT count(*) FROM synthetic').fetchone(), (100,))
        self.assertEqual(stat.S_IMODE(destination.stat().st_mode), 0o600)

    def test_health_failure_restores_only_code_and_restarts_only_own_service(self):
        if os.geteuid() != 0:
            self.skipTest('root-owned deployment checks require root')
        source = self.root / 'server.py'
        old_code, new_code = b'VALUE = 1\n', b'VALUE = 2\n'
        source.write_bytes(old_code)
        source.chmod(0o644)
        unit = self.root / 'service'
        unit.write_bytes(b'synthetic service metadata')
        environment = self.root / 'config.env'
        environment.write_bytes(b'synthetic; not read by deployment helper')
        database = self.root / 'synthetic.sqlite3'
        with closing(sqlite3.connect(database)) as connection:
            connection.execute('CREATE TABLE synthetic(value INTEGER)')
        database.chmod(0o600)
        (self.root / 'source-commit.txt').write_text('a' * 40)
        baseline = {'source_sha256': deploy.digest(old_code), 'unit_sha256': deploy.digest(unit.read_bytes())}
        state = {'ActiveState': 'active', 'SubState': 'running', 'MainPID': '123'}
        calls = []
        with patch.multiple(deploy, SOURCE=source, UNIT=unit, DATABASE=database,
                            ENVIRONMENT=environment, BACKUP_ROOT=self.root / 'backups'), \
             patch.object(deploy.pwd, 'getpwnam', return_value=type('User', (), {'pw_uid': 0})()), \
             patch.object(deploy, 'service_state', return_value=state), \
             patch.object(deploy, 'command', side_effect=lambda args: calls.append(args) or b''), \
             patch.object(deploy, 'wait_healthy', side_effect=[False, True]), \
             patch('builtins.print'):
            with self.assertRaisesRegex(RuntimeError, 'code_rolled_back'):
                deploy.apply_repair(self.root, baseline, new_code)
        self.assertEqual(source.read_bytes(), old_code)
        self.assertEqual(calls, [['systemctl', 'restart', deploy.SERVICE]] * 2)
        self.assertTrue(any((self.root / 'backups').glob('*/activitywatch.sqlite3')))


if __name__ == '__main__':
    unittest.main()
