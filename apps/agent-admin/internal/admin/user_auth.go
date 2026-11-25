package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
	"github.com/tsxuehu/agent-proxy/shared/keys"
	"github.com/tsxuehu/agent-proxy/shared/storage"
	"golang.org/x/crypto/argon2"
)

const (
	sessionCookie = "__Host-agent-proxy-session"
	sessionTTL    = 7 * 24 * 60 * 60
	saltLength    = 16
	hashLength    = 32
	argon2Version = 1
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{2,31}$`)

type UserAuth struct {
	redis       *redis.Client
	allowSignup bool
	hashSlots   chan struct{}
}

func NewUserAuth(client *redis.Client, allowSignup bool) *UserAuth {
	return &UserAuth{redis: client, allowSignup: allowSignup, hashSlots: make(chan struct{}, 2)}
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func readCredentials(w http.ResponseWriter, r *http.Request) (credentials, bool) {
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var input credentials
	if mediaType != "application/json" {
		problem(w, http.StatusUnsupportedMediaType, "application/json required")
		return input, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !decode(w, r, &input) {
		return input, false
	}
	input.Username = strings.ToLower(strings.TrimSpace(input.Username))
	if !usernamePattern.MatchString(input.Username) || len(input.Password) > 256 || utf8.RuneCountInString(input.Password) < 12 {
		problem(w, http.StatusBadRequest, "invalid username or password format")
		return input, false
	}
	return input, true
}

func (s Server) signup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.Auth.allowSignup {
		problem(w, http.StatusForbidden, "registration disabled")
		return
	}
	input, ok := readCredentials(w, r)
	if !ok || !s.Auth.allow(w, r.Context(), "signup", 12, 60) || !s.Auth.acquire(w) {
		return
	}
	defer s.Auth.release()
	account := storage.PortalAccount{ID: strings.ToLower(keys.New("acct_")), Username: input.Username}
	token := keys.New("aps_")
	err := s.Store.CreatePortalAccount(r.Context(), account, hashPassword(input.Password), keys.Digest(token), sessionTTL)
	if errors.Is(err, storage.ErrConflict) {
		problem(w, http.StatusConflict, "username already in use")
		return
	}
	if err != nil {
		writeStorageError(w, err)
		return
	}
	setSession(w, token, sessionTTL)
	s.writeSession(w, http.StatusCreated, account.Username)
}

func (s Server) login(w http.ResponseWriter, r *http.Request) {
	input, ok := readCredentials(w, r)
	if !ok || !s.Auth.allow(w, r.Context(), "login", 120, 60) ||
		!s.Auth.allow(w, r.Context(), "login:"+input.Username, 12, 600) || !s.Auth.acquire(w) {
		return
	}
	defer s.Auth.release()
	account, hash, err := s.Store.PortalPassword(r.Context(), input.Username)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeStorageError(w, err)
		return
	}
	if !passwordMatches(input.Password, hash) || err != nil {
		problem(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	token := keys.New("aps_")
	if err := s.Store.CreatePortalSession(r.Context(), account.ID, keys.Digest(token), keys.Digest(sessionToken(r)), sessionTTL); err != nil {
		writeStorageError(w, err)
		return
	}
	setSession(w, token, sessionTTL)
	s.writeSession(w, http.StatusOK, account.Username)
}

func (s Server) session(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var username string
	if token := sessionToken(r); token != "" {
		account, err := s.Store.PortalSession(r.Context(), keys.Digest(token))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeStorageError(w, err)
			return
		}
		username = account.Username
	}
	s.writeSession(w, http.StatusOK, username)
}

func (s Server) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if token := sessionToken(r); token != "" {
		if err := s.Store.DeletePortalSession(r.Context(), keys.Digest(token)); err != nil {
			writeStorageError(w, err)
			return
		}
	}
	setSession(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) writeSession(w http.ResponseWriter, code int, username string) {
	write(w, code, struct {
		Username      string `json:"username,omitempty"`
		SignupEnabled bool   `json:"signup_enabled"`
	}{username, s.Auth.allowSignup})
}

func sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || !strings.HasPrefix(cookie.Value, "aps_") || len(cookie.Value) > 128 {
		return ""
	}
	return cookie.Value
}

func setSession(w http.ResponseWriter, token string, ttl int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: ttl,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func hashPassword(password string) []byte {
	salt := make([]byte, saltLength)
	_, _ = rand.Read(salt)
	return append(append([]byte{argon2Version}, salt...), derivePassword(password, salt)...)
}

func passwordMatches(password string, hash []byte) bool {
	valid := len(hash) == 1+saltLength+hashLength && hash[0] == argon2Version
	if !valid {
		// Do the same password work for accounts that do not exist.
		hash = make([]byte, 1+saltLength+hashLength)
	}
	derived := derivePassword(password, hash[1:1+saltLength])
	return subtle.ConstantTimeCompare(derived, hash[1+saltLength:]) == 1 && valid
}

func derivePassword(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, hashLength)
}

var countAttempts = redis.NewScript(`
  local count = redis.call('INCR', KEYS[1])
  if count == 1 then redis.call('EXPIRE', KEYS[1], ARGV[1]) end
  return count
`)

func (a *UserAuth) allow(w http.ResponseWriter, ctx context.Context, bucket string, limit, seconds int) bool {
	count, err := countAttempts.Run(ctx, a.redis, []string{"agent-proxy:portal-auth:" + bucket}, seconds).Int()
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "authentication temporarily unavailable")
		return false
	}
	if count > limit {
		problem(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return false
	}
	return true
}

func (a *UserAuth) acquire(w http.ResponseWriter) bool {
	select {
	case a.hashSlots <- struct{}{}:
		return true
	default:
		problem(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return false
	}
}

func (a *UserAuth) release() { <-a.hashSlots }
