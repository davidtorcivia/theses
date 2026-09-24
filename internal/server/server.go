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

	"github.com/davidtorcivia/theses/internal/api"
	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/backup"
	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/config"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/integrations"
	"github.com/davidtorcivia/theses/internal/legal"
	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/mcp"
	"github.com/davidtorcivia/theses/internal/notify"
	"github.com/davidtorcivia/theses/internal/realtime"
	"github.com/davidtorcivia/theses/internal/safehttp"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
	"github.com/davidtorcivia/theses/internal/workflow"
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
	backups  *backup.Backup
	log      *slog.Logger
	version  string

	board *board.Service
	hub   *realtime.Hub
	docs  *docs.Service

	dev        bool
	assets     *assets
	templateFS fs.FS
	templates  map[string]*template.Template

	// hasUsers latches once the owner exists, so the setup gate costs one query.
	hasUsers atomic.Bool
	// ponytail: one publication at a time; use per-proposition guards if parallel publishing is needed.
	publishing atomic.Bool
	pending    *pendingStore
	checks     []Check
	handler    http.Handler

	api *api.API
	mcp *mcp.Server

	files *files.Service
	blobs *buckets

	notify *notify.Service

	// The two integrations. Each is kept rather than made per request, so that
	// an access token one of them refreshed outlives the request that fetched
	// it; both are handed the settings again on every use.
	drive      *integrations.Drive
	transistor *integrations.Transistor
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
	s.api = api.New(db, s.auth, set, log)
	s.mcp = mcp.New(s.api, db, set, log, version)

	s.backups = backup.New(cfg, db, set, log, version, func(ctx context.Context) (blob.Config, error) {
		return s.bucketConfig(ctx, "storage.primary")
	})

	// One bus, one command service, one hub. Everything that mutates goes
	// through the first and everything watching hangs off the second.
	s.board = board.New(core.New(db, core.NewBus()), func() board.Defaults {
		statuses := settings.Get[[]string](set, "defaults.statuses")
		status := "idea"
		if len(statuses) > 0 {
			status = statuses[0]
		}
		return board.Defaults{Status: status, Statuses: statuses,
			Columns: settings.Get[[]string](set, "defaults.columns")}
	})
	s.api.Workflow = workflow.New(s.board.Service)
	s.api.Legal = legal.New(s.board.Service, s.mail)
	s.hub = realtime.New(s.board, s.auth, log)
	s.docs = docs.New(s.board.Service, filepath.Join(cfg.DataDir, "docs"), func() string {
		return settings.Get[string](set, "defaults.document_template")
	}, log)
	s.backups.RestoreFiles = func(ctx context.Context, run func() error) error {
		return s.api.Workflow.WithPaused(ctx, func() error { return s.docs.WithMirrorPaused(ctx, run) })
	}
	s.backups.CheckObject = func(ctx context.Context, restored *settings.Settings, folder, key string, want int64) error {
		prefix := "storage.primary"
		if folder == files.Recordings && settings.Get[string](restored, "storage.recordings.bucket") != "" {
			prefix = "storage.recordings"
		}
		cfg, err := bucketConfigFor(ctx, restored, prefix)
		if err != nil {
			return err
		}
		client, err := blob.New(cfg)
		if err != nil {
			return err
		}
		size, _, err := client.Head(ctx, key)
		if err != nil {
			return err
		}
		if size != want {
			return fmt.Errorf("stored object size differs from backup")
		}
		return nil
	}
	s.api.Docs, s.hub.Docs, s.hub.Frozen = s.docs, s.docs, s.backups.Frozen
	// A new proposition arrives with the three documents every episode has, so
	// that nobody meets an empty document area and has to guess what goes in it.
	// The first takes the workspace template; the other two are their heading.
	s.board.Seed = func(ctx context.Context, a core.Actor, id int64) error {
		for _, name := range []string{"Research", "Script", "Show notes"} {
			if _, err := s.docs.CreateDocument(ctx, a, id, name); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := board.EnsureShow(context.Background(), db); err != nil {
		return nil, fmt.Errorf("ensure Show workspace: %w", err)
	}

	// Links and files hang off the same command service, registered after the
	// board because they chain onto the reader it set and share its rule about
	// an archived proposition. Metadata is fetched through the SSRF-safe
	// client, which is the only outbound fetch the app makes.
	s.blobs = newBuckets()
	s.files = files.New(s.board.Service, s.bucketFor, safehttp.Client())
	s.files.ReserveMaintenance = s.backups.ReserveMaintenance
	s.api.Board, s.api.Files, s.api.Backup = s.board, s.files, s.backups
	s.api.Workflow.Files = s.files
	s.api.Workflow.WhisperURL = cfg.WhisperURL
	if err := s.api.Workflow.Recover(context.Background()); err != nil {
		return nil, fmt.Errorf("recover transcription jobs: %w", err)
	}
	s.api.Diagnostics = s.diagnostics
	mcp.Workflow(s.mcp)
	mcp.Legal(s.mcp)
	mcp.Files(s.mcp, s.files)
	mcp.Board(s.mcp, s.board, s.files, s.backups.Now)

	s.AddCheck(Check{Name: "database", Run: func(ctx context.Context) error {
		var n int
		return db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n)
	}})
	s.AddCheck(Check{Name: "object store", Run: s.backups.CheckStore})
	s.AddCheck(Check{Name: "backup age", Run: s.backups.CheckAge})

	n, err := store.CountUsers(context.Background(), db)
	if err != nil {
		return nil, fmt.Errorf("count users: %w", err)
	}
	s.hasUsers.Store(n > 0)

	// The notifier watches the bus every command publishes on and fills its own
	// outbox; main runs its worker and stops it with the process.
	s.notify = notify.New(db, set, log, cfg.BaseURL)

	// Drive and Transistor share one outbound client, safehttp's, which is
	// what checks every address they resolve to before it is dialed.
	outbound := integrations.Client()
	s.drive = &integrations.Drive{HTTP: outbound, Save: s.saveDriveToken}
	s.transistor = &integrations.Transistor{HTTP: outbound}

	s.handler = s.chain(s.routes())
	return s, nil
}

