package files

import (
	"context"
	"database/sql"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/links"
)

// fetchTimeout bounds the metadata fetch. safehttp's client has a timeout of
// its own; this one is what the person who pasted the URL waits for, and it
// runs outside the transaction so a slow page holds nothing but its own
// request.
const fetchTimeout = 20 * time.Second

// AddLink saves a pasted URL with whatever the page says about itself. The
// fetch happens before the transaction opens, because it takes seconds and the
// database takes its write lock immediately; a page that refuses, times out or
// is not a page at all still saves the link with its address.
func (s *Service) AddLink(ctx context.Context, a core.Actor, proposition int64, raw string) (core.Event, error) {
	return s.AddLinkWithNote(ctx, a, proposition, raw, "", "")
}

// AddLinkWithNote saves annotations in the same command as the fetched link.
func (s *Service) AddLinkWithNote(ctx context.Context, a core.Actor, proposition int64, raw, note, questionText string) (core.Event, error) {
	address, err := webURL(raw)
	if err != nil {
		return core.Event{}, err
	}
	note, err = board.Field(note, board.MaxBody)
	if err != nil {
		return core.Event{}, err
	}
	questionValue, err := question(questionText)
	if err != nil {
		return core.Event{}, err
	}
	if err := s.mayWrite(ctx, a, proposition); err != nil {
		return core.Event{}, err
	}
	meta, fetched := s.metadata(ctx, address)

	return s.do(ctx, a, proposition, auth.CanEdit, "link", "create", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		res, err := tx.ExecContext(ctx, `INSERT INTO links
			(proposition_id, url, canonical_url, title, author, year, kind, note_md,
			 question, added_by, created_at, fetched_at, text_for_search)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			proposition, address, meta.CanonicalURL, meta.Title, meta.Author,
			year(meta), meta.Kind, note, questionValue, by(a), s.now(), fetched, meta.Text)
		if err != nil {
			return core.Change{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return core.Change{}, err
		}
		row, err := GetLink(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		return core.Change{Entity: "link", EntityID: id, Action: "create", After: row}, nil
	})
}

// ReadLink is one link, refusing a proposition the reader may not see the same
// way the list does.
func (s *Service) ReadLink(ctx context.Context, a core.Actor, id int64) (Link, error) {
	l, err := GetLink(ctx, s.DB, id)
	if err != nil {
		return Link{}, err
	}
	return l, visible(ctx, s.DB, a, l.Proposition)
}

// Edit is the drawer's fields. Every one of them is what a person corrected,
// including the kind the guess got wrong.
type Edit struct {
	Title    string
	Author   string
	Year     string
	Kind     string
	Note     string
	Question string
}

// EditLink writes the drawer's fields. There is no version on a link: the
// fields are short, one person edits one at a time, and the last write wins as
// it does for a card's due date.
func (s *Service) EditLink(ctx context.Context, a core.Actor, id int64, in Edit) (core.Event, error) {
	return s.PatchLink(ctx, a, id, LinkPatch{&in.Title, &in.Author, &in.Year, &in.Kind, &in.Note, &in.Question})
}

type LinkPatch struct {
	Title    *string `json:"title"`
	Author   *string `json:"author"`
	Year     *string `json:"year"`
	Kind     *string `json:"kind"`
	Note     *string `json:"note_md"`
	Question *string `json:"question"`
}

// PatchLink writes only supplied fields inside the command transaction.
func (s *Service) PatchLink(ctx context.Context, a core.Actor, id int64, in LinkPatch) (core.Event, error) {
	assignments := []string{"id = id"}
	var values []any
	for _, field := range []struct {
		name  string
		value *string
		limit int
	}{
		{"title", in.Title, board.MaxLine}, {"author", in.Author, board.MaxLine},
		{"year", in.Year, board.MaxWord}, {"kind", in.Kind, board.MaxWord},
		{"note_md", in.Note, board.MaxBody}, {"question", in.Question, board.MaxWord},
	} {
		if field.value == nil {
			continue
		}
		text, err := board.Field(*field.value, field.limit)
		if err != nil {
			return core.Event{}, err
		}
		var value any = text
		if field.name == "kind" && text != "" && !known(text, Kinds) {
			return core.Event{}, ErrKind
		}
		if field.name == "question" {
			value, err = question(text)
			if err != nil {
				return core.Event{}, err
			}
		}
		assignments = append(assignments, field.name+" = ?")
		values = append(values, value)
	}
	return s.link(ctx, a, id, auth.CanEdit, "update", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE links SET `+strings.Join(assignments, ", ")+` WHERE id = ?`, append(values, id)...)
		return err
	})
}

