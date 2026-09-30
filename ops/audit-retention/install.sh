#!/usr/bin/env bash
set -euo pipefail
apply=false
profile=logs
node=50
for argument in "$@"; do
  case "$argument" in
    --apply) apply=true ;;
    --profile=logs|--profile=journal) profile=${argument#*=} ;;
    --newapi-logs) profile=newapi; node=119 ;;
    --node=50|--node=74|--node=119) node=${argument#*=} ;;
    *) echo 'usage: install.sh [--apply] [--profile=logs|journal] [--node=50|74] [--newapi-logs]'; exit 1 ;;
  esac
done
[[ $profile != logs || $node == 50 ]] || { echo 'logs profile is only approved for node 50'; exit 1; }
[[ $profile != journal || $node != 119 ]] || { echo 'journal profile is only approved for nodes 50 and 74'; exit 1; }
[[ $profile != newapi || $node == 119 ]] || { echo 'legacy NewAPI logs profile is only approved for node 119'; exit 1; }
source_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
runtime_dir=/usr/local/lib/audit-log-retention
units=()
if [[ $profile == logs ]]; then
  files=(retention.py audit-inkling-log-retention.service audit-inkling-log-retention.timer audit-cpa-debug-retention.service audit-cpa-debug-retention.timer)
  units=(audit-inkling-log-retention audit-cpa-debug-retention)
elif [[ $profile == journal ]]; then
  files=(audit-journal-retention.service audit-journal-retention.timer 90-audit-retention.conf)
  units=(audit-journal-retention)
else
  files=(retention.py audit-newapi-closed-log-retention.service audit-newapi-closed-log-retention.timer)
  units=(audit-newapi-closed-log-retention)
fi
target_for() {
  case "$1" in
    retention.py) echo "$runtime_dir/$1" ;;
    90-audit-retention.conf) echo "/etc/systemd/journald.conf.d/$1" ;;
    *) echo "/etc/systemd/system/$1" ;;
  esac
}
for file in "${files[@]}"; do
  [[ -f "$source_dir/$file" && ! -L "$source_dir/$file" ]] || { echo 'source file missing or linked'; exit 1; }
  printf '%s -> %s\n' "$file" "$(target_for "$file")"
done
if [[ $apply == false ]]; then
  echo "preview only; profile=$profile node=$node; no files or services changed"
  exit 0
fi
[[ $(id -u) == 0 ]] || { echo 'root required'; exit 1; }
[[ -f "$source_dir/source-commit.txt" && ! -L "$source_dir/source-commit.txt" ]] || { echo 'verified CI artifact source-commit.txt required'; exit 1; }
source_commit=$(cat "$source_dir/source-commit.txt")
[[ $source_commit =~ ^[0-9a-f]{40}$ && ${AUDIT_RETENTION_CONFIRM_COMMIT:-} == "$source_commit" ]] || { echo 'exact CI source commit confirmation required'; exit 1; }
umask 077
safe_directory() {
  if [[ -e $1 || -L $1 ]]; then
    [[ -d $1 && ! -L $1 && $(stat -c %u "$1") == 0 ]] || { echo 'unsafe target directory'; exit 1; }
  else
    install -d -m "$2" "$1"
  fi
}
safe_directory /opt/audit-log-retention-install-backups 700
[[ $(stat -c %a /opt/audit-log-retention-install-backups) == 700 ]] || { echo 'backup directory must be root-only'; exit 1; }
backup_dir=$(mktemp -d "/opt/audit-log-retention-install-backups/$(date -u +%Y%m%dT%H%M%SZ)-XXXXXXXX")
safe_directory "$runtime_dir" 700
[[ $(stat -c %a "$runtime_dir") == 700 ]] || { echo 'runtime directory must be root-only'; exit 1; }
if [[ $profile == journal ]]; then safe_directory /etc/systemd/journald.conf.d 755; fi
printf '%s\n' "$source_commit" > "$backup_dir/source-commit.txt"
printf '%s\n' "$profile" > "$backup_dir/profile.txt"
for unit in "${units[@]}"; do
  systemctl is-enabled --quiet "$unit.timer" && touch "$backup_dir/$unit.previously-enabled" || true
  systemctl is-active --quiet "$unit.timer" && touch "$backup_dir/$unit.previously-active" || true
done
for file in "${files[@]}"; do
  target=$(target_for "$file")
  if [[ -e $target || -L $target ]]; then
    [[ -f $target && ! -L $target && $(stat -c %u "$target") == 0 && $(stat -c %h "$target") == 1 ]] || { echo 'unsafe existing target'; exit 1; }
    cp --preserve=mode,timestamps "$target" "$backup_dir/$file"
    chmod 600 "$backup_dir/$file"
  else
    touch "$backup_dir/$file.previously-absent"
  fi
done
for file in "${files[@]}"; do
  target=$(target_for "$file")
  mode=644
  [[ $file != retention.py ]] || mode=600
  install -o root -g root -m "$mode" "$source_dir/$file" "$target"
done
systemctl daemon-reload
for unit in "${units[@]}"; do
  systemctl enable --now "$unit.timer"
  systemctl start "$unit.service"
done
printf 'installed; root-only rollback backup: %s\n' "$backup_dir"
if [[ $profile == journal ]]; then
  echo 'journald was not restarted; archived-journal vacuum is active now; config limits apply on the next separately authorized journald restart'
fi
