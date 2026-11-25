# Codex 接入方式

本文区分 Codex 的客户端入口、认证方式、网络协议和 Agent Proxy 的端点配置。账号选择、额度采样、令牌续期和故障切换见 [Codex 账号池化设计](account-pooling.md)。

## 1. 先区分四个概念

| 概念 | 选项 | 说明 |
| --- | --- | --- |
| 客户端 | Codex CLI、IDE 扩展、桌面端、直接调用 API 的程序 | 客户端决定配置格式和本地能力。 |
| 客户端认证 | ChatGPT 登录、API Key | 官方文档将这两种方式列为本地 Codex 的登录方式。 |
| 请求协议 | Responses HTTP/SSE、Responses WebSocket | 两者都承载 Responses 请求。WebSocket 是长连接传输方式。 |
| Agent Proxy 上游认证 | Codex OAuth 账号、厂商 API Key | 这是代理访问上游时使用的凭据，和调用方访问代理的 Key 分开保存。 |

**账号池化发生在上游认证这一层。** 调用 Agent Proxy 的用户通常只需要一个端点 Key。代理内部再从端点绑定的 Codex OAuth 账号池中选一个账号，并负责续期和冷却。调用方不应拿到池内账号的 refresh token。

## 2. 官方支持的两种登录方式

### 2.1 使用 ChatGPT 登录

本地 Codex CLI、IDE 扩展和桌面端可以打开浏览器完成 ChatGPT 登录。此方式使用 ChatGPT 订阅和工作区权限，适用于有订阅席位的本地使用。Codex cloud 需要 ChatGPT 登录。

在账号池场景中，ChatGPT 登录是**代理到上游的认证方式**。Agent Proxy 为每个获准账号保存自己的 OAuth 凭据，账号之间独立刷新和冷却。调用方可以使用端点 Key 接入代理，不需要分别登录池内账号。

### 2.2 使用 API Key

API Key 按 API 组织和用量计费，和 ChatGPT 订阅额度分开。调用方可以把 Agent Proxy 配置成 Codex 的自定义 Responses provider，用端点 Key 访问代理。这个模式适合多用户、多个端点和统一撤销。

官方配置支持在用户级 `config.toml` 中使用 `model_provider` 和 `model_providers.<id>` 定义自定义 provider。`base_url` 指向 Responses API，`wire_api` 使用 `responses`，`env_key` 指定保存 Key 的环境变量。需要 WebSocket 时，只有端点确实支持，才设置 `supports_websockets = true`。

## 3. Codex 的两种请求传输

### 3.1 Responses HTTP/SSE

客户端通过 HTTP `POST` 发送 Responses 请求。流式响应使用 Server-Sent Events。该方式适合普通请求、脚本、SDK 和不需要长连接的客户端。

Agent Proxy 需要保留 Responses 事件顺序、工具调用、推理事件、终止事件、取消和 `previous_response_id` 语义。端点可以同时支持普通请求和流式请求。

### 3.2 Responses WebSocket

客户端通过 `GET` Upgrade 建立 Responses WebSocket。一个连接可以承载多轮交互，适合需要低延迟和持续会话的编程工具。

Agent Proxy 需要在连接建立时绑定端点和副本，在每一轮重新校验端点版本、权限和并发租约。上游账号切换只能发生在可安全重放的边界。已经提交正文、推理或工具调用后，连接应按协议终止当前轮，由客户端重连或续接。

### 3.3 compact 是操作，不是新的认证方式

`/responses/compact` 用于长会话压缩。它沿用当前端点的认证和上游选择规则。压缩请求优先使用当前会话绑定的账号，只有在协议确认输入可移植时，才允许在同一端点绑定的账号池内切换。

## 4. Agent Proxy 的接入模式

### 4.1 推荐模式：端点 Key + 自定义 Responses provider

这是账号池化的默认模式。调用方只持有一个端点 Key，端点指定要使用的上游。

示例配置如下。 `endpoint_id` 和环境变量名按实际端点生成。

