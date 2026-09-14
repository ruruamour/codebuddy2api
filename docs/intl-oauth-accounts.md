# 国际版（intl）账号凭证获取教程

这份文档讲清楚一件事：**`www.codebuddy.ai` / `www.workbuddy.ai` 那套 OAuth 凭证
是怎么拿到的，以及怎么批量搞更多账号来轮询。**

国内版（`copilot.tencent.com`）用的是控制台签发的 `ck_` API key，和本文无关。
国际版**不接受 `ck_` key**（打过去是 `401 {"message":"not_found"}`），只能走 OAuth。

验证时间：2026-09-12。文中命令均实测可用。

---

## 一、我们要拿到什么

一条 intl 凭证是**一对 token**：

| 字段 | 说明 |
|---|---|
| `accessToken` | Keycloak JWT，默认 **365 天**。请求时放 `Authorization: Bearer <它>` |
| `refreshToken` | 同样 365 天，**每次刷新会轮换**（返回新的）。用来续期 |

access token 解出来的 JWT：

```json
{
  "iss": "https://www.codebuddy.ai/auth/realms/copilot",
  "aud": "account",
  "sub": "16539ee0-de05-4b35-b85e-165d1a1bd465",
  "azp": "console",
  "scope": "openid profile offline_access email",
  "email": "cnnnd@foxmail.com",
  "exp": 1820671020
}
```

关键点：

- `sub` 是账号唯一标识，**同一个第三方身份重复授权拿到的 `sub` 相同**
- `scope` 里的 `offline_access` 是"能长期刷新"的依据，没有它 refresh 会失败
- 服务端发请求时 `X-User-Id` 头就是 `sub`

---

## 二、前置条件

1. **一个第三方身份**：Google / GitHub / X 三选一。这是唯一的门槛——
   CodeBuddy 没有邮箱密码注册，全靠这三个 OAuth 提供商
2. **一个浏览器**（或能跑无头浏览器的环境），用来完成授权点击
3. 首次注册要**选注册地区**（新加坡 / 日本 / 泰国 / 印度尼西亚），**注册后不可改**

> 地区决定汇率和税率，不影响能不能用。选哪个都行。

---

## 三、获取流程（三步）

整体是标准 OAuth device-ish 流程：**起 state → 浏览器授权 → 轮询换 token**。

### 第 1 步：起授权请求，拿 `state` 和 `authUrl`

```bash
NONCE=$(openssl rand -hex 8)
curl -s -X POST "https://www.codebuddy.ai/v2/plugin/auth/state?platform=CLI&nonce=$NONCE" \
  -H "Accept: application/json, text/plain, */*" \
  -H "Content-Type: application/json" \
  -H "X-Requested-With: XMLHttpRequest" \
  -H "X-Domain: www.codebuddy.ai" \
  -H "X-No-Authorization: true" \
  -H "X-No-User-Id: true" \
  -H "X-No-Enterprise-Id: true" \
  -H "X-No-Department-Info: true" \
  -H "X-Product: SaaS" \
  -H "X-Request-Id: $(openssl rand -hex 16)" \
  -H "User-Agent: CLI/1.0.8 CodeBuddy/1.0.8" \
  -d "{\"nonce\":\"$NONCE\"}"
```

返回：

```json
{
  "code": 0,
  "msg": "OK",
  "data": {
    "state": "591515ff-26a9-4975-90fb-aafd6fd54499",
    "authUrl": "https://www.codebuddy.ai/login?platform=CLI&state=591515ff-..."
  }
}
```

**坑**：必须是 `POST`。用 `GET` 会得到 `404 page not found`。

### 第 2 步：浏览器打开 `authUrl` 完成授权

打开 `authUrl` 会看到登录页，三个入口：

```
使用 Google 注册 / 登录
使用 GitHub 注册 / 登录     ← 本文示例走这个
使用 X 注册 / 登录
```

