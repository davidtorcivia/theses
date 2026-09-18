package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
)

// driveFake is Drive as the import path uses it: a metadata read and a
// download of the same id. hits counts what reached it, so a test can show
// that a refusal never asked Drive anything.
type driveFake struct {
	*httptest.Server
	hits atomic.Int64
}

func driveWith(t *testing.T, contents map[string]string) *driveFake {
	t.Helper()
	fake := &driveFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-1", "refresh_token": "refresh-1", "expires_in": 3600})
	})
	mux.HandleFunc("GET /drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		fake.hits.Add(1)
		rows := make([]map[string]string, 0, len(contents))
		for id, body := range contents {
			rows = append(rows, map[string]string{
				"id": id, "name": id + ".wav", "mimeType": "audio/wav",
				"size": strconv.Itoa(len(body))})
		}
		json.NewEncoder(w).Encode(map[string]any{"files": rows})
	})
	mux.HandleFunc("GET /drive/v3/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		fake.hits.Add(1)
		body, ok := contents[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"message":"File not found."}}`)
			return
		}
		if r.URL.Query().Get("alt") == "media" {
			io.WriteString(w, body)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"id": r.PathValue("id"), "name": r.PathValue("id") + ".wav",
			"mimeType": "audio/wav", "size": strconv.Itoa(len(body))})
	})
	fake.Server = httptest.NewServer(mux)
	t.Cleanup(fake.Close)
	return fake
}

// connectedDrive is a harness with a bucket, a connected Drive and one
// proposition.
func connectedDrive(t *testing.T, contents map[string]string) (*harness, *driveFake, int64) {
	t.Helper()
	h := newHarness(t)
	h.setupOwner()
	h.configureBucket(fakeBucket(t, "theses"), "theses")
	drive := driveWith(t, contents)
	h.pointAtFakes(drive.URL, "")
	h.saveSecret("integrations.drive.client_id", "the-client")
	h.saveSecret("integrations.drive.client_secret", "the-secret")
	h.saveSecret("integrations.drive.token", `{"refresh_token":"refresh-1"}`)
	return h, drive, h.proposition("Tidal Power")
}

func TestDriveImportStreamsIntoTheBucket(t *testing.T) {
	h, _, proposition := connectedDrive(t, map[string]string{"f1": "twelve bytes"})
	token := h.csrf("/profile")

	res, body := h.send("POST", "/app/drive/import", token,
		`{"proposition":`+strconv.FormatInt(proposition, 10)+`,"file":"f1","folder":"Recordings"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the import gave %d: %s", res.StatusCode, body)
	}
	var answer struct {
		File files.File `json:"file"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.File.State != "ready" {
		t.Fatalf("the file is at %q", answer.File.State)
	}
	if answer.File.Name != "f1.wav" || answer.File.Size != 12 || answer.File.Folder != "Recordings" {
		t.Fatalf("the row is %+v", answer.File)
	}
	// It is attributed to the person who asked, not to the integration.
	var by int64
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT uploaded_by FROM files WHERE id = ?`, answer.File.ID).Scan(&by); err != nil {
		t.Fatal(err)
	}
	if by != 1 {
		t.Fatalf("the file is attributed to user %d", by)
	}

	// And the pane sees it in the ordinary listing.
	res, body = h.send("GET", "/app/files?proposition="+strconv.FormatInt(proposition, 10), "", "")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "f1.wav") {
		t.Fatalf("the files listing gave %d: %s", res.StatusCode, body)
	}
}

func TestDriveImportRefusals(t *testing.T) {
	h, _, proposition := connectedDrive(t, map[string]string{"f1": "twelve bytes"})
	token := h.csrf("/profile")
	at := strconv.FormatInt(proposition, 10)

	cases := []struct {
		name, body string
		want       int
	}{
		{"a file Drive does not have", `{"proposition":` + at + `,"file":"nope","folder":"Recordings"}`, http.StatusUnprocessableEntity},
		{"a folder that is not one of ours", `{"proposition":` + at + `,"file":"f1","folder":"Elsewhere"}`, http.StatusUnprocessableEntity},
		{"a proposition that is not there", `{"proposition":9999,"file":"f1","folder":"Recordings"}`, http.StatusNotFound},
		{"a body that is not JSON", `not json`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, body := h.send("POST", "/app/drive/import", token, c.body)
			if res.StatusCode != c.want {
				t.Fatalf("gave %d, want %d: %s", res.StatusCode, c.want, body)
			}
			if strings.Contains(body, "refresh-1") || strings.Contains(body, "the-secret") {
				t.Fatal("the refusal carries a secret")
			}
		})
	}

	// Nothing was left half made by any of them.
	var rows int
	if err := h.db.QueryRowContext(context.Background(), `SELECT count(*) FROM files`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("the refused imports left %d file rows", rows)
	}
}

