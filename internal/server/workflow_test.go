package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/workflow"
)

func TestLocalTranscriptionQueue(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.configureBucket(fakeBucket(t, "transcription-fixture"), "transcription-fixture")
	prop := h.proposition("Recorded episode")
	csrf := h.csrf(fmt.Sprintf("/p/%d/settings", prop))
	res, raw := h.send("POST", "/app/files", csrf, fmt.Sprintf(`{"proposition":%d,"name":"take.wav","folder":"Recordings","size":4}`, prop))
	if res.StatusCode != 200 {
		t.Fatal(raw)
	}
	var upload files.Upload
	if err := json.Unmarshal([]byte(raw), &upload); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("PUT", upload.URL, strings.NewReader("wave"))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range upload.Headers {
		req.Header.Set(k, v)
	}
	put, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	put.Body.Close()
	if put.StatusCode >= 300 {
		t.Fatalf("upload %d", put.StatusCode)
	}
	res, raw = h.send("POST", fmt.Sprintf("/app/files/%d/complete", upload.File.ID), csrf, `{"duration_ms":10000}`)
	if res.StatusCode != 200 {
		t.Fatal(raw)
	}
	called := make(chan struct{}, 2)
	var blockNext atomic.Bool
	started := make(chan struct{})
	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Error(err)
			http.Error(w, "bad multipart", 400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		content, _ := io.ReadAll(file)
		if string(content) != "wave" || r.FormValue("prompt") != "" || r.FormValue("diarize") != "false" || r.FormValue("response_format") != "vtt" {
			t.Error("wrong local transcription request")
		}
		if blockNext.Load() {
			close(started)
			<-r.Context().Done()
			return
		}
		called <- struct{}{}
		w.Header().Set("Content-Type", "text/vtt")
		io.WriteString(w, "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\n<v Host A>Searchable transcript evidence.</v>\n")
	}))
	defer inference.Close()
	h.srv.Workflow().WhisperURL = inference.URL
	endpoint := fmt.Sprintf("/app/files/%d/transcription-jobs", upload.File.ID)
	res, raw = h.send("POST", endpoint, csrf, `{"stereo":false}`)
	if res.StatusCode != 200 {
		t.Fatal(raw)
	}
	res, raw = h.send("POST", endpoint, csrf, `{}`)
	if res.StatusCode != 409 || !strings.Contains(raw, "already queued") {
		t.Fatalf("duplicate queue %d %s", res.StatusCode, raw)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.srv.Workflow().Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-called:
	case <-time.After(10 * time.Second):
		t.Fatal("local inference never called")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, raw = h.get(endpoint)
		var result struct {
			Jobs []workflow.Job `json:"jobs"`
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Jobs) > 0 && result.Jobs[0].State == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, raw = h.get(fmt.Sprintf("/app/files/%d/transcript", upload.File.ID))
	if !strings.Contains(raw, "Searchable transcript evidence") || !strings.Contains(raw, "Host A") {
		t.Fatal(raw)
	}
	_, raw = h.get("/app/search?q=Searchable")
	if !strings.Contains(raw, "transcript") {
		t.Fatal(raw)
	}
	res, raw = h.send("PUT", fmt.Sprintf("/app/files/%d/transcript", upload.File.ID), csrf, `{"text":"Old text","format":"txt","version":0}`)
	if res.StatusCode != 409 {
		t.Fatalf("stale transcript %d %s", res.StatusCode, raw)
	}
	res, raw = h.send("POST", endpoint, csrf, `{}`)
	if res.StatusCode != 200 {
		t.Fatal(raw)
	}
	if err := h.srv.Workflow().WithPaused(context.Background(), func() error { return h.srv.Workflow().Recover(context.Background()) }); err != nil {
		t.Fatal(err)
	}
	_, raw = h.get(endpoint)
	if !strings.Contains(raw, `"state":"queued"`) {
		t.Fatal("configured recovery discarded queued work: " + raw)
	}
	cancel()
	<-done
	h.srv.Workflow().WhisperURL = ""
	if err := h.srv.Workflow().Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, raw = h.get(endpoint)
	if strings.Contains(raw, `"state":"queued"`) || !strings.Contains(raw, "no longer configured") {
		t.Fatal(raw)
	}
	h.srv.Workflow().WhisperURL = inference.URL
	secondCtx, secondCancel := context.WithCancel(context.Background())
	secondDone := make(chan struct{})
	go func() { defer close(secondDone); h.srv.Workflow().Run(secondCtx) }()
	defer func() { secondCancel(); <-secondDone }()

	blockNext.Store(true)
	res, raw = h.send("POST", endpoint, csrf, `{}`)
	if res.StatusCode != 200 {
		t.Fatal(raw)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("second inference not started")
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := h.srv.Workflow().WithPaused(canceled, func() error {
		_, err := h.db.ExecContext(context.Background(), `DELETE FROM transcripts WHERE file_id=?`, upload.File.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, raw = h.get(endpoint)
	if !strings.Contains(raw, "Interrupted by restart or restore") || strings.Contains(raw, `"state":"running"`) {
		t.Fatal(raw)
	}
	_, raw = h.get(fmt.Sprintf("/app/files/%d/transcript", upload.File.ID))
	if strings.Contains(raw, "Searchable transcript evidence") {
		t.Fatal("pre-restore inference wrote into restored state")
	}

}
