package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// APIToken lets agents and scripts call the API with "Authorization: Bearer
// <token>". Only the SHA-256 of the token is stored; the token itself is shown
// once, when it is created. Tokens are replicated to the peer (jobs sync).
type APIToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Hash      string    `json:"hash,omitempty"` // sha256 hex; never sent to API clients
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	Version   int64     `json:"version"` // last-writer-wins replication
	Deleted   bool      `json:"deleted,omitempty"`
}

const tokenSchema = `
CREATE TABLE IF NOT EXISTS api_tokens (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	role       TEXT NOT NULL,
	hash       TEXT NOT NULL,
	created_by TEXT,
	created_ms INTEGER NOT NULL,
	expires_ms INTEGER,
	version    INTEGER NOT NULL,
	deleted    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS api_tokens_hash ON api_tokens(hash);`

func (s *Store) ensureTokens() error {
	_, err := s.db.Exec(tokenSchema)
	return err
}

// TokenHash is how a presented token is looked up.
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// CreateToken stores a new token and returns it with the plain-text secret.
func (s *Store) CreateToken(name, role, createdBy string, ttl time.Duration) (APIToken, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return APIToken{}, "", err
	}
	secret := "smt_" + base64.RawURLEncoding.EncodeToString(raw)
	idb := make([]byte, 6)
	rand.Read(idb)
	now := time.Now().UTC()
	t := APIToken{ID: hex.EncodeToString(idb), Name: name, Role: role, Hash: TokenHash(secret),
		CreatedBy: createdBy, CreatedAt: now, Version: now.UnixNano()}
	if ttl > 0 {
		t.ExpiresAt = now.Add(ttl)
	}
	return t, secret, s.write(func(tx *sql.Tx) error { return upsertToken(tx, t) })
}

func upsertToken(tx *sql.Tx, t APIToken) error {
	var exp sql.NullInt64
	if !t.ExpiresAt.IsZero() {
		exp = sql.NullInt64{Int64: ms(t.ExpiresAt), Valid: true}
	}
	_, err := tx.Exec(`INSERT INTO api_tokens (id, name, role, hash, created_by, created_ms, expires_ms, version, deleted)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, role=excluded.role, hash=excluded.hash,
			created_by=excluded.created_by, created_ms=excluded.created_ms, expires_ms=excluded.expires_ms,
			version=excluded.version, deleted=excluded.deleted
		WHERE excluded.version > api_tokens.version`,
		t.ID, t.Name, t.Role, t.Hash, t.CreatedBy, ms(t.CreatedAt), exp, t.Version, t.Deleted)
	return err
}

// RevokeToken marks a token deleted (a tombstone, so the peer learns it too).
func (s *Store) RevokeToken(id string) error {
	return s.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE api_tokens SET deleted = 1, version = ? WHERE id = ? AND deleted = 0`, time.Now().UnixNano(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("token not found")
		}
		return nil
	})
}

// LookupToken returns the active token with this hash.
func (s *Store) LookupToken(hash string) (APIToken, bool) {
	var t APIToken
	var created int64
	var exp sql.NullInt64
	err := s.db.QueryRow(`SELECT id, name, role, created_by, created_ms, expires_ms FROM api_tokens
		WHERE hash = ? AND deleted = 0`, hash).Scan(&t.ID, &t.Name, &t.Role, &t.CreatedBy, &created, &exp)
	if err != nil {
		return t, false
	}
	t.CreatedAt, t.ExpiresAt = fromMs(created), nullMs(exp)
	if !t.ExpiresAt.IsZero() && time.Now().After(t.ExpiresAt) {
		return t, false
	}
	return t, true
}

// Tokens lists tokens; withHash/withDeleted are used for peer sync only.
func (s *Store) Tokens(withHash, withDeleted bool) []APIToken {
	q := `SELECT id, name, role, hash, created_by, created_ms, expires_ms, version, deleted FROM api_tokens`
	if !withDeleted {
		q += ` WHERE deleted = 0`
	}
	rows, err := s.db.Query(q + ` ORDER BY created_ms`)
	if err != nil {
		logErr("tokens", err)
		return nil
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		var created int64
		var exp sql.NullInt64
		var del int
		if rows.Scan(&t.ID, &t.Name, &t.Role, &t.Hash, &t.CreatedBy, &created, &exp, &t.Version, &del) != nil {
			continue
		}
		t.CreatedAt, t.ExpiresAt, t.Deleted = fromMs(created), nullMs(exp), del == 1
		if !withHash {
			t.Hash = ""
		}
		out = append(out, t)
	}
	return out
}

// MergeTokens applies tokens from the peer (newer version wins).
func (s *Store) MergeTokens(in []APIToken) error {
	return s.write(func(tx *sql.Tx) error {
		for _, t := range in {
			if t.ID == "" || t.Hash == "" {
				return fmt.Errorf("invalid token record")
			}
			if err := upsertToken(tx, t); err != nil {
				return err
			}
		}
		return nil
	})
}
