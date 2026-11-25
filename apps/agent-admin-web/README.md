# agent-admin-web · 系统管理控制台

管理供应商 API Key、Codex 账号和用户令牌。前端资源随 Go 可执行文件发布。

## 构建与运行

工程可以作为单独的 Go module 构建。直接在本目录执行。

```sh
go test ./...
go build -o bin/agent-admin-web .
go run .
docker build -t agent-admin-web:dev .
kubectl kustomize deploy/k8s
```

在 monorepo 内验证独立构建时，将环境变量 `GOWORK` 设为 `off`。迁走本目录时一并保留 `deps/shared`，其中包含构建所需的共享源码。

| 环境变量 | 用途 |
| --- | --- |
| `AGENT_ADMIN_URL` | 管理 API 基础地址，默认 `http://localhost:8081` |

通过 `AGENT_HTTP_ADDR` 覆盖默认监听地址 `:8085`。管理员使用管理 API 令牌登录。通过受限的 HTTPS 入口发布此控制台。
