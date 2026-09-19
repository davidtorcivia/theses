package server

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/integrations"
	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/settings"
)

// The Integrations section: one row per service with connect, configure,
// disconnect and a test button, beside the workspace webhooks that were
// already there.

// driveKeys and transistorKeys are the settings prefixes each integration
// reads. A key not under one of these is not its business.
const (
	drivePrefix      = "integrations.drive."
	transistorPrefix = "integrations.transistor."
)

// oauthCookie carries the state parameter's other half. It is the CSRF check
// for the callback, which is a GET that changes settings and so cannot carry a
// form token: the code is only accepted when the state it comes back with
// matches the value this browser was given when it pressed Connect.
const oauthCookie = "theses_oauth"

type integrationsView struct {
	// Redirect is the callback URL, which has to be listed on the OAuth client
	// in Google's console. It is built from THESES_BASE_URL and never from the
	// request, so a forwarded Host header cannot move it.
	Redirect            string
	DriveConnected      bool
	DriveState          string
	TransistorConnected bool
	TransistorState     string
}

func (s *Server) integrationsSection() integrationsView {
	v := integrationsView{
		Redirect:   s.cfg.BaseURL + driveCallback,
		DriveState: "Not set up. Paste an OAuth client id and secret, then connect.",
		TransistorState: "Not set up. Paste an API key and test it to find the show " +
			"id, or paste both.",
	}
	switch {
	case s.settings.IsSet(drivePrefix+"token") && s.settings.IsSet(drivePrefix+"client_id"):
		v.DriveConnected = true
		v.DriveState = "Connected. Anyone who can edit a proposition can add files from Drive."
	case s.settings.IsSet(drivePrefix + "client_id"):
		v.DriveState = "Configured but not connected. Press Connect and sign in to the Google account the files are in."
	}
	if s.settings.IsSet(transistorPrefix+"api_key") &&
		settings.Get[string](s.settings, transistorPrefix+"show_id") != "" {
		v.TransistorConnected = true
		v.TransistorState = "Connected. A proposition can be published from its own settings page once it reaches " +
			settings.Get[string](s.settings, transistorPrefix+"publish_status") + "."
	}
	return v
}

// integrationSettings reads one integration's keys, unsealing its secrets. The
// values leave this function and go straight into the integration; nothing
// puts them on a page or in a log.
func (s *Server) integrationSettings(ctx context.Context, prefix string) (integrations.Settings, error) {
	out := integrations.Settings{}
	for _, d := range settings.Registry {
		if !strings.HasPrefix(d.Key, prefix) {
			continue
		}
		name := strings.TrimPrefix(d.Key, prefix)
		if !d.Secret {
			out[name] = settings.Get[string](s.settings, d.Key)
			continue
		}
		value, err := s.settings.Secret(ctx, d.Key)
		if err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, nil
}

// loadDrive hands the one Drive instance whatever settings hold now. It is
// re-read on every use so that a key saved a second ago is the key in use, and
// the instance is kept so that its refreshed access token outlives the request
// that fetched it.
func (s *Server) loadDrive(ctx context.Context) (*integrations.Drive, error) {
	set, err := s.integrationSettings(ctx, drivePrefix)
	if err != nil {
		return nil, err
	}
	if err := s.drive.Configure(set); err != nil {
		return nil, err
	}
	return s.drive, nil
}

func (s *Server) loadTransistor(ctx context.Context) (*integrations.Transistor, error) {
	set, err := s.integrationSettings(ctx, transistorPrefix)
	if err != nil {
		return nil, err
	}
	if err := s.transistor.Configure(set); err != nil {
		return nil, err
	}
	return s.transistor, nil
}

// saveDriveToken stores a token the integration refreshed. The actor is the
// system: the renewal is the app's own housekeeping, and recording it as the
// person who happened to be importing a file names them for something they did
// not do.
func (s *Server) saveDriveToken(ctx context.Context, t integrations.Token) error {
	blob, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return s.settings.SetAs(ctx, drivePrefix+"token", []string{string(blob)}, settings.System())
}

const driveCallback = "/settings/integrations/drive/callback"

// postDriveConnect starts the code flow. The state is a random value kept in a
// cookie of its own; the callback is only believed when the two match.
func (s *Server) postDriveConnect(w http.ResponseWriter, r *http.Request) {
	drive, err := s.loadDrive(r.Context())
	if err != nil {
		s.integrationRefused(w, r, err)
		return
	}
	if !drive.Configured() {
		s.integrationRefused(w, r, errors.New("paste the OAuth client id and secret first"))
		return
	}
	c, err := s.auth.NewBrowserCookie(oauthCookie)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, c)
	http.Redirect(w, r, drive.AuthURL(c.Value, s.cfg.BaseURL+driveCallback), http.StatusSeeOther)
}

