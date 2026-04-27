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
cp .env.example .env
go run ./cmd/codebuddy2api
```

Build a binary:

```bash
go build -o codebuddy2api ./cmd/codebuddy2api
./codebuddy2api
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

- `GET /admin` lightweight management panel
- `GET /health`
- `GET /v1/models`
- `POST /v1/chat/completions`
- `GET /admin/accounts`
- `POST /admin/accounts`
- `PATCH /admin/accounts/{id}`
- `POST /admin/accounts/{id}/enable`
- `POST /admin/accounts/{id}/disable`
- `DELETE /admin/accounts/{id}`
- `POST /admin/accounts/{id}/probe`
- `GET /admin/stats`

## Config

See `.env.example`.

Important defaults:

- upstream: `https://copilot.tencent.com/v2/chat/completions`
- listen: `127.0.0.1:18182`
- default model list: `glm-5.1`, `minimax-m2.7`, `kimi-k2.6`
- default account pool strategy: `round-robin`
- database: `./data/codebuddy2api.sqlite3`

`CODEBUDDY2API_API_KEY` protects `/v1/*`. `CODEBUDDY2API_ADMIN_KEY`
protects `/admin/*`; if omitted, the admin API also accepts the client API key.

The `/admin` panel stores the admin key only in browser localStorage and sends it
as `Authorization: Bearer <key>` to the Admin API. API responses only expose
`api_key_preview`, never the full CodeBuddy account key.

The management panel supports Chinese UI, account/key add and edit, key
rotation, enable/disable, delete, probe, cooldown reset, filtering, bulk
enable/disable/probe/reset, and line-based bulk import.
It also supports per-account credit quota limits, quota-exhausted auto pause,
expiry auto pause, and error-code based auto pause. CodeBuddy does not expose a
confirmed stable balance endpoint for `ck_...` keys yet; the service uses the
upstream `usage.credit` tail packet as the persisted accounting source.

## Account fields

- `api_key`: CodeBuddy `ck_...` key. Stored in SQLite; API responses only show a preview.
- `enabled`: whether the scheduler can use this account.
- `priority`: higher priority accounts are tried first.
- `weight`: weighted round-robin inside the same priority tier.
- `concurrency`: max in-flight requests for this account in the current process.
- `quota_limit`: optional account credit limit. `0` means unlimited.
- `quota_auto_disable`: disable scheduling when accumulated `usage.credit`
  reaches `quota_limit`.
- `expires_at` / `expire_auto_disable`: optional expiry timestamp and automatic
  scheduling pause after expiry.
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

- pool strategy is configurable from the admin settings page or
  `CODEBUDDY2API_POOL_STRATEGY`.
- `round-robin`: higher priority tiers are tried first; inside the same tier,
  accounts are selected by weighted round-robin.
- `fill-first`: higher priority tiers are tried first; inside the same tier,
  higher weight and lower ID accounts are filled first. If no quota/expiry is
  configured, the first eligible account may keep receiving all sequential
  traffic.
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
