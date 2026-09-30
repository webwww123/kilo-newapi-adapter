#!/usr/bin/env python3
"""74 两条已确认失效任务及孤儿证书续期配置的定向整改。"""
import argparse
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess

CERT_NAME = "grok45.23.238.28.237.sslip.io"
CERT = Path("/etc/letsencrypt/renewal") / (CERT_NAME + ".conf")
EXPECTED_CERT = "3b0927a92f1208c4d1240341b4d461a2da79c4bb7d6c3c9818a1c8d4bfd99240"
EXPECTED_CRON = "9037183c4ce7a81c214d8387a640bee9a7c50ab5f1ac88c1b1f168323c4d982e"
REMOVE = {
    "*/5 * * * * /root/newapi_backups/heal_charity_token_status.sh >> /root/newapi_backups/heal_status.log 2>&1",
    "0 19 * * * /opt/backups/backup_all.sh >> /opt/backups/backup.log 2>&1",
}
SCRIPTS = (Path("/opt/backups/backup_all.sh"), Path("/root/newapi_backups/heal_charity_token_status.sh"))
NGINX = ["docker", "exec", "1Panel-openresty-5P8O", "/usr/local/openresty/nginx/sbin/nginx", "-T"]


def digest(value):
    return hashlib.sha256(value).hexdigest()


def command(args, **kwargs):
    return subprocess.run(args, capture_output=True, timeout=15, check=True, **kwargs)


def amended_cron(before):
    lines = before.decode().splitlines(keepends=True)
    if any(sum(line.rstrip("\r\n") == target for line in lines) != 1 for target in REMOVE):
        raise RuntimeError("exact obsolete cron entries missing or duplicated")
    return "".join(line for line in lines if line.rstrip("\r\n") not in REMOVE).encode()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise RuntimeError("root required")
    os.umask(0o077)
    lock_fd = os.open("/run/lock/audit-dead-jobs.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lock_fd, "w") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        before = command(["crontab", "-l"]).stdout
        if digest(before) != EXPECTED_CRON:
            raise RuntimeError("crontab changed; review before applying")
        after = amended_cron(before)
        if any(path.exists() or path.is_symlink() for path in SCRIPTS):
            raise RuntimeError("a removed-script candidate is now present")
        st = CERT.lstat()
        if not stat.S_ISREG(st.st_mode) or st.st_nlink != 1 or st.st_uid != 0:
            raise RuntimeError("unsafe renewal configuration")
        cert_bytes = CERT.read_bytes()
        if digest(cert_bytes) != EXPECTED_CERT:
            raise RuntimeError("renewal configuration changed")
        expanded = command(NGINX).stdout.decode(errors="replace")
        if CERT_NAME in expanded:
            raise RuntimeError("certificate/domain has a live web consumer")
        active = subprocess.run(["systemctl", "is-active", "certbot.service"], capture_output=True, timeout=5)
        if active.stdout.strip() in {b"active", b"activating", b"reloading"}:
            raise RuntimeError("certbot running; defer cleanup")
        others = {str(p): digest(p.read_bytes()) for p in CERT.parent.glob("*.conf") if p != CERT}
        if not args.apply:
            print(json.dumps({"apply": False, "cron_entries": 2, "renewal": CERT_NAME, "other_renewals": len(others)}))
            return
        base = Path("/root/ops-audit-20260930-rollback")
        base.mkdir(mode=0o700, exist_ok=True)
        if base.is_symlink() or not base.is_dir() or base.stat().st_uid != 0 or base.stat().st_mode & 0o077:
            raise RuntimeError("unsafe rollback directory")
        backup = base / dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        backup.mkdir(mode=0o700)
        for name, value in {"root.crontab.before": before, "root.crontab.after": after, "renewal.before.conf": cert_bytes}.items():
            with (backup / name).open("xb") as output:
                output.write(value)
                output.flush()
                os.fsync(output.fileno())
        if command(["crontab", "-l"]).stdout != before:
            raise RuntimeError("crontab changed before apply")
        current = CERT.lstat()
        if (current.st_dev, current.st_ino) != (st.st_dev, st.st_ino) or digest(CERT.read_bytes()) != EXPECTED_CERT:
            raise RuntimeError("renewal identity changed before apply")
        command(["crontab", "-"], input=after)
        try:
            os.replace(CERT, backup / (CERT_NAME + ".conf.quarantined"))
        except Exception:
            if command(["crontab", "-l"]).stdout == after:
                command(["crontab", "-"], input=before)
            raise
        if command(["crontab", "-l"]).stdout != after or CERT.exists():
            raise RuntimeError("post-apply verification failed")
        if any(digest(Path(path).read_bytes()) != value for path, value in others.items()):
            raise RuntimeError("another renewal configuration changed")
        command(NGINX)
        result = {"apply": True, "cron_entries_removed": 2, "renewal_quarantined": CERT_NAME, "other_renewals_preserved": len(others), "rollback_dir": str(backup), "web_config_valid": True}
        (backup / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        print(json.dumps(result))


if __name__ == "__main__":
    main()
