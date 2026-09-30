#!/usr/bin/env bash
set -euo pipefail
umask 077

BACKUP_DIR='/opt/backups/newapi-db'
RETENTION_DAYS='7'
DB_NAME='defaultdb'
R2_ENV_FILE='/opt/backups/.r2.env'

# 与其他数据库备份串行，避免同时增加线上数据库负担。
exec 9>/run/lock/audit-core-backup.lock
flock -w 300 9
mkdir -p "$BACKUP_DIR"
TS="$(date '+%F_%H%M%S')"
OUT_SQL_GZ="$BACKUP_DIR/${DB_NAME}_no_logs_${TS}.sql.gz"
OUT_META="$BACKUP_DIR/${DB_NAME}_no_logs_${TS}.meta.txt"

python3 -B /usr/local/lib/newapi-db-backup/dump.py --output "$OUT_SQL_GZ"

SHA256="$(sha256sum "$OUT_SQL_GZ" | awk '{print $1}')"
SIZE_HUMAN="$(du -h "$OUT_SQL_GZ" | awk '{print $1}')"
SIZE_BYTES="$(stat -c%s "$OUT_SQL_GZ")"
{
  echo "timestamp=$TS"
  echo "db=$DB_NAME"
  echo 'excluded_tables=logs'
  echo 'schema_only_tables=logs error_request_traces'
  echo "file=$(basename "$OUT_SQL_GZ")"
  echo "size_human=$SIZE_HUMAN"
  echo "size_bytes=$SIZE_BYTES"
  echo "sha256=$SHA256"
} > "$OUT_META"

# 沿用原有命名、7 日保留和 R2 上传位置。
find "$BACKUP_DIR" -type f \( -name "${DB_NAME}_no_logs_*.sql.gz" -o -name "${DB_NAME}_no_logs_*.meta.txt" \) -mtime +"$RETENTION_DAYS" -delete
ln -sfn "$OUT_SQL_GZ" "$BACKUP_DIR/latest.sql.gz"
ln -sfn "$OUT_META" "$BACKUP_DIR/latest.meta.txt"

UPLOAD_TO_R2=0
if [ -f "$R2_ENV_FILE" ]; then
  # shellcheck source=/dev/null
  . "$R2_ENV_FILE"
  if [ -n "${R2_ENDPOINT:-}" ] && [ -n "${R2_BUCKET:-}" ] && [ -n "${R2_ACCESS_KEY_ID:-}" ] && [ -n "${R2_SECRET_ACCESS_KEY:-}" ]; then
    export AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID"
    export AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"
    export AWS_DEFAULT_REGION="${R2_REGION:-auto}"
    export AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED
    export AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED
    R2_PREFIX="${R2_PREFIX:-newapi-db}"
    R2_PREFIX="${R2_PREFIX#/}"
    R2_PREFIX="${R2_PREFIX%/}"
    aws --endpoint-url "$R2_ENDPOINT" s3 cp "$OUT_SQL_GZ" "s3://${R2_BUCKET}/${R2_PREFIX}/$(basename "$OUT_SQL_GZ")"
    aws --endpoint-url "$R2_ENDPOINT" s3 cp "$OUT_META" "s3://${R2_BUCKET}/${R2_PREFIX}/$(basename "$OUT_META")"
    aws --endpoint-url "$R2_ENDPOINT" s3 cp "$OUT_SQL_GZ" "s3://${R2_BUCKET}/${R2_PREFIX}/latest.sql.gz"
    aws --endpoint-url "$R2_ENDPOINT" s3 cp "$OUT_META" "s3://${R2_BUCKET}/${R2_PREFIX}/latest.meta.txt"
    UPLOAD_TO_R2=1
  fi
fi
if [ "$UPLOAD_TO_R2" -eq 1 ]; then
  echo "[$(date '+%F %T')] OK: backup created + uploaded to R2 -> $OUT_SQL_GZ ($SIZE_HUMAN)"
else
  echo "[$(date '+%F %T')] OK: backup created (local only) -> $OUT_SQL_GZ ($SIZE_HUMAN)"
fi
