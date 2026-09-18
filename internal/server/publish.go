package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/integrations"
	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// Publishing a proposition as an episode. The section on the proposition's own
// settings page chooses which document is the show notes and which recording is
// the audio, and the button sends both to Transistor.

// showNotes is the document a proposition uses for the episode's notes when
// nobody has chosen one.
const showNotes = "Show notes"

// publishChoice is what this proposition publishes with. It lives in the
// settings table under a key of its own, the way the document switches beside
// it do, because there is one of each per proposition and nothing else reads
// them. The episode id is not here: it is a column on the proposition, because
// it is the fact that makes a second publish an update.
type publishChoice struct {
	Document int64  `json:"document"`
	File     int64  `json:"file"`
	ShareURL string `json:"share_url"`
}

func publishKey(proposition int64) string {
	return "proposition." + strconv.FormatInt(proposition, 10) + ".transistor"
}

func (s *Server) publishChoice(ctx context.Context, proposition int64) (publishChoice, error) {
	var c publishChoice
	row, err := store.GetSetting(ctx, s.db, publishKey(proposition))
	if errors.Is(err, store.ErrNotFound) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal([]byte(row.ValueJSON), &c)
}

func (s *Server) savePublishChoice(ctx context.Context, proposition, by int64, c publishChoice) error {
	value, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return store.PutSetting(ctx, s.db, publishKey(proposition), string(value), false, by)
}

// episodeOf reads the Transistor episode this proposition has already been
// published as, empty when it has not been.
func (s *Server) episodeOf(ctx context.Context, proposition int64) (string, error) {
	var id sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT transistor_episode_id FROM propositions WHERE id = ?`, proposition).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrNotFound
	}
	return id.String, err
}

type publishView struct {
	// On is whether the owner has connected Transistor at all. The section is
	// not drawn when they have not.
	On        bool
	Ready     bool
	Why       string
	Status    string
	Documents []option
	Files     []option
	Episode   string
	URL       string
	Error     string
}

// publishSection is the Publish row of the proposition settings page. Anything
// it cannot read is a line inside the section rather than a failure of the
// page, which is the same rule the Backups section follows.
func (s *Server) publishSection(ctx context.Context, p board.Proposition, me *store.User) publishView {
	v := publishView{Status: settings.Get[string](s.settings, transistorPrefix+"publish_status")}
	if !s.settings.IsSet(transistorPrefix+"api_key") ||
		settings.Get[string](s.settings, transistorPrefix+"show_id") == "" {
		return v
	}
	v.On = true

	chosen, err := s.publishChoice(ctx, p.ID)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.URL = chosen.ShareURL
	if v.Episode, err = s.episodeOf(ctx, p.ID); err != nil {
		v.Error = err.Error()
		return v
	}

	written, err := docs.ListDocuments(ctx, s.db, p.ID)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	document := chosen.Document
	if document == 0 {
		document = defaultDocument(written)
	}
	for _, d := range written {
		v.Documents = append(v.Documents, option{
			Value: strconv.FormatInt(d.ID, 10), Label: d.Name, On: d.ID == document})
	}

	actor := core.Actor{Kind: core.KindUser, ID: me.ID, Name: me.Name}
	held, err := s.files.ListFiles(ctx, actor, p.ID)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	for _, f := range held {
		if f.Folder != files.Recordings || !f.Ready() {
			continue
		}
		v.Files = append(v.Files, option{
			Value: strconv.FormatInt(f.ID, 10), Label: f.Name, On: f.ID == chosen.File})
	}

	switch {
	case len(v.Documents) == 0:
		v.Why = "This proposition has no documents, so there are no show notes to send."
	case len(v.Files) == 0:
		v.Why = "Nothing is in the Recordings folder yet, so there is no audio to send."
	case p.Status != v.Status:
		v.Why = "This proposition is at " + p.Status + ". It can be published once it reaches " + v.Status + "."
	case p.ArchivedAt != nil:
		v.Why = "This proposition is archived."
	default:
		v.Ready = true
	}
	return v
}

// defaultDocument is the one called Show notes, or the first one there is.
func defaultDocument(written []docs.Document) int64 {
	if len(written) == 0 {
		return 0
	}
	for _, d := range written {
		if strings.EqualFold(d.Name, showNotes) {
			return d.ID
		}
	}
	return written[0].ID
}

// postPublish saves the two choices and, when the button says so, sends the
// episode. Both are on one route because the form is one form.
func (s *Server) postPublish(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	me := userOf(r)
	p, err := s.publishable(r, id)
	if errors.Is(err, core.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if errors.Is(err, core.ErrForbidden) {
		s.errorPage(w, r, http.StatusForbidden)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	chosen, err := s.publishChoice(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	chosen.Document = number64(r.PostFormValue("document"))
	chosen.File = number64(r.PostFormValue("file"))
	if err := s.savePublishChoice(r.Context(), id, me.ID, chosen); err != nil {
		s.fail(w, r, err)
		return
	}
	if r.PostFormValue("do") != "publish" {
		http.Redirect(w, r, "/p/"+r.PathValue("id")+"/settings?saved=1#publish", http.StatusSeeOther)
		return
	}

	said, err := s.publish(r, p, chosen)
	if err != nil {
		s.renderPropositionSettings(w, r, http.StatusUnprocessableEntity, map[string]any{
			"PublishResult": s.redactSecrets(r.Context(), err.Error()), "PublishFailed": true,
		})
		return
	}
	s.renderPropositionSettings(w, r, http.StatusOK, map[string]any{"PublishResult": said})
}

// publishable is the standing this needs: the proposition has to be readable,
// the person has to be allowed to edit it, and it must not be archived. It is
// the same rule every other change on this page goes through.
func (s *Server) publishable(r *http.Request, id int64) (board.Proposition, error) {
	me := userOf(r)
	readable, err := board.Readable(r.Context(), s.db, me, id)
	if err != nil {
		return board.Proposition{}, err
	}
	if !readable {
		return board.Proposition{}, core.ErrNotFound
	}
	p, err := board.GetProposition(r.Context(), s.db, id)
	if err != nil {
		return board.Proposition{}, err
	}
	if !auth.Can(me.Role, auth.CanEdit) || p.ArchivedAt != nil {
		return board.Proposition{}, core.ErrForbidden
	}
	return p, nil
}

// publish builds the episode and sends it. The episode id is written the
// moment Transistor gives one, before anything else can fail, because an
// episode that exists and is not recorded is the one thing that would make the
// next attempt a second episode rather than an update.
func (s *Server) publish(r *http.Request, p board.Proposition, chosen publishChoice) (string, error) {
	ctx := r.Context()
	me := userOf(r)
	actor := core.Actor{Kind: core.KindUser, ID: me.ID, Name: me.Name}

	want := settings.Get[string](s.settings, transistorPrefix+"publish_status")
	if p.Status != want {
		return "", errors.New("this proposition is at " + p.Status + " and is published at " + want)
	}
	tr, err := s.loadTransistor(ctx)
	if err != nil {
		return "", err
	}
	if !tr.Connected() {
		return "", integrations.ErrNotConnected
	}

	notes, err := s.showNotesHTML(ctx, p.ID, chosen.Document)
	if err != nil {
		return "", err
	}
	audio, err := s.audioURL(ctx, actor, p.ID, chosen.File)
	if err != nil {
		return "", err
	}
	was, err := s.episodeOf(ctx, p.ID)
	if err != nil {
		return "", err
	}

	out, sendErr := tr.Publish(ctx, integrations.Episode{
		ID:          was,
		Title:       p.Title,
		Summary:     p.Blurb,
		Description: notes,
		AudioURL:    audio,
		Number:      episodeNumber(p.Episode),
	})
	// The id comes back even when the publish step failed, so it is recorded
	// either way and the next attempt updates rather than duplicates.
	if out.ID != "" && out.ID != was {
		if err := s.recordEpisode(ctx, p.ID, me.ID, out.ID); err != nil {
			s.log.Error("an episode was created and its id could not be stored",
				"proposition", p.ID, "episode", out.ID, "err", err)
			return "", err
		}
	}
	if sendErr != nil {
		// Transistor quotes back what it was sent, and one of the things it
		// was sent is a signed link that reads the recording for a day. It is
		// not a stored secret, so nothing else would take it out.
		return "", errors.New(mail.Redact(sendErr.Error(), audio))
	}
	chosen.ShareURL = out.ShareURL
	if err := s.savePublishChoice(ctx, p.ID, me.ID, chosen); err != nil {
		return "", err
	}
	if was == "" {
		return "Published as episode " + out.ID + ".", nil
	}
	return "Episode " + out.ID + " was updated.", nil
}

// recordEpisode writes the id and its activity row in one transaction, so the
// log says who published and what it became.
func (s *Server) recordEpisode(ctx context.Context, proposition, by int64, episode string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE propositions SET transistor_episode_id = ? WHERE id = ?`, episode, proposition); err != nil {
		return err
	}
	if err := store.InsertActivity(ctx, tx, "user", itoa(by), "",
		"proposition", itoa(proposition), "publish", "", episode); err != nil {
		return err
	}
	return tx.Commit()
}

