# Kilo request-log retention on node 50

The adapter creates daily `requests-YYYY-MM-DD.jsonl` files and opens/closes each
append. This hourly maintenance task removes only ordinary, single-link files
from `/var/log/kilo-newapi-adapter` whose names match that format and whose last
write was more than 48 hours ago. Current/future dated files, open files, symlinks,
hard links, recently changed files, and every other directory or filename stay.
Retention uses whole daily files, so a kept file can contain older entries.

Run `python3 -B retention.py` on the indexed service node for a metadata-only
preview. The systemd service uses `--apply`, a single-instance lock, low priority,
and no network access. It never reads request bodies or restarts the adapter/CPA.
If process-open-file inspection fails, cleanup stops and systemd reports failure.

Use the existing CircleCI pipeline with `build_requested=false`,
`request_log_retention_requested=true`, and the exact `expected_commit`.
The `request-log-retention-check` job validates preservation/deletion behavior,
installer syntax and systemd units, then publishes a commit-scoped tarball and
checksums. Verify every downloaded artifact and its source commit before running
`install.sh --apply` on node 50. Installation records the previous owned files.

To roll back this first installation, disable/stop only
`kilo-request-log-retention.timer`, remove the three newly installed owned files
and reload systemd. For an update, restore saved owned files and the previous
timer state instead. Removed historical logs cannot be restored by rollback.
Do not disable any unrelated timer or clear the active daily log.
