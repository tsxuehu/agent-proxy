package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/tsxuehu/agent-proxy/shared/app"
	"github.com/tsxuehu/agent-proxy/shared/codex"
	"github.com/tsxuehu/agent-proxy/shared/keys"
	"github.com/tsxuehu/agent-proxy/shared/storage"
)

type userContextKey struct{}

func userID(r *http.Request) string {
	user, _ := r.Context().Value(userContextKey{}).(string)
	return user
}

func (s Server) userOnly(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		token := app.Bearer(r.Header.Get("Authorization"))
		if !strings.HasPrefix(token, "apu_") || len(token) > 128 {
			problem(w, 401, "invalid portal token")
			return
		}
		user, err := s.Store.UserForKey(r.Context(), keys.Digest(token))
		if errors.Is(err, sql.ErrNoRows) {
			problem(w, 401, "invalid portal token")
			return
		}
		if err != nil {
			writeStorageError(w, err)
			return
		}
		handler(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, user)))
	}
}

func (s Server) canManage(w http.ResponseWriter, r *http.Request) bool {
	user := userID(r)
	if user == "" {
		return true
	}
	endpoint, err := s.Store.Endpoint(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && endpoint.UserID != user) {
		problem(w, 404, "endpoint does not exist")
		return false
	}
	if err != nil {
		writeStorageError(w, err)
		return false
	}
	return true
}

func (s Server) provisionUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"user_id"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !userPattern.MatchString(body.ID) {
		problem(w, 400, "valid user_id required")
		return
	}
	token := keys.New("apu_")
	if err := s.Store.PutUser(r.Context(), body.ID, keys.Digest(token)); err != nil {
		writeStorageError(w, err)
		return
	}
	write(w, 200, map[string]string{"user_id": body.ID, "token": token})
}

func (s Server) upstreams(w http.ResponseWriter, _ *http.Request) {
	write(w, 200, []map[string]string{{"id": codex.Backend, "name": "Codex OAuth account pool"}})
}

func (s Server) addModelKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string
		Label    string
		Key      string
	}
	if !decode(w, r, &body) {
		return
	}
	switch body.Provider {
	case "openai", "deepseek", "glm":
	default:
		problem(w, 400, "unsupported model provider")
		return
	}
	if body.Label == "" || len(body.Label) > 255 || body.Key == "" || len(body.Key) > 4096 ||
		strings.ContainsAny(body.Key, " \r\n\t") {
		problem(w, 400, "provider, label and valid key are required")
		return
	}
	item := storage.ModelKey{ID: keys.New("mk_"), Provider: body.Provider, Label: body.Label}
	if err := s.Store.CreateModelKey(r.Context(), item, s.Vault.Encrypt(item.ID, []byte(body.Key))); err != nil {
		writeStorageError(w, err)
		return
	}
	write(w, 201, item)
}

func (s Server) modelKeys(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ModelKeys(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	write(w, 200, items)
}

func (s Server) deleteModelKey(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteModelKey(r.Context(), r.PathValue("id")); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
