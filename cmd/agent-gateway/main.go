package main

import (
	"context"
	"net/http"

	relay "github.com/tsxuehu/agent-proxy/api/relay/v1"
	"github.com/tsxuehu/agent-proxy/internal/app"
	"github.com/tsxuehu/agent-proxy/internal/codex"
	"github.com/tsxuehu/agent-proxy/internal/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() { app.Main(run) }

func run(ctx context.Context) error {
	store, err := app.Database()
	if err != nil {
		return err
	}
	defer store.DB.Close()
	token, err := app.Secret("AGENT_RELAY_TOKEN")
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(app.Value("AGENT_FORWARDER_ADDR", "localhost:9090"),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(codex.MaxBody+65536), grpc.MaxCallRecvMsgSize(codex.MaxEvent+65536)))
	if err != nil {
		return err
	}
	defer conn.Close()
	mux := http.NewServeMux()
	app.Probes(mux, store.Ready)
	gateway.Server{Store: store, Relay: relay.NewRelayClient(conn), Token: token}.Register(mux)
	return app.Serve(ctx, app.Value("AGENT_HTTP_ADDR", ":8080"), app.Draining(ctx, mux))
}
