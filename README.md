# Agent Proxy

面向 AI 编程工具的自托管代理。首版通过 Codex OAuth 账号池转发 Codex CLI 和 Claude Code，请求按用户端点路由。

## Monorepo 工程

| 入口 | 职责 | 默认端口 |
| --- | --- | --- |
| `agent-portal` | 用户控制台，创建和管理自己的接入端点和 Key | 8084 |
| `agent-admin-web` | 系统管理控制台，管理用户令牌、Codex 账号和模型 API Key | 8085 |
| `agent-admin` | 两个控制台使用的管理 API，以及数据库初始化 | 8081 |
| `agent-gateway` | Codex Responses 和 Claude Code Messages 协议入口 | 8080 |
| `agent-forwarder` | 内部 gRPC 转发、Codex 调度和续期 | 9090，探针 8082 |
| `agent-billing` | 预留的计费应用，提供健康检查 | 8083 |

每个部署单元位于 `apps/<应用名>/`，拥有自己的 Go module、源码、测试、Dockerfile 和 Kubernetes 清单。根目录的 `go.work` 用于联合开发。根目录 `deploy/` 的 Kustomize 配置组合各应用的清单。

共享协议与基础库的源码位于 `shared/`。各应用在 `deps/shared/` 随附自己的依赖副本，独立构建使用目录内的副本。联合开发通过 `go.work` 直接读取 `shared/`。修改共享源码后运行 `go run ./tools/sync-shared/main.go` 更新副本，将共享变更和对应副本一起提交。使用 `go run ./tools/sync-shared/main.go -check` 检查同步。

迁出时可以单独复制 `apps/<应用名>/`，或者用 `git subtree split --prefix apps/agent-gateway -b split/agent-gateway` 保留该目录的提交历史。迁出的工程可以继续使用随附依赖，运行时仍通过配置连接需要的外部服务。

## 本地启动

需要 Go 1.25.13、MySQL 8.4 和 Redis 7。通过运行环境提供以下变量。

| 变量 | 应用与格式 |
| --- | --- |
| `AGENT_MYSQL_DSN` | admin、gateway、forwarder。Go MySQL DSN，例如 `user:password@tcp(127.0.0.1:3306)/agent_proxy`。先创建数据库。 |
| `AGENT_REDIS_URL` | forwarder，例如 `redis://127.0.0.1:6379/0`。 |
| `AGENT_ADMIN_TOKEN` | admin。随机管理员令牌，至少 24 个字符。系统管理控制台使用该令牌。 |
| `AGENT_RELAY_TOKEN` | gateway、forwarder。独立随机服务令牌，至少 24 个字符。 |
| `AGENT_CREDENTIAL_KEY` | admin、forwarder。Base64 编码的随机 32 字节主密钥。与 MySQL 备份分开保存。 |
| `AGENT_FORWARDER_ADDR` | gateway。默认 `localhost:9090`。 |
| `AGENT_ADMIN_URL` | 两个控制台。默认 `http://localhost:8081`。 |
| `AGENT_GATEWAY_PUBLIC_URL` | 用户控制台显示的网关地址。默认 `http://localhost:8080`。 |

```sh
go run ./apps/agent-admin migrate
go run ./apps/agent-admin
```

在另外的终端启动 `agent-forwarder`、`agent-gateway`、`agent-admin-web` 和 `agent-portal`，例如 `go run ./apps/agent-forwarder`。计费占位应用可按需启动。初始数据库结构由 migrate 命令创建。

1. 打开 `http://localhost:8085`，用管理员令牌登录。导入获准使用的 Codex OAuth 凭据 JSON，支持 Codex CLI 的 `auth.json` 或含相同字段的导出文件。代理应是该上游刷新令牌的唯一使用方。
2. 在系统管理控制台为用户签发控制台令牌。令牌只显示一次。替换控制台令牌后，旧令牌失效。
3. 打开 `http://localhost:8084`，用用户令牌登录，创建端点并选择协议、上游和模型映射。保存后复制端点 Key。
4. 用端点 URL 和 Key 配置工具。Codex 使用自定义 Responses provider，Claude Code 使用 Messages 基础地址和端点 Key。具体路径见 [接入 API](docs/architecture.md#4-用户端点与接入-api) 和 [Codex 接入方式](docs/codex-access.md)。

第一版使用管理员导入的 OAuth 凭据并自动续期。厂商模型 Key 加密入库，预留给后续转发能力。Claude Code 支持文本、JSON Schema 函数工具和流式响应，token 计数是估算。WebSocket 支持一条连接上的顺序轮次。已发送业务输出的轮次遇到断流时按协议返回中断。

## Kubernetes

每个应用从自己的工程目录构建镜像。用部署环境的镜像名或 digest 覆盖各 Kustomize 基础清单中的 `:dev` 镜像名。可以单独部署一个应用，也可以使用根目录的 `deploy/k8s` 组合部署。

```sh
docker build -t registry.example.com/agent-gateway:VERSION ./apps/agent-gateway
kubectl kustomize apps/agent-gateway/deploy/k8s
kubectl kustomize deploy/migrate
kubectl kustomize deploy/k8s
```

在目标命名空间创建 `agent-proxy` Secret，键名与上表的数据库、Redis、服务令牌和主密钥一致。先部署并等待迁移 Job 完成，再部署应用。生产 Kustomize overlay 将 `AGENT_GATEWAY_PUBLIC_URL` 设置为实际对外 URL，通过支持 WebSocket 和 SSE 的 TLS Ingress 或 Gateway 暴露推理及用户控制台。系统管理控制台使用受限入口。管理 API 和转发 gRPC 使用集群内 Service，转发 gRPC 默认用 Bearer 认证和 NetworkPolicy，跨信任域部署需要 TLS 传输。

MySQL 和 Redis 由目标环境提供。基础清单不创建公共入口或生产 Secret。Redis 故障转移后的严格全局并发上限、活跃 WebSocket 无损迁移和真实账号长期验收仍属于后续验收范围。

## 验证

```sh
go run ./tools/sync-shared/main.go -check
go test ./apps/agent-admin/... ./apps/agent-gateway/... ./apps/agent-forwarder/... ./apps/agent-billing/... ./apps/agent-portal/... ./apps/agent-admin-web/...
```

在单个应用目录内关闭 workspace 后，可以运行 `go test ./...`、`go vet ./...` 和 `go build .`。跨应用集成测试使用模拟 Codex 和 OAuth 上游。设置 `AGENT_TEST_MYSQL_DSN` 和 `AGENT_TEST_REDIS_URL` 指向专用临时测试库，再运行 `go test -count=1 ./tests/integration`。测试关闭 workspace，独立构建服务并通过 HTTP 和 gRPC 验证组合行为。

[系统架构](docs/architecture.md) · [账号池设计](docs/account-pooling.md)
