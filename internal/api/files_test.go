package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/safehttp"
	"github.com/davidtorcivia/theses/internal/store"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// fileHarness is the links and files routes behind a bearer token, over a
// database, an in-process bucket and a page served on loopback.
type fileHarness struct {
	*testing.T
	db      *store.DB
	auth    *auth.Auth
	svc     *files.Service
	handler http.Handler
	owner   *store.User
	// stranger is an editor who is a member of nothing.
	stranger *store.User
	// board is the command service behind the same core, for the tests that
	// have to put a proposition into a state the routes cannot.
	board *board.Service
	prop  int64
	card  int64
	page  string
}

func newFileHarness(t *testing.T) *fileHarness {
	t.Helper()
	ctx := context.Background()
	h := newHarness(t)

	backend := s3mem.New()
	fake := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(fake.Close)
	if err := backend.CreateBucket("theses"); err != nil {
		t.Fatal(err)
	}
	bucket, err := blob.New(blob.Config{
		Provider: "s3", Endpoint: fake.URL, Region: "us-east-1",
		Bucket: "theses", AccessKey: "key", SecretKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}

	c := core.New(h.db, core.NewBus())
	b := board.New(c, func() board.Defaults {
		return board.Defaults{Status: "idea", Statuses: []string{"idea", "recording"}, Columns: []string{"Research"}}
	})
	svc := files.New(c, func(context.Context, string) (*blob.Client, error) { return bucket, nil },
		safehttp.Client(safehttp.AllowLoopback()))

	strangerID, err := store.CreateUser(ctx, h.db, &store.User{
		Handle: "ada", Email: "ada@example.com", Name: "Ada Lovelace",
		Initials: "AL", Colour: "#1100ff", Role: auth.RoleEditor, PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := store.UserByID(ctx, h.db, strangerID)
	if err != nil {
		t.Fatal(err)
	}

	owner := core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}
	e, err := b.CreateProposition(ctx, owner, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	cols, err := board.ListColumns(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	card, err := b.CreateCard(ctx, owner, cols[0].ID, "Read the tide tables", nil)
	if err != nil {
		t.Fatal(err)
	}

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<html><head><title>The tide tables</title>
			<meta property="og:site_name" content="Example Review"></head>
			<body><p>Twice a day.</p></body></html>`)
	}))
	t.Cleanup(page.Close)

	api := New(h.db, h.auth, h.set, slog.New(slog.NewTextHandler(io.Discard, nil)))
	api.Files = svc
	return &fileHarness{
		T: t, db: h.db, auth: h.auth, svc: svc, board: b,
		handler: api.Handler(),
		owner:   h.user, stranger: stranger, prop: e.EntityID, card: card.EntityID,
		page: page.URL,
	}
}

func (h *fileHarness) token(user *store.User, scopes ...string) string {
	h.Helper()
	clear, err := h.auth.CreateAPIToken(context.Background(), user.ID, strings.Join(scopes, "-"), scopes)
	if err != nil {
		h.Fatal(err)
	}
	return clear
}

func (h *fileHarness) do(method, target, token, body string) *httptest.ResponseRecorder {
	h.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func TestLinkRoutes(t *testing.T) {
	h := newFileHarness(t)
	write := h.token(h.owner, auth.ScopeRead, auth.ScopeWrite)

	w := h.do("POST", "/api/v1/links", write,
		`{"proposition":`+strconv.FormatInt(h.prop, 10)+`,"url":"`+h.page+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /links: %d %s", w.Code, w.Body)
	}
	created := decode(t, w)
	if created["title"] != "The tide tables" {
		t.Fatalf("title %v", created["title"])
	}
	if created["citation"] == "" {
		t.Fatal("no citation on the answer")
	}
	id := strconv.FormatInt(int64(created["id"].(float64)), 10)

	if w = h.do("GET", "/api/v1/links?proposition="+strconv.FormatInt(h.prop, 10), write, ""); w.Code != http.StatusOK {
		t.Fatalf("GET /links: %d %s", w.Code, w.Body)
	}
	if n := len(decode(t, w)["links"].([]any)); n != 1 {
		t.Fatalf("got %d links, want 1", n)
	}

	if w = h.do("PATCH", "/api/v1/links/"+id, write, `{"title":"Tide tables","kind":"book"}`); w.Code != http.StatusOK {
		t.Fatalf("PATCH /links: %d %s", w.Code, w.Body)
	}
	if w = h.do("PATCH", "/api/v1/links/"+id, write, `{"kind":"manuscript"}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a kind the list does not have: %d %s", w.Code, w.Body)
	}
	if w = h.do("POST", "/api/v1/links/"+id+"/refetch", write, ""); w.Code != http.StatusOK {
		t.Fatalf("refetch: %d %s", w.Code, w.Body)
	}
	if w = h.do("DELETE", "/api/v1/links/"+id, write, ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE /links: %d %s", w.Code, w.Body)
	}
	if w = h.do("GET", "/api/v1/links/"+id, write, ""); w.Code != http.StatusNotFound {
		t.Fatalf("GET a deleted link: %d", w.Code)
	}
}

// Every route is behind a scope and behind the token owner's standing. A
// proposition the owner of the token is not a member of is a 404, not a 403:
// the answer says nothing about what is there.
func TestFileRoutesAuthorisation(t *testing.T) {
	h := newFileHarness(t)
	proposition := strconv.FormatInt(h.prop, 10)

	for _, tc := range []struct {
		name, method, target, body string
		token                      string
		want                       int
	}{
		{
			name: "a read token cannot add a link", method: "POST", target: "/api/v1/links",
			body:  `{"proposition":` + proposition + `,"url":"https://example.com"}`,
			token: h.token(h.owner, auth.ScopeRead), want: http.StatusForbidden,
		},
		{
			name: "a write token cannot create a file", method: "POST", target: "/api/v1/files",
			body:  `{"proposition":` + proposition + `,"name":"x.md","folder":"Documents","size":4}`,
			token: h.token(h.owner, auth.ScopeWrite), want: http.StatusForbidden,
		},
		{
			name: "a files token can", method: "POST", target: "/api/v1/files",
			body:  `{"proposition":` + proposition + `,"name":"x.md","folder":"Documents","size":4}`,
			token: h.token(h.owner, auth.ScopeFiles), want: http.StatusOK,
		},
		{
			name:   "somebody who is not a member is told nothing is there",
			method: "GET", target: "/api/v1/files?proposition=" + proposition,
			token: h.token(h.stranger, auth.ScopeRead), want: http.StatusNotFound,
		},
		{
			name: "and cannot add a link to it either", method: "POST", target: "/api/v1/links",
			body:  `{"proposition":` + proposition + `,"url":"https://example.com"}`,
			token: h.token(h.stranger, auth.ScopeRead, auth.ScopeWrite), want: http.StatusNotFound,
		},
		{
			name:   "a proposition that does not exist is the same answer",
			method: "GET", target: "/api/v1/links?proposition=9999",
			token: h.token(h.owner, auth.ScopeRead), want: http.StatusNotFound,
		},
		{
			name:   "a file id that does not exist is not found",
			method: "GET", target: "/api/v1/files/9999/download",
			token: h.token(h.owner, auth.ScopeRead), want: http.StatusNotFound,
		},
		{
			// The file above is four bytes, so it has one part and there is
			// nothing after it. This is the case the part number bound answers,
			// and the only one that reaches it.
			name:   "a part number past the end of the upload is refused",
			method: "GET", target: "/api/v1/files/1/parts?after=1",
			token: h.token(h.owner, auth.ScopeFiles), want: http.StatusUnprocessableEntity,
		},
		{
			// Clamped to the beginning rather than refused, so it gets as far
			// as looking for the upload, which a file small enough for one PUT
			// does not have.
			name:   "a negative part number is the beginning",
			method: "GET", target: "/api/v1/files/1/parts?after=-1",
			token: h.token(h.owner, auth.ScopeFiles), want: http.StatusNotFound,
		},
		{
			name: "a URL that is not a web address is refused", method: "POST", target: "/api/v1/links",
			body:  `{"proposition":` + proposition + `,"url":"file:///etc/passwd"}`,
			token: h.token(h.owner, auth.ScopeWrite), want: http.StatusUnprocessableEntity,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := h.do(tc.method, tc.target, tc.token, tc.body)
			if w.Code != tc.want {
				t.Fatalf("%s %s: %d, want %d: %s", tc.method, tc.target, w.Code, tc.want, w.Body)
			}
		})
	}
}

func TestUploadRoutes(t *testing.T) {
	h := newFileHarness(t)
	all := h.token(h.owner, auth.ScopeRead, auth.ScopeWrite, auth.ScopeFiles)
	body := "tide tables\n"

	w := h.do("POST", "/api/v1/files", all, `{"proposition":`+strconv.FormatInt(h.prop, 10)+
		`,"name":"tides.md","folder":"Documents","size":`+strconv.Itoa(len(body))+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /files: %d %s", w.Code, w.Body)
	}
	var up files.Upload
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if up.URL == "" {
		t.Fatalf("no presigned PUT in %s", w.Body)
	}
	id := strconv.FormatInt(up.File.ID, 10)

	// Completing before anything is in the bucket is refused, and the file is
	// left as it was rather than marked ready.
	if w = h.do("POST", "/api/v1/files/"+id+"/complete", all, `{}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("completing an empty upload: %d %s", w.Code, w.Body)
	}

	req, err := http.NewRequest(http.MethodPut, up.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", up.Headers["Content-Type"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PUT to the bucket: %d", resp.StatusCode)
	}

	if w = h.do("POST", "/api/v1/files/"+id+"/complete", all, `{}`); w.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", w.Code, w.Body)
	}
	if w = h.do("GET", "/api/v1/files/"+id+"/download", all, ""); w.Code != http.StatusOK {
		t.Fatalf("download: %d %s", w.Code, w.Body)
	}
	if url, _ := decode(t, w)["url"].(string); !strings.Contains(url, "X-Amz-Signature") {
		t.Fatalf("the download link is not presigned: %q", url)
	}
	if w = h.do("GET", "/api/v1/files/"+id+"/versions", all, ""); w.Code != http.StatusOK {
		t.Fatalf("versions: %d %s", w.Code, w.Body)
	}
	if w = h.do("PATCH", "/api/v1/files/"+id, all, `{"name":"tide tables.md","folder":"Reading"}`); w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body)
	}
	if w = h.do("DELETE", "/api/v1/files/"+id, all, ""); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
}

