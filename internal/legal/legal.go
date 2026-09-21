// Package legal stores releases and the immutable agreements participants sign.
package legal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	outbox "github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/store"
)

var ErrInvalid = errors.New("check the release fields and each person's name, date, age and consent")
var ErrChanged = errors.New("this release changed; reload and read the current agreement before signing or saving")
var ErrClosed = errors.New("this release is closed")

const Consent = "I am at least 18 years old. I have read and agree to this release, and intend my typed name to be my electronic signature."
const EmailSubject = "{title} is now available"
const EmailBody = "Hello {name},\n\nThank you for taking part in {brand}. The episode is now available:\n\n{url}\n\nThank you,\n{brand}"

func Draft(kind string) string {
	intro := "I voluntarily agree to take part in a street recording for the production described above."
	if kind == "interview" {
		intro = "I voluntarily agree to be interviewed for the production described above."
	}
	if kind == "custom" {
		return ""
	}
	return intro + `

I authorize {rights_holder} and its production team to record my voice, image, likeness, name and contributions in audio, video and photographs in connection with this production.

I grant {rights_holder} a worldwide, perpetual, royalty-free permission to reproduce, edit, translate, caption, distribute, publicly perform and display those recordings and my recorded contributions, in whole or in part, in any media, including the production, excerpts, archives and promotion of the production. The producer may authorize its distributors and service providers to do the same. I retain any rights in my pre-existing material; this permission covers its inclusion in my recorded contribution.

I understand the producer controls editorial decisions, need not use my contribution, and does not owe me payment, royalties, approval of edits or a copy before publication unless we separately agree in writing. This release does not authorize an unrelated product endorsement or creation of a synthetic voice or digital replica of me.

To the extent allowed by applicable law, I release {rights_holder} and those acting with its permission from claims based on the recording and uses I authorize here, including privacy and publicity claims. This does not release fraud, intentional misconduct or uses outside this permission, or waive rights that cannot lawfully be waived.

I confirm that I am at least 18 years old, am signing voluntarily, and have had the opportunity to ask questions. I agree to use an electronic signature and may save or print a copy of this agreement. Optional contact information and episode notifications are separate from this permission.`
}

type Release struct {
	ID           int64  `json:"id"`
	Proposition  int64  `json:"proposition_id"`
	Token        string `json:"token"`
	Version      int64  `json:"version"`
	State        string `json:"state"`
	Title        string `json:"title"`
	Kind         string `json:"kind"`
	Brand        string `json:"brand"`
	RightsHolder string `json:"rights_holder"`
	Details      string `json:"details"`
	Body         string `json:"body"`
	Closed       bool   `json:"closed"`
	EmailSubject string `json:"email_subject"`
	EmailBody    string `json:"email_body"`
	Created      int64  `json:"created_at"`
}
type Person struct {
	Name    string `json:"name"`
	Date    string `json:"date"`
	Email   string `json:"email,omitempty"`
	Notify  bool   `json:"notify"`
	Consent bool   `json:"consent"`
}
type Agreement struct {
	GoverningLaw string `json:"governing_law"`
	State        string `json:"state"`
	Title        string `json:"title"`
	Brand        string `json:"brand"`
	RightsHolder string `json:"rights_holder"`
	Details      string `json:"details"`
	Body         string `json:"body"`
	Version      int64  `json:"version"`
	Consent      string `json:"consent"`
}

func (r Release) Agreement() Agreement {
	return Agreement{GoverningLaw: GoverningLaw(r.State), State: r.State, Title: r.Title, Brand: r.Brand, RightsHolder: r.RightsHolder, Details: r.Details, Body: strings.ReplaceAll(r.Body, "{rights_holder}", r.RightsHolder), Version: r.Version, Consent: Consent}
}

type Submission struct {
	ID        int64     `json:"id"`
	Release   int64     `json:"release_id"`
	Receipt   string    `json:"receipt"`
	Agreement Agreement `json:"agreement"`
	People    []Person  `json:"people"`
	SignedAt  int64     `json:"signed_at"`
}
type Service struct{ *core.Service }

func New(c *core.Service) *Service { return &Service{c} }
func Token() string                { b := make([]byte, 16); rand.Read(b); return hex.EncodeToString(b) }

const columns = `id,proposition_id,token,version,state,title,kind,brand,rights_holder,details,body,closed,email_subject,email_body,created_at`

