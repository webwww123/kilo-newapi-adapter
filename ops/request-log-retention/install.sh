#!/usr/bin/env bash
set -euo pipefail
[[ ${1:-} == --apply ]] || { echo 'usage: install.sh --apply (verified CI artifact required)'; exit 1; }
[[ $(id -u) == 0 ]] || { echo 'root required'; exit 1; }
source_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
runtime_dir="/usr/local/lib/kilo-request-log-retention"
backup_dir="/opt/kilo-request-log-retention-install-backups/$(date -u +%Y%m%dT%H%M%SZ)"
install -d -m 700 "$backup_dir" "$runtime_dir"
for file in retention.py kilo-request-log-retention.service kilo-request-log-retention.timer; do
  if [[ "$file" == retention.py ]]; then target="$runtime_dir/$file"; else target="/etc/systemd/system/$file"; fi
  if [[ -e "$target" ]]; then cp -p "$target" "$backup_dir/$file"; else touch "$backup_dir/$file.previously-absent"; fi
done
if systemctl is-enabled --quiet kilo-request-log-retention.timer; then
  touch "$backup_dir/timer.previously-enabled"
fi
install -m 600 "$source_dir/retention.py" "$runtime_dir/retention.py"
install -m 644 "$source_dir/kilo-request-log-retention.service" "$source_dir/kilo-request-log-retention.timer" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now kilo-request-log-retention.timer
systemctl start kilo-request-log-retention.service
echo "installed; rollback files: $backup_dir"
