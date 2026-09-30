# Audited log retention

This folder is an operations-only package. It does not build or replace a business
binary, restart CPA/NewAPI/Monitor, change SSH/firewalls, or touch business databases.
Install only an independently verified CI artifact. All commands preview by default.

## Approved targets

| Target | Schedule | Policy |
| --- | --- | --- |
| Node 50 `/var/log/kilo-newapi-adapter-inkling-migration/requests-YYYY-MM-DD.jsonl` | Hourly | Delete only closed logs with last-write age greater than 48 hours; keep today, yesterday, future dates and unmatched files. |
| Node 50 `/var/log/private/cpa-*-migration/debug.jsonl` | Every 15 minutes | If greater than 128 MiB, compress only the last 128 MiB, durably commit the archive, then truncate the same live inode. Keep three recent compressed tails per service. |
| Nodes 50 and 74 archived system journals | Hourly | `journalctl --vacuum-size=1G`; active journals are left intact. Total usage can exceed the target by the active files. |
| Node 119 `/root/new-api-official-latest/logs/oneapi-YYYYMMDDhhmmss.log` | Hourly, optional | Delete only closed logs with last-write age greater than 48 hours. Historical filename dates alone never make an active log eligible. |

The Inkling service is **still active**. The original audit's stopped-writing claim
was incorrect. The two actively written legacy NewAPI files are kept even though
one has a July filename.

The default Inkling/debug roots must be real root-owned mode-700 directories.
Debug child directories and files must match the verified live service UID **65534**,
with modes 700/600 respectively. Their live ownership/permissions are never changed.
Legacy NewAPI's existing root-owned directory/file modes 755/644 are validated
without changing them. Every selected log must be regular and single-link.
Symlinks in any directory component, hardlinks, unexpected modes/owners, metadata
replacement and inaccessible process metadata prevent the operation.

## Preview, CI and install

```bash
# Root is needed to exercise the real 0/65534 ownership checks in temporary fixtures.
sudo -n python3 -B ops/audit-retention/test_retention.py
bash -n ops/audit-retention/install.sh ops/audit-retention/rollback.sh
systemd-analyze verify ops/audit-retention/*.service ops/audit-retention/*.timer

bash ops/audit-retention/install.sh
bash ops/audit-retention/install.sh --profile=journal --node=74
bash ops/audit-retention/install.sh --newapi-logs
```

The CI artifact must contain these source files and a `source-commit.txt` file with
the verified full 40-character source commit. The deployment caller verifies the
artifact checksum and chooses the actual SSH target corresponding to `--node`.
`--node` is a declared deployment target, not an independent hostname detector.
After approval, set `AUDIT_RETENTION_CONFIRM_COMMIT` to exactly that verified commit
and use `--apply` on the appropriate target. Do not derive confirmation from an
unverified artifact.

Install `--profile=logs --node=50` for Inkling/debug, separately
`--profile=journal --node=50` or `--node=74` for journals, and `--newapi-logs` on 119.
The installer records root-only previous files and timer enable/active states under
`/opt/audit-log-retention-install-backups/`. Runtime code is root-only at
`/usr/local/lib/audit-log-retention/retention.py`. New timers immediately run their
corresponding one-shot checks after installation.

```bash
bash ops/audit-retention/rollback.sh /opt/audit-log-retention-install-backups/<backup>
# Only after verifying the preview:
bash ops/audit-retention/rollback.sh /opt/audit-log-retention-install-backups/<backup> --apply
```

Rollback restores the installed files and prior timer state. It does **not** restore
deleted/older debug logs; these are deliberately bounded by the approved policy.
The original main Kilo request-log timer is not touched by this package.

## Debug safeguards and cost

Before every truncate, the root, child directory, source path and opened file inode
are revalidated. Every observable writer must use `O_APPEND`, including an immediate
last check. Otherwise cleanup stops; this avoids non-append writers re-expanding a
truncated file from their previous offset. No business process is signalled or stopped.

Only a bounded tail is read, in 1-MiB chunks, with gzip level 1 and low CPU/I/O priority.
An archive must finish, flush, fsync and atomically receive its final name before any
source truncation. A source replacement/shrink, insufficient free space, unexpected
archive entry or compression/write failure leaves the source intact. The archive
directory and compressed files are root-owned modes 700/600. Archive counts and
compressed file sizes are checked before creating a new archive; stale closed partial
files older than one hour are cleaned. Open archive files are preserved and block
further rotations if they would exceed the budget. The maximum transient budget is
four roughly-128-MiB compressed files per service (three kept plus one new).

`copytruncate` has an unavoidable log-only loss window: entries appended while the
tail is compressed or between the final check and truncation can be lost. The output
reports observed append bytes during the copy; the final syscall race is not measured.
Older content beyond the last 128 MiB is intentionally discarded. Do not promise a
strict 128-MiB instantaneous live-file cap: the file can grow between 15-minute runs.

Process scans and result records contain only metadata. Log contents are compressed
directly to root-only disk and never printed.

## Journald configuration

`90-audit-retention.conf` proposes `SystemMaxUse=1G`, runtime 128 MiB and smaller
individual journal files. Both live nodes 50 and 74 report `CanReload=no` for
`systemd-journald.service`; installing this drop-in does not reload it.
The archive-vacuum timer works immediately **without restarting journald**. The
configuration cap becomes active on a subsequent separately authorized journald
restart/reboot. The installer and rollback never restart journald.

The separate node-74 dead-job repair script, when included in this package, is a
manual preview/`--apply` operation. It is not invoked by any retention timer.
