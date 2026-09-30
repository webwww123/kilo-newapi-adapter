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
  printf 'apply requires: verified CI source-commit.txt and AUDIT_CORE_BACKUP_CONFIRM_COMMIT matching its exact 40-character commit\n'
  exit 0
fi
[[ $EUID -eq 0 ]] || { echo 'Root required' >&2; exit 1; }
safe_file() {
  [[ -f "$1" && ! -L "$1" && $(stat -c %u "$1") == 0 && $(stat -c %h "$1") == 1 && $(realpath -- "$1") == "$1" ]] || { echo 'Refuse unsafe or non-root file' >&2; exit 1; }
}
safe_directory() {
  if [[ -e "$1" || -L "$1" ]]; then
    [[ -d "$1" && ! -L "$1" && $(stat -c %u "$1") == 0 && $(stat -c %a "$1") == 700 && $(realpath -- "$1") == "$1" ]] || { echo 'Directory must be real, root-owned and private' >&2; exit 1; }
  else
    install -d -o root -g root -m 700 "$1"
  fi
}
safe_file "$script_dir/source-commit.txt"
source_commit=$(cat "$script_dir/source-commit.txt")
[[ $source_commit =~ ^[0-9a-f]{40}$ && ${AUDIT_CORE_BACKUP_CONFIRM_COMMIT:-} == "$source_commit" ]] || { echo 'Exact verified CI source commit confirmation required' >&2; exit 1; }
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
source_files=(common.py backup.py verify.py transfer.py restore_isolated.py targets.json source-commit.txt)
for filename in "${source_files[@]}"; do
  safe_file "$script_dir/$filename"
done
if [[ "$role" == backup ]]; then
  safe_file "$script_dir/audit-core-backup.service"
  safe_file "$script_dir/audit-core-backup.timer"
fi
# 只防止本任务安装并发，不使用生产数据库锁。
install_lock=/run/lock/audit-core-backup-install.lock
if [[ ! -e "$install_lock" && ! -L "$install_lock" ]]; then
  touch "$install_lock"
fi
safe_file "$install_lock"
exec 9<>"$install_lock"
flock -n 9 || { echo 'Another audit-core installation is running' >&2; exit 1; }
# 不追随系统目的目录的符号链接，也不覆盖其他用途的同名目录。
for dir in "$destination" /opt/backups/audit-core /opt/backup-archive/node119/audit-core; do
  [[ ! -L "$dir" ]] || { echo 'Refuse symlink destination' >&2; exit 1; }
done
if [[ -e "$destination" && ! -f "$destination/.audit-core-owned" ]]; then
  echo 'Destination has no ownership marker' >&2
  exit 1
fi
safe_directory /opt/audit-core-backup-install-backups
rollback_dir=$(mktemp -d "/opt/audit-core-backup-install-backups/$(date -u +%Y%m%dT%H%M%SZ)-${role}-XXXXXXXX")
safe_directory "$rollback_dir"
safe_directory "$destination"
printf '%s\n' "$source_commit" > "$rollback_dir/source-commit.txt"
printf '%s\n' "$role" > "$rollback_dir/role.txt"
if [[ "$role" == backup ]]; then
  systemctl is-enabled audit-core-backup.timer > "$rollback_dir/timer-enabled-state.txt" 2>/dev/null || true
  systemctl is-active audit-core-backup.timer > "$rollback_dir/timer-active-state.txt" 2>/dev/null || true
  systemctl is-enabled --quiet audit-core-backup.timer && touch "$rollback_dir/timer.previously-enabled" || true
  systemctl is-active --quiet audit-core-backup.timer && touch "$rollback_dir/timer.previously-active" || true
fi
if [[ -f "$destination/.audit-core-owned" ]]; then
  safe_file "$destination/.audit-core-owned"
  cp -p -- "$destination/.audit-core-owned" "$rollback_dir/ownership-marker.txt"
fi
for filename in "${source_files[@]}"; do
  if [[ -e "$destination/$filename" || -L "$destination/$filename" ]]; then
    safe_file "$destination/$filename"
    cp --preserve=mode,timestamps "$destination/$filename" "$rollback_dir/previous-$filename"
    chmod 600 "$rollback_dir/previous-$filename"
  else
    touch "$rollback_dir/${filename}.previously-absent"
  fi
  [[ ! -e "$destination/.${filename}.new" && ! -L "$destination/.${filename}.new" ]] || { echo 'Refuse existing staging file' >&2; exit 1; }
  install -o root -g root -m 600 "$script_dir/$filename" "$destination/.${filename}.new"
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
      safe_file "$existing"
      [[ -f "$rollback_dir/$filename" ]] || cp -p -- "$existing" "$rollback_dir/$filename"
    else
      touch "$rollback_dir/${filename}.absent"
    fi
    [[ ! -e "/etc/systemd/system/.${filename}.new" && ! -L "/etc/systemd/system/.${filename}.new" ]] || { echo 'Refuse existing unit staging file' >&2; exit 1; }
    install -o root -g root -m 600 "$script_dir/$filename" "/etc/systemd/system/.${filename}.new"
    mv -T -- "/etc/systemd/system/.${filename}.new" "$existing"
  done
  systemd-analyze verify /etc/systemd/system/audit-core-backup.service /etc/systemd/system/audit-core-backup.timer
  systemctl daemon-reload
  if "$enable_timer"; then
    systemctl enable --now audit-core-backup.timer
  fi
fi
printf 'installed: role=%s rollback_files=%s timer_enabled=%s\n' "$role" "$rollback_dir" "$enable_timer"
