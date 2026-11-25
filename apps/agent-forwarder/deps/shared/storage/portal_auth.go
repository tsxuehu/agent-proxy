package storage

import (
	"context"
	"database/sql"
	"errors"

	"github.com/go-sql-driver/mysql"
)

type PortalAccount struct {
	ID       string
	Username string
}

func (s *Store) CreatePortalAccount(ctx context.Context, account PortalAccount, passwordHash []byte, token [32]byte, ttlSeconds int) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "INSERT INTO portal_accounts (id,username,password_hash) VALUES (?,?,?)",
		account.ID, account.Username, passwordHash)
	if err != nil {
		var dbErr *mysql.MySQLError
		if errors.As(err, &dbErr) && dbErr.Number == 1062 {
			return ErrConflict
		}
		return err
	}
	if err := insertPortalSession(ctx, tx, account.ID, token, ttlSeconds); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PortalPassword(ctx context.Context, username string) (PortalAccount, []byte, error) {
	account := PortalAccount{Username: username}
	var hash []byte
	err := s.DB.QueryRowContext(ctx, "SELECT id,password_hash FROM portal_accounts WHERE username=?", username).Scan(&account.ID, &hash)
	return account, hash, err
}

func (s *Store) CreatePortalSession(ctx context.Context, accountID string, token, previous [32]byte, ttlSeconds int) error {
	// End any previous session for this account presented by the same browser.
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM portal_sessions WHERE account_id=?
		AND (expires_at<=UTC_TIMESTAMP(6) OR token_hash=?)`, accountID, previous[:]); err != nil {
		return err
	}
	if err := insertPortalSession(ctx, tx, accountID, token, ttlSeconds); err != nil {
		return err
	}
	return tx.Commit()
}

type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertPortalSession(ctx context.Context, db sqlExecer, accountID string, token [32]byte, ttlSeconds int) error {
	_, err := db.ExecContext(ctx, `INSERT INTO portal_sessions (token_hash,account_id,expires_at)
		VALUES (?,?,TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)))`, token[:], accountID, ttlSeconds)
	return err
}

func (s *Store) PortalSession(ctx context.Context, token [32]byte) (PortalAccount, error) {
	var account PortalAccount
	err := s.DB.QueryRowContext(ctx, `SELECT a.id,a.username FROM portal_sessions s JOIN portal_accounts a ON a.id=s.account_id
		WHERE s.token_hash=? AND s.expires_at>UTC_TIMESTAMP(6)`, token[:]).Scan(&account.ID, &account.Username)
	return account, err
}

func (s *Store) DeletePortalSession(ctx context.Context, token [32]byte) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM portal_sessions WHERE token_hash=?", token[:])
	return err
}
