package forwarder

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/tsxuehu/agent-proxy/shared/codex"
)

func (s *Server) Maintain(ctx context.Context) {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if ok, err := s.Cache.SetNX(ctx, "agent:maintenance", "1", 55*time.Second).Result(); err == nil && ok {
			s.maintenanceCycle(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func (s *Server) maintenanceCycle(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	if err := s.Store.RecoverStaleRefreshes(ctx, time.Now().Add(-refreshUncertainAfter)); err != nil {
		slog.Warn("could not reconcile Codex refresh state")
		return
	}
	_ = s.Store.PurgeBindings(ctx)
	accounts, err := s.Store.Accounts(ctx, false, "")
	if err != nil {
		slog.Warn("could not load account maintenance candidates")
		return
	}
	for _, account := range accounts {
		if ctx.Err() != nil {
			return
		}
		if !account.Enabled || account.RefreshState != "ready" ||
			(account.Cooldown.Valid && account.Cooldown.Time.After(time.Now())) {
			continue
		}
		if s.UsageURL == "" && account.Expires.After(time.Now().Add(30*time.Second)) {
			continue
		}
		credential, err := s.credential(ctx, account, false)
		if err == nil && s.UsageURL != "" {
			s.sample(ctx, account.ID, credential)
		}
	}
}

func (s *Server) sample(ctx context.Context, id string, credential codex.Credentials) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	observed := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.UsageURL, nil)
	if err != nil {
		return
	}
	req.Header = codex.Headers(nil, id, credential.Access)
	req.Header.Set("Accept", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	data, err := readLimited(resp.Body, 64<<10)
	if err != nil {
		return
	}
	type window struct {
		Used  *float64 `json:"used_percent"`
		Reset int64    `json:"reset_at"`
	}
	var snapshot struct {
		Rate struct {
			Primary   window `json:"primary_window"`
			Secondary window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return
	}
	var seen bool
	var peak float64
	for _, w := range []window{snapshot.Rate.Primary, snapshot.Rate.Secondary} {
		if w.Used == nil || *w.Used < 0 || *w.Used > 100 || (w.Reset != 0 && w.Reset <= observed.Unix()) {
			continue
		}
		seen = true
		peak = max(peak, *w.Used/100)
		if *w.Used >= 100 && w.Reset > observed.Unix() {
			_ = s.Store.Cool(ctx, id, "", capReset(time.Unix(w.Reset, 0), observed))
		}
	}
	if seen {
		_ = s.Store.ObserveQuota(ctx, id, peak, observed)
	}
}

func capReset(reset, now time.Time) time.Time {
	if cap := now.Add(30 * 24 * time.Hour); reset.After(cap) {
		return cap
	}
	return reset
}
