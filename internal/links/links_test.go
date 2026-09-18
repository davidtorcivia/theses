package links

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/safehttp"
	"golang.org/x/net/html"
)

func date(t *testing.T, layout, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(layout, value)
	if err != nil {
		t.Fatalf("bad test date %q: %v", value, err)
	}
	return parsed
}

func TestExtract(t *testing.T) {
	tests := []struct {
		fixture string
		url     string
		want    Meta
	}{
		{
			fixture: "paper.html",
			url:     "https://www.nature.com/articles/s41586-024-07123-4",
			want: Meta{
				Title:        "Ice loss in the Amundsen Sea Embayment",
				Author:       "Ada Lovelace, Grace Hopper",
				Published:    date(t, "2006-01-02", "2024-03-13"),
				SiteName:     "Nature",
				Kind:         "paper",
				Description:  "A twenty year record of thinning across the embayment.",
				CanonicalURL: "https://www.nature.com/articles/s41586-024-07123-4",
			},
		},
		{
			fixture: "article-og.html",
			url:     "https://riverbed.example/long-emergency",
			want: Meta{
				Title:        "The Long Emergency",
				Author:       "Marion Stokes",
				Published:    date(t, time.RFC3339, "2025-11-02T09:30:00Z"),
				SiteName:     "The Riverbed",
				Kind:         "article",
				Description:  "What a decade of deferred maintenance costs.",
				CanonicalURL: "https://riverbed.example/long-emergency",
			},
		},
		{
			fixture: "book-jsonld.html",
			url:     "https://www.bloomsbury.com/fossil-capital",
			want: Meta{
				Title:        "Fossil Capital",
				Author:       "Andreas Malm",
				Published:    date(t, "2006-01-02", "2016-01-05"),
				SiteName:     "Verso Books",
				Kind:         "book",
				Description:  "The rise of steam power and the roots of global warming.",
				CanonicalURL: "https://www.bloomsbury.com/fossil-capital",
			},
		},
		{
			fixture: "video-og.html",
			url:     "https://www.youtube.com/watch?v=abc123",
			want: Meta{
				Title:        "Grid failure explained",
				Author:       "Practical Engineering",
				SiteName:     "YouTube",
				Kind:         "video",
				Description:  "Twelve minutes on how the February outage spread.",
				CanonicalURL: "https://www.youtube.com/watch?v=abc123",
			},
		},
		{
			fixture: "bare.html",
			url:     "https://records.example.org/minutes/1974-05-14",
			want: Meta{
				Title:        "Minutes of the water board, 14 May 1974",
				Kind:         "article",
				KindGuessed:  true,
				CanonicalURL: "https://records.example.org/minutes/1974-05-14",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			body, err := os.ReadFile("testdata/" + tt.fixture)
			if err != nil {
				t.Fatal(err)
			}
			got := Extract(tt.url, body, "text/html; charset=utf-8")
			if !got.Published.Equal(tt.want.Published) {
				t.Errorf("Published = %v, want %v", got.Published, tt.want.Published)
			}
			got.Published, tt.want.Published = time.Time{}, time.Time{}
			if got != tt.want {
				t.Errorf("Extract =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestExtractNonHTML(t *testing.T) {
	got := Extract("https://arxiv.org/pdf/2401.00001", []byte("%PDF-1.7\n"), "application/pdf")
	want := Meta{Kind: "paper", KindGuessed: true}
	if got != want {
		t.Errorf("Extract = %+v, want %+v", got, want)
	}
}

func TestKind(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		path        string
		citation    bool
		ogType      string
		ldType      string
		want        string
		wantGuessed bool
	}{
		{name: "doi host", host: "doi.org", path: "/10.1038/x", want: "paper", wantGuessed: true},
		{name: "arxiv subdomain", host: "export.arxiv.org", path: "/abs/2401", want: "paper", wantGuessed: true},
		{name: "citation tags anywhere", host: "journal.example", citation: true, want: "paper", wantGuessed: false},
		{name: "scholarly article", host: "journal.example", ldType: "ScholarlyArticle", want: "paper", wantGuessed: false},
		{name: "youtu.be", host: "youtu.be", path: "/abc", want: "video", wantGuessed: true},
		{name: "vimeo", host: "vimeo.com", path: "/1", want: "video", wantGuessed: true},
		{name: "og video", host: "clips.example", ogType: "video.other", want: "video", wantGuessed: false},
		{name: "publisher host", host: "penguinrandomhouse.com", path: "/books/1", want: "book", wantGuessed: true},
		{name: "verso", host: "www.versobooks.com", path: "/books/1", want: "book", wantGuessed: true},
		{name: "json-ld book", host: "shop.example", ldType: "Book", want: "book", wantGuessed: false},
		{name: "bluesky", host: "bsky.app", path: "/profile/x/post/1", want: "thread", wantGuessed: true},
		{name: "twitter", host: "twitter.com", path: "/x/status/1", want: "thread", wantGuessed: true},
		{name: "mastodon style path", host: "hachyderm.io", path: "/@someone/1234", want: "thread", wantGuessed: true},
		{name: "thread host beats og article", host: "bsky.app", path: "/profile/x", ogType: "article", want: "thread", wantGuessed: true},
		{name: "zenodo", host: "zenodo.org", path: "/record/1", want: "dataset", wantGuessed: true},
		{name: "data.gov subdomain", host: "catalog.data.gov", path: "/dataset/x", want: "dataset", wantGuessed: true},
		{name: "og article", host: "blog.example", path: "/post", ogType: "article", want: "article", wantGuessed: false},
		{name: "json-ld news article", host: "blog.example", path: "/post", ldType: "NewsArticle", want: "article", wantGuessed: false},
		{name: "nothing to go on", host: "blog.example", path: "/post", want: "article", wantGuessed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// hostIn expects the host without its www prefix, as Extract passes it.
			host := strings.TrimPrefix(tt.host, "www.")
			got, guessed := kind(host, tt.path, tt.citation, tt.ogType, tt.ldType)
			if got != tt.want || guessed != tt.wantGuessed {
				t.Errorf("kind = %q, %v, want %q, %v", got, guessed, tt.want, tt.wantGuessed)
			}
		})
	}
}

func TestCitation(t *testing.T) {
	tests := []struct {
		name string
		meta Meta
		url  string
		want string
	}{
		{
			name: "everything",
			meta: Meta{Author: "Ada Lovelace, Grace Hopper", Published: date(t, "2006-01-02", "2024-03-13"),
				Title: "Ice loss in the Amundsen Sea Embayment", SiteName: "Nature"},
			url:  "https://www.nature.com/articles/x",
			want: "Ada Lovelace, Grace Hopper (2024). Ice loss in the Amundsen Sea Embayment. Nature",
		},
		{
			name: "no date",
			meta: Meta{Author: "Marion Stokes", Title: "The Long Emergency", SiteName: "The Riverbed"},
			url:  "https://riverbed.example/long-emergency",
			want: "Marion Stokes. The Long Emergency. The Riverbed",
		},
		{
			name: "no author",
			meta: Meta{Published: date(t, "2006-01-02", "1974-05-14"), Title: "Minutes of the water board"},
			url:  "https://records.example.org/minutes/1974-05-14",
			want: "(1974). Minutes of the water board. records.example.org",
		},
		{
			name: "nothing but the url",
			meta: Meta{},
			url:  "https://records.example.org/minutes/1974-05-14",
			want: "https://records.example.org/minutes/1974-05-14. records.example.org",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Citation(tt.meta, tt.url); got != tt.want {
				t.Errorf("Citation = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFetch(t *testing.T) {
	page, err := os.ReadFile("testdata/article-og.html")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("plain", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(page)
		}))
		defer srv.Close()
		meta := fetch(t, srv.URL)
		if meta.Title != "The Long Emergency" || meta.Author != "Marion Stokes" {
			t.Errorf("Fetch = %+v", meta)
		}
	})

	t.Run("gzip", func(t *testing.T) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(page)
		zw.Close()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				t.Errorf("request did not ask for gzip: %q", r.Header.Get("Accept-Encoding"))
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(buf.Bytes())
		}))
		defer srv.Close()
		if meta := fetch(t, srv.URL); meta.Title != "The Long Emergency" {
			t.Errorf("Fetch title = %q, want %q", meta.Title, "The Long Emergency")
		}
	})

	t.Run("windows-1252", func(t *testing.T) {
		body := []byte("<html><head><title>Caf\xe9 ferm\xe9</title></head><body></body></html>")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=windows-1252")
			w.Write(body)
		}))
		defer srv.Close()
		if meta := fetch(t, srv.URL); meta.Title != "Café fermé" {
			t.Errorf("Fetch title = %q, want %q", meta.Title, "Café fermé")
		}
	})

	t.Run("redirect gives the final url", func(t *testing.T) {
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/final" {
				http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte("<html><head><title>Arrived</title></head></html>"))
		}))
		defer srv.Close()
		if meta := fetch(t, srv.URL+"/start"); meta.Title != "Arrived" {
			t.Errorf("Fetch title = %q, want %q", meta.Title, "Arrived")
		}
	})

	t.Run("body past the cap is truncated, not an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte("<html><head><title>Long page</title></head><body>"))
			w.Write(bytes.Repeat([]byte("padding "), maxBody/4))
		}))
		defer srv.Close()
		if meta := fetch(t, srv.URL); meta.Title != "Long page" {
			t.Errorf("Fetch title = %q, want %q", meta.Title, "Long page")
		}
	})

	t.Run("http error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "gone", http.StatusNotFound)
		}))
		defer srv.Close()
		client := safehttp.Client(safehttp.AllowLoopback())
		if _, err := Fetch(t.Context(), client, srv.URL); err == nil {
			t.Error("Fetch of a 404 returned no error")
		}
	})

	t.Run("private address", func(t *testing.T) {
		client := safehttp.Client()
		if _, err := Fetch(t.Context(), client, "http://10.0.0.1/page"); err == nil {
			t.Error("Fetch of a private address returned no error")
		}
	})
}

