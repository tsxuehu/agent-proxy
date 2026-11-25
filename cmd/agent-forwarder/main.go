package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	relay "github.com/tsxuehu/agent-proxy/api/relay/v1"
	"github.com/tsxuehu/agent-proxy/internal/app"
	"github.com/tsxuehu/agent-proxy/internal/codex"
	"github.com/tsxuehu/agent-proxy/internal/forwarder"
	"github.com/tsxuehu/agent-proxy/internal/keys"
	"google.golang.org/grpc"
)

func main() { app.Main(run) }

func run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	store, err := app.Database()
	if err != nil {
		return err
	}
	defer store.DB.Close()
	cache, err := app.Redis()
	if err != nil {
		return err
	}
	defer cache.Close()
	token, err := app.Secret("AGENT_RELAY_TOKEN")
	if err != nil {
		return err
	}
	vault, err := keys.NewVault(os.Getenv("AGENT_CREDENTIAL_KEY"))
	if err != nil {
		return err
	}
	userLimit, err := limit("AGENT_USER_CONCURRENCY", "4")
	if err != nil {
		return err
	}
	accountLimit, err := limit("AGENT_ACCOUNT_CONCURRENCY", "2")
	if err != nil {
		return err
	}
	baseURL, err := trustedURL("AGENT_CODEX_BASE_URL", codex.BaseURL)
	if err != nil {
		return err
	}
	tokenURL, err := trustedURL("AGENT_CODEX_TOKEN_URL", codex.TokenURL)
	if err != nil {
		return err
	}
	usageURL, err := trustedURL("AGENT_CODEX_USAGE_URL", codex.UsageURL)
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.MaxIdleConnsPerHost = 32
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	svc := &forwarder.Server{Store: store, Cache: cache, Vault: vault, Token: token, HTTP: client, BaseURL: baseURL, UsageURL: usageURL,
		Tokens: codex.TokenClient{URL: tokenURL, HTTP: client}, UserLimit: userLimit, AccountLimit: accountLimit}
	listener, err := net.Listen("tcp", app.Value("AGENT_GRPC_ADDR", ":9090"))
	if err != nil {
		return err
	}
	defer listener.Close()
	server := grpc.NewServer(grpc.MaxRecvMsgSize(codex.MaxBody+65536), grpc.MaxSendMsgSize(codex.MaxEvent+65536))
	relay.RegisterRelayServer(server, svc)
	go svc.Maintain(ctx)
	mux := http.NewServeMux()
	app.Probes(mux, store.Ready, func(ctx context.Context) error { return cache.Ping(ctx).Err() })
	finished := make(chan error, 2)
	go func() { finished <- server.Serve(listener) }()
	go func() { finished <- app.Serve(ctx, app.Value("AGENT_HTTP_ADDR", ":8082"), app.Draining(ctx, mux)) }()
	select {
	case err := <-finished:
		if ctx.Err() == nil {
			server.Stop()
			return err
		}
	case <-ctx.Done():
	}
	drained := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(25 * time.Second):
		server.Stop()
	}
	return nil
}

func limit(name, fallback string) (int, error) {
	value, err := strconv.Atoi(app.Value(name, fallback))
	if err != nil || value <= 0 || value > 10000 {
		return 0, errors.New(name + " must be between 1 and 10000")
	}
	return value, nil
}

func trustedURL(name, fallback string) (string, error) {
	value, err := url.Parse(app.Value(name, fallback))
	if err != nil || value.Host == "" || value.User != nil || value.RawQuery != "" || value.Fragment != "" ||
		(value.Scheme != "https" && value.Scheme != "http") {
		return "", errors.New(name + " must be an HTTP(S) URL without credentials, query or fragment")
	}
	return strings.TrimRight(value.String(), "/"), nil
}
