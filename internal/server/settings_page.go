package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/backup"
	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

type option struct {
	Value, Label string
	On           bool
}

type memberView struct {
	ID                            int64
	Name, Initials, Colour, Email string
	LastSeen                      string
	Self                          bool
	RoleOptions                   []option
}

type inviteView struct {
	ID                             int64
	Email, Role, Sent, InviterName string
}

type tokenView struct {
	ID            int64
	Name, Scopes  string
	Created, Used string
}

type bucketView struct {
	Prefix                   string
	Endpoint, Region, Bucket string
	PublicBaseURL            string
	AccessSet, SecretSet     bool
	Providers                []option
	ProviderLabel            string
	Origin, CORS             string
}

type backupView struct {
	Prefix               string
	Enabled              bool
	Bucket               string
	AccessSet, SecretSet bool
	Running              bool
	State                string // what the last run did, in one line
	Failure              string // and why, when it failed
	Restore              string // what the last restore said
	Entries              []backupEntry
	ListError            string
}

type backupEntry struct {
	Key, Name, When, Size string
	Complete              bool
}

type envRow struct{ Name, Value, Why string }

var roleLabels = []option{
	{Value: auth.RoleOwner, Label: "Owner"},
	{Value: auth.RoleEditor, Label: "Editor"},
	{Value: auth.RoleResearcher, Label: "Researcher"},
	{Value: auth.RoleGuest, Label: "Guest"},
}

// blobProvider maps the settings vocabulary to internal/blob's.
var blobProvider = map[string]string{"backblaze": "b2", "r2": "r2", "s3": "s3"}

var providerLabels = []option{
	{Value: "backblaze", Label: "Backblaze B2"},
	{Value: "r2", Label: "Cloudflare R2"},
	{Value: "s3", Label: "Other S3"},
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	data, err := s.settingsData(r, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "settings.html", s.said(w, r, data, settingsSections))
}

// settingsSections are the anchors a form on the settings page may send the
// browser back to. Anything else is dropped rather than put in a Location.
var settingsSections = map[string]bool{
	"workspace": true, "defaults": true, "storage": true, "mail": true, "signin": true,
	"backups": true, "team": true, "tokens": true, "notifications": true, "integrations": true,
}

// settingsTo is where a form on the settings page sends the browser: the
// section it posted from, with the saved notice on the way if it saved. The
// section is in the query as well as the fragment, because the fragment never
// reaches this process and the notice has to be printed in the right section.
func settingsTo(section string, saved bool) string {
	return pageTo("/settings", section, settingsSections[section], saved)
}

func pageTo(path, section string, known, saved bool) string {
	if !known {
		section = ""
	}
	to := path
	if saved {
		to += "?saved=" + cmp.Or(section, "1")
	}
	if section != "" {
		to += "#" + section
	}
	return to
}

// sectionOf is which section of the page a form posted from, taken from the
// address a redirect built.
func sectionOf(to string, known map[string]bool) string {
	_, section, _ := strings.Cut(to, "#")
	if !known[section] {
		return ""
	}
	return section
}

// said adds what the last form had to say to a page that is about to render:
// the saved notice the address carries, and the flash. The flash is spent here
// rather than at the top of the handler so that a page which cannot be built
// does not eat the one thing it was meant to print.
func (s *Server) said(w http.ResponseWriter, r *http.Request, data map[string]any, known map[string]bool) map[string]any {
	if v := r.URL.Query().Get("saved"); v != "" {
		data["Notice"] = "Saved."
		if known[v] {
			data["Section"] = v
		}
	}
	if f := s.pending.takeFlash(w, r, s.cfg.CookieSecure, userOf(r).ID); f != nil {
		data["Section"] = f.Section
		merge(data, f.Say)
	}
	return data
}

// flashValueMax is how much of one value the cookie carries. A provider is
// free to answer with kilobytes, and a cookie over about four of them is one
// the browser drops without a word, taking the whole notice with it.
const flashValueMax = 1024

// cutForFlash is one value shortened to what the cookie can carry. The cut is
// by byte and a provider answers in whatever alphabet it likes, so the rune the
// cut lands inside goes rather than half of it, which would reach the page as a
// replacement character or not at all.
func cutForFlash(text string) string {
	if len(text) <= flashValueMax {
		return text
	}
	return strings.ToValidUTF8(text[:flashValueMax], "") + "… The rest is in the log."
}

