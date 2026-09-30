#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

apply=false
role=backup
enable_timer=false
for arg in "$@"; do
  case "$arg" in
    --apply) apply=true ;;
    --archive) role=archive ;;
    --enable-timer) enable_timer=true ;;
    *) echo 'Unknown argument' >&2; exit 2 ;;
  esac
done
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
destination=/usr/local/lib/audit-core-backup
if ! "$apply"; then
  printf 'preview: role=%s destination=%s enable_timer=%s\n' "$role" "$destination" "$enable_timer"
  exit 0
fi
[[ $EUID -eq 0 ]] || { echo 'Root required' >&2; exit 1; }
if [[ "$role" == archive && "$enable_timer" == true ]]; then
  echo 'Archive node must not run the source backup timer' >&2
  exit 1
fi
for tool in python3 ssh scp; do
  command -v "$tool" >/dev/null
done
if [[ "$role" == backup ]]; then
  command -v docker >/dev/null
fi
# 不追随系统目的目录的符号链接，也不覆盖其他用途的同名目录。
for dir in "$destination" /opt/backups/audit-core /opt/backup-archive/node119/audit-core; do
  [[ ! -L "$dir" ]] || { echo 'Refuse symlink destination' >&2; exit 1; }
done
if [[ -e "$destination" && ! -f "$destination/.audit-core-owned" ]]; then
  echo 'Destination has no ownership marker' >&2
  exit 1
fi
rollback_dir="/opt/audit-core-backup-install-backups/$(date -u +%Y%m%dT%H%M%SZ)-${role}"
install -d -m 700 "$rollback_dir" "$destination"
if [[ -f "$destination/.audit-core-owned" ]]; then
  for filename in common.py backup.py verify.py transfer.py targets.json; do
    [[ ! -f "$destination/$filename" ]] || cp -p -- "$destination/$filename" "$rollback_dir/$filename"
  done
fi
for filename in common.py backup.py verify.py transfer.py targets.json; do
  [[ -f "$script_dir/$filename" && ! -L "$script_dir/$filename" ]]
  install -m 600 "$script_dir/$filename" "$destination/.${filename}.new"
  mv -T -- "$destination/.${filename}.new" "$destination/$filename"
done
printf 'audit-core/v1\n' > "$destination/.audit-core-owned"
chmod 600 "$destination/.audit-core-owned"
if [[ "$role" == archive ]]; then
  python3 - "$destination" <<'PY'
import sys
sys.path.insert(0, sys.argv[1])
from common import REMOTE_ROOT, private_dir
private_dir(REMOTE_ROOT)
PY
else
  python3 - "$destination" <<'PY'
import sys
sys.path.insert(0, sys.argv[1])
from common import LOCAL_ROOT, private_dir
private_dir(LOCAL_ROOT)
PY
  for filename in audit-core-backup.service audit-core-backup.timer; do
    existing="/etc/systemd/system/$filename"
    [[ ! -L "$existing" ]] || { echo 'Refuse symlink unit' >&2; exit 1; }
    if [[ -e "$existing" ]]; then
      [[ -f "$rollback_dir/$filename" ]] || cp -p -- "$existing" "$rollback_dir/$filename"
    else
      touch "$rollback_dir/${filename}.absent"
    fi
    install -m 600 "$script_dir/$filename" "/etc/systemd/system/.${filename}.new"
    mv -T -- "/etc/systemd/system/.${filename}.new" "$existing"
  done
  systemd-analyze verify /etc/systemd/system/audit-core-backup.service /etc/systemd/system/audit-core-backup.timer
  systemctl daemon-reload
  if "$enable_timer"; then
    systemctl enable --now audit-core-backup.timer
  fi
fi
printf 'installed: role=%s rollback_files=%s timer_enabled=%s\n' "$role" "$rollback_dir" "$enable_timer"
