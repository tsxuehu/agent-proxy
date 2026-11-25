package storage

import "context"

func (s *Store) PutUser(ctx context.Context, user string, digest [32]byte) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO portal_users (id,key_hash) VALUES (?,?) AS incoming
		ON DUPLICATE KEY UPDATE key_hash=incoming.key_hash`, user, digest[:])
	return err
}

func (s *Store) UserForKey(ctx context.Context, digest [32]byte) (string, error) {
	var user string
	err := s.DB.QueryRowContext(ctx, "SELECT id FROM portal_users WHERE key_hash=?", digest[:]).Scan(&user)
	return user, err
}

type ModelKey struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
}

func (s *Store) CreateModelKey(ctx context.Context, item ModelKey, ciphertext []byte) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO model_api_keys (id,provider,label,credentials) VALUES (?,?,?,?)",
		item.ID, item.Provider, item.Label, ciphertext)
	return err
}

func (s *Store) ModelKeys(ctx context.Context) ([]ModelKey, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT id,provider,label FROM model_api_keys ORDER BY provider,label,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelKey{}
	for rows.Next() {
		var item ModelKey
		if err := rows.Scan(&item.ID, &item.Provider, &item.Label); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) DeleteModelKey(ctx context.Context, id string) error {
	return one(s.DB.ExecContext(ctx, "DELETE FROM model_api_keys WHERE id=?", id))
}
