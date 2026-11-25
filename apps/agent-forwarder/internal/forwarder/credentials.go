package forwarder

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tsxuehu/agent-proxy/shared/codex"
	"github.com/tsxuehu/agent-proxy/shared/keys"
	"github.com/tsxuehu/agent-proxy/shared/storage"
)

const refreshUncertainAfter = 90 * time.Second

func (s *Server) credential(ctx context.Context, a storage.Account, force bool) (codex.Credentials, error) {
	version := a.Version
	wait := time.NewTicker(150 * time.Millisecond)
	defer wait.Stop()
	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	for {
		if !a.Enabled || a.RefreshState == "reauthorize" {
			return codex.Credentials{}, errors.New("account unavailable")
		}
		old, err := s.decodeCredential(a)
		if err != nil {
			return codex.Credentials{}, err
		}
		if a.Expires.After(time.Now().Add(30*time.Second)) && (!force || a.Version != version) {
			return old, nil
		}
		if a.RefreshState == "ready" {
			owner := keys.New("")
			err := s.Store.Claim(ctx, a.ID, a.Version, owner)
			if err == nil {
				return s.refresh(ctx, a, owner, old)
			}
			if !errors.Is(err, storage.ErrConflict) {
				return codex.Credentials{}, err
			}
		}
		select {
		case <-ctx.Done():
			return codex.Credentials{}, ctx.Err()
		case <-deadline.C:
			return codex.Credentials{}, errors.New("refresh still in progress")
		case <-wait.C:
			a, err = s.Store.Account(ctx, a.ID)
			if err != nil {
				return codex.Credentials{}, err
			}
		}
	}
}

func (s *Server) decodeCredential(a storage.Account) (codex.Credentials, error) {
	var credentials codex.Credentials
	plain, err := s.Vault.Decrypt(a.ID, a.Secret)
	if err == nil {
		err = json.Unmarshal(plain, &credentials)
	}
	if err != nil {
		return codex.Credentials{}, errors.New("cannot read Codex credentials")
	}
	return credentials, nil
}

func (s *Server) refresh(ctx context.Context, account storage.Account, owner string, old codex.Credentials) (codex.Credentials, error) {
	exchange, cancel := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Second)
	fresh, expires, err := s.Tokens.Refresh(exchange, old)
	cancel()
	save, stop := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Second)
	defer stop()
	if err != nil {
		state := "reauthorize"
		var refreshErr codex.RefreshError
		if errors.As(err, &refreshErr) && refreshErr.Retryable {
			state = "ready"
			_ = s.Store.Cool(save, account.ID, "", time.Now().Add(time.Minute))
		}
		_ = s.Store.FinishFailedRefresh(save, account, owner, state)
		return codex.Credentials{}, err
	}
	plain, err := json.Marshal(fresh)
	if err != nil {
		return codex.Credentials{}, err
	}
	secret := s.Vault.Encrypt(account.ID, plain)
	for {
		err = s.Store.CompleteRefresh(save, account, owner, secret, expires)
		if err == nil {
			return fresh, nil
		}
		if errors.Is(err, storage.ErrConflict) {
			current, readErr := s.Store.Account(save, account.ID)
			if readErr == nil && current.Enabled && current.Version > account.Version && current.RefreshState == "ready" {
				return s.decodeCredential(current)
			}
			return codex.Credentials{}, err
		}
		select {
		case <-save.Done():
			return codex.Credentials{}, err
		case <-time.After(250 * time.Millisecond):
		}
	}
}
