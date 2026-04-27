from __future__ import annotations

import json
import sqlite3
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

from .schemas import AccountCreate, AccountPatch


def now_ts() -> int:
    return int(time.time())


def mask_secret(value: str) -> str:
    if not value:
        return ""
    if len(value) <= 12:
        return value[:3] + "..."
    return f"{value[:6]}...{value[-4:]}"


@dataclass(frozen=True)
class Account:
    id: int
    name: str
    api_key: str
    enabled: bool
    status: str
    priority: int
    weight: int
    concurrency: int
    proxy_url: str | None
    header_profile: dict[str, Any]
    notes: str | None
    consecutive_failures: int
    cooldown_until: int | None

    @property
    def api_key_preview(self) -> str:
        return mask_secret(self.api_key)


class AccountStore:
    def __init__(self, db_path: str):
        self.db_path = str(Path(db_path).expanduser())
        Path(self.db_path).parent.mkdir(parents=True, exist_ok=True)
        self._init_db()

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.db_path, timeout=30)
        conn.row_factory = sqlite3.Row
        return conn

    def _init_db(self) -> None:
        with self._connect() as conn:
            conn.execute(
                """
                CREATE TABLE IF NOT EXISTS accounts (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    name TEXT NOT NULL,
                    api_key TEXT NOT NULL,
                    enabled INTEGER NOT NULL DEFAULT 1,
                    status TEXT NOT NULL DEFAULT 'active',
                    priority INTEGER NOT NULL DEFAULT 100,
                    weight INTEGER NOT NULL DEFAULT 1,
                    concurrency INTEGER NOT NULL DEFAULT 1,
                    proxy_url TEXT,
                    header_profile TEXT NOT NULL DEFAULT '{}',
                    notes TEXT,
                    consecutive_failures INTEGER NOT NULL DEFAULT 0,
                    cooldown_until INTEGER,
                    total_requests INTEGER NOT NULL DEFAULT 0,
                    total_success INTEGER NOT NULL DEFAULT 0,
                    total_failures INTEGER NOT NULL DEFAULT 0,
                    total_credit REAL NOT NULL DEFAULT 0,
                    prompt_tokens INTEGER NOT NULL DEFAULT 0,
                    completion_tokens INTEGER NOT NULL DEFAULT 0,
                    total_tokens INTEGER NOT NULL DEFAULT 0,
                    last_success_at INTEGER,
                    last_failure_at INTEGER,
                    last_error TEXT,
                    created_at INTEGER NOT NULL,
                    updated_at INTEGER NOT NULL
                )
                """
            )
            conn.execute(
                "CREATE INDEX IF NOT EXISTS idx_accounts_sched ON accounts(enabled, status, priority, id)"
            )

    def add_account(self, payload: AccountCreate) -> int:
        ts = now_ts()
        with self._connect() as conn:
            cur = conn.execute(
                """
                INSERT INTO accounts (
                    name, api_key, enabled, status, priority, weight, concurrency,
                    proxy_url, header_profile, notes, created_at, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    payload.name,
                    payload.api_key,
                    1 if payload.enabled else 0,
                    "active" if payload.enabled else "disabled",
                    payload.priority,
                    payload.weight,
                    payload.concurrency,
                    payload.proxy_url,
                    json.dumps(payload.header_profile or {}, ensure_ascii=False),
                    payload.notes,
                    ts,
                    ts,
                ),
            )
            return int(cur.lastrowid)

    def patch_account(self, account_id: int, payload: AccountPatch) -> bool:
        fields: list[str] = []
        values: list[Any] = []
        data = payload.model_dump(exclude_unset=True)
        reset_failures = bool(data.pop("reset_failures", False))
        for key, value in data.items():
            if key == "header_profile" and value is not None:
                value = json.dumps(value, ensure_ascii=False)
            if key == "enabled" and value is not None:
                value = 1 if value else 0
                fields.append("status = ?")
                values.append("active" if value else "disabled")
            fields.append(f"{key} = ?")
            values.append(value)
        if reset_failures:
            fields.extend(["consecutive_failures = ?", "cooldown_until = ?", "last_error = ?", "status = ?"])
            values.extend([0, None, None, "active"])
        if not fields:
            return self.get_account(account_id) is not None
        fields.append("updated_at = ?")
        values.append(now_ts())
        values.append(account_id)
        with self._connect() as conn:
            cur = conn.execute(f"UPDATE accounts SET {', '.join(fields)} WHERE id = ?", values)
            return cur.rowcount > 0

    def set_enabled(self, account_id: int, enabled: bool) -> bool:
        ts = now_ts()
        with self._connect() as conn:
            cur = conn.execute(
                """
                UPDATE accounts
                SET enabled = ?, status = ?, consecutive_failures = CASE WHEN ? THEN 0 ELSE consecutive_failures END,
                    cooldown_until = CASE WHEN ? THEN NULL ELSE cooldown_until END, updated_at = ?
                WHERE id = ?
                """,
                (1 if enabled else 0, "active" if enabled else "disabled", enabled, enabled, ts, account_id),
            )
            return cur.rowcount > 0

    def delete_account(self, account_id: int) -> bool:
        with self._connect() as conn:
            cur = conn.execute("DELETE FROM accounts WHERE id = ?", (account_id,))
            return cur.rowcount > 0

    def get_account(self, account_id: int) -> Account | None:
        with self._connect() as conn:
            row = conn.execute("SELECT * FROM accounts WHERE id = ?", (account_id,)).fetchone()
        return self._row_to_account(row) if row else None

    def list_accounts(self) -> list[dict[str, Any]]:
        with self._connect() as conn:
            rows = conn.execute("SELECT * FROM accounts ORDER BY priority DESC, id ASC").fetchall()
        return [self._row_to_public(row) for row in rows]

    def schedulable_accounts(self) -> list[Account]:
        ts = now_ts()
        with self._connect() as conn:
            rows = conn.execute(
                """
                SELECT * FROM accounts
                WHERE enabled = 1
                  AND (status = 'active' OR (status = 'cooldown' AND (cooldown_until IS NULL OR cooldown_until <= ?)))
                ORDER BY priority DESC, id ASC
                """,
                (ts,),
            ).fetchall()
        return [self._row_to_account(row) for row in rows]

    def record_success(self, account_id: int, usage: dict[str, Any] | None) -> None:
        usage = usage or {}
        credit = _float_value(usage.get("credit"))
        prompt = _int_value(usage.get("prompt_tokens"))
        completion = _int_value(usage.get("completion_tokens"))
        total = _int_value(usage.get("total_tokens")) or prompt + completion
        ts = now_ts()
        with self._connect() as conn:
            conn.execute(
                """
                UPDATE accounts
                SET status = 'active',
                    consecutive_failures = 0,
                    cooldown_until = NULL,
                    total_requests = total_requests + 1,
                    total_success = total_success + 1,
                    total_credit = total_credit + ?,
                    prompt_tokens = prompt_tokens + ?,
                    completion_tokens = completion_tokens + ?,
                    total_tokens = total_tokens + ?,
                    last_success_at = ?,
                    last_error = NULL,
                    updated_at = ?
                WHERE id = ?
                """,
                (credit, prompt, completion, total, ts, ts, account_id),
            )

    def record_failure(
        self,
        account_id: int,
        error: str,
        *,
        status_code: int | None,
        cooldown_seconds: int,
        failure_threshold: int,
    ) -> None:
        ts = now_ts()
        error = error[:1000]
        terminal_auth_error = status_code in (401, 403)
        cooldown_error = status_code == 429 or (status_code is not None and status_code >= 500)
        with self._connect() as conn:
            row = conn.execute(
                "SELECT consecutive_failures FROM accounts WHERE id = ?",
                (account_id,),
            ).fetchone()
            next_failures = (int(row["consecutive_failures"]) if row else 0) + 1
            enabled = 0 if terminal_auth_error else 1
            next_status = "disabled" if terminal_auth_error else "active"
            cooldown_until = None
            if not terminal_auth_error and (cooldown_error or next_failures >= failure_threshold):
                next_status = "cooldown"
                cooldown_until = ts + cooldown_seconds
            conn.execute(
                """
                UPDATE accounts
                SET enabled = ?,
                    status = ?,
                    consecutive_failures = ?,
                    cooldown_until = ?,
                    total_requests = total_requests + 1,
                    total_failures = total_failures + 1,
                    last_failure_at = ?,
                    last_error = ?,
                    updated_at = ?
                WHERE id = ?
                """,
                (enabled, next_status, next_failures, cooldown_until, ts, error, ts, account_id),
            )

    def stats(self) -> dict[str, Any]:
        with self._connect() as conn:
            row = conn.execute(
                """
                SELECT
                    COUNT(*) AS accounts,
                    SUM(CASE WHEN enabled = 1 THEN 1 ELSE 0 END) AS enabled_accounts,
                    SUM(total_requests) AS total_requests,
                    SUM(total_success) AS total_success,
                    SUM(total_failures) AS total_failures,
                    SUM(total_credit) AS total_credit,
                    SUM(prompt_tokens) AS prompt_tokens,
                    SUM(completion_tokens) AS completion_tokens,
                    SUM(total_tokens) AS total_tokens
                FROM accounts
                """
            ).fetchone()
        return {key: row[key] or 0 for key in row.keys()}

    def _row_to_account(self, row: sqlite3.Row) -> Account:
        try:
            profile = json.loads(row["header_profile"] or "{}")
        except json.JSONDecodeError:
            profile = {}
        return Account(
            id=int(row["id"]),
            name=row["name"],
            api_key=row["api_key"],
            enabled=bool(row["enabled"]),
            status=row["status"],
            priority=int(row["priority"]),
            weight=max(1, int(row["weight"])),
            concurrency=max(1, int(row["concurrency"])),
            proxy_url=row["proxy_url"],
            header_profile=profile if isinstance(profile, dict) else {},
            notes=row["notes"],
            consecutive_failures=int(row["consecutive_failures"]),
            cooldown_until=row["cooldown_until"],
        )

    def _row_to_public(self, row: sqlite3.Row) -> dict[str, Any]:
        account = self._row_to_account(row)
        data = dict(row)
        data.pop("api_key", None)
        data["api_key_preview"] = account.api_key_preview
        data["enabled"] = bool(row["enabled"])
        try:
            data["header_profile"] = json.loads(row["header_profile"] or "{}")
        except json.JSONDecodeError:
            data["header_profile"] = {}
        return data


def _int_value(value: Any) -> int:
    try:
        return int(value or 0)
    except (TypeError, ValueError):
        return 0


def _float_value(value: Any) -> float:
    try:
        return float(value or 0)
    except (TypeError, ValueError):
        return 0.0
