from __future__ import annotations

import json
import logging
from typing import Any

from fastapi import Depends, FastAPI, HTTPException, Request, status
from fastapi.responses import HTMLResponse, JSONResponse, RedirectResponse, StreamingResponse

from . import __version__
from .admin_ui import ADMIN_HTML
from .config import Settings
from .pool import AccountPool, Lease, NoAccountAvailable
from .schemas import AccountCreate, AccountPatch, ChatCompletionRequest
from .store import AccountStore
from .upstream import CodeBuddyClient, UpstreamStatusError


logger = logging.getLogger(__name__)


def create_app(settings: Settings | None = None) -> FastAPI:
    settings = settings or Settings.from_env()
    logging.basicConfig(level=getattr(logging, settings.log_level, logging.INFO))
    store = AccountStore(settings.db_path)
    pool = AccountPool(store)
    upstream = CodeBuddyClient(settings)

    app = FastAPI(title="CodeBuddy2API", version=__version__)
    app.state.settings = settings
    app.state.store = store
    app.state.pool = pool
    app.state.upstream = upstream

    @app.get("/")
    async def root() -> RedirectResponse:
        return RedirectResponse(url="/admin", status_code=307)

    @app.get("/admin", response_class=HTMLResponse)
    async def admin_ui() -> HTMLResponse:
        return HTMLResponse(ADMIN_HTML)

    @app.get("/health")
    async def health() -> dict[str, Any]:
        stats = store.stats()
        return {
            "status": "ok",
            "service": "codebuddy2api",
            "version": __version__,
            "accounts": stats["accounts"],
            "enabled_accounts": stats["enabled_accounts"],
        }

    @app.get("/v1/models", dependencies=[Depends(require_client_auth)])
    async def list_models(request: Request) -> dict[str, Any]:
        settings = request.app.state.settings
        return {
            "object": "list",
            "data": [
                {"id": model, "object": "model", "created": 0, "owned_by": "codebuddy"}
                for model in settings.models
            ],
        }

    @app.post("/v1/chat/completions", dependencies=[Depends(require_client_auth)])
    async def chat_completions(request: Request) -> Any:
        try:
            raw_body = await request.json()
            chat_req = ChatCompletionRequest.model_validate(raw_body)
        except Exception as exc:
            raise HTTPException(status_code=400, detail=f"invalid request body: {exc}") from exc

        request_body = chat_req.model_dump(exclude_none=True)
        if settings.debug_requests:
            logger.info("chat.request summary=%s", json.dumps(summarize_chat_request(request_body), ensure_ascii=False))
        if chat_req.model not in settings.models:
            # Keep OpenAI-compatible behavior permissive for mapped clients, but log drift.
            logger.info("requested model is not in advertised list: %s", chat_req.model)

        try:
            lease = await pool.acquire()
        except NoAccountAvailable as exc:
            return openai_error(str(exc), "no_account_available", 503)

        if chat_req.stream:
            return StreamingResponse(
                stream_response(request.app, lease, request_body),
                media_type="text/event-stream",
                headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
            )

        try:
            response, state = await upstream.complete_chat(lease.account, request_body)
            store.record_success(lease.account.id, state.usage)
            return JSONResponse(response)
        except UpstreamStatusError as exc:
            store.record_failure(
                lease.account.id,
                exc.body,
                status_code=exc.status_code,
                cooldown_seconds=settings.cooldown_seconds,
                failure_threshold=settings.failure_threshold,
            )
            return openai_error(exc.body, "upstream_error", exc.status_code)
        except Exception as exc:
            store.record_failure(
                lease.account.id,
                str(exc),
                status_code=None,
                cooldown_seconds=settings.cooldown_seconds,
                failure_threshold=settings.failure_threshold,
            )
            return openai_error(str(exc), "proxy_error", 502)
        finally:
            await pool.release(lease)

    @app.get("/admin/accounts", dependencies=[Depends(require_admin_auth)])
    async def admin_list_accounts(request: Request) -> dict[str, Any]:
        in_flight = await request.app.state.pool.in_flight_snapshot()
        accounts = request.app.state.store.list_accounts()
        for account in accounts:
            account["in_flight"] = in_flight.get(account["id"], 0)
        return {"accounts": accounts}

    @app.post("/admin/accounts", dependencies=[Depends(require_admin_auth)], status_code=201)
    async def admin_create_account(request: Request, payload: AccountCreate) -> dict[str, Any]:
        account_id = request.app.state.store.add_account(payload)
        return {"id": account_id}

    @app.patch("/admin/accounts/{account_id}", dependencies=[Depends(require_admin_auth)])
    async def admin_patch_account(request: Request, account_id: int, payload: AccountPatch) -> dict[str, Any]:
        ok = request.app.state.store.patch_account(account_id, payload)
        if not ok:
            raise HTTPException(status_code=404, detail="account not found")
        return {"ok": True}

    @app.post("/admin/accounts/{account_id}/enable", dependencies=[Depends(require_admin_auth)])
    async def admin_enable_account(request: Request, account_id: int) -> dict[str, Any]:
        if not request.app.state.store.set_enabled(account_id, True):
            raise HTTPException(status_code=404, detail="account not found")
        return {"ok": True}

    @app.post("/admin/accounts/{account_id}/disable", dependencies=[Depends(require_admin_auth)])
    async def admin_disable_account(request: Request, account_id: int) -> dict[str, Any]:
        if not request.app.state.store.set_enabled(account_id, False):
            raise HTTPException(status_code=404, detail="account not found")
        return {"ok": True}

    @app.delete("/admin/accounts/{account_id}", dependencies=[Depends(require_admin_auth)])
    async def admin_delete_account(request: Request, account_id: int) -> dict[str, Any]:
        if not request.app.state.store.delete_account(account_id):
            raise HTTPException(status_code=404, detail="account not found")
        return {"ok": True}

    @app.post("/admin/accounts/{account_id}/probe", dependencies=[Depends(require_admin_auth)])
    async def admin_probe_account(request: Request, account_id: int) -> dict[str, Any]:
        account = request.app.state.store.get_account(account_id)
        if account is None:
            raise HTTPException(status_code=404, detail="account not found")
        try:
            result = await request.app.state.upstream.probe(account)
            request.app.state.store.record_success(account.id, result.get("usage"))
            return result
        except UpstreamStatusError as exc:
            request.app.state.store.record_failure(
                account.id,
                exc.body,
                status_code=exc.status_code,
                cooldown_seconds=request.app.state.settings.cooldown_seconds,
                failure_threshold=request.app.state.settings.failure_threshold,
            )
            raise HTTPException(status_code=exc.status_code, detail=exc.body[:500]) from exc
        except Exception as exc:
            request.app.state.store.record_failure(
                account.id,
                str(exc),
                status_code=None,
                cooldown_seconds=request.app.state.settings.cooldown_seconds,
                failure_threshold=request.app.state.settings.failure_threshold,
            )
            raise HTTPException(status_code=502, detail=str(exc)) from exc

    @app.get("/admin/stats", dependencies=[Depends(require_admin_auth)])
    async def admin_stats(request: Request) -> dict[str, Any]:
        return request.app.state.store.stats()

    return app


