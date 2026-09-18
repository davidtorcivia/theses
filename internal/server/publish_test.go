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
}

func newTransistorFake(t *testing.T) *transistorFake {
	t.Helper()
	f := &transistorFake{status: map[string]string{}, next: 900}
	mux := http.NewServeMux()
	answer := func(w http.ResponseWriter, id string) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"id": id, "attributes": map[string]any{
				"status": f.status[id], "share_url": "https://example.com/s/" + id}}})
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
		f.mu.Unlock()
		answer(w, id)
	})
	mux.HandleFunc("PATCH /v1/episodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		answer(w, r.PathValue("id"))
	})
	mux.HandleFunc("PATCH /v1/episodes/{id}/publish", func(w http.ResponseWriter, r *http.Request) {
		note(r)
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

	doc, err := h.srv.docs.CreateDocument(ctx, actor, id, "Show notes")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.docs.InsertBlock(ctx, actor, doc.EntityID, 0, "The tide comes in."); err != nil {
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
	if len(fake.calls) != 2 || fake.calls[0] != "POST /v1/episodes" ||
		fake.calls[1] != "PATCH /v1/episodes/901/publish" {
		t.Fatalf("publish made %v", fake.calls)
	}
	sent := fake.forms[0]
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

	fake.calls, fake.forms = nil, nil
	res, body = h.post("/p/"+at+"/publish", url.Values{
		"csrf": {h.csrf("/p/" + at + "/settings")}, "do": {"publish"},
		"document": {document}, "file": {strconv.FormatInt(file, 10)},
	})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Episode 901 was updated.") {
		t.Fatalf("the second publish said %q", firstNotice(body))
	}
	if len(fake.calls) != 1 || fake.calls[0] != "PATCH /v1/episodes/901" {
		t.Fatalf("the second publish made %v", fake.calls)
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

// Nothing of the section is drawn before an owner has connected Transistor.
func TestPublishSectionIsHiddenUntilTransistorIsConnected(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	at := strconv.FormatInt(h.proposition("Tidal Power"), 10)
	_, page := h.get("/p/" + at + "/settings")
	if strings.Contains(page, "Publish</h3>") {
		t.Fatal("the Publish section is drawn with no Transistor connected")
	}
	res, _ := h.post("/p/"+at+"/publish", url.Values{
		"csrf": {h.csrf("/p/" + at + "/settings")}, "do": {"publish"}})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("publishing with nothing connected gave %d", res.StatusCode)
	}
}
