package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image/png"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/davidtorcivia/theses/internal/store"
)

const totpPeriod = 30

// Enrolment is what the setup and invite pages show: the shared secret, the
// otpauth URL behind the QR code, and the QR code itself as a data URI so that
// no extra request and no inline script is needed.
type Enrolment struct {
	Secret string
	URL    string
	QR     string
}

// Enrol makes a new authenticator secret for an account name.
func Enrol(handle string) (*Enrolment, error) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "THESES", AccountName: handle})
	if err != nil {
		return nil, fmt.Errorf("generate authenticator secret: %w", err)
	}
	img, err := key.Image(240, 240)
	if err != nil {
		return nil, fmt.Errorf("render QR code: %w", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode QR code: %w", err)
	}
	return &Enrolment{
		Secret: key.Secret(),
		URL:    key.URL(),
		QR:     "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}

var totpOpts = totp.ValidateOpts{Period: totpPeriod, Skew: 0, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}

// CheckCode validates a code against a secret with one step of skew either way
// and returns the time step it matched, which is what replay protection records.
func CheckCode(secret, code string, now time.Time) (int64, bool) {
	code = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, code)
	if len(code) != 6 || secret == "" {
		return 0, false
	}
	for _, offset := range []time.Duration{0, -totpPeriod * time.Second, totpPeriod * time.Second} {
		at := now.Add(offset)
		if ok, err := totp.ValidateCustom(code, secret, at, totpOpts); err == nil && ok {
			return at.Unix() / totpPeriod, true
		}
	}
	return 0, false
}

// CheckTOTP validates a code for a user and records the time step, so the same
// code cannot be used twice even within its window.
func (a *Auth) CheckTOTP(ctx context.Context, u *store.User, code string) error {
	step, ok := CheckCode(u.TOTPSecret, code, a.Now())
	if !ok {
		return ErrBadCredentials
	}
	fresh, err := store.ClaimTOTPStep(ctx, a.db, u.ID, step)
	if err != nil {
		return err
	}
	if !fresh {
		return ErrBadCredentials
	}
	return nil
}
