# ActivityWatch sync TLS repair

This is the canonical source package for the existing personal ActivityWatch sync
service. It is not a NewAPI business service. The exact running `server.py` and its
coverage tests were copied only after AST/credential-pattern review found no embedded
live credential. No environment file, token, certificate private key, SQLite content
or rollback file is included in the package.

## Scope

The listening TLS socket defers its handshake. `SyncHttpServer` performs it in the
individual request thread, with a 15-second handshake timeout. A peer that connects
without sending a handshake can no longer block the shared accept thread. After
handshake, normal HTTPS requests use a 60-second **per network read idle** timeout;
this is not a total upload deadline. The existing default maximum body is 16 MiB;
authentication, routes, payload validation, SQLite transactions and synchronization
format are unchanged. No new configuration variable is required. Plain HTTP handling
is unchanged.

## Validation

```bash
cd ops/activitywatch-sync
sudo -n python3 -B -m unittest -v test_server.py test_tls.py test_deploy.py
```

Tests use only temporary synthetic databases, a short-lived localhost certificate
and a synthetic authentication token. They reproduce the original listener-handshake
blocking behavior, then verify that a stalled client leaves a second HTTPS `/health`
responsive; handshake stalls expire; a full 16-MiB valid authenticated upload succeeds;
a slow upload lasting longer than the handshake deadline succeeds; authentication and
the body-size rejection remain intact. Deployment tests verify consistent active-WAL
SQLite backups, code permissions/owner preservation, drift rejection and automatic
code rollback after health failure. Root is required for full deployment-fixture tests.

## Reviewed deployment

Only deploy the package produced by the approved CI workflow after checking its
checksum and source commit. CI must put the full verified 40-character commit in
`source-commit.txt` alongside `deploy.py`; the deployment caller verifies the package
digest before using it. The script itself does not fetch artifacts or trust a branch
name. Default behavior is preview:

```bash
python3 -B deploy.py
# After review and authorization, set ACTIVITYWATCH_SYNC_CONFIRM_COMMIT to the
# independently verified full CI commit and run on the indexed node 119:
python3 -B deploy.py --apply
```

Preflight refuses any unexpected production source or service-unit hash relative to
`baseline.json`, unsafe file metadata or changed service/config-file metadata. The
secret-bearing config file is checked **only by metadata**; its contents are never
read or copied. Apply requires root and a nonblocking rollout lock. It first stores
root-only code/unit backups and a consistent SQLite backup with a 30-second deadline
and integrity check under `/opt/activitywatch-sync-deploy-backups/`.

The new source atomically replaces only `/opt/activitywatch-sync/server.py`, retaining
its owner/group/mode. Only `activitywatch-sync.service` restarts. Verification checks
its public localhost HTTPS `/health`, including a simultaneous stalled TCP client,
and a new active PID. It does not need a production token or write a synthetic event
into the production database.

If verification fails and there is no concurrent change, the script atomically
restores the backed-up **code**, restarts the same service and verifies basic health.
It never automatically restores the database: doing so could erase valid new uploads.
If another operator changed source/unit/config metadata, rollback stops instead of
overwriting their work. The backup is preserved for reviewed recovery.

Current production bind/listener, service-unit, authentication/config files and TLS
files are untouched. No SSH, firewall, browser, NewAPI, CPA or Monitor operation is
performed. Deployment briefly interrupts only this personal sync receiver; clients
can retry using the existing idempotent batch protocol.

`node-index-entry.txt` supplies two exact proposed source/runtime entries for the
main agent to review and add through the normal index workflow; no live index is
edited by this package.
