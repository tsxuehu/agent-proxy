package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	BaseURL  = "https://chatgpt.com/backend-api/codex"
	TokenURL = "https://auth.openai.com/oauth/token"
	UsageURL = "https://chatgpt.com/backend-api/wham/usage"
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

type Credentials struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
}

type Imported struct {
	ID          string
	Credentials Credentials
	Expires     time.Time
}

func Import(data []byte) (Imported, error) {
	type record struct {
		Credentials
		AccountID string    `json:"account_id"`
		ExpiresAt time.Time `json:"expires_at"`
		Expired   time.Time `json:"expired"`
	}
	var entry struct {
		record
		Tokens *record `json:"tokens"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return Imported{}, errors.New("invalid credential document")
	}
	value := entry.record
	if entry.Tokens != nil {
		value = *entry.Tokens
	}
	var claims struct {
		Expiry int64 `json:"exp"`
		Auth   struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	parts := strings.Split(value.Access, ".")
	if len(parts) == 3 {
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		_ = json.Unmarshal(raw, &claims)
	}
	if value.AccountID == "" {
		value.AccountID = claims.Auth.AccountID
	}
	if claims.Auth.AccountID != "" && claims.Auth.AccountID != value.AccountID {
		return Imported{}, errors.New("account ID does not match the credential")
	}
	expires := value.ExpiresAt
	if expires.IsZero() {
		expires = value.Expired
	}
	if expires.IsZero() && claims.Expiry > 0 {
		expires = time.Unix(claims.Expiry, 0)
	}
	if value.AccountID == "" || len(value.AccountID) > 128 || value.Access == "" || value.Refresh == "" || expires.IsZero() ||
		strings.ContainsAny(value.AccountID+value.Access+value.Refresh, " \t\r\n") {
		return Imported{}, errors.New("account_id, access token, refresh token and expiry are required")
	}
	return Imported{ID: value.AccountID, Credentials: value.Credentials, Expires: expires.UTC()}, nil
}

type TokenClient struct {
	URL  string
	HTTP *http.Client
}

type RefreshError struct{ Retryable bool }

func (e RefreshError) Error() string { return "Codex OAuth refresh failed" }

func (c TokenClient) Refresh(ctx context.Context, current Credentials) (Credentials, time.Time, error) {
	body, err := json.Marshal(map[string]string{
		"client_id": ClientID, "grant_type": "refresh_token", "refresh_token": current.Refresh,
	})
	if err != nil {
		return Credentials{}, time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return Credentials{}, time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Credentials{}, time.Time{}, RefreshError{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Credentials{}, time.Time{}, RefreshError{Retryable: resp.StatusCode == http.StatusTooManyRequests}
	}
	var result struct {
		Credentials
		Expires int64 `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil ||
		result.Access == "" || result.Refresh == "" || result.Expires <= 0 || result.Expires > 86400*365 {
		return Credentials{}, time.Time{}, RefreshError{}
	}
	return result.Credentials, time.Now().UTC().Add(time.Duration(result.Expires) * time.Second), nil
}
