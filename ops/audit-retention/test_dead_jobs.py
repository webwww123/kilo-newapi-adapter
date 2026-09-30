import unittest
from repair_dead_jobs import REMOVE, amended_cron


class ObsoleteCronTest(unittest.TestCase):
    def test_exact_removal_preserves_other_lines_and_newlines(self):
        other = "# retained\r\n0 1 * * * /root/needed.sh\n"
        before = other + "\n".join(sorted(REMOVE)) + "\n"
        self.assertEqual(amended_cron(before.encode()), other.encode())

    def test_missing_and_duplicate_targets_abort(self):
        lines = "\n".join(sorted(REMOVE)) + "\n"
        with self.assertRaises(RuntimeError):
            amended_cron((lines + next(iter(REMOVE)) + "\n").encode())
        with self.assertRaises(RuntimeError):
            amended_cron((next(iter(REMOVE)) + "\n").encode())

    def test_similar_job_is_preserved(self):
        other = "0 19 * * * /opt/backups/backup_all.sh --alternate\n"
        before = other + "\n".join(sorted(REMOVE)) + "\n"
        self.assertEqual(amended_cron(before.encode()), other.encode())


if __name__ == "__main__":
    unittest.main()
