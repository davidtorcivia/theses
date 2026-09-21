package files

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
)

func TestAddLinkReadsThePage(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	url := page(t, `<html><head>
		<meta property="og:title" content="The tide tables">
		<meta name="citation_author" content="Ada Lovelace">
		<meta name="citation_publication_date" content="2019-04-01">
		<meta property="og:site_name" content="Example Review">
		</head><body><p>Twice a day the water climbs.</p>
		<script>var hidden = "not for the index";</script></body></html>`)

	e, err := f.AddLink(ctx, f.who["editor"], f.prop, url)
	if err != nil {
		t.Fatal(err)
	}
	link, err := GetLink(ctx, f.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if link.Title != "The tide tables" {
		t.Errorf("title %q", link.Title)
	}
	if link.Author != "Ada Lovelace" {
		t.Errorf("author %q", link.Author)
	}
	if link.Year != "2019" {
		t.Errorf("year %q", link.Year)
	}
	// A citation tag is a paper whatever the host says.
	if link.Kind != "paper" {
		t.Errorf("kind %q", link.Kind)
	}
	if link.FetchedAt == nil {
		t.Error("fetched_at is null after a fetch that worked")
	}

	var indexed string
	if err := f.db.QueryRowContext(ctx,
		`SELECT text_for_search FROM links WHERE id = ?`, link.ID).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(indexed, "Twice a day") {
		t.Errorf("readable text was not stored: %q", indexed)
	}
	if strings.Contains(indexed, "not for the index") {
		t.Errorf("a script's text was indexed: %q", indexed)
	}
	// The event carries the row, and never the text: it is large and no client
	// draws it.
	if strings.Contains(string(e.After), "text_for_search") {
		t.Errorf("the event carries the search text: %s", e.After)
	}
}

func TestAddLinkKeepsAPageThatSaysNothing(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	e, err := f.AddLink(ctx, f.who["editor"], f.prop, srv.URL+"/paper")
	if err != nil {
		t.Fatalf("a link to a page that refuses should still save: %v", err)
	}
	link, err := GetLink(ctx, f.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if link.URL != srv.URL+"/paper" {
		t.Errorf("url %q", link.URL)
	}
	if link.FetchedAt != nil {
		t.Error("fetched_at is set although nothing was read")
	}
}

// A URL is the one thing a person pastes that the server then fetches, so it
// is the way in for a request forgery. safehttp refuses the address; this is
// the gate before it, on what a URL may be at all.
func TestAddLinkRefusesWhatIsNotAWebAddress(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, url string
		want      error
	}{
		{"a file URL", "file:///etc/passwd", ErrURL},
		{"a javascript URL", "javascript:alert(1)", ErrURL},
		{"a data URL", "data:text/html,<script>", ErrURL},
		{"a gopher URL", "gopher://example.com/1", ErrURL},
		{"nothing at all", "   ", ErrURL},
		{"no host", "http:///nowhere", ErrURL},
		{"a private address", "http://127.0.0.1:1/x", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.AddLink(ctx, f.who["editor"], f.prop, tc.url)
			if !errors.Is(err, tc.want) {
				t.Fatalf("AddLink(%q): %v, want %v", tc.url, err, tc.want)
			}
		})
	}
}

