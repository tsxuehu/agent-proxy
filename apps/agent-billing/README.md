# agent-billing · 计费占位

当前提供 `/healthz` 和 `/readyz`。服务不依赖 MySQL、Redis 或其他应用。

## 构建与运行

工程可以作为单独的 Go module 构建。直接在本目录执行。

```sh
go test ./...
go build -o bin/agent-billing .
go run .
docker build -t agent-billing:dev .
kubectl kustomize deploy/k8s
```

在 monorepo 内验证独立构建时，将环境变量 `GOWORK` 设为 `off`。迁走本目录时一并保留 `deps/shared`，其中包含构建所需的共享源码。

通过 `AGENT_HTTP_ADDR` 覆盖默认监听地址 `:8083`。计费、额度预检和账单处理留到后续。
