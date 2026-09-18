// Package links reads the metadata a research link is worth keeping: who wrote
// it, when, what it is called and what kind of thing it is. It holds no state
// and stores nothing.
package links

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// Meta is what a page says about itself.
type Meta struct {
	Title     string
	Author    string
	Published time.Time
	SiteName  string
	// Kind is one of paper, video, book, thread, dataset or article.
	Kind string
	// KindGuessed is true when Kind came from the host or from the fallback to
	// article, and false when the page declared it through citation tags,
	// JSON-LD or og:type. The UI lets a person correct a guess.
	KindGuessed  bool
	Description  string
	CanonicalURL string
	// Partial is true when the page could not be parsed as a document and the
	// fields come from a scan of its tags instead, so the caller knows the
	// metadata may be short of what the page holds.
	Partial bool
}

// maxBody is what Fetch will read from a page. Metadata lives in the head, so
// two megabytes is already generous.
const maxBody = 2 << 20

// Fetch gets rawURL through client, which is expected to come from safehttp,
// and extracts the metadata. It reads at most maxBody bytes, decompresses
// gzip, and converts the body to UTF-8 from whatever charset it declares.
func Fetch(ctx context.Context, client *http.Client, rawURL string) (Meta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Meta{}, fmt.Errorf("links: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5")
	// Asking for gzip explicitly means the answer always comes back through
	// this code rather than through the transport's own transparent path.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		return Meta{}, fmt.Errorf("links: fetching %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Meta{}, fmt.Errorf("links: fetching %s: %s", rawURL, resp.Status)
	}
	var body io.Reader = io.LimitReader(resp.Body, maxBody)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(body)
		if err != nil {
			return Meta{}, fmt.Errorf("links: reading %s: %w", rawURL, err)
		}
		defer zr.Close()
		// The cap applies again after decompression, or a small response could
		// expand into a large one.
		body = io.LimitReader(zr, maxBody)
	}
	final := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	contentType := resp.Header.Get("Content-Type")
	decoded, err := charset.NewReader(body, contentType)
	if errors.Is(err, io.EOF) {
		// An empty body is a page with nothing to say, not a failure: the link
		// still saves, with whatever the host implies.
		return Extract(final, nil, contentType), nil
	}
	if err != nil {
		return Meta{}, fmt.Errorf("links: reading %s: %w", rawURL, err)
	}
	raw, err := io.ReadAll(decoded)
	if err != nil {
		return Meta{}, fmt.Errorf("links: reading %s: %w", rawURL, err)
	}
	return Extract(final, raw, contentType), nil
}

// Extract reads the metadata out of a page already fetched from finalURL. It
// prefers citation_* meta tags, then JSON-LD, then OpenGraph, then the title
// and the canonical link.
func Extract(finalURL string, body []byte, contentType string) Meta {
	host, path := hostAndPath(finalURL)
	var p page
	var partial bool
	if isHTML(contentType, body) {
		p, partial = parse(body)
	}
	ld := p.linkedData()
	hasCitation := len(p.all("citation_title")) > 0 || len(p.all("citation_author")) > 0

	m := Meta{
		Title:        first(p.one("citation_title"), ld.title, p.one("og:title"), p.title),
		Author:       first(strings.Join(p.all("citation_author"), ", "), ld.author, notURL(p.one("article:author")), p.one("author")),
		SiteName:     first(p.one("citation_journal_title"), ld.site, p.one("og:site_name")),
		Description:  first(ld.description, p.one("og:description"), p.one("description")),
		CanonicalURL: first(p.one("og:url"), p.canonical),
		Published: parseDate(first(p.one("citation_publication_date"), p.one("citation_date"),
			ld.published, p.one("article:published_time"))),
		Partial: partial,
	}
	m.Kind, m.KindGuessed = kind(host, path, hasCitation, p.one("og:type"), ld.kind)
	return m
}

// Citation formats the fields the way the drawer copies them, leaving out
// whatever the page did not say.
func Citation(m Meta, rawURL string) string {
	var b strings.Builder
	if m.Author != "" {
		b.WriteString(m.Author)
	}
	if !m.Published.IsZero() {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "(%d)", m.Published.Year())
	}
	if b.Len() > 0 {
		b.WriteString(". ")
	}
	title := m.Title
	if title == "" {
		title = rawURL
	}
	b.WriteString(title)
	site := m.SiteName
	if site == "" {
		site, _ = hostAndPath(rawURL)
	}
	if site != "" {
		b.WriteString(". " + site)
	}
	return b.String()
}

var (
	paperHosts   = []string{"doi.org", "arxiv.org", "nature.com", "pnas.org"}
	videoHosts   = []string{"youtube.com", "youtu.be", "vimeo.com"}
	bookHosts    = []string{"penguinrandomhouse.com", "versobooks.com", "bloomsbury.com"}
	threadHosts  = []string{"bsky.app", "x.com", "twitter.com"}
	datasetHosts = []string{"data.gov", "zenodo.org", "kaggle.com"}
)

// kind decides what the link is. A page that declares a specific kind is
// believed; otherwise the host decides. og:type article is left until last,
// because every thread and dataset page also calls itself an article.
func kind(host, path string, hasCitation bool, ogType, ldType string) (string, bool) {
	switch {
	case hasCitation, ldType == "ScholarlyArticle":
		return "paper", false
	case hostIn(host, paperHosts):
		return "paper", true
	case strings.HasPrefix(ogType, "video"), ldType == "VideoObject":
		return "video", false
	case hostIn(host, videoHosts):
		return "video", true
	case ogType == "book", ldType == "Book":
		return "book", false
	case hostIn(host, bookHosts):
		return "book", true
	case ldType == "Dataset":
		return "dataset", false
	case hostIn(host, threadHosts), strings.HasPrefix(path, "/@"):
		return "thread", true
	case hostIn(host, datasetHosts):
		return "dataset", true
	case ogType == "article", strings.HasSuffix(ldType, "Article"):
		return "article", false
	}
	return "article", true
}

