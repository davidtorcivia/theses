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
	URL           string `json:"url,omitempty"`
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

// A Reader is who is searching. An owner reads every proposition; everybody
// else reads the ones they are a member of, which is the rule the rest of the
// app uses. The zero value is a member of nothing, so forgetting to say who is
// searching shows them their own name and no more.
type Reader struct {
	All    bool
	UserID int64
}

// Everything is the Reader for a caller who may read the whole workspace.
func Everything() Reader { return Reader{All: true} }

// Member is the Reader for everybody else.
func Member(userID int64) Reader { return Reader{UserID: userID} }

// DefaultLimit is how many hits a kind returns when the caller does not say.
const DefaultLimit = 10

// MaxLimit caps what a caller may ask for. The palette shows a handful and the
// API is read by agents, so no caller needs a page of results.
const MaxLimit = 50

// snippetArgs is the tail of every snippet() call: no markers around the match,
// because every caller puts the text somewhere the markers would have to be
// escaped anyway, an ellipsis where it was cut, and enough tokens to read a line.
const snippetArgs = `'', '', '…', 12`

// memberOf is the membership test, put where {member} stands in a query so
// that the limit counts hits the reader may actually see. Filtering afterwards
// would let one busy proposition they are not a member of fill the page.
const memberOf = `IN (SELECT proposition_id FROM proposition_members WHERE user_id = ?)`

// searches is every kind, in the order the palette shows them. Each selects id,
// proposition id, title and snippet, and takes one argument and a limit: the
// MATCH expression for a full text kind, the LIKE pattern for a name kind.
// prop is the column holding the proposition a hit belongs to, and {member} is
// where the membership test goes; a kind that belongs to no proposition has
// neither and is searched by anyone who may search at all.
var searches = []struct {
	kind, query, prop string
	like              bool
}{
	{kind: KindProposition, like: true, prop: "id", query: `
		SELECT id, id, title, statement FROM propositions
		WHERE title LIKE ? ESCAPE '\' AND archived_at IS NULL {member} ORDER BY number LIMIT ?`},
	{kind: KindCard, prop: "c.proposition_id", query: `
		SELECT c.id, c.proposition_id, c.title, snippet(cards_fts, -1, ` + snippetArgs + `)
		FROM cards_fts JOIN cards c ON c.id = cards_fts.rowid
		WHERE cards_fts MATCH ? {member} ORDER BY cards_fts.rank LIMIT ?`},
	{kind: KindBlock, prop: "d.proposition_id", query: `
		SELECT b.id, d.proposition_id, d.name, snippet(blocks_fts, -1, ` + snippetArgs + `)
		FROM blocks_fts JOIN blocks b ON b.id = blocks_fts.rowid
		JOIN documents d ON d.id = b.document_id
		WHERE blocks_fts MATCH ? AND b.deleted_at IS NULL {member} ORDER BY blocks_fts.rank LIMIT ?`},
	{kind: KindLink, prop: "l.proposition_id", query: `
		SELECT l.id, l.proposition_id, coalesce(nullif(l.title, ''), l.url),
			snippet(links_fts, -1, ` + snippetArgs + `)
		FROM links_fts JOIN links l ON l.id = links_fts.rowid
		WHERE links_fts MATCH ? {member} ORDER BY links_fts.rank LIMIT ?`},
	{kind: KindFile, prop: "f.proposition_id", query: `
		SELECT f.id, f.proposition_id, f.name, snippet(files_fts, -1, ` + snippetArgs + `)
		FROM files_fts JOIN files f ON f.id = files_fts.rowid
		WHERE files_fts MATCH ? AND f.state = 'ready' {member} ORDER BY files_fts.rank LIMIT ?`},
	{kind: "transcript", prop: "f.proposition_id", query: `SELECT f.id,f.proposition_id,f.name,snippet(transcripts_fts,-1,` + snippetArgs + `) FROM transcripts_fts JOIN files f ON f.id=transcripts_fts.rowid WHERE transcripts_fts MATCH ? AND f.state='ready' {member} ORDER BY transcripts_fts.rank LIMIT ?`},
	{kind: "evidence", prop: "e.proposition_id", query: `SELECT e.id,e.proposition_id,e.title,snippet(evidence_fts,-1,` + snippetArgs + `) FROM evidence_fts JOIN evidence e ON e.id=evidence_fts.rowid WHERE evidence_fts MATCH ? {member} ORDER BY evidence_fts.rank LIMIT ?`},
	{kind: KindComment, prop: "c.proposition_id", query: `
		SELECT m.id, c.proposition_id, c.title, snippet(comments_fts, -1, ` + snippetArgs + `)
		FROM comments_fts JOIN comments m ON m.id = comments_fts.rowid
		JOIN cards c ON c.id = m.card_id
		WHERE comments_fts MATCH ? {member} ORDER BY comments_fts.rank LIMIT ?`},
	{kind: KindUser, like: true, query: `
		SELECT id, 0, name, handle FROM users
		WHERE name LIKE ? ESCAPE '\' ORDER BY name LIMIT ?`},
}

// Search runs every kind for one query and returns the groups that matched. A
// query with nothing searchable left in it, once the FTS syntax is stripped
// out, returns no groups rather than an error.
func Search(ctx context.Context, q store.Querier, query string, limit int, reader Reader) ([]Group, error) {
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
		sql, args := s.query, []any{arg}
		if s.prop == "" || reader.All {
			sql = strings.Replace(sql, "{member}", "", 1)
		} else {
			sql = strings.Replace(sql, "{member}", "AND "+s.prop+" "+memberOf, 1)
			args = append(args, reader.UserID)
		}
		hits, err := run(ctx, q, s.kind, sql, append(args, limit)...)
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 {
			groups = append(groups, Group{Kind: s.kind, Hits: hits})
		}
	}
	return groups, nil
}

func run(ctx context.Context, q store.Querier, kind, query string, args ...any) ([]Hit, error) {
	rows, err := q.QueryContext(ctx, query, args...)
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
		if h.PropositionID > 0 {
			h.URL = fmt.Sprintf("/p/%d", h.PropositionID)
			if kind != KindProposition {
				anchor := kind
				if kind == "transcript" {
					anchor = "file"
				}
				h.URL += fmt.Sprintf("#%s-%d", anchor, h.ID)
			}
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
