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

// pending is an account part-way through enrollment: the details are already
// checked and the password already hashed, but nothing is in the database until
// the authenticator code comes back. Keeping it in an encrypted cookie means a
// crash or an abandoned tab leaves no half-made user, and "no users yet" stays a
// clean first-run check.
type pending struct {
	Kind         string `json:"k"` // setup, invite, signin or reenrol
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
		return nil, fmt.Errorf("derive enrollment key: %w", err)
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

var errNoPending = errors.New("no enrollment in progress")

func (p *pendingStore) seal(v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, p.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(p.aead.Seal(nonce, nonce, plain, nil)), nil
}

// unseal says whether the cookie is one this process wrote and still parses.
func (p *pendingStore) unseal(value string, into any) bool {
	blob, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(blob) < p.aead.NonceSize() {
		return false
	}
	plain, err := p.aead.Open(nil, blob[:p.aead.NonceSize()], blob[p.aead.NonceSize():], nil)
	if err != nil {
		return false
	}
	return json.Unmarshal(plain, into) == nil
}

func (p *pendingStore) put(w http.ResponseWriter, secure bool, v *pending) error {
	v.Expires = time.Now().Add(pendingValidity).Unix()
	value, err := p.seal(v)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     pendingCookie,
		Value:    value,
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
	var v pending
	if !p.unseal(c.Value, &v) {
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

// flash is what a form has to say, carried across the redirect that takes the
// browser back to the section it posted from. It is encrypted for the same
// reason the enrollment cookie is: a new API token and a new invitation link
// both pass through it, and neither may sit in a readable cookie or in an
// address the browser keeps in its history.
//
// It names who it is for, when it stops being true and which page it belongs
// on, and all three are checked before a word of it is printed. A browser two
// people share would otherwise show the second one the first one's token, and
// a cookie with no expiry of its own would still show it days later.
type flash struct {
	UserID  int64          `json:"u"`
	Path    string         `json:"p"`
	Section string         `json:"s"`
	Expires int64          `json:"x"`
	Say     map[string]any `json:"y"`
}

const flashCookie = "theses_flash"
const flashValidity = time.Minute

func (p *pendingStore) putFlash(w http.ResponseWriter, secure bool, f *flash) error {
	f.Expires = time.Now().Add(flashValidity).Unix()
	value, err := p.seal(f)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   int(flashValidity.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// takeFlash reads it and clears it: what a form said is said once, on the page
// it posted from, to the account that posted it, and not again afterwards. One
// that is still good but belongs on the other page is left where it is, since
// that page has not been asked for yet and the minute is short.
func (p *pendingStore) takeFlash(w http.ResponseWriter, r *http.Request, secure bool, userID int64) *flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	var f flash
	live := p.unseal(c.Value, &f) && f.UserID == userID && f.Expires > time.Now().Unix()
	if live && f.Path != r.URL.Path {
		return nil
	}
	p.clearFlash(w, secure)
	if !live {
		return nil
	}
	return &f
}

func (p *pendingStore) clearFlash(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}
