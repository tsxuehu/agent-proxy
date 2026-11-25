# agent-admin · 管理 API

管理用户令牌、接入端点、客户端 Key、Codex OAuth 账号和厂商模型 API Key。该工程负责初始化共享 MySQL 表。

## 构建与运行

工程可以作为单独的 Go module 构建。直接在本目录执行。

```sh
go test ./...
go build -o bin/agent-admin .
go run .
docker build -t agent-admin:dev .
kubectl kustomize deploy/k8s
```

在 monorepo 内验证独立构建时，将环境变量 `GOWORK` 设为 `off`。迁走本目录时一并保留 `deps/shared`，其中包含构建所需的共享源码。

| 环境变量 | 用途 |
| --- | --- |
| `AGENT_MYSQL_DSN` | 共享 MySQL DSN |
| `AGENT_ADMIN_TOKEN` | 管理 API 令牌 |
| `AGENT_CREDENTIAL_KEY` | Base64 编码的 32 字节凭据主密钥 |

通过 `AGENT_HTTP_ADDR` 覆盖默认监听地址 `:8081`。创建数据库后运行 `go run . migrate`。Kubernetes 可先渲染 `kubectl kustomize deploy/migrate`，确保初始化 Job 完成后再启动其余应用。
