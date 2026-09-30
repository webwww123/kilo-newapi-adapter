#!/usr/bin/env python3
"""使用 Python 标准库实现的 ActivityWatch 增量同步接收服务。"""

from __future__ import annotations

import hmac
import json
import logging
import os
import signal
import sqlite3
import ssl
import threading
from contextlib import closing
from dataclasses import dataclass
from datetime import date, datetime, time, timedelta, timezone
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlparse
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError


VERSION = "0.3.0"
LOGGER = logging.getLogger("activitywatch-sync")
BEIJING = ZoneInfo("Asia/Shanghai")


class ApiError(Exception):
    def __init__(self, status: int, code: str, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.code = code
        self.message = message


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def normalize_timestamp(value: Any, field: str, *, optional: bool = False) -> str | None:
    if value is None and optional:
        return None
    if not isinstance(value, str) or not value or len(value) > 64:
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{field} 必须是 ISO 时间字符串")
    candidate = value[:-1] + "+00:00" if value.endswith("Z") else value
    try:
        parsed = datetime.fromisoformat(candidate)
    except ValueError as exc:
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{field} 不是有效的 ISO 时间") from exc
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def required_text(value: Any, field: str, max_length: int) -> str:
    if not isinstance(value, str) or not value.strip() or len(value) > max_length:
        raise ApiError(
            HTTPStatus.BAD_REQUEST,
            "invalid_payload",
            f"{field} 必须是 1 到 {max_length} 个字符的字符串",
        )
    return value.strip()


def optional_text(value: Any, field: str, max_length: int) -> str | None:
    if value is None:
        return None
    if not isinstance(value, str) or len(value) > max_length:
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{field} 必须是字符串或 null")
    return value


def json_object(value: Any, field: str) -> dict[str, Any]:
    if value is None:
        return {}
    if not isinstance(value, dict):
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{field} 必须是 JSON 对象")
    return value


def compact_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), sort_keys=True)


def normalize_date(value: Any, field: str) -> str:
    if not isinstance(value, str) or len(value) != 10:
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{field} 必须是 YYYY-MM-DD 日期")
    try:
        return date.fromisoformat(value).isoformat()
    except ValueError as exc:
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{field} 不是有效日期") from exc


