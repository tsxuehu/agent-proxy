# agent-forwarder · 上游转发

接收网关 gRPC 流，处理 Codex 账号选择、凭据续期、会话绑定、冷却和转发。健康检查默认 `8082`，内部 gRPC 默认 `9090`。

## 构建与运行

工程可以作为单独的 Go module 构建。直接在本目录执行。

```sh
go test ./...
go build -o bin/agent-forwarder .
go run .
docker build -t agent-forwarder:dev .
kubectl kustomize deploy/k8s
```

在 monorepo 内验证独立构建时，将环境变量 `GOWORK` 设为 `off`。迁走本目录时一并保留 `deps/shared`，其中包含构建所需的共享源码。

| 环境变量 | 用途 |
| --- | --- |
| `AGENT_MYSQL_DSN` | 共享 MySQL DSN |
| `AGENT_REDIS_URL` | 共享 Redis URL，默认 `redis://localhost:6379/0` |
| `AGENT_RELAY_TOKEN` | 与网关一致的内部服务令牌 |
| `AGENT_CREDENTIAL_KEY` | 与管理 API 一致的 Base64 凭据主密钥 |

通过 `AGENT_HTTP_ADDR` 覆盖默认监听地址 `:8082`。使用已初始化的 MySQL。内部 gRPC 通过集群内 Service 暴露。默认 NetworkPolicy 允许网关 Pod 访问该服务。
