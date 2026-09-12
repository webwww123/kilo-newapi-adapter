import json
import unittest

from kilo_auto_telemetry_report import MODELS, PREFIX, summarize


class ReportTest(unittest.TestCase):
    def test_denominators_and_repairs(self):
        lines = [PREFIX + json.dumps({
            "version": 1, "outcome": "visible_complete",
            "reliability": {"used": True},
            "attempts": [{"model": "unknown"}, {"model": MODELS[2], "raw_retry": True}],
        }), PREFIX + json.dumps({
            "version": 1, "outcome": "http_error",
            "attempts": [{"model": "PRIVATE_MODEL_SECRET"}],
        })]
        result = summarize(lines)
        self.assertEqual(result["counts"]["middleware_requests"], 2)
        self.assertEqual(result["counts"]["recorded_upstream_attempts"], 3)
        self.assertEqual(result["counts"]["repaired_with_completion_evidence"], 1)
        self.assertEqual(result["attempt_model_distribution"][-1]["percent"], 66.67)
        self.assertEqual(result["completed_reply_model_distribution"][2]["percent"], 100)
        self.assertNotIn("PRIVATE_", json.dumps(result))

    def test_no_false_success_or_private_echo(self):
        lines = [PREFIX + json.dumps({"version": 1, "attempts": [], "outcome": x})
                 for x in ["cancelled", "unverified", "local_object_fallback", "PRIVATE_ERROR"]]
        lines += [PREFIX + "not json PRIVATE_SECRET", "unrelated private log"]
        result = summarize(lines)
        self.assertEqual(result["counts"]["malformed_records"], 1)
        self.assertTrue(all(x["count"] == 0 for x in result["completed_reply_model_distribution"]))
        self.assertTrue(all(x["percent"] is None for x in result["completed_reply_model_distribution"]))
        self.assertNotIn("PRIVATE_", json.dumps(result))


if __name__ == "__main__":
    unittest.main()
