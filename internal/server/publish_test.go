package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
)

// transistorFake is the two endpoints a publish uses, with what it was sent.
type transistorFake struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []string
	forms  []url.Values
	status map[string]string
	next   int
	// onCreate runs as the episode is made, which is where a test that wants
	// the request to go away underneath the publish pulls it.
	onCreate func()
	// refuse answers the publish step instead of publishing, so a test can see
	// what a refusal carries.
	refuse func(w http.ResponseWriter, r *http.Request) bool
}

// The two hooks are set from the test goroutine and read from the one serving,
// so they go through the same lock as everything else on this fake. Without it
// the race detector has something to say about every test that sets one.
func (f *transistorFake) onCreated(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onCreate = hook
}

func (f *transistorFake) refuseWith(hook func(w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = hook
}

// sent is a recorded request body, read under the lock.
func (f *transistorFake) sent(n int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forms[n]
}

// made is what reached the fake, read under the lock.
func (f *transistorFake) made() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// forget clears what was recorded, for a test that measures a second publish.
func (f *transistorFake) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.forms = nil, nil
}

func newTransistorFake(t *testing.T) *transistorFake {
	t.Helper()
	f := &transistorFake{status: map[string]string{}, next: 900}
	mux := http.NewServeMux()
	// The status is copied out under the lock: the handlers that write it hold
	// it, and encoding straight from the map would read it while they do.
	answer := func(w http.ResponseWriter, id string) {
		f.mu.Lock()
		status := f.status[id]
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"id": id, "attributes": map[string]any{
				"status": status, "share_url": "https://example.com/s/" + id}}})
	}
	note := func(r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.forms = append(f.forms, r.PostForm)
		f.mu.Unlock()
	}
	mux.HandleFunc("POST /v1/episodes", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		f.mu.Lock()
		f.next++
		id := strconv.Itoa(f.next)
		f.status[id] = "draft"
		hook := f.onCreate
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		answer(w, id)
	})
	mux.HandleFunc("PATCH /v1/episodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		answer(w, r.PathValue("id"))
	})
	mux.HandleFunc("PATCH /v1/episodes/{id}/publish", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		f.mu.Lock()
		refuse := f.refuse
		f.mu.Unlock()
		if refuse != nil && !refuse(w, r) {
			return
		}
		f.mu.Lock()
		f.status[r.PathValue("id")] = "published"
		f.mu.Unlock()
		answer(w, r.PathValue("id"))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// publishable is a workspace with a connected Transistor, a proposition at
// released, a Show notes document with a paragraph in it, and one recording.
func publishable(t *testing.T) (*harness, *transistorFake, int64) {
	t.Helper()
	h := newHarness(t)
	h.setupOwner()
	h.configureBucket(fakeBucket(t, "theses"), "theses")
	fake := newTransistorFake(t)
	h.pointAtFakes("http://127.0.0.1:1", fake.URL)
	h.saveSecret("integrations.transistor.api_key", "the-key")
	h.saveSecret("integrations.transistor.show_id", "1")

	ctx := context.Background()
	id := h.proposition("Tidal Power")
	actor := core.Actor{Kind: core.KindUser, ID: 1, Name: "Ada Lovelace"}

	// Show notes is one of the three a proposition is seeded with, so the
	// paragraph goes into that one rather than into a second of the same name.
	var doc int64
	if err := h.db.QueryRowContext(ctx,
		`SELECT id FROM documents WHERE proposition_id = ? AND name = 'Show notes'`, id).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.docs.InsertBlock(ctx, actor, doc, 0, "", "The tide comes in.", false); err != nil {
		t.Fatal(err)
	}
	const audio = "twelve bytes"
	if _, err := h.srv.files.Import(ctx, actor, id, "Episode.mp3", files.Recordings,
		int64(len(audio)), io.LimitReader(bytes.NewReader([]byte(audio)), int64(len(audio)))); err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.board.SetStatus(ctx, actor, id, "released"); err != nil {
		t.Fatal(err)
	}
	return h, fake, id
}

// chosen is the two selects on the section, read off the rendered page.
func (h *harness) chosen(page, name string) string {
	h.Helper()
	at := strings.Index(page, `name="`+name+`"`)
	if at < 0 {
		h.Fatalf("no %s select on the page", name)
	}
	rest := page[at:]
	end := strings.Index(rest, "</select>")
	selected := strings.Index(rest[:end], ` selected`)
	if selected < 0 {
		h.Fatalf("nothing is selected in %s", name)
	}
	value := rest[:selected]
	from := strings.LastIndex(value, `value="`) + len(`value="`)
	to := strings.Index(value[from:], `"`)
	return value[from : from+to]
}

func TestPublishSendsTheEpisodeAndIsIdempotent(t *testing.T) {
	h, fake, id := publishable(t)
	at := strconv.FormatInt(id, 10)

	_, page := h.get("/p/" + at + "/settings")
	if !strings.Contains(page, "Publish</h3>") {
		t.Fatal("the settings page has no Publish section")
	}
	// Show notes is the default document, and the one recording the default
	// audio is not chosen for you, so it is sent with the form.
	document := h.chosen(page, "document")
	var file int64
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT id FROM files WHERE folder = ?`, files.Recordings).Scan(&file); err != nil {
		t.Fatal(err)
	}

	res, body := h.post("/p/"+at+"/publish", url.Values{
		"csrf": {h.csrf("/p/" + at + "/settings")}, "do": {"publish"},
		"document": {document}, "file": {strconv.FormatInt(file, 10)},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("publish gave %d: %s", res.StatusCode, firstNotice(body))
	}
	if !strings.Contains(body, "Published as episode 901.") {
		t.Fatalf("publish said %q", firstNotice(body))
	}
	if made := fake.made(); len(made) != 2 || made[0] != "POST /v1/episodes" ||
		made[1] != "PATCH /v1/episodes/901/publish" {
		t.Fatalf("publish made %v", made)
	}
	sent := fake.sent(0)
	if sent.Get("episode[title]") != "Tidal Power" {
		t.Fatalf("the title was %q", sent.Get("episode[title]"))
	}
	if !strings.Contains(sent.Get("episode[description]"), "The tide comes in.") {
		t.Fatalf("the show notes were %q", sent.Get("episode[description]"))
	}
	// The audio is a presigned GET of the object, which is the only way
	// Transistor can read a file out of a private bucket.
	audio, err := url.Parse(sent.Get("episode[audio_url]"))
	if err != nil {
		t.Fatal(err)
	}
	if audio.Query().Get("X-Amz-Signature") == "" {
		t.Fatalf("the audio url is %q", audio)
	}
	if audio.Query().Get("X-Amz-Expires") != "86400" {
		t.Fatalf("the audio url lasts %q seconds", audio.Query().Get("X-Amz-Expires"))
	}

	// The episode id is on the proposition, so a second publish updates it.
	var episode string
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT transistor_episode_id FROM propositions WHERE id = ?`, id).Scan(&episode); err != nil {
		t.Fatal(err)
	}
	if episode != "901" {
		t.Fatalf("the proposition holds episode %q", episode)
	}

	fake.forget()
	res, body = h.post("/p/"+at+"/publish", url.Values{
		"csrf": {h.csrf("/p/" + at + "/settings")}, "do": {"publish"},
		"document": {document}, "file": {strconv.FormatInt(file, 10)},
	})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Episode 901 was updated.") {
		t.Fatalf("the second publish said %q", firstNotice(body))
	}
	if made := fake.made(); len(made) != 1 || made[0] != "PATCH /v1/episodes/901" {
		t.Fatalf("the second publish made %v", made)
	}
	if !strings.Contains(body, "https://example.com/s/901") {
		t.Fatal("the page does not link to the episode")
	}
}

