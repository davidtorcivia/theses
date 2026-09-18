package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeTransistor is as much of the API as this package uses: one show, one
// episode store, and the publish endpoint beside the episode one.
type fakeTransistor struct {
	*httptest.Server

	mu       sync.Mutex
	key      string
	calls    []string     // method and path, in order
	forms    []url.Values // every body, in order
	episodes map[string]map[string]string
	next     int
	fail     func(w http.ResponseWriter, r *http.Request) bool
}

func newFakeTransistor(t *testing.T) *fakeTransistor {
	t.Helper()
	f := &fakeTransistor{key: "the-key", episodes: map[string]map[string]string{}, next: 900}
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.forms = append(f.forms, r.PostForm)
		f.mu.Unlock()
	}
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("x-api-key") != f.key {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"errors":[{"title":"Unauthorized"}]}`)
			return false
		}
		if f.fail != nil {
			return f.fail(w, r)
		}
		return true
	}
	write := func(w http.ResponseWriter, id string) {
		f.mu.Lock()
		e := f.episodes[id]
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"id": id, "type": "episode", "attributes": map[string]any{
				"title": e["title"], "status": e["status"], "share_url": "https://example.com/s/" + id,
			}}})
	}

	mux.HandleFunc("GET /v1/shows", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if !guard(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "1", "type": "show", "attributes": map[string]string{"title": "Workspace"}}}})
	})
	mux.HandleFunc("GET /v1/shows/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if !guard(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"id": r.PathValue("id"), "type": "show",
			"attributes": map[string]string{"title": "Workspace"}}})
	})
	mux.HandleFunc("POST /v1/episodes", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if !guard(w, r) {
			return
		}
		f.mu.Lock()
		f.next++
		id := strconv.Itoa(f.next)
		f.episodes[id] = map[string]string{
			"title": r.PostFormValue("episode[title]"), "status": "draft",
			"audio_url": r.PostFormValue("episode[audio_url]"),
		}
		f.mu.Unlock()
		write(w, id)
	})
	mux.HandleFunc("PATCH /v1/episodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if !guard(w, r) {
			return
		}
		id := r.PathValue("id")
		f.mu.Lock()
		e, ok := f.episodes[id]
		if ok {
			e["title"] = r.PostFormValue("episode[title]")
			e["audio_url"] = r.PostFormValue("episode[audio_url]")
		}
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errors":[{"title":"Not Found"}]}`)
			return
		}
		write(w, id)
	})
	mux.HandleFunc("PATCH /v1/episodes/{id}/publish", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if !guard(w, r) {
			return
		}
		id := r.PathValue("id")
		f.mu.Lock()
		if e, ok := f.episodes[id]; ok {
			e["status"] = r.PostFormValue("episode[status]")
		}
		f.mu.Unlock()
		write(w, id)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTransistor) transistor(t *testing.T, s Settings) *Transistor {
	t.Helper()
	tr := &Transistor{HTTP: loopback(), API: f.URL}
	if err := tr.Configure(s); err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestTransistorTest(t *testing.T) {
	f := newFakeTransistor(t)
	cases := []struct {
		name string
		set  Settings
		want string
	}{
		{"with a show", Settings{"api_key": "the-key", "show_id": "1"}, "Connected to Workspace."},
		{"without one", Settings{"api_key": "the-key"}, "That key reaches Workspace (1). Put the id in the show field."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := f.transistor(t, c.set)
			said, err := tr.Test(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if said != c.want {
				t.Fatalf("test said %q, want %q", said, c.want)
			}
		})
	}

	t.Run("with a key Transistor refuses", func(t *testing.T) {
		tr := f.transistor(t, Settings{"api_key": "wrong", "show_id": "1"})
		_, err := tr.Test(context.Background())
		if err == nil || !strings.Contains(err.Error(), "refused the API key") {
			t.Fatalf("a bad key gave %v", err)
		}
		if strings.Contains(err.Error(), "wrong") {
			t.Fatal("the refusal carries the key")
		}
	})

	t.Run("with nothing set", func(t *testing.T) {
		tr := f.transistor(t, Settings{})
		if _, err := tr.Test(context.Background()); !errors.Is(err, ErrNotConnected) {
			t.Fatalf("an unconfigured integration gave %v", err)
		}
		if tr.Connected() {
			t.Fatal("an unconfigured integration says it is connected")
		}
	})
}

func TestTransistorPublishIsIdempotent(t *testing.T) {
	f := newFakeTransistor(t)
	tr := f.transistor(t, Settings{"api_key": "the-key", "show_id": "1"})

	first, err := tr.Publish(context.Background(), Episode{
		Title: "The first one", Summary: "A summary", Description: "<p>Notes</p>",
		AudioURL: "https://bucket.example.com/a.wav?sig=1", Number: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.Status != "published" {
		t.Fatalf("the first publish returned %+v", first)
	}
	if got := []string{"POST /v1/episodes", "PATCH /v1/episodes/" + first.ID + "/publish"}; !same(f.calls, got) {
		t.Fatalf("the first publish made %v, want %v", f.calls, got)
	}
	if got := f.forms[0].Get("episode[show_id]"); got != "1" {
		t.Fatalf("the create named show %q", got)
	}
	if got := f.forms[0].Get("episode[description]"); got != "<p>Notes</p>" {
		t.Fatalf("the description was %q", got)
	}

	// The same proposition, published again with the id it was given: the
	// episode is updated in place, and it is not asked to publish twice.
	f.calls, f.forms = nil, nil
	second, err := tr.Publish(context.Background(), Episode{
		ID: first.ID, Title: "The first one, renamed",
		AudioURL: "https://bucket.example.com/a.wav?sig=2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("the second publish made a new episode %s", second.ID)
	}
	if got := []string{"PATCH /v1/episodes/" + first.ID}; !same(f.calls, got) {
		t.Fatalf("the second publish made %v, want %v", f.calls, got)
	}
	if f.episodes[first.ID]["title"] != "The first one, renamed" {
		t.Fatalf("the episode was not updated: %v", f.episodes[first.ID])
	}
}

// A failure at the publish step leaves the episode on Transistor, so the id
// has to come back with the error or the next attempt makes a second episode.
func TestTransistorReturnsTheIdWhenPublishingFails(t *testing.T) {
	f := newFakeTransistor(t)
	f.fail = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/publish") {
			return true
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"errors":[{"title":"Audio has not finished processing"}]}`)
		return false
	}
	tr := f.transistor(t, Settings{"api_key": "the-key", "show_id": "1"})
	out, err := tr.Publish(context.Background(), Episode{Title: "Half done"})
	if err == nil {
		t.Fatal("a refused publish came back as a success")
	}
	if !strings.Contains(err.Error(), "Audio has not finished processing") {
		t.Fatalf("the refusal said %q", err)
	}
	if out.ID == "" {
		t.Fatal("the episode id was not returned with the failure")
	}
}

func same(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