func scan(row interface{ Scan(...any) error }) (r Release, err error) {
	err = row.Scan(&r.ID, &r.Proposition, &r.Token, &r.Version, &r.State, &r.Title, &r.Kind, &r.Brand, &r.RightsHolder, &r.Details, &r.Body, &r.Closed, &r.EmailSubject, &r.EmailBody, &r.Created)
	if errors.Is(err, sql.ErrNoRows) {
		err = core.ErrNotFound
	}
	return
}
func get(ctx context.Context, q store.Querier, id int64) (Release, error) {
	return scan(q.QueryRowContext(ctx, "SELECT "+columns+" FROM legal_releases WHERE id=?", id))
}
func (s *Service) Public(ctx context.Context, token string) (Release, error) {
	return scan(s.DB.QueryRowContext(ctx, "SELECT "+columns+" FROM legal_releases WHERE token=?", token))
}
func access(ctx context.Context, q store.Querier, a core.Actor, prop int64) error {
	if a.Kind != core.KindUser {
		return core.ErrForbidden
	}
	u, err := store.UserByID(ctx, q, a.ID)
	if err != nil {
		return err
	}
	if !auth.Can(u.Role, auth.CanEdit) {
		return core.ErrForbidden
	}
	ok, err := board.Readable(ctx, q, u, prop)
	if err != nil {
		return err
	}
	if !ok {
		return core.ErrNotFound
	}
	return nil
}
func (s *Service) Get(ctx context.Context, a core.Actor, id int64) (Release, error) {
	r, err := get(ctx, s.DB, id)
	if err == nil {
		err = access(ctx, s.DB, a, r.Proposition)
	}
	return r, err
}
func (s *Service) List(ctx context.Context, a core.Actor, prop int64) ([]Release, error) {
	if err := access(ctx, s.DB, a, prop); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT "+columns+" FROM legal_releases WHERE proposition_id=? ORDER BY id DESC", prop)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Release{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func validText(v string, max int, required bool) bool {
	return len(v) <= max && (!required || strings.TrimSpace(v) != "") && !strings.ContainsRune(v, 0)
}
func (s *Service) Save(ctx context.Context, a core.Actor, r Release) (core.Event, error) {
	r.Title = strings.TrimSpace(r.Title)
	r.Brand = strings.TrimSpace(r.Brand)
	r.RightsHolder = strings.TrimSpace(r.RightsHolder)
	if r.State != "NY" && r.State != "GA" {
		return core.Event{}, ErrInvalid
	}
	if r.Kind != "street" && r.Kind != "interview" && r.Kind != "custom" {
		return core.Event{}, ErrInvalid
	}
	if r.Body == "" {
		r.Body = Draft(r.Kind)
	}
	if !validText(r.Title, 200, true) || !validText(r.Brand, 120, true) || !validText(r.RightsHolder, 200, true) || !validText(r.Details, 4000, false) || !validText(r.Body, 24000, true) || !validText(r.EmailSubject, 200, true) || strings.ContainsAny(r.EmailSubject, "\r\n") || !validText(r.EmailBody, 12000, true) {
		return core.Event{}, ErrInvalid
	}
	prop := r.Proposition
	if r.ID != 0 {
		old, err := s.Get(ctx, a, r.ID)
		if err != nil {
			return core.Event{}, err
		}
		prop = old.Proposition
	}
	return s.Do(ctx, a, prop, auth.CanEdit, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		if err := s.Allow(ctx, tx, prop, "legal_release", "edit"); err != nil {
			return core.Change{}, err
		}
		if err := access(ctx, tx, a, r.Proposition); err != nil {
			return core.Change{}, err
		}
		if err := s.Allow(ctx, tx, r.Proposition, "legal_release", "edit"); err != nil {
			return core.Change{}, err
		}
		var before any
		action := "create"
		if r.ID == 0 {
			r.Token = Token()
			r.Version = 1
			r.Created = s.Now().Unix()
			res, err := tx.ExecContext(ctx, `INSERT INTO legal_releases(proposition_id,token,state,title,kind,brand,rights_holder,details,body,closed,email_subject,email_body,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.Proposition, r.Token, r.State, r.Title, r.Kind, r.Brand, r.RightsHolder, r.Details, r.Body, r.Closed, r.EmailSubject, r.EmailBody, r.Created)
			if err != nil {
				return core.Change{}, err
			}
			r.ID, err = res.LastInsertId()
			if err != nil {
				return core.Change{}, err
			}
		} else {
			old, err := get(ctx, tx, r.ID)
			if err != nil {
				return core.Change{}, err
			}
			if old.Proposition != prop || old.Version != r.Version {
				return core.Change{}, ErrChanged
			}
			// Moving a signed agreement would expose participants to a different team.
			if old.Proposition != r.Proposition {
				var n int
				if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM legal_submissions WHERE release_id=?", r.ID).Scan(&n); err != nil {
					return core.Change{}, err
				}
				if n > 0 {
					return core.Change{}, fmt.Errorf("%w: signed releases cannot change assignment", ErrInvalid)
				}
			}
			before = map[string]any{"id": old.ID, "version": old.Version, "closed": old.Closed}
			action = "edit"
			r.Token = old.Token
			r.Created = old.Created
			r.Version++
			_, err = tx.ExecContext(ctx, `UPDATE legal_releases SET proposition_id=?,version=?,state=?,title=?,kind=?,brand=?,rights_holder=?,details=?,body=?,closed=?,email_subject=?,email_body=? WHERE id=?`, r.Proposition, r.Version, r.State, r.Title, r.Kind, r.Brand, r.RightsHolder, r.Details, r.Body, r.Closed, r.EmailSubject, r.EmailBody, r.ID)
			if err != nil {
				return core.Change{}, err
			}
		}
		return core.Change{Entity: "legal_release", EntityID: r.ID, Action: action, Before: before, After: map[string]any{"id": r.ID, "version": r.Version, "closed": r.Closed}}, nil
	})
}
func (s *Service) Sign(ctx context.Context, token, receipt string, version int64, people []Person, agreementDigest string) (Submission, error) {
	if len(agreementDigest) != 64 || len(receipt) != 32 || len(people) == 0 || len(people) > 20 {
		return Submission{}, ErrInvalid
	}
	for _, ch := range receipt {
		if !(ch >= 'a' && ch <= 'f' || ch >= '0' && ch <= '9') {
			return Submission{}, ErrInvalid
		}
	}
	for i := range people {
		p := &people[i]
		p.Name = strings.TrimSpace(p.Name)
		p.Email = strings.TrimSpace(p.Email)
		_, err := time.Parse("2006-01-02", p.Date)
		if !validText(p.Name, 200, true) || strings.ContainsAny(p.Name, "\r\n") || !p.Consent || err != nil || len(p.Email) > 254 || p.Notify && p.Email == "" {
			return Submission{}, ErrInvalid
		}
		if p.Email != "" {
			addr, err := mail.ParseAddress(p.Email)
			if err != nil || addr.Address != p.Email {
				return Submission{}, ErrInvalid
			}
		}
	}
	raw, _ := json.Marshal(struct {
		Version         int64
		People          []Person
		AgreementDigest string
	}{version, people, agreementDigest})
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	r, err := s.Public(ctx, token)
	if err != nil {
		return Submission{}, err
	}
	var out Submission
	_, err = s.Do(ctx, core.Actor{Kind: core.KindPublic, Name: "Public participant", Via: "release"}, r.Proposition, core.SignRelease, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		current, err := get(ctx, tx, r.ID)
		if err != nil {
			return core.Change{}, err
		}
		if current.Proposition != r.Proposition || current.Token != token {
			return core.Change{}, ErrChanged
		}
		var priorHash string
		err = tx.QueryRowContext(ctx, "SELECT request_hash FROM legal_submissions WHERE release_id=? AND receipt=?", r.ID, receipt).Scan(&priorHash)
		if err == nil {
			if priorHash != hash {
				return core.Change{}, ErrChanged
			}
			return core.Change{}, errDuplicate
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return core.Change{}, err
		}
		for _, p := range people {
			date, _ := time.Parse("2006-01-02", p.Date)
			if date.Before(s.Now().AddDate(0, 0, -2)) || date.After(s.Now().AddDate(0, 0, 1)) {
				return core.Change{}, ErrInvalid
			}
		}
		if current.Closed {
			return core.Change{}, ErrClosed
		}
		if current.Version != version || current.Agreement().Digest() != agreementDigest {
			return core.Change{}, ErrChanged
		}
		if err = s.Allow(ctx, tx, r.Proposition, "legal_submission", "sign"); err != nil {
			return core.Change{}, err
		}
		out = Submission{Release: r.ID, Receipt: receipt, Agreement: current.Agreement(), People: people, SignedAt: s.Now().Unix()}
		snapshot, _ := json.Marshal(out.Agreement)
		participants, _ := json.Marshal(people)
		res, err := tx.ExecContext(ctx, "INSERT INTO legal_submissions(release_id,receipt,request_hash,snapshot,people,signed_at) VALUES(?,?,?,?,?,?)", r.ID, receipt, hash, string(snapshot), string(participants), out.SignedAt)
		if err != nil {
			return core.Change{}, err
		}
		out.ID, err = res.LastInsertId()
		return core.Change{Entity: "legal_submission", EntityID: out.ID, Action: "sign", After: map[string]any{"release_id": r.ID, "participants": len(people)}}, err
	})
	if errors.Is(err, errDuplicate) {
		return s.Receipt(ctx, token, receipt)
	}
	return out, err
}

var errDuplicate = errors.New("already signed")

func scanSubmission(row interface{ Scan(...any) error }) (out Submission, err error) {
	var snapshot, people string
	err = row.Scan(&out.ID, &out.Release, &out.Receipt, &snapshot, &people, &out.SignedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = core.ErrNotFound
	}
	if err != nil {
		return
	}
	if err = json.Unmarshal([]byte(snapshot), &out.Agreement); err != nil {
		return
	}
	err = json.Unmarshal([]byte(people), &out.People)
	return
}

const submissionColumns = `id,release_id,receipt,snapshot,people,signed_at`

func (s *Service) Receipt(ctx context.Context, token, receipt string) (Submission, error) {
	return scanSubmission(s.DB.QueryRowContext(ctx, "SELECT "+submissionColumns+" FROM legal_submissions WHERE receipt=? AND release_id=(SELECT id FROM legal_releases WHERE token=?)", receipt, token))
}
func (s *Service) Submissions(ctx context.Context, a core.Actor, id int64) ([]Submission, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	release, err := get(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err = access(ctx, tx, a, release.Proposition); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+submissionColumns+" FROM legal_submissions WHERE release_id=? ORDER BY id DESC", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Submission{}
	for rows.Next() {
		row, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

type Message struct {
	Email   string `json:"email"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func (s *Service) Preview(ctx context.Context, a core.Actor, id int64, episodeURL, subject, body string) ([]Message, error) {
	r, err := s.Get(ctx, a, id)
	if err != nil {
		return nil, err
	}
	subs, err := s.Submissions(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if !validText(subject, 200, true) || strings.ContainsAny(subject, "\r\n") || !validText(body, 12000, true) {
		return nil, ErrInvalid
	}
	u, err := url.Parse(episodeURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || len(episodeURL) > 2000 {
		return nil, ErrInvalid
	}
	sent := map[string]bool{}
	rows, err := s.DB.QueryContext(ctx, "SELECT email FROM legal_notifications WHERE release_id=?", id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var email string
		if err = rows.Scan(&email); err != nil {
			rows.Close()
			return nil, err
		}
		sent[email] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []Message{}
	for _, sub := range subs {
		for _, p := range sub.People {
			key := strings.ToLower(p.Email)
			if !p.Notify || key == "" || sent[key] {
				continue
			}
			sent[key] = true
			repl := strings.NewReplacer("{name}", p.Name, "{brand}", r.Brand, "{title}", r.Title, "{url}", episodeURL)
			out = append(out, Message{p.Email, repl.Replace(subject), repl.Replace(body)})
		}
	}
	return out, nil
}
func (s *Service) Notify(ctx context.Context, a core.Actor, id, version int64, episodeURL, subject, body string, expected ...string) (core.Event, error) {
	r, err := s.Get(ctx, a, id)
	if err != nil {
		return core.Event{}, err
	}
	messages, err := s.Preview(ctx, a, id, episodeURL, subject, body)
	if err != nil {
		return core.Event{}, err
	}
	return s.Do(ctx, a, r.Proposition, auth.CanEdit, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		if len(expected) > 0 && expected[0] != MessageDigest(messages) {
			return core.Change{}, ErrChanged
		}
		current, err := get(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		if current.Version != version || current.Proposition != r.Proposition {
			return core.Change{}, ErrChanged
		}
		count := 0
		for _, m := range messages {
			res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO legal_notifications(release_id,email,subject,body,queued_at) VALUES(?,?,?,?,?)", id, strings.ToLower(m.Email), m.Subject, m.Body, s.Now().Unix())
			if err != nil {
				return core.Change{}, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return core.Change{}, err
			}
			if n == 0 {
				continue
			}
			if err = outbox.Enqueue(ctx, tx, outbox.Message{To: []string{m.Email}, Subject: m.Subject, Text: m.Body}, time.Time{}, ""); err != nil {
				return core.Change{}, err
			}
			count++
		}
		return core.Change{Entity: "legal_release", EntityID: id, Action: "notify", After: map[string]any{"id": id, "queued": count}}, nil
	})
}

func GoverningLaw(state string) string {
	name := "New York"
	if state == "GA" {
		name = "Georgia"
	}
	return "This release is governed by the laws of the State of " + name + ", except where applicable law requires otherwise. This choice does not waive protections that cannot legally be waived."
}

func MessageDigest(messages []Message) string {
	raw, _ := json.Marshal(messages)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (a Agreement) Digest() string {
	raw, _ := json.Marshal(a)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
