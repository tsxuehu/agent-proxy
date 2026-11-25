package portal

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestConsoleAppRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
	}))
	t.Cleanup(upstream.Close)
	adminURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	app := console(adminURL, "https://proxy.example.com")
	page := httptest.NewRecorder()
	app.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), "用户控制台") || !strings.Contains(page.Body.String(), "https://proxy.example.com") {
		t.Fatal("console page did not render")
	}
	asset := httptest.NewRecorder()
	app.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/assets/console.js", nil))
	if asset.Code != 200 || !strings.Contains(asset.Body.String(), "loadEndpoints") {
		t.Fatal("console JavaScript asset is unavailable")
	}
	api := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/endpoints?scope=all", nil)
	req.Header.Set("Authorization", "Bearer test")
	app.ServeHTTP(api, req)
	if api.Body.String() != "/user/v1/"+"endpoints?scope=all Bearer test" || api.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("console request was routed incorrectly: %s", api.Body.String())
	}
}
