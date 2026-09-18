package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/settings"
)

// buckets holds one client per configured bucket, rebuilt when the settings
// change. A client makes no network call to build, but it does read two
// secrets out of the database and decrypt them, which is not work to do on
// every part of a six gigabyte upload.
type buckets struct {
	mu    sync.Mutex
	made  map[string]*blob.Client
	stamp map[string]string
}

func newBuckets() *buckets {
	return &buckets{made: map[string]*blob.Client{}, stamp: map[string]string{}}
}

// bucketFor is what files.Service asks: the recordings bucket when one is
// configured and the folder is Recordings, the primary one otherwise.
func (s *Server) bucketFor(ctx context.Context, folder string) (*blob.Client, error) {
	prefix := "storage.primary"
	if folder == files.Recordings && settings.Get[string](s.settings, "storage.recordings.bucket") != "" {
		prefix = "storage.recordings"
	}
	cfg, err := s.bucketConfig(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, files.ErrNoBucket
	}
	// The fingerprint is every field that changes the client, so saving new
	// keys on the settings page takes effect on the next upload without a
	// restart. The secret is in it because rotating a key changes nothing else.
	stamp := strings.Join([]string{cfg.Provider, cfg.Endpoint, cfg.Region, cfg.Bucket, cfg.AccessKey, cfg.SecretKey}, "\x00")
	s.blobs.mu.Lock()
	defer s.blobs.mu.Unlock()
	if c, ok := s.blobs.made[prefix]; ok && s.blobs.stamp[prefix] == stamp {
		return c, nil
	}
	c, err := blob.New(cfg)
	if err != nil {
		return nil, err
	}
	s.blobs.made[prefix], s.blobs.stamp[prefix] = c, stamp
	return c, nil
}

// storageOrigins are the origins the browser talks to directly: the bucket
// endpoints and any CDN in front of them. They go into the CSP, because
// uploading and showing a thumbnail are cross-origin requests the page makes
// and connect-src 'self' would refuse them.
func (s *Server) storageOrigins() []string {
	var out []string
	for _, prefix := range []string{"storage.primary", "storage.recordings"} {
		for _, key := range []string{".endpoint", ".public_base_url"} {
			if o := originOf(settings.Get[string](s.settings, prefix+key)); o != "" && !contains(out, o) {
				out = append(out, o)
			}
		}
	}
	return out
}

func originOf(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// sweepEvery is how often abandoned uploads are looked for. The window is 48
// hours, so an hour's granularity is plenty and a restart never misses one.
const sweepEvery = time.Hour

// SweepUploads abandons uploads nobody finished, now and every hour after. It
// stops with ctx and holds nothing between ticks, so a cancellation costs
// whatever the current sweep has done and no more.
func (s *Server) SweepUploads(ctx context.Context) {
	sweep := func() {
		if err := s.files.Sweep(ctx); err != nil && !errors.Is(err, files.ErrNoBucket) {
			s.log.Error("upload sweep", "err", err)
		}
	}
	sweep()
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

// postTestCORS is the other half of the storage test, the half the server
// cannot do: whether the bucket's CORS rule lets this origin PUT to it. The
// page asks for a presigned PUT, tries it the way an upload does, and asks
// again with done=1 to have the probe object removed. A bucket with no rule
// fails on the settings page rather than on somebody's first upload.
func (s *Server) postTestCORS(w http.ResponseWriter, r *http.Request) {
	prefix := r.PostFormValue("prefix")
	if prefix != "storage.primary" && prefix != "storage.recordings" {
		writeAPIError(w, http.StatusNotFound, "no such bucket")
		return
	}
	cfg, err := s.bucketConfig(r.Context(), prefix)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, mail.Redact(err.Error(), cfg.SecretKey))
		return
	}
	client, err := blob.New(cfg)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, mail.Redact(err.Error(), cfg.SecretKey))
		return
	}
	// The key is derived from who is asking rather than sent by the browser, so
	// this route cannot be used to write or delete an object of somebody's
	// choosing. Two owners testing at once get a probe each.
	key := fmt.Sprintf("probe/cors-%d", userOf(r).ID)

	if r.PostFormValue("done") != "" {
		if err := client.Delete(r.Context(), key); err != nil {
			s.log.Warn("could not delete a cors probe object", "key", key, "err", err)
		}
		writeJSON(w, map[string]any{"cleaned": true})
		return
	}

	const body = "theses cors probe\n"
	url, headers, err := client.PresignPut(r.Context(), key, "text/plain", int64(len(body)), corsProbeTTL)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{
		"url": url, "headers": headers, "body": body, "origin": s.origin(),
	})
}

// corsProbeTTL is how long the probe URL lives: long enough for a browser to
// do a preflight and a PUT, short enough that it is no use to anyone who saw
// it go past.
const corsProbeTTL = 2 * time.Minute

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
