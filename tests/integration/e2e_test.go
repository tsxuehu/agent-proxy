package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsxuehu/agent-proxy/shared/keys"
	"github.com/tsxuehu/agent-proxy/shared/storage"
)

func TestAgentProxyWithMySQLAndRedis(t *testing.T) {
	dsn, redisURL := os.Getenv("AGENT_TEST_MYSQL_DSN"), os.Getenv("AGENT_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("set AGENT_TEST_MYSQL_DSN and AGENT_TEST_REDIS_URL to dedicated test databases")
	}
	store, err := storage.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	binaries := buildApps(t, ctx, "agent-admin", "agent-forwarder", "agent-gateway", "agent-portal", "agent-admin-web")
	migrate := exec.CommandContext(ctx, binaries["agent-admin"], "migrate")
	migrate.Env = append(os.Environ(), "AGENT_MYSQL_DSN="+dsn)
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("initialize database: %v\n%s", err, output)
	}
	var refreshes atomic.Int32
	var accountHits atomic.Int32
	cancelObserved := make(chan struct{}, 1)
	upstreamMux := http.NewServeMux()
	upstreamMux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"rotated","expires_in":3600}`)
	})
	upstreamMux.HandleFunc("POST /responses/compact", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmp_1","object":"response.compaction","output":[]}`)
	})
	upstreamMux.HandleFunc("GET /usage", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	const completed = `{"type":"response.completed","response":{"id":"resp_1","output":[{"id":"item_1","type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":2,"output_tokens":1}}}`
	upstreamMux.HandleFunc("POST /responses", func(w http.ResponseWriter, r *http.Request) {
		accountHits.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(string(raw), "cancel-me") {
			<-r.Context().Done()
			cancelObserved <- struct{}{}
			return
		}
		if r.Header.Get("Authorization") == "Bearer fail-access" {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","resets_in_seconds":60}}`)
			return
		}
		if strings.Contains(string(raw), "post-output-limit") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"+
				"data: {\"type\":\"error\",\"status\":429,\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":60}}\n\n")
			return
		}
		if !strings.Contains(string(raw), `"stream":true`) {
			t.Error("stream was not normalized")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+completed+"\n\n")
	})
	upgrader := websocket.Upgrader{}
	upstreamMux.HandleFunc("GET /responses", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if conn.WriteMessage(websocket.TextMessage, []byte(completed)) != nil {
				return
			}
		}
	})
	upstream := httptest.NewServer(upstreamMux)
	t.Cleanup(upstream.Close)
	adminToken, relayToken := keys.New("admin_"), keys.New("relay_")
	env := []string{"AGENT_MYSQL_DSN=" + dsn, "AGENT_REDIS_URL=" + redisURL, "AGENT_ADMIN_TOKEN=" + adminToken,
		"AGENT_RELAY_TOKEN=" + relayToken, "AGENT_CREDENTIAL_KEY=" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{43}, 32)),
		"AGENT_CODEX_BASE_URL=" + upstream.URL, "AGENT_CODEX_TOKEN_URL=" + upstream.URL + "/token",
		"AGENT_CODEX_USAGE_URL=" + upstream.URL + "/usage", "AGENT_ACCOUNT_CONCURRENCY=4"}
	adminURL := runApp(t, ctx, binaries["agent-admin"], append(append([]string(nil), env...), "AGENT_ALLOW_SIGNUP=true"))
	webEnv := append(append([]string(nil), env...), "AGENT_ADMIN_URL="+adminURL)
	adminConsole := runApp(t, ctx, binaries["agent-admin-web"], webEnv)
	userConsole := runApp(t, ctx, binaries["agent-portal"], webEnv)
	secondAdminURL := runApp(t, ctx, binaries["agent-admin"], append(append([]string(nil), env...), "AGENT_ALLOW_SIGNUP=false"))
	secondConsole := runApp(t, ctx, binaries["agent-portal"], append(append([]string(nil), env...), "AGENT_ADMIN_URL="+secondAdminURL))
	t.Run("user center", func(t *testing.T) { testUserCenter(t, ctx, store, userConsole, secondConsole) })
	newGateway := func() string {
		t.Helper()
		relayAddress := availableAddress(t)
		pair := append(append([]string(nil), env...), "AGENT_GRPC_ADDR="+relayAddress, "AGENT_FORWARDER_ADDR="+relayAddress)
		runApp(t, ctx, binaries["agent-forwarder"], pair)
		return runApp(t, ctx, binaries["agent-gateway"], pair)
	}
	first, second := newGateway(), newGateway()
	fire := func(method, path, token, body string, code int) []byte {
		t.Helper()
		if rest, ok := strings.CutPrefix(path, "/admin/v1/"); ok {
			return request(t, ctx, method, adminConsole+"/api/"+rest, token, body, code)
		}
		return request(t, ctx, method, userConsole+strings.Replace(path, "/user/v1/", "/api/", 1), token, body, code)
	}
	addUser := func(name string) string {
		t.Helper()
		raw := fire("POST", "/admin/v1/users/token", adminToken, fmt.Sprintf(`{"user_id":%q}`, name), 200)
		return field(t, raw, "token")
	}
	userName := keys.New("alice_")
	userToken := addUser(userName)
	otherUserToken := addUser(keys.New("bob_"))
	makeEndpoint := func(protocol string) (id, token string) {
		t.Helper()
		body := fmt.Sprintf(`{"protocol":%q,"backend":"codex-pool","models":{"alias":"chosen-model"}}`, protocol)
		raw := fire("POST", "/user/v1/endpoints", userToken, body, 201)
		return field(t, raw, "id"), field(t, raw, "key")
	}
	id, endpointKey := makeEndpoint("responses")
	messagesID, messagesKey := makeEndpoint("messages")
	fire("POST", "/user/v1/endpoints/"+id+"/rotate-key", otherUserToken, "", 404)
	fire("POST", "/admin/v1/model-keys", userToken, `{}`, 401)
	apiKey := keys.New("provider_")
	result := fire("POST", "/admin/v1/model-keys", adminToken, fmt.Sprintf(`{"provider":"openai","label":"fake","key":%q}`, apiKey), 201)
	keyID := field(t, result, "id")
	list := fire("GET", "/admin/v1/model-keys", adminToken, "", 200)
	if bytes.Contains(list, []byte(apiKey)) {
		t.Fatal("provider key returned to browser")
	}
	fire("DELETE", "/admin/v1/model-keys/"+keyID, adminToken, "", 204)
	firstID, secondID := keys.New("first_"), keys.New("second_")
	t.Cleanup(func() {
		_, _ = store.DB.Exec("DELETE FROM codex_accounts WHERE id IN (?,?)", firstID, secondID)
	})
	importAccount := func(accountID, access string, expires time.Time) {
		t.Helper()
		body := fmt.Sprintf(`{"tokens":{"account_id":%q,"access_token":%q,"refresh_token":"old-refresh","expires_at":%q}}`, accountID, access, expires.UTC().Format(time.RFC3339))
		result := fire("POST", "/admin/v1/codex/accounts/import", adminToken, body, 200)
		if bytes.Contains(result, []byte("old-refresh")) {
			t.Fatal("refresh token returned to browser")
		}
	}
	importAccount(firstID, "fail-access", time.Now().Add(time.Hour))
	importAccount(secondID, "stale-access", time.Now().Add(-time.Minute))
	if err := store.ObserveQuota(ctx, firstID, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveQuota(ctx, secondID, 0.7, time.Now()); err != nil {
		t.Fatal(err)
	}
	responsePath := "/e/" + id + "/v1/responses"
	request(t, ctx, "GET", first+"/e/"+id+"/v1/models", messagesKey, "", 401)
	request(t, ctx, "GET", first+"/e/"+id+"/v1/models", endpointKey, "", 200)
	request(t, ctx, "GET", first+responsePath, endpointKey, "", 426)
	result = request(t, ctx, "POST", first+responsePath, endpointKey, `{"model":"alias","input":"first","stream":true}`, 200)
	if !bytes.Contains(result, []byte("response.completed")) || accountHits.Load() != 2 {
		t.Fatalf("expected failover and SSE completion, got %d attempts", accountHits.Load())
	}
	if refreshes.Load() != 1 {
		t.Fatal("expected one token rotation")
	}
	if err := store.Cool(ctx, secondID, "chosen-model", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	result = request(t, ctx, "POST", second+responsePath, endpointKey, `{"model":"alias","input":"rate limited","stream":true}`, 429)
	if !bytes.Contains(result, []byte("quota")) {
		t.Fatalf("expected exhausted quota: %s", result)
	}
	if _, err := store.DB.ExecContext(ctx, "DELETE FROM model_cooldowns WHERE account_id=?", secondID); err != nil {
		t.Fatal(err)
	}
	importAccount(secondID, "stale-access", time.Now().Add(-time.Minute))
	var concurrent sync.WaitGroup
	for _, server := range []string{first, second} {
		concurrent.Add(1)
		go func(server string) {
			defer concurrent.Done()
			request(t, ctx, "POST", server+responsePath, endpointKey, `{"model":"alias","input":"parallel requests"}`, 200)
		}(server)
	}
	concurrent.Wait()
	if refreshes.Load() != 2 {
		t.Fatalf("cross-replica refresh was not deduplicated: %d total", refreshes.Load())
	}
	result = request(t, ctx, "POST", second+responsePath+"/compact", endpointKey, `{"model":"alias","input":"compact","previous_response_id":"resp_1"}`, 200)
	if !bytes.Contains(result, []byte("response.compaction")) {
		t.Fatal("missing compact response")
	}
	result = request(t, ctx, "POST", second+"/e/"+messagesID+"/v1/messages", messagesKey,
		`{"model":"alias","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"stream":true}`, 200)
	if !bytes.Contains(result, []byte("event: message_stop")) || !bytes.Contains(result, []byte("hello")) {
		t.Fatal("missing Messages stream")
	}
	wsURL := "ws" + strings.TrimPrefix(first, "http") + responsePath
	header := http.Header{"Authorization": {"Bearer " + endpointKey}}
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, header)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for _, turn := range []string{
		`{"type":"response.create","model":"alias","input":"first WS turn"}`,
		`{"type":"response.create","input":"second WS turn","previous_response_id":"resp_1"}`,
	} {
		if err := ws.WriteMessage(websocket.TextMessage, []byte(turn)); err != nil {
			t.Fatal(err)
		}
		_, raw, err := ws.ReadMessage()
		if err != nil || !bytes.Contains(raw, []byte("response.completed")) {
			t.Fatalf("WebSocket failed: %s %v", raw, err)
		}
	}
	_ = ws.Close()
	child, stop := context.WithTimeout(ctx, 250*time.Millisecond)
	defer stop()
	req, err := http.NewRequestWithContext(child, "POST", first+responsePath, strings.NewReader(`{"model":"alias","input":"cancel-me"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+endpointKey)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected canceled client request")
	}
	select {
	case <-cancelObserved:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not receive cancellation")
	}
	before := accountHits.Load()
	result = request(t, ctx, "POST", second+responsePath, endpointKey,
		`{"model":"alias","input":"post-output-limit","stream":true}`, 200)
	if !bytes.Contains(result, []byte("partial")) || !bytes.Contains(result, []byte("usage_limit_reached")) || accountHits.Load() != before+1 {
		t.Fatal("a 429 committed to the client was not forwarded as exactly one attempt")
	}
}

func buildApps(t *testing.T, ctx context.Context, names ...string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	binaries := make(map[string]string, len(names))
	for _, name := range names {
		binary := filepath.Join(dir, name+".exe")
		build := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", binary, ".")
		build.Dir = filepath.Join("..", "..", "apps", name)
		build.Env = append(os.Environ(), "GOWORK=off")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, output)
		}
		binaries[name] = binary
	}
	return binaries
}

func availableAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

func runApp(t *testing.T, ctx context.Context, binary string, env []string) string {
	t.Helper()
	address := availableAddress(t)
	command := exec.CommandContext(ctx, binary)
	command.Env = append(append(os.Environ(), env...), "AGENT_HTTP_ADDR="+address)
	var log bytes.Buffer
	command.Stderr = &log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		<-done
		if t.Failed() && log.Len() != 0 {
			t.Logf("%s: %s", filepath.Base(binary), log.String())
		}
	})
	client := &http.Client{Timeout: time.Second}
	baseURL := "http://" + address
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		response, err := client.Get(baseURL + "/readyz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return baseURL
			}
		}
		select {
		case <-done:
			t.Fatalf("%s exited during startup", filepath.Base(binary))
		case <-deadline.C:
			t.Fatalf("%s did not become ready", filepath.Base(binary))
		case <-time.After(40 * time.Millisecond):
		}
	}
}

func request(t *testing.T, ctx context.Context, method, url, token, body string, code int) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		t.Error(err)
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return nil
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != code {
		t.Errorf("%s got HTTP %d, want %d: %s (%v)", method, response.StatusCode, code, raw, err)
	}
	return raw
}

func field(t *testing.T, raw []byte, name string) string {
	t.Helper()
	var payload map[string]json.RawMessage
	var result string
	if json.Unmarshal(raw, &payload) != nil || json.Unmarshal(payload[name], &result) != nil || result == "" {
		t.Fatalf("missing response field %s", name)
	}
	return result
}
