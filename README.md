# CodeBuddy2API

OpenAI-compatible proxy for Tencent CodeBuddy Key accounts.

This project is based on the MIT-licensed `Jevil961/codebuddy-openai-proxy`
protocol shape, but is rewritten around the NuoAPI production needs:

- `ck_...` API key credentials
- account pool with per-account concurrency
- proxy/header profile per account
- health/cooldown/failover state
- upstream `usage.credit` accounting
- OpenAI-compatible `/v1/chat/completions` and `/v1/models`

## Quick start

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
python main.py
```

Add a CodeBuddy account:

```bash
curl -X POST http://127.0.0.1:18182/admin/accounts \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $CODEBUDDY2API_ADMIN_KEY" \
  -d '{
    "name": "codebuddy-1",
    "api_key": "ck_xxx",
    "concurrency": 1,
    "weight": 1,
    "priority": 100
  }'
```

Call through OpenAI-compatible chat completions:

```bash
curl -N http://127.0.0.1:18182/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $CODEBUDDY2API_API_KEY" \
  -d '{
    "model": "glm-5.1",
    "messages": [{"role": "user", "content": "只回复OK"}],
    "stream": true,
    "max_tokens": 8
  }'
```

## API surface

- `GET /health`
- `GET /v1/models`
- `POST /v1/chat/completions`
- `GET /admin/accounts`
- `POST /admin/accounts`
- `PATCH /admin/accounts/{id}`
- `POST /admin/accounts/{id}/enable`
- `POST /admin/accounts/{id}/disable`
- `POST /admin/accounts/{id}/probe`
- `GET /admin/stats`

## Config

See `.env.example`.

Important defaults:

- upstream: `https://copilot.tencent.com/v2/chat/completions`
- listen: `127.0.0.1:18182`
- model list: `glm-5.1`
- database: `./data/codebuddy2api.sqlite3`

`CODEBUDDY2API_API_KEY` protects `/v1/*`. `CODEBUDDY2API_ADMIN_KEY`
protects `/admin/*`; if omitted, the admin API also accepts the client API key.

## Account fields

- `api_key`: CodeBuddy `ck_...` key. Stored in SQLite; API responses only show a preview.
- `enabled`: whether the scheduler can use this account.
- `priority`: higher priority accounts are tried first.
- `weight`: weighted round-robin inside the same priority tier.
- `concurrency`: max in-flight requests for this account in the current process.
- `proxy_url`: optional `http://`, `https://`, or `socks5://` proxy URL for this account.
- `header_profile`: optional request header profile:

```json
{
  "user_agent": "CLI/1.0.8 CodeBuddy/1.0.8",
  "machine_id": "stable-machine-id",
  "agent_intent": "CodeCompletion",
  "env_id": "production",
  "extra_headers": {
    "X-Custom": "value"
  }
}
```

## Scheduler behavior

- 401/403: account is disabled.
- 429/5xx: account enters cooldown.
- repeated failures: account enters cooldown after `CODEBUDDY2API_FAILURE_THRESHOLD`.
- cooldown expiry: account is eligible again automatically.
- success: clears consecutive failures and cooldown.

## sub2api integration

Configure a normal OpenAI-compatible APIKey upstream:

```text
base_url = http://127.0.0.1:18182/v1
api_key  = CODEBUDDY2API_API_KEY
model    = glm-5.1
```

Keep sub2api billing on its existing `glm-5.1` pricing. CodeBuddy2API records
the upstream `usage.credit` separately for reconciliation.