```toml
model_provider = "agent_proxy"

[model_providers.agent_proxy]
name = "Agent Proxy"
base_url = "https://proxy.example.com/e/endpoint_id/v1"
wire_api = "responses"
env_key = "AGENT_PROXY_ENDPOINT_KEY"
supports_websockets = true
```

对应的端点可以配置为：

| 端点 | 协议 | 指定上游 |
| --- | --- | --- |
| `ep_codex_pool` | Responses | Codex OAuth 账号池 |
| `ep_codex_openai` | Responses | OpenAI 官方模型 API |
| `ep_deepseek` | DeepSeek Harness 适配协议 | DeepSeek API，后续接入 |

调用方的 Key 只属于一个端点。端点改绑上游时递增路由版本，已经开始的轮次继续使用旧版本，后续轮次读取新版本。

### 4.2 可选模式：原生 ChatGPT 登录兼容

如果需要让本地 Codex 客户端保留 ChatGPT 登录体验，Agent Proxy 可以提供一组受保护的配置和 OAuth 转发端点，例如：

- `GET /e/{endpoint_id}/config/codex/auth.json`
- `GET /e/{endpoint_id}/config/codex/config.toml`
- `POST /e/{endpoint_id}/oauth/token`

这条路径只用于明确获准的账号和客户端。代理不把账号池中的 refresh token 下发给调用方。客户端拿到的是代理签发的端点凭据，代理再在后端选择和刷新池内账号。此模式需要单独完成目标 Codex CLI 版本的登录、刷新、退出和错误兼容测试，不能只验证 Responses 请求。

### 4.3 直接调用 Responses API

脚本或 SDK 可以直接调用端点：

```http
POST /e/endpoint_id/v1/responses
Authorization: Bearer <endpoint-key>
Content-Type: application/json
```

这种方式不需要 Codex CLI 配置，但调用方需要自己维护 Responses 请求、SSE 解析、工具循环和会话标识。是否支持 WebSocket 由端点能力声明决定。

## 5. 支持矩阵

| 客户端 | ChatGPT 登录 | Agent Proxy 端点 Key | HTTP/SSE | WebSocket |
| --- | --- | --- | --- | --- |
| Codex CLI | 官方本地模式支持 | 推荐 | 支持 | 端点启用后支持 |
| Codex IDE 扩展 | 官方本地模式支持 | 通过自定义 provider 验证 | 支持 | 端点启用后支持 |
| Codex 桌面端 | 官方本地模式支持 | 通过目标版本配置验证 | 支持 | 端点启用后支持 |
| 直接 API 客户端 | 需要自行实现 OAuth | 推荐 | 支持 | 按客户端实现 |

官方认证方式与 Agent Proxy 的端点 Key 是两层概念。调用方可以使用 API Key 访问代理，同时由代理使用 OAuth 账号访问 Codex 上游。代理也可以增加原生 ChatGPT 登录兼容，但这不改变端点、会话和账号池的隔离规则。

## 6. Agent Proxy 的实现顺序

1. 先实现端点 Key、Responses HTTP/SSE、模型目录和 `/responses/compact`。
2. 再实现 Responses WebSocket，覆盖多轮、取消、断流、账号切换和客户端重连。
3. 用 `model_provider` 配置验证 Codex CLI 的自定义 provider，确认端点 Key、模型目录、工具调用和会话续接。
4. 根据目标版本决定是否实现原生 ChatGPT 登录兼容。需要实现时，单独验证配置下发、OAuth 回调、token refresh 和退出。
5. 最后接入 IDE、桌面端和其他编程工具。每种客户端单独记录配置格式、协议能力和兼容性结果。

## 7. 官方参考

- [Codex Authentication](https://developers.openai.com/codex/auth) 说明 ChatGPT 登录和 API Key 两种本地认证方式。
- [Codex Configuration Reference](https://developers.openai.com/codex/config-reference) 说明 `model_provider`、`model_providers.<id>`、`base_url`、`wire_api`、`env_key` 和 `supports_websockets`。
- [Responses streaming](https://developers.openai.com/api/docs/guides/streaming-responses) 说明 Responses 的流式事件。
- [WebSocket mode](https://developers.openai.com/api/docs/guides/websocket-mode) 说明 Responses WebSocket 传输。
