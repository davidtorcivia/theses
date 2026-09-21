package workflow

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"time"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
)

type Job struct {
	ID      int64  `json:"id"`
	File    int64  `json:"file_id"`
	User    *int64 `json:"user_id"`
	State   string `json:"state"`
	Error   string `json:"error"`
	Stereo  bool   `json:"stereo"`
	Base    int64  `json:"base_version"`
	Created int64  `json:"created_at"`
}

var ErrTranscriptionUnavailable = errors.New("local transcription is not configured")

const jobColumns = `id,file_id,user_id,state,error,stereo,base_version,created_at`

func scanJob(row interface{ Scan(...any) error }) (j Job, err error) {
	err = row.Scan(&j.ID, &j.File, &j.User, &j.State, &j.Error, &j.Stereo, &j.Base, &j.Created)
	return
}
func (s *Service) QueueTranscription(ctx context.Context, a core.Actor, file int64, stereo bool) (core.Event, error) {
	if s.WhisperURL == "" {
		return core.Event{}, ErrTranscriptionUnavailable
	}
	f, err := files.GetFile(ctx, s.DB, file)
	if err != nil {
		return core.Event{}, err
	}
	return s.change(ctx, a, f.Proposition, "transcription_job", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		f, err := files.GetFile(ctx, tx, file)
		if err != nil {
			return core.Change{}, err
		}
		if !f.Ready() || f.Folder != files.Recordings || f.Size > 1<<30 {
			return core.Change{}, ErrInvalid
		}
		var active int
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM transcription_jobs WHERE state IN ('queued','running')`).Scan(&active)
		if err != nil {
			return core.Change{}, err
		}
		if active >= 10 {
			return core.Change{}, ErrChanged
		}
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM transcription_jobs WHERE file_id=? AND state IN ('queued','running')`, file).Scan(&active)
		if err != nil {
			return core.Change{}, err
		}
		if active > 0 {
			return core.Change{}, ErrChanged
		}
		var version int64
		err = tx.QueryRowContext(ctx, `SELECT version FROM transcripts WHERE file_id=?`, file).Scan(&version)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return core.Change{}, err
		}
		j := Job{File: file, User: &a.ID, State: "queued", Stereo: stereo, Base: version, Created: s.Now().Unix()}
		res, err := tx.ExecContext(ctx, `INSERT INTO transcription_jobs(file_id,user_id,state,stereo,base_version,created_at,updated_at) VALUES(?,?,'queued',?,?,?,?)`, file, a.ID, stereo, version, j.Created, j.Created)
		if err != nil {
			return core.Change{}, err
		}
		j.ID, err = res.LastInsertId()
		return core.Change{Entity: "transcription_job", EntityID: j.ID, Action: "create", After: j}, err
	})
}
func (s *Service) Jobs(ctx context.Context, a core.Actor, file int64) ([]Job, error) {
	f, err := files.GetFile(ctx, s.DB, file)
	if err != nil {
		return nil, err
	}
	if err = s.Readable(ctx, a, f.Proposition); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+jobColumns+` FROM transcription_jobs WHERE file_id=? ORDER BY id DESC LIMIT 10`, file)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *Service) Recover(ctx context.Context) error {
	if s.WhisperURL == "" {
		_, err := s.DB.ExecContext(ctx, `UPDATE transcription_jobs SET state='failed',error='Transcription service is no longer configured.',updated_at=unixepoch() WHERE state IN ('queued','running')`)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE transcription_jobs SET state='failed',error='Interrupted by restart or restore. Retry transcription.',updated_at=unixepoch() WHERE state='running'`)
	return err
}

// WithPaused cancels and drains inference before the database can be replaced.
func (s *Service) WithPaused(ctx context.Context, run func() error) error {
	s.controlMu.Lock()
	s.paused = true
	if s.workerCancel != nil {
		s.workerCancel()
	}
	s.controlMu.Unlock()
	s.workerMu.Lock()
	defer s.workerMu.Unlock()
	defer func() { s.controlMu.Lock(); s.paused = false; s.controlMu.Unlock() }()
	err := run()
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	recovery := s.Recover(recoveryCtx)
	return errors.Join(err, recovery)
}
func (s *Service) Run(ctx context.Context) {
	if s.WhisperURL == "" {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runNext(ctx)
		}
	}
}
func (s *Service) runNext(parent context.Context) {
	s.workerMu.Lock()
	defer s.workerMu.Unlock()
	s.controlMu.Lock()
	if s.paused {
		s.controlMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.workerCancel = cancel
	s.controlMu.Unlock()
	defer func() { cancel(); s.controlMu.Lock(); s.workerCancel = nil; s.controlMu.Unlock() }()
	j, err := scanJob(s.DB.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM transcription_jobs WHERE state='queued' ORDER BY id LIMIT 1`))
	if err != nil {
		return
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE transcription_jobs SET state='running',updated_at=unixepoch() WHERE id=? AND state='queued'`, j.ID)
	if err != nil {
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return
	}
	err = s.transcribe(ctx, j)
	if err == nil || ctx.Err() != nil {
		return
	}
	slog.Error("local transcription failed", "job", j.ID, "error", err)
	message := "Local transcription failed. Check the recording and local service, then retry."
	if errors.Is(err, ErrChanged) {
		message = "The recording or transcript changed. Existing work was preserved."
	}
	if errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrNotFound) {
		message = "The recording is no longer available to the requesting user."
	}
	for ctx.Err() == nil {
		_, err = s.DB.ExecContext(ctx, `UPDATE transcription_jobs SET state='failed',error=?,updated_at=unixepoch() WHERE id=? AND state='running'`, message, j.ID)
		if err == nil {
			return
		}
		slog.Error("persist transcription failure", "job", j.ID, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
func (s *Service) transcribe(parent context.Context, j Job) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Hour)
	defer cancel()
	if j.User == nil {
		return core.ErrForbidden
	}
	actor := core.Actor{Kind: core.KindUser, ID: *j.User}
	f, err := s.Files.ReadFile(ctx, actor, j.File)
	if err != nil {
		return err
	}
	// Reauthorize edits immediately before starting expensive local processing.
	_, err = s.change(ctx, actor, f.Proposition, "transcription_job", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		file, err := files.GetFile(ctx, tx, j.File)
		if err != nil {
			return core.Change{}, err
		}
		if !file.Ready() || file.Folder != files.Recordings || file.ObjectKey != f.ObjectKey {
			return core.Change{}, ErrChanged
		}
		var version int64
		err = tx.QueryRowContext(ctx, `SELECT version FROM transcripts WHERE file_id=?`, j.File).Scan(&version)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return core.Change{}, err
		}
		if version != j.Base {
			return core.Change{}, ErrChanged
		}
		return core.Change{Entity: "transcription_job", EntityID: j.ID, Action: "start", After: map[string]any{"file_id": j.File, "state": "running"}}, nil
	})
	if err != nil {
		return err
	}
	url, err := s.Files.DownloadURL(ctx, actor, j.File)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return ErrInvalid
	}
	client := &http.Client{Timeout: 2 * time.Hour, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("could not download recording")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("recording download failed")
	}
	temp, err := os.CreateTemp("", "theses-transcription-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	writer := multipart.NewWriter(temp)
	part, err := writer.CreateFormFile("file", "recording")
	if err != nil {
		return err
	}
	count, err := io.Copy(part, io.LimitReader(response.Body, (1<<30)+1))
	if err != nil {
		return errors.New("recording download was interrupted")
	}
	if count != f.Size {
		return errors.New("recording size differs from its verified upload")
	}
	if count > 1<<30 {
		return errors.New("local transcription accepts recordings up to 1 GiB")
	}
	fields := map[string]string{"response_format": "vtt", "language": "en", "prompt": "", "temperature": "0", "diarize": "false", "tinydiarize": "false"}
	if j.Stereo {
		fields["diarize"] = "true"
	}
	for k, v := range fields {
		if err = writer.WriteField(k, v); err != nil {
			return err
		}
	}
	if err = writer.Close(); err != nil {
		return err
	}
	if _, err = temp.Seek(0, 0); err != nil {
		return err
	}
	request, err = http.NewRequestWithContext(ctx, "POST", s.WhisperURL, temp)
	if err != nil {
		return ErrInvalid
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	info, err := temp.Stat()
	if err != nil {
		return err
	}
	request.ContentLength = info.Size()
	response, err = client.Do(request)
	if err != nil {
		return errors.New("local transcription service did not finish; retry when available")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("local transcription service refused the recording")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxTranscript+1))
	if err != nil {
		return errors.New("local transcription response was interrupted")
	}
	segments, err := ParseTranscript(string(raw), "vtt")
	if err != nil {
		return errors.New("local transcription returned no valid timed transcript; check audio format and stereo mode")
	}
	current, err := s.Files.ReadFile(ctx, actor, j.File)
	if err != nil {
		return err
	}
	if current.ObjectKey != f.ObjectKey {
		return ErrChanged
	}
	_, err = s.saveTranscript(ctx, actor, Transcript{File: j.File, Segments: segments, Version: j.Base}, j.ID)
	return err
}
