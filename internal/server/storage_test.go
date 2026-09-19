package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// fakeBucket starts gofakes3 in process with one bucket and returns its URL.
func fakeBucket(t *testing.T, name string) string { return fakeCORSBucket(t, name, nil) }

// fakeCORSBucket is fakeBucket with the ?cors subresource answered by cors,
// which gofakes3 does not implement at all: without one, a call to it would
// land on the bucket routes and be taken for something else.
func fakeCORSBucket(t *testing.T, name string, cors http.Handler) string {
	t.Helper()
	backend := s3mem.New()
	fake := gofakes3.New(backend).Server()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cors != nil && r.URL.Query().Has("cors") {
			cors.ServeHTTP(w, r)
			return
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	if err := backend.CreateBucket(name); err != nil {
		t.Fatal(err)
	}
	return srv.URL
}

// corsRules is a bucket's rule document: a PUT keeps it, a GET gives it back.
// deny, when set, is the sentence the provider refuses every call with, the
// way a key that may not write bucket settings does. answer, when set, is what
// a GET reports whatever was put, which is a provider quietly keeping the rule
// it already had.
func corsRules(deny, answer string) http.Handler {
	var mu sync.Mutex
	var held string
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case deny != "":
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>"+deny+"</Message></Error>")
		case r.Method == http.MethodPut:
			b, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			held = string(b)
		case answer != "":
			io.WriteString(w, answer)
		case held == "":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<Error><Code>NoSuchCORSConfiguration</Code>"+
				"<Message>The CORS configuration does not exist</Message></Error>")
		default:
			io.WriteString(w, held)
		}
	})
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

	res, body := h.postBack("/settings/test/storage", url.Values{
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

	res, body := h.postBack("/settings/test/storage", url.Values{
		"csrf": {h.csrf("/settings")}, "prefix": {"storage.primary"},
	})
	if res.StatusCode != http.StatusOK {
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

func TestApplyCORS(t *testing.T) {
	// The origin is THESES_BASE_URL's, which the harness sets to this.
	const origin = "http://localhost:8080"
	for _, c := range []struct {
		name, deny, answer, want string
		failed                   bool
	}{
		{name: "the rule is put and read back",
			want: "Applied the rule to theses, which now allows " + origin + "."},
		{name: "the bucket keeps a rule for somebody else",
			answer: "<CORSConfiguration><CORSRule><AllowedOrigin>https://elsewhere.example.com" +
				"</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>",
			want: "reading it back did not find " + origin, failed: true},
		{name: "the key may not write bucket settings",
			deny: "this key cannot write bucket settings",
			want: "this key cannot write bucket settings", failed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			endpoint := fakeCORSBucket(t, "theses", corsRules(c.deny, c.answer))
			h := newHarness(t)
			h.setupOwner()
			h.configureBucket(endpoint, "theses")

			res, body := h.postBack("/settings/cors", url.Values{
				"csrf": {h.csrf("/settings")}, "prefix": {"storage.primary"},
			})
			if res.StatusCode != http.StatusOK {
				t.Fatalf("applying the rule gave %d:\n%s", res.StatusCode, body)
			}
			if !strings.Contains(body, c.want) {
				t.Errorf("the page does not say %q:\n%s", c.want, firstNotice(body))
			}
			byHand := strings.Contains(body, corsByHand)
			if byHand != c.failed {
				t.Errorf("the by hand line is %v, want %v:\n%s", byHand, c.failed, firstNotice(body))
			}
			if strings.Contains(body, bucketSecret) {
				t.Error("the secret key is on the page")
			}
		})
	}
}

func TestApplyCORSRefusesAnUnknownBucketName(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	res, _ := h.post("/settings/cors", url.Values{
		"csrf": {h.csrf("/settings")}, "prefix": {"storage.backups"},
	})
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a prefix outside the two buckets gave %d", res.StatusCode)
	}
}

// The button is only printed for a bucket there is one of, because applying a
// rule to a bucket nobody named is a refusal with nothing to say.
func TestTheApplyButtonWaitsForABucket(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	_, body := h.get("/settings")
	if strings.Contains(body, "Apply the CORS rule") {
		t.Errorf("the button is printed with no bucket configured:\n%s", body)
	}
	h.configureBucket("https://s3.example.com", "theses")
	_, body = h.get("/settings")
	if !strings.Contains(body, "Apply the CORS rule") {
		t.Error("the button is missing for the configured bucket")
	}
}