def validate_daily_summary(value: Any, field: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{field} 必须是 JSON 对象")
    summary = dict(value)
    if summary.get("schema_version") != 1:
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{field}.schema_version 必须是 1")
    summary["date"] = normalize_date(summary.get("date"), f"{field}.date")
    summary["generated_at"] = normalize_timestamp(summary.get("generated_at"), f"{field}.generated_at")
    summary["data_as_of"] = normalize_timestamp(summary.get("data_as_of"), f"{field}.data_as_of")
    report_status = optional_text(summary.get("report_status"), f"{field}.report_status", 32)
    if report_status not in {"in_progress", "complete"}:
        raise ApiError(
            HTTPStatus.BAD_REQUEST,
            "invalid_payload",
            f"{field}.report_status 必须是 in_progress 或 complete",
        )
    if not isinstance(summary.get("is_day_complete"), bool):
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{field}.is_day_complete 必须是布尔值")
    for key in ("segments", "categories", "apps", "hosts"):
        if not isinstance(summary.get(key, []), list):
            raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{field}.{key} 必须是数组")
    encoded = compact_json(summary).encode("utf-8")
    if len(encoded) > 512 * 1024:
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{field} 超过 512 KiB")
    return summary


def validate_sync_payload(payload: Any) -> dict[str, Any]:
    if not isinstance(payload, dict):
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", "请求体必须是 JSON 对象")
    if payload.get("schema_version") != 1:
        raise ApiError(HTTPStatus.BAD_REQUEST, "unsupported_schema", "仅支持 schema_version=1")

    batch_id = required_text(payload.get("batch_id"), "batch_id", 128)
    sent_at = normalize_timestamp(payload.get("sent_at"), "sent_at")
    raw_device = payload.get("device")
    if not isinstance(raw_device, dict):
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", "device 必须是 JSON 对象")
    device = {
        "id": required_text(raw_device.get("id"), "device.id", 255),
        "hostname": required_text(raw_device.get("hostname"), "device.hostname", 255),
        "source_version": optional_text(raw_device.get("source_version"), "device.source_version", 128),
    }

    raw_buckets = payload.get("buckets")
    if not isinstance(raw_buckets, list) or len(raw_buckets) > 100:
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", "buckets 必须是最多包含 100 项的数组")

    total_events = 0
    buckets: list[dict[str, Any]] = []
    for bucket_index, raw_bucket in enumerate(raw_buckets):
        prefix = f"buckets[{bucket_index}]"
        if not isinstance(raw_bucket, dict):
            raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{prefix} 必须是 JSON 对象")
        raw_events = raw_bucket.get("events")
        if not isinstance(raw_events, list) or len(raw_events) > 5000:
            raise ApiError(
                HTTPStatus.BAD_REQUEST,
                "invalid_payload",
                f"{prefix}.events 必须是最多包含 5000 项的数组",
            )
        total_events += len(raw_events)
        if total_events > 20_000:
            raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", "单次请求最多包含 20000 个事件")

        events: list[dict[str, Any]] = []
        for event_index, raw_event in enumerate(raw_events):
            event_prefix = f"{prefix}.events[{event_index}]"
            if not isinstance(raw_event, dict):
                raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{event_prefix} 必须是 JSON 对象")
            event_id = raw_event.get("id")
            if isinstance(event_id, bool) or not isinstance(event_id, (int, str)):
                raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{event_prefix}.id 必须是整数或字符串")
            event_id_text = str(event_id)
            if not event_id_text or len(event_id_text) > 128:
                raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{event_prefix}.id 长度无效")
            duration = raw_event.get("duration", 0)
            if isinstance(duration, bool) or not isinstance(duration, (int, float)):
                raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{event_prefix}.duration 必须是数字")
            duration_float = float(duration)
            if duration_float < 0 or duration_float > 366 * 24 * 60 * 60:
                raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", f"{event_prefix}.duration 超出范围")
            events.append(
                {
                    "id": event_id_text,
                    "timestamp": normalize_timestamp(raw_event.get("timestamp"), f"{event_prefix}.timestamp"),
                    "duration": duration_float,
                    "data": json_object(raw_event.get("data"), f"{event_prefix}.data"),
                }
            )

        buckets.append(
            {
                "id": required_text(raw_bucket.get("id"), f"{prefix}.id", 512),
                "created": normalize_timestamp(raw_bucket.get("created"), f"{prefix}.created", optional=True),
                "name": optional_text(raw_bucket.get("name"), f"{prefix}.name", 512),
                "type": optional_text(raw_bucket.get("type"), f"{prefix}.type", 255),
                "client": optional_text(raw_bucket.get("client"), f"{prefix}.client", 255),
                "hostname": optional_text(raw_bucket.get("hostname"), f"{prefix}.hostname", 255),
                "last_updated": normalize_timestamp(
                    raw_bucket.get("last_updated"), f"{prefix}.last_updated", optional=True
                ),
                "data": json_object(raw_bucket.get("data"), f"{prefix}.data"),
                "events": events,
            }
        )

    raw_summaries = payload.get("summaries", [])
    if not isinstance(raw_summaries, list) or len(raw_summaries) > 31:
        raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_payload", "summaries 必须是最多包含 31 项的数组")
    summaries = [
        validate_daily_summary(raw_summary, f"summaries[{summary_index}]")
        for summary_index, raw_summary in enumerate(raw_summaries)
    ]

    return {
        "schema_version": 1,
        "batch_id": batch_id,
        "sent_at": sent_at,
        "device": device,
        "buckets": buckets,
        "summaries": summaries,
    }


@dataclass(frozen=True)
class Config:
    bind_host: str
    port: int
    token: str
    db_path: Path
    cert_path: Path | None
    key_path: Path | None
    max_body_bytes: int

    @classmethod
    def from_environment(cls) -> "Config":
        token = os.environ.get("AWSYNC_TOKEN", "")
        if len(token) < 32:
            raise RuntimeError("AWSYNC_TOKEN 必须至少包含 32 个字符")
        port = int(os.environ.get("AWSYNC_PORT", "18765"))
        if port < 1 or port > 65535:
            raise RuntimeError("AWSYNC_PORT 超出范围")
        cert_value = os.environ.get("AWSYNC_CERT", "").strip()
        key_value = os.environ.get("AWSYNC_KEY", "").strip()
        if bool(cert_value) != bool(key_value):
            raise RuntimeError("AWSYNC_CERT 和 AWSYNC_KEY 必须同时设置")
        return cls(
            bind_host=os.environ.get("AWSYNC_BIND_HOST", "127.0.0.1"),
            port=port,
            token=token,
            db_path=Path(os.environ.get("AWSYNC_DB", "/opt/activitywatch-sync/data/activitywatch.sqlite3")),
            cert_path=Path(cert_value) if cert_value else None,
            key_path=Path(key_value) if key_value else None,
            max_body_bytes=int(os.environ.get("AWSYNC_MAX_BODY_BYTES", str(16 * 1024 * 1024))),
        )


class Database:
    def __init__(self, path: Path) -> None:
        self.path = path

    def connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(self.path, timeout=30)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys=ON")
        connection.execute("PRAGMA busy_timeout=5000")
        return connection

    def initialize(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        with closing(self.connect()) as connection, connection:
            connection.execute("PRAGMA journal_mode=WAL")
            connection.execute("PRAGMA synchronous=NORMAL")
            connection.executescript(
                """
                CREATE TABLE IF NOT EXISTS devices (
                    device_id TEXT PRIMARY KEY,
                    hostname TEXT NOT NULL,
                    source_version TEXT,
                    first_seen TEXT NOT NULL,
                    last_seen TEXT NOT NULL
                );

                CREATE TABLE IF NOT EXISTS buckets (
                    device_id TEXT NOT NULL,
                    bucket_id TEXT NOT NULL,
                    created TEXT,
                    name TEXT,
                    type TEXT,
                    client TEXT,
                    hostname TEXT,
                    data_json TEXT NOT NULL,
                    last_updated TEXT,
                    first_seen TEXT NOT NULL,
                    last_seen TEXT NOT NULL,
                    PRIMARY KEY (device_id, bucket_id),
                    FOREIGN KEY (device_id) REFERENCES devices(device_id) ON DELETE CASCADE
                );

                CREATE TABLE IF NOT EXISTS events (
                    device_id TEXT NOT NULL,
                    bucket_id TEXT NOT NULL,
                    event_id TEXT NOT NULL,
                    timestamp TEXT NOT NULL,
                    duration REAL NOT NULL,
                    data_json TEXT NOT NULL,
                    first_received TEXT NOT NULL,
                    last_received TEXT NOT NULL,
                    PRIMARY KEY (device_id, bucket_id, event_id),
                    FOREIGN KEY (device_id, bucket_id)
                        REFERENCES buckets(device_id, bucket_id) ON DELETE CASCADE
                );

                CREATE INDEX IF NOT EXISTS idx_events_timestamp
                    ON events(device_id, timestamp);
                CREATE INDEX IF NOT EXISTS idx_events_bucket_timestamp
                    ON events(device_id, bucket_id, timestamp);

                CREATE TABLE IF NOT EXISTS sync_batches (
                    batch_id TEXT PRIMARY KEY,
                    device_id TEXT NOT NULL,
                    sent_at TEXT NOT NULL,
                    received_at TEXT NOT NULL,
                    event_count INTEGER NOT NULL,
                    FOREIGN KEY (device_id) REFERENCES devices(device_id) ON DELETE CASCADE
                );

                CREATE TABLE IF NOT EXISTS daily_summaries (
                    device_id TEXT NOT NULL,
                    report_date TEXT NOT NULL,
                    generated_at TEXT NOT NULL,
                    data_as_of TEXT NOT NULL,
                    report_json TEXT NOT NULL,
                    first_received TEXT NOT NULL,
                    last_received TEXT NOT NULL,
                    PRIMARY KEY (device_id, report_date),
                    FOREIGN KEY (device_id) REFERENCES devices(device_id) ON DELETE CASCADE
                );

                CREATE INDEX IF NOT EXISTS idx_daily_summaries_date
                    ON daily_summaries(device_id, report_date DESC);
                """
            )

    def store_sync(self, payload: dict[str, Any]) -> dict[str, int]:
        now = utc_now()
        device = payload["device"]
        inserted = 0
        updated = 0
        unchanged = 0
        summaries_inserted = 0
        summaries_updated = 0
        summaries_unchanged = 0
        with closing(self.connect()) as connection, connection:
            connection.execute("BEGIN IMMEDIATE")
            connection.execute(
                """
                INSERT INTO devices(device_id, hostname, source_version, first_seen, last_seen)
                VALUES (?, ?, ?, ?, ?)
                ON CONFLICT(device_id) DO UPDATE SET
                    hostname=excluded.hostname,
                    source_version=excluded.source_version,
                    last_seen=excluded.last_seen
                """,
                (device["id"], device["hostname"], device["source_version"], now, now),
            )

            event_count = 0
            for bucket in payload["buckets"]:
                connection.execute(
                    """
                    INSERT INTO buckets(
                        device_id, bucket_id, created, name, type, client, hostname,
                        data_json, last_updated, first_seen, last_seen
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    ON CONFLICT(device_id, bucket_id) DO UPDATE SET
                        created=excluded.created,
                        name=excluded.name,
                        type=excluded.type,
                        client=excluded.client,
                        hostname=excluded.hostname,
                        data_json=excluded.data_json,
                        last_updated=excluded.last_updated,
                        last_seen=excluded.last_seen
                    """,
                    (
                        device["id"],
                        bucket["id"],
                        bucket["created"],
                        bucket["name"],
                        bucket["type"],
                        bucket["client"],
                        bucket["hostname"],
                        compact_json(bucket["data"]),
                        bucket["last_updated"],
                        now,
                        now,
                    ),
                )
                for event in bucket["events"]:
                    event_count += 1
                    data_json = compact_json(event["data"])
                    existing = connection.execute(
                        """
                        SELECT timestamp, duration, data_json
                        FROM events
                        WHERE device_id=? AND bucket_id=? AND event_id=?
                        """,
                        (device["id"], bucket["id"], event["id"]),
                    ).fetchone()
                    if existing is None:
                        connection.execute(
                            """
                            INSERT INTO events(
                                device_id, bucket_id, event_id, timestamp, duration,
                                data_json, first_received, last_received
                            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                            """,
                            (
                                device["id"],
                                bucket["id"],
                                event["id"],
                                event["timestamp"],
                                event["duration"],
                                data_json,
                                now,
                                now,
                            ),
                        )
                        inserted += 1
                    elif (
                        existing["timestamp"] != event["timestamp"]
                        or float(existing["duration"]) != event["duration"]
                        or existing["data_json"] != data_json
                    ):
                        connection.execute(
                            """
                            UPDATE events
                            SET timestamp=?, duration=?, data_json=?, last_received=?
                            WHERE device_id=? AND bucket_id=? AND event_id=?
                            """,
                            (
                                event["timestamp"],
                                event["duration"],
                                data_json,
                                now,
                                device["id"],
                                bucket["id"],
                                event["id"],
                            ),
                        )
                        updated += 1
                    else:
                        unchanged += 1

            for summary in payload["summaries"]:
                report_json = compact_json(summary)
                existing_summary = connection.execute(
                    """
                    SELECT generated_at, data_as_of, report_json
                    FROM daily_summaries
                    WHERE device_id=? AND report_date=?
                    """,
                    (device["id"], summary["date"]),
                ).fetchone()
                if existing_summary is None:
                    connection.execute(
                        """
                        INSERT INTO daily_summaries(
                            device_id, report_date, generated_at, data_as_of,
                            report_json, first_received, last_received
                        ) VALUES (?, ?, ?, ?, ?, ?, ?)
                        """,
                        (
                            device["id"],
                            summary["date"],
                            summary["generated_at"],
                            summary["data_as_of"],
                            report_json,
                            now,
                            now,
                        ),
                    )
                    summaries_inserted += 1
                elif (
                    existing_summary["generated_at"] != summary["generated_at"]
                    or existing_summary["data_as_of"] != summary["data_as_of"]
                    or existing_summary["report_json"] != report_json
                ):
                    connection.execute(
                        """
                        UPDATE daily_summaries
                        SET generated_at=?, data_as_of=?, report_json=?, last_received=?
                        WHERE device_id=? AND report_date=?
                        """,
                        (
                            summary["generated_at"],
                            summary["data_as_of"],
                            report_json,
                            now,
                            device["id"],
                            summary["date"],
                        ),
                    )
                    summaries_updated += 1
                else:
                    summaries_unchanged += 1

            connection.execute(
                """
                INSERT INTO sync_batches(batch_id, device_id, sent_at, received_at, event_count)
                VALUES (?, ?, ?, ?, ?)
                ON CONFLICT(batch_id) DO NOTHING
                """,
                (payload["batch_id"], device["id"], payload["sent_at"], now, event_count),
            )
        return {
            "inserted": inserted,
            "updated": updated,
            "unchanged": unchanged,
            "summaries_inserted": summaries_inserted,
            "summaries_updated": summaries_updated,
            "summaries_unchanged": summaries_unchanged,
        }

    def status(self) -> dict[str, Any]:
        with closing(self.connect()) as connection:
            counts = connection.execute(
                """
                SELECT
                    (SELECT COUNT(*) FROM devices) AS devices,
                    (SELECT COUNT(*) FROM buckets) AS buckets,
                    (SELECT COUNT(*) FROM events) AS events,
                    (SELECT COUNT(*) FROM sync_batches) AS batches,
                    (SELECT COUNT(*) FROM daily_summaries) AS summaries
                """
            ).fetchone()
            devices = connection.execute(
                """
                SELECT d.device_id, d.hostname, d.source_version, d.first_seen, d.last_seen,
                       COUNT(DISTINCT b.bucket_id) AS bucket_count,
                       COUNT(e.event_id) AS event_count,
                       MAX(e.timestamp) AS latest_event,
                       (SELECT MAX(s.report_date) FROM daily_summaries s
                        WHERE s.device_id=d.device_id) AS latest_summary_date,
                       (SELECT MAX(s.data_as_of) FROM daily_summaries s
                        WHERE s.device_id=d.device_id) AS latest_summary_as_of
                FROM devices d
                LEFT JOIN buckets b ON b.device_id=d.device_id
                LEFT JOIN events e ON e.device_id=b.device_id AND e.bucket_id=b.bucket_id
                GROUP BY d.device_id
                ORDER BY d.last_seen DESC
                """
            ).fetchall()
        return {
            "counts": dict(counts),
            "devices": [dict(row) for row in devices],
            "server_time": utc_now(),
        }

    def list_buckets(self, device_id: str) -> list[dict[str, Any]]:
        with closing(self.connect()) as connection:
            rows = connection.execute(
                """
                SELECT b.device_id, b.bucket_id, b.created, b.name, b.type, b.client,
                       b.hostname, b.data_json, b.last_updated, b.last_seen,
                       COUNT(e.event_id) AS event_count,
                       MAX(e.timestamp) AS latest_event
                FROM buckets b
                LEFT JOIN events e ON e.device_id=b.device_id AND e.bucket_id=b.bucket_id
                WHERE b.device_id=?
                GROUP BY b.device_id, b.bucket_id
                ORDER BY b.bucket_id
                """,
                (device_id,),
            ).fetchall()
        result = []
        for row in rows:
            item = dict(row)
            item["data"] = json.loads(item.pop("data_json"))
            result.append(item)
        return result

    def list_events(
        self,
        *,
        device_id: str,
        bucket_id: str | None,
        start: str | None,
        end: str | None,
        limit: int,
    ) -> list[dict[str, Any]]:
        conditions = ["device_id=?"]
        values: list[Any] = [device_id]
        if bucket_id:
            conditions.append("bucket_id=?")
            values.append(bucket_id)
        if start:
            conditions.append("timestamp>=?")
            values.append(start)
        if end:
            conditions.append("timestamp<?")
            values.append(end)
        values.append(limit)
        query = f"""
            SELECT device_id, bucket_id, event_id AS id, timestamp, duration, data_json
            FROM events
            WHERE {' AND '.join(conditions)}
            ORDER BY timestamp DESC
            LIMIT ?
        """
        with closing(self.connect()) as connection:
            rows = connection.execute(query, values).fetchall()
        result = []
        for row in rows:
            item = dict(row)
            item["data"] = json.loads(item.pop("data_json"))
            result.append(item)
        return result

    @staticmethod
    def _summary_from_row(row: sqlite3.Row) -> dict[str, Any]:
        report = json.loads(row["report_json"])
        report["device_id"] = row["device_id"]
        report["server_received_at"] = row["last_received"]
        return report

    @staticmethod
    def _parse_utc_timestamp(value: str | None) -> datetime | None:
        if not value:
            return None
        candidate = value[:-1] + "+00:00" if value.endswith("Z") else value
        try:
            parsed = datetime.fromisoformat(candidate)
        except ValueError:
            return None
        if parsed.tzinfo is None:
            parsed = parsed.replace(tzinfo=timezone.utc)
        return parsed.astimezone(timezone.utc)

    @staticmethod
    def _utc_iso(value: datetime) -> str:
        return value.astimezone(timezone.utc).isoformat(timespec="milliseconds").replace(
            "+00:00", "Z"
        )

    def _source_coverage(
        self,
        connection: sqlite3.Connection,
        *,
        device_id: str,
        report_date: str,
        report: dict[str, Any],
    ) -> dict[str, Any]:
        timezone_name = str(report.get("timezone") or "Asia/Shanghai")
        try:
            report_timezone = ZoneInfo(timezone_name)
        except ZoneInfoNotFoundError:
            report_timezone = BEIJING
            timezone_name = "Asia/Shanghai"
        day = date.fromisoformat(report_date)
        start_local = datetime.combine(day, time.min, report_timezone)
        end_local = start_local + timedelta(days=1)
        start_utc = start_local.astimezone(timezone.utc)
        end_utc = end_local.astimezone(timezone.utc)
        start_iso = self._utc_iso(start_utc)
        end_iso = self._utc_iso(end_utc)
        rows = connection.execute(
            """
            SELECT b.type, b.client, b.created, b.last_updated, b.first_seen, b.last_seen,
                   MIN(e.timestamp) AS first_event,
                   MAX(e.timestamp) AS latest_event,
                   SUM(CASE WHEN e.timestamp>=? AND e.timestamp<? THEN 1 ELSE 0 END)
                       AS events_on_date
            FROM buckets b
            LEFT JOIN events e
              ON e.device_id=b.device_id AND e.bucket_id=b.bucket_id
            WHERE b.device_id=?
            GROUP BY b.device_id, b.bucket_id
            ORDER BY b.bucket_id
            """,
            (start_iso, end_iso, device_id),
        ).fetchall()

        grouped: dict[str, list[sqlite3.Row]] = {"window": [], "afk": []}
        for row in rows:
            bucket_type = str(row["type"] or "").lower()
            client = str(row["client"] or "").lower()
            if bucket_type == "currentwindow" or "watcher-window" in client:
                grouped["window"].append(row)
            elif bucket_type == "afkstatus" or "watcher-afk" in client:
                grouped["afk"].append(row)

        def role_summary(role: str) -> dict[str, Any]:
            role_rows = grouped[role]
            starts: list[datetime] = []
            confirmed_through: list[datetime] = []
            events_on_date = 0
            for row in role_rows:
                first_event = self._parse_utc_timestamp(row["first_event"])
                created = self._parse_utc_timestamp(row["created"])
                if first_event:
                    starts.append(first_event)
                elif created:
                    starts.append(created)
                for field in ("latest_event", "last_updated", "last_seen"):
                    parsed = self._parse_utc_timestamp(row[field])
                    if parsed:
                        confirmed_through.append(parsed)
                events_on_date += int(row["events_on_date"] or 0)
            coverage_start = min(starts) if starts else None
            coverage_end = max(confirmed_through) if confirmed_through else None
            return {
                "exists": bool(role_rows),
                "bucket_count": len(role_rows),
                "coverage_started_at": self._utc_iso(coverage_start) if coverage_start else None,
                "coverage_confirmed_through": self._utc_iso(coverage_end) if coverage_end else None,
                "events_on_date": events_on_date,
                "has_events_on_date": events_on_date > 0,
                "covers_day_start": bool(coverage_start and coverage_start <= start_utc),
                "covers_day_end": bool(coverage_end and coverage_end >= end_utc),
            }

        window = role_summary("window")
        afk = role_summary("afk")
        role_starts = [
            self._parse_utc_timestamp(item["coverage_started_at"])
            for item in (window, afk)
            if item["coverage_started_at"]
        ]
        role_ends = [
            self._parse_utc_timestamp(item["coverage_confirmed_through"])
            for item in (window, afk)
            if item["coverage_confirmed_through"]
        ]
        both_exist = window["exists"] and afk["exists"]
        source_started_after_day = bool(
            both_exist
            and role_starts
            and all(item is not None and item >= end_utc for item in role_starts)
        )
        day_complete = bool(report.get("is_day_complete"))
        covers_full_day = bool(
            both_exist
            and window["covers_day_start"]
            and window["covers_day_end"]
            and afk["covers_day_start"]
            and afk["covers_day_end"]
        )
        if not both_exist or source_started_after_day:
            coverage_status = "missing"
        elif day_complete and covers_full_day:
            coverage_status = "complete"
        else:
            coverage_status = "partial"
        data_latest = max((item for item in role_ends if item is not None), default=None)
        common_start = max((item for item in role_starts if item is not None), default=None)
        common_end = min((item for item in role_ends if item is not None), default=None)
        return {
            "status": coverage_status,
            "report_date": report_date,
            "timezone": timezone_name,
            "window_bucket_exists": window["exists"],
            "afk_bucket_exists": afk["exists"],
            "window_bucket": window,
            "afk_bucket": afk,
            "coverage_started_at": self._utc_iso(common_start) if common_start else None,
            "coverage_confirmed_through": self._utc_iso(common_end) if common_end else None,
            "data_latest_at": self._utc_iso(data_latest) if data_latest else None,
            "covers_full_day": covers_full_day,
            "sufficient_to_confirm_zero": coverage_status == "complete",
        }

    def get_daily_report(self, *, device_id: str, report_date: str | None) -> dict[str, Any] | None:
        query = """
            SELECT device_id, report_date, report_json, last_received
            FROM daily_summaries
            WHERE device_id=?
        """
        values: list[Any] = [device_id]
        if report_date is not None:
            query += " AND report_date=?"
            values.append(report_date)
        query += " ORDER BY report_date DESC LIMIT 1"
        with closing(self.connect()) as connection:
            row = connection.execute(query, values).fetchone()
            if row is None:
                return None
            report = self._summary_from_row(row)
            report["source_coverage"] = self._source_coverage(
                connection,
                device_id=device_id,
                report_date=row["report_date"],
                report=report,
            )
            return report

    def list_daily_reports(self, *, device_id: str, days: int) -> list[dict[str, Any]]:
        with closing(self.connect()) as connection:
            rows = connection.execute(
                """
                SELECT device_id, report_date, report_json, last_received
                FROM daily_summaries
                WHERE device_id=?
                ORDER BY report_date DESC
                LIMIT ?
                """,
                (device_id, days),
            ).fetchall()
            reports = []
            for row in reversed(rows):
                report = self._summary_from_row(row)
                report["source_coverage"] = self._source_coverage(
                    connection,
                    device_id=device_id,
                    report_date=row["report_date"],
                    report=report,
                )
                reports.append(report)
            return reports


class SyncHttpServer(ThreadingHTTPServer):
    daemon_threads = True
    tls_handshake_timeout_seconds = 15.0
    request_idle_timeout_seconds = 60.0

    def __init__(self, address: tuple[str, int], config: Config, database: Database) -> None:
        self.config = config
        self.database = database
        super().__init__(address, SyncRequestHandler)

    def process_request_thread(self, request: Any, client_address: Any) -> None:
        # 握手只能在单个请求线程进行，不能阻塞主接受线程。
        if isinstance(request, ssl.SSLSocket):
            try:
                request.settimeout(self.tls_handshake_timeout_seconds)
                request.do_handshake()
                # 正常上传按每次网络读取的空闲时间限时，不限制整包上传总时长。
                request.settimeout(self.request_idle_timeout_seconds)
            except OSError as error:
                LOGGER.warning("TLS 握手未完成，关闭连接: %s", type(error).__name__)
                self.shutdown_request(request)
                return
        super().process_request_thread(request, client_address)


class SyncRequestHandler(BaseHTTPRequestHandler):
    server: SyncHttpServer
    server_version = f"ActivityWatchSync/{VERSION}"
    sys_version = ""

    def _send_json(self, status: int, payload: dict[str, Any] | list[Any]) -> None:
        body = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("X-Frame-Options", "DENY")
        if self.server.config.cert_path:
            self.send_header("Strict-Transport-Security", "max-age=31536000")
        self.end_headers()
        self.wfile.write(body)

    def _send_error(self, error: ApiError) -> None:
        self._send_json(error.status, {"error": error.code, "message": error.message})

    def _require_auth(self) -> None:
        authorization = self.headers.get("Authorization", "")
        prefix = "Bearer "
        supplied = authorization[len(prefix) :] if authorization.startswith(prefix) else ""
        if not supplied or not hmac.compare_digest(supplied, self.server.config.token):
            raise ApiError(HTTPStatus.UNAUTHORIZED, "unauthorized", "鉴权失败")

    def _read_json(self) -> Any:
        content_type = self.headers.get("Content-Type", "").split(";", 1)[0].strip().lower()
        if content_type != "application/json":
            raise ApiError(HTTPStatus.UNSUPPORTED_MEDIA_TYPE, "invalid_content_type", "Content-Type 必须是 application/json")
        try:
            content_length = int(self.headers.get("Content-Length", ""))
        except ValueError as exc:
            raise ApiError(HTTPStatus.LENGTH_REQUIRED, "invalid_length", "Content-Length 无效") from exc
        if content_length <= 0:
            raise ApiError(HTTPStatus.BAD_REQUEST, "empty_body", "请求体为空")
        if content_length > self.server.config.max_body_bytes:
            raise ApiError(HTTPStatus.REQUEST_ENTITY_TOO_LARGE, "body_too_large", "请求体过大")
        raw = self.rfile.read(content_length)
        try:
            return json.loads(raw)
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_json", "请求体不是有效 JSON") from exc

    def do_GET(self) -> None:  # noqa: N802
        try:
            parsed = urlparse(self.path)
            if parsed.path == "/health":
                self._send_json(
                    HTTPStatus.OK,
                    {"status": "ok", "service": "activitywatch-sync", "version": VERSION, "server_time": utc_now()},
                )
                return

            self._require_auth()
            query = parse_qs(parsed.query)
            if parsed.path == "/v1/status":
                self._send_json(HTTPStatus.OK, self.server.database.status())
                return
            if parsed.path == "/v1/buckets":
                device_id = required_text((query.get("device_id") or [None])[0], "device_id", 255)
                self._send_json(HTTPStatus.OK, {"buckets": self.server.database.list_buckets(device_id)})
                return
            if parsed.path == "/v1/reports/daily":
                device_id = required_text((query.get("device_id") or [None])[0], "device_id", 255)
                report_date_raw = (query.get("date") or [None])[0]
                report_date = normalize_date(report_date_raw, "date") if report_date_raw is not None else None
                if report_date is not None and date.fromisoformat(report_date) > datetime.now(BEIJING).date():
                    raise ApiError(
                        HTTPStatus.BAD_REQUEST,
                        "date_in_future",
                        "ActivityWatch 日报日期不能晚于北京时间今天",
                    )
                report = self.server.database.get_daily_report(device_id=device_id, report_date=report_date)
                if report is None:
                    raise ApiError(HTTPStatus.NOT_FOUND, "report_not_found", "没有找到对应的 ActivityWatch 日报")
                self._send_json(HTTPStatus.OK, {"report": report})
                return
            if parsed.path == "/v1/reports/recent":
                device_id = required_text((query.get("device_id") or [None])[0], "device_id", 255)
                try:
                    days = int((query.get("days") or ["7"])[0])
                except ValueError as exc:
                    raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_days", "days 必须是整数") from exc
                if days < 1 or days > 31:
                    raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_days", "days 必须在 1 到 31 之间")
                reports = self.server.database.list_daily_reports(device_id=device_id, days=days)
                self._send_json(HTTPStatus.OK, {"reports": reports, "days": days})
                return
            if parsed.path == "/v1/events":
                device_id = required_text((query.get("device_id") or [None])[0], "device_id", 255)
                bucket_id = (query.get("bucket_id") or [None])[0]
                if bucket_id is not None:
                    bucket_id = required_text(bucket_id, "bucket_id", 512)
                start_raw = (query.get("start") or [None])[0]
                end_raw = (query.get("end") or [None])[0]
                start = normalize_timestamp(start_raw, "start", optional=True)
                end = normalize_timestamp(end_raw, "end", optional=True)
                try:
                    limit = int((query.get("limit") or ["1000"])[0])
                except ValueError as exc:
                    raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_limit", "limit 必须是整数") from exc
                if limit < 1 or limit > 5000:
                    raise ApiError(HTTPStatus.BAD_REQUEST, "invalid_limit", "limit 必须在 1 到 5000 之间")
                events = self.server.database.list_events(
                    device_id=device_id,
                    bucket_id=bucket_id,
                    start=start,
                    end=end,
                    limit=limit,
                )
                self._send_json(HTTPStatus.OK, {"events": events})
                return
            raise ApiError(HTTPStatus.NOT_FOUND, "not_found", "接口不存在")
        except ApiError as error:
            self._send_error(error)
        except Exception:
            LOGGER.exception("GET 请求处理失败: %s", self.path)
            self._send_json(HTTPStatus.INTERNAL_SERVER_ERROR, {"error": "internal_error", "message": "服务器内部错误"})

    def do_POST(self) -> None:  # noqa: N802
        try:
            parsed = urlparse(self.path)
            if parsed.path != "/v1/sync":
                raise ApiError(HTTPStatus.NOT_FOUND, "not_found", "接口不存在")
            self._require_auth()
            payload = validate_sync_payload(self._read_json())
            result = self.server.database.store_sync(payload)
            self._send_json(
                HTTPStatus.OK,
                {
                    "status": "ok",
                    "batch_id": payload["batch_id"],
                    **result,
                    "server_time": utc_now(),
                },
            )
        except ApiError as error:
            self._send_error(error)
        except Exception:
            LOGGER.exception("POST 请求处理失败: %s", self.path)
            self._send_json(HTTPStatus.INTERNAL_SERVER_ERROR, {"error": "internal_error", "message": "服务器内部错误"})

    def log_message(self, format_string: str, *args: Any) -> None:
        LOGGER.info("%s - %s", self.address_string(), format_string % args)


def main() -> None:
    logging.basicConfig(
        level=os.environ.get("AWSYNC_LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    config = Config.from_environment()
    database = Database(config.db_path)
    database.initialize()
    server = SyncHttpServer((config.bind_host, config.port), config, database)

    scheme = "http"
    if config.cert_path and config.key_path:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(config.cert_path, config.key_path)
        server.socket = context.wrap_socket(
            server.socket, server_side=True, do_handshake_on_connect=False
        )
        scheme = "https"

    def request_shutdown(signum: int, _frame: Any) -> None:
        LOGGER.info("收到信号 %s，准备停止", signum)
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, request_shutdown)
    signal.signal(signal.SIGINT, request_shutdown)
    LOGGER.info("ActivityWatch Sync %s 正在监听 %s://%s:%s", VERSION, scheme, config.bind_host, config.port)
    try:
        server.serve_forever(poll_interval=0.5)
    finally:
        server.server_close()
        LOGGER.info("服务已停止")


if __name__ == "__main__":
    main()
