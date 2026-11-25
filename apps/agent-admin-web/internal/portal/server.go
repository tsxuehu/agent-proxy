package portal

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/tsxuehu/agent-proxy/shared/app"
)

//go:embed index.html assets/*
var files embed.FS

var page = template.Must(template.ParseFS(files, "index.html"))

func Run(ctx context.Context) error {
	target, err := url.Parse(app.Value("AGENT_ADMIN_URL", "http://localhost:8081"))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" ||
		target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Path != "" && target.Path != "/") {
		return errors.New("AGENT_ADMIN_URL must be an HTTP(S) origin")
	}
	gateway, err := url.Parse(app.Value("AGENT_GATEWAY_PUBLIC_URL", "http://localhost:8080"))
	if err != nil || (gateway.Scheme != "http" && gateway.Scheme != "https") || gateway.Host == "" ||
		gateway.User != nil || gateway.RawQuery != "" || gateway.Fragment != "" {
		return errors.New("AGENT_GATEWAY_PUBLIC_URL must be an HTTP(S) base URL")
	}
	handler := console(target, strings.TrimRight(gateway.String(), "/"))
	return app.Serve(ctx, app.Value("AGENT_HTTP_ADDR", ":8085"), app.Draining(ctx, handler))
}

func console(target *url.URL, gateway string) http.Handler {
	const prefix = "/admin/v1/"
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			suffix := strings.TrimPrefix(request.In.URL.Path, "/api/")
			request.SetURL(target)
			request.Out.URL.Path = prefix + suffix
			request.Out.URL.RawPath = ""
		},
	}
	mux := http.NewServeMux()
	app.Probes(mux)
	mux.Handle("GET /assets/", http.FileServerFS(files))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		data := struct {
			Gateway string
		}{gateway}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, data)
	})
	mux.Handle("/api/{operation...}", proxy)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			for _, part := range strings.Split(r.URL.Path, "/") {
				if part == "." || part == ".." {
					http.Error(w, "invalid path", 400)
					return
				}
			}
		}
		mux.ServeHTTP(w, r)
	})
}
