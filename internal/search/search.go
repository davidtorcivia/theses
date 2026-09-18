// Package search is the one query behind the ⌘K palette and GET /api/v1/search:
// the FTS5 tables for cards, blocks, links, files and comments, plus the two
// small tables a person searches by name rather than by text.
package search

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/davidtorcivia/theses/internal/store"
)

// Kinds, in the order the palette shows them.
const (
	KindProposition = "proposition"
	KindCard        = "card"
	KindBlock       = "block"
	KindLink        = "link"
	KindFile        = "file"
	KindComment     = "comment"
	KindUser        = "user"
)

// A Hit is one row. Title is what to show; Snippet is the matching text around
// the query, plain text with an ellipsis where it was cut.
type Hit struct {
	Kind          string `json:"kind"`
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	Snippet       string `json:"snippet,omitempty"`
	PropositionID int64  `json:"proposition_id,omitempty"`
}

// A Group is the hits of one kind. Kinds with no hits are left out.
type Group struct {
	Kind string `json:"kind"`
	Hits []Hit  `json:"hits"`
}

// DefaultLimit is how many hits a kind returns when the caller does not say.
const DefaultLimit = 10

// MaxLimit caps what a caller may ask for. The palette shows a handful and the
// API is read by agents, so no caller needs a page of results.
const MaxLimit = 50

// snippetArgs is the tail of every snippet() call: no markers around the match,
// because every caller puts the text somewhere the markers would have to be
// escaped anyway, an ellipsis where it was cut, and enough tokens to read a line.
const snippetArgs = `'', '', '…', 12`

// searches is every kind, in the order the palette shows them. Each selects id,
// proposition id, title and snippet, and takes one argument and a limit: the
// MATCH expression for a full text kind, the LIKE pattern for a name kind.
var searches = []struct {
	kind, query string
	like        bool
}{
	{kind: KindProposition, like: true, query: `
		SELECT id, id, title, statement FROM propositions
		WHERE title LIKE ? ESCAPE '\' AND archived_at IS NULL ORDER BY number LIMIT ?`},
	{kind: KindCard, query: `
		SELECT c.id, c.proposition_id, c.title, snippet(cards_fts, -1, ` + snippetArgs + `)
		FROM cards_fts JOIN cards c ON c.id = cards_fts.rowid
		WHERE cards_fts MATCH ? ORDER BY cards_fts.rank LIMIT ?`},
	{kind: KindBlock, query: `
		SELECT b.id, d.proposition_id, d.name, snippet(blocks_fts, -1, ` + snippetArgs + `)
		FROM blocks_fts JOIN blocks b ON b.id = blocks_fts.rowid
		JOIN documents d ON d.id = b.document_id
		WHERE blocks_fts MATCH ? AND b.deleted_at IS NULL ORDER BY blocks_fts.rank LIMIT ?`},
	{kind: KindLink, query: `
		SELECT l.id, l.proposition_id, coalesce(nullif(l.title, ''), l.url),
			snippet(links_fts, -1, ` + snippetArgs + `)
		FROM links_fts JOIN links l ON l.id = links_fts.rowid
		WHERE links_fts MATCH ? ORDER BY links_fts.rank LIMIT ?`},
	{kind: KindFile, query: `
		SELECT f.id, f.proposition_id, f.name, snippet(files_fts, -1, ` + snippetArgs + `)
		FROM files_fts JOIN files f ON f.id = files_fts.rowid
		WHERE files_fts MATCH ? AND f.state = 'ready' ORDER BY files_fts.rank LIMIT ?`},
	{kind: KindComment, query: `
		SELECT m.id, c.proposition_id, c.title, snippet(comments_fts, -1, ` + snippetArgs + `)
		FROM comments_fts JOIN comments m ON m.id = comments_fts.rowid
		JOIN cards c ON c.id = m.card_id
		WHERE comments_fts MATCH ? ORDER BY comments_fts.rank LIMIT ?`},
	{kind: KindUser, like: true, query: `
		SELECT id, 0, name, handle FROM users
		WHERE name LIKE ? ESCAPE '\' ORDER BY name LIMIT ?`},
}

// Search runs every kind for one query and returns the groups that matched. A
// query with nothing searchable left in it, once the FTS syntax is stripped
// out, returns no groups rather than an error.
func Search(ctx context.Context, q store.Querier, query string, limit int) ([]Group, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	like := likePattern(query)

	var groups []Group
	for _, s := range searches {
		arg := match
		if s.like {
			arg = like
		}
		hits, err := run(ctx, q, s.kind, s.query, arg, limit)
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 {
			groups = append(groups, Group{Kind: s.kind, Hits: hits})
		}
	}
	return groups, nil
}

func run(ctx context.Context, q store.Querier, kind, query, arg string, limit int) ([]Hit, error) {
	rows, err := q.QueryContext(ctx, query, arg, limit)
	if err != nil {
		return nil, fmt.Errorf("search %ss: %w", kind, err)
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		h := Hit{Kind: kind}
		if err := rows.Scan(&h.ID, &h.PropositionID, &h.Title, &h.Snippet); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// maxTerms bounds a query, so that pasting a page into the palette is one cheap
// MATCH rather than a thousand term one.
const maxTerms = 12

// ftsQuery turns what someone typed into an FTS5 MATCH expression. Every run of
// letters and digits becomes one double quoted phrase, so quotation marks,
// asterisks, colons, parentheses, carets and the bare words NEAR, AND, OR and
// NOT are text to match rather than syntax to obey. The last phrase takes a
// prefix star, so the palette finds something while a word is half typed. Input
// with no letter or digit in it returns "", and the caller searches for nothing.
func ftsQuery(s string) string {
	terms := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(terms) == 0 {
		return ""
	}
	if len(terms) > maxTerms {
		terms = terms[:maxTerms]
	}
	for i, t := range terms {
		terms[i] = `"` + t + `"`
	}
	terms[len(terms)-1] += "*"
	return strings.Join(terms, " ")
}

// likePattern wraps the query in wildcards for the tables searched by name, with
// LIKE's own three special characters escaped so that they match themselves.
func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(strings.TrimSpace(s)) + "%"
}
