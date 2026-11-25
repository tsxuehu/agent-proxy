package main

import (
	"context"
	"net/http"

	"github.com/tsxuehu/agent-proxy/shared/app"
)

func main() {
	app.Main(func(ctx context.Context) error {
		mux := http.NewServeMux()
		app.Probes(mux)
		return app.Serve(ctx, app.Value("AGENT_HTTP_ADDR", ":8083"), app.Draining(ctx, mux))
	})
}
