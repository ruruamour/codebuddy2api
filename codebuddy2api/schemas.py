from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, Field


class AccountCreate(BaseModel):
    name: str = Field(default="CodeBuddy account", min_length=1, max_length=120)
    api_key: str = Field(min_length=8)
    enabled: bool = True
    priority: int = 100
    weight: int = Field(default=1, ge=1, le=100)
    concurrency: int = Field(default=1, ge=1, le=100)
    proxy_url: str | None = None
    header_profile: dict[str, Any] | None = None
    notes: str | None = None


class AccountPatch(BaseModel):
    name: str | None = Field(default=None, min_length=1, max_length=120)
    api_key: str | None = Field(default=None, min_length=8)
    enabled: bool | None = None
    priority: int | None = None
    weight: int | None = Field(default=None, ge=1, le=100)
    concurrency: int | None = Field(default=None, ge=1, le=100)
    proxy_url: str | None = None
    header_profile: dict[str, Any] | None = None
    notes: str | None = None
    reset_failures: bool = False


class ChatMessage(BaseModel):
    role: str
    content: Any = None


class ChatCompletionRequest(BaseModel):
    model: str = "glm-5.1"
    messages: list[dict[str, Any]]
    stream: bool = False
    temperature: float | None = None
    top_p: float | None = None
    max_tokens: int | None = None
    stop: Any = None
    tools: Any = None
    tool_choice: Any = None
    response_format: Any = None
    stream_options: dict[str, Any] | None = None

    model_config = {"extra": "allow"}


class ErrorResponse(BaseModel):
    error: dict[str, Any]


class AccountStatus(BaseModel):
    id: int
    name: str
    enabled: bool
    status: Literal["active", "disabled", "cooldown"]
    priority: int
    weight: int
    concurrency: int
    in_flight: int
    proxy_url: str | None
    api_key_preview: str
    total_requests: int
    total_success: int
    total_failures: int
    total_credit: float
    prompt_tokens: int
    completion_tokens: int
    total_tokens: int
    consecutive_failures: int
    cooldown_until: int | None
    last_success_at: int | None
    last_failure_at: int | None
    last_error: str | None
    notes: str | None
