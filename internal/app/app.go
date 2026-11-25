package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tsxuehu/agent-proxy/internal/storage"
)

func Context() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func Value(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func Secret(name string) (string, error) {
	value := os.Getenv(name)
	if len(value) < 24 || strings.ContainsAny(value, " \t\r\n") {
		return "", errors.New(name + " must be a secret of at least 24 characters without whitespace")
	}
	return value, nil
}

func Database() (*storage.Store, error) { return storage.Open(os.Getenv("AGENT_MYSQL_DSN")) }

func Redis() (*redis.Client, error) {
	opts, err := redis.ParseURL(Value("AGENT_REDIS_URL", "redis://localhost:6379/0"))
	if err != nil {
		return nil, errors.New("invalid AGENT_REDIS_URL")
	}
	opts.DialTimeout = 3 * time.Second
	opts.ReadTimeout = 3 * time.Second
	opts.WriteTimeout = 3 * time.Second
	return redis.NewClient(opts), nil
}

func Probes(mux *http.ServeMux, dependencies ...func(context.Context) error) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for _, check := range dependencies {
			if check(ctx) != nil {
				http.Error(w, "dependency unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}

func Serve(ctx context.Context, address string, handler http.Handler) error {
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second}
	finished := make(chan error, 1)
	go func() { finished <- server.ListenAndServe() }()
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		drain, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if err := server.Shutdown(drain); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}

func Main(run func(context.Context) error) {
	ctx, cancel := Context()
	defer cancel()
	if err := run(ctx); err != nil {
		slog.Error("service exited", "error", err)
		os.Exit(1)
	}
}

func Draining(ctx context.Context, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ctx.Err() != nil && r.URL.Path != "/healthz" {
			http.Error(w, "instance draining", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

func Bearer(header string) string {
	scheme, token, ok := strings.Cut(header, " ")
	if ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(token)
	}
	return ""
}
