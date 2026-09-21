package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
)

func TestBrowserWorkflow(t *testing.T) {
	if os.Getenv("THESES_BROWSER_TEST") != "1" {
		t.Skip("set THESES_BROWSER_TEST=1 with Playwright installed")
	}
	h := newHarness(t)
	h.setupOwner()
	h.configureBucket(fakeBucket(t, "browser-fixture"), "browser-fixture")
	proposition := h.proposition("Tidal Power")
	ctx := context.Background()
	owner := h.owner()
	actor := core.Actor{Kind: core.KindUser, ID: owner.ID, Name: owner.Name}
	show, err := board.GetShow(ctx, h.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.board.CreateColumn(ctx, actor, show.ID, "Ready"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.board.CreateColumn(ctx, actor, show.ID, "Next"); err != nil {
		t.Fatal(err)
	}
	cols, err := board.ListColumns(ctx, h.db, proposition)
	if err != nil {
		t.Fatal(err)
	}
	card, err := h.srv.board.CreateCard(ctx, actor, cols[0].ID, "Browser fixture task", nil)
	if err != nil {
		t.Fatal(err)
	}
	large := h.proposition("Large workspace")
	largeCols, err := board.ListColumns(ctx, h.db, large)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<500) INSERT INTO cards(proposition_id,column_id,position,title,created_at) SELECT ?,?,printf('%06d',x),'Research task '||x,1 FROM n`, large, largeCols[0].ID); err != nil {
		t.Fatal(err)
	}
	recording, err := h.db.ExecContext(ctx, `INSERT INTO files(proposition_id,name,folder,kind,size,object_key,state,duration_ms,created_at) VALUES(?,'Studio recording.wav','Recordings','audio',4,'browser-recording','ready',10000,unixepoch())`, proposition)
	if err != nil {
		t.Fatal(err)
	}
	recordingID, err := recording.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	var largeDocument int64
	if err := h.db.QueryRowContext(ctx, "SELECT id FROM documents WHERE proposition_id=? ORDER BY position,id LIMIT 1", large).Scan(&largeDocument); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS(VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<200) INSERT INTO blocks(document_id,position,text,updated_at) SELECT ?,printf('z%06d',x),?,1 FROM n`, largeDocument, strings.Repeat("Research paragraph with checked sources and recording notes. ", 15)); err != nil {
		t.Fatal(err)
	}
	var workerRevision atomic.Uint64
	browserServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__smoke/upgrade" {
			workerRevision.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/sw.js" {
			h.srv.ServeHTTP(w, r)
			fmt.Fprintf(w, "\n// fixture revision %d\n", workerRevision.Load())
			return
		}
		h.srv.ServeHTTP(w, r)
	}))
	t.Cleanup(browserServer.Close)
	h.srv.cfg.BaseURL = browserServer.URL
	// The fixture's independent anonymous context checks the same private targets.
	origin, _ := url.Parse(h.http.URL)
	cookies := []map[string]any{}
	for _, cookie := range h.client.Jar.Cookies(origin) {
		cookies = append(cookies, map[string]any{"name": cookie.Name, "value": cookie.Value, "url": browserServer.URL})
	}
	fixture := map[string]any{"url": browserServer.URL, "cookies": cookies, "proposition": proposition, "large": large, "card": card.EntityID, "owner": owner.ID, "recording": recordingID}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "browser.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	run, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	script := os.Getenv("THESES_BROWSER_SCRIPT")
	if script == "" {
		script = "../../web/browser_smoke.mjs"
	}
	cmd := exec.CommandContext(run, "node", script, path)
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("browser workflow: %v", err)
	}
}
