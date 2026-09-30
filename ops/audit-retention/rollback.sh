#!/usr/bin/env bash
set -euo pipefail
[[ $# -ge 1 && $# -le 2 ]] || { echo 'usage: rollback.sh ROOT_ONLY_BACKUP [--apply]'; exit 1; }
backup_dir=$1
apply=false
[[ ${2:-} != --apply ]] || apply=true
[[ $# != 2 || ${2:-} == --apply ]] || { echo 'unknown argument'; exit 1; }
[[ $backup_dir == /opt/audit-log-retention-install-backups/* && -d $backup_dir && ! -L $backup_dir && $(stat -c %u "$backup_dir") == 0 && $(stat -c %a "$backup_dir") == 700 ]] || { echo 'root-only installation backup required'; exit 1; }
profile=$(cat "$backup_dir/profile.txt")
case "$profile" in
  logs)
    files=(retention.py audit-inkling-log-retention.service audit-inkling-log-retention.timer audit-cpa-debug-retention.service audit-cpa-debug-retention.timer)
    units=(audit-inkling-log-retention audit-cpa-debug-retention) ;;
  journal)
    files=(audit-journal-retention.service audit-journal-retention.timer 90-audit-retention.conf)
    units=(audit-journal-retention) ;;
  newapi)
    files=(retention.py audit-newapi-closed-log-retention.service audit-newapi-closed-log-retention.timer)
    units=(audit-newapi-closed-log-retention) ;;
  *) echo 'invalid backup profile'; exit 1 ;;
esac
target_for() {
  case "$1" in
    retention.py) echo "/usr/local/lib/audit-log-retention/$1" ;;
    90-audit-retention.conf) echo "/etc/systemd/journald.conf.d/$1" ;;
    *) echo "/etc/systemd/system/$1" ;;
  esac
}
for file in "${files[@]}"; do
  if [[ ! -f "$backup_dir/$file.previously-absent" ]]; then
    [[ -f "$backup_dir/$file" && ! -L "$backup_dir/$file" && $(stat -c %u "$backup_dir/$file") == 0 && $(stat -c %h "$backup_dir/$file") == 1 ]] || { echo 'unsafe or missing backup file'; exit 1; }
  fi
  printf 'restore installation state: %s\n' "$(target_for "$file")"
done
[[ $apply == true ]] || { echo 'preview only; logs removed earlier cannot be restored by this rollback'; exit 0; }
[[ $(id -u) == 0 ]] || { echo 'root required'; exit 1; }
for unit in "${units[@]}"; do
  systemctl disable --now "$unit.timer" || true
  systemctl stop "$unit.service" || true
done
for file in "${files[@]}"; do
  target=$(target_for "$file")
  if [[ -e $target || -L $target ]]; then
    [[ -f $target && ! -L $target && $(stat -c %u "$target") == 0 && $(stat -c %h "$target") == 1 ]] || { echo 'unsafe current target'; exit 1; }
  fi
  if [[ -f "$backup_dir/$file.previously-absent" ]]; then
    [[ ! -e $target ]] || rm -- "$target"
  else
    mode=644
    [[ $file != retention.py ]] || mode=600
    install -o root -g root -m "$mode" "$backup_dir/$file" "$target"
  fi
done
systemctl daemon-reload
for unit in "${units[@]}"; do
  [[ ! -f "$backup_dir/$unit.previously-enabled" ]] || systemctl enable "$unit.timer"
  [[ ! -f "$backup_dir/$unit.previously-active" ]] || systemctl start "$unit.timer"
done
echo 'installation rolled back; no business service or journald restart performed'