// getDriveCallback is where Google sends the owner back. Nothing here trusts
// the request beyond the code: the state has to match this browser's cookie,
// and the redirect URL sent with the exchange is the configured one, not the
// one this request arrived on.
func (s *Server) getDriveCallback(w http.ResponseWriter, r *http.Request) {
	state, err := r.Cookie(oauthCookie)
	// The cookie is spent whichever way this goes, so a code cannot be replayed
	// against it.
	http.SetCookie(w, &http.Cookie{
		Name: oauthCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	if err != nil || state.Value == "" ||
		!hmac.Equal([]byte(state.Value), []byte(r.URL.Query().Get("state"))) {
		s.log.Warn("drive callback with a state that does not match", "addr", s.auth.ClientIP(r))
		s.integrationRefused(w, r, errors.New(
			"that did not come back from the connection this browser started; press Connect again"))
		return
	}
	if said := r.URL.Query().Get("error"); said != "" {
		s.integrationRefused(w, r, errors.New("Google refused the connection: "+said))
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.integrationRefused(w, r, errors.New("Google sent no authorization code"))
		return
	}

	drive, err := s.loadDrive(r.Context())
	if err != nil {
		s.integrationRefused(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	token, err := drive.Exchange(ctx, code, s.cfg.BaseURL+driveCallback)
	if err != nil {
		s.integrationRefused(w, r, err)
		return
	}
	if err := s.saveDriveToken(ctx, token); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, settingsTo("integrations", true), http.StatusSeeOther)
}

// postDriveDisconnect throws the token away. The client id and secret stay, so
// connecting again is one button rather than a trip to Google's console.
func (s *Server) postDriveDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.settings.Delete(r.Context(), drivePrefix+"token", settings.User(userOf(r).ID)); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, settingsTo("integrations", true), http.StatusSeeOther)
}

func (s *Server) postTransistorDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.settings.Delete(r.Context(), transistorPrefix+"api_key", settings.User(userOf(r).ID)); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, settingsTo("integrations", true), http.StatusSeeOther)
}

func (s *Server) postTestDrive(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	drive, err := s.loadDrive(ctx)
	if err != nil {
		s.integrationRefused(w, r, err)
		return
	}
	said, err := drive.Test(ctx)
	s.integrationResult(w, r, said, err)
}

func (s *Server) postTestTransistor(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	tr, err := s.loadTransistor(ctx)
	if err != nil {
		s.integrationRefused(w, r, err)
		return
	}
	said, err := tr.Test(ctx)
	s.integrationResult(w, r, said, err)
}

func (s *Server) integrationResult(w http.ResponseWriter, r *http.Request, said string, err error) {
	if err != nil {
		s.integrationRefused(w, r, err)
		return
	}
	s.back(w, r, "/settings#integrations", map[string]any{"IntegrationResult": said})
}

// integrationRefused prints why on the section rather than taking the whole
// page out. Whatever the message carries is redacted against the secrets this
// section holds, because a provider is free to quote back what it was sent.
func (s *Server) integrationRefused(w http.ResponseWriter, r *http.Request, err error) {
	s.back(w, r, "/settings#integrations", map[string]any{
		"IntegrationResult": s.redactSecrets(r.Context(), err.Error()), "IntegrationFailed": true,
	})
}

// redactSecrets takes every secret this workspace stores out of a message.
// Reading them all costs one decryption each and happens only on a failure.
func (s *Server) redactSecrets(ctx context.Context, text string) string {
	for _, d := range settings.Registry {
		if !d.Secret || !s.settings.IsSet(d.Key) {
			continue
		}
		value, err := s.settings.Secret(ctx, d.Key)
		if err != nil {
			continue
		}
		text = mail.Redact(text, value)
	}
	return text
}

// publishWindow is how long the presigned audio URL handed to Transistor
// lasts. Transistor fetches the file some time after the episode is created,
// not during the request, so the URL has to outlive the call by a good margin;
// a day is short enough that a leaked one is not a lasting handout and long
// enough that a slow queue at the other end still finds the file.
const publishWindow = 24 * time.Hour
