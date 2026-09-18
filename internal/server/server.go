// Package server is the HTTP surface: routes, templates, static assets, the
// security headers and the pages outside the workspace.
package server

import (
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/config"
	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
	"github.com/davidtorcivia/theses/web"
)

// Check is one readiness probe. The database is registered here; the object
// store and the backup age are added by the packages that own them.
type Check struct {
	Name string
	Run  func(context.Context) error
}

type Server struct {
	cfg      *config.Config
	db       *store.DB
	auth     *auth.Auth
	settings *settings.Settings
	mail     *mail.Outbox
	log      *slog.Logger
	version  string

	dev        bool
	assets     *assets
	templateFS fs.FS
	templates  map[string]*template.Template

	// hasUsers latches once the owner exists, so the setup gate costs one query.
	hasUsers atomic.Bool
	pending  *pendingStore
	checks   []Check
	handler  http.Handler
}

func New(cfg *config.Config, db *store.DB, set *settings.Settings, log *slog.Logger, version string) (*Server, error) {
	templateFS, staticFS := web.Templates, web.Static
	if cfg.Dev {
		templateFS = os.DirFS(filepath.Join(web.DevDir, "templates"))
		staticFS = os.DirFS(filepath.Join(web.DevDir, "static"))
	}

	a, err := newAssets(staticFS, cfg.Dev)
	if err != nil {
		return nil, fmt.Errorf("static assets: %w", err)
	}
	pending, err := newPendingStore(cfg.SecretKey)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:        cfg,
		db:         db,
		auth:       auth.New(db, cfg.SessionKey, cfg.TrustProxy, cfg.CookieSecure),
		settings:   set,
		mail:       mail.NewOutbox(db, set, log),
		log:        log,
		version:    version,
		dev:        cfg.Dev,
		assets:     a,
		templateFS: templateFS,
		pending:    pending,
	}
	if s.templates, err = parseTemplates(templateFS, s.funcs()); err != nil {
		return nil, err
	}

	s.AddCheck(Check{Name: "database", Run: func(ctx context.Context) error {
		var n int
		return db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n)
	}})

	n, err := store.CountUsers(context.Background(), db)
	if err != nil {
		return nil, fmt.Errorf("count users: %w", err)
	}
	s.hasUsers.Store(n > 0)

	s.handler = s.chain(s.routes())
	return s, nil
}

// Mail is the outbox worker. main runs it and stops it with the process.
func (s *Server) Mail() *mail.Outbox { return s.mail }

// AddCheck registers a readiness probe. Call it before the server starts serving.
func (s *Server) AddCheck(c Check) { s.checks = append(s.checks, c) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", s.assets)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "theses", s.version)
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /offline", func(w http.ResponseWriter, r *http.Request) { s.offlinePage(w, r) })

	mux.HandleFunc("GET /setup", s.getSetup)
	mux.HandleFunc("POST /setup", s.postSetup)
	mux.HandleFunc("GET /setup/authenticator", s.getEnrol)
	mux.HandleFunc("POST /setup/authenticator", s.postEnrol)

	mux.HandleFunc("GET /login", s.getLogin)
	mux.HandleFunc("POST /login", s.postLogin)
	mux.HandleFunc("POST /logout", s.postLogout)

	mux.HandleFunc("GET /reset", s.getReset)
	mux.HandleFunc("POST /reset", s.postReset)
	mux.HandleFunc("GET /reset/{token}", s.getResetToken)
	mux.HandleFunc("POST /reset/{token}", s.postResetToken)

	mux.HandleFunc("GET /invite/{token}", s.getInvite)
	mux.HandleFunc("POST /invite/{token}", s.postInvite)
	mux.HandleFunc("GET /invite/{token}/authenticator", s.getEnrol)
	mux.HandleFunc("POST /invite/{token}/authenticator", s.postEnrol)

	mux.HandleFunc("GET /{$}", s.requireUser(s.getShell))

	mux.HandleFunc("GET /profile", s.requireUser(s.getProfile))
	mux.HandleFunc("POST /profile", s.requireUser(s.postProfile))
	mux.HandleFunc("POST /profile/password", s.requireUser(s.postPassword))
	mux.HandleFunc("POST /profile/totp", s.requireUser(s.postReenrol))
	mux.HandleFunc("GET /profile/authenticator", s.requireUser(s.getEnrol))
	mux.HandleFunc("POST /profile/authenticator", s.requireUser(s.postEnrol))
	mux.HandleFunc("POST /profile/signout-everywhere", s.requireUser(s.postSignOutEverywhere))
	mux.HandleFunc("POST /profile/delete", s.requireUser(s.postDeleteAccount))

	mux.HandleFunc("GET /settings", s.requireOwner(s.getSettings))
	mux.HandleFunc("POST /settings", s.requireOwner(s.postSettings))
	mux.HandleFunc("POST /settings/test/storage", s.requireOwner(s.postTestStorage))
	mux.HandleFunc("POST /settings/test/mail", s.requireOwner(s.postTestMail))
	mux.HandleFunc("POST /settings/mail/retry", s.requireOwner(s.postMailRetry))
	mux.HandleFunc("POST /settings/team/role", s.requireOwner(s.postRole))
	mux.HandleFunc("POST /settings/team/invite", s.requireOwner(s.postInviteCreate))
	mux.HandleFunc("POST /settings/team/invite/{id}/resend", s.requireOwner(s.postInviteResend))
	mux.HandleFunc("POST /settings/team/invite/{id}/revoke", s.requireOwner(s.postInviteRevoke))
	mux.HandleFunc("POST /settings/tokens", s.requireOwner(s.postTokenCreate))
	mux.HandleFunc("POST /settings/tokens/{id}/revoke", s.requireOwner(s.postTokenRevoke))

	// Anything unclaimed is the 404 page rather than Go's plain text one.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.errorPage(w, r, http.StatusNotFound)
	})
	return mux
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	status := http.StatusOK
	var body string
	for _, c := range s.checks {
		if err := c.Run(r.Context()); err != nil {
			status = http.StatusServiceUnavailable
			body += fmt.Sprintf("%s: %v\n", c.Name, err)
			s.log.Error("readiness check failed", "check", c.Name, "err", err)
			continue
		}
		body += c.Name + ": ok\n"
	}
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}
