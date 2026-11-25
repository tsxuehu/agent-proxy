package main

import (
	"context"

	"github.com/tsxuehu/agent-proxy/apps/agent-portal/internal/portal"
	"github.com/tsxuehu/agent-proxy/shared/app"
)

func main() {
	app.Main(func(ctx context.Context) error { return portal.Run(ctx) })
}
