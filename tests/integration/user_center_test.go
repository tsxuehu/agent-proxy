package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tsxuehu/agent-proxy/shared/keys"
	"github.com/tsxuehu/agent-proxy/shared/storage"
)

func testUserCenter(t *testing.T, ctx context.Context, store *storage.Store, signupPortal, otherPortal string) {
	const sessionPath = "/api/auth/session"
	const registerPath = "/api/auth/signup"
	const endpointPath = "/api/endpoints"
	const endpointInput = `{"protocol":"responses","backend":"codex-pool","models":{"alias":"chosen-model"}}`
	username := strings.ToLower(keys.New("user_"))
	otherUser := strings.ToLower(keys.New("user_"))
	password := keys.New("test-password-")
	creds := func(name, password string) string {
		return fmt.Sprintf(`{"username":%q,"password":%q}`, name, password)
	}
	t.Cleanup(func() {
		_, _ = store.DB.Exec(`DELETE FROM endpoints WHERE user_id IN (SELECT id FROM portal_accounts WHERE username IN (?,?))`, username, otherUser)
		_, _ = store.DB.Exec(`DELETE FROM portal_accounts WHERE username IN (?,?)`, username, otherUser)
	})

	call := func(method, target, body string, cookie *http.Cookie, want int) ([]byte, []*http.Cookie) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s %s returned HTTP %d, want %d (%v)", method, req.URL.Path, resp.StatusCode, want, err)
		}
		return payload, resp.Cookies()
	}
	sessionCookie := func(cookies []*http.Cookie) *http.Cookie {
		t.Helper()
		if len(cookies) != 1 {
			t.Fatal("expected one session cookie")
		}
		cookie := cookies[0]
		if !cookie.Secure || !cookie.HttpOnly || cookie.Domain != "" || cookie.Path != "/" ||
			cookie.SameSite != http.SameSiteStrictMode || !strings.HasPrefix(cookie.Name, "__Host-") || cookie.MaxAge <= 0 {
			t.Fatal("session cookie attributes are incorrect")
		}
		return cookie
	}

	call(http.MethodPost, otherPortal+registerPath, creds(username, password), nil, 403)
	call(http.MethodPost, signupPortal+registerPath, creds(username, "too short"), nil, 400)
	body, cookies := call(http.MethodPost, signupPortal+registerPath, creds(strings.ToUpper(username), password), nil, 201)
	if field(t, body, "username") != username {
		t.Fatal("expected a normalized username")
	}
	first := sessionCookie(cookies)
	call(http.MethodPost, signupPortal+registerPath, creds(username, password), nil, 409)
	call(http.MethodPost, otherPortal+sessionPath, creds(username, keys.New("wrong-")), nil, 401)
	call(http.MethodGet, otherPortal+sessionPath, "", first, 200)
	body, _ = call(http.MethodPost, otherPortal+endpointPath, endpointInput, first, 201)
	endpointID := field(t, body, "id")
	_, otherCookies := call(http.MethodPost, signupPortal+registerPath, creds(otherUser, password), nil, 201)
	other := sessionCookie(otherCookies)
	call(http.MethodPost, otherPortal+endpointPath+"/"+endpointID+"/rotate-key", "", other, 404)
	body, _ = call(http.MethodGet, signupPortal+endpointPath, "", other, 200)
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatal("another user can see the first user's endpoints")
	}
	_, cookies = call(http.MethodPost, otherPortal+sessionPath, creds(username, password), first, 200)
	second := sessionCookie(cookies)
	call(http.MethodGet, signupPortal+endpointPath, "", first, 401)
	call(http.MethodGet, signupPortal+endpointPath, "", second, 200)
	_, cookies = call(http.MethodDelete, signupPortal+sessionPath, "", second, 204)
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Fatal("logout did not clear the session cookie")
	}
	call(http.MethodGet, otherPortal+endpointPath, "", second, 401)
	digest := keys.Digest(other.Value)
	if _, err := store.DB.ExecContext(ctx, `UPDATE portal_sessions SET expires_at=UTC_TIMESTAMP(6) WHERE token_hash=?`, digest[:]); err != nil {
		t.Fatal(err)
	}
	call(http.MethodGet, otherPortal+endpointPath, "", other, 401)
}