页面结构（排障时有用）：登录表单在 **Keycloak iframe** 里，
`https://www.codebuddy.ai/auth/realms/copilot/protocol/openid-connect/auth?client_id=console`。
所以直接抓外层页面的 DOM 是空的，要进 iframe 才能点到按钮。

点击第三方登录后的链路：

```
/login?platform=CLI&state=...
  → Keycloak iframe（勾选两个协议复选框）
  → 点「使用 GitHub 注册」
  → github.com/login/oauth/authorize?client_id=Iv23lijhQ5xyezqGSzfU
      App 名称：Tencent Buddy Agent（owner: CodeBuddy-Official-Account）
  → 点 Authorize
  → www.codebuddy.ai/register/user/complete   ← 首次注册才出现
      选注册地区 → 提交
  → www.codebuddy.ai/started?platform=CLI&state=...
```

> 已有账号的话不会出现选地区那步，直接到 `/started`。

### 第 3 步：轮询换 token

```bash
STATE="591515ff-26a9-4975-90fb-aafd6fd54499"
curl -s "https://www.codebuddy.ai/v2/plugin/auth/token?state=$STATE" \
  -H "Accept: application/json, text/plain, */*" \
  -H "X-Domain: www.codebuddy.ai" \
  -H "X-Product: SaaS" \
  -H "X-Requested-With: XMLHttpRequest" \
  -H "User-Agent: CLI/1.0.8 CodeBuddy/1.0.8"
```

授权完成前会一直返回：

```json
{"code":11217,"msg":"11217:login ing...","requestId":"..."}
```

授权完成后返回：

```json
{
  "code": 0,
  "msg": "OK",
  "data": {
    "accessToken": "eyJhbGciOiJSUzI1NiIsInR5cCIgOiAiSldUIiwia2lkIiA6ICJXVzhVVkZuS0lNSnl3cFdQWjBEWTZxeE9LQ2dpcVVjNXN3RHBkVjM1UUV3In0...",
    "refreshToken": "eyJhbGciOiJIUzI1NiIsInR5cCIgOiAiSldUIiwia2lkIiA6ICJhYTNm...",
    "expiresIn": 31535360,
    "refreshExpiresIn": 31535360,
    "tokenType": "Bearer",
    "sessionState": "...",
    "scope": "openid profile offline_access email",
    "domain": "www.codebuddy.ai"
  }
}
```

`expiresIn: 31535360` ≈ 365 天。

**重要**：这个 `state` **只能用一次**。取到 token 后立刻落盘，
再轮询一次会返回 `11217 login ing...`（已被消费）。

---

## 四、验证凭证可用

```bash
TOK=$(jq -r .data.accessToken /tmp/token.json)

curl -s -N -X POST "https://www.codebuddy.ai/v2/chat/completions" \
  -H "Authorization: Bearer $TOK" \
  -H "X-Domain: www.codebuddy.ai" \
  -H "X-Product: SaaS" \
  -H "X-Requested-With: XMLHttpRequest" \
  -H "X-Env-ID: production" \
  -H "X-Agent-Intent: CodeCompletion" \
  -H "X-Request-Id: $(openssl rand -hex 16)" \
  -H "X-Machine-Id: $(openssl rand -hex 16)" \
  -H "User-Agent: CLI/1.0.8 CodeBuddy/1.0.8" \
  -H "Accept: text/event-stream" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v4.1-flash",
    "stream": true,
    "messages": [
      {"role": "system", "content": "You are CodeBuddy Code."},
      {"role": "user", "content": [{"type": "text", "text": "只回答两个字：你好"}]}
    ]
  }'
```

**请求体硬性要求**（不满足会被拒）：

- `stream` 必须为 `true` —— 非流式直接 `11101 Non-stream chat request is currently not supported`
- 首条消息必须是 `system`，且 `content` 是**纯字符串** —— 否则 `11128 first message is not system prompt`
- `user` 的 `content` 是 **typed block 数组**（`[{"type":"text","text":"..."}]`），不是裸字符串

正常响应末尾的 usage：

