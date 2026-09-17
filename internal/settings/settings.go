// Package settings is the typed key-value table behind /settings. Secrets are
// encrypted with a key derived from THESES_SECRET_KEY and never leave the server.
package settings

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/hkdf"

	"github.com/davidtorcivia/theses/internal/store"
)

type Settings struct {
	db   *store.DB
	aead cipher.AEAD

	mu      sync.RWMutex
	values  map[string]any  // non-secret values that have been set
	present map[string]bool // every key that has a row, secrets included
}

// Open reads the settings table into memory. secretKey is config.SecretKey; the
// AES key is derived from it so that other uses of the same secret (the backup
// archive) get a different key.
func Open(ctx context.Context, db *store.DB, secretKey []byte) (*Settings, error) {
	var derived [32]byte
	if _, err := hkdf.New(sha256.New, secretKey, nil, []byte("theses/settings")).Read(derived[:]); err != nil {
		return nil, fmt.Errorf("derive settings key: %w", err)
	}
	block, err := aes.NewCipher(derived[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	s := &Settings{db: db, aead: aead, values: map[string]any{}, present: map[string]bool{}}
	return s, s.reload(ctx)
}

func (s *Settings) reload(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value_json, secret FROM settings`)
	if err != nil {
		return fmt.Errorf("read settings: %w", err)
	}
	defer rows.Close()

	values := map[string]any{}
	present := map[string]bool{}
	for rows.Next() {
		var key, valueJSON string
		var secret bool
		if err := rows.Scan(&key, &valueJSON, &secret); err != nil {
			return err
		}
		def, ok := Lookup(key)
		if !ok {
			continue // a key from a newer version, or one since removed
		}
		present[key] = true
		if secret {
			continue // decrypted on demand, never cached
		}
		v, err := decode(def, valueJSON)
		if err != nil {
			return fmt.Errorf("setting %s: %w", key, err)
		}
		values[key] = v
	}
	if err := rows.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	s.values, s.present = values, present
	s.mu.Unlock()
	return nil
}

// Get returns the value of a non-secret key, or its registered default if it has
// never been set. An unknown key or the wrong T is a programming error and panics.
func Get[T any](s *Settings, key string) T {
	def, ok := Lookup(key)
	if !ok {
		panic("settings: unknown key " + key)
	}
	if def.Secret {
		panic("settings: " + key + " is a secret, use Secret")
	}
	s.mu.RLock()
	v, set := s.values[key]
	s.mu.RUnlock()
	if !set {
		v = def.Default
	}
	typed, ok := v.(T)
	if !ok {
		panic(fmt.Sprintf("settings: %s is %T, not %T", key, v, typed))
	}
	return typed
}

// Secret decrypts a secret key's value. It returns "" when the key has never
// been set, and an error when the stored value cannot be decrypted, which is
// what a changed THESES_SECRET_KEY looks like.
func (s *Settings) Secret(ctx context.Context, key string) (string, error) {
	def, ok := Lookup(key)
	if !ok || !def.Secret {
		return "", fmt.Errorf("settings: %s is not a secret key", key)
	}
	row, err := store.GetSetting(ctx, s.db, key)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	blob, err := base64.StdEncoding.DecodeString(row.ValueJSON)
	if err != nil || len(blob) < s.aead.NonceSize() {
		return "", fmt.Errorf("settings: %s is not readable; re-enter it", key)
	}
	plain, err := s.aead.Open(nil, blob[:s.aead.NonceSize()], blob[s.aead.NonceSize():], []byte(key))
	if err != nil {
		return "", fmt.Errorf("settings: %s cannot be decrypted with this THESES_SECRET_KEY; re-enter it", key)
	}
	return string(plain), nil
}

// IsSet reports whether a key has a stored value. The settings page uses it to
// show "set" for a secret instead of its value.
func (s *Settings) IsSet(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.present[key]
}

// Set validates values against the key's definition, stores it and writes an
// activity row. values is the form's slice for that field: a list setting takes
// every non-empty entry, everything else takes the first.
func (s *Settings) Set(ctx context.Context, key string, values []string, actorID int64) error {
	def, ok := Lookup(key)
	if !ok {
		return fmt.Errorf("settings: unknown key %s", key)
	}

	parsed, err := parse(def, values)
	if err != nil {
		return err
	}

	stored, after := "", ""
	if def.Secret {
		secret := parsed.(string)
		if secret == "" {
			return fmt.Errorf("%s: cannot be blanked; leave the field empty to keep the stored value", def.Label)
		}
		nonce := make([]byte, s.aead.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return err
		}
		stored = base64.StdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(secret), []byte(key)))
		after = `{"set":true}`
	} else {
		b, err := json.Marshal(parsed)
		if err != nil {
			return err
		}
		stored, after = string(b), string(b)
	}

	before := ""
	if old, err := store.GetSetting(ctx, s.db, key); err == nil {
		if def.Secret {
			before = `{"set":true}`
		} else {
			before = old.ValueJSON
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := store.PutSetting(ctx, tx, key, stored, def.Secret, actorID); err != nil {
		return fmt.Errorf("save %s: %w", key, err)
	}
	if err := store.InsertActivity(ctx, tx, "user", strconv.FormatInt(actorID, 10),
		"setting", key, "set", before, after); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	s.mu.Lock()
	s.present[key] = true
	if !def.Secret {
		s.values[key] = parsed
	}
	s.mu.Unlock()
	return nil
}

func parse(def Def, values []string) (any, error) {
	first := ""
	if len(values) > 0 {
		first = strings.TrimSpace(values[len(values)-1])
	}
	switch def.Kind {
	case KindString:
		return first, nil
	case KindText:
		if len(values) > 0 {
			return strings.ReplaceAll(values[len(values)-1], "\r\n", "\n"), nil
		}
		return "", nil
	case KindInt:
		n, err := strconv.Atoi(first)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a number", def.Label, first)
		}
		return n, nil
	case KindBool:
		// An unchecked checkbox sends nothing, so an absent value is false.
		return first != "" && first != "false" && first != "off", nil
	case KindChoice:
		for _, c := range def.Choices {
			if c == first {
				return first, nil
			}
		}
		return nil, fmt.Errorf("%s: %q is not one of %s", def.Label, first, strings.Join(def.Choices, ", "))
	case KindList:
		// Accepts both repeated fields and one textarea of lines.
		var out []string
		for _, v := range values {
			for _, line := range strings.Split(v, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					out = append(out, line)
				}
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%s: needs at least one entry", def.Label)
		}
		return out, nil
	}
	return nil, fmt.Errorf("settings: %s has no kind", def.Key)
}

func decode(def Def, valueJSON string) (any, error) {
	var target any
	switch def.Kind {
	case KindString, KindText, KindChoice:
		target = new(string)
	case KindInt:
		target = new(int)
	case KindBool:
		target = new(bool)
	case KindList:
		target = new([]string)
	default:
		return nil, fmt.Errorf("no kind")
	}
	if err := json.Unmarshal([]byte(valueJSON), target); err != nil {
		return nil, err
	}
	switch v := target.(type) {
	case *string:
		return *v, nil
	case *int:
		return *v, nil
	case *bool:
		return *v, nil
	case *[]string:
		return *v, nil
	}
	return nil, fmt.Errorf("no kind")
}