func TestPublishRefusals(t *testing.T) {
	h, _, id := publishable(t)
	at := strconv.FormatInt(id, 10)
	ctx := context.Background()
	actor := core.Actor{Kind: core.KindUser, ID: 1, Name: "Ada Lovelace"}
	var file int64
	if err := h.db.QueryRowContext(ctx, `SELECT id FROM files WHERE folder = ?`,
		files.Recordings).Scan(&file); err != nil {
		t.Fatal(err)
	}
	_, page := h.get("/p/" + at + "/settings")
	document := h.chosen(page, "document")

	send := func(form url.Values) (*http.Response, string) {
		form.Set("csrf", h.csrf("/p/"+at+"/settings"))
		form.Set("do", "publish")
		return h.post("/p/"+at+"/publish", form)
	}

	t.Run("a recording that is not one", func(t *testing.T) {
		res, body := send(url.Values{"document": {document}, "file": {"0"}})
		if res.StatusCode != http.StatusUnprocessableEntity ||
			!strings.Contains(body, "choose the recording") {
			t.Fatalf("gave %d: %q", res.StatusCode, firstNotice(body))
		}
	})
	t.Run("a document on another proposition", func(t *testing.T) {
		other := h.proposition("Grid Storage")
		doc, err := h.srv.docs.CreateDocument(ctx, actor, other, "Show notes")
		if err != nil {
			t.Fatal(err)
		}
		res, body := send(url.Values{
			"document": {strconv.FormatInt(doc.EntityID, 10)}, "file": {strconv.FormatInt(file, 10)}})
		if res.StatusCode != http.StatusUnprocessableEntity ||
			!strings.Contains(body, "choose the document") {
			t.Fatalf("gave %d: %q", res.StatusCode, firstNotice(body))
		}
	})
	t.Run("a proposition that has not reached the status", func(t *testing.T) {
		if _, err := h.srv.board.SetStatus(ctx, actor, id, "editing"); err != nil {
			t.Fatal(err)
		}
		res, body := send(url.Values{"document": {document}, "file": {strconv.FormatInt(file, 10)}})
		if res.StatusCode != http.StatusUnprocessableEntity ||
			!strings.Contains(body, "published at released") {
			t.Fatalf("gave %d: %q", res.StatusCode, firstNotice(body))
		}
		if _, err := h.srv.board.SetStatus(ctx, actor, id, "released"); err != nil {
			t.Fatal(err)
		}
	})
	// The section is not drawn for an archived proposition, so a form that
	// reaches this is one that should not have been sent. It answers the same
	// page a role that may not edit gets rather than falling through to a 500.
	t.Run("an archived proposition", func(t *testing.T) {
		if _, err := h.srv.board.ArchiveProposition(ctx, actor, id); err != nil {
			t.Fatal(err)
		}
		res, _ := h.post("/p/"+at+"/publish", url.Values{
			"csrf": {h.csrf("/p/" + at + "/settings")}, "do": {"publish"},
			"document": {document}, "file": {strconv.FormatInt(file, 10)}})
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("publishing an archived proposition gave %d", res.StatusCode)
		}
		if _, err := h.srv.board.RestoreProposition(ctx, actor, id); err != nil {
			t.Fatal(err)
		}
	})

	// A guest is a member of this proposition and may read the page, and may
	// not publish it, which is the rule every other change on it follows.
	t.Run("a guest", func(t *testing.T) {
		const password = "a long enough password"
		if err := h.srv.settings.Set(ctx, "signin.require_totp", []string{"owners"}, 1); err != nil {
			t.Fatal(err)
		}
		guest := h.withoutAuthenticator("mara", auth.RoleGuest, password)
		if _, err := h.srv.board.AddMember(ctx, actor, id, guest); err != nil {
			t.Fatal(err)
		}
		h.signOut()
		if res, _ := h.signIn("mara", password, ""); res.Header.Get("Location") != "/" {
			t.Fatal("the guest could not sign in")
		}
		if res, _ := h.get("/p/" + at + "/settings"); res.StatusCode != http.StatusOK {
			t.Fatalf("the guest cannot read the page: %d", res.StatusCode)
		}
		res, _ := h.post("/p/"+at+"/publish", url.Values{
			"csrf": {h.csrf("/p/" + at + "/settings")}, "do": {"publish"},
			"document": {document}, "file": {strconv.FormatInt(file, 10)}})
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("a guest publishing gave %d", res.StatusCode)
		}
	})
}

