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
	// The fixture's independent anonymous context checks the same private targets.
	origin, _ := url.Parse(h.http.URL)
	cookies := []map[string]any{}
	for _, cookie := range h.client.Jar.Cookies(origin) {
		cookies = append(cookies, map[string]any{"name": cookie.Name, "value": cookie.Value, "url": browserServer.URL})
	}
	fixture := map[string]any{"url": browserServer.URL, "cookies": cookies, "proposition": proposition, "large": large, "card": card.EntityID, "owner": owner.ID}
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
	cmd := exec.CommandContext(run, "node", "../../web/browser_smoke.mjs", path)
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("browser workflow: %v", err)
	}
}
