"""Private stdin/stdout tools-only counter for the offline runtime prototype.

No network, tool execution, body files, or customer conversation input. The
parent owns a hard process deadline; a stuck native encode is killed with its
worker. This is not installed in production.
"""
from __future__ import annotations

import argparse
import json
import resource
from pathlib import Path
import sys
import time

sys.path[:0] = [str(Path(__file__).resolve().parent),str(Path(__file__).resolve().parent.parent / "python")]
from step_token_count import OfficialStepCounter, REVISION

MAX_FRAME = 525312


def emit(value):
    sys.stdout.write(json.dumps(value, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--assets", required=True)
    args = parser.parse_args()
    init_wall = time.perf_counter_ns()
    init_cpu = time.process_time_ns()
    counter = OfficialStepCounter(args.assets)
    empty = {"messages": [{"role": "user", "content": ""}]}
    base = counter.count(empty)["official_template_tokens"]
    model = counter.tokenizer.model
    if not callable(getattr(model, "_clear_cache", None)):
        raise ValueError("native_cache_clear_required")
    model._clear_cache()
    counter.tokenizer.model = model
    memory = {}
    for line in Path("/proc/self/smaps_rollup").read_text().splitlines():
        fields = line.split()
        if len(fields) >= 2 and fields[0] in {"Rss:", "Pss:"}:
            memory[fields[0][:-1].lower() + "_kib"] = int(fields[1])
    emit({"ready": True, "revision": REVISION,
          "clear_text_cache": True,
          "init_wall_us": (time.perf_counter_ns() - init_wall) // 1000,
          "init_cpu_us": (time.process_time_ns() - init_cpu) // 1000,
          "rss_peak_kib": resource.getrusage(resource.RUSAGE_SELF).ru_maxrss,
          **memory})
    while True:
        frame = sys.stdin.buffer.readline(MAX_FRAME + 1)
        if not frame:
            return
        if len(frame) > MAX_FRAME or not frame.endswith(b"\n"):
            return
        request = None
        response = None
        job_cpu = time.process_time_ns()
        try:
            request = json.loads(frame)
            if not isinstance(request, dict) or set(request) != {"id", "tools"}:
                raise ValueError("request_guard")
            request_id = request["id"]
            if not isinstance(request_id, int) or isinstance(request_id, bool) or request_id <= 0:
                raise ValueError("request_guard")
            started = time.perf_counter_ns()
            counted = counter.count({**empty, "tools": request["tools"]})
            response = {"id": request_id, "tokens": counted["official_template_tokens"] - base,
                        "compute_us": (time.perf_counter_ns() - started) // 1000}
        except (ValueError, TypeError, KeyError):
            # Never return exception text, schema names, or request fragments.
            request_id = request.get("id", 0) if isinstance(request, dict) else 0
            response = {"id": request_id, "error": "schema_guard"}
        finally:
            # HF's native BPE cache can otherwise retain schema text across
            # jobs even though the parent caches only an integer. Keep useful
            # within-job caching, clear it before the next request can start.
            model = counter.tokenizer.model
            model._clear_cache()
            counter.tokenizer.model = model
            frame = None
            request = None
        response["cpu_us"] = (time.process_time_ns() - job_cpu) // 1000
        emit(response)


if __name__ == "__main__":
    main()