// RefetchLink asks the page again, for a link saved while the site was down or
// edited since. What a person typed into the fields is replaced, which is what
// asking for a refetch means; the note and the question are theirs and stay.
func (s *Service) RefetchLink(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	was, err := GetLink(ctx, s.DB, id)
	if err != nil {
		return core.Event{}, err
	}
	// The stored URL is validated again rather than trusted: it has been in the
	// database since it was pasted, and this is a fetch of whatever it says.
	address, err := webURL(was.URL)
	if err != nil {
		return core.Event{}, err
	}
	if err := s.mayWrite(ctx, a, was.Proposition); err != nil {
		return core.Event{}, err
	}
	meta, fetched := s.metadata(ctx, address)
	return s.link(ctx, a, id, auth.CanEdit, "update", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE links SET canonical_url = ?, title = ?,
			author = ?, year = ?, kind = ?, fetched_at = ?, text_for_search = ? WHERE id = ?`,
			meta.CanonicalURL, meta.Title, meta.Author, year(meta), meta.Kind,
			fetched, meta.Text, id)
		return err
	})
}

func (s *Service) DeleteLink(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	return s.link(ctx, a, id, auth.CanDelete, "delete", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, id)
		return err
	})
}

// link is the shape every link command has: the proposition read outside the
// transaction, the row read before and after inside it.
func (s *Service) link(ctx context.Context, a core.Actor, id int64, need, action string,
	apply func(context.Context, *sql.Tx) error) (core.Event, error) {
	proposition, err := s.propositionOf(ctx, linkScope, id)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, need, "link", action, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		was, err := GetLink(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		if err := apply(ctx, tx); err != nil {
			return core.Change{}, err
		}
		change := core.Change{Entity: "link", EntityID: id, Action: action, Before: was}
		if action == "delete" {
			return change, nil
		}
		if change.After, err = GetLink(ctx, tx, id); err != nil {
			return core.Change{}, err
		}
		return change, nil
	})
}

// metadata fetches the page and returns what it said and when, or the empty
// meta and a null fetched_at when it said nothing. A link nobody can reach is
// still worth keeping: the address is the part a person pasted.
func (s *Service) metadata(ctx context.Context, address string) (links.Meta, any) {
	if s.HTTP == nil {
		return links.Meta{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	meta, err := links.Fetch(ctx, s.HTTP, address)
	if err != nil {
		return links.Meta{}, nil
	}
	// A canonical URL is a string off a page somebody else controls, so it is
	// kept only when it is a web address; a javascript: or data: value in that
	// column would end up in an href.
	if _, err := webURL(meta.CanonicalURL); err != nil {
		meta.CanonicalURL = ""
	}
	meta.Title, meta.Author = clip(meta.Title, board.MaxLine), clip(meta.Author, board.MaxLine)
	return meta, s.now()
}

// clip cuts a value off a page to what the column takes, by runes, so a title
// is never cut through the middle of a character.
func clip(value string, most int) string {
	runes := []rune(value)
	if len(runes) <= most {
		return value
	}
	return string(runes[:most])
}

// mayWrite is the authorization asked before the fetch. core asks it again
// inside the transaction, where it is authoritative; this one is here so that
// somebody who may not write to this proposition cannot use the paste field as
// a way to make the server fetch a URL of their choosing.
func (s *Service) mayWrite(ctx context.Context, a core.Actor, proposition int64) error {
	if err := visible(ctx, s.DB, a, proposition); err != nil {
		return err
	}
	if a.Kind == core.KindUser {
		var role string
		if err := s.DB.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, a.ID).Scan(&role); err != nil {
			return err
		}
		if !auth.Can(role, auth.CanEdit) {
			return core.ErrForbidden
		}
	}
	var archived sql.NullInt64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT archived_at FROM propositions WHERE id = ?`, proposition).Scan(&archived); err != nil {
		return err
	}
	if archived.Valid {
		return board.ErrArchived
	}
	return nil
}

// webURL is the one gate a pasted address passes: http or https with a host,
// and nothing longer than a URL a person actually has. Where it may point is
// safehttp's business, checked again on every hop of the fetch.
func webURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > board.MaxBody {
		return "", ErrURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", ErrURL
	}
	return u.String(), nil
}

func year(m links.Meta) string {
	if m.Published.IsZero() {
		return ""
	}
	return strconv.Itoa(m.Published.Year())
}

// Citation is the line the drawer copies, built from the fields as they stand
// rather than from the page, so that a corrected author appears in it.
func Citation(l Link) string {
	m := links.Meta{Title: l.Title, Author: l.Author}
	if n, err := strconv.Atoi(l.Year); err == nil && n > 0 {
		m.Published = time.Date(n, time.January, 1, 0, 0, 0, 0, time.UTC)
	}
	return links.Citation(m, l.URL)
}
