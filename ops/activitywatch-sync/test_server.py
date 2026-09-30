import json
from contextlib import closing
import tempfile
import unittest
from pathlib import Path

import server


class ActivityWatchSyncCoverageTests(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.db = server.Database(Path(self.tempdir.name) / "activitywatch.sqlite3")
        self.db.initialize()
        self.device_id = "device-test"
        with closing(self.db.connect()) as connection, connection:
            connection.execute(
                "INSERT INTO devices(device_id,hostname,source_version,first_seen,last_seen) VALUES(?,?,?,?,?)",
                (
                    self.device_id,
                    "host-test",
                    "test",
                    "2026-08-05T00:00:00.000Z",
                    "2026-08-08T00:00:00.000Z",
                ),
            )
            for bucket_id, bucket_type, client in [
                ("window-test", "currentwindow", "aw-watcher-window"),
                ("afk-test", "afkstatus", "aw-watcher-afk"),
            ]:
                connection.execute(
                    """
                    INSERT INTO buckets(
                        device_id,bucket_id,created,name,type,client,hostname,data_json,
                        last_updated,first_seen,last_seen
                    ) VALUES(?,?,?,?,?,?,?,?,?,?,?)
                    """,
                    (
                        self.device_id,
                        bucket_id,
                        "2026-08-05T00:00:00.000Z",
                        bucket_id,
                        bucket_type,
                        client,
                        "host-test",
                        "{}",
                        "2026-08-08T00:00:00.000Z",
                        "2026-08-05T00:00:00.000Z",
                        "2026-08-08T00:00:00.000Z",
                    ),
                )
                connection.execute(
                    """
                    INSERT INTO events(
                        device_id,bucket_id,event_id,timestamp,duration,data_json,
                        first_received,last_received
                    ) VALUES(?,?,?,?,?,?,?,?)
                    """,
                    (
                        self.device_id,
                        bucket_id,
                        "event-before",
                        "2026-08-05T00:00:00.000Z",
                        1,
                        json.dumps({"title": "must-not-leak"}),
                        "2026-08-08T00:00:00.000Z",
                        "2026-08-08T00:00:00.000Z",
                    ),
                )

    def tearDown(self):
        self.tempdir.cleanup()

    def add_summary(self, report_date, total_work_seconds=0):
        report = {
            "schema_version": 1,
            "date": report_date,
            "timezone": "Asia/Shanghai",
            "report_status": "complete",
            "is_day_complete": True,
            "total_work_seconds": total_work_seconds,
            "generated_at": f"{report_date}T23:59:59.000Z",
            "data_as_of": f"{report_date}T23:59:59.000Z",
            "segments": [],
            "categories": [],
            "apps": [],
            "hosts": [],
        }
        with closing(self.db.connect()) as connection, connection:
            connection.execute(
                """
                INSERT INTO daily_summaries(
                    device_id,report_date,generated_at,data_as_of,report_json,
                    first_received,last_received
                ) VALUES(?,?,?,?,?,?,?)
                """,
                (
                    self.device_id,
                    report_date,
                    report["generated_at"],
                    report["data_as_of"],
                    json.dumps(report),
                    "2026-08-08T00:00:00.000Z",
                    "2026-08-08T00:00:00.000Z",
                ),
            )

    def test_completed_day_after_source_start_has_complete_coverage(self):
        self.add_summary("2026-08-06", total_work_seconds=0)
        report = self.db.get_daily_report(
            device_id=self.device_id,
            report_date="2026-08-06",
        )
        coverage = report["source_coverage"]
        self.assertEqual(coverage["status"], "complete")
        self.assertTrue(coverage["window_bucket_exists"])
        self.assertTrue(coverage["afk_bucket_exists"])
        self.assertTrue(coverage["sufficient_to_confirm_zero"])
        self.assertNotIn("must-not-leak", str(coverage))

    def test_summary_before_source_start_is_missing_not_zero(self):
        self.add_summary("2026-08-02", total_work_seconds=0)
        report = self.db.get_daily_report(
            device_id=self.device_id,
            report_date="2026-08-02",
        )
        coverage = report["source_coverage"]
        self.assertEqual(coverage["status"], "missing")
        self.assertFalse(coverage["sufficient_to_confirm_zero"])

    def test_recent_reports_include_coverage_for_each_day(self):
        self.add_summary("2026-08-02", total_work_seconds=0)
        self.add_summary("2026-08-06", total_work_seconds=3600)
        reports = self.db.list_daily_reports(device_id=self.device_id, days=31)
        self.assertEqual(len(reports), 2)
        self.assertEqual(reports[0]["source_coverage"]["status"], "missing")
        self.assertEqual(reports[1]["source_coverage"]["status"], "complete")


if __name__ == "__main__":
    unittest.main()
