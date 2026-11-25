package main

import (
	"context"

	"github.com/tsxuehu/agent-proxy/internal/app"
	"github.com/tsxuehu/agent-proxy/internal/portal"
)

func main() {
	app.Main(func(ctx context.Context) error { return portal.Run(ctx, true) })
}