async def stream_response(app: FastAPI, lease: Lease, request_body: dict[str, Any]):
    store: AccountStore = app.state.store
    pool: AccountPool = app.state.pool
    upstream: CodeBuddyClient = app.state.upstream
    settings: Settings = app.state.settings
    final_usage: dict[str, Any] | None = None
    try:
        async for wire, state in upstream.stream_chat(lease.account, request_body):
            final_usage = state.usage or final_usage
            yield wire
        store.record_success(lease.account.id, final_usage)
    except UpstreamStatusError as exc:
        store.record_failure(
            lease.account.id,
            exc.body,
            status_code=exc.status_code,
            cooldown_seconds=settings.cooldown_seconds,
            failure_threshold=settings.failure_threshold,
        )
        yield f"data: {json.dumps({'error': {'message': exc.body[:500], 'type': 'upstream_error'}}, ensure_ascii=False)}\n\n"
        yield "data: [DONE]\n\n"
    except Exception as exc:
        store.record_failure(
            lease.account.id,
            str(exc),
            status_code=None,
            cooldown_seconds=settings.cooldown_seconds,
            failure_threshold=settings.failure_threshold,
        )
        yield f"data: {json.dumps({'error': {'message': str(exc), 'type': 'proxy_error'}}, ensure_ascii=False)}\n\n"
        yield "data: [DONE]\n\n"
    finally:
        await pool.release(lease)


async def require_client_auth(request: Request) -> None:
    settings: Settings = request.app.state.settings
    if not settings.api_key:
        return
    if bearer_token(request) != settings.api_key:
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="invalid API key")


async def require_admin_auth(request: Request) -> None:
    settings: Settings = request.app.state.settings
    tokens = set(settings.admin_tokens())
    if not tokens:
        return
    if bearer_token(request) not in tokens:
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="invalid admin API key")


def bearer_token(request: Request) -> str:
    auth = request.headers.get("authorization", "")
    if auth.lower().startswith("bearer "):
        return auth[7:].strip()
    return request.headers.get("x-api-key", "").strip()


def openai_error(message: str, error_type: str, status_code: int) -> JSONResponse:
    return JSONResponse(
        status_code=status_code,
        content={"error": {"message": message[:1000], "type": error_type}},
    )


def summarize_chat_request(body: dict[str, Any]) -> dict[str, Any]:
    summary: dict[str, Any] = {
        "model": body.get("model"),
        "stream": body.get("stream"),
        "max_tokens": body.get("max_tokens"),
        "max_completion_tokens": body.get("max_completion_tokens"),
        "reasoning_effort": body.get("reasoning_effort"),
        "keys": sorted(body.keys()),
    }
    messages = body.get("messages")
    if isinstance(messages, list):
        summary["messages"] = [
            {
                "role": msg.get("role") if isinstance(msg, dict) else None,
                "content_type": type(msg.get("content")).__name__ if isinstance(msg, dict) else type(msg).__name__,
                "content_preview": _content_preview(msg.get("content")) if isinstance(msg, dict) else "",
            }
            for msg in messages[-3:]
        ]
    return summary


def _content_preview(content: Any) -> str:
    if isinstance(content, str):
        return content[:120]
    if isinstance(content, list):
        parts: list[str] = []
        for item in content[:3]:
            if isinstance(item, dict):
                text = item.get("text")
                parts.append(str(text)[:60] if text is not None else str(item.get("type", ""))[:60])
            else:
                parts.append(str(item)[:60])
        return " | ".join(parts)
    return str(content)[:120]
