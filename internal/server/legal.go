package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/legal"
	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/settings"
)

func legalActor(r *http.Request) core.Actor {
	u := userOf(r)
	return core.Actor{Kind: core.KindUser, ID: u.ID, Name: u.Name}
}
func legalNumber(v string) int64 { n, _ := strconv.ParseInt(v, 10, 64); return n }
func (s *Server) legalError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		s.errorPage(w, r, http.StatusNotFound)
	case errors.Is(err, core.ErrForbidden):
		s.errorPage(w, r, http.StatusForbidden)
	default:
		s.fail(w, r, err)
	}
}
func (s *Server) legalRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /legal", s.requireUser(s.legalManage))
	m.HandleFunc("GET /releases/{id}", s.requireUser(s.legalManage))
	m.HandleFunc("POST /legal", s.requireUser(s.legalSave))
	m.HandleFunc("POST /releases/{id}", s.requireUser(s.legalSave))
	m.HandleFunc("POST /releases/{id}/notify", s.requireUser(s.legalNotify))
	m.HandleFunc("GET /releases/{id}/export", s.requireUser(s.legalExport))
	m.HandleFunc("GET /legal/{token}", s.legalPublic)
	m.HandleFunc("POST /legal/{token}", s.legalSign)
	m.HandleFunc("GET /legal/{token}/receipt/{receipt}", s.legalReceipt)
	m.HandleFunc("GET /legal/{token}/qr", s.legalQRPage)
	m.HandleFunc("GET /legal/{token}/qr.svg", s.legalQR)
}
func (s *Server) legalManage(w http.ResponseWriter, r *http.Request) {
	if !auth.Can(userOf(r).Role, auth.CanEdit) {
		s.errorPage(w, r, http.StatusForbidden)
		return
	}
	var release legal.Release
	var err error
	if id := legalNumber(r.PathValue("id")); id > 0 {
		release, err = s.api.Legal.Get(r.Context(), legalActor(r), id)
	} else {
		prop := legalNumber(r.URL.Query().Get("proposition"))
		if prop == 0 {
			p, e := board.GetShow(r.Context(), s.db)
			err = e
			prop = p.ID
		}
		release = legal.Release{Proposition: prop, Kind: "street", State: "NY", Title: "Recording release", Brand: settings.Get[string](s.settings, "workspace.name"), RightsHolder: settings.Get[string](s.settings, "workspace.legal_rights_holder"), Body: legal.Draft("street"), EmailSubject: settings.Get[string](s.settings, "workspace.legal_email_subject"), EmailBody: settings.Get[string](s.settings, "workspace.legal_email_body")}
	}
	if err != nil {
		s.legalError(w, r, err)
		return
	}
	s.renderLegalManage(w, r, http.StatusOK, release, nil)
}
func (s *Server) renderLegalManage(w http.ResponseWriter, r *http.Request, status int, release legal.Release, extra map[string]any) {
	state, err := s.shellState(r, release.Proposition)
	if err != nil {
		s.legalError(w, r, err)
		return
	}
	links, err := s.api.Legal.List(r.Context(), legalActor(r), release.Proposition)
	if err != nil {
		s.legalError(w, r, err)
		return
	}
	subs := []legal.Submission{}
	if release.ID > 0 {
		subs, err = s.api.Legal.Submissions(r.Context(), legalActor(r), release.ID)
		if err != nil {
			s.legalError(w, r, err)
			return
		}
	}
	data := s.page(r, "Recording releases", map[string]any{"Manage": true, "Release": release, "Links": links, "Submissions": subs, "Propositions": state.Propositions, "URL": s.cfg.BaseURL + "/legal/" + release.Token, "Street": legal.Draft("street"), "Interview": legal.Draft("interview"), "EmailSubject": release.EmailSubject, "EmailBody": release.EmailBody})
	for k, v := range extra {
		data[k] = v
	}
	s.render(w, r, status, "legal_manage.html", data)
}
func (s *Server) legalSave(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	release := legal.Release{ID: legalNumber(r.PathValue("id")), Proposition: legalNumber(f.Get("proposition")), Version: legalNumber(f.Get("version")), Title: f.Get("title"), Kind: f.Get("kind"), State: f.Get("state"), Brand: f.Get("brand"), RightsHolder: f.Get("rights_holder"), Details: f.Get("details"), Body: f.Get("body"), Closed: f.Get("closed") == "on", EmailSubject: f.Get("email_subject"), EmailBody: f.Get("email_body")}
	ctx := r.Context()
	var err error
	key := f.Get("key")
	if key != "" {
		ctx, err = core.WithKey(ctx, key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
	}
	event, err := s.api.Legal.Save(ctx, legalActor(r), release)
	if err != nil {
		if errors.Is(err, legal.ErrInvalid) || errors.Is(err, legal.ErrChanged) || errors.Is(err, board.ErrArchived) {
			if release.ID > 0 {
				old, e := s.api.Legal.Get(r.Context(), legalActor(r), release.ID)
				if e != nil {
					s.legalError(w, r, e)
					return
				}
				release.Token = old.Token
			}
			s.renderLegalManage(w, r, http.StatusUnprocessableEntity, release, map[string]any{"Error": err.Error()})
			return
		}
		s.legalError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", event.EntityID), http.StatusSeeOther)
}
func (s *Server) legalPublic(w http.ResponseWriter, r *http.Request) {
	release, err := s.api.Legal.Public(r.Context(), r.PathValue("token"))
	if err != nil {
		s.legalError(w, r, err)
		return
	}
	s.renderLegalPublic(w, r, http.StatusOK, release, []legal.Person{{Date: time.Now().Format("2006-01-02")}}, legal.Token(), "")
}
func (s *Server) renderLegalPublic(w http.ResponseWriter, r *http.Request, status int, release legal.Release, people []legal.Person, receipt, message string) {
	if len(people) == 0 {
		people = []legal.Person{{Date: time.Now().Format("2006-01-02")}}
	}
	s.render(w, r, status, "legal_public.html", s.page(r, release.Title, map[string]any{"Release": release, "People": people, "ReceiptKey": receipt, "Consent": legal.Consent, "Error": message}))
}
func (s *Server) legalSign(w http.ResponseWriter, r *http.Request) {
	// Spent before any read, so a guessed token costs the same as a real one.
	limited := !s.auth.Allow("legal", s.auth.ClientIP(r))
	release, err := s.api.Legal.Public(r.Context(), r.PathValue("token"))
	if err != nil {
		s.legalError(w, r, err)
		return
	}
	people := []legal.Person{}
	for _, index := range r.PostForm["person"] {
		if len(people) >= 20 {
			break
		}
		prefix := "person_" + index + "_"
		people = append(people, legal.Person{Name: r.PostForm.Get(prefix + "name"), Date: r.PostForm.Get(prefix + "date"), Email: r.PostForm.Get(prefix + "email"), Consent: r.PostForm.Get(prefix+"consent") == "on", Notify: r.PostForm.Get(prefix+"notify") == "on"})
	}
	receipt := r.PostForm.Get("receipt")
	// The form comes back filled in with the same receipt, so waiting and
	// sending it again cannot sign twice.
	if limited {
		w.Header().Set("Retry-After", "60")
		s.renderLegalPublic(w, r, http.StatusTooManyRequests, release, people, receipt, "Too many submissions. Please wait a minute and try again.")
		return
	}
	if len(r.PostForm["person"]) > 20 {
		err = legal.ErrInvalid
	} else {
		_, err = s.api.Legal.Sign(r.Context(), release.Token, receipt, legalNumber(r.PostForm.Get("version")), people, r.PostForm.Get("agreement_digest"))
	}
	if err != nil {
		if errors.Is(err, legal.ErrInvalid) || errors.Is(err, legal.ErrChanged) || errors.Is(err, legal.ErrClosed) {
			for i := range people {
				people[i].Consent = false
			}
			// Nothing was saved, and the old receipt may be spent on different
			// details, so the corrected form signs under a new one.
			s.renderLegalPublic(w, r, http.StatusUnprocessableEntity, release, people, legal.Token(), err.Error())
			return
		}
		s.legalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/legal/"+release.Token+"/receipt/"+receipt, http.StatusSeeOther)
}
func (s *Server) legalReceipt(w http.ResponseWriter, r *http.Request) {
	sub, err := s.api.Legal.Receipt(r.Context(), r.PathValue("token"), r.PathValue("receipt"))
	if err != nil {
		s.legalError(w, r, err)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="signed-release.json"`)
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(sub)
		return
	}
	s.render(w, r, http.StatusOK, "legal_receipt.html", s.page(r, "Release signed", map[string]any{"Submission": sub, "SignedAt": time.Unix(sub.SignedAt, 0).UTC().Format(time.RFC3339), "Brand": sub.Agreement.Brand}))
}
func (s *Server) legalExport(w http.ResponseWriter, r *http.Request) {
	subs, err := s.api.Legal.Submissions(r.Context(), legalActor(r), legalNumber(r.PathValue("id")))
	if err != nil {
		s.legalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="signed-releases.json"`)
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(subs)
}
func (s *Server) legalNotify(w http.ResponseWriter, r *http.Request) {
	id := legalNumber(r.PathValue("id"))
	release, err := s.api.Legal.Get(r.Context(), legalActor(r), id)
	if err != nil {
		s.legalError(w, r, err)
		return
	}
	f := r.PostForm
	url, subject, body := f.Get("episode_url"), f.Get("email_subject"), f.Get("email_body")
	extra := map[string]any{"EpisodeURL": url, "EmailSubject": subject, "EmailBody": body}
	status := http.StatusOK
	if f.Get("send") == "yes" {
		ctx, keyErr := core.WithKey(r.Context(), f.Get("key"))
		if keyErr != nil {
			http.Error(w, keyErr.Error(), http.StatusUnprocessableEntity)
			return
		}
		_, err = s.api.Legal.Notify(ctx, legalActor(r), id, legalNumber(f.Get("version")), url, subject, body, f.Get("preview_hash"))
		if err == nil {
			extra["Notice"] = "Notifications queued. Each opted-in email address receives one episode notification per release."
		}
	} else {
		var messages []legal.Message
		messages, err = s.api.Legal.Preview(r.Context(), legalActor(r), id, url, subject, body)
		extra["Preview"] = messages
		extra["PreviewHash"] = legal.MessageDigest(messages)
		extra["Previewed"] = err == nil
	}
	if err != nil {
		if errors.Is(err, legal.ErrInvalid) || errors.Is(err, legal.ErrChanged) || errors.Is(err, legal.ErrRecipientsChanged) || errors.Is(err, mail.ErrNotConfigured) {
			status = http.StatusUnprocessableEntity
			extra["Error"] = err.Error()
		} else {
			s.legalError(w, r, err)
			return
		}
	}
	s.renderLegalManage(w, r, status, release, extra)
}