// The section is drawn with no Transistor connected, saying why and with
// nothing in it that can be pressed. A section that vanished left somebody
// looking for a feature the page never mentions.
func TestPublishSectionSaysWhyWhenTransistorIsNotConnected(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	at := strconv.FormatInt(h.proposition("Tidal Power"), 10)
	_, page := h.get("/p/" + at + "/settings")
	for _, want := range []string{"Publish</h3>", "Transistor is not connected",
		`<select name="document" disabled>`, `value="publish" disabled>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the Publish section has no %q", want)
		}
	}
	// Neither button works, and neither is a fault of the server: both are
	// drawn refused and both are refused again if a post arrives anyway.
	for _, do := range []string{"publish", "save"} {
		res, body := h.post("/p/"+at+"/publish", url.Values{
			"csrf": {h.csrf("/p/" + at + "/settings")}, "do": {do}})
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("%s with nothing connected gave %d", do, res.StatusCode)
		}
		if !strings.Contains(body, "Transistor is not connected") {
			t.Fatalf("%s did not say why: %s", do, firstNotice(body))
		}
	}
	// The two selects have nothing to offer, and an empty select box says
	// nothing at all to the person looking at it.
	if strings.Count(page, "Nothing to choose yet") != 2 {
		t.Error("the empty selects do not say they are empty")
	}
}

// A publish that is under way when the browser goes away has to finish. The
// episode exists at Transistor from the moment it answers, and a canceled
// write of its id is a second episode on the next attempt.
func TestPublishRecordsTheEpisodeWhenTheRequestGoesAway(t *testing.T) {
	h, fake, id := publishable(t)
	at := strconv.FormatInt(id, 10)
	form := h.publishForm(t, at)

	ctx, cancel := context.WithCancel(context.Background())
	// Transistor has made the episode; now the browser closes the tab.
	fake.onCreated(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.http.URL+"/p/"+at+"/publish", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := h.client.Do(req); err == nil {
		t.Fatal("the request was not canceled")
	}

	// The handler runs on after the connection has gone, so the assertion
	// waits for it rather than racing it.
	var episode string
	for i := 0; i < 250 && episode == ""; i++ {
		time.Sleep(20 * time.Millisecond)
		if err := h.db.QueryRowContext(context.Background(),
			`SELECT coalesce(transistor_episode_id, '') FROM propositions WHERE id = ?`, id).
			Scan(&episode); err != nil {
			t.Fatal(err)
		}
	}
	if episode != "901" {
		t.Fatalf("the episode id was not recorded: %q", episode)
	}
}

// Transistor quotes back what it was sent, and one of the things it is sent is
// a link that reads the recording for a day.
func TestPublishRefusalDoesNotCarryTheSignedAudioLink(t *testing.T) {
	h, fake, id := publishable(t)
	at := strconv.FormatInt(id, 10)
	fake.refuseWith(func(w http.ResponseWriter, r *http.Request) bool {
		f := fake.sent(0)
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{
			{"title": "Could not fetch " + f.Get("episode[audio_url]")}}})
		return false
	})

	form := h.publishForm(t, at)
	res, body := h.post("/p/"+at+"/publish", form)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a refused publish gave %d", res.StatusCode)
	}
	said := firstNotice(body)
	if !strings.Contains(said, "Could not fetch") {
		t.Fatalf("the page said %q", said)
	}
	for _, leak := range []string{"X-Amz-Signature", "X-Amz-Credential", "AWS4-HMAC"} {
		if strings.Contains(body, leak) {
			t.Fatalf("the page carries %s", leak)
		}
	}
	if !strings.Contains(said, "[redacted]") {
		t.Fatalf("the link was not redacted: %q", said)
	}
}

// publishForm is the section's two selects, as the page renders them, plus the
// token and the button.
func (h *harness) publishForm(t *testing.T, at string) url.Values {
	t.Helper()
	_, page := h.get("/p/" + at + "/settings")
	var file int64
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT id FROM files WHERE folder = ?`, files.Recordings).Scan(&file); err != nil {
		t.Fatal(err)
	}
	return url.Values{
		"csrf": {h.csrf("/p/" + at + "/settings")}, "do": {"publish"},
		"document": {h.chosen(page, "document")}, "file": {strconv.FormatInt(file, 10)},
	}
}

func TestEpisodeNumberIsOnlyAPlainNumber(t *testing.T) {
	four := "4"
	spaced := " 4 "
	season := "S2E4"
	zero := "0"
	negative := "-3"
	empty := ""
	cases := []struct {
		name  string
		given *string
		want  string
	}{
		{"nothing scheduled", nil, ""},
		{"an empty field", &empty, ""},
		{"a number", &four, "4"},
		{"a number with spaces round it", &spaced, "4"},
		{"a season and episode", &season, ""},
		{"zero", &zero, ""},
		{"a negative number", &negative, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := episodeNumber(c.given); got != c.want {
				t.Fatalf("gave %q, want %q", got, c.want)
			}
		})
	}
}
