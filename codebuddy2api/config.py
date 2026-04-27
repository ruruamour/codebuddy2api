from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path
from typing import Sequence

from dotenv import load_dotenv


def _env_int(name: str, default: int) -> int:
    raw = os.getenv(name)
    if raw is None or raw.strip() == "":
        return default
    return int(raw)


def _env_bool(name: str, default: bool = False) -> bool:
    raw = os.getenv(name)
    if raw is None or raw.strip() == "":
        return default
    return raw.strip().lower() in {"1", "true", "yes", "on"}


@dataclass(frozen=True)
class Settings:
    host: str
    port: int
    db_path: str
    api_key: str
    admin_key: str
    upstream_url: str
    models: tuple[str, ...]
    cooldown_seconds: int
    failure_threshold: int
    default_concurrency: int
    request_timeout_seconds: int
    connect_timeout_seconds: int
    log_level: str
    debug_requests: bool

    @classmethod
    def from_env(cls) -> "Settings":
        load_dotenv()
        models = tuple(
            item.strip()
            for item in os.getenv("CODEBUDDY2API_MODELS", "glm-5.1").split(",")
            if item.strip()
        )
        db_path = os.getenv("CODEBUDDY2API_DB_PATH", "./data/codebuddy2api.sqlite3")
        Path(db_path).expanduser().parent.mkdir(parents=True, exist_ok=True)
        return cls(
            host=os.getenv("CODEBUDDY2API_HOST", "127.0.0.1"),
            port=_env_int("CODEBUDDY2API_PORT", 18182),
            db_path=db_path,
            api_key=os.getenv("CODEBUDDY2API_API_KEY", "").strip(),
            admin_key=os.getenv("CODEBUDDY2API_ADMIN_KEY", "").strip(),
            upstream_url=os.getenv(
                "CODEBUDDY2API_UPSTREAM_URL",
                "https://copilot.tencent.com/v2/chat/completions",
            ).strip(),
            models=models or ("glm-5.1",),
            cooldown_seconds=_env_int("CODEBUDDY2API_COOLDOWN_SECONDS", 300),
            failure_threshold=_env_int("CODEBUDDY2API_FAILURE_THRESHOLD", 3),
            default_concurrency=_env_int("CODEBUDDY2API_DEFAULT_CONCURRENCY", 1),
            request_timeout_seconds=_env_int("CODEBUDDY2API_REQUEST_TIMEOUT_SECONDS", 300),
            connect_timeout_seconds=_env_int("CODEBUDDY2API_CONNECT_TIMEOUT_SECONDS", 10),
            log_level=os.getenv("CODEBUDDY2API_LOG_LEVEL", "INFO").upper(),
            debug_requests=_env_bool("CODEBUDDY2API_DEBUG_REQUESTS", False),
        )

    def admin_tokens(self) -> Sequence[str]:
        tokens = [self.admin_key, self.api_key]
        return tuple(token for token in tokens if token)
