package files

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/safehttp"
	"github.com/davidtorcivia/theses/internal/store"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

type fixture struct {
	*Service
	board  *board.Service
	db     *store.DB
	who    map[string]core.Actor
	prop   int64
	other  int64
	card   int64
	bucket *blob.Client
	// second is the recordings bucket, so a cross bucket move can be refused.
	second *blob.Client
}

// setup is one owner, one editor, one researcher, one guest and one outsider,
// with a proposition the first four are members of, a card on it, and two
// buckets in an in-process fake.
func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db := store.OpenTemp(t)
	c := core.New(db, core.NewBus())
	b := board.New(c, func() board.Defaults {
		return board.Defaults{Status: "idea", Statuses: []string{"idea", "recording"}, Columns: []string{"Research"}}
	})

	primary, recordings := buckets(t)
	f := &fixture{board: b, db: db, who: map[string]core.Actor{}, bucket: primary, second: recordings}
	f.Service = New(c, func(_ context.Context, folder string) (*blob.Client, error) {
		if folder == Recordings {
			return f.second, nil
		}
		return f.bucket, nil
	}, safehttp.Client(safehttp.AllowLoopback()))

	for _, u := range []struct{ handle, role string }{
		{"owner", auth.RoleOwner},
		{"editor", auth.RoleEditor},
		{"researcher", auth.RoleResearcher},
		{"guest", auth.RoleGuest},
		{"outsider", auth.RoleEditor},
	} {
		id, err := store.CreateUser(ctx, db, &store.User{
			Handle: u.handle, Email: u.handle + "@example.com", Name: u.handle,
			Initials: "XX", Colour: "#1100ff", Role: u.role, PasswordHash: "x",
		})
		if err != nil {
			t.Fatal(err)
		}
		f.who[u.handle] = core.Actor{Kind: core.KindUser, ID: id, Name: u.handle}
	}

	e, err := b.CreateProposition(ctx, f.who["owner"], "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	f.prop = e.EntityID
	for _, handle := range []string{"editor", "researcher", "guest"} {
		if _, err := b.AddMember(ctx, f.who["owner"], f.prop, f.who[handle].ID); err != nil {
			t.Fatal(err)
		}
	}
	if e, err = b.CreateProposition(ctx, f.who["owner"], "Grid Storage"); err != nil {
		t.Fatal(err)
	}
	f.other = e.EntityID

	cols, err := board.ListColumns(ctx, db, f.prop)
	if err != nil {
		t.Fatal(err)
	}
	card, err := b.CreateCard(ctx, f.who["editor"], cols[0].ID, "Read the tide tables", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.card = card.EntityID
	return f
}

// buckets starts gofakes3 in process with the two buckets the app can be
// configured with. No container, no MinIO, no network beyond loopback.
func buckets(t *testing.T) (*blob.Client, *blob.Client) {
	t.Helper()
	backend := s3mem.New()
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	out := make([]*blob.Client, 2)
	for i, name := range []string{"theses", "theses-recordings"} {
		if err := backend.CreateBucket(name); err != nil {
			t.Fatalf("create bucket %s: %v", name, err)
		}
		c, err := blob.New(blob.Config{
			Provider: "s3", Endpoint: srv.URL, Region: "us-east-1",
			Bucket: name, AccessKey: "key", SecretKey: "secret",
		})
		if err != nil {
			t.Fatalf("client for %s: %v", name, err)
		}
		out[i] = c
	}
	return out[0], out[1]
}

// page serves one HTML document over loopback, which is what a link fetch
// reads instead of the internet.
func page(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestVisibleIsTheReadRule(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.AddLink(ctx, f.who["editor"], f.prop, page(t, "<title>Tides</title>")); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, who string
		want      error
	}{
		{"an owner reads every proposition", "owner", nil},
		{"a member reads theirs", "editor", nil},
		{"a guest reads what they are a member of", "guest", nil},
		{"somebody who is not a member is not told it is there", "outsider", core.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.ListLinks(ctx, f.who[tc.who], f.prop)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ListLinks: %v, want %v", err, tc.want)
			}
			if err == nil && len(got) != 1 {
				t.Fatalf("got %d links, want 1", len(got))
			}
		})
	}
}

func TestAttachmentsStayWithinAProposition(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	mine, err := f.AddLink(ctx, f.who["editor"], f.prop, page(t, "<title>Tides</title>"))
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := f.AddLink(ctx, f.who["owner"], f.other, page(t, "<title>Batteries</title>"))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		who  string
		link int64
		want error
	}{
		{"a member attaches a link on the same proposition", "editor", mine.EntityID, nil},
		{"a link on another proposition is not there", "editor", theirs.EntityID, core.ErrNotFound},
		{"somebody who is not a member sees no card", "outsider", mine.EntityID, core.ErrNotFound},
		{"a guest may read but not attach", "guest", mine.EntityID, core.ErrForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.AttachLink(ctx, f.who[tc.who], f.card, tc.link)
			if !errors.Is(err, tc.want) {
				t.Fatalf("AttachLink: %v, want %v", err, tc.want)
			}
		})
	}

	got, err := f.Attachments(ctx, f.who["editor"], f.prop)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Links) != 1 || got.Links[0].CardID != f.card || got.Links[0].LinkID != mine.EntityID {
		t.Fatalf("attachments: %+v", got.Links)
	}

	if _, err := f.DetachLink(ctx, f.who["editor"], f.card, mine.EntityID); err != nil {
		t.Fatal(err)
	}
	if got, err = f.Attachments(ctx, f.who["editor"], f.prop); err != nil {
		t.Fatal(err)
	}
	if len(got.Links) != 0 {
		t.Fatalf("after detach: %+v", got.Links)
	}
}