func hostIn(host string, hosts []string) bool {
	for _, h := range hosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

func hostAndPath(rawURL string) (string, string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."), u.EscapedPath()
}

// page is everything one walk of the document collects.
type page struct {
	meta      map[string][]string
	title     string
	canonical string
	scripts   []string
}

func (p page) all(key string) []string { return p.meta[key] }

func (p page) one(key string) string {
	if v := p.meta[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// element records what one tag contributes. text is the tag's text content,
// which only title and script use.
func (p *page) element(tag string, attrs []html.Attribute, text string) {
	switch tag {
	case "meta":
		key := attrOf(attrs, "property")
		if key == "" {
			key = attrOf(attrs, "name")
		}
		if key = strings.ToLower(strings.TrimSpace(key)); key != "" {
			p.meta[key] = append(p.meta[key], strings.TrimSpace(attrOf(attrs, "content")))
		}
	case "title":
		if p.title == "" {
			p.title = strings.TrimSpace(text)
		}
	case "link":
		if p.canonical == "" && hasToken(attrOf(attrs, "rel"), "canonical") {
			p.canonical = strings.TrimSpace(attrOf(attrs, "href"))
		}
	case "script":
		if strings.Contains(strings.ToLower(attrOf(attrs, "type")), "ld+json") {
			p.scripts = append(p.scripts, text)
		}
	}
}

// parse reads a page. It returns partial true when the document could not be
// built, which x/net/html refuses to do past 512 open elements, in which case
// the tags are scanned instead of walked.
func parse(body []byte) (p page, partial bool) {
	p = page{meta: map[string][]string{}}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return scan(body), true
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			var text string
			if n.Data == "title" || n.Data == "script" {
				text = textOf(n)
			}
			p.element(n.Data, n.Attr, text)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return p, false
}

// scan reads the same tags with the tokenizer, which keeps no stack of open
// elements and so gets through a page the parser gives up on.
func scan(body []byte) page {
	p := page{meta: map[string][]string{}}
	z := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return p
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			var attrs []html.Attribute
			for hasAttr {
				key, val, more := z.TagAttr()
				attrs = append(attrs, html.Attribute{Key: string(key), Val: string(val)})
				hasAttr = more
			}
			var text string
			if tag == "title" || tag == "script" {
				if z.Next() == html.TextToken {
					text = string(z.Text())
				}
			}
			p.element(tag, attrs, text)
		}
	}
}

func attrOf(attrs []html.Attribute, name string) string {
	for _, a := range attrs {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}

func hasToken(list, want string) bool {
	for _, f := range strings.Fields(strings.ToLower(list)) {
		if f == want {
			return true
		}
	}
	return false
}

func textOf(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode:
			b.WriteString(c.Data)
		case html.ElementNode:
			b.WriteString(textOf(c))
		}
	}
	return b.String()
}

// linked is the part of a JSON-LD node this package uses.
type linked struct {
	kind, title, author, published, site, description string
}

func (p page) linkedData() linked {
	for _, raw := range p.scripts {
		var v any
		if json.Unmarshal([]byte(raw), &v) != nil {
			continue
		}
		if l, ok := findLinked(v); ok {
			return l
		}
	}
	return linked{}
}

var ldTypes = []string{"Article", "NewsArticle", "BlogPosting", "ScholarlyArticle", "Book", "VideoObject", "Dataset"}

// findLinked walks the arrays and @graph wrappers publishers put their JSON-LD
// in and returns the first node of a type worth reading.
func findLinked(v any) (linked, bool) {
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if l, ok := findLinked(e); ok {
				return l, true
			}
		}
	case map[string]any:
		if g, ok := t["@graph"]; ok {
			if l, ok := findLinked(g); ok {
				return l, true
			}
		}
		kind := ldType(t["@type"])
		if kind == "" {
			return linked{}, false
		}
		return linked{
			kind:        kind,
			title:       first(ldString(t["headline"]), ldString(t["name"])),
			author:      ldName(t["author"]),
			published:   ldString(t["datePublished"]),
			site:        ldName(t["publisher"]),
			description: ldString(t["description"]),
		}, true
	}
	return linked{}, false
}

func ldType(v any) string {
	for _, s := range ldStrings(v) {
		for _, want := range ldTypes {
			if s == want {
				return s
			}
		}
	}
	return ""
}

// ldName reads the name of an author or publisher, which may be a string, an
// object with a name, or a list of either.
func ldName(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case map[string]any:
		return ldString(t["name"])
	case []any:
		var names []string
		for _, e := range t {
			if n := ldName(e); n != "" {
				names = append(names, n)
			}
		}
		return strings.Join(names, ", ")
	}
	return ""
}

func ldString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func ldStrings(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// dateLayouts covers RFC 3339 and the shapes citation tags and publishers use.
var dateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02",
	"2006/01/02",
	"2006/1/2",
	"2006-01",
	"January 2, 2006",
	"2 January 2006",
	"2006",
}

func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func first(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// notURL drops article:author values that are a link to a profile rather than a
// name, which is what most publishing platforms put there.
func notURL(s string) string {
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		return ""
	}
	return s
}

func isHTML(contentType string, body []byte) bool {
	if contentType != "" {
		t := strings.ToLower(contentType)
		return strings.Contains(t, "html") || strings.Contains(t, "xml")
	}
	return bytes.Contains(bytes.ToLower(body[:min(len(body), 1024)]), []byte("<html"))
}
