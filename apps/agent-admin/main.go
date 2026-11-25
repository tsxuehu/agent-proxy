package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
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
	allowSignup, err := strconv.ParseBool(app.Value("AGENT_ALLOW_SIGNUP", "false"))
	if err != nil {
		return errors.New("AGENT_ALLOW_SIGNUP must be true or false")
	}
	redisOptions, err := redis.ParseURL(app.Value("AGENT_REDIS_URL", "redis://localhost:6379/0"))
	if err != nil {
		return errors.New("invalid AGENT_REDIS_URL")
	}
	redisOptions.DialTimeout = 3 * time.Second
	redisOptions.ReadTimeout = 3 * time.Second
	redisOptions.WriteTimeout = 3 * time.Second
	redisClient := redis.NewClient(redisOptions)
	defer redisClient.Close()
	mux := http.NewServeMux()
	app.Probes(mux, store.Ready)
	admin.Server{Store: store, Token: token, Vault: vault, Auth: admin.NewUserAuth(redisClient, allowSignup)}.Register(mux)
	return app.Serve(ctx, app.Value("AGENT_HTTP_ADDR", ":8081"), app.Draining(ctx, mux))
}
