package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
)

const MaxTranscript = 4 << 20

type Segment struct {
	Start   *int64 `json:"start_ms"`
	End     *int64 `json:"end_ms"`
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
}
type Transcript struct {
	File     int64     `json:"file_id"`
	Segments []Segment `json:"segments"`
	Version  int64     `json:"version"`
}

var cueTime = regexp.MustCompile(`^(?:(\d{1,3}):)?(\d{2}):(\d{2})[.,](\d{3})$`)
var voice = regexp.MustCompile(`^<v(?:\.[^ >]+)*\s+([^>]+)>`)
var cueTags = regexp.MustCompile(`<[^>]*>`)

func timestamp(s string) (int64, error) {
	m := cueTime.FindStringSubmatch(s)
	if m == nil {
		return 0, ErrInvalid
	}
	n := make([]int64, 4)
	for i := range n {
		n[i], _ = strconv.ParseInt(m[i+1], 10, 64)
	}
	if n[1] > 59 || n[2] > 59 {
		return 0, ErrInvalid
	}
	return ((n[0]*60+n[1])*60+n[2])*1000 + n[3], nil
}
func ParseTranscript(text, format string) ([]Segment, error) {
	if len(text) > MaxTranscript {
		return nil, ErrInvalid
	}
	text = strings.TrimPrefix(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\ufeff")
	out := []Segment{}
	if format == "txt" {
		for _, p := range strings.Split(text, "\n\n") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, Segment{Text: p})
			}
		}
	} else if format == "srt" || format == "vtt" {
		for index, chunk := range strings.Split(text, "\n\n") {
			lines := strings.Split(strings.TrimSpace(chunk), "\n")
			if len(lines) == 0 {
				continue
			}
			if format == "vtt" && ((index == 0 && (lines[0] == "WEBVTT" || strings.HasPrefix(lines[0], "WEBVTT ") || strings.HasPrefix(lines[0], "WEBVTT\t"))) || lines[0] == "NOTE" || strings.HasPrefix(lines[0], "NOTE ") || strings.HasPrefix(lines[0], "NOTE\t") || lines[0] == "STYLE" || lines[0] == "REGION") {
				continue
			}
			at := -1
			for i, line := range lines {
				if strings.Contains(line, " --> ") {
					at = i
					break
				}
			}
			if at < 0 {
				if strings.TrimSpace(chunk) != "" {
					return nil, ErrInvalid
				}
				continue
			}
			times := strings.Fields(lines[at])
			if len(times) < 3 {
				return nil, ErrInvalid
			}
			start, err := timestamp(times[0])
			if err != nil {
				return nil, err
			}
			end, err := timestamp(times[2])
			if err != nil || end < start || end > 86400000 {
				return nil, ErrInvalid
			}
			body := strings.Join(lines[at+1:], "\n")
			speaker := ""
			if m := voice.FindStringSubmatch(body); m != nil {
				speaker = html.UnescapeString(m[1])
			}
			body = html.UnescapeString(cueTags.ReplaceAllString(body, ""))
			body = strings.TrimSpace(body)
			if body != "" {
				out = append(out, Segment{Start: &start, End: &end, Speaker: speaker, Text: body})
			}
		}
	} else {
		return nil, ErrInvalid
	}
	if len(out) == 0 || len(out) > 10000 {
		return nil, ErrInvalid
	}
	return out, nil
}
func (s *Service) Transcript(ctx context.Context, a core.Actor, file int64) (Transcript, error) {
	f, err := files.GetFile(ctx, s.DB, file)
	if err != nil {
		return Transcript{}, err
	}
	if err = s.Readable(ctx, a, f.Proposition); err != nil {
		return Transcript{}, err
	}
	row := Transcript{File: file, Segments: []Segment{}}
	var raw string
	err = s.DB.QueryRowContext(ctx, `SELECT segments,version FROM transcripts WHERE file_id=?`, file).Scan(&raw, &row.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return row, nil
	}
	if err != nil {
		return row, err
	}
	err = json.Unmarshal([]byte(raw), &row.Segments)
	return row, err
}
func (s *Service) SaveTranscript(ctx context.Context, a core.Actor, in Transcript) (core.Event, error) {
	return s.saveTranscript(ctx, a, in, 0)
}
func (s *Service) saveTranscript(ctx context.Context, a core.Actor, in Transcript, job int64) (core.Event, error) {
	f, err := files.GetFile(ctx, s.DB, in.File)
	if err != nil {
		return core.Event{}, err
	}
	return s.change(ctx, a, f.Proposition, "transcript", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		f, err := files.GetFile(ctx, tx, in.File)
		if err != nil {
			return core.Change{}, err
		}
		if !f.Ready() || f.Folder != files.Recordings {
			return core.Change{}, ErrInvalid
		}
		var version int64
		err = tx.QueryRowContext(ctx, `SELECT version FROM transcripts WHERE file_id=?`, in.File).Scan(&version)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return core.Change{}, err
		}
		if in.Version != version {
			return core.Change{}, ErrChanged
		}
		if len(in.Segments) == 0 || len(in.Segments) > 10000 {
			return core.Change{}, ErrInvalid
		}
		var text strings.Builder
		for _, seg := range in.Segments {
			if strings.TrimSpace(seg.Text) == "" || utf8.RuneCountInString(seg.Speaker) > 100 || strings.ContainsAny(seg.Speaker, "\r\n") {
				return core.Change{}, ErrInvalid
			}
			if (seg.Start == nil) != (seg.End == nil) {
				return core.Change{}, ErrInvalid
			}
			if seg.Start != nil && (*seg.Start < 0 || *seg.End < *seg.Start || *seg.End > 86400000) {
				return core.Change{}, ErrInvalid
			}
			text.WriteString(seg.Speaker + " " + seg.Text + "\n")
		}
		raw, err := json.Marshal(in.Segments)
		if err != nil {
			return core.Change{}, err
		}
		if len(raw) > MaxTranscript {
			return core.Change{}, ErrInvalid
		}
		in.Version++
		_, err = tx.ExecContext(ctx, `INSERT INTO transcripts(file_id,segments,text,version) VALUES(?,?,?,?) ON CONFLICT(file_id) DO UPDATE SET segments=excluded.segments,text=excluded.text,version=excluded.version`, in.File, string(raw), text.String(), in.Version)
		if err != nil {
			return core.Change{}, err
		}
		if job > 0 {
			result, err := tx.ExecContext(ctx, `UPDATE transcription_jobs SET state='complete',error='',updated_at=unixepoch() WHERE id=? AND file_id=? AND base_version=? AND state='running'`, job, in.File, in.Version-1)
			if err != nil {
				return core.Change{}, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return core.Change{}, err
			}
			if n != 1 {
				return core.Change{}, ErrChanged
			}
		}
		// Transcript bodies are fetched on demand, not broadcast to every open board.
		return core.Change{Entity: "transcript", EntityID: in.File, Action: "edit", After: map[string]any{"file_id": in.File, "version": in.Version}}, err
	})
}
func ExportTranscript(t Transcript, format string) (string, error) {
	var b strings.Builder
	stamp := func(ms int64) string {
		return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, (ms/60000)%60, (ms/1000)%60, ms%1000)
	}
	if format == "vtt" {
		b.WriteString("WEBVTT\n\n")
	} else if format != "txt" {
		return "", ErrInvalid
	}
	for _, s := range t.Segments {
		if format == "vtt" {
			if s.Start == nil {
				return "", ErrInvalid
			}
			fmt.Fprintf(&b, "%s --> %s\n", stamp(*s.Start), stamp(*s.End))
			if s.Speaker != "" {
				fmt.Fprintf(&b, "<v %s>", html.EscapeString(s.Speaker))
			}
			b.WriteString(html.EscapeString(strings.Join(strings.FieldsFunc(s.Text, func(r rune) bool { return r == '\n' || r == '\r' }), "\n")))
			if s.Speaker != "" {
				b.WriteString("</v>")
			}
		} else {
			if s.Speaker != "" {
				b.WriteString(s.Speaker + ": ")
			}
			b.WriteString(s.Text)
		}
		b.WriteString("\n\n")
	}
	return b.String(), nil
}
