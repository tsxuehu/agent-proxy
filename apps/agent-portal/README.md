# agent-portal · 用户控制台

注册、登录和管理自己的接入端点、模型映射、上游与端点 Key。前端资源随 Go 可执行文件发布。

## 构建与运行

工程可以作为单独的 Go module 构建。直接在本目录执行。

```sh
go test ./...
go build -o bin/agent-portal .
go run .
docker build -t agent-portal:dev .
kubectl kustomize deploy/k8s
```

在 monorepo 内验证独立构建时，将环境变量 `GOWORK` 设为 `off`。迁走本目录时一并保留 `deps/shared`，其中包含构建所需的共享源码。

| 环境变量 | 用途 |
| --- | --- |
| `AGENT_ADMIN_URL` | 管理 API 基础地址，默认 `http://localhost:8081` |
| `AGENT_GATEWAY_PUBLIC_URL` | 用户可访问的推理网关地址，默认 `http://localhost:8080` |

通过 `AGENT_HTTP_ADDR` 覆盖默认监听地址 `:8084`。用户可使用用户名和密码或管理 API 签发的控制台令牌登录。管理 API 配置 `AGENT_ALLOW_SIGNUP=true` 时显示注册入口。部署 Kubernetes 时为用户中心配置 HTTPS，并将推理网关地址设置为用户实际访问的 URL。