// back is how every form on the settings and profile pages answers. The
// browser goes to the section it posted from, carrying what the form has to
// say in a one-time cookie. Rendering the answer instead left it on an address
// that only accepts POST, at the top of a page thousands of pixels long, where
// a reload or the Back button found nothing at all.
func (s *Server) back(w http.ResponseWriter, r *http.Request, to string, say map[string]any) {
	if len(say) > 0 {
		path, _, _ := strings.Cut(to, "#")
		path, _, _ = strings.Cut(path, "?")
		known := settingsSections
		if path == "/profile" {
			known = profileSections
		}
		for k, v := range say {
			text, ok := v.(string)
			if !ok || len(text) <= flashValueMax {
				continue
			}
			s.log.Warn("a notice was too long for the page to carry", "key", k, "said", text)
			say[k] = cutForFlash(text)
		}
		f := &flash{UserID: userOf(r).ID, Path: path, Section: sectionOf(to, known), Say: say}
		if err := s.pending.putFlash(w, s.cfg.CookieSecure, f); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) settingsData(r *http.Request, extra map[string]any) (map[string]any, error) {
	ctx := r.Context()
	me := userOf(r)

	shown := map[string]string{}
	isSet := map[string]bool{}
	for _, d := range settings.Registry {
		isSet[d.Key] = s.settings.IsSet(d.Key)
		if d.Secret {
			continue
		}
		switch d.Kind {
		case settings.KindList:
			shown[d.Key] = strings.Join(settings.Get[[]string](s.settings, d.Key), "\n")
		case settings.KindInt:
			shown[d.Key] = strconv.Itoa(settings.Get[int](s.settings, d.Key))
		default:
			shown[d.Key] = settings.Get[string](s.settings, d.Key)
		}
	}

	users, err := store.ListUsers(ctx, s.db)
	if err != nil {
		return nil, err
	}
	members := make([]memberView, 0, len(users))
	for _, u := range users {
		roles := make([]option, len(roleLabels))
		copy(roles, roleLabels)
		for i := range roles {
			roles[i].On = roles[i].Value == u.Role
		}
		members = append(members, memberView{
			ID: u.ID, Name: u.Name, Initials: u.Initials, Colour: u.Colour, Email: u.Email,
			LastSeen: ago(u.LastSeenAt.Int64, u.LastSeenAt.Valid), Self: u.ID == me.ID, RoleOptions: roles,
		})
	}

	invitations, err := store.ListPendingInvitations(ctx, s.db, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	invites := make([]inviteView, 0, len(invitations))
	for _, i := range invitations {
		invites = append(invites, inviteView{
			ID: i.ID, Email: i.Email, Role: i.Role,
			Sent: "sent " + on(i.CreatedAt), InviterName: i.InviterName,
		})
	}

	apiTokens, err := store.ListAPITokens(ctx, s.db)
	if err != nil {
		return nil, err
	}
	tokens := make([]tokenView, 0, len(apiTokens))
	for _, t := range apiTokens {
		used := "never used"
		if t.LastUsedAt.Valid {
			used = "last used " + on(t.LastUsedAt.Int64)
		}
		tokens = append(tokens, tokenView{ID: t.ID, Name: t.Name, Scopes: t.Scopes,
			Created: on(t.CreatedAt), Used: used})
	}

	outbox, err := s.mail.State(ctx)
	if err != nil {
		return nil, err
	}
	// The reason mail cannot go out yet, shown on the section that fixes it.
	mailProblem := ""
	if _, err := s.mail.Sender(ctx); err != nil {
		mailProblem = err.Error()
	}

	data := map[string]any{
		"Plain":       true,
		"Section":     "",
		"S":           shown,
		"Set":         isSet,
		"Days":        []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"},
		"TLSModes":    []string{"starttls", "tls", "none"},
		"Primary":     s.bucket("storage.primary", shown, isSet),
		"Recordings":  s.bucket("storage.recordings", shown, isSet),
		"Members":     members,
		"Invitations": invites,
		"Tokens":      tokens,
		"InviteRoles": roleLabels[1:], // everything but owner
		"Env":         s.envRows(),
		"Outbox":      outbox,
		"MailProblem": mailProblem,
		"Backups":     s.backupSection(r, shown, isSet),
		"Ints":        s.integrationsSection(),
	}
	notifications, err := s.notifySettings(r)
	if err != nil {
		return nil, err
	}
	return s.page(r, "Settings", merge(merge(data, notifications), extra)), nil
}

// backupSection is the Backups row. The listing is a request to the bucket, so
// it is skipped while the key is missing and its failure is printed inside the
// section rather than taken out on the whole page.
func (s *Server) backupSection(r *http.Request, shown map[string]string, isSet map[string]bool) backupView {
	v := backupView{
		Prefix:    backup.Prefix,
		Enabled:   shown["backups.enabled"] == "on",
		Bucket:    shown["backups.bucket"],
		AccessSet: isSet["backups.access_key"],
		SecretSet: isSet["backups.secret_key"],
		Running:   s.backups.Running(),
		Restore:   s.backups.LastRestore(),
		Failure:   settings.Get[string](s.settings, "backups.last_error"),
	}
	if at := settings.Get[int](s.settings, "backups.last_ok_at"); at > 0 {
		v.State = fmt.Sprintf("Last backup %s, %s.", ago(int64(at), true),
			size(int64(settings.Get[int](s.settings, "backups.last_size"))))
	} else {
		v.State = "No backup has been taken yet."
	}
	if !v.AccessSet || !v.SecretSet {
		return v
	}

	// Shorter than the probe: this one is on the way to a page the owner is
	// waiting for, not a button they pressed to test a bucket.
	ctx, cancel := context.WithTimeout(r.Context(), listTimeout)
	defer cancel()
	entries, err := s.backups.List(ctx)
	if err != nil {
		v.ListError = err.Error()
		return v
	}
	for _, e := range entries {
		v.Entries = append(v.Entries, backupEntry{
			Key:      e.Key,
			Name:     strings.TrimPrefix(e.Key, backup.Prefix),
			When:     e.When.Format("2 Jan 2006 15:04 UTC"),
			Size:     size(e.Size),
			Complete: e.Manifest != "",
		})
	}
	return v
}

// size is a byte count as the page prints it, which is to two figures because
// nobody reads the third.
func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// postTestBackupKey runs the probe behind "Test backup key": it writes and
// lists under the prefix and then tries to delete what it wrote. The refusal is
// the pass, so the result says so in as many words.
func (s *Server) postTestBackupKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	msg, err := s.backups.Probe(ctx)
	if err != nil {
		s.back(w, r, "/settings#backups", map[string]any{
			"BackupResult": err.Error(), "BackupFailed": true,
		})
		return
	}
	s.back(w, r, "/settings#backups", map[string]any{"BackupResult": msg})
}

func (s *Server) postBackupNow(w http.ResponseWriter, r *http.Request) {
	if err := s.backups.Now(r.Context()); err != nil {
		s.back(w, r, "/settings#backups", map[string]any{
			"BackupResult": err.Error(), "BackupFailed": true,
		})
		return
	}
	s.back(w, r, "/settings#backups", map[string]any{
		"BackupResult": "Started. It appears in the list below, and says here what it did, once it has finished.",
	})
}

// postRestore starts a restore of one listed archive. It answers straight away
// because the restore outlives the request; what it did is on this page after.
func (s *Server) postRestore(w http.ResponseWriter, r *http.Request) {
	key := r.PostFormValue("key")
	if !strings.HasPrefix(key, backup.Prefix) || !strings.HasSuffix(key, ".tar.gz.age") {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if err := s.backups.RestoreNow(r.Context(), key, userOf(r).ID); err != nil {
		s.back(w, r, "/settings#backups", map[string]any{
			"BackupResult": err.Error(), "BackupFailed": true,
		})
		return
	}
	s.back(w, r, "/settings#backups", map[string]any{
		"BackupResult": "Restoring " + strings.TrimPrefix(key, backup.Prefix) +
			". Changes are refused until it is done, and this page says what happened when it is.",
	})
}

func (s *Server) bucket(prefix string, shown map[string]string, isSet map[string]bool) bucketView {
	providers := make([]option, len(providerLabels))
	copy(providers, providerLabels)
	label := ""
	for i := range providers {
		providers[i].On = providers[i].Value == shown[prefix+".provider"]
		if providers[i].On {
			label = providers[i].Label
		}
	}
	origin := s.origin()
	return bucketView{
		Prefix:        prefix,
		Endpoint:      shown[prefix+".endpoint"],
		Region:        shown[prefix+".region"],
		Bucket:        shown[prefix+".bucket"],
		PublicBaseURL: shown[prefix+".public_base_url"],
		AccessSet:     isSet[prefix+".access_key"],
		SecretSet:     isSet[prefix+".secret_key"],
		Providers:     providers,
		ProviderLabel: label,
		Origin:        origin,
		CORS:          corsRule(shown[prefix+".provider"], origin),
	}
}

// origin is the scheme and host of THESES_BASE_URL, which is what the bucket's
// CORS rule has to allow: the browser sends the origin, never the path.
func (s *Server) origin() string {
	u, err := url.Parse(s.cfg.BaseURL)
	if err != nil || u.Host == "" {
		return s.cfg.BaseURL
	}
	return u.Scheme + "://" + u.Host
}

// corsRule is the document for the chosen provider. B2 keeps its own rule shape
// even behind the S3 endpoint; everything else takes the S3 one.
func corsRule(provider, origin string) string {
	if provider == "backblaze" {
		return blob.CORSRuleB2(origin)
	}
	return blob.CORSRuleR2(origin)
}

func (s *Server) envRows() []envRow {
	set := func(b bool) string {
		if b {
			return "set"
		}
		return "not set"
	}
	return []envRow{
		{"THESES_BIND", s.cfg.Bind, "The address is needed before anything, including the database, is open."},
		{"THESES_DATA_DIR", s.cfg.DataDir, "The database this page is stored in lives there."},
		{"THESES_BASE_URL", s.cfg.BaseURL, "Links in mail and the Secure flag on cookies depend on it, so it has to be right before the first sign-in."},
		{"THESES_SECRET_KEY", set(len(s.cfg.SecretKey) > 0), "It encrypts the secrets on this page, so it cannot be one of them."},
		{"THESES_SESSION_KEY", set(len(s.cfg.SessionKey) > 0), "It signs the cookie that says you are allowed to see this page."},
		{"THESES_TRUST_PROXY", strconv.FormatBool(s.cfg.TrustProxy), "Whether the address a proxy forwards is believed depends on there being a proxy, which is a deployment fact rather than a preference."},
		{"THESES_DEV", strconv.FormatBool(s.cfg.Dev), "Templates and static files are read from disk rather than the binary."},
		{"THESES_LOG_LEVEL", s.cfg.LogLevel.String(), "Logging starts before the database does."},
	}
}

// postSettings saves every registry key the submitted form carried, so one
// handler serves all five field sections.
func (s *Server) postSettings(w http.ResponseWriter, r *http.Request) {
	me := userOf(r)
	section := r.PostFormValue("section")
	for _, d := range settings.Registry {
		values, ok := r.PostForm[d.Key]
		if !ok {
			continue
		}
		if d.Secret && strings.TrimSpace(strings.Join(values, "")) == "" {
			continue // an empty secret field means keep what is stored
		}
		if err := s.settings.Set(r.Context(), d.Key, values, me.ID); err != nil {
			s.back(w, r, settingsTo(section, false), map[string]any{"Error": err.Error()})
			return
		}
	}
	http.Redirect(w, r, settingsTo(section, true), http.StatusSeeOther)
}

// postTestStorage writes, heads and deletes one probe object with the saved
// credentials. Whether the bucket's CORS rule is right is not checked here; that
// needs a preflight from the browser and belongs with the upload code.
func (s *Server) postTestStorage(w http.ResponseWriter, r *http.Request) {
	prefix := r.PostFormValue("prefix")
	if prefix != "storage.primary" && prefix != "storage.recordings" {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	cfg, err := s.bucketConfig(r.Context(), prefix)
	refuse := func(msg string) {
		s.back(w, r, "/settings#storage", map[string]any{
			"StorageResult": mail.Redact(msg, cfg.SecretKey), "StorageFailed": true,
		})
	}
	if err != nil {
		refuse(err.Error())
		return
	}
	c, err := blob.New(cfg)
	if err != nil {
		refuse(err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	if err := c.Probe(ctx); err != nil {
		refuse(err.Error())
		return
	}
	s.back(w, r, "/settings#storage", map[string]any{
		"StorageResult": "Wrote, read and deleted a probe object in " + cfg.Bucket + ".",
	})
}

// postApplyCORS sets the printed rule on the bucket with the saved keys and
// reads it back, so the answer is what the bucket holds rather than what was
// sent to it. A key without the capability is refused by the provider, whose
// sentence is printed with the reminder that the rule can still be pasted in.
func (s *Server) postApplyCORS(w http.ResponseWriter, r *http.Request) {
	prefix := r.PostFormValue("prefix")
	if prefix != "storage.primary" && prefix != "storage.recordings" {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	cfg, err := s.bucketConfig(r.Context(), prefix)
	refuse := func(msg string) {
		s.back(w, r, "/settings#storage", map[string]any{
			"StorageResult": mail.Redact(msg, cfg.SecretKey) + " " + corsByHand,
			"StorageFailed": true,
		})
	}
	if err != nil {
		refuse(err.Error())
		return
	}
	c, err := blob.New(cfg)
	if err != nil {
		refuse(err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	origin := s.origin()
	if err := c.PutCORS(ctx, origin); err != nil {
		refuse(err.Error())
		return
	}
	allowed, err := c.CORSAllows(ctx, origin)
	if err != nil {
		refuse(err.Error())
		return
	}
	if !allowed {
		refuse("The rule went to " + cfg.Bucket + " and reading it back did not find " + origin + ".")
		return
	}
	s.back(w, r, "/settings#storage", map[string]any{
		"StorageResult": "Applied the rule to " + cfg.Bucket + ", which now allows " + origin + ".",
	})
}

// corsByHand ends every refusal: the button is a convenience, and the rule
// above it is still there to paste into the provider's own tools.
const corsByHand = "The rule can still be applied by hand."

// probeTimeout bounds the whole three-call probe, so a bucket that accepts the
// connection and then says nothing does not hold the settings page open.
const probeTimeout = 30 * time.Second

// listTimeout bounds the listing the Backups section renders.
const listTimeout = 10 * time.Second

func (s *Server) bucketConfig(ctx context.Context, prefix string) (blob.Config, error) {
	get := func(name string) string { return settings.Get[string](s.settings, prefix+"."+name) }
	access, err := s.settings.Secret(ctx, prefix+".access_key")
	if err != nil {
		return blob.Config{}, err
	}
	secret, err := s.settings.Secret(ctx, prefix+".secret_key")
	if err != nil {
		return blob.Config{}, err
	}
	return blob.Config{
		Provider:      blobProvider[get("provider")],
		Endpoint:      get("endpoint"),
		Region:        get("region"),
		Bucket:        get("bucket"),
		AccessKey:     access,
		SecretKey:     secret,
		PublicBaseURL: get("public_base_url"),
	}, nil
}

// postTestMail sends to the owner who asked, there and then rather than through
// the outbox, because the point is to see the SMTP server answer.
func (s *Server) postTestMail(w http.ResponseWriter, r *http.Request) {
	me := userOf(r)
	refuse := func(msg string) {
		s.back(w, r, "/settings#mail", map[string]any{"MailResult": msg, "MailFailed": true})
	}
	if me.Email == "" {
		refuse("Your account has no email address, so there is nowhere to send it. Add one on your profile first.")
		return
	}
	sender, err := s.mail.Sender(r.Context())
	if err != nil {
		refuse(err.Error())
		return
	}
	if err := sender.Send(r.Context(), mail.Message{
		To:      []string{me.Email},
		Subject: "THESES test message",
		Text: "This is the test message from the Mail section of the THESES settings page." +
			"\n\nIf it arrived, invitations, password resets and notifications will too.",
	}); err != nil {
		refuse(mail.Redact(err.Error(), sender.Password))
		return
	}
	s.back(w, r, "/settings#mail", map[string]any{"MailResult": "Sent to " + me.Email + "."})
}

func (s *Server) postMailRetry(w http.ResponseWriter, r *http.Request) {
	if err := s.mail.RetryNow(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.back(w, r, "/settings#mail", map[string]any{
		"MailResult": "Every unsent message is back at the front of the queue.",
	})
}

func (s *Server) postRole(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PostFormValue("user"), 10, 64)
	role := r.PostFormValue("role")
	if err != nil || !auth.ValidRole(role) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if id == userOf(r).ID {
		s.back(w, r, "/settings#team", map[string]any{"Error": "You cannot change your own role."})
		return
	}
	u, err := store.UserByID(r.Context(), s.db, id)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.write(r, "user", itoa(id), "role", u.Role, role, func(q store.Querier) error {
		kept, err := store.SetUserRoleKeepingAnOwner(r.Context(), q, id, role)
		if err != nil {
			return err
		}
		if !kept {
			return errRefused
		}
		return nil
	}); err != nil {
		if errors.Is(err, errRefused) {
			s.back(w, r, "/settings#team", map[string]any{
				"Error": "That is the last owner. Make someone else an owner first.",
			})
			return
		}
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, settingsTo("team", true), http.StatusSeeOther)
}

func (s *Server) postInviteCreate(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.PostFormValue("email"))
	if !s.auth.Allow(auth.BucketInvite, s.auth.ClientIP(r), strings.ToLower(email)) {
		s.back(w, r, "/settings#team", map[string]any{"Error": auth.ErrRateLimited.Error()})
		return
	}
	role := r.PostFormValue("role")
	id, token, err := s.auth.CreateInvitation(r.Context(), email, role, userOf(r).ID)
	if err != nil {
		s.back(w, r, "/settings#team", map[string]any{"Error": err.Error()})
		return
	}
	// ponytail: the invitation row is already committed by auth on its own
	// handle, so this is a second transaction and a failure here leaves an
	// invitation with no mail; give CreateInvitation and ReissueInvitation a
	// store.Querier and pass this one when auth is next opened.
	said := s.inviteMailSaid(r, s.write(r, "invitation", email, "create", "", role, func(q store.Querier) error {
		return mail.Enqueue(r.Context(), q, s.inviteMessage(r, email, role, token),
			s.auth.Now().Add(auth.InviteValidity), inviteRef(id))
	}), email)
	s.mail.Nudge()
	s.logInvite(email)
	// The link travels in the flash rather than in the address, for the same
	// reason as a new API token: this is the only time it exists anywhere it
	// can be read from.
	s.back(w, r, "/settings#team", map[string]any{"NewInvite": s.inviteURL(token), "InviteSaid": said})
}

// inviteMailSaid is the line above a new invitation link. The invitation is
// made whether or not the mail goes out, so the notice has to say which of the
// three happened: sent, waiting in the outbox because no mail server is set up
// yet, or not queued at all because the write failed. In every one of them the
// link beside it is what gets the person in.
func (s *Server) inviteMailSaid(r *http.Request, enqueued error, to string) string {
	if enqueued != nil {
		s.log.Error("the invitation mail could not be queued", "err", enqueued)
		return "The invitation was made, but no mail could be queued for it."
	}
	if _, err := s.mail.Sender(r.Context()); err != nil {
		return "Mail is not configured, so the invitation is queued and nothing has gone out to " + to + " yet."
	}
	return "An invitation is on its way to " + to + "."
}

func (s *Server) postInviteResend(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if !s.auth.Allow(auth.BucketInvite, s.auth.ClientIP(r), "invitation "+itoa(id)) {
		s.back(w, r, "/settings#team", map[string]any{"Error": auth.ErrRateLimited.Error()})
		return
	}
	// Read before the reissue, because the mail needs the address and the role
	// and neither comes back with the new token.
	inv, err := store.InvitationByID(r.Context(), s.db, id)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// An invitation that has already been accepted keeps its row, and reissuing
	// it stores nothing, so the link would open nothing. The statement reports
	// that it changed no row and this answers as if the invitation were gone.
	token, err := s.auth.ReissueInvitation(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// ponytail: the reissued token is committed separately, as on create.
	said := s.inviteMailSaid(r, s.write(r, "invitation", itoa(id), "resend", "", "", func(q store.Querier) error {
		// The ref abandons the mail from the last time, whose link the reissue
		// above has just killed.
		return mail.Enqueue(r.Context(), q, s.inviteMessage(r, inv.Email, inv.Role, token),
			s.auth.Now().Add(auth.InviteValidity), inviteRef(id))
	}), inv.Email)
	s.mail.Nudge()
	s.logInvite("invitation " + itoa(id))
	s.back(w, r, "/settings#team", map[string]any{"NewInvite": s.inviteURL(token), "InviteSaid": said})
}

func (s *Server) postInviteRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if err := s.write(r, "invitation", itoa(id), "revoke", "", "", func(q store.Querier) error {
		if err := store.DeleteInvitation(r.Context(), q, id); err != nil {
			return err
		}
		// The token dies here, so the mail still carrying it dies with it.
		return mail.Abandon(r.Context(), q, inviteRef(id), mail.Revoked)
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, settingsTo("team", true), http.StatusSeeOther)
}

func (s *Server) inviteURL(token string) string { return s.cfg.BaseURL + "/invite/" + token }

// inviteRef files a queued invitation mail under the invitation it came from,
// so a resend can abandon the one it replaces.
func inviteRef(id int64) string { return "invitation:" + itoa(id) }

func (s *Server) inviteMessage(r *http.Request, email, role, token string) mail.Message {
	return mail.Invite{
		To:      email,
		Inviter: userOf(r).Name,
		Role:    role,
		URL:     s.inviteURL(token),
		Expires: auth.InviteValidity,
	}.Message()
}

// The link is deliberately not logged, because anything that can read the log
// could accept the invitation with it. The mail carries it, and the page that
// made it shows it once so the owner can pass it on by hand as well.
func (s *Server) logInvite(who string) {
	s.log.Info("invitation issued", "to", who)
}

func (s *Server) postTokenCreate(w http.ResponseWriter, r *http.Request) {
	scopes := strings.Fields(r.PostFormValue("scopes"))
	token, err := s.auth.CreateAPIToken(r.Context(), userOf(r).ID, strings.TrimSpace(r.PostFormValue("name")), scopes)
	if err != nil {
		s.back(w, r, "/settings#tokens", map[string]any{"Error": err.Error()})
		return
	}
	if err := s.activity(r.Context(), userOf(r).ID, "api_token", r.PostFormValue("name"), "create", "", r.PostFormValue("scopes")); err != nil {
		s.fail(w, r, err)
		return
	}
	// The token is shown once and nowhere else, so it reaches the page it is
	// printed on in the flash rather than in the address.
	s.back(w, r, "/settings#tokens", map[string]any{"NewToken": token})
}

func (s *Server) postTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if err := s.write(r, "api_token", itoa(id), "revoke", "", "", func(q store.Querier) error {
		return store.RevokeAPIToken(r.Context(), q, id)
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, settingsTo("tokens", true), http.StatusSeeOther)
}

// activity records a mutation that could not share a transaction with its
// change, because the change went through a package holding its own handle.
func (s *Server) activity(ctx context.Context, actorID int64, entity, entityID, action, before, after string) error {
	return store.InsertActivity(ctx, s.db, "user", itoa(actorID), "", entity, entityID, action, before, after)
}

// errRefused is what a write closure returns when the statement it guards
// changed nothing. It travels back through s.write so the activity row rolls
// back with the write that did not happen.
var errRefused = errors.New("the write was refused by its own guard")

// write applies a mutation and its activity row in one transaction.
func (s *Server) write(r *http.Request, entity, entityID, action, before, after string, apply func(store.Querier) error) error {
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := apply(tx); err != nil {
		return err
	}
	actor := ""
	if u := userOf(r); u != nil {
		actor = itoa(u.ID)
	}
	if err := store.InsertActivity(r.Context(), tx, "user", actor, "", entity, entityID, action, before, after); err != nil {
		return err
	}
	return tx.Commit()
}

func merge(into, extra map[string]any) map[string]any {
	for k, v := range extra {
		into[k] = v
	}
	return into
}

func on(unix int64) string { return time.Unix(unix, 0).Format("2 Jan 2006") }

func ago(unix int64, valid bool) string {
	if !valid {
		return "never"
	}
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Hour:
		return "now"
	case d < 24*time.Hour:
		return "today"
	case d < 48*time.Hour:
		return "yesterday"
	}
	return on(unix)
}
