#!/usr/bin/env bash
set -euo pipefail
umask 077
apply=false
enable_timer=false
for argument in "$@"; do
  case "$argument" in
    --apply) apply=true ;;
    --enable-timer) enable_timer=true ;;
    *) echo 'usage: install.sh [--apply] [--enable-timer]' >&2; exit 2 ;;
  esac
done
source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
runtime_dir=/usr/local/lib/audit-source-backup
config_dir=/etc/audit-source-backup
files=(backup.py verify.py sources.json audit-source-backup.service audit-source-backup.timer)
if [[ "$apply" == false ]]; then
  printf 'preview: source backup program, inventory and units; timer_enabled=%s; no changes\n' "$enable_timer"
  exit 0
fi
[[ "$EUID" -eq 0 ]] || { echo 'root required' >&2; exit 1; }
safe_file() {
  [[ -f "$1" && ! -L "$1" && $(stat -c %u "$1") == 0 && $(stat -c %h "$1") == 1 ]] || { echo 'unsafe file' >&2; exit 1; }
}
safe_directory() {
  if [[ -e "$1" || -L "$1" ]]; then
    [[ -d "$1" && ! -L "$1" && $(stat -c %u "$1") == 0 ]] || { echo 'unsafe directory' >&2; exit 1; }
    [[ "$2" != 700 || $(stat -c %a "$1") == 700 ]] || { echo 'directory must be root-only' >&2; exit 1; }
  else
    install -d -o root -g root -m "$2" "$1"
  fi
}
safe_file "$source_dir/source-commit.txt"
source_commit=$(< "$source_dir/source-commit.txt")
[[ "$source_commit" =~ ^[0-9a-f]{40}$ && ${AUDIT_SOURCE_BACKUP_CONFIRM_COMMIT:-} == "$source_commit" ]] || { echo 'exact verified CI source commit confirmation required' >&2; exit 1; }
for tool in python3 git gpg gpgconf ssh scp; do command -v "$tool" >/dev/null; done
for filename in "${files[@]}"; do safe_file "$source_dir/$filename"; done
python3 -m py_compile "$source_dir/backup.py" "$source_dir/verify.py"
systemd-analyze verify "$source_dir/audit-source-backup.service" "$source_dir/audit-source-backup.timer"
for directory in /usr/local/lib /etc/systemd/system /opt; do safe_directory "$directory" 755; done
safe_directory /root/secrets 700
safe_directory /opt/audit-source-backup-install-backups 700
if [[ -e "$runtime_dir" || -L "$runtime_dir" ]]; then
  safe_directory "$runtime_dir" 700
  safe_file "$runtime_dir/.audit-source-owned"
  [[ $(< "$runtime_dir/.audit-source-owned") == audit-source/v1 ]] || { echo 'runtime ownership marker mismatch' >&2; exit 1; }
else
  safe_directory "$runtime_dir" 700
fi
safe_directory "$config_dir" 700
safe_directory /opt/audit-source-backups 700
rollback_dir=$(mktemp -d "/opt/audit-source-backup-install-backups/$(date -u +%Y%m%dT%H%M%SZ)-XXXXXXXX")
printf '%s\n' "$source_commit" > "$rollback_dir/ci-source-commit.txt"
systemctl is-enabled --quiet audit-source-backup.timer && touch "$rollback_dir/timer.previously-enabled" || true
systemctl is-active --quiet audit-source-backup.timer && touch "$rollback_dir/timer.previously-active" || true
target_for() {
  case "$1" in
    backup.py|verify.py) printf '%s/%s\n' "$runtime_dir" "$1" ;;
    sources.json) printf '%s/%s\n' "$config_dir" "$1" ;;
    *) printf '/etc/systemd/system/%s\n' "$1" ;;
  esac
}
# 所有既有文件先检查和保全，再写任何运行文件。
for filename in "${files[@]}" .audit-source-owned source-commit.txt; do
  if [[ "$filename" == .audit-source-owned || "$filename" == source-commit.txt ]]; then target="$runtime_dir/$filename"; else target=$(target_for "$filename"); fi
  if [[ -e "$target" || -L "$target" ]]; then
    safe_file "$target"
    cp --preserve=mode,timestamps -- "$target" "$rollback_dir/$filename"
    chmod 600 "$rollback_dir/$filename"
  else
    touch "$rollback_dir/$filename.previously-absent"
  fi
done
for filename in "${files[@]}"; do
  target=$(target_for "$filename")
  mode=600
  [[ "$filename" != *.service && "$filename" != *.timer ]] || mode=644
  temporary=$(mktemp "$(dirname -- "$target")/.audit-source-install-XXXXXXXX")
  install -o root -g root -m "$mode" "$source_dir/$filename" "$temporary"
  mv -T -- "$temporary" "$target"
done
printf '%s\n' audit-source/v1 > "$runtime_dir/.audit-source-owned"
chmod 600 "$runtime_dir/.audit-source-owned"
printf '%s\n' "$source_commit" > "$runtime_dir/source-commit.txt"
chmod 600 "$runtime_dir/source-commit.txt"
systemctl daemon-reload
if [[ "$enable_timer" == true ]]; then systemctl enable --now audit-source-backup.timer; fi
printf 'installed; root-only rollback_files=%s timer_enabled=%s\n' "$rollback_dir" "$enable_timer"