func TestArchivedIsReadOnly(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	link, err := f.AddLink(ctx, f.who["editor"], f.prop, page(t, "<title>Tides</title>"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.board.ArchiveProposition(ctx, f.who["owner"], f.prop); err != nil {
		t.Fatal(err)
	}

	if _, err := f.AddLink(ctx, f.who["editor"], f.prop, page(t, "<title>More</title>")); !errors.Is(err, board.ErrArchived) {
		t.Fatalf("AddLink on an archived proposition: %v", err)
	}
	if _, err := f.EditLink(ctx, f.who["editor"], link.EntityID, Edit{Title: "Tides"}); !errors.Is(err, board.ErrArchived) {
		t.Fatalf("EditLink on an archived proposition: %v", err)
	}
	if _, err := f.Create(ctx, f.who["editor"], f.prop, "notes.md", "Documents", 10, 0); !errors.Is(err, board.ErrArchived) {
		t.Fatalf("Create on an archived proposition: %v", err)
	}
	if _, err := f.ListLinks(ctx, f.who["editor"], f.prop); err != nil {
		t.Fatalf("reading an archived proposition: %v", err)
	}
}

// The citation is on the row, not on one surface's view of it, so an event is
// as complete as an HTTP answer: a tab that replaces the link it holds with an
// event's payload keeps the line it was showing.
func TestEveryLinkCarriesItsCitation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	e, err := f.AddLink(ctx, f.who["editor"], f.prop, page(t,
		`<html><head><meta property="og:title" content="The tide tables">
		<meta name="citation_author" content="Ada Lovelace"></head></html>`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(e.After), `"citation":"Ada Lovelace`) {
		t.Fatalf("the event payload has no citation: %s", e.After)
	}
	rows, err := f.ListLinks(ctx, f.who["editor"], f.prop)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !strings.HasPrefix(rows[0].Citation, "Ada Lovelace") {
		t.Fatalf("the listed link has no citation: %+v", rows)
	}
}

func TestFilenameIsSanitised(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		fails          bool
	}{
		{name: "a plain name is kept", in: "tide-tables.pdf", want: "tide-tables.pdf"},
		{name: "a path is reduced to its base", in: "../../backups/secret.tar", want: "secret.tar"},
		{name: "a windows path is reduced too", in: `C:\Users\x\notes.md`, want: "notes.md"},
		{name: "an empty segment cannot be made", in: "a//b.txt", want: "b.txt"},
		{name: "a leading slash is gone", in: "/etc/passwd", want: "passwd"},
		{name: "a dot segment is refused", in: "..", fails: true},
		{name: "a control character is dropped", in: "no\x00tes\x1f.md", want: "notes.md"},
		{name: "a leading dot cannot hide a thumbnail", in: ".thumb.jpg", want: "thumb.jpg"},
		{name: "an emoji survives", in: "tide 🌊.wav", want: "tide 🌊.wav"},
		{name: "a quote cannot break a header", in: `a"b.txt`, want: "a-b.txt"},
		{name: "nothing but spaces is refused", in: "   ", fails: true},
		// The same name typed on two platforms: an e with a combining acute,
		// and the single character for the same letter. One key, one file, and
		// so one offer to replace it rather than two files that never meet.
		{name: "two spellings of one name are one name",
			in: "tidé.wav", want: "tidé.wav"},
		{name: "a long name keeps its extension", in: strings.Repeat("a", 400) + ".wav",
			want: strings.Repeat("a", maxName-4) + ".wav"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := filename(tc.in)
			if tc.fails {
				if err == nil {
					t.Fatalf("filename(%q) = %q, want a refusal", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("filename(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("filename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestObjectKeysAreValid(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, name := range []string{"tide-tables.pdf", "../../backups/x.tar", "a//b.txt", "tide 🌊.wav"} {
		up, err := f.Create(ctx, f.who["editor"], f.prop, name, "Documents", 12, 0)
		if err != nil {
			t.Fatalf("Create(%q): %v", name, err)
		}
		// blob refuses a key it could not sign; presigning is the same gate the
		// upload goes through, so a key that gets a URL is a key the bucket
		// will see the way it was signed.
		if _, _, err := f.bucket.PresignPut(ctx, up.File.ObjectKey, "text/plain", 12, uploadTTL); err != nil {
			t.Fatalf("key %q from name %q: %v", up.File.ObjectKey, name, err)
		}
		if !strings.HasPrefix(up.File.ObjectKey, "01-tidal-power/") {
			t.Fatalf("key %q does not start with the proposition prefix", up.File.ObjectKey)
		}
	}
}

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Tidal Power", "tidal-power"},
		{"  Spaces   everywhere  ", "spaces-everywhere"},
		{"Ångström & Co.", "ngstr-m-co"},
		{"", "untitled"},
		{"...", "untitled"},
	} {
		if got := slug(tc.in); got != tc.want {
			t.Errorf("slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