// Mail is the outbox worker. main runs it and stops it with the process.
func (s *Server) Mail() *mail.Outbox { return s.mail }

// Backups is the archive scheduler. main runs it and stops it with the process.
func (s *Server) Backups() *backup.Backup { return s.backups }

// Docs is the document service. main runs its markdown mirror and watcher and
// stops them with the process.
func (s *Server) Docs() *docs.Service { return s.docs }

// Notify is the notifier. main runs its worker and its watcher over the bus the
// board publishes on, and stops both with the process.
func (s *Server) Notify() *notify.Service { return s.notify }

// Bus is what the notifier watches: the one bus every applied command is
// published on.
func (s *Server) Bus() *core.Bus { return s.board.Bus }

// AddCheck registers a readiness probe. Call it before the server starts serving.
func (s *Server) AddCheck(c Check) { s.checks = append(s.checks, c) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// CloseSockets ends every websocket and held poll, for http.Server.RegisterOnShutdown.
func (s *Server) CloseSockets() { s.hub.Close() }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", s.assets)
	for path := range agentDocumentPaths {
		mux.HandleFunc("GET "+path, s.getAgentDocument)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "theses", s.version)
	})
	mux.HandleFunc("GET /readyz", s.readyz)

	// The machine surfaces: bearer tokens instead of a session, and so outside
	// the setup gate and the CSRF check, but inside the headers and the log.
	mux.Handle("/api/v1/", s.api.Handler())
	mux.Handle("/mcp", s.mcp.Handler())
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
	mux.HandleFunc("GET /show", s.requireUser(s.getShell))
	mux.HandleFunc("GET /show/settings", s.requireUser(s.showSettings(s.getPropositionSettings)))
	mux.HandleFunc("POST /show/settings", s.requireUser(s.showSettings(s.postPropositionSettings)))
	mux.HandleFunc("GET /p/{id}", s.requireUser(s.getProposition))
	mux.HandleFunc("GET /p/{id}/settings", s.requireUser(s.getPropositionSettings))
	mux.HandleFunc("POST /p/{id}/settings", s.requireUser(s.postPropositionSettings))
	mux.HandleFunc("GET /documents/{id}/revisions", s.requireUser(s.getDocumentRevisions))

	// Both of these authenticate the session themselves, because one of them
	// answers on a connection the handler chain never gets to write to.
	mux.Handle("GET /ws", s.hub.Handler())
	mux.HandleFunc("GET /api/events", s.hub.Events)
	mux.HandleFunc("POST /app/commands", s.requireUser(s.hub.Commands))

	// The same stream for a token, at the path the plan names. It is more
	// specific than the API's own /api/v1/ pattern, so it wins the match, and
	// it is wrapped in the API's bearer middleware: a session cookie is not a
	// way in here. The handler is realtime's either way.
	mux.Handle("GET /api/v1/propositions/{id}/events", s.api.Authenticate(http.HandlerFunc(s.propositionEvents)))

	mux.HandleFunc("GET /calendar/{token}/production.ics", s.getCalendar)
	mux.HandleFunc("POST /profile/tokens", s.requireUser(s.postTokenCreate))
	mux.HandleFunc("POST /profile/tokens/{id}/revoke", s.requireUser(s.postTokenRevoke))
	mux.HandleFunc("POST /profile/calendar", s.requireUser(s.postCalendar))
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
	mux.HandleFunc("POST /settings/cors", s.requireOwner(s.postApplyCORS))
	mux.HandleFunc("POST /settings/test/mail", s.requireOwner(s.postTestMail))
	mux.HandleFunc("POST /settings/mail/retry", s.requireOwner(s.postMailRetry))
	mux.HandleFunc("POST /settings/test/backups", s.requireOwner(s.postTestBackupKey))
	mux.HandleFunc("POST /settings/backups/now", s.requireOwner(s.postBackupNow))
	mux.HandleFunc("POST /settings/backups/verify", s.requireOwner(s.postVerifyBackup))
	mux.HandleFunc("POST /settings/backups/restore", s.requireOwner(s.postRestore))
	mux.HandleFunc("POST /settings/team/role", s.requireOwner(s.postRole))
	mux.HandleFunc("POST /settings/team/invite", s.requireOwner(s.postInviteCreate))
	mux.HandleFunc("POST /settings/team/invite/{id}/resend", s.requireOwner(s.postInviteResend))
	mux.HandleFunc("POST /settings/team/invite/{id}/revoke", s.requireOwner(s.postInviteRevoke))
	mux.HandleFunc("POST /settings/tokens", s.requireOwner(s.postTokenCreate))
	mux.HandleFunc("POST /settings/tokens/{id}/revoke", s.requireOwner(s.postTokenRevoke))

	mux.HandleFunc("POST /profile/notifications", s.requireUser(s.postNotificationRules))
	mux.HandleFunc("POST /profile/notifications/channel", s.requireUser(s.postChannel))
	mux.HandleFunc("POST /profile/notifications/channel/{id}/test", s.requireUser(s.postChannelTest))
	mux.HandleFunc("POST /profile/notifications/channel/{id}/delete", s.requireUser(s.postChannelDelete))
	mux.HandleFunc("POST /settings/notifications/defaults", s.requireOwner(s.postNotifyDefaults))
	mux.HandleFunc("POST /settings/integrations/webhook", s.requireOwner(s.postWorkspaceWebhook))
	mux.HandleFunc("POST /settings/integrations/webhook/{id}/test", s.requireOwner(s.postWorkspaceWebhookTest))
	mux.HandleFunc("POST /settings/integrations/webhook/{id}/delete", s.requireOwner(s.postWorkspaceWebhookDelete))

	// Anything unclaimed is the 404 page rather than Go's plain text one.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.errorPage(w, r, http.StatusNotFound)
	})

	// Links and files for a token are part of /api/v1 and live on the API's own
	// mux. The same handlers under /app are the browser's, which has a session
	// instead of a token; it cannot use the first, because /api/ carries no
	// CSRF check.
	mux.Handle("/app/", s.requireUser(api.SessionHandler(s.api, s.files, userOf).ServeHTTP))
	mux.HandleFunc("POST /settings/test/cors", s.requireOwner(s.postTestCORS))

	// Offline. The worker is served from the root so its scope is the whole
	// site; the shell is what it answers an app navigation with when the
	// network is gone; the activity panel and the palette read through the
	// session, and their patterns are more specific than /app/ so they win the
	// match.
	mux.HandleFunc("GET /sw.js", s.serviceWorker)
	mux.HandleFunc("GET /shell", s.offlineShell)
	mux.HandleFunc("GET /app/activity", s.requireUser(s.getActivity))
	mux.HandleFunc("GET /app/search", s.requireUser(s.getSearch))
	mux.HandleFunc("GET /app/production-plans", s.requireUser(s.getProductionPlans))
	mux.HandleFunc("GET /app/production", s.requireUser(s.getProduction))
	mux.HandleFunc("GET /app/my-work", s.requireUser(s.getMyWork))
	mux.HandleFunc("GET /app/backlinks", s.requireUser(s.getBacklinks))

	// Integrations. Enrollment on the way in, for an account the
	// workspace requires an authenticator of and has none.
	mux.HandleFunc("GET /login/authenticator", s.getEnrol)
	mux.HandleFunc("POST /login/authenticator", s.postEnrol)

	mux.HandleFunc("POST /settings/integrations/drive/connect", s.requireOwner(s.postDriveConnect))
	mux.HandleFunc("GET "+driveCallback, s.requireOwner(s.getDriveCallback))
	mux.HandleFunc("POST /settings/integrations/drive/disconnect", s.requireOwner(s.postDriveDisconnect))
	mux.HandleFunc("POST /settings/integrations/transistor/disconnect", s.requireOwner(s.postTransistorDisconnect))
	mux.HandleFunc("POST /settings/test/drive", s.requireOwner(s.postTestDrive))
	mux.HandleFunc("POST /settings/test/transistor", s.requireOwner(s.postTestTransistor))

	// Add from Drive, in the files pane. More specific than the /app/ pattern
	// the links and files routes are mounted on, so these win the match.
	mux.HandleFunc("GET /app/drive", s.requireUser(s.getDriveList))
	mux.Handle("POST /app/drive/import", s.api.WithKey(http.HandlerFunc(s.requireUser(s.postDriveImport))))

	s.legalRoutes(mux)
	mux.HandleFunc("POST /p/{id}/publish", s.requireUser(s.postPublish))
	return mux
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	status := http.StatusOK
	var body string
	for _, c := range s.checks {
		if err := c.Run(r.Context()); err != nil {
			status = http.StatusServiceUnavailable
			// Named, not explained: this route has no session behind it, and
			// the reasons carry the endpoint, the bucket and the paths on
			// the container. Whoever can read the log can have those.
			body += c.Name + ": failed\n"
			s.log.Error("readiness check failed", "check", c.Name, "err", err)
			continue
		}
		body += c.Name + ": ok\n"
	}
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

func (s *Server) Workflow() *workflow.Service { return s.api.Workflow }
