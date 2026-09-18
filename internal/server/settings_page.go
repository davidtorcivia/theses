package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
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
}

type envRow struct{ Name, Value, Why string }

var roleLabels = []option{
	{Value: auth.RoleOwner, Label: "Owner"},
	{Value: auth.RoleEditor, Label: "Editor"},
	{Value: auth.RoleResearcher, Label: "Researcher"},
	{Value: auth.RoleGuest, Label: "Guest"},
}

var providerLabels = []option{
	{Value: "backblaze", Label: "Backblaze B2"},
	{Value: "r2", Label: "Cloudflare R2"},
	{Value: "s3", Label: "Other S3"},
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	extra := map[string]any{}
	if r.URL.Query().Get("saved") != "" {
		extra["Notice"] = "Saved."
	}
	s.renderSettings(w, r, http.StatusOK, extra)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, extra map[string]any) {
	data, err := s.settingsData(r, extra)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, status, "settings.html", data)
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

	data := map[string]any{
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
	}
	return s.page(r, "Settings", merge(data, extra)), nil
}

func (s *Server) bucket(prefix string, shown map[string]string, isSet map[string]bool) bucketView {
	providers := make([]option, len(providerLabels))
	copy(providers, providerLabels)
	for i := range providers {
		providers[i].On = providers[i].Value == shown[prefix+".provider"]
	}
	return bucketView{
		Prefix:        prefix,
		Endpoint:      shown[prefix+".endpoint"],
		Region:        shown[prefix+".region"],
		Bucket:        shown[prefix+".bucket"],
		PublicBaseURL: shown[prefix+".public_base_url"],
		AccessSet:     isSet[prefix+".access_key"],
		SecretSet:     isSet[prefix+".secret_key"],
		Providers:     providers,
	}
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
		{"THESES_TRUST_PROXY", strconv.FormatBool(s.cfg.TrustProxy), "Whether a forwarded address is believed is a deployment fact, not a preference."},
		{"THESES_DEV", strconv.FormatBool(s.cfg.Dev), "Templates and static files are read from disk rather than the binary."},
		{"THESES_LOG_LEVEL", s.cfg.LogLevel.String(), "Logging starts before the database does."},
	}
}

// postSettings saves every registry key the submitted form carried, so one
// handler serves all five field sections.
func (s *Server) postSettings(w http.ResponseWriter, r *http.Request) {
	me := userOf(r)
	for _, d := range settings.Registry {
		values, ok := r.PostForm[d.Key]
		if !ok {
			continue
		}
		if d.Secret && strings.TrimSpace(strings.Join(values, "")) == "" {
			continue // an empty secret field means keep what is stored
		}
		if err := s.settings.Set(r.Context(), d.Key, values, me.ID); err != nil {
			s.renderSettings(w, r, http.StatusUnprocessableEntity, map[string]any{"Error": err.Error()})
			return
		}
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func (s *Server) postTestStorage(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, map[string]any{
		"Error": "Testing the bucket is not wired yet. It arrives with the files step.",
	})
}

func (s *Server) postTestMail(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, map[string]any{
		"Error": "Sending a test message is not wired yet. It arrives with the mail step.",
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
		s.renderSettings(w, r, http.StatusUnprocessableEntity, map[string]any{
			"Error": "You cannot change your own role.",
		})
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
	kept := false
	if err := s.write(r, "user", itoa(id), "role", u.Role, role, func(q store.Querier) error {
		var err error
		kept, err = store.SetUserRoleKeepingAnOwner(r.Context(), q, id, role)
		return err
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	if !kept {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, map[string]any{
			"Error": "That is the last owner. Make someone else an owner first.",
		})
		return
	}
	http.Redirect(w, r, "/settings?saved=1#team", http.StatusSeeOther)
}

func (s *Server) postInviteCreate(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.PostFormValue("email"))
	if !s.auth.Allow(auth.BucketInvite, s.auth.ClientIP(r), strings.ToLower(email)) {
		s.renderSettings(w, r, http.StatusTooManyRequests, map[string]any{"Error": auth.ErrRateLimited.Error()})
		return
	}
	token, err := s.auth.CreateInvitation(r.Context(), email, r.PostFormValue("role"), userOf(r).ID)
	if err != nil {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, map[string]any{"Error": err.Error()})
		return
	}
	if err := s.activity(r.Context(), userOf(r).ID, "invitation", email, "create", "", r.PostFormValue("role")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.logInvite(email, token)
	http.Redirect(w, r, "/settings?saved=1#team", http.StatusSeeOther)
}

func (s *Server) postInviteResend(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	token, err := s.auth.ReissueInvitation(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.activity(r.Context(), userOf(r).ID, "invitation", itoa(id), "resend", "", ""); err != nil {
		s.fail(w, r, err)
		return
	}
	s.logInvite("invitation "+itoa(id), token)
	http.Redirect(w, r, "/settings?saved=1#team", http.StatusSeeOther)
}

func (s *Server) postInviteRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if err := s.write(r, "invitation", itoa(id), "revoke", "", "", func(q store.Querier) error {
		return store.DeleteInvitation(r.Context(), q, id)
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings?saved=1#team", http.StatusSeeOther)
}

// ponytail: nothing sends it until internal/mail lands, so the invitation is
// recorded and the link goes nowhere. The link is deliberately not logged:
// anything with read access to the log could accept the invitation with it.
func (s *Server) logInvite(who, token string) {
	_ = token
	s.log.Info("invitation issued", "to", who)
}

func (s *Server) postTokenCreate(w http.ResponseWriter, r *http.Request) {
	scopes := strings.Fields(r.PostFormValue("scopes"))
	token, err := s.auth.CreateAPIToken(r.Context(), userOf(r).ID, strings.TrimSpace(r.PostFormValue("name")), scopes)
	if err != nil {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, map[string]any{"Error": err.Error()})
		return
	}
	if err := s.activity(r.Context(), userOf(r).ID, "api_token", r.PostFormValue("name"), "create", "", r.PostFormValue("scopes")); err != nil {
		s.fail(w, r, err)
		return
	}
	// Rendered rather than redirected: the token is shown once and nowhere else.
	s.renderSettings(w, r, http.StatusOK, map[string]any{"NewToken": token})
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
	http.Redirect(w, r, "/settings?saved=1#team", http.StatusSeeOther)
}

// activity records a mutation that could not share a transaction with its
// change, because the change went through a package holding its own handle.
func (s *Server) activity(ctx context.Context, actorID int64, entity, entityID, action, before, after string) error {
	return store.InsertActivity(ctx, s.db, "user", itoa(actorID), entity, entityID, action, before, after)
}

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
	if err := store.InsertActivity(r.Context(), tx, "user", actor, entity, entityID, action, before, after); err != nil {
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