// The refetch is a second way to make the server fetch a URL, and it takes an
// id rather than an address, so the authorization is what stands between a
// stranger and it.
func TestRefetchNeedsTheSameStanding(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	first := page(t, "<title>Before</title>")
	e, err := f.AddLink(ctx, f.who["editor"], f.prop, first)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, who string
		want      error
	}{
		{"a stranger is told it is not there", "outsider", core.ErrNotFound},
		{"a guest may not write", "guest", core.ErrForbidden},
		{"a researcher may", "researcher", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.RefetchLink(ctx, f.who[tc.who], e.EntityID)
			if !errors.Is(err, tc.want) {
				t.Fatalf("RefetchLink: %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEditLinkValidatesItsFields(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	e, err := f.AddLink(ctx, f.who["editor"], f.prop, page(t, "<title>Tides</title>"))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		in   Edit
		want error
	}{
		{"a kind the list does not have", Edit{Kind: "manuscript"}, ErrKind},
		{"a question that is not one of the four", Edit{Question: "V"}, ErrQuestion},
		{"a note longer than the field", Edit{Note: strings.Repeat("x", board.MaxBody+1)}, board.ErrTooLong},
		{"a correction", Edit{Title: "Tide tables", Kind: "book", Question: "II", Note: "Chapter 3."}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.EditLink(ctx, f.who["editor"], e.EntityID, tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("EditLink: %v, want %v", err, tc.want)
			}
		})
	}

	link, err := GetLink(ctx, f.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if link.Title != "Tide tables" || link.Kind != "book" || link.Note != "Chapter 3." {
		t.Fatalf("after the correction: %+v", link)
	}
	if link.Question == nil || *link.Question != "II" {
		t.Fatalf("question %v", link.Question)
	}
}

// An edit can be taken back: it is the one link action undo can put right,
// because a create would have to be a delete and a delete a new row.
func TestUndoPutsALinkBack(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	e, err := f.AddLink(ctx, f.who["editor"], f.prop, page(t, "<title>Tides</title>"))
	if err != nil {
		t.Fatal(err)
	}
	edit, err := f.EditLink(ctx, f.who["editor"], e.EntityID, Edit{Title: "Wrong", Kind: "video"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Undo(ctx, f.who["editor"], edit.Seq); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	link, err := GetLink(ctx, f.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if link.Title != "Tides" {
		t.Fatalf("title after undo is %q, want the one the page gave", link.Title)
	}
	if _, err := f.Undo(ctx, f.who["editor"], e.Seq); !errors.Is(err, core.ErrNotUndoable) {
		t.Fatalf("undoing a create: %v, want not undoable", err)
	}
}

func TestCitationIsBuiltFromTheFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		link Link
		want string
	}{
		{
			name: "everything present",
			link: Link{Author: "Ada Lovelace", Year: "2019", Title: "The tide tables", URL: "https://example.com/tides"},
			want: "Ada Lovelace (2019). The tide tables. example.com",
		},
		{
			name: "no author and no year",
			link: Link{Title: "The tide tables", URL: "https://example.com/tides"},
			want: "The tide tables. example.com",
		},
		{
			name: "nothing but the address",
			link: Link{URL: "https://example.com/tides"},
			want: "https://example.com/tides. example.com",
		},
		{
			name: "a year that is not a year",
			link: Link{Author: "Ada Lovelace", Year: "n.d.", Title: "Tides", URL: "https://example.com/t"},
			want: "Ada Lovelace. Tides. example.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Citation(tc.link); got != tc.want {
				t.Fatalf("Citation() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCanonicalURLIsNotBelieved(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	url := page(t, `<html><head><title>Tides</title>
		<link rel="canonical" href="javascript:alert(1)"></head></html>`)
	e, err := f.AddLink(ctx, f.who["editor"], f.prop, url)
	if err != nil {
		t.Fatal(err)
	}
	link, err := GetLink(ctx, f.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if link.CanonicalURL != "" {
		t.Fatalf("canonical_url is %q; a page's own value is not a web address", link.CanonicalURL)
	}
}

func TestLinkPatchesKeepOtherFields(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	e, err := f.AddLink(ctx, f.who["editor"], f.prop, page(t, "<title>Original</title>"))
	if err != nil {
		t.Fatal(err)
	}
	note, kind, question := "Research note", "paper", "II"
	for _, patch := range []LinkPatch{{Note: &note}, {Kind: &kind}, {Question: &question}} {
		if _, err := f.PatchLink(ctx, f.who["editor"], e.EntityID, patch); err != nil {
			t.Fatal(err)
		}
	}
	row, err := GetLink(ctx, f.db, e.EntityID)
	if err != nil || row.Title != "Original" || row.Note != note || row.Kind != kind || row.Question == nil || *row.Question != question {
		t.Fatalf("patch: %+v, %v", row, err)
	}
	empty := ""
	if _, err := f.PatchLink(ctx, f.who["editor"], e.EntityID, LinkPatch{Question: &empty}); err != nil {
		t.Fatal(err)
	}
	row, err = GetLink(ctx, f.db, e.EntityID)
	if err != nil || row.Question != nil || row.Note != note {
		t.Fatalf("clear: %+v, %v", row, err)
	}
}
