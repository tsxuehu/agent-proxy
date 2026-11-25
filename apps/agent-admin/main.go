package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/tsxuehu/agent-proxy/apps/agent-admin/internal/admin"
	"github.com/tsxuehu/agent-proxy/shared/app"
	"github.com/tsxuehu/agent-proxy/shared/keys"
	"github.com/tsxuehu/agent-proxy/shared/storage"
)

func main() { app.Main(run) }

func run(ctx context.Context) error {
	store, err := storage.Open(os.Getenv("AGENT_MYSQL_DSN"))
	if err != nil {
		return err
	}
	defer store.DB.Close()
	if len(os.Args) == 2 && os.Args[1] == "migrate" {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return store.Migrate(ctx)
	}
	token, err := app.Secret("AGENT_ADMIN_TOKEN")
	if err != nil {
		return err
	}
	vault, err := keys.NewVault(os.Getenv("AGENT_CREDENTIAL_KEY"))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	app.Probes(mux, store.Ready)
	admin.Server{Store: store, Token: token, Vault: vault}.Register(mux)
	return app.Serve(ctx, app.Value("AGENT_HTTP_ADDR", ":8081"), app.Draining(ctx, mux))
}