// showNotesHTML renders the chosen document the way an export does. The
// document has to belong to this proposition: the id comes off a form.
func (s *Server) showNotesHTML(ctx context.Context, proposition, document int64) (string, error) {
	written, err := docs.ListDocuments(ctx, s.db, proposition)
	if err != nil {
		return "", err
	}
	if document == 0 {
		document = defaultDocument(written)
	}
	found := false
	for _, d := range written {
		if d.ID == document {
			found = true
		}
	}
	if !found {
		return "", errors.New("choose the document the show notes come from")
	}
	blocks, err := docs.Blocks(ctx, s.db, document)
	if err != nil {
		return "", err
	}
	notes := strings.TrimSpace(string(docs.RenderDocument(blocks)))
	if notes == "" {
		return "", errors.New("that document is empty, so there are no show notes to send")
	}
	return notes, nil
}

// audioURL is a presigned GET of the chosen recording. Transistor fetches the
// file some time after the episode is made rather than during the call, so the
// URL has to outlive the request by a wide margin.
func (s *Server) audioURL(ctx context.Context, actor core.Actor, proposition, file int64) (string, error) {
	if file == 0 {
		return "", errors.New("choose the recording this episode is")
	}
	row, err := s.files.ReadFile(ctx, actor, file)
	if errors.Is(err, core.ErrNotFound) {
		return "", errors.New("that recording is not there any more")
	}
	if err != nil {
		return "", err
	}
	if row.Proposition != proposition || row.Folder != files.Recordings {
		return "", errors.New("that file is not a recording on this proposition")
	}
	if !row.Ready() {
		return "", errors.New("that recording has not finished uploading")
	}
	bucket, err := s.bucketFor(ctx, row.Folder)
	if err != nil {
		return "", err
	}
	return bucket.PresignGet(ctx, row.ObjectKey, row.Name, publishWindow)
}

// episodeNumber is the proposition's episode field when it is a plain number,
// which is what Transistor takes. The field is free text here, so something
// like S2E4 is left out rather than refused by the other end and taking the
// whole publish with it.
func episodeNumber(episode *string) string {
	if episode == nil {
		return ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(*episode))
	if err != nil || n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func number64(v string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if n < 0 {
		return 0
	}
	return n
}
