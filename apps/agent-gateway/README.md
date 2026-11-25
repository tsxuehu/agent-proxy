# agent-gateway · 工具接入网关

提供 Codex Responses 和 Claude Code Messages 接入。按端点鉴权，向转发应用发起内部 gRPC 流。

## 构建与运行

工程可以作为单独的 Go module 构建。直接在本目录执行。

```sh
go test ./...
go build -o bin/agent-gateway .
go run .
docker build -t agent-gateway:dev .
kubectl kustomize deploy/k8s
```

在 monorepo 内验证独立构建时，将环境变量 `GOWORK` 设为 `off`。迁走本目录时一并保留 `deps/shared`，其中包含构建所需的共享源码。

| 环境变量 | 用途 |
| --- | --- |
| `AGENT_MYSQL_DSN` | 共享 MySQL DSN |
| `AGENT_RELAY_TOKEN` | 与转发应用一致的内部服务令牌 |
| `AGENT_FORWARDER_ADDR` | 转发应用 gRPC 地址，默认 `localhost:9090` |

通过 `AGENT_HTTP_ADDR` 覆盖默认监听地址 `:8080`。先启动 `agent-forwarder`，并使用已初始化的 MySQL。Kubernetes 中通过支持 SSE 和 WebSocket 的 HTTPS 入口公开网关。
