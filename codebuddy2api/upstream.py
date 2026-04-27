from __future__ import annotations

import json
import time
import uuid
from collections.abc import AsyncIterator
from dataclasses import dataclass, field
from typing import Any

import httpx

from .config import Settings
from .store import Account


@dataclass
class StreamState:
    response_id: str
    model: str
    content_parts: list[str] = field(default_factory=list)
    reasoning_parts: list[str] = field(default_factory=list)
    tool_calls: list[dict[str, Any]] = field(default_factory=list)
    finish_reason: str | None = None
    usage: dict[str, Any] | None = None
    saw_done: bool = False


class UpstreamStatusError(RuntimeError):
    def __init__(self, status_code: int, body: str):
        self.status_code = status_code
        self.body = body
        super().__init__(f"upstream returned {status_code}: {body[:300]}")


class CodeBuddyClient:
    def __init__(self, settings: Settings):
        self.settings = settings

    def prepare_payload(self, request_body: dict[str, Any]) -> dict[str, Any]:
        payload = dict(request_body)
        payload["stream"] = True
        payload.setdefault("model", "glm-5.1")
        return payload

    def build_headers(self, account: Account) -> dict[str, str]:
        profile = account.header_profile or {}
        request_id = str(uuid.uuid4()).replace("-", "")
        headers = {
            "Accept": "text/event-stream",
            "Content-Type": "application/json",
            "X-Requested-With": "XMLHttpRequest",
            "X-Domain": "copilot.tencent.com",
            "X-Product": "SaaS",
            "X-Agent-Intent": str(profile.get("agent_intent") or "CodeCompletion"),
            "X-Env-ID": str(profile.get("env_id") or "production"),
            "X-Request-ID": str(profile.get("request_id") or request_id),
            "X-Machine-Id": str(profile.get("machine_id") or uuid.uuid4()),
            "User-Agent": str(profile.get("user_agent") or "CLI/1.0.8 CodeBuddy/1.0.8"),
            "X-Api-Key": account.api_key,
        }
        extra_headers = profile.get("extra_headers")
        if isinstance(extra_headers, dict):
            for key, value in extra_headers.items():
                if key and value is not None:
                    headers[str(key)] = str(value)
        return headers

    async def stream_chat(
        self,
        account: Account,
        request_body: dict[str, Any],
    ) -> AsyncIterator[tuple[str, StreamState]]:
        payload = self.prepare_payload(request_body)
        state = StreamState(
            response_id=f"chatcmpl-{uuid.uuid4().hex}",
            model=str(request_body.get("model") or payload.get("model") or "glm-5.1"),
        )
        timeout = httpx.Timeout(
            timeout=self.settings.request_timeout_seconds,
            connect=self.settings.connect_timeout_seconds,
            read=self.settings.request_timeout_seconds,
            write=self.settings.connect_timeout_seconds,
        )
        async with httpx.AsyncClient(timeout=timeout, proxy=account.proxy_url) as client:
            async with client.stream(
                "POST",
                self.settings.upstream_url,
                json=payload,
                headers=self.build_headers(account),
            ) as response:
                if response.status_code != 200:
                    body = (await response.aread()).decode("utf-8", errors="replace")
                    raise UpstreamStatusError(response.status_code, body)

                async for line in response.aiter_lines():
                    data = parse_sse_data_line(line)
                    if data is None:
                        continue
                    if data == "[DONE]":
                        state.saw_done = True
                        yield "data: [DONE]\n\n", state
                        continue
                    try:
                        chunk = json.loads(data)
                    except json.JSONDecodeError:
                        continue
                    normalize_chunk_for_client(chunk, state)
                    yield f"data: {json.dumps(chunk, ensure_ascii=False)}\n\n", state

    async def complete_chat(self, account: Account, request_body: dict[str, Any]) -> tuple[dict[str, Any], StreamState]:
        state: StreamState | None = None
        async for _wire, current_state in self.stream_chat(account, request_body):
            state = current_state
        if state is None:
            state = StreamState(
                response_id=f"chatcmpl-{uuid.uuid4().hex}",
                model=str(request_body.get("model") or "glm-5.1"),
            )
        return build_non_stream_response(state), state

    async def probe(self, account: Account) -> dict[str, Any]:
        body = {
            "model": "glm-5.1",
            "messages": [{"role": "user", "content": "只回复OK"}],
            "stream": False,
            "max_tokens": 8,
        }
        response, state = await self.complete_chat(account, body)
        return {"ok": True, "usage": state.usage or {}, "response": response}


def parse_sse_data_line(line: str) -> str | None:
    if not line:
        return None
    stripped = line.strip()
    if not stripped:
        return None
    if stripped.startswith("data:"):
        return stripped[5:].strip()
    return None


def normalize_chunk_for_client(chunk: dict[str, Any], state: StreamState) -> None:
    if not chunk.get("id"):
        chunk["id"] = state.response_id
    chunk["model"] = state.model
    chunk.setdefault("object", "chat.completion.chunk")
    chunk.setdefault("created", int(time.time()))
    if chunk.get("usage"):
        state.usage = chunk.get("usage")

    choices = chunk.get("choices")
    if not isinstance(choices, list):
        return
    for choice in choices:
        if not isinstance(choice, dict):
            continue
        if choice.get("finish_reason"):
            state.finish_reason = choice.get("finish_reason")
        delta = choice.get("delta")
        if not isinstance(delta, dict):
            continue
        content = delta.get("content")
        if isinstance(content, str):
            state.content_parts.append(content)
        reasoning_content = delta.get("reasoning_content")
        if isinstance(reasoning_content, str) and reasoning_content:
            state.reasoning_parts.append(reasoning_content)
            if not content:
                # Some compatible upstreams emit all visible text in reasoning_content.
                # Surface it as content so standard OpenAI clients do not receive blanks.
                delta["content"] = reasoning_content
        _merge_tool_calls(state.tool_calls, delta.get("tool_calls"))


def _merge_tool_calls(target: list[dict[str, Any]], incoming: Any) -> None:
    if not isinstance(incoming, list):
        return
    for item in incoming:
        if not isinstance(item, dict):
            continue
        index = int(item.get("index") or 0)
        while len(target) <= index:
            target.append({"id": "", "type": "function", "function": {"name": "", "arguments": ""}})
        current = target[index]
        if item.get("id"):
            current["id"] = item["id"]
        if item.get("type"):
            current["type"] = item["type"]
        function = item.get("function")
        if isinstance(function, dict):
            current_function = current.setdefault("function", {"name": "", "arguments": ""})
            if function.get("name"):
                current_function["name"] = str(current_function.get("name", "")) + str(function["name"])
            if function.get("arguments"):
                current_function["arguments"] = str(current_function.get("arguments", "")) + str(function["arguments"])


def build_non_stream_response(state: StreamState) -> dict[str, Any]:
    message: dict[str, Any] = {
        "role": "assistant",
        "content": "".join(state.content_parts) or "".join(state.reasoning_parts),
    }
    if state.tool_calls:
        message["tool_calls"] = state.tool_calls
        if not message["content"]:
            message["content"] = None
    usage = state.usage or {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
    return {
        "id": state.response_id,
        "object": "chat.completion",
        "created": int(time.time()),
        "model": state.model,
        "choices": [
            {
                "index": 0,
                "message": message,
                "finish_reason": state.finish_reason or "stop",
            }
        ],
        "usage": usage,
    }
