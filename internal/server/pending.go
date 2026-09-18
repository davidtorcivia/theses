package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/crypto/hkdf"
)

const pendingCookie = "theses_pending"
const pendingValidity = 15 * time.Minute

// pending is an account part-way through enrolment: the details are already
// checked and the password already hashed, but nothing is in the database until
// the authenticator code comes back. Keeping it in an encrypted cookie means a
// crash or an abandoned tab leaves no half-made user, and "no users yet" stays a
// clean first-run check.
type pending struct {
	Kind         string `json:"k"` // setup, invite or reenrol
	UserID       int64  `json:"u,omitempty"`
	InvitationID int64  `json:"i,omitempty"`
	Handle       string `json:"h,omitempty"`
	Name         string `json:"n,omitempty"`
	Initials     string `json:"in,omitempty"`
	Colour       string `json:"c,omitempty"`
	Email        string `json:"e,omitempty"`
	PasswordHash string `json:"p,omitempty"`
	Role         string `json:"r,omitempty"`
	Secret       string `json:"s"`
	OTPURL       string `json:"o"`
	Expires      int64  `json:"x"`
}

type pendingStore struct {
	aead cipher.AEAD
}

func newPendingStore(secretKey []byte) (*pendingStore, error) {
	var derived [32]byte
	if _, err := hkdf.New(sha256.New, secretKey, nil, []byte("theses/pending")).Read(derived[:]); err != nil {
		return nil, fmt.Errorf("derive enrolment key: %w", err)
	}
	block, err := aes.NewCipher(derived[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &pendingStore{aead: aead}, nil
}

var errNoPending = errors.New("no enrolment in progress")

func (p *pendingStore) put(w http.ResponseWriter, secure bool, v *pending) error {
	v.Expires = time.Now().Add(pendingValidity).Unix()
	plain, err := json.Marshal(v)
	if err != nil {
		return err
	}
	nonce := make([]byte, p.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     pendingCookie,
		Value:    base64.RawURLEncoding.EncodeToString(p.aead.Seal(nonce, nonce, plain, nil)),
		Path:     "/",
		MaxAge:   int(pendingValidity.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (p *pendingStore) get(r *http.Request) (*pending, error) {
	c, err := r.Cookie(pendingCookie)
	if err != nil {
		return nil, errNoPending
	}
	blob, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(blob) < p.aead.NonceSize() {
		return nil, errNoPending
	}
	plain, err := p.aead.Open(nil, blob[:p.aead.NonceSize()], blob[p.aead.NonceSize():], nil)
	if err != nil {
		return nil, errNoPending
	}
	var v pending
	if err := json.Unmarshal(plain, &v); err != nil {
		return nil, errNoPending
	}
	if v.Expires <= time.Now().Unix() {
		return nil, errNoPending
	}
	return &v, nil
}

func (p *pendingStore) clear(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: pendingCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}
