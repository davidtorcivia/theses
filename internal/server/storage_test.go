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

// corsFake is a bucket's rule document: a PUT keeps it, a GET gives it back,
// and a GET before any PUT says there is none, the way a bucket with no rule
// does. put and get replace either answer, which is how a provider that
// refuses the call, or takes it and will not read it back, is tested.
type corsFake struct {
	put, get corsAnswer
	mu       sync.Mutex
	held     string
}

// corsAnswer is one canned reply: status, when set, is the refusal, and body
// is what goes with it, or what a GET reports instead of the document held.
type corsAnswer struct {
	status int
	body   string
}

// corsError is the shape a provider refuses in, so the sentence in it is the
// one the SDK lifts out and the page has to print.
func corsError(code, message string) string {
	return "<Error><Code>" + code + "</Code><Message>" + message + "</Message></Error>"
}

func (f *corsFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	said := f.get
	if r.Method == http.MethodPut {
		said = f.put
	}
	w.Header().Set("Content-Type", "application/xml")
	switch {
	case said.status != 0:
		w.WriteHeader(said.status)
		io.WriteString(w, said.body)
	case r.Method == http.MethodPut:
		b, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.held = string(b)
	case said.body != "":
		io.WriteString(w, said.body)
	case f.held == "":
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, corsError("NoSuchCORSConfiguration", "The CORS configuration does not exist"))
	default:
		io.WriteString(w, f.held)
	}
}

// rule is the document the bucket is holding, for a test that wants to see
// what was actually sent to it.
func (f *corsFake) rule() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
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
		name, want  string
		put, get    corsAnswer // what the fake bucket answers, when not the truth
		rule        []string   // what the bucket is holding afterwards
		byHand, bad bool
	}{
		{name: "the rule is put and read back",
			want: "The rule went to theses. It now allows " + origin + ".",
			rule: []string{
				"<AllowedOrigin>" + origin + "</AllowedOrigin>",
				"<AllowedMethod>GET</AllowedMethod>",
				"<AllowedMethod>HEAD</AllowedMethod>",
				"<AllowedMethod>PUT</AllowedMethod>",
				"<AllowedHeader>*</AllowedHeader>",
				"<ExposeHeader>ETag</ExposeHeader>",
				"<MaxAgeSeconds>3600</MaxAgeSeconds>",
			}},
		{name: "the key may not write bucket settings",
			put: corsAnswer{status: http.StatusForbidden,
				body: corsError("AccessDenied", "this key cannot write bucket settings")},
			want: "this key cannot write bucket settings", byHand: true, bad: true},

		// Everything below took the put, so the rule is on the bucket whatever
		// the read back says and none of it sends the owner off to do it again.
		{name: "the bucket keeps a rule for somebody else",
			get: corsAnswer{body: "<CORSConfiguration><CORSRule><AllowedOrigin>" +
				"https://elsewhere.example.com</AllowedOrigin><AllowedMethod>GET</AllowedMethod>" +
				"</CORSRule></CORSConfiguration>"},
			want: "The rule went to theses. Reading it back did not find " + origin + " yet. " + corsSoon},
		{name: "the read back is refused",
			get: corsAnswer{status: http.StatusForbidden,
				body: corsError("AccessDenied", "this key cannot read bucket settings")},
			want: "this key cannot read bucket settings"},
		{name: "the bucket says it has no rule",
			get: corsAnswer{status: http.StatusNotFound,
				body: corsError("NoSuchCORSConfiguration", "The CORS configuration does not exist")},
			want: "The rule went to theses. Reading it back failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := &corsFake{put: c.put, get: c.get}
			endpoint := fakeCORSBucket(t, "theses", fake)
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
			if got := strings.Contains(body, corsByHand); got != c.byHand {
				t.Errorf("the by hand line is %v, want %v:\n%s", got, c.byHand, firstNotice(body))
			}
			if got := strings.Contains(noticeClass(body, c.want), "bad"); got != c.bad {
				t.Errorf("the notice is a failure: %v, want %v:\n%s", got, c.bad, firstNotice(body))
			}
			for _, want := range c.rule {
				if !strings.Contains(fake.rule(), want) {
					t.Errorf("the rule on the bucket has no %s:\n%s", want, fake.rule())
				}
			}
			if strings.Contains(body, bucketSecret) {
				t.Error("the secret key is on the page")
			}
		})
	}
}

// noticeClass is the class list of the notice carrying text. The page has
// notices from other sections on it, and only the one holding the answer to
// this button says whether it failed.
func noticeClass(body, text string) string {
	i := strings.Index(body, text)
	if i < 0 {
		return ""
	}
	const open = `<p class="`
	at := strings.LastIndex(body[:i], open)
	if at < 0 {
		return ""
	}
	rest := body[at+len(open):]
	return rest[:strings.Index(rest, `"`)]
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
	// The hint beside the printed rule names the button, so it is the element
	// itself that is looked for.
	const button = `value="storage.primary">Apply the CORS rule<`
	_, body := h.get("/settings")
	if strings.Contains(body, button) {
		t.Errorf("the button is printed with no bucket configured:\n%s", body)
	}
	h.configureBucket("https://s3.example.com", "theses")
	_, body = h.get("/settings")
	if !strings.Contains(body, button) {
		t.Error("the button is missing for the configured bucket")
	}
}
