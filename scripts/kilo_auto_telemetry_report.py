#!/usr/bin/env python3
"""Summarize body-free Auto journal records from stdin; no network or writes."""
import collections
import json
import sys

MODELS = (
    "dots-studio/dots-3-note-preview:free",
    "inclusionai/ling-3.0-flash-vl:free",
    "nex-agi/nex-n2.5-pro:free",
    "nvidia/nemotron-3-ultra-550b-a55b:free",
    "poolside/laguna-s-2.1:free",
)
OUTCOMES = {
    "cancelled", "http_error", "local_object_fallback", "json_validated",
    "visible_complete", "response_error", "terminal_observed", "unverified",
}
EVIDENCED = {"json_validated", "visible_complete", "terminal_observed"}
PREFIX = "kilo_auto_telemetry "


def distribution(counts):
    total = sum(counts.values())
    return [
        {"model": model, "count": counts[model],
         "percent": round(counts[model] * 100 / total, 2) if total else None}
        for model in (*MODELS, "unknown")
    ]


def summarize(lines):
    totals = collections.Counter()
    outcomes = collections.Counter()
    attempts = collections.Counter()
    final_replies = collections.Counter()
    for line in lines:
        if len(line) > 65536 or PREFIX not in line:
            continue
        try:
            row = json.loads(line.split(PREFIX, 1)[1])
            if not isinstance(row, dict) or row.get("version") != 1:
                continue
            legs = row["attempts"]
            if not isinstance(legs, list) or len(legs) > 8 or not all(isinstance(a, dict) for a in legs):
                raise ValueError("invalid attempts")
            outcome = row.get("outcome", "unverified")
            if not isinstance(outcome, str) or outcome not in OUTCOMES:
                outcome = "unverified"
            repair = row.get("reliability") or {}
            structured = row.get("json") or {}
            if not isinstance(repair, dict) or not isinstance(structured, dict):
                raise ValueError("invalid repair metadata")
        except (ValueError, KeyError, TypeError):
            totals["malformed_records"] += 1
            continue
        totals["middleware_requests"] += 1
        outcomes[outcome] += 1
        for leg in legs:
            model = leg.get("model")
            attempts[model if isinstance(model, str) and model in MODELS else "unknown"] += 1
        totals["recorded_upstream_attempts"] += len(legs)
        if row.get("attempts_not_recorded"):
            totals["requests_with_incomplete_attempt_capture"] += 1
        repaired = repair.get("used") is True or structured.get("repair_used") is True
        retried = any(a.get("raw_retry") is True for a in legs)
        totals["requests_with_repair"] += repaired
        totals["requests_with_raw_retry"] += retried
        if outcome in EVIDENCED:
            totals["repaired_with_completion_evidence"] += repaired
            totals["raw_retried_with_completion_evidence"] += retried
            model = legs[-1].get("model") if legs else None
            final_replies[model if isinstance(model, str) and model in MODELS else "unknown"] += 1
    return {
        "counts": dict(totals), "outcomes": dict(outcomes),
        "attempt_model_distribution": distribution(attempts),
        "completed_reply_model_distribution": distribution(final_replies),
        "notes": [
            "Percent denominators include unknown models; attempts include retries and repairs.",
            "Completed replies require adapter validation or visible output plus a protocol terminal.",
            "Protocol terminal evidence is not proof of tool execution or task correctness.",
            "Local object fallback is separate, not a validated upstream answer.",
            "Middleware requests are not deduplicated NewAPI logical requests or pure user traffic.",
            "Feed one bounded journal window once; probes are not automatically excluded.",
        ],
    }


if __name__ == "__main__":
    json.dump(summarize(sys.stdin), sys.stdout, ensure_ascii=False, indent=2)
    print()