func TestDriveImportNeedsASessionAndACSRFToken(t *testing.T) {
	h, _, proposition := connectedDrive(t, map[string]string{"f1": "twelve bytes"})
	at := strconv.FormatInt(proposition, 10)

	if res, _ := h.send("POST", "/app/drive/import", "", `{"proposition":`+at+`,"file":"f1"}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("an import with no CSRF token gave %d", res.StatusCode)
	}
	token := h.csrf("/profile")
	h.signOut()
	// The token was bound to the session that has just ended, so the CSRF
	// check refuses this before the session check gets to it.
	res, _ := h.send("POST", "/app/drive/import", token, `{"proposition":`+at+`,"file":"f1"}`)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("an import with no session gave %d", res.StatusCode)
	}
	if res, _ := h.get("/app/drive"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("a listing with no session gave %d", res.StatusCode)
	}
}

// A guest may read a proposition and may not add to it, so the picker is not
// theirs either.
func TestDriveListIsRefusedToAGuest(t *testing.T) {
	const password = "a long enough password"
	h, _, _ := connectedDrive(t, map[string]string{})
	if err := h.srv.settings.Set(context.Background(), "signin.require_totp",
		[]string{"owners"}, 1); err != nil {
		t.Fatal(err)
	}
	h.withoutAuthenticator("mara", auth.RoleGuest, password)
	h.signOut()
	if res, _ := h.signIn("mara", password, ""); res.Header.Get("Location") != "/" {
		t.Fatalf("the guest could not sign in: %s", res.Header.Get("Location"))
	}
	if res, _ := h.get("/app/drive"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a guest listing Drive gave %d", res.StatusCode)
	}
}

// Drive that is not connected is a state the pane can say something about
// rather than a failure.
func TestDriveListSaysWhenNothingIsConnected(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	res, body := h.get("/app/drive")
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("an unconnected Drive gave %d: %s", res.StatusCode, body)
	}
	if !strings.Contains(body, "not connected yet") {
		t.Fatalf("the answer said %s", body)
	}
}

// The import asks Drive what a file is called and how big it is before it
// writes anything, so the standing has to be settled first: otherwise somebody
// who may not import gets one answer for a file that is there and another for
// one that is not, and spends the workspace's Drive quota finding out.
func TestDriveRoutesCheckStandingBeforeAskingDrive(t *testing.T) {
	const password = "a long enough password"
	h, drive, live := connectedDrive(t, map[string]string{"f1": "twelve bytes"})
	ctx := context.Background()
	owner := core.Actor{Kind: core.KindUser, ID: 1, Name: "Ada Lovelace"}
	if err := h.srv.settings.Set(ctx, "signin.require_totp", []string{"owners"}, 1); err != nil {
		t.Fatal(err)
	}

	archived := h.proposition("Grid Storage")
	guest := h.withoutAuthenticator("gwen", auth.RoleGuest, password)
	member := h.withoutAuthenticator("mara", auth.RoleEditor, password)
	if _, err := h.srv.board.AddMember(ctx, owner, live, guest); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int64{live, archived} {
		if _, err := h.srv.board.AddMember(ctx, owner, at, member); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.srv.board.ArchiveProposition(ctx, owner, archived); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, who          string
		at                 int64
		listing, importing int
	}{
		{"a guest who is a member", "gwen", live, http.StatusForbidden, http.StatusForbidden},
		{"an editor who is not a member", "stranger", live, http.StatusOK, http.StatusNotFound},
		{"an editor on an archived proposition", "mara", archived, http.StatusOK, http.StatusForbidden},
	}
	// The stranger is an editor of the workspace and a member of nothing.
	h.withoutAuthenticator("stranger", auth.RoleEditor, password)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.signOut()
			if res, _ := h.signIn(c.who, password, ""); res.Header.Get("Location") != "/" {
				t.Fatalf("%s could not sign in: %s", c.who, res.Header.Get("Location"))
			}
			if res, _ := h.get("/app/drive"); res.StatusCode != c.listing {
				t.Errorf("listing gave %d, want %d", res.StatusCode, c.listing)
			}
			was := drive.hits.Load()
			res, body := h.send("POST", "/app/drive/import", h.csrf("/profile"),
				`{"proposition":`+strconv.FormatInt(c.at, 10)+`,"file":"f1","folder":"Recordings"}`)
			if res.StatusCode != c.importing {
				t.Errorf("import gave %d, want %d: %s", res.StatusCode, c.importing, body)
			}
			if drive.hits.Load() != was {
				t.Error("the refusal asked Drive about the file first")
			}
		})
	}

	// And the same request from somebody who may make it goes through, so the
	// refusals above are the standing and not the fixture.
	h.signOut()
	if res, _ := h.signIn("mara", password, ""); res.Header.Get("Location") != "/" {
		t.Fatal("the member could not sign in")
	}
	res, body := h.send("POST", "/app/drive/import", h.csrf("/profile"),
		`{"proposition":`+strconv.FormatInt(live, 10)+`,"file":"f1","folder":"Recordings"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a member importing gave %d: %s", res.StatusCode, body)
	}
}
