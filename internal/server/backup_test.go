package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// noDeleteBucket is the bucket as B2 presents it to a key made with write and
// list but not delete: everything works except the delete, which is refused.
func noDeleteBucket(t *testing.T, name string) string {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(name); err != nil {
		t.Fatal(err)
	}
	inner := gofakes3.New(backend).Server()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (h *harness) configureBackups(endpoint, bucket string) {
	h.Helper()
	h.configureBucket(endpoint, bucket)
	res, body := h.post("/settings", url.Values{
		"csrf":               {h.csrf("/settings")},
		"backups.enabled":    {"on"},
		"backups.time":       {"03:30"},
		"backups.keep":       {"30"},
		"backups.bucket":     {""},
		"backups.access_key": {"backup-key"},
		"backups.secret_key": {bucketSecret},
	})
	if res.StatusCode != http.StatusSeeOther {
		h.Fatalf("saving the backup settings gave %d:\n%s", res.StatusCode, body)
	}
}

func TestBackupSectionRendersAndSaves(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	_, body := h.get("/settings")
	for _, want := range []string{"Backups", "backups/", "Object Lock", "Test backup key", "Back up now",
		"No backup has been taken yet."} {
		if !strings.Contains(body, want) {
			t.Errorf("the section does not mention %q", want)
		}
	}

	h.configureBackups(noDeleteBucket(t, "example-bucket"), "example-bucket")
	_, body = h.get("/settings")
	if !strings.Contains(body, `<option value="on" selected>On</option>`) {
		t.Error("the toggle did not save")
	}
	if !strings.Contains(body, "set, leave empty to keep") {
		t.Error("the saved backup key is not shown as set")
	}
	if strings.Contains(body, bucketSecret) {
		t.Error("the backup secret key is on the page")
	}
	if !strings.Contains(body, "Nothing under backups/ yet.") {
		t.Errorf("the empty listing is not shown:\n%s", body)
	}
}

func TestTestBackupKeyPassesOnARefusedDelete(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.configureBackups(noDeleteBucket(t, "example-bucket"), "example-bucket")

	res, body := h.postBack("/settings/test/backups", url.Values{"csrf": {h.csrf("/settings")}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the probe gave %d:\n%s", res.StatusCode, body)
	}
	if !strings.Contains(body, "the delete was refused") {
		t.Errorf("the result does not say a refused delete is the pass:\n%s", body)
	}
}

func TestTestBackupKeyFailsWhenTheKeyCanDelete(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.configureBackups(fakeBucket(t, "example-bucket"), "example-bucket")

	res, body := h.postBack("/settings/test/backups", url.Values{"csrf": {h.csrf("/settings")}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a key that can delete gave %d", res.StatusCode)
	}
	if !strings.Contains(body, "deleted its own probe object") {
		t.Errorf("the failure does not say why:\n%s", body)
	}
}

func TestBackUpNowThenListAndRestore(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.configureBackups(noDeleteBucket(t, "example-bucket"), "example-bucket")

	// Production owns the restore hook from the document watcher goroutine.
	// This integration test starts and joins it on the same lifecycle.
	docsCtx, stopDocs := context.WithCancel(context.Background())
	docsDone := make(chan struct{})
	go func() { defer close(docsDone); h.srv.Docs().Run(docsCtx) }()
	defer func() {
		stopDocs()
		select {
		case <-docsDone:
		case <-time.After(5 * time.Second):
			t.Error("timed out stopping the document mirror")
		}
	}()
	mirrorReady := false
	for range 100 {
		if h.srv.Docs().WithMirrorPaused(context.Background(), func() error { return nil }) == nil {
			mirrorReady = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !mirrorReady {
		t.Fatal("the document mirror did not start")
	}

	// The run is in the background, so the page says it started and the archive
	// shows up in the listing once it has.
	res, body := h.postBack("/settings/backups/now", url.Values{"csrf": {h.csrf("/settings")}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Started.") {
		t.Fatalf("back up now gave %d:\n%s", res.StatusCode, body)
	}
	key := ""
	for range 100 {
		time.Sleep(20 * time.Millisecond)
		list, err := h.srv.backups.List(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 && list[0].Manifest != "" {
			key = list[0].Key
			break
		}
	}
	if key == "" {
		t.Fatal("no archive appeared under the prefix")
	}

	// The archive is in the bucket before the run is written down: the object
	// goes up and the settings row saying when it went follows it. So the page
	// is read the way the listing was, rather than once and at whichever moment
	// the upload happened to finish.
	recorded := false
	for range 100 {
		_, body = h.get("/settings")
		if strings.Contains(body, "Last backup") {
			recorded = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !recorded {
		t.Error("the page does not say when the last backup was")
	}
	if !strings.Contains(body, "Restore") || !strings.Contains(body, "<dialog id=\"restore-0\">") {
		t.Errorf("the listed backup has no confirmation dialog:\n%s", body)
	}

	res, body = h.postBack("/settings/backups/restore", url.Values{
		"csrf": {h.csrf("/settings")}, "key": {key},
	})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Restoring") {
		t.Fatalf("restore gave %d:\n%s", res.StatusCode, body)
	}
	// The restore runs in the background; the page reports it afterwards.
	for range 100 {
		time.Sleep(20 * time.Millisecond)
		if strings.Contains(h.srv.backups.LastRestore(), "Restored") {
			return
		}
		if msg := h.srv.backups.LastRestore(); msg != "" {
			t.Fatalf("the restore failed: %s", msg)
		}
	}
	t.Fatal("the restore never finished")
}

func TestRestoreRefusesAKeyOutsideThePrefix(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.configureBackups(noDeleteBucket(t, "example-bucket"), "example-bucket")

	res, _ := h.post("/settings/backups/restore", url.Values{
		"csrf": {h.csrf("/settings")}, "key": {"files/10-slug/x.tar.gz.age"},
	})
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a key outside the prefix gave %d", res.StatusCode)
	}
}

func TestReadyzNamesEveryCheck(t *testing.T) {
	h := newHarness(t)
	res, body := h.get("/readyz")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("readyz gave %d:\n%s", res.StatusCode, body)
	}
	for _, want := range []string{"database: ok", "object store: ok", "backup age: ok"} {
		if !strings.Contains(body, want) {
			t.Errorf("readyz does not report %q:\n%s", want, body)
		}
	}
}

func TestReadyzFailsOnAStaleBackup(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.configureBackups(noDeleteBucket(t, "example-bucket"), "example-bucket")

	res, body := h.get("/readyz")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("backups on and never run gave %d:\n%s", res.StatusCode, body)
	}
	if !strings.Contains(body, "backup age: failed") {
		t.Errorf("readyz does not name the stale backup:\n%s", body)
	}
	if !strings.Contains(h.log.String(), "no backup has succeeded yet") {
		t.Error("the reason is not in the log either")
	}
}

func TestWritesAreRefusedWhileARestoreRuns(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.srv.backups.Freeze(true)
	defer h.srv.backups.Freeze(false)

	res, body := h.post("/settings", url.Values{
		"csrf": {h.csrf("/settings")}, "workspace.name": {"Workspace"},
	})
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a write during a restore gave %d", res.StatusCode)
	}
	if !strings.Contains(body, "Restoring a backup.") {
		t.Errorf("the offline style page is not what came back:\n%s", body)
	}
	if res, _ := h.get("/settings"); res.StatusCode != http.StatusOK {
		t.Errorf("reading during a restore gave %d", res.StatusCode)
	}
}

func TestReadyzDoesNotPrintWhereTheObjectStoreIs(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	// A bucket that is not answering. The error names the endpoint and the
	// bucket, and this route has no session in front of it.
	gone := httptest.NewServer(http.NotFoundHandler())
	endpoint := gone.URL
	gone.Close()
	h.configureBucket(endpoint, "example-bucket")

	res, body := h.get("/readyz")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a bucket that is not there gave %d:\n%s", res.StatusCode, body)
	}
	if !strings.Contains(body, "object store: failed") {
		t.Errorf("readyz does not name the check:\n%s", body)
	}
	for _, leak := range []string{endpoint, "http", "example-bucket"} {
		if strings.Contains(body, leak) {
			t.Errorf("readyz prints %q to anyone who asks:\n%s", leak, body)
		}
	}
	if !strings.Contains(h.log.String(), endpoint) {
		t.Error("the detail is not in the log either")
	}
}