```json
{"usage":{"prompt_tokens":17,"completion_tokens":1,"total_tokens":18,"credit":0}}
```

`credit: 0` 表示这次调用没扣积分（限免模型）。

---

## 五、限额特性（决定要不要多账号）

### 限额是**按模型**算的

实测同一账号、同一时刻：

| 模型 | 结果 |
|---|---|
| `deepseek-v4.1-flash` | ❌ 频率限制 |
| `glm-5.3` | ✅ 可用 |
| `gpt-5.4` | ✅ 可用 |
| `kimi-k2.6` | ✅ 可用 |

被限流时返回：

```json
{
  "code": 6004,
  "msg": "usage exceeds frequency limit, but don't worry, your usage will reset at 2026-09-12 22:09:16 UTC+8, alternatively, you can switch to the other models to continue using it."
}
```

**这句话里有两个有用信息**：
1. `will reset at <时刻>` —— 权威的重置时间，按这个时间冷却即可（服务端已实现）
2. `switch to the other models` —— 换模型能立刻绕开

### 只有 v4.1-flash 是限免的

`credit` 字段对比（同一次小请求）：

| 模型 | credit |
|---|---|
| `deepseek-v4.1-flash` | `0` |
| `glm-5.3` | `0.02` |
| `gpt-5.4` | `0.04` |

所以"换模型"这条只适合不想被限流打断的场景，换过去就开始扣积分了。

### 限免期限

第三方注册表记录 `promoFreeUntil: "2026-09-24"`（UTC 零点起不再免费）。
这不是官方公告，是社区维护的注册表（`techysy/10router`）里的值，仅供参考。

---

## 六、怎么搞更多账号（轮询用）

### 核心规则：一个第三方身份 = 一个 CodeBuddy 号

同一个浏览器里先 GitHub 再 Google 授权，**拿到的 `sub` 是同一个** ——
Keycloak 会把同一浏览器会话里的身份链到同一个 user 上。

所以每个新号必须满足：

1. **换一个第三方账号**（另一个 Google / GitHub / X）
2. **清掉 Keycloak 会话**，否则又会被链到旧号

清会话的两个 cookie（域 `www.codebuddy.ai`）：

```
KEYCLOAK_SESSION
KEYCLOAK_IDENTITY      ← 这个最容易漏，只清 SESSION 不够
```

或者干脆**每个号用独立的浏览器 profile**，最省事、最不容易出错。

### 批量流程

```
for 每个第三方身份:
    1. POST /v2/plugin/auth/state  → state, authUrl
    2. 独立 profile 的浏览器打开 authUrl → 授权 → （首次）选地区
    3. GET /v2/plugin/auth/token?state=... 轮询到 code=0
    4. 存下 accessToken + refreshToken（立刻，state 一次性）
    5. 验证：打一次 deepseek-v4.1-flash
```

导入服务：

- 面板「账号池」→ 粘贴 token → 账号类型选 **CodeBuddy 国际版**（或"自动识别"）
- 或走导入导出接口批量灌

### 关于登录态能不能复用

浏览器 profile 里只要还有 `www.codebuddy.ai` 域的 Keycloak cookie，
重走一遍流程时**不用重新输密码**，点两下就完成。这是批量开号能自动化的前提。

---

## 七、续期（别让 365 天到期静默失效）

refresh token 也能自助续期，**每次刷新会返回新的 refresh token**（轮换），
所以只要在过期前刷一次，理论上可以永久续下去。

```bash
curl -s -X POST "https://www.codebuddy.ai/v2/plugin/auth/token/refresh" \
  -H "Accept: application/json, text/plain, */*" \
  -H "Content-Type: application/json" \
  -H "X-Requested-With: XMLHttpRequest" \
  -H "X-Request-Id: $(openssl rand -hex 16)" \
  -H "X-Domain: www.codebuddy.ai" \
  -H "X-Refresh-Token: $REFRESH_TOKEN" \
  -H "X-Auth-Refresh-Source: plugin" \
  -H "X-Product: SaaS" \
  -H "User-Agent: CLI/1.0.8 CodeBuddy/1.0.8" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -d '{}'
```

