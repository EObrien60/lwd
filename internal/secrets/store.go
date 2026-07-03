package secrets

import (
	"fmt"

	"lwd/internal/store"
)

// Store persists per-app secret values encrypted via Cipher and resolves them.
type Store struct {
	cipher *Cipher
	db     *store.Store
}

// NewStore combines a Cipher with the persistence store.
func NewStore(c *Cipher, db *store.Store) *Store {
	return &Store{cipher: c, db: db}
}

// Set encrypts value and upserts it for (app, key).
func (s *Store) Set(app, key, value string) error {
	enc, err := s.cipher.Encrypt([]byte(value))
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	return s.db.SetSecret(app, key, enc)
}

// Get returns the decrypted value and whether it exists.
func (s *Store) Get(app, key string) (string, bool, error) {
	enc, err := s.db.GetSecret(app, key)
	if err != nil {
		return "", false, err
	}
	if enc == nil {
		return "", false, nil
	}
	pt, err := s.cipher.Decrypt(enc)
	if err != nil {
		return "", false, fmt.Errorf("decrypt %s/%s: %w", app, key, err)
	}
	return string(pt), true, nil
}

// List returns the secret names for an app (never values), sorted.
func (s *Store) List(app string) ([]string, error) {
	return s.db.ListSecretKeys(app)
}

// Delete removes a secret.
func (s *Store) Delete(app, key string) error {
	return s.db.DeleteSecret(app, key)
}

// Resolve returns values for all names, or an error naming the first that is
// missing or undecryptable (fail-closed — used at deploy time).
func (s *Store) Resolve(app string, names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for _, name := range names {
		v, ok, err := s.Get(app, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("secret %q is not set for app %q", name, app)
		}
		out[name] = v
	}
	return out, nil
}
