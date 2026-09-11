# 官方 CodeBuddy CLI 请求基线（2.149.0）

抓取方式：本地录制反代（`CODEBUDDY_BASE_URL=http://127.0.0.1:19999/v2`
+ `CODEBUDDY_AUTH_TOKEN=<intl access token>`），官方 CLI 原样发出、录完再转发给
`https://www.codebuddy.ai`，请求与 SSE 响应双向落盘。不需要 TLS 中间人。

抓取时间：2026-09-11。官方 CLI 版本 `@tencent-ai/codebuddy-code@2.149.0`。

## 结论

我们当前 `BuildHeaders` 发 10 个头，官方发 35 个有效头。
**正确 6 · 值错 3 · 漏发 26 · 多余 2。**

## 请求头基线（原始顺序）

官方 CLI 用的是 OpenAI 官方 JS SDK（stainless 生成），所以带一整套 `x-stainless-*`
指纹；另有一套完整的分布式追踪头（W3C traceparent + B3）和会话/请求 ID 族。

| # | 头 | 官方值 | 我们 | 备注 |
|---|---|---|---|---|
| 1 | `Accept` | `application/json` | `text/event-stream` | **值错**。即使 `stream:true` 官方也发 json |
| 2 | `Content-Type` | `application/json` | 同 | ✓ |
| 3 | `x-requested-with` | `XMLHttpRequest` | 同 | ✓ |
| 4-10 | `x-stainless-arch` `-lang` `-os` `-package-version` `-retry-count` `-runtime` `-runtime-version` | `x64` `js` `Linux` `6.25.0` `0` `node` `v22.22.0` | — | **漏发 7 个**。OpenAI JS SDK 指纹 |
| 11 | `X-Conversation-ID` | UUIDv7 | — | 漏发。整轮会话固定 |
| 12 | `X-Agent-Intent` | `craft` | `CodeCompletion` | **值错** |
| 13 | `X-Agent-Purpose` | `conversation` | — | 漏发 |
| 14-16 | `X-IDE-Type` `X-IDE-Name` `X-IDE-Version` | `CLI` `CLI` `2.149.0` | — | 漏发 |
| 17 | `X-Private-Data` | `false` | — | 漏发 |
| 18 | `x-codebuddy-request` | `1` | — | 漏发 |
| 19 | `X-Request-ID` | 32 hex | 32 hex | ✓ |
| 20 | `X-Conversation-Message-ID` | = X-Request-ID | — | 漏发 |
| 21 | `X-Conversation-Request-ID` | 32 hex | — | 漏发。同一轮内固定 |
| 22 | `X-Root-Request-ID` | = X-Conversation-Request-ID | — | 漏发 |
| 23 | `X-Agent-Type` | `main` | — | 漏发。子 agent 应为其它值 |
| 24 | `traceparent` | `00-<32hex>-<16hex>-01` | — | 漏发。W3C |
| 25 | `b3` | `<trace>-<span>-1-<parent>` | — | 漏发。B3 single |
| 26-29 | `X-B3-TraceId` `-ParentSpanId` `-SpanId` `-Sampled` | 32hex / 16hex / 16hex / `1` | — | 漏发 |
| 30 | `X-Trace-ID` | = X-B3-TraceId | — | 漏发 |
| 31 | `Authorization` | `Bearer <access token>` | 同 | ✓ |
| 32 | `X-User-Id` | JWT 的 `sub` claim | — | **漏发**，可从 token 直接解出 |
| 33 | `X-Domain` | `www.codebuddy.ai` | 同 | ✓ |
| 34 | `X-Product` | `SaaS` | 同 | ✓ |
| 35 | `User-Agent` | `CLI/2.149.0 CodeBuddy/2.149.0` | `CLI/1.0.8 CodeBuddy/1.0.8` | **值错**，版本落后 140+ |
| — | `X-Env-ID` | 不发 | `production` | **多余** |
| — | `X-Machine-Id` | 不发 | uuid | **多余** |

### ID 之间的关系（同一轮请求内）

```
X-Conversation-ID          整轮会话固定（UUIDv7）
X-Conversation-Request-ID  == X-Root-Request-ID     一轮请求固定
X-Request-ID               == X-Conversation-Message-ID  每条消息一个
X-B3-TraceId == X-Trace-ID == traceparent 的 trace 段
X-B3-SpanId  == traceparent 的 span 段
```

## 请求体基线

```jsonc
{
  "model": "deepseek-v4.1-flash",
  "messages": [
    { "role": "system", "content": "You are CodeBuddy Code.You are an interactive CLI tool..." },  // 纯字符串
    { "role": "user", "content": [ { "type": "text", "text": "..." } ] }                            // typed blocks
  ],
  "tools": [ /* 22 个 */ ],
  "temperature": 1,
  "stream": true,
  "stream_options": { "include_usage": true }
}
```

- 首条必须是 `system`，且 `content` 是**纯字符串**（不是 typed block）
- `user` 的 `content` 是 **typed block 数组**
- 我们现有的 intl 整形方向正确，`stream_options.include_usage` 也对得上

## 响应基线

响应头里有可用于健康检查的字段：

```
Content-Type: text/event-stream
Traceid: <32hex>
X-Request-Id: <32hex>
X-User-Id: <sub>
X-WAF-UUID: <...>          ← 有 WAF
```

SSE 为标准 OpenAI chunk 格式，`delta` 里除 `content` 外还有 `reasoning_content`，
以 `data: [DONE]` 结束。

## 复现方法

```bash
python3 scratchpad/record_proxy.py 19999    # 录制反代
CODEBUDDY_BASE_URL=http://127.0.0.1:19999/v2 \
CODEBUDDY_AUTH_TOKEN=$(cat ~/.cache/codebuddy-intl/access.txt) \
CODEBUDDY_MODEL=deepseek-v4.1-flash \
codebuddy -p "只回复两个字：收到"
```