func fetch(t *testing.T, url string) Meta {
	t.Helper()
	client := safehttp.Client(safehttp.AllowLoopback())
	meta, err := Fetch(t.Context(), client, url)
	if err != nil {
		t.Fatalf("Fetch(%s) = %v", url, err)
	}
	return meta
}

func TestLinkedDataShapes(t *testing.T) {
	tests := []struct {
		name string
		json string
		want linked
	}{
		{
			name: "author as a string",
			json: `{"@type":"Article","headline":"H","author":"A Name","datePublished":"2020-01-02"}`,
			want: linked{kind: "Article", title: "H", author: "A Name", published: "2020-01-02"},
		},
		{
			name: "author as an object",
			json: `{"@type":"Article","headline":"H","author":{"@type":"Person","name":"A Name"}}`,
			want: linked{kind: "Article", title: "H", author: "A Name"},
		},
		{
			name: "several authors",
			json: `{"@type":"Article","name":"H","author":[{"name":"One"},"Two"]}`,
			want: linked{kind: "Article", title: "H", author: "One, Two"},
		},
		{
			name: "top level array",
			json: `[{"@type":"WebSite"},{"@type":"VideoObject","name":"V"}]`,
			want: linked{kind: "VideoObject", title: "V"},
		},
		{
			name: "no type worth reading",
			json: `{"@type":"WebSite","name":"Site"}`,
			want: linked{},
		},
		{
			name: "broken json",
			json: `{"@type":`,
			want: linked{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := page{scripts: []string{tt.json}}
			if got := p.linkedData(); got != tt.want {
				t.Errorf("linkedData = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseDate(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"2024-03-13", "2024-03-13"},
		{"2024/03/13", "2024-03-13"},
		{"2024/3/13", "2024-03-13"},
		{"2025-11-02T09:30:00Z", "2025-11-02"},
		{"March 13, 2024", "2024-03-13"},
		{"13 March 2024", "2024-03-13"},
		{"2024", "2024-01-01"},
		{"", ""},
		{"whenever", ""},
	}
	for _, tt := range tests {
		got := parseDate(tt.in)
		if tt.want == "" {
			if !got.IsZero() {
				t.Errorf("parseDate(%q) = %v, want the zero time", tt.in, got)
			}
			continue
		}
		if got.Format("2006-01-02") != tt.want {
			t.Errorf("parseDate(%q) = %v, want %s", tt.in, got, tt.want)
		}
	}
}

func TestAuthorFromProfileLinkIsDropped(t *testing.T) {
	body := []byte(`<html><head><title>T</title>` +
		`<meta property="article:author" content="https://facebook.com/someone"></head></html>`)
	if got := Extract("https://blog.example/post", body, "text/html"); got.Author != "" {
		t.Errorf("Author = %q, want it dropped", got.Author)
	}
}

// A 200 with nothing in it is a link worth saving, not an error.
func TestFetchEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	meta, err := Fetch(t.Context(), safehttp.Client(safehttp.AllowLoopback()), srv.URL)
	if err != nil {
		t.Fatalf("Fetch of an empty body = %v", err)
	}
	if meta.Title != "" || meta.Kind != "article" || !meta.KindGuessed {
		t.Errorf("Fetch = %+v, want an empty article with a guessed kind", meta)
	}
}

// x/net/html refuses to build a document past 512 open elements. The metadata
// sits in the head, well before that, so it is read with the tokenizer and the
// caller is told the page was only scanned.
func TestExtractDeeplyNestedPage(t *testing.T) {
	body := []byte(`<!DOCTYPE html><html><head>` +
		`<title>Deep page</title>` +
		`<meta property="og:title" content="Deep page">` +
		`<meta property="article:published_time" content="2021-06-01">` +
		`<meta name="author" content="Someone">` +
		`<link rel="canonical" href="https://deep.example/page">` +
		`</head><body>` + strings.Repeat("<div>", 600) + `text`)

	if _, err := html.Parse(bytes.NewReader(body)); err == nil {
		t.Fatal("html.Parse handled 600 unclosed divs, so this test no longer covers the fallback")
	}
	got := Extract("https://deep.example/page", body, "text/html")
	if !got.Partial {
		t.Error("Partial = false, want true")
	}
	if got.Title != "Deep page" {
		t.Errorf("Title = %q, want %q", got.Title, "Deep page")
	}
	if got.CanonicalURL != "https://deep.example/page" {
		t.Errorf("CanonicalURL = %q, want the canonical link", got.CanonicalURL)
	}
	if got.Author != "Someone" {
		t.Errorf("Author = %q, want %q", got.Author, "Someone")
	}
	if got.Published.Format("2006-01-02") != "2021-06-01" {
		t.Errorf("Published = %v, want 2021-06-01", got.Published)
	}
}
