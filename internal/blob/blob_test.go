package blob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

const ttl = 10 * time.Minute

// fake starts gofakes3 in process with one bucket and returns a client for it.
func fake(t *testing.T) *Client {
	t.Helper()
	backend := s3mem.New()
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	if err := backend.CreateBucket("theses"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	c, err := New(Config{
		Provider:  "s3",
		Endpoint:  srv.URL,
		Region:    "us-east-1",
		Bucket:    "theses",
		AccessKey: "key",
		SecretKey: "secret",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func do(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s %s: status %d: %s", req.Method, req.URL.Path, resp.StatusCode, body)
	}
	return resp, body
}

func putSigned(t *testing.T, rawurl string, headers map[string]string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, rawurl, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.ContentLength = int64(len(body))
	resp, _ := do(t, req)
	return resp
}

func TestDefaults(t *testing.T) {
	for _, tc := range []struct {
		provider, hint, endpoint, region string
	}{
		{"b2", "us-west-004", "https://s3.us-west-004.backblazeb2.com", "us-west-004"},
		{"r2", "abc123", "https://abc123.r2.cloudflarestorage.com", "auto"},
		{"s3", "eu-central-1", "", "eu-central-1"},
	} {
		got := Defaults(tc.provider, tc.hint)
		if got.Provider != tc.provider || got.Endpoint != tc.endpoint || got.Region != tc.region {
			t.Errorf("Defaults(%q, %q) = %+v, want endpoint %q region %q", tc.provider, tc.hint, got, tc.endpoint, tc.region)
		}
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	ok := Config{Provider: "b2", Endpoint: "https://s3.us-west-004.backblazeb2.com", Region: "us-west-004", Bucket: "b", AccessKey: "k", SecretKey: "s"}
	for name, mutate := range map[string]func(*Config){
		"provider": func(c *Config) { c.Provider = "gcs" },
		"bucket":   func(c *Config) { c.Bucket = "" },
		"region":   func(c *Config) { c.Region = "" },
		"secret":   func(c *Config) { c.SecretKey = "" },
		"endpoint": func(c *Config) { c.Endpoint = "s3.us-west-004.backblazeb2.com" },
	} {
		cfg := ok
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("New with bad %s: want error, got nil", name)
		}
	}
	if _, err := New(ok); err != nil {
		t.Errorf("New with good config: %v", err)
	}
}

func TestPresignPutThenGet(t *testing.T) {
	c := fake(t)
	ctx := context.Background()
	body := []byte("a small recording stands in for a large one\n")

	put, headers, err := c.PresignPut(ctx, "1-slug/f1/notes.txt", "text/plain", int64(len(body)), ttl)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	putSigned(t, put, headers, body)

	size, etag, err := c.Head(ctx, "1-slug/f1/notes.txt")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if size != int64(len(body)) {
		t.Errorf("Head size = %d, want %d", size, len(body))
	}
	if strings.Trim(etag, `"`) == "" {
		t.Errorf("Head etag = %q, want a non empty tag", etag)
	}

	get, err := c.PresignGet(ctx, "1-slug/f1/notes.txt", "Notes on the thing.txt", ttl)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	u, err := url.Parse(get)
	if err != nil {
		t.Fatalf("parse get url: %v", err)
	}
	// gofakes3 does not apply response-content-disposition, so the query
	// parameter is what we can check; the header is the bucket's job.
	if got := u.Query().Get("response-content-disposition"); got != `attachment; filename="Notes on the thing.txt"` {
		t.Errorf("response-content-disposition = %q", got)
	}
	req, err := http.NewRequest(http.MethodGet, get, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, got := do(t, req)
	if !bytes.Equal(got, body) {
		t.Errorf("GET body = %q, want %q", got, body)
	}
}

// The fake never checks a signature, so the only way to catch the SDK adding
// headers a browser cannot send is to read the signed set out of the URL.
func TestPresignPutSignsOnlyBrowserHeaders(t *testing.T) {
	c := fake(t)
	put, headers, err := c.PresignPut(context.Background(), "k", "audio/wav", 1024, ttl)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	u, err := url.Parse(put)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := u.Query().Get("X-Amz-SignedHeaders"); got != "content-length;content-type;host" {
		t.Errorf("X-Amz-SignedHeaders = %q, want content-length;content-type;host", got)
	}
	if strings.Contains(strings.ToLower(put), "checksum") {
		t.Errorf("presigned PUT URL carries a checksum parameter: %s", put)
	}
	for k := range headers {
		if strings.Contains(strings.ToLower(k), "checksum") {
			t.Errorf("signed header %q is a checksum header the browser cannot produce", k)
		}
	}
	if got := headers["Content-Type"]; got != "audio/wav" {
		t.Errorf("signed Content-Type = %q, want audio/wav", got)
	}

	// The part URL is the path every raw WAV takes, so check its shape too.
	part, err := c.PresignParts(context.Background(), "k", "upload", []int{1}, ttl)
	if err != nil {
		t.Fatalf("PresignParts: %v", err)
	}
	pu, err := url.Parse(part[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := pu.Query().Get("X-Amz-SignedHeaders"); got != "host" {
		t.Errorf("part X-Amz-SignedHeaders = %q, want host", got)
	}
	if strings.Contains(strings.ToLower(part[0]), "checksum") {
		t.Errorf("presigned part URL carries a checksum parameter: %s", part[0])
	}
}

func TestMultipartRoundTrip(t *testing.T) {
	c := fake(t)
	ctx := context.Background()
	const key = "2-other/f2/take.wav"
	// gofakes3 does not enforce the 5 MiB minimum, so the parts are small;
	// PartSize is a constant the caller slices with, not a client setting.
	parts := [][]byte{bytes.Repeat([]byte("a"), 1024), bytes.Repeat([]byte("b"), 512)}

	uploadID, err := c.StartMultipart(ctx, key, "audio/wav")
	if err != nil {
		t.Fatalf("StartMultipart: %v", err)
	}
	urls, err := c.PresignParts(ctx, key, uploadID, []int{1, 2}, ttl)
	if err != nil {
		t.Fatalf("PresignParts: %v", err)
	}
	if len(urls) != 2 {
		t.Fatalf("PresignParts returned %d urls, want 2", len(urls))
	}
	var uploaded []Part
	for i, u := range urls {
		resp := putSigned(t, u, nil, parts[i])
		uploaded = append(uploaded, Part{Number: i + 1, ETag: resp.Header.Get("ETag")})
	}

	listed, err := c.ListParts(ctx, key, uploadID)
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if len(listed) != 2 || listed[0].Number != 1 || listed[1].Number != 2 {
		t.Fatalf("ListParts = %+v, want parts 1 and 2", listed)
	}

	// Out of order on purpose: CompleteMultipart sorts.
	if err := c.CompleteMultipart(ctx, key, uploadID, []Part{uploaded[1], uploaded[0]}); err != nil {
		t.Fatalf("CompleteMultipart: %v", err)
	}
	size, _, err := c.Head(ctx, key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if want := int64(len(parts[0]) + len(parts[1])); size != want {
		t.Errorf("assembled size = %d, want %d", size, want)
	}
	get, err := c.PresignGet(ctx, key, "", ttl)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, get, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, got := do(t, req)
	if want := append(append([]byte{}, parts[0]...), parts[1]...); !bytes.Equal(got, want) {
		t.Errorf("assembled object is %d bytes, want the two parts concatenated", len(got))
	}
}

func TestAbortMultipart(t *testing.T) {
	c := fake(t)
	ctx := context.Background()
	const key = "3-x/f3/dropped.wav"
	uploadID, err := c.StartMultipart(ctx, key, "audio/wav")
	if err != nil {
		t.Fatalf("StartMultipart: %v", err)
	}
	if err := c.AbortMultipart(ctx, key, uploadID); err != nil {
		t.Fatalf("AbortMultipart: %v", err)
	}
	if _, err := c.ListParts(ctx, key, uploadID); err == nil {
		t.Error("ListParts after abort: want error, got nil")
	}
}

func TestCopyAndDelete(t *testing.T) {
	c := fake(t)
	ctx := context.Background()
	body := []byte("version one")
	put, headers, err := c.PresignPut(ctx, "4-y/f4/paper.pdf", "application/pdf", int64(len(body)), ttl)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	putSigned(t, put, headers, body)

	if err := c.Copy(ctx, "4-y/f4/paper.pdf", "4-y/f4/v1/paper.pdf"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if size, _, err := c.Head(ctx, "4-y/f4/v1/paper.pdf"); err != nil || size != int64(len(body)) {
		t.Fatalf("Head of copy = %d, %v", size, err)
	}
	if err := c.Delete(ctx, "4-y/f4/paper.pdf"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := c.Head(ctx, "4-y/f4/paper.pdf"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Head after delete = %v, want ErrNotFound", err)
	}
}

func TestProbe(t *testing.T) {
	if err := fake(t).Probe(context.Background()); err != nil {
		t.Errorf("Probe: %v", err)
	}
}

func TestProbeNamesTheFailingStep(t *testing.T) {
	// A bucket that does not exist fails on the put, and the error says so.
	backend := s3mem.New()
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	c, err := New(Config{Provider: "s3", Endpoint: srv.URL, Region: "us-east-1", Bucket: "missing", AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = c.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "probe put") {
		t.Errorf("Probe against a missing bucket = %v, want an error naming the put step", err)
	}
}

func TestCORSRules(t *testing.T) {
	for name, rule := range map[string]string{"b2": CORSRuleB2("https://theses.example.com"), "r2": CORSRuleR2("https://theses.example.com")} {
		if !json.Valid([]byte(rule)) {
			t.Errorf("%s rule is not valid JSON: %s", name, rule)
		}
		var got []map[string]any
		if err := json.Unmarshal([]byte(rule), &got); err != nil {
			t.Fatalf("%s rule: %v", name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s rule has %d entries, want 1", name, len(got))
		}
		if !strings.Contains(rule, "https://theses.example.com") {
			t.Errorf("%s rule does not name the origin: %s", name, rule)
		}
	}
	var b2 []struct {
		Name       string   `json:"corsRuleName"`
		Operations []string `json:"allowedOperations"`
		Expose     []string `json:"exposeHeaders"`
		MaxAge     int      `json:"maxAgeSeconds"`
	}
	if err := json.Unmarshal([]byte(CORSRuleB2("https://x.test")), &b2); err != nil {
		t.Fatalf("b2 rule: %v", err)
	}
	if b2[0].Name == "" || strings.Join(b2[0].Operations, ",") != "s3_get,s3_head,s3_put" || b2[0].MaxAge != 3600 {
		t.Errorf("b2 rule = %+v", b2[0])
	}
	if strings.Join(b2[0].Expose, ",") != "ETag" {
		t.Errorf("b2 exposeHeaders = %v, want ETag", b2[0].Expose)
	}
	var r2 []struct {
		Methods []string `json:"AllowedMethods"`
		Expose  []string `json:"ExposeHeaders"`
		MaxAge  int      `json:"MaxAgeSeconds"`
	}
	if err := json.Unmarshal([]byte(CORSRuleR2("https://x.test")), &r2); err != nil {
		t.Fatalf("r2 rule: %v", err)
	}
	if strings.Join(r2[0].Methods, ",") != "GET,HEAD,PUT" || strings.Join(r2[0].Expose, ",") != "ETag" || r2[0].MaxAge != 3600 {
		t.Errorf("r2 rule = %+v", r2[0])
	}
}