// A PATCH names the fields it changes. The command underneath takes the whole
// set, because that is what a form posts, so a body with one field in it must
// not arrive as five empty ones.
func TestPatchLeavesWhatItDoesNotName(t *testing.T) {
	h := newFileHarness(t)
	all := h.token(h.owner, auth.ScopeRead, auth.ScopeWrite, auth.ScopeFiles)

	w := h.do("POST", "/api/v1/links", all,
		`{"proposition":`+strconv.FormatInt(h.prop, 10)+`,"url":"`+h.page+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /links: %d %s", w.Code, w.Body)
	}
	link := strconv.FormatInt(int64(decode(t, w)["id"].(float64)), 10)
	if w = h.do("PATCH", "/api/v1/links/"+link, all, `{"note_md":"Chapter three."}`); w.Code != http.StatusOK {
		t.Fatalf("PATCH /links: %d %s", w.Code, w.Body)
	}
	after := decode(t, w)
	if after["title"] != "The tide tables" {
		t.Fatalf("the title was cleared by a body that did not name it: %v", after["title"])
	}
	if after["note_md"] != "Chapter three." {
		t.Fatalf("the note did not land: %v", after["note_md"])
	}

	w = h.do("POST", "/api/v1/files", all, `{"proposition":`+strconv.FormatInt(h.prop, 10)+
		`,"name":"tides.md","folder":"Documents","size":4}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /files: %d %s", w.Code, w.Body)
	}
	var up files.Upload
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if w = h.do("PATCH", "/api/v1/files/"+strconv.FormatInt(up.File.ID, 10), all,
		`{"folder":"Reading"}`); w.Code != http.StatusOK {
		t.Fatalf("PATCH /files: %d %s", w.Code, w.Body)
	}
	file := decode(t, w)["file"].(map[string]any)
	if file["name"] != "tides.md" || file["folder"] != "Reading" {
		t.Fatalf("a move should not rename: %v", file)
	}
}

func TestAttachmentRoutes(t *testing.T) {
	h := newFileHarness(t)
	all := h.token(h.owner, auth.ScopeRead, auth.ScopeWrite)
	proposition := strconv.FormatInt(h.prop, 10)

	w := h.do("POST", "/api/v1/links", all, `{"proposition":`+proposition+`,"url":"`+h.page+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /links: %d %s", w.Code, w.Body)
	}
	link := strconv.FormatInt(int64(decode(t, w)["id"].(float64)), 10)
	card := strconv.FormatInt(h.card, 10)

	if w = h.do("POST", "/api/v1/cards/"+card+"/links/"+link, all, ""); w.Code != http.StatusOK {
		t.Fatalf("attach: %d %s", w.Code, w.Body)
	}
	if w = h.do("GET", "/api/v1/attachments?proposition="+proposition, all, ""); w.Code != http.StatusOK {
		t.Fatalf("attachments: %d %s", w.Code, w.Body)
	}
	if n := len(decode(t, w)["links"].([]any)); n != 1 {
		t.Fatalf("got %d attached links, want 1", n)
	}
	if w = h.do("DELETE", "/api/v1/cards/"+card+"/links/"+link, all, ""); w.Code != http.StatusOK {
		t.Fatalf("detach: %d %s", w.Code, w.Body)
	}
	if w = h.do("GET", "/api/v1/attachments?proposition="+proposition, all, ""); len(decode(t, w)["links"].([]any)) != 0 {
		t.Fatal("the attachment is still there after detaching")
	}
}
