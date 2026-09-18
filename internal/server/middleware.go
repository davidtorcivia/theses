package server

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/store"
)

// The CSP the plan asks for. Nothing is inline: no script tags with bodies, no
// event handlers, no style attributes, no third party anything.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; form-action 'self'"

type ctxKey int

const (
	seedKey ctxKey = iota
	userKey
)

func (s *Server) chain(h http.Handler) http.Handler {
	return s.logAndRecover(s.securityHeaders(s.browserSeed(s.setupGate(s.writeGate(s.csrfGuard(h))))))
}

// recorder keeps the status for the log line and tells the recoverer whether a
// response has already started.
type recorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (rec *recorder) WriteHeader(code int) {
	if !rec.written {
		rec.status, rec.written = code, true
		rec.ResponseWriter.WriteHeader(code)
	}
}

// Hijack hands the raw connection to the websocket upgrade. Embedding the
// ResponseWriter interface promotes only its three methods, so without this the
// upgrade fails on every socket that goes through the middleware chain.
func (rec *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rec.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("this response writer cannot be hijacked")
	}
	rec.written = true
	return h.Hijack()
}

func (rec *recorder) Write(b []byte) (int, error) {
	if !rec.written {
		rec.status, rec.written = http.StatusOK, true
	}
	return rec.ResponseWriter.Write(b)
}

// logAndRecover is the outermost middleware: it logs every request with its
// status and turns a panic into a 500 when nothing has been written yet.
func (s *Server) logAndRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "panic", v)
				if !rec.written {
					s.fail(w, r, errors.New("panic"))
				}
			}
			s.log.Info("request",
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"ms", time.Since(start).Milliseconds(), "addr", s.auth.ClientIP(r))
		}()
		next.ServeHTTP(rec, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// machinePath reports whether a request is for the API or MCP: a bearer token
// rather than a session, so no CSRF token, no setup redirect and no cookie of
// its own. The security headers and the logging still apply.
func machinePath(p string) bool {
	return strings.HasPrefix(p, "/api/") || p == "/mcp"
}

// browserSeed puts the value a CSRF token is bound to into the context: the
// session cookie once signed in, a cookie of its own before that.
func (s *Server) browserSeed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if machinePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		seed := ""
		if c, err := r.Cookie(auth.SessionCookie); err == nil {
			seed = c.Value
		} else if c, err := r.Cookie(auth.CSRFCookie); err == nil {
			seed = c.Value
		} else if r.Method == http.MethodGet {
			c, err := s.auth.NewBrowserCookie(auth.CSRFCookie)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			http.SetCookie(w, c)
			seed = c.Value
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), seedKey, seed)))
	})
}

func seedOf(r *http.Request) string {
	seed, _ := r.Context().Value(seedKey).(string)
	return seed
}

// setupGate sends everything to /setup until an owner exists.
func (s *Server) setupGate(next http.Handler) http.Handler {
	exempt := []string{"/setup", "/static/", "/healthz", "/readyz"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if machinePath(r.URL.Path) || s.hasUsers.Load() {
			next.ServeHTTP(w, r)
			return
		}
		n, err := store.CountUsers(r.Context(), s.db)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if n > 0 {
			s.hasUsers.Store(true)
			next.ServeHTTP(w, r)
			return
		}
		for _, p := range exempt {
			if r.URL.Path == p || strings.HasPrefix(r.URL.Path, p) {
				next.ServeHTTP(w, r)
				return
			}
		}
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	})
}

// writeGate refuses mutations while a restore is replacing the database.
// Reading still works throughout; anything that would write gets the page an
// offline browser gets, with a 503 so a proxy, a script or the service worker
// knows to come back rather than to treat it as done.
func (s *Server) writeGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if s.backups.Frozen() {
				s.restoringPage(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// maxFormBytes is what a POST body may be. Every form here is a few hundred
// bytes; file uploads go to object storage from the browser and never through
// this process.
const maxFormBytes = 64 << 10

// csrfGuard parses every form and checks its token against this browser's seed.
func (s *Server) csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || machinePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if err := r.ParseForm(); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				s.errorPage(w, r, http.StatusRequestEntityTooLarge)
				return
			}
			s.errorPage(w, r, http.StatusForbidden)
			return
		}
		if !s.auth.CheckCSRF(seedOf(r), r.PostFormValue("csrf")) {
			s.log.Warn("csrf token rejected", "path", r.URL.Path, "addr", s.auth.ClientIP(r))
			s.errorPage(w, r, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireUser resolves the session or sends the visitor to sign in.
func (s *Server) requireUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.auth.SessionUser(r.Context(), r)
		if errors.Is(err, store.ErrNotFound) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

func (s *Server) requireOwner(h http.HandlerFunc) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request) {
		if !auth.Can(userOf(r).Role, auth.CanSettings) {
			s.errorPage(w, r, http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

func userOf(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}
