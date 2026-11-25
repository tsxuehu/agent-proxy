package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/tsxuehu/agent-proxy/shared/app"
	"github.com/tsxuehu/agent-proxy/shared/codex"
	"github.com/tsxuehu/agent-proxy/shared/keys"
	"github.com/tsxuehu/agent-proxy/shared/storage"
)

type Server struct {
	Store *storage.Store
	Vault keys.Vault
	Token string
	Auth  *UserAuth
}

var userPattern = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,128}$`)
var modelPattern = regexp.MustCompile(`^[A-Za-z0-9_./:-]{1,128}$`)

func (s Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/v1/endpoints", s.protect(s.createEndpoint))
	mux.HandleFunc("GET /admin/v1/endpoints", s.protect(s.listEndpoints))
	mux.HandleFunc("PUT /admin/v1/endpoints/{id}", s.protect(s.updateEndpoint))
	mux.HandleFunc("POST /admin/v1/endpoints/{id}/rotate-key", s.protect(s.rotateKey))
	mux.HandleFunc("POST /admin/v1/codex/accounts/import", s.protect(s.importAccount))
	mux.HandleFunc("GET /admin/v1/codex/accounts", s.protect(s.listAccounts))
	mux.HandleFunc("PUT /admin/v1/codex/accounts/{id}/enabled", s.protect(s.enableAccount))
	mux.HandleFunc("POST /admin/v1/users/token", s.protect(s.provisionUser))
	mux.HandleFunc("GET /admin/v1/model-keys", s.protect(s.modelKeys))
	mux.HandleFunc("POST /admin/v1/model-keys", s.protect(s.addModelKey))
	mux.HandleFunc("DELETE /admin/v1/model-keys/{id}", s.protect(s.deleteModelKey))
	mux.HandleFunc("POST /user/v1/auth/signup", s.signup)
	mux.HandleFunc("POST /user/v1/auth/session", s.login)
	mux.HandleFunc("GET /user/v1/auth/session", s.session)
	mux.HandleFunc("DELETE /user/v1/auth/session", s.logout)
	mux.HandleFunc("GET /user/v1/upstreams", s.userOnly(s.upstreams))
	mux.HandleFunc("POST /user/v1/endpoints", s.userOnly(s.createEndpoint))
	mux.HandleFunc("GET /user/v1/endpoints", s.userOnly(s.listEndpoints))
	mux.HandleFunc("PUT /user/v1/endpoints/{id}", s.userOnly(s.updateEndpoint))
	mux.HandleFunc("POST /user/v1/endpoints/{id}/rotate-key", s.userOnly(s.rotateKey))
}

func (s Server) protect(handler http.HandlerFunc) http.HandlerFunc {
	digest := keys.Digest(s.Token)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !keys.Matches(digest[:], app.Bearer(r.Header.Get("Authorization"))) {
			problem(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		handler(w, r)
	}
}

type endpointInput struct {
	Backend  string            `json:"backend,omitempty"`
	UserID   string            `json:"user_id"`
	Protocol string            `json:"protocol"`
	Models   map[string]string `json:"models"`
	Enabled  *bool             `json:"enabled,omitempty"`
	Version  uint64            `json:"version,omitempty"`
}

func (input endpointInput) valid() bool {
	if input.Backend != "" && input.Backend != codex.Backend {
		return false
	}
	if input.Protocol != "responses" && input.Protocol != "messages" || len(input.Models) == 0 || len(input.Models) > 100 {
		return false
	}
	for alias, model := range input.Models {
		if !modelPattern.MatchString(alias) || !modelPattern.MatchString(model) {
			return false
		}
	}
	return true
}

func (s Server) createEndpoint(w http.ResponseWriter, r *http.Request) {
	var in endpointInput
	if !decode(w, r, &in) {
		return
	}
	if user := userID(r); user != "" {
		if in.UserID != "" && in.UserID != user {
			problem(w, 403, "endpoint owner cannot be changed")
			return
		}
		in.UserID = user
	}
	if !userPattern.MatchString(in.UserID) || !in.valid() || in.Enabled != nil || in.Version != 0 {
		problem(w, 400, "user_id, protocol and nonempty model aliases are required")
		return
	}
	key := keys.New("ap_")
	digest := keys.Digest(key)
	e := storage.Endpoint{
		ID: keys.New("ep_"), KeyID: keys.New("key_"), KeyHash: digest[:], UserID: in.UserID,
		Models: in.Models, Protocol: in.Protocol, Backend: codex.Backend, Version: 1, Enabled: true,
	}
	if err := s.Store.CreateEndpoint(r.Context(), e); err != nil {
		writeStorageError(w, err)
		return
	}
	write(w, http.StatusCreated, struct {
		storage.Endpoint
		Key string `json:"key"`
	}{e, key})
}

func (s Server) listEndpoints(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user_id")
	if owner := userID(r); owner != "" {
		user = owner
	}
	if !userPattern.MatchString(user) {
		problem(w, 400, "user_id is required")
		return
	}
	list, err := s.Store.Endpoints(r.Context(), user)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	write(w, 200, list)
}

func (s Server) updateEndpoint(w http.ResponseWriter, r *http.Request) {
	if !s.canManage(w, r) {
		return
	}
	var in endpointInput
	if !decode(w, r, &in) {
		return
	}
	if in.UserID != "" || in.Version == 0 || in.Enabled == nil || !in.valid() {
		problem(w, 400, "version, protocol, enabled and model aliases are required")
		return
	}
	e := storage.Endpoint{ID: r.PathValue("id"), Protocol: in.Protocol, Backend: codex.Backend, Models: in.Models, Enabled: *in.Enabled, Version: in.Version}
	if err := s.Store.UpdateEndpoint(r.Context(), e); err != nil {
		writeStorageError(w, err)
		return
	}
	write(w, 200, map[string]uint64{"version": in.Version + 1})
}

func (s Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	if !s.canManage(w, r) {
		return
	}
	key := keys.New("ap_")
	if err := s.Store.RotateKey(r.Context(), r.PathValue("id"), keys.New("key_"), keys.Digest(key)); err != nil {
		writeStorageError(w, err)
		return
	}
	write(w, 200, map[string]string{"key": key})
}

func (s Server) importAccount(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if err != nil {
		problem(w, 400, "invalid credential body")
		return
	}
	imported, err := codex.Import(raw)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	label := r.URL.Query().Get("label")
	if len(label) > 255 {
		problem(w, 400, "label is too long")
		return
	}
	plain, err := json.Marshal(imported.Credentials)
	if err != nil {
		problem(w, 500, "cannot encode credential")
		return
	}
	account := storage.Account{ID: imported.ID, Label: label, Expires: imported.Expires, Secret: s.Vault.Encrypt(imported.ID, plain)}
	if err := s.Store.PutAccount(r.Context(), account); err != nil {
		writeStorageError(w, err)
		return
	}
	updated, err := s.Store.Account(r.Context(), account.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	write(w, 200, updated)
}

func (s Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.Accounts(r.Context(), false, "")
	if err != nil {
		writeStorageError(w, err)
		return
	}
	write(w, 200, list)
}

func (s Server) enableAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		problem(w, 400, "enabled is required")
		return
	}
	if err := s.Store.EnableAccount(r.Context(), r.PathValue("id"), *body.Enabled); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		problem(w, 400, "invalid JSON")
		return false
	}
	return true
}

func writeStorageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrConflict):
		problem(w, 409, "resource changed or does not exist")
	case errors.Is(err, sql.ErrNoRows):
		problem(w, 404, "resource does not exist")
	default:
		slog.Warn("admin database operation failed")
		problem(w, 503, "storage unavailable")
	}
}

func problem(w http.ResponseWriter, code int, message string) {
	write(w, code, map[string]string{"error": message})
}

func write(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
