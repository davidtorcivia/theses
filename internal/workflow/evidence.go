package workflow

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/files"
)

type Evidence struct {
	ID             int64  `json:"id"`
	Proposition    int64  `json:"proposition_id"`
	Link           *int64 `json:"link_id"`
	File           *int64 `json:"file_id"`
	Block          *int64 `json:"block_id"`
	Title          string `json:"title"`
	Author         string `json:"author"`
	Year           string `json:"year"`
	URL            string `json:"url"`
	Quotation      string `json:"quotation"`
	Locator        string `json:"locator"`
	Interpretation string `json:"interpretation"`
	Claim          string `json:"claim"`
	Verified       bool   `json:"verified"`
	VerifiedBy     *int64 `json:"verified_by"`
	Version        int64  `json:"version"`
}

const evidenceColumns = `id,proposition_id,link_id,file_id,block_id,title,author,year,url,quotation,locator,interpretation,claim,verified,verified_by,version`

func scanEvidence(row interface{ Scan(...any) error }) (e Evidence, err error) {
	err = row.Scan(&e.ID, &e.Proposition, &e.Link, &e.File, &e.Block, &e.Title, &e.Author, &e.Year, &e.URL, &e.Quotation, &e.Locator, &e.Interpretation, &e.Claim, &e.Verified, &e.VerifiedBy, &e.Version)
	return
}
func (s *Service) Evidence(ctx context.Context, a core.Actor, prop int64) ([]Evidence, error) {
	if err := s.Readable(ctx, a, prop); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+evidenceColumns+` FROM evidence WHERE proposition_id=? ORDER BY id`, prop)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Evidence{}
	for rows.Next() {
		e, err := scanEvidence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Service) SaveEvidence(ctx context.Context, a core.Actor, in Evidence) (core.Event, error) {
	return s.change(ctx, a, in.Proposition, "evidence", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		var before *Evidence
		if in.ID > 0 {
			row, err := scanEvidence(tx.QueryRowContext(ctx, `SELECT `+evidenceColumns+` FROM evidence WHERE id=? AND proposition_id=?`, in.ID, in.Proposition))
			if err != nil {
				return core.Change{}, core.ErrNotFound
			}
			before = &row
			if row.Version != in.Version {
				return core.Change{}, ErrChanged
			}
		}
		for _, field := range []*string{&in.Title, &in.Author, &in.Year, &in.URL, &in.Quotation, &in.Locator, &in.Interpretation, &in.Claim} {
			clean, err := board.Field(*field, board.MaxBody)
			if err != nil {
				return core.Change{}, err
			}
			*field = clean
		}
		if in.Title == "" {
			return core.Change{}, board.ErrEmpty
		}
		if in.URL != "" {
			u, err := url.Parse(in.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return core.Change{}, ErrInvalid
			}
		}
		if in.Link != nil {
			l, err := files.GetLink(ctx, tx, *in.Link)
			if err != nil || l.Proposition != in.Proposition {
				return core.Change{}, core.ErrNotFound
			}
		}
		if in.File != nil {
			f, err := files.GetFile(ctx, tx, *in.File)
			if err != nil || f.Proposition != in.Proposition {
				return core.Change{}, core.ErrNotFound
			}
		}
		if in.Block != nil {
			b, err := docs.GetBlock(ctx, tx, *in.Block)
			if err != nil || b.DeletedAt != nil {
				return core.Change{}, core.ErrNotFound
			}
			d, err := docs.GetDocument(ctx, tx, b.Document)
			if err != nil || d.Proposition != in.Proposition {
				return core.Change{}, core.ErrNotFound
			}
		}
		in.VerifiedBy = nil
		if in.Verified {
			user := a.ID
			in.VerifiedBy = &user
		}
		in.Version++
		if before == nil {
			in.Version = 1
		}
		args := []any{in.Proposition, in.Link, in.File, in.Block, in.Title, in.Author, in.Year, in.URL, in.Quotation, in.Locator, in.Interpretation, in.Claim, in.Verified, in.VerifiedBy, in.Version}
		if before == nil {
			r, err := tx.ExecContext(ctx, `INSERT INTO evidence(proposition_id,link_id,file_id,block_id,title,author,year,url,quotation,locator,interpretation,claim,verified,verified_by,version) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
			if err != nil {
				return core.Change{}, err
			}
			in.ID, err = r.LastInsertId()
			if err != nil {
				return core.Change{}, err
			}
		} else {
			args = append(args, in.ID)
			if _, err := tx.ExecContext(ctx, `UPDATE evidence SET proposition_id=?,link_id=?,file_id=?,block_id=?,title=?,author=?,year=?,url=?,quotation=?,locator=?,interpretation=?,claim=?,verified=?,verified_by=?,version=? WHERE id=?`, args...); err != nil {
				return core.Change{}, err
			}
		}
		return core.Change{Entity: "evidence", EntityID: in.ID, Action: "edit", Before: before, After: in}, nil
	})
}
func ExportEvidence(rows []Evidence, format string) (string, error) {
	var b strings.Builder
	line := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for _, e := range rows {
		switch format {
		case "ris":
			fmt.Fprintf(&b, "TY  - GEN\nID  - %d\nTI  - %s\n", e.ID, line(e.Title))
			for _, f := range [][2]string{{"AU", e.Author}, {"PY", e.Year}, {"UR", e.URL}, {"N1", e.Locator}, {"N1", e.Quotation}} {
				if f[1] != "" {
					fmt.Fprintf(&b, "%s  - %s\n", f[0], line(f[1]))
				}
			}
			b.WriteString("ER  - \n\n")
		case "md":
			status := "Needs checking"
			if e.Verified {
				status = "Verified"
			}
			fmt.Fprintf(&b, "## Reference %d: %s\n\n%s (%s). %s\n\nLocation: %s\nStatus: %s\n\nQuotation:\n> %s\n\nInterpretation: %s\n\nClaim: %s\n\n", e.ID, line(e.Title), line(e.Author), line(e.Year), line(e.URL), line(e.Locator), status, strings.ReplaceAll(e.Quotation, "\n", "\n> "), e.Interpretation, e.Claim)
		default:
			return "", ErrInvalid
		}
	}
	if format != "ris" && format != "md" {
		return "", ErrInvalid
	}
	return b.String(), nil
}

func (s *Service) DeleteEvidence(ctx context.Context, a core.Actor, id, version int64) (core.Event, error) {
	row, err := scanEvidence(s.DB.QueryRowContext(ctx, `SELECT `+evidenceColumns+` FROM evidence WHERE id=?`, id))
	if err != nil {
		return core.Event{}, core.ErrNotFound
	}
	return s.Do(ctx, a, row.Proposition, auth.CanDelete, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		if err := s.Allow(ctx, tx, row.Proposition, "evidence", "delete"); err != nil {
			return core.Change{}, err
		}
		row, err := scanEvidence(tx.QueryRowContext(ctx, `SELECT `+evidenceColumns+` FROM evidence WHERE id=?`, id))
		if err != nil {
			return core.Change{}, core.ErrNotFound
		}
		if row.Version != version {
			return core.Change{}, ErrChanged
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM evidence WHERE id=?`, id)
		return core.Change{Entity: "evidence", EntityID: id, Action: "delete", Before: row}, err
	})
}
