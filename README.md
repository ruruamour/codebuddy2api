# MBAgent - 三模型 PK 编程助手

> 三个 AI 模型各司其职、互相竞技，为你输出最优答案。

## 项目简介

MBAgent 是一个基于 [CodeBuddy Agent SDK](https://www.codebuddy.ai) 构建的 AI 编程助手。它独创了 **三模型 PK 机制**：

```
用户提问
    │
    ▼
┌─────────────────────────────────────────┐
│  🏗️ 架构师 (Kimi K2.5)    → 设计方案   │
│  💻 开发者 (GLM 5.0)       → 写代码     │
│  🔍 质保 QA (DeepSeek V3)  → 审查挑刺   │
│              × N 轮迭代优化              │
│  💻 最终综合输出（支持工具调用）          │
└─────────────────────────────────────────┘
    │
    ▼
  返回结果（含完整的思考过程）
```

三个模型各司其职、互相竞技，最后综合所有反馈输出最终结果。整个 PK 过程会**实时流式展示**，你可以看到每个模型在做什么。

## 项目结构

```
agent/
├── .codebuddy/
│   └── settings.json     # CodeBuddy CN 版 endpoint 配置
├── .env                  # API 密钥配置（CODEBUDDY_API_KEY=你的密钥）
├── run.py                # API 服务启动脚本（解决 Windows 兼容性）
├── server.py             # FastAPI 服务端（OpenAI 兼容 API + 三模型 PK）
├── agent.py              # 命令行交互客户端（终端里直接聊天）
├── pk.py                 # 独立 PK 脚本（命令行运行 PK 竞赛）
├── requirements.txt      # Python 依赖列表
└── README.md             # 本文档
```

| 文件 | 作用 | 运行方式 |
|------|------|----------|
| `run.py` | 启动 API 服务（Windows 兼容入口） | `python run.py` |
| `server.py` | FastAPI 服务端，提供 OpenAI 兼容 API | 通过 `run.py` 启动 |
| `agent.py` | 终端交互式聊天（多轮对话+工具） | `python agent.py` |
| `pk.py` | 独立运行 PK 竞赛（终端输出） | `python pk.py "你的需求"` |

## 快速开始

### 前置条件

- **Python 3.10+**（SDK 要求）
- **CodeBuddy 账号**（国内版 codebuddy.cn）
- **API Key**（从 CodeBuddy 后台获取）

### 1. 安装依赖

```bash
pip install codebuddy-agent-sdk python-dotenv fastapi uvicorn sse-starlette
```

或使用虚拟环境：

```bash
python -m venv .venv
.venv\Scripts\activate      # Windows
source .venv/bin/activate    # Linux/Mac
pip install -r requirements.txt
```

### 2. 配置 API Key

创建 `.env` 文件（已存在则编辑）：

```env
CODEBUDDY_API_KEY=你的真实API密钥
```

> 注意：等号前后不要有空格，值不要加引号。

### 3. 启动 API 服务

```bash
python run.py
```

看到以下输出说明启动成功：

```
INFO:     Uvicorn running on http://0.0.0.0:8000 (Press CTRL+C to quit)
```

### 4. 接入客户端

#### 方式 A：Cline（VS Code 插件）

| 配置项 | 值 |
|--------|-----|
| API Base URL | `http://localhost:8000/v1` |
| Model ID | `mbagent-pk`（PK 模式）或 `quick`（快速模式） |
| API Key | 随便填，如 `123`（本地服务不校验 Key） |

#### 方式 B：ChatGPT-Next-Web / LobeChat

API 地址填 `http://localhost:8000/v1`，自定义模型名填 `mbagent-pk` 和 `quick`。

#### 方式 C：curl 测试

```bash
# 三模型 PK 模式（流式）
curl http://localhost:8000/v1/chat/completions ^
  -H "Content-Type: application/json" ^
  -d "{\"messages\":[{\"role\":\"user\",\"content\":\"写一个LRU缓存\"}],\"stream\":true}"

# 快速模式（单模型秒回）
curl http://localhost:8000/v1/chat/completions/quick ^
  -H "Content-Type: application/json" ^
  -d "{\"messages\":[{\"role\":\"user\",\"content\":\"你好\"}]}"
```

## API 接口文档

所有接口兼容 OpenAI API 格式，可直接对接第三方客户端。

### POST `/v1/chat/completions` — 三模型 PK 模式

每次请求自动执行：架构师设计 → 开发者写码 → QA 审查 → 综合输出。

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `model` | string | `mbagent-pk` | 模型 ID |
| `messages` | array | 必填 | OpenAI 格式消息列表 |
| `stream` | bool | `false` | 是否流式输出 |
| `temperature` | float | null | 温度（PK 模式暂不生效） |
| `max_tokens` | int | null | 最大 token 数（PK 模式暂不生效） |

**流式响应格式：**

```
data: {"choices":[{"delta":{"reasoning_content":"🏗️ 架构师思考中..."}}]}  ← PK 过程
data: {"choices":[{"delta":{"reasoning_content":"💻 开发者写代码中..."}}]}  ← PK 过程
data: {"choices":[{"delta":{"reasoning_content":"🔍 QA 审查中..."}}]}      ← PK 过程
data: {"choices":[{"delta":{"content":"最终结果..."}}]}                     ← 最终输出
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
```

- `reasoning_content`：PK 思考过程（Cline 会折叠显示）
- `content`：最终结果（支持工具调用）

### POST `/v1/chat/completions/quick` — 快速模式

单模型直接回复，支持工具调用，适合简单问题。参数同上。

### GET `/v1/models` — 模型列表

```json
{
  "data": [
    {"id": "mbagent-pk", "description": "三模型 PK 模式"},
    {"id": "quick", "description": "快速模式"}
  ]
}
```

### 会话管理接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/v1/sessions` | 查看所有会话 |
| DELETE | `/v1/sessions/{id}` | 清空指定会话记忆 |
| DELETE | `/v1/sessions` | 清空所有会话记忆 |

## 三模型角色详解

| 角色 | 模型 | 职责 | 擅长领域 |
|------|------|------|----------|
| 🏗️ 架构师 | Kimi K2.5 | 设计方案、评审架构 | 长文本理解、结构化分析 |
| 💻 开发者 | GLM 5.0 | 编写代码、修复问题 | 逻辑推理、代码生成 |
| 🔍 质保 QA | DeepSeek V3 | 审查代码、打分挑刺 | 代码审查、数学推理、找 bug |

### PK 流程

```
第 1 轮：
  架构师 → 分析需求，提出设计方案
  开发者 → 根据方案编写代码
  质保 QA → 审查代码，打分，找问题

第 2 轮：
  架构师 → 根据反馈修改设计
  开发者 → 修复 QA 提出的问题
  质保 QA → 再次审查

...（可配置轮数，默认 1 轮）

最终输出：
  综合所有反馈 → 直接回答用户（支持工具调用）
```

## 核心架构说明

### 技术栈

```
┌──────────────────────────────────────────────┐
│  客户端（Cline / ChatGPT-Next-Web / curl）    │
└──────────────────┬───────────────────────────┘
                   │ OpenAI 兼容 API
                   ▼
┌──────────────────────────────────────────────┐
│  FastAPI (server.py)                          │
│  ├─ PK 引擎（三模型异步生成器）               │
│  ├─ 会话记忆系统（内存存储）                   │
│  └─ OpenAI 格式转换层                         │
└──────────────────┬───────────────────────────┘
                   │ codebuddy-agent-sdk
                   ▼
┌──────────────────────────────────────────────┐
│  CodeBuddy SDK                                │
│  ├─ query() 函数（无状态查询）                 │
│  ├─ CodeBuddySDKClient（有状态多轮对话）       │
│  └─ 内部启动 codebuddy-headless CLI 子进程     │
└──────────────────┬───────────────────────────┘
                   │ HTTPS
                   ▼
┌──────────────────────────────────────────────┐
│  CodeBuddy CN (codebuddy.cn)                  │
│  ├─ Kimi K2.5    (架构师)                     │
│  ├─ GLM 5.0     (开发者)                      │
│  └─ DeepSeek V3  (质保 QA)                    │
└──────────────────────────────────────────────┘
```

### Windows 兼容性

Windows 默认使用 `ProactorEventLoop`，不支持子进程。SDK 内部需要启动 CLI 子进程通信，因此 `run.py` 在启动前将事件循环切换为 `SelectorEventLoop`。

```python
# run.py 中的关键代码
if sys.platform == "win32":
    asyncio.set_event_loop_policy(asyncio.WindowsSelectorEventLoopPolicy())
```

### 会话记忆系统

- 使用内存字典存储，服务重启后丢失
- 通过 `user` 字段或消息哈希作为会话 ID
- 最多保留 10 轮历史，每条截断 500 字符
- PK 结果只保存前 1000 字符摘要（防止上下文爆炸）

## 配置说明

### 修改 PK 轮数

在 `server.py` 中修改：

```python
result = await run_pk(pk_prompt, rounds=1)  # 改为你想要的轮数
```

> 注意：轮数越多耗时越长，1 轮约 4-8 分钟。Cline 的上下文有限，建议 1-3 轮。

### 修改模型

在 `server.py` 中修改各角色的模型 ID：

```python
# 查看你的账号可用模型
# GET http://localhost:8000/v1/models

# 可选模型（以实际账号为准）：
#   deepseek-v3-2-volc, glm-5.0, kimi-k2.5
#   gemini-3.1-pro, gpt-5.4, gemini-2.5-pro
```

### 修改 Agent 身份

在 `server.py` 中修改 `BASE_IDENTITY`：

```python
BASE_IDENTITY = """你是你的Agent名字，一个强大的 AI 编程助手。"""
```

### 添加 MCP 工具

在 `agent.py` 中配置 MCP 服务器：

```python
MCP_SERVERS = {
    "filesystem": {
        "command": "npx",
        "args": ["@modelcontextprotocol/server-filesystem", "/path/to/dir"],
    },
}
```

## 命令行工具

### 终端聊天（agent.py）

直接在终端和 Agent 对话，支持多轮对话和工具调用：

```bash
python agent.py
```

### 独立 PK 竞赛（pk.py）

不启动 API 服务，直接在终端运行 PK 竞赛：

```bash
python pk.py "实现一个 LRU 缓存"
```

## 常见问题

### Q: 启动报 `NotImplementedError`

Windows 事件循环问题。确保使用 `python run.py` 启动，不要直接 `python server.py`。

### Q: 报 `401 Authentication required`

API Key 无效或未配置。检查 `.env` 文件中的 `CODEBUDDY_API_KEY` 是否正确。

### Q: 报 `Max turns exceeded`

`max_turns` 太小或模型调用了工具消耗了轮次。已在最新版本中修复。

### Q: Cline 报 `You did not use a tool`

Cline 期望 AI 调用工具。PK 模式的最终输出阶段已支持工具调用，如果仍报错请确保使用最新版 `server.py`。

### Q: 响应很慢

PK 模式需要调用 3-4 次模型（架构师+开发者+QA+最终输出），每次约 10-30 秒，总计约 4-8 分钟。如需快速回复，使用 `quick` 模式。

## 依赖说明

| 包名 | 作用 |
|------|------|
| `codebuddy-agent-sdk` | CodeBuddy Agent SDK（核心） |
| `python-dotenv` | 从 `.env` 文件加载环境变量 |
| `fastapi` | Web 框架 |
| `uvicorn` | ASGI 服务器 |
| `sse-starlette` | Server-Sent Events 支持（流式输出） |

## License

MIT
## 免责声明

本项目仅供学习交流使用，使用请遵守腾讯云 CodeBuddy 相关服务条款。
开发者不对因使用本项目导致的账号封禁、API 限制等后果承担责任。
