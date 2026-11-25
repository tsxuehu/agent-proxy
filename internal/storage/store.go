package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

//go:embed schema.sql
var schema string

type Store struct{ DB *sql.DB }

func Open(dsn string) (*Store, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("invalid MySQL DSN")
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 10 * time.Second
	cfg.WriteTimeout = 10 * time.Second
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, errors.New("could not open MySQL")
	}
	db.SetMaxOpenConns(24)
	db.SetMaxIdleConns(12)
	db.SetConnMaxLifetime(4 * time.Minute)
	return &Store{DB: db}, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	for _, stmt := range strings.Split(schema, ";") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("apply schema: %w", err)
			}
		}
	}
	return nil
}

func (s *Store) Ready(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, "SELECT backend FROM endpoints LIMIT 0")
	if err != nil {
		return err
	}
	return rows.Close()
}

type Endpoint struct {
	ID       string            `json:"id"`
	UserID   string            `json:"user_id"`
	Protocol string            `json:"protocol"`
	Backend  string            `json:"backend"`
	Version  uint64            `json:"version"`
	Models   map[string]string `json:"models"`
	Enabled  bool              `json:"enabled"`
	KeyID    string            `json:"-"`
	KeyHash  []byte            `json:"-"`
}

const endpointColumns = "id, user_id, protocol, backend, version, models, enabled, key_id, key_hash"

func scanEndpoint(row interface{ Scan(...any) error }) (Endpoint, error) {
	var e Endpoint
	var models []byte
	err := row.Scan(&e.ID, &e.UserID, &e.Protocol, &e.Backend, &e.Version, &models, &e.Enabled, &e.KeyID, &e.KeyHash)
	if err == nil {
		err = json.Unmarshal(models, &e.Models)
	}
	return e, err
}

func (s *Store) Endpoint(ctx context.Context, id string) (Endpoint, error) {
	return scanEndpoint(s.DB.QueryRowContext(ctx, "SELECT "+endpointColumns+" FROM endpoints WHERE id = ?", id))
}

func (s *Store) Endpoints(ctx context.Context, userID string) ([]Endpoint, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+endpointColumns+" FROM endpoints WHERE user_id = ? ORDER BY id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Endpoint{}
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CreateEndpoint(ctx context.Context, e Endpoint) error {
	models, err := json.Marshal(e.Models)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, "INSERT INTO endpoints (id,user_id,protocol,backend,models,key_hash,key_id) VALUES (?,?,?,?,?,?,?)",
		e.ID, e.UserID, e.Protocol, e.Backend, models, e.KeyHash, e.KeyID)
	return err
}

func (s *Store) UpdateEndpoint(ctx context.Context, e Endpoint) error {
	models, err := json.Marshal(e.Models)
	if err != nil {
		return err
	}
	return one(s.DB.ExecContext(ctx, "UPDATE endpoints SET protocol=?,backend=?,models=?,enabled=?,version=version+1 WHERE id=? AND version=?",
		e.Protocol, e.Backend, models, e.Enabled, e.ID, e.Version))
}

func (s *Store) RotateKey(ctx context.Context, id, keyID string, hash [32]byte) error {
	return one(s.DB.ExecContext(ctx, "UPDATE endpoints SET key_hash=?,key_id=? WHERE id=?", hash[:], keyID, id))
}

type Account struct {
	ID             string          `json:"id"`
	Label          string          `json:"label"`
	Enabled        bool            `json:"enabled"`
	Expires        time.Time       `json:"expires_at"`
	RefreshState   string          `json:"refresh_state"`
	Cooldown       sql.NullTime    `json:"-"`
	Quota          sql.NullFloat64 `json:"-"`
	QuotaObserved  sql.NullTime    `json:"-"`
	Version        uint64          `json:"-"`
	Secret         []byte          `json:"-"`
	RefreshOwner   string          `json:"-"`
	RefreshStarted sql.NullTime    `json:"-"`
}

const accountColumns = "id,label,enabled,expires_at,refresh_state,cooldown_until,quota_score,quota_observed,version,credentials,refresh_owner,refresh_started"

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Label, &a.Enabled, &a.Expires, &a.RefreshState, &a.Cooldown, &a.Quota, &a.QuotaObserved, &a.Version, &a.Secret,
		&a.RefreshOwner, &a.RefreshStarted)
	return a, err
}

func (s *Store) Account(ctx context.Context, id string) (Account, error) {
	return scanAccount(s.DB.QueryRowContext(ctx, "SELECT "+accountColumns+" FROM codex_accounts WHERE id=?", id))
}

const eligibility = "enabled AND refresh_state <> 'reauthorize' AND (cooldown_until IS NULL OR cooldown_until <= UTC_TIMESTAMP(6))" +
	" AND NOT EXISTS (SELECT 1 FROM model_cooldowns c WHERE c.account_id=codex_accounts.id AND c.model=? AND c.until_at>UTC_TIMESTAMP(6))"

func (s *Store) EligibleAccount(ctx context.Context, id, model string) (Account, error) {
	return scanAccount(s.DB.QueryRowContext(ctx, "SELECT "+accountColumns+" FROM codex_accounts WHERE id=? AND "+eligibility, id, model))
}

