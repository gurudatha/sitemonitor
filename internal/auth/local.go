package auth

import (
	"context"

	"golang.org/x/crypto/bcrypt"

	"sitemonitor/internal/config"
)

// Local authenticates against bcrypt hashes in node.yaml (auth.local.users).
// Intended for POCs and as a break-glass fallback behind Active Directory.
type Local struct {
	users map[string]config.LocalUser
	dummy []byte
}

func NewLocal(cfg config.LocalAuth) *Local {
	l := &Local{users: map[string]config.LocalUser{}}
	for _, u := range cfg.Users {
		l.users[u.Username] = u
	}
	// Compared against for unknown users so response time doesn't reveal which usernames exist.
	l.dummy, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcrypt.DefaultCost)
	return l
}

func (l *Local) Name() string { return "local" }

func (l *Local) Authenticate(_ context.Context, username, password string) (*Identity, error) {
	u, ok := l.users[username]
	hash := []byte(u.PasswordHash)
	if !ok {
		hash = l.dummy
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil || !ok {
		return nil, ErrInvalidCredentials
	}
	name := u.DisplayName
	if name == "" {
		name = u.Username
	}
	return &Identity{Username: u.Username, DisplayName: name, Role: u.Role, Provider: "local"}, nil
}

// HashPassword returns a bcrypt hash for auth.local.users[].password_hash.
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	return string(h), err
}