**坑**：refresh token 放在 **`X-Refresh-Token` 头**里，不在 body。
放 body 会得到 `10001 refreshToken is empty`。

服务端已实现自动续期（`internal/app/refresh.go`）：每 6 小时巡检一次，
剩余不足 30 天就自动换新。**导入账号时务必带上 refresh token**，
只导 access token 的话，365 天后就是个"到期即废"的残废号。

---

## 八、错误码速查

| code | 含义 | 处理 |
|---|---|---|
| `11217` | `login ing...` 授权还没完成 | 继续轮询；或 state 已被消费 |
| `6004` | 频率限制（HTTP 429） | 按 `msg` 里的 `reset at` 时刻冷却，或换模型 |
| `11101` | 非流式请求不支持 | 请求体加 `"stream": true` |
| `11102` | `model service info not found` | 该模型在 intl 不可用（如 `deepseek-v4-flash` 老款） |
| `11128` | 首条不是 system prompt | 补一条前导 `system` |
| `11150` | `invalid_reasoning_effort` | 只接受 `minimal/low/medium/high/xhigh/max`（小写） |
| `10001` | refreshToken is empty | refresh token 要放 `X-Refresh-Token` 头 |
| `401 not_found` | 用 `ck_` key 打了 intl | intl 不认 API key，只能 OAuth |

---

## 九、模型目录的坑

intl 的模型目录要**合并两个来源**：

| 来源 | 内容 |
|---|---|
| `www.codebuddy.ai/v3/config` | CodeBuddy CLI 目录，约 35 个（Gemini / GPT / Hunyuan / GLM），**没有** v4.1-flash |
| `copilot.tencent.com/v3/config` | 29 个，**有** `deepseek-v4.1-flash` / `hy4-preview` |

用同一个 intl token 两个都能读。只读第一个会以为"intl 没有 v4.1-flash"。

WorkBuddy 是另一套域名（`www.workbuddy.ai`），协议相同、模型目录不同，
且要用 IDE 指纹（`User-Agent: VSCode/... WorkBuddy/...`）而不是 CLI 的。

---

## 十、安全

- access / refresh token **等同账号密码**，能直接打上游。别进 git、别贴群里
- 服务端存储：`accounts.api_key` / `accounts.refresh_token`，数据库文件权限要收紧
- 导出接口（`/admin/accounts/export`）是**明文**的（脱敏过就导不回去），
  面板必须放在 Cloudflare Access 之类的鉴权后面
- 面板只暴露 `/v1/*` 和 `/health`，管理接口不要开公网

---

## 附：一页速查

```bash
# 1. 起 state
NONCE=$(openssl rand -hex 8)
curl -s -X POST "https://www.codebuddy.ai/v2/plugin/auth/state?platform=CLI&nonce=$NONCE" \
  -H "Content-Type: application/json" -H "X-Domain: www.codebuddy.ai" \
  -H "X-Product: SaaS" -H "X-Requested-With: XMLHttpRequest" \
  -H "X-No-Authorization: true" -H "X-No-User-Id: true" \
  -H "X-No-Enterprise-Id: true" -H "X-No-Department-Info: true" \
  -H "X-Request-Id: $(openssl rand -hex 16)" \
  -H "User-Agent: CLI/1.0.8 CodeBuddy/1.0.8" -d "{\"nonce\":\"$NONCE\"}"

# 2. 浏览器打开 data.authUrl，完成授权

# 3. 轮询（授权完成后立刻取，state 一次性）
curl -s "https://www.codebuddy.ai/v2/plugin/auth/token?state=<state>" \
  -H "X-Domain: www.codebuddy.ai" -H "X-Product: SaaS" \
  -H "X-Requested-With: XMLHttpRequest" \
  -H "User-Agent: CLI/1.0.8 CodeBuddy/1.0.8"
```