func (s *Store) Accounts(ctx context.Context, eligible bool, model string) ([]Account, error) {
	query := "SELECT " + accountColumns + " FROM codex_accounts"
	var args []any
	if eligible {
		query += " WHERE " + eligibility
		args = append(args, model)
	}
	rows, err := s.DB.QueryContext(ctx, query+" ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) PutAccount(ctx context.Context, a Account) error {
	return one(s.DB.ExecContext(ctx, `INSERT INTO codex_accounts (id,label,credentials,expires_at) VALUES (?,?,?,?) AS incoming
		ON DUPLICATE KEY UPDATE label=IF(refresh_state='refreshing',codex_accounts.label,incoming.label),
		credentials=IF(refresh_state='refreshing',codex_accounts.credentials,incoming.credentials),
		expires_at=IF(refresh_state='refreshing',codex_accounts.expires_at,incoming.expires_at),
		version=IF(refresh_state='refreshing',version,version+1),
		refresh_owner=IF(refresh_state='refreshing',refresh_owner,''),
		refresh_started=IF(refresh_state='refreshing',refresh_started,NULL),
		refresh_state=IF(refresh_state='refreshing',refresh_state,'ready')`, a.ID, a.Label, a.Secret, a.Expires))
}

func (s *Store) EnableAccount(ctx context.Context, id string, enabled bool) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE codex_accounts SET enabled=? WHERE id=?", enabled, id)
	return err
}

func (s *Store) Claim(ctx context.Context, id string, version uint64, owner string) error {
	return one(s.DB.ExecContext(ctx, `UPDATE codex_accounts SET refresh_owner=?,refresh_state='refreshing',
		refresh_started=UTC_TIMESTAMP(6) WHERE id=? AND version=? AND enabled AND refresh_state='ready'`, owner, id, version))
}

func (s *Store) CompleteRefresh(ctx context.Context, a Account, owner string, secret []byte, expires time.Time) error {
	return one(s.DB.ExecContext(ctx, `UPDATE codex_accounts SET credentials=?,expires_at=?,version=version+1,refresh_state='ready',
		refresh_owner='',refresh_started=NULL WHERE id=? AND version=? AND refresh_owner=? AND refresh_state='refreshing'`,
		secret, expires, a.ID, a.Version, owner))
}

func (s *Store) FinishFailedRefresh(ctx context.Context, a Account, owner, state string) error {
	return one(s.DB.ExecContext(ctx, `UPDATE codex_accounts SET refresh_state=?,refresh_owner='',refresh_started=NULL
		WHERE id=? AND version=? AND refresh_owner=? AND refresh_state='refreshing'`, state, a.ID, a.Version, owner))
}

func (s *Store) RecoverStaleRefreshes(ctx context.Context, before time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE codex_accounts SET refresh_state='reauthorize',refresh_owner='',refresh_started=NULL
		WHERE refresh_state='refreshing' AND refresh_started < ?`, before)
	return err
}

func (s *Store) Cool(ctx context.Context, id, model string, until time.Time) error {
	if model != "" {
		_, err := s.DB.ExecContext(ctx, `INSERT INTO model_cooldowns (account_id,model,until_at) VALUES (?,?,?) AS incoming
			ON DUPLICATE KEY UPDATE until_at=GREATEST(model_cooldowns.until_at,incoming.until_at)`, id, model, until)
		return err
	}
	_, err := s.DB.ExecContext(ctx, "UPDATE codex_accounts SET cooldown_until=GREATEST(COALESCE(cooldown_until,?),?) WHERE id=?", until, until, id)
	return err
}

func (s *Store) NextCooldown(ctx context.Context, model string) (time.Time, error) {
	var next sql.NullTime
	err := s.DB.QueryRowContext(ctx, `SELECT CAST(MIN(GREATEST(COALESCE(a.cooldown_until,'1970-01-01 00:00:00'),
		COALESCE(m.until_at,'1970-01-01 00:00:00'))) AS DATETIME(6)) FROM codex_accounts a LEFT JOIN model_cooldowns m
		ON m.account_id=a.id AND m.model=? WHERE a.enabled AND a.refresh_state='ready'`, model).Scan(&next)
	if !next.Time.After(time.Now()) {
		return time.Time{}, err
	}
	return next.Time, err
}

func (s *Store) ObserveQuota(ctx context.Context, id string, score float64, observed time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE codex_accounts SET quota_score=?,quota_observed=?
		WHERE id=? AND (quota_observed IS NULL OR quota_observed <= ?)`, score, observed, id, observed)
	return err
}

type Binding int

const (
	Session Binding = iota
	Response
)

func (b Binding) table() string {
	if b == Response {
		return "response_bindings"
	}
	return "session_bindings"
}

func Scope(fields ...string) [32]byte {
	data, _ := json.Marshal(fields)
	return sha256.Sum256(data)
}

func (s *Store) Binding(ctx context.Context, kind Binding, scope [32]byte) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, "SELECT account_id FROM "+kind.table()+" WHERE scope=? AND expires_at > UTC_TIMESTAMP(6)", scope[:]).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *Store) Bind(ctx context.Context, kind Binding, scope [32]byte, accountID string, expiry time.Time) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO "+kind.table()+" (scope,account_id,expires_at) VALUES (?,?,?) AS incoming"+
		" ON DUPLICATE KEY UPDATE account_id=incoming.account_id,expires_at=incoming.expires_at", scope[:], accountID, expiry)
	return err
}

func (s *Store) PurgeBindings(ctx context.Context) error {
	for _, kind := range []Binding{Session, Response} {
		if _, err := s.DB.ExecContext(ctx, "DELETE FROM "+kind.table()+" WHERE expires_at <= UTC_TIMESTAMP(6) LIMIT 500"); err != nil {
			return err
		}
	}
	return nil
}

var ErrConflict = errors.New("state changed concurrently or resource not found")

func one(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return ErrConflict
	}
	return err
}
