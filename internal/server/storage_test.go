package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// fakeBucket starts gofakes3 in process with one bucket and returns its URL.
func fakeBucket(t *testing.T, name string) string {
	t.Helper()
	backend := s3mem.New()
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	if err := backend.CreateBucket(name); err != nil {
		t.Fatal(err)
	}
	return srv.URL
}

// bucketSecret is distinctive so a test can tell it from the word "secret" the
// form prints beside the field.
const bucketSecret = "s3cr3t-never-on-the-page"

func (h *harness) configureBucket(endpoint, bucket string) {
	h.Helper()
	res, body := h.post("/settings", url.Values{
		"csrf":                            {h.csrf("/settings")},
		"storage.primary.provider":        {"s3"},
		"storage.primary.endpoint":        {endpoint},
		"storage.primary.region":          {"us-east-1"},
		"storage.primary.bucket":          {bucket},
		"storage.primary.access_key":      {"key"},
		"storage.primary.secret_key":      {bucketSecret},
		"storage.primary.public_base_url": {""},
	})
	if res.StatusCode != http.StatusSeeOther {
		h.Fatalf("saving the storage settings gave %d:\n%s", res.StatusCode, body)
	}
}

func TestStorageProbeWritesReadsAndDeletes(t *testing.T) {
	endpoint := fakeBucket(t, "theses")
	h := newHarness(t)
	h.setupOwner()
	h.configureBucket(endpoint, "theses")

	res, body := h.post("/settings/test/storage", url.Values{
		"csrf": {h.csrf("/settings")}, "prefix": {"storage.primary"},
	})
	if res.StatusCode != http.StatusOK ||
		!strings.Contains(body, "Wrote, read and deleted a probe object in theses.") {
		t.Fatalf("the probe gave %d:\n%s", res.StatusCode, body)
	}
}

func TestStorageProbeReportsAMissingBucket(t *testing.T) {
	endpoint := fakeBucket(t, "theses")
	h := newHarness(t)
	h.setupOwner()
	h.configureBucket(endpoint, "elsewhere")

	res, body := h.post("/settings/test/storage", url.Values{
		"csrf": {h.csrf("/settings")}, "prefix": {"storage.primary"},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a bucket that is not there gave %d", res.StatusCode)
	}
	if !strings.Contains(body, "blob: probe put") {
		t.Errorf("the failure is not shown inline:\n%s", body)
	}
	if strings.Contains(body, bucketSecret) {
		t.Error("the secret key is on the page")
	}
}

func TestStorageProbeRefusesAnUnknownBucketName(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	res, _ := h.post("/settings/test/storage", url.Values{
		"csrf": {h.csrf("/settings")}, "prefix": {"storage.backups"},
	})
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a prefix outside the two buckets gave %d", res.StatusCode)
	}
}

func TestCORSBlockFollowsTheProvider(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	// Backblaze is the default, and B2 keeps its own rule shape.
	_, body := h.get("/settings")
	if !strings.Contains(body, "corsRuleName") || !strings.Contains(body, "s3_put") {
		t.Errorf("the B2 rule is not rendered:\n%s", body)
	}
	if !strings.Contains(body, "http://localhost:8080") {
		t.Error("the rule does not carry the origin from THESES_BASE_URL")
	}

	res, _ := h.post("/settings", url.Values{
		"csrf": {h.csrf("/settings")}, "storage.primary.provider": {"r2"},
		"storage.recordings.provider": {"r2"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("saving the provider gave %d", res.StatusCode)
	}
	_, body = h.get("/settings")
	if !strings.Contains(body, "AllowedOrigins") || !strings.Contains(body, "AllowedMethods") {
		t.Errorf("the S3 rule is not rendered for R2:\n%s", body)
	}
	if strings.Contains(body, "corsRuleName") {
		t.Error("the B2 rule is still on the page for the primary bucket")
	}
}
